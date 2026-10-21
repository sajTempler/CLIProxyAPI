package helps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorfixture"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	"google.golang.org/protobuf/proto"
)

// cursorReplayTransport serves recorded Runs in order. Each server frame is
// released only after the client has sent as many mcp_results as preceded
// it in the recording, so a held-open Run waits for its tool result exactly
// like the live server.
type cursorReplayTransport struct {
	t        *testing.T
	fixtures []*cursorfixture.Fixture
	mu       sync.Mutex
	conns    []*cursorReplayConn
}

func loadCursorFixtures(t *testing.T, names ...string) *cursorReplayTransport {
	t.Helper()
	rt := &cursorReplayTransport{t: t}
	for _, n := range names {
		fx, err := cursorfixture.Load(filepath.Join("cursorfixture", "testdata", "runs", n+".json"))
		if err != nil {
			t.Fatal(err)
		}
		rt.fixtures = append(rt.fixtures, fx)
	}
	return rt
}

func (rt *cursorReplayTransport) OpenRun(_ context.Context, first *cursorpb.AgentClientMessage) (cursorRunConn, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.conns) >= len(rt.fixtures) {
		return nil, errors.New("replay: no fixture left for another Run")
	}
	c, err := newCursorReplayConn(rt.fixtures[len(rt.conns)])
	if err != nil {
		return nil, err
	}
	c.sent = append(c.sent, first)
	rt.conns = append(rt.conns, c)
	return c, nil
}

func (rt *cursorReplayTransport) opened() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.conns)
}

func (rt *cursorReplayTransport) conn(i int) *cursorReplayConn {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.conns[i]
}

type cursorReplayConn struct {
	mu          sync.Mutex
	cond        *sync.Cond
	frames      []*cursorpb.AgentServerMessage
	gates       []int
	end         error
	next        int
	results     int
	sent        []*cursorpb.AgentClientMessage
	halfClosed  bool
	closed      bool
	hasEndFrame bool
	eofAtEnd    bool // unit-test scripts: clean end right after the last frame
}

func isMCPResult(m *cursorpb.AgentClientMessage) bool {
	return m.GetExecClientMessage().GetMcpResult() != nil
}

func newCursorReplayConn(fx *cursorfixture.Fixture) (*cursorReplayConn, error) {
	c := &cursorReplayConn{}
	c.cond = sync.NewCond(&c.mu)
	results := 0
	for _, fr := range fx.Frames {
		switch fr.Dir {
		case cursorfixture.DirClient:
			one := &cursorfixture.Fixture{Format: fx.Format, Frames: []cursorfixture.Frame{fr}}
			cli, err := one.Client()
			if err != nil {
				return nil, err
			}
			if isMCPResult(cli[0]) {
				results++
			}
		case cursorfixture.DirServer:
			one := &cursorfixture.Fixture{Format: fx.Format, Frames: []cursorfixture.Frame{fr}}
			srv, err := one.Server()
			if err != nil {
				return nil, err
			}
			c.frames = append(c.frames, srv[0])
			c.gates = append(c.gates, results)
		}
	}
	if len(fx.EndStream) > 0 {
		c.hasEndFrame = true
		c.end = parseConnectEndStream(fx.EndStream)
	}
	return c, nil
}

func (c *cursorReplayConn) Send(m *cursorpb.AgentClientMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.halfClosed {
		return io.ErrClosedPipe
	}
	c.sent = append(c.sent, m)
	if isMCPResult(m) {
		c.results++
	}
	c.cond.Broadcast()
	return nil
}

func (c *cursorReplayConn) Recv() (*cursorpb.AgentServerMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		switch {
		case c.closed:
			return nil, errors.New("replay: stream closed")
		case c.next < len(c.frames) && c.results >= c.gates[c.next]:
			m := c.frames[c.next]
			c.next++
			return m, nil
		case c.next >= len(c.frames) && c.hasEndFrame:
			return nil, c.end
		case c.next >= len(c.frames) && (c.halfClosed || c.eofAtEnd):
			return nil, io.EOF
		}
		c.cond.Wait()
	}
}

func (c *cursorReplayConn) CloseSend() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.halfClosed = true
	c.cond.Broadcast()
	return nil
}

func (c *cursorReplayConn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.cond.Broadcast()
}

func (c *cursorReplayConn) isHalfClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.halfClosed
}

func (c *cursorReplayConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *cursorReplayConn) sentMessages() []*cursorpb.AgentClientMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*cursorpb.AgentClientMessage(nil), c.sent...)
}

