package helps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// One live agent.v1.AgentService/Run. A reader goroutine answers everything
// the server asks of the client (KV blobs, MCP tool state, request context,
// soft-denied built-in execs) and queues what the Claude response needs
// (text, thinking, MCP tool calls, usage) for whichever request currently
// owns the Run. The Run outlives a single Claude request: after a tool_use
// response it stays open until the next request brings the tool_result.

// DefaultCursorHeartbeat is how often a client heartbeat goes out on an open Run.
const DefaultCursorHeartbeat = 5 * time.Second

type cursorEventKind int

const (
	cursorEvText cursorEventKind = iota
	cursorEvThinking
	cursorEvThinkingDone
	cursorEvToolCall
	cursorEvTokens
	cursorEvTurnEnded
	cursorEvEnd // terminal: err is nil (clean end) or the stream error
)

type cursorRunEvent struct {
	kind  cursorEventKind
	text  string
	call  *cursorToolCall
	count int64
	usage *cursorpb.TurnEndedUpdate
	err   error
}

// cursorToolCall is an MCP call Cursor asked for, surfaced to Claude Code as a tool_use.
type cursorToolCall struct {
	ToolUseID    string // Claude tool_use id (generated; Cursor ids are not Claude-shaped)
	CursorCallID string
	ExecID       string
	ExecNum      uint32
	Name         string
	Args         json.RawMessage
}

func (c *cursorToolCall) fingerprint() string { return c.Name + "\x00" + string(c.Args) }

// cursorEventQueue is an unbounded FIFO, so the reader never blocks on a slow
// (or absent, while a tool runs) consumer and keeps answering KV requests.
type cursorEventQueue struct {
	mu     sync.Mutex
	items  []cursorRunEvent
	notify chan struct{}
}

func newCursorEventQueue() *cursorEventQueue {
	return &cursorEventQueue{notify: make(chan struct{}, 1)}
}

func (q *cursorEventQueue) push(ev cursorRunEvent) {
	q.mu.Lock()
	q.items = append(q.items, ev)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// pop waits for the next event or ctx.
func (q *cursorEventQueue) pop(ctx context.Context) (cursorRunEvent, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			ev := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return ev, nil
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
		case <-ctx.Done():
			return cursorRunEvent{}, ctx.Err()
		}
	}
}

// cursorRunContext is what the reader needs from the Claude request that owns
// the Run; it is replaced when a later request continues the Run.
type cursorRunContext struct {
	requestContext *cursorpb.RequestContext
	definitions    []*cursorpb.McpToolDefinition
	// resumeAnswers maps a tool call fingerprint to the cached tool_result text
	// for a resume_action Run, which regenerates the interrupted call.
	resumeAnswers map[string]cursorToolAnswer
}

type cursorToolAnswer struct {
	text    string
	isError bool
}

type cursorLiveRun struct {
	conn     cursorRunConn
	sess     *cursorSession
	observer CursorFrameObserver
	events   *cursorEventQueue
	cancel   context.CancelFunc
	done     chan struct{} // closed when the reader exits

	sendMu sync.Mutex // serializes conn.Send and observer calls

	ctxMu sync.Mutex
	rctx  cursorRunContext

	closeOnce sync.Once
}

// startCursorRun opens a Run, sends first and starts the reader (and the
// heartbeat when interval > 0). The Run's lifetime is detached from ctx: it
// must survive the request that opened it while a tool runs in Claude Code.
func startCursorRun(ctx context.Context, t cursorTransport, sess *cursorSession, first *cursorpb.AgentClientMessage, rctx cursorRunContext, observer CursorFrameObserver, heartbeat time.Duration) (*cursorLiveRun, error) {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if observer != nil {
		if err := observer.Client(first); err != nil {
			log.WithError(err).Debug("cursor: observer failed on run request")
		}
	}
	conn, err := t.OpenRun(runCtx, first)
	if err != nil {
		cancel()
		return nil, err
	}
	r := &cursorLiveRun{conn: conn, sess: sess, observer: observer, events: newCursorEventQueue(), cancel: cancel, done: make(chan struct{}), rctx: rctx}
	go r.read()
	if heartbeat > 0 {
		go r.heartbeat(heartbeat)
	}
	return r, nil
}

