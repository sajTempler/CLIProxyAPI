package helps

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// scriptConn replays server messages without gating (unit tests).
func scriptConn(msgs ...*cursorpb.AgentServerMessage) *cursorReplayConn {
	c := &cursorReplayConn{frames: msgs, gates: make([]int, len(msgs)), eofAtEnd: true}
	c.cond = sync.NewCond(&c.mu)
	return c
}

type scriptTransport struct{ conn *cursorReplayConn }

func (s *scriptTransport) OpenRun(_ context.Context, first *cursorpb.AgentClientMessage) (cursorRunConn, error) {
	s.conn.sent = append(s.conn.sent, first)
	return s.conn, nil
}

func execMsg(e *cursorpb.ExecServerMessage) *cursorpb.AgentServerMessage {
	return &cursorpb.AgentServerMessage{Message: &cursorpb.AgentServerMessage_ExecServerMessage{ExecServerMessage: e}}
}

// runScript starts a Run over msgs and returns once the reader has handled them all.
func runScript(t *testing.T, sess *cursorSession, rctx cursorRunContext, msgs ...*cursorpb.AgentServerMessage) (*cursorLiveRun, *cursorReplayConn) {
	t.Helper()
	conn := scriptConn(msgs...)
	run, err := startCursorRun(context.Background(), &scriptTransport{conn: conn}, sess, &cursorpb.AgentClientMessage{}, rctx, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The reader answers each frame before its next Recv, which returns EOF after the script.
	<-run.done
	return run, conn
}

func TestCursorSoftDenyBuiltins(t *testing.T) {
	msgs := []*cursorpb.AgentServerMessage{
		execMsg(&cursorpb.ExecServerMessage{Id: 1, ExecId: "e1", Message: &cursorpb.ExecServerMessage_ReadArgs{ReadArgs: &cursorpb.ReadArgs{Path: "/etc/hosts"}}}),
		execMsg(&cursorpb.ExecServerMessage{Id: 2, ExecId: "e2", Message: &cursorpb.ExecServerMessage_ShellStreamArgs{ShellStreamArgs: &cursorpb.ShellArgs{Command: "rm -rf /"}}}),
		execMsg(&cursorpb.ExecServerMessage{Id: 3, ExecId: "e3", Message: &cursorpb.ExecServerMessage_GrepArgs{GrepArgs: &cursorpb.GrepArgs{}}}),
		{Message: &cursorpb.AgentServerMessage_InteractionQuery{InteractionQuery: &cursorpb.InteractionQuery{Id: 9, Query: &cursorpb.InteractionQuery_WebSearchRequestQuery{WebSearchRequestQuery: &cursorpb.WebSearchRequestQuery{}}}}},
	}
	_, conn := runScript(t, newCursorSession("k", 1<<20), cursorRunContext{}, msgs...)
	sent := conn.sentMessages()[1:]
	if len(sent) != 5 {
		t.Fatalf("sent %d messages: %v", len(sent), sent)
	}
	read := sent[0].GetExecClientMessage()
	if read.GetId() != 1 || read.GetExecId() != "e1" || read.GetReadResult().GetRejected().GetPath() != "/etc/hosts" ||
		!strings.Contains(read.GetReadResult().GetRejected().GetReason(), "not available") {
		t.Fatalf("read reply = %v", read)
	}
	shell := sent[1].GetExecClientMessage().GetShellStream().GetRejected()
	if shell.GetCommand() != "rm -rf /" || shell.GetReason() == "" {
		t.Fatalf("shell_stream reply = %v", sent[1])
	}
	if sent[2].GetExecClientControlMessage().GetStreamClose().GetId() != 2 {
		t.Fatalf("shell_stream not closed: %v", sent[2])
	}
	if sent[3].GetExecClientMessage().GetGrepResult().GetError().GetError() == "" {
		t.Fatalf("grep reply = %v", sent[3])
	}
	if q := sent[4].GetInteractionResponse(); q.GetId() != 9 || q.GetWebSearchRequestResponse().GetRejected() == nil {
		t.Fatalf("query reply = %v", sent[4])
	}
}

// Every exec type Cursor can send gets a typed reply, so a new built-in in a
// future agent version is denied instead of leaving the Run waiting.
func TestCursorDenyReplyCoversEveryExecType(t *testing.T) {
	handled := map[string]bool{"request_context_args": true, "mcp_args": true, "mcp_state_exec_args": true}
	od := (&cursorpb.ExecServerMessage{}).ProtoReflect().Descriptor().Oneofs().ByName("message")
	for i := 0; i < od.Fields().Len(); i++ {
		fd := od.Fields().Get(i)
		if handled[string(fd.Name())] {
			continue
		}
		e := &cursorpb.ExecServerMessage{Id: 7, ExecId: "x"}
		rm := e.ProtoReflect()
		rm.Set(fd, protoreflect.ValueOfMessage(rm.NewField(fd).Message()))
		out, ok := cursorDenyReply(e, string(fd.Name()))
		if !ok || cursorOneofName(out, "message") == "" || out.GetId() != 7 {
			t.Errorf("%s: no typed reply", fd.Name())
		}
	}
	qd := (&cursorpb.InteractionQuery{}).ProtoReflect().Descriptor().Oneofs().ByName("query")
	for i := 0; i < qd.Fields().Len(); i++ {
		fd := qd.Fields().Get(i)
		q := &cursorpb.InteractionQuery{}
		rm := q.ProtoReflect()
		rm.Set(fd, protoreflect.ValueOfMessage(rm.NewField(fd).Message()))
		resp := &cursorpb.InteractionResponse{}
		if !cursorFillDenial(rm, resp.ProtoReflect(), "query", "result") {
			t.Errorf("query %s: no reply", fd.Name())
		}
	}
}

func TestCursorRunAnswersKVAndMCPState(t *testing.T) {
	sess := newCursorSession("k", 1<<20)
	creq, err := parseCursorClaudeRequest(func() []byte {
		c := &cursorTestConversation{Tools: cursorTestTools}
		c.user("hi")
		return c.body("m")
	}())
	if err != nil {
		t.Fatal(err)
	}
	rctx := cursorRunContext{requestContext: cursorRequestContext(creq), definitions: cursorMCPDefinitions(creq)}
	blobID := []byte(strings.Repeat("b", 32))
	kv := func(id uint32, m *cursorpb.KvServerMessage) *cursorpb.AgentServerMessage {
		m.Id = id
		return &cursorpb.AgentServerMessage{Message: &cursorpb.AgentServerMessage_KvServerMessage{KvServerMessage: m}}
	}
	_, conn := runScript(t, sess, rctx,
		kv(1, &cursorpb.KvServerMessage{Message: &cursorpb.KvServerMessage_SetBlobArgs{SetBlobArgs: &cursorpb.SetBlobArgs{BlobId: blobID, BlobData: []byte("data")}}}),
		kv(2, &cursorpb.KvServerMessage{Message: &cursorpb.KvServerMessage_GetBlobArgs{GetBlobArgs: &cursorpb.GetBlobArgs{BlobId: blobID}}}),
		kv(3, &cursorpb.KvServerMessage{Message: &cursorpb.KvServerMessage_GetBlobArgs{GetBlobArgs: &cursorpb.GetBlobArgs{BlobId: []byte(strings.Repeat("z", 32))}}}),
		execMsg(&cursorpb.ExecServerMessage{Id: 4, ExecId: "s", Message: &cursorpb.ExecServerMessage_McpStateExecArgs{McpStateExecArgs: &cursorpb.McpStateExecArgs{}}}),
		execMsg(&cursorpb.ExecServerMessage{Id: 5, ExecId: "r", Message: &cursorpb.ExecServerMessage_RequestContextArgs{RequestContextArgs: &cursorpb.RequestContextArgs{}}}),
		&cursorpb.AgentServerMessage{Message: &cursorpb.AgentServerMessage_ConversationCheckpointUpdate{ConversationCheckpointUpdate: &cursorpb.ConversationStateStructure{Turns: [][]byte{[]byte("t1")}}}},
	)
	sent := conn.sentMessages()[1:]
	if sent[0].GetKvClientMessage().GetSetBlobResult() == nil || string(sent[1].GetKvClientMessage().GetGetBlobResult().GetBlobData()) != "data" {
		t.Fatalf("kv replies = %v %v", sent[0], sent[1])
	}
	if got := sent[2].GetKvClientMessage().GetGetBlobResult(); got == nil || len(got.GetBlobData()) != 0 {
		t.Fatalf("missing blob reply = %v", sent[2])
	}
	servers := sent[3].GetExecClientMessage().GetMcpStateExecResult().GetSuccess().GetServers()
	if len(servers) != 1 || servers[0].GetServerName() != CursorMCPServer || servers[0].GetTools()[0].GetName() != "claude-code-lookup_secret" ||
		servers[0].GetTools()[0].GetToolName() != "lookup_secret" || servers[0].GetTools()[0].GetInputSchema() == nil {
		t.Fatalf("mcp state reply = %v", sent[3])
	}
	if sent[4].GetExecClientMessage().GetRequestContextResult().GetSuccess().GetRequestContext().GetRules()[0].GetType().GetGlobal() == nil {
		t.Fatalf("request context reply = %v", sent[4])
	}
	if sess.usableCheckpoint() == nil {
		t.Fatal("checkpoint not stored")
	}
}

func TestCursorEmitterToolUseEvents(t *testing.T) {
	var events []CursorClaudeEvent
	em := newCursorClaudeEmitter("composer-2.5", func(ev CursorClaudeEvent) error {
		events = append(events, ev)
		return nil
	})
	em.thinkingEnabled = true
	_ = em.thinking("hmm")
	_ = em.text("Looking.")
	_ = em.toolUse(&cursorToolCall{ToolUseID: "toolu_1", Name: "Read", Args: []byte(`{"file_path":"/a"}`)})
	em.outputTokens = 12
	_ = em.finish(cursorStopToolUse, nil)
	a, err := assembleCursorClaude(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Blocks) != 3 || a.Blocks[2]["input"].(map[string]any)["file_path"] != "/a" || a.StopReason != "tool_use" || a.Usage["output_tokens"].(float64) != 12 {
		t.Fatalf("assembled = %+v", a)
	}
	if !strings.HasPrefix(string(events[0].SSE()), "event: message_start\ndata: {") {
		t.Fatalf("sse = %q", events[0].SSE())
	}
}