func (c *cursorReplayConn) mcpResults() []string {
	var out []string
	for _, m := range c.sentMessages() {
		if r := m.GetExecClientMessage().GetMcpResult(); r != nil {
			out = append(out, r.GetSuccess().GetContent()[0].GetText().GetText())
		}
	}
	return out
}

func newCursorTestClient(rt *cursorReplayTransport) *CursorClient {
	c := newCursorClient(rt, NewCursorSessionStore(0, 0, nil))
	c.Heartbeat = 0
	return c
}

const cursorTestSystem = "You are Claude Code, Anthropic's official CLI for Claude. Be concise.\n\n<env>\nWorking directory: /tmp/cursor-live-ws\nPlatform: darwin\n</env>"

func streamCursor(t *testing.T, c *CursorClient, key, model string, conv *cursorTestConversation) *cursorAssembled {
	t.Helper()
	var events []CursorClaudeEvent
	err := c.Stream(context.Background(), CursorStreamRequest{ConversationKey: key, Model: model, Body: conv.body(model)}, func(ev CursorClaudeEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	a, err := assembleCursorClaude(events)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return a
}

func lastCheckpoint(t *testing.T, fx *cursorfixture.Fixture) *cursorpb.ConversationStateStructure {
	t.Helper()
	srv, err := fx.Server()
	if err != nil {
		t.Fatal(err)
	}
	var cp *cursorpb.ConversationStateStructure
	for _, m := range srv {
		if c := m.GetConversationCheckpointUpdate(); c != nil {
			cp = c
		}
	}
	if cp == nil {
		t.Fatal("fixture has no checkpoint")
	}
	return cp
}

func lastTurnEnded(t *testing.T, fx *cursorfixture.Fixture) *cursorpb.TurnEndedUpdate {
	t.Helper()
	srv, _ := fx.Server()
	for i := len(srv) - 1; i >= 0; i-- {
		if te := srv[i].GetInteractionUpdate().GetTurnEnded(); te != nil {
			return te
		}
	}
	t.Fatal("fixture has no turn_ended")
	return nil
}

func TestCursorStreamPlainText(t *testing.T) {
	rt := loadCursorFixtures(t, "plain-text")
	c := newCursorTestClient(rt)
	conv := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools, Thinking: true}
	conv.user("Reply with one short sentence: what is 2+2?")

	a := streamCursor(t, c, "conv", "composer-2.5", conv)
	if !strings.Contains(a.text(), "4") || a.StopReason != cursorStopEndTurn {
		t.Fatalf("text %q stop %q", a.text(), a.StopReason)
	}
	if !a.has("thinking") || a.Blocks[0]["type"] != "thinking" {
		t.Fatalf("blocks = %v", a.Blocks)
	}
	te := lastTurnEnded(t, rt.fixtures[0])
	wantInput := te.GetInputTokens() - te.GetCacheReadTokens() - te.GetCacheWriteTokens()
	if a.Usage["input_tokens"].(float64) != float64(wantInput) || a.Usage["output_tokens"].(float64) != float64(te.GetOutputTokens()) ||
		a.Usage["cache_read_input_tokens"].(float64) != float64(te.GetCacheReadTokens()) {
		t.Fatalf("usage = %v, turn_ended = %v", a.Usage, te)
	}

	first := rt.conn(0).sentMessages()[0].GetRunRequest()
	uma := first.GetAction().GetUserMessageAction()
	if uma.GetUserMessage().GetText() != "Reply with one short sentence: what is 2+2?" || uma.GetUserMessage().GetMode() != cursorpb.AgentMode_AGENT_MODE_AGENT {
		t.Fatalf("user message = %v", uma.GetUserMessage())
	}
	rc := uma.GetRequestContext()
	if len(rc.GetRules()) != 1 || rc.GetRules()[0].GetType().GetGlobal() == nil ||
		!strings.HasPrefix(rc.GetRules()[0].GetContent(), "You are Claude Code") || !strings.Contains(rc.GetRules()[0].GetContent(), "built-in tools") {
		t.Fatalf("rules = %v", rc.GetRules())
	}
	if rc.GetEnv().GetWorkspacePaths()[0] != "/tmp/cursor-live-ws" {
		t.Fatalf("workspace = %v", rc.GetEnv().GetWorkspacePaths())
	}
	tools := rc.GetMcpMetaToolOptions().GetMcpDescriptors()[0].GetTools()
	if len(tools) != 2 || tools[0].GetToolName() != "lookup_secret" || tools[0].GetInputSchema().GetStructValue().GetFields()["required"] == nil {
		t.Fatalf("tools = %v", tools)
	}
	if uma.GetConversationHistory() != nil || first.GetCustomSystemPrompt() != "" || first.GetSystemPromptSpec() != nil || rc.GetSystemPromptOverride() != nil {
		t.Fatal("unsupported prompt channels must stay unset")
	}
	if first.GetRequestedModel().GetModelId() != "composer-2.5" || len(first.GetConversationState().GetTurns()) != 0 {
		t.Fatalf("run request = %v", first)
	}
}

func TestCursorStreamThinkingOnlyWhenRequested(t *testing.T) {
	rt := loadCursorFixtures(t, "plain-text")
	conv := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools}
	conv.user("Reply with one short sentence: what is 2+2?")
	a := streamCursor(t, newCursorTestClient(rt), "conv", "composer-2.5", conv)
	if a.has("thinking") || len(a.Blocks) != 1 {
		t.Fatalf("blocks = %v", a.Blocks)
	}
}