// heartbeat keeps the Run alive while Claude Code executes a tool, which can
// take minutes. It is a keepalive, not a deadline: nothing here gives up on
// the stream, and a failed send only stops the heartbeat (the reader reports
// the stream's end). The reference client sends one every 5 s; the server
// sends its own every ~10 s while it waits for a tool result (recorded).
// Heartbeats are not passed to the frame observer.
func (r *cursorLiveRun) heartbeat(every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	msg := &cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &cursorpb.ClientHeartbeat{}}}
	for {
		select {
		case <-r.done:
			return
		case <-tick.C:
			r.sendMu.Lock()
			err := r.conn.Send(msg) // bypasses send(): not recorded
			r.sendMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (r *cursorLiveRun) send(m *cursorpb.AgentClientMessage) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if r.observer != nil {
		if err := r.observer.Client(m); err != nil {
			log.WithError(err).Debug("cursor: observer failed on client message")
		}
	}
	return r.conn.Send(m)
}

func (r *cursorLiveRun) setContext(c cursorRunContext) {
	r.ctxMu.Lock()
	r.rctx = c
	r.ctxMu.Unlock()
}

func (r *cursorLiveRun) context() cursorRunContext {
	r.ctxMu.Lock()
	defer r.ctxMu.Unlock()
	return r.rctx
}

// alive reports whether the reader is still running.
func (r *cursorLiveRun) alive() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// close aborts the Run. Safe to call more than once.
func (r *cursorLiveRun) close() {
	r.closeOnce.Do(func() {
		r.conn.Close()
		r.cancel()
	})
}

// finishSend half-closes after turn_ended so the server ends the stream once
// it has sent the final checkpoint; the reader drains it in the background.
func (r *cursorLiveRun) finishSend() {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if err := r.conn.CloseSend(); err != nil {
		log.WithError(err).Debug("cursor: close send")
	}
}

// answerTool sends an mcp_result for a pending call.
func (r *cursorLiveRun) answerTool(call *cursorToolCall, text string, isError bool) error {
	return r.send(cursorMCPResult(call.ExecNum, call.ExecID, text, isError))
}

func cursorMCPResult(id uint32, execID, text string, isError bool) *cursorpb.AgentClientMessage {
	return &cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ExecClientMessage{ExecClientMessage: &cursorpb.ExecClientMessage{
		Id:     id,
		ExecId: execID,
		Message: &cursorpb.ExecClientMessage_McpResult{McpResult: &cursorpb.McpResult{Result: &cursorpb.McpResult_Success{Success: &cursorpb.McpSuccess{
			Content: []*cursorpb.McpToolResultContentItem{{Content: &cursorpb.McpToolResultContentItem_Text{Text: &cursorpb.McpTextContent{Text: text}}}},
			IsError: isError,
		}}}},
	}}}
}

func (r *cursorLiveRun) read() {
	defer close(r.done)
	defer r.cancel()
	for {
		m, err := r.conn.Recv()
		if err != nil {
			var se *CursorStatusError
			if r.observer != nil && errors.As(err, &se) && se.HTTPStatus == 0 {
				raw, _ := json.Marshal(map[string]any{"error": map[string]string{"code": se.Code, "message": se.Message}})
				r.observer.EndStream(raw)
			}
			if errors.Is(err, io.EOF) {
				err = nil
			}
			r.events.push(cursorRunEvent{kind: cursorEvEnd, err: err})
			return
		}
		if r.observer != nil {
			r.sendMu.Lock()
			if errObs := r.observer.Server(m); errObs != nil {
				log.WithError(errObs).Debug("cursor: observer failed on server message")
			}
			r.sendMu.Unlock()
		}
		if errHandle := r.handle(m); errHandle != nil {
			// A reply that cannot be sent means the request side is gone (the
			// server ended the stream or it broke); Recv reports the real end.
			log.WithError(errHandle).Debug("cursor: reply to server message failed")
		}
	}
}

func (r *cursorLiveRun) handle(m *cursorpb.AgentServerMessage) error {
	switch msg := m.GetMessage().(type) {
	case *cursorpb.AgentServerMessage_InteractionUpdate:
		r.handleUpdate(msg.InteractionUpdate)
	case *cursorpb.AgentServerMessage_ExecServerMessage:
		return r.handleExec(msg.ExecServerMessage)
	case *cursorpb.AgentServerMessage_KvServerMessage:
		return r.handleKV(msg.KvServerMessage)
	case *cursorpb.AgentServerMessage_ConversationCheckpointUpdate:
		raw, err := proto.Marshal(msg.ConversationCheckpointUpdate)
		if err != nil {
			return err
		}
		r.sess.setCheckpoint(raw)
	case *cursorpb.AgentServerMessage_InteractionQuery:
		return r.handleQuery(msg.InteractionQuery)
	}
	return nil
}

func (r *cursorLiveRun) handleUpdate(u *cursorpb.InteractionUpdate) {
	switch v := u.GetMessage().(type) {
	case *cursorpb.InteractionUpdate_TextDelta:
		r.events.push(cursorRunEvent{kind: cursorEvText, text: v.TextDelta.GetText()})
	case *cursorpb.InteractionUpdate_ThinkingDelta:
		r.events.push(cursorRunEvent{kind: cursorEvThinking, text: v.ThinkingDelta.GetText()})
	case *cursorpb.InteractionUpdate_ThinkingCompleted:
		r.events.push(cursorRunEvent{kind: cursorEvThinkingDone})
	case *cursorpb.InteractionUpdate_TokenDelta:
		r.events.push(cursorRunEvent{kind: cursorEvTokens, count: int64(v.TokenDelta.GetTokens())})
	case *cursorpb.InteractionUpdate_TurnEnded:
		r.events.push(cursorRunEvent{kind: cursorEvTurnEnded, usage: v.TurnEnded})
	}
	// tool_call_started/completed (get_mcp_tools, mcp_tool_call), partial tool
	// calls, tool_requests_listed, steps, heartbeats and the rest are Cursor UI
	// state; Claude Code sees tool calls via mcp_args.
}

func (r *cursorLiveRun) handleExec(e *cursorpb.ExecServerMessage) error {
	reply := func(msg *cursorpb.ExecClientMessage) error {
		msg.Id, msg.ExecId = e.GetId(), e.GetExecId()
		return r.send(&cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ExecClientMessage{ExecClientMessage: msg}})
	}
	rc := r.context()
	switch v := e.GetMessage().(type) {
	case *cursorpb.ExecServerMessage_RequestContextArgs:
		return reply(&cursorpb.ExecClientMessage{Message: &cursorpb.ExecClientMessage_RequestContextResult{RequestContextResult: &cursorpb.RequestContextResult{
			Result: &cursorpb.RequestContextResult_Success{Success: &cursorpb.RequestContextSuccess{RequestContext: rc.requestContext}},
		}}})
	case *cursorpb.ExecServerMessage_McpStateExecArgs:
		return reply(&cursorpb.ExecClientMessage{Message: &cursorpb.ExecClientMessage_McpStateExecResult{McpStateExecResult: &cursorpb.McpStateExecResult{
			Result: &cursorpb.McpStateExecResult_Success{Success: &cursorpb.McpStateSuccess{Servers: []*cursorpb.McpStateServer{{
				ServerName: CursorMCPServer, ServerIdentifier: CursorMCPServer, Tools: rc.definitions,
			}}}},
		}}})
	case *cursorpb.ExecServerMessage_McpArgs:
		call := &cursorToolCall{
			ToolUseID:    newCursorToolUseID(),
			CursorCallID: v.McpArgs.GetToolCallId(),
			ExecID:       e.GetExecId(),
			ExecNum:      e.GetId(),
			Name:         cursorToolName(v.McpArgs),
			Args:         cursorArgsJSON(v.McpArgs),
		}
		if ans, ok := rc.resumeAnswers[call.fingerprint()]; ok {
			// A resumed Run regenerates the interrupted call; answer it from the
			// tool_result Claude Code already sent instead of asking again.
			r.ctxMu.Lock()
			delete(r.rctx.resumeAnswers, call.fingerprint())
			r.ctxMu.Unlock()
			return r.answerTool(call, ans.text, ans.isError)
		}
		r.events.push(cursorRunEvent{kind: cursorEvToolCall, call: call})
		return nil
	case nil:
		return nil
	default:
		return r.softDeny(e)
	}
}