func TestCursorStreamToolRoundTripThenNewTurn(t *testing.T) {
	rt := loadCursorFixtures(t, "tool-roundtrip-1", "tool-roundtrip-2")
	c := newCursorTestClient(rt)
	conv := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools, Thinking: true}
	conv.user("Use the lookup_secret tool to get the secret for key 'alpha' and tell me the value.")

	a := streamCursor(t, c, "conv", "composer-2.5", conv)
	uses := a.toolUses()
	if a.StopReason != cursorStopToolUse || len(uses) != 1 || uses[0]["name"] != "lookup_secret" ||
		uses[0]["input"].(map[string]any)["key"] != "alpha" || !strings.HasPrefix(uses[0]["id"].(string), "toolu_") {
		t.Fatalf("first response: stop %q blocks %v", a.StopReason, a.Blocks)
	}
	if a.Usage["output_tokens"].(float64) <= 0 {
		t.Fatalf("usage = %v", a.Usage)
	}
	conv.assistant(a)
	conv.toolResults(a, cursorTestToolRunner)

	b := streamCursor(t, c, "conv", "composer-2.5", conv)
	if rt.opened() != 1 {
		t.Fatalf("tool result opened a new Run (%d)", rt.opened())
	}
	if got := rt.conn(0).mcpResults(); len(got) != 1 || got[0] != "PURPLE-42" {
		t.Fatalf("mcp results = %v", got)
	}
	if !strings.Contains(b.text(), "PURPLE-42") || b.StopReason != cursorStopEndTurn || len(b.toolUses()) != 0 {
		t.Fatalf("second response: %q %q", b.text(), b.StopReason)
	}
	// Per-response output tokens add up to the Run's turn_ended total.
	if te := lastTurnEnded(t, rt.fixtures[0]); a.Usage["output_tokens"].(float64)+b.Usage["output_tokens"].(float64) != float64(te.GetOutputTokens()) {
		t.Fatalf("output tokens %v + %v != %d", a.Usage["output_tokens"], b.Usage["output_tokens"], te.GetOutputTokens())
	}
	conv.assistant(b)
	conv.user("Thanks. Now say the value backwards, no tools.")

	d := streamCursor(t, c, "conv", "composer-2.5", conv)
	if rt.opened() != 2 {
		t.Fatalf("runs opened = %d", rt.opened())
	}
	if !rt.conn(0).isHalfClosed() {
		t.Fatal("finished Run was not half-closed")
	}
	run1 := rt.conn(0).sentMessages()[0].GetRunRequest()
	run2 := rt.conn(1).sentMessages()[0].GetRunRequest()
	if !proto.Equal(run2.GetConversationState(), lastCheckpoint(t, rt.fixtures[0])) {
		t.Fatal("new turn did not start from the last checkpoint")
	}
	if run2.GetConversationId() != run1.GetConversationId() {
		t.Fatalf("conversation id changed: %s -> %s", run1.GetConversationId(), run2.GetConversationId())
	}
	if got := run2.GetAction().GetUserMessageAction().GetUserMessage().GetText(); got != "Thanks. Now say the value backwards, no tools." {
		t.Fatalf("new turn text = %q", got)
	}
	if !strings.Contains(d.text(), "24-ELPURP") {
		t.Fatalf("third response: %q", d.text())
	}
}