// softDeny answers a Cursor built-in exec (shell, read, write, ...) with its
// typed "rejected"/"error" result so the model falls back to the MCP tools.
// Nothing Cursor asks for is ever executed. Only the type name is logged.
func (r *cursorLiveRun) softDeny(e *cursorpb.ExecServerMessage) error {
	name := cursorOneofName(e, "message")
	log.WithField("exec", name).Warn("cursor: soft-denied built-in exec")
	out, ok := cursorDenyReply(e, name)
	if !ok {
		return nil
	}
	if err := r.send(&cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ExecClientMessage{ExecClientMessage: out}}); err != nil {
		return err
	}
	if name == "shell_stream_args" {
		// A streamed shell also needs its stream closed.
		return r.send(&cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: &cursorpb.ExecClientControlMessage{
			Message: &cursorpb.ExecClientControlMessage_StreamClose{StreamClose: &cursorpb.ExecClientStreamClose{Id: e.GetId()}},
		}}})
	}
	return nil
}

func (r *cursorLiveRun) handleQuery(q *cursorpb.InteractionQuery) error {
	name := cursorOneofName(q, "query")
	log.WithField("query", name).Warn("cursor: rejected interaction query")
	resp := &cursorpb.InteractionResponse{Id: q.GetId()}
	if !cursorFillDenial(q.ProtoReflect(), resp.ProtoReflect(), "query", "result") {
		return nil
	}
	return r.send(&cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_InteractionResponse{InteractionResponse: resp}})
}

func (r *cursorLiveRun) handleKV(k *cursorpb.KvServerMessage) error {
	switch v := k.GetMessage().(type) {
	case *cursorpb.KvServerMessage_SetBlobArgs:
		r.sess.setBlob(v.SetBlobArgs.GetBlobId(), v.SetBlobArgs.GetBlobData())
		return r.send(&cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_KvClientMessage{KvClientMessage: &cursorpb.KvClientMessage{
			Id: k.GetId(), Message: &cursorpb.KvClientMessage_SetBlobResult{SetBlobResult: &cursorpb.SetBlobResult{}},
		}}})
	case *cursorpb.KvServerMessage_GetBlobArgs:
		id := v.GetBlobArgs.GetBlobId()
		data, ok := r.sess.getBlob(id)
		if !ok && len(id) != 32 {
			// Ids that are not 32-byte content hashes carry the data inline
			// (behaviour of the CLI's blob store, kept from the spike client).
			data = id
		}
		if data == nil {
			data = []byte{}
		}
		return r.send(&cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_KvClientMessage{KvClientMessage: &cursorpb.KvClientMessage{
			Id: k.GetId(), Message: &cursorpb.KvClientMessage_GetBlobResult{GetBlobResult: &cursorpb.GetBlobResult{BlobData: data}},
		}}})
	}
	return nil
}

// cursorOneofName returns the set field name of a oneof ("" when unset).
func cursorOneofName(m proto.Message, oneof string) string {
	rm := m.ProtoReflect()
	od := rm.Descriptor().Oneofs().ByName(protoreflect.Name(oneof))
	if od == nil {
		return ""
	}
	if fd := rm.WhichOneof(od); fd != nil {
		return string(fd.Name())
	}
	return ""
}

// cursorDenyReply builds the typed soft-deny result for a built-in exec by
// reflection, so new exec types are denied without code changes: the result
// field is found by name (foo_args -> foo_result, with the few irregular
// names mapped), then its rejected/error variant is filled with the reason.
func cursorDenyReply(e *cursorpb.ExecServerMessage, argsName string) (*cursorpb.ExecClientMessage, bool) {
	out := &cursorpb.ExecClientMessage{Id: e.GetId(), ExecId: e.GetExecId()}
	if !cursorFillDenial(e.ProtoReflect(), out.ProtoReflect(), "message", "message") {
		log.WithField("exec", argsName).Warn("cursor: no result type for built-in exec; left unanswered")
		return nil, false
	}
	return out, true
}

var cursorIrregularResults = map[string]string{
	"shell_stream_args": "shell_stream",
}

// cursorFillDenial sets, on dst's oneof dstOneof, the result matching src's
// set oneof field, choosing its rejected/error/permission_denied variant (or
// an empty result) and filling reason/error strings plus same-named string
// fields copied from the request (path, command, url).
func cursorFillDenial(src, dst protoreflect.Message, srcOneof, dstOneof string) bool {
	sod := src.Descriptor().Oneofs().ByName(protoreflect.Name(srcOneof))
	dod := dst.Descriptor().Oneofs().ByName(protoreflect.Name(dstOneof))
	if sod == nil || dod == nil {
		return false
	}
	sfd := src.WhichOneof(sod)
	if sfd == nil {
		return false
	}
	name := string(sfd.Name())
	var want []string
	if irr, ok := cursorIrregularResults[name]; ok {
		want = append(want, irr)
	}
	for _, suffix := range []string{"_args", "_query", "_request"} {
		if base, ok := strings.CutSuffix(name, suffix); ok {
			want = append(want, base+"_result", base+"_response")
		}
	}
	var dfd protoreflect.FieldDescriptor
	for _, w := range want {
		if f := dod.Fields().ByName(protoreflect.Name(w)); f != nil && f.Message() != nil {
			dfd = f
			break
		}
	}
	if dfd == nil {
		// Same field number is the protocol's convention for most pairs.
		if f := dod.Fields().ByNumber(sfd.Number()); f != nil && f.Message() != nil {
			dfd = f
		}
	}
	if dfd == nil {
		return false
	}
	var args protoreflect.Message
	if sfd.Message() != nil {
		args = src.Get(sfd).Message()
	}
	result := dst.NewField(dfd).Message()
	cursorFillVariant(result, args)
	dst.Set(dfd, protoreflect.ValueOfMessage(result))
	return true
}

func cursorFillVariant(result, args protoreflect.Message) {
	fields := result.Descriptor().Fields()
	for _, v := range []string{"rejected", "error", "permission_denied"} {
		fd := fields.ByName(protoreflect.Name(v))
		if fd == nil || fd.Message() == nil {
			continue
		}
		variant := result.NewField(fd).Message()
		vf := variant.Descriptor().Fields()
		for i := 0; i < vf.Len(); i++ {
			f := vf.Get(i)
			if f.Kind() != protoreflect.StringKind || f.IsList() || f.IsMap() {
				continue
			}
			switch f.Name() {
			case "reason", "error", "message", "error_message":
				variant.Set(f, protoreflect.ValueOfString(cursorSoftDenyReason))
			default:
				if args != nil {
					if af := args.Descriptor().Fields().ByName(f.Name()); af != nil && af.Kind() == protoreflect.StringKind && !af.IsList() {
						variant.Set(f, args.Get(af))
					}
				}
			}
		}
		result.Set(fd, protoreflect.ValueOfMessage(variant))
		return
	}
}