func TestCursorStreamTwoToolCallsInOneStep(t *testing.T) {
	for _, tc := range []struct{ fixture, model string }{
		{"two-tools", "composer-2.5"},
		{"two-tools-gpt", "gpt-5.4-mini"},
		{"two-tools-sonnet", "claude-sonnet-5-low"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			rt := loadCursorFixtures(t, tc.fixture)
			c := newCursorTestClient(rt)
			conv := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools, Thinking: true}
			conv.user("Call lookup_secret for key 'alpha' and for key 'beta' in parallel (two tool calls in the same step, do not wait between them), then report both values.")
			var keys []string
			var final *cursorAssembled
			for step := 0; step < 4; step++ {
				a := streamCursor(t, c, "conv", tc.model, conv)
				conv.assistant(a)
				if a.StopReason != cursorStopToolUse {
					final = a
					break
				}
				for _, u := range a.toolUses() {
					keys = append(keys, u["input"].(map[string]any)["key"].(string))
				}
				conv.toolResults(a, cursorTestToolRunner)
			}
			// Cursor dispatches the step's calls one at a time, so they arrive as two tool_use responses.
			if strings.Join(keys, ",") != "alpha,beta" {
				t.Fatalf("tool calls = %v", keys)
			}
			if final == nil || !strings.Contains(final.text(), "PURPLE-42") || !strings.Contains(final.text(), "ORANGE-17") {
				t.Fatalf("final = %+v", final)
			}
			if rt.opened() != 1 {
				t.Fatalf("runs opened = %d", rt.opened())
			}
		})
	}
}

func TestCursorStreamResumeAfterLostRun(t *testing.T) {
	rt := loadCursorFixtures(t, "resume-lost-run-1", "resume-lost-run-2")
	c := newCursorTestClient(rt)
	conv := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools, Thinking: true}
	conv.user("Use the lookup_secret tool to get the secret for key 'alpha' and tell me the value.")
	a := streamCursor(t, c, "conv", "composer-2.5", conv)
	if a.StopReason != cursorStopToolUse {
		t.Fatalf("stop = %q", a.StopReason)
	}
	withCursorSession(c.sessions, "conv", func(s *cursorSession) {
		s.live.close()
		<-s.live.done
	})
	conv.assistant(a)
	conv.toolResults(a, cursorTestToolRunner)

	b := streamCursor(t, c, "conv", "composer-2.5", conv)
	if rt.opened() != 2 {
		t.Fatalf("runs opened = %d", rt.opened())
	}
	run2 := rt.conn(1).sentMessages()[0].GetRunRequest()
	if run2.GetAction().GetResumeAction() == nil || !proto.Equal(run2.GetConversationState(), lastCheckpoint(t, rt.fixtures[0])) {
		t.Fatalf("resume run request = %v", run2.GetAction())
	}
	// The regenerated call is answered from the cached tool_result, not surfaced again.
	if len(b.toolUses()) != 0 || !strings.Contains(b.text(), "PURPLE-42") {
		t.Fatalf("resumed response: %v", b.Blocks)
	}
	if got := rt.conn(1).mcpResults(); len(got) != 1 || got[0] != "PURPLE-42" {
		t.Fatalf("mcp results = %v", got)
	}
}

func TestCursorStreamBigSystemPromptRule(t *testing.T) {
	rt := loadCursorFixtures(t, "big-prompt")
	conv := &cursorTestConversation{System: cursorBigSystemPrompt(), Tools: cursorTestTools, Thinking: true}
	conv.user("Who are you, and which product are you running in? Answer in one sentence.")
	a := streamCursor(t, newCursorTestClient(rt), "conv", "composer-2.5", conv)
	text := strings.TrimSpace(a.text())
	if !strings.HasPrefix(text, "ZQX> ") || !strings.HasSuffix(text, "<<done>>") || text[5:] != strings.ToLower(text[5:]) {
		t.Fatalf("rule not followed: %q", text)
	}
	rule := rt.conn(0).sentMessages()[0].GetRunRequest().GetAction().GetUserMessageAction().GetRequestContext().GetRules()[0]
	if len(rule.GetContent()) < 60000 {
		t.Fatalf("rule length = %d", len(rule.GetContent()))
	}
}

func TestCursorStreamSeedsTranscriptWithoutState(t *testing.T) {
	rt := loadCursorFixtures(t, "plain-text")
	conv := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools}
	conv.user("Use the lookup_secret tool for 'alpha'.")
	conv.Messages = append(conv.Messages, map[string]any{"role": "assistant", "content": []map[string]any{
		{"type": "text", "text": "Looking it up."},
		{"type": "tool_use", "id": "toolu_prev1", "name": "lookup_secret", "input": map[string]any{"key": "alpha"}},
	}})
	conv.Messages = append(conv.Messages, map[string]any{"role": "user", "content": []map[string]any{
		{"type": "tool_result", "tool_use_id": "toolu_prev1", "content": []map[string]any{{"type": "text", "text": "PURPLE-42"}}},
	}})
	streamCursor(t, newCursorTestClient(rt), "other-provider-history", "composer-2.5", conv)
	text := rt.conn(0).sentMessages()[0].GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	for _, want := range []string{
		"<conversation_history>\n<user>\nUse the lookup_secret tool for 'alpha'.\n</user>",
		`[tool call lookup_secret id=toolu_prev1] {"key":"alpha"}`,
		"<tool_result id=toolu_prev1>\nPURPLE-42\n</tool_result>",
		"</conversation_history>\n\nContinue the task above from where it left off.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("transcript missing %q:\n%s", want, text)
		}
	}
}

func TestCursorStreamSideRequestKeepsHeldRun(t *testing.T) {
	rt := loadCursorFixtures(t, "tool-roundtrip-1", "plain-text")
	c := newCursorTestClient(rt)
	main := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools, Thinking: true}
	main.user("Use the lookup_secret tool to get the secret for key 'alpha' and tell me the value.")
	a := streamCursor(t, c, "session-1", "composer-2.5", main)
	main.assistant(a)
	main.toolResults(a, cursorTestToolRunner)

	// Same Claude Code session id, different conversation (title generation, subagent...).
	side := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools}
	side.user("Reply with one short sentence: what is 2+2?")
	streamCursor(t, c, "session-1", "composer-2.5", side)

	b := streamCursor(t, c, "session-1", "composer-2.5", main)
	if rt.opened() != 2 || !strings.Contains(b.text(), "PURPLE-42") {
		t.Fatalf("held Run disturbed: opened %d, text %q", rt.opened(), b.text())
	}
}

func TestCursorStreamErrorBeforeOutput(t *testing.T) {
	rt := &cursorReplayTransport{t: t, fixtures: []*cursorfixture.Fixture{{
		Format:    cursorfixture.Format,
		EndStream: json.RawMessage(`{"error":{"code":"resource_exhausted","message":"Error"}}`),
	}}}
	conv := &cursorTestConversation{}
	conv.user("hi")
	var events int
	err := newCursorTestClient(rt).Stream(context.Background(), CursorStreamRequest{ConversationKey: "k", Model: "m", Body: conv.body("m")}, func(CursorClaudeEvent) error {
		events++
		return nil
	})
	var se *CursorStatusError
	if !errors.As(err, &se) || se.Code != "resource_exhausted" || events != 0 {
		t.Fatalf("err = %v, events = %d", err, events)
	}
}

func TestCursorStreamRejectsBadRequests(t *testing.T) {
	c := newCursorTestClient(&cursorReplayTransport{t: t})
	for _, body := range []string{`not json`, `{"messages":[]}`, `{"messages":[{"role":"assistant","content":"x"}]}`} {
		if err := c.Stream(context.Background(), CursorStreamRequest{Body: []byte(body)}, func(CursorClaudeEvent) error { return nil }); err == nil {
			t.Errorf("%s: no error", body)
		}
	}
}

func TestCursorStreamRequestsMaxMode(t *testing.T) {
	for _, maxMode := range []bool{true, false} {
		rt := loadCursorFixtures(t, "plain-text")
		conv := &cursorTestConversation{System: cursorTestSystem}
		conv.user("Reply with one short sentence: what is 2+2?")
		err := newCursorTestClient(rt).Stream(context.Background(), CursorStreamRequest{ConversationKey: "k", Model: "composer-2.5", MaxMode: maxMode, Body: conv.body("composer-2.5")}, func(CursorClaudeEvent) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if got := rt.conn(0).sentMessages()[0].GetRunRequest().GetRequestedModel().GetMaxMode(); got != maxMode {
			t.Fatalf("requested max_mode = %v, want %v", got, maxMode)
		}
	}
}

// Cursor thinking has no Anthropic signature, and the emitter must not invent
// one: an unsigned block is what lets the claude executor recognize and drop
// it when the session switches back to a Claude model.
func TestCursorStreamThinkingCarriesNoSignature(t *testing.T) {
	rt := loadCursorFixtures(t, "plain-text")
	conv := &cursorTestConversation{System: cursorTestSystem, Thinking: true}
	conv.user("Reply with one short sentence: what is 2+2?")
	var events []CursorClaudeEvent
	err := newCursorTestClient(rt).Stream(context.Background(), CursorStreamRequest{ConversationKey: "k", Model: "composer-2.5", Body: conv.body("composer-2.5")}, func(ev CursorClaudeEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sawThinking := false
	for _, ev := range events {
		sawThinking = sawThinking || strings.Contains(string(ev.Data), `"thinking_delta"`)
		if strings.Contains(string(ev.Data), "signature") {
			t.Fatalf("event carries a signature: %s", ev.Data)
		}
	}
	if !sawThinking {
		t.Fatal("fixture produced no thinking to check")
	}
}
