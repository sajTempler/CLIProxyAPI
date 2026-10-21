package helps

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// CursorClient turns Claude Messages requests into Cursor Runs and Run
// streams into Claude stream events, keeping per-conversation state in a
// CursorSessionStore. See docs/cursor/PLAN.md (Phase 2) and SPIKE.md.
type CursorClient struct {
	transport cursorTransport
	sessions  *CursorSessionStore
	// Heartbeat is the client heartbeat interval on open Runs; 0 disables it.
	Heartbeat time.Duration
	// Observe, when set, returns an observer for each Run (fixture recording).
	Observe func(conversationKey, model string) CursorFrameObserver
}

// NewCursorClient returns a client over an HTTP transport and a session store.
func NewCursorClient(t *CursorHTTPTransport, sessions *CursorSessionStore) *CursorClient {
	return newCursorClient(t, sessions)
}

func newCursorClient(t cursorTransport, sessions *CursorSessionStore) *CursorClient {
	if sessions == nil {
		sessions = NewCursorSessionStore(0, 0, nil)
	}
	return &CursorClient{transport: t, sessions: sessions, Heartbeat: DefaultCursorHeartbeat}
}

// CursorStreamRequest is one Claude Messages request bound for Cursor.
type CursorStreamRequest struct {
	// ConversationKey namespaces conversation state (e.g. the Claude Code
	// session id). State is kept per key and first message, so side requests
	// and subagents sharing a session id do not disturb each other. Empty
	// means a one-off request with no reusable state.
	ConversationKey string
	// Model is the Cursor model id (e.g. "composer-2.5").
	Model string
	// ResponseModel is the model name reported to the client (e.g.
	// "cursor/composer-2.5"); empty reports Model.
	ResponseModel string
	// Body is the Claude Messages request JSON.
	Body []byte
}

// CursorClaudeEvent is one Claude streaming event (type + JSON data).
type CursorClaudeEvent struct {
	Type string
	Data []byte
}

// SSE renders the event as a Claude server-sent event.
func (e CursorClaudeEvent) SSE() []byte {
	return []byte("event: " + e.Type + "\ndata: " + string(e.Data) + "\n\n")
}

// CursorStopReason values the client ends a response with.
const (
	cursorStopEndTurn = "end_turn"
	cursorStopToolUse = "tool_use"
)

// Stream runs one Claude request and emits its Claude stream events. It
// returns an error before any event when the request cannot start (the
// caller maps it to an HTTP status); after events have been emitted, errors
// are returned too and the caller ends the stream with a Claude error event.
func (c *CursorClient) Stream(ctx context.Context, req CursorStreamRequest, emit func(CursorClaudeEvent) error) error {
	creq, err := parseCursorClaudeRequest(req.Body)
	if err != nil {
		return err
	}
	key := req.ConversationKey
	if key == "" {
		key = uuid.NewString() // one-off: nothing will continue this state
	}
	sess, err := c.sessions.acquire(ctx, key+"|"+creq.identity())
	if err != nil {
		return err
	}
	defer c.sessions.release(sess)

	if err = c.awaitFinishing(ctx, sess); err != nil {
		return err
	}
	shownModel := req.ResponseModel
	if shownModel == "" {
		shownModel = req.Model
	}
	em := newCursorClaudeEmitter(shownModel, emit)
	em.thinkingEnabled = creq.Thinking
	n := len(creq.Messages)
	results := creq.toolResults()
	continuing := n > 1 && sess.nextLen == n
	rctx := cursorRunContext{requestContext: cursorRequestContext(creq), definitions: cursorMCPDefinitions(creq)}

	prevPending := sess.pending
	run, err := c.prepareRun(ctx, sess, req, creq, continuing, results, rctx)
	if err != nil {
		return err
	}
	stop, err := run.pump(ctx, em, sess)
	if err != nil {
		run.close()
		sess.live = nil
		if em.started {
			// Partial output reached Claude Code; this state no longer matches its history.
			sess.closeRuns()
			sess.resetState()
		} else {
			// Nothing reached Claude Code: a retry of this request can still
			// resume from the checkpoint with the same pending calls.
			sess.pending = prevPending
		}
		return err
	}
	sess.nextLen = n + 2
	if stop == cursorStopToolUse {
		sess.live = run
		return nil
	}
	sess.live, sess.pending = nil, nil
	sess.finishing = run
	run.finishSend()
	return nil
}

// awaitFinishing waits for the previous Run's final checkpoint (it arrives
// after turn_ended, while Claude Code is already reading the response). The
// server ends that stream itself once we half-close; nothing here times out.
func (c *CursorClient) awaitFinishing(ctx context.Context, sess *cursorSession) error {
	if sess.finishing == nil {
		return nil
	}
	select {
	case <-sess.finishing.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	sess.finishing.close()
	sess.finishing = nil
	return nil
}

// prepareRun picks how this request continues the conversation:
//  1. tool_result for a held-open Run: answer the pending calls on it;
//  2. tool_result, Run lost, checkpoint kept: resume_action, answering the
//     regenerated calls from the cached results;
//  3. new user turn with a checkpoint: new Run on the checkpoint;
//  4. otherwise (new conversation, or history Cursor never saw): new
//     conversation, prior turns seeded as a transcript.
func (c *CursorClient) prepareRun(ctx context.Context, sess *cursorSession, req CursorStreamRequest, creq *cursorClaudeRequest, continuing bool, results []cursorClaudeBlock, rctx cursorRunContext) (*cursorLiveRun, error) {
	if continuing && len(results) > 0 && sess.live != nil && sess.live.alive() {
		run := sess.live
		run.setContext(rctx)
		if err := c.answerPending(run, sess, creq, results); err != nil {
			return nil, err
		}
		return run, nil
	}
	if sess.live != nil {
		// A held Run that this request does not continue (new user text after a
		// tool_use, or the Run died): cancel it; its checkpoint stays usable.
		if sess.live.alive() {
			_ = sess.live.send(&cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ConversationAction{ConversationAction: &cursorpb.ConversationAction{
				Action: &cursorpb.ConversationAction_CancelAction{CancelAction: &cursorpb.CancelAction{Reason: "superseded by a new user turn"}},
			}}})
		}
		sess.live.close()
		sess.live = nil
	}

	checkpoint := sess.usableCheckpoint()
	var action *cursorpb.ConversationAction
	switch {
	case continuing && len(results) > 0 && checkpoint != nil && len(sess.pending) > 0:
		answers := map[string]cursorToolAnswer{}
		byID := cursorResultsByID(results, creq.lastUserText(), creq.lastUserImageBlocks())
		for _, p := range sess.pending {
			if a, ok := byID[p.ToolUseID]; ok {
				answers[p.fingerprint()] = a
			}
		}
		rctx.resumeAnswers = answers
		action = &cursorpb.ConversationAction{Action: &cursorpb.ConversationAction_ResumeAction{ResumeAction: &cursorpb.ResumeAction{RequestContext: rctx.requestContext}}}
		log.WithField("pending", len(sess.pending)).Info("cursor: held Run lost; resuming from checkpoint")
	case continuing && len(results) == 0 && checkpoint != nil:
		action = cursorUserAction(creq.lastUserText(), creq.lastUserImageBlocks(), rctx.requestContext)
	default:
		if n := len(creq.Messages); n > 1 {
			log.WithField("messages", n).Debug("cursor: no usable state; seeding transcript")
		}
		sess.closeRuns()
		sess.resetState()
		checkpoint = nil
		text := creq.lastUserText()
		if len(creq.Messages) > 1 {
			text = cursorTranscript(creq.Messages)
		}
		// The last message's images ride along (tool_result images too, as the
		// transcript inlines its results); earlier ones stay placeholders.
		action = cursorUserAction(text, cursorMessageImages(creq.Messages[len(creq.Messages)-1]), rctx.requestContext)
	}
	sess.pending = nil

	state := &cursorpb.ConversationStateStructure{}
	if checkpoint != nil {
		if err := proto.Unmarshal(checkpoint, state); err != nil {
			return nil, fmt.Errorf("cursor: stored checkpoint: %w", err)
		}
	}
	convID := sess.convID()
	first := &cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_RunRequest{RunRequest: &cursorpb.AgentRunRequest{
		ConversationState:   state,
		Action:              action,
		McpTools:            &cursorpb.McpTools{},
		ConversationId:      proto.String(convID),
		ConversationGroupId: proto.String(convID),
		RunId:               proto.String(uuid.NewString()),
		RequestedModel:      &cursorpb.RequestedModel{ModelId: req.Model, MaxMode: false},
	}}}
	var observer CursorFrameObserver
	if c.Observe != nil {
		observer = c.Observe(req.ConversationKey, req.Model)
	}
	return startCursorRun(ctx, c.transport, sess, first, rctx, observer, c.Heartbeat)
}

// cursorUserAction is a new user turn; images go inline as selected images,
// as the agent CLI sends them.
func cursorUserAction(text string, images []cursorImage, rc *cursorpb.RequestContext) *cursorpb.ConversationAction {
	msg := &cursorpb.UserMessage{Text: text, MessageId: uuid.NewString(), Mode: cursorpb.AgentMode_AGENT_MODE_AGENT}
	if len(images) > 0 {
		selected := make([]*cursorpb.SelectedImage, 0, len(images))
		for _, img := range images {
			selected = append(selected, &cursorpb.SelectedImage{
				DataOrBlobId: &cursorpb.SelectedImage_Data{Data: img.Data},
				Uuid:         uuid.NewString(),
				MimeType:     img.MediaType,
			})
		}
		msg.SelectedContext = &cursorpb.SelectedContext{SelectedImages: selected}
	}
	return &cursorpb.ConversationAction{Action: &cursorpb.ConversationAction_UserMessageAction{UserMessageAction: &cursorpb.UserMessageAction{
		UserMessage:    msg,
		RequestContext: rc,
	}}}
}

// cursorResultsByID maps tool_use ids to their results. Text and images the
// user message carries next to the results (Claude Code system reminders, a
// pasted image) have no slot on a held Run, so they ride along with the last
// result.
func cursorResultsByID(results []cursorClaudeBlock, extra string, extraImages []cursorImage) map[string]cursorToolAnswer {
	out := make(map[string]cursorToolAnswer, len(results))
	for i, r := range results {
		ans := cursorToolAnswer{text: r.Text, isError: r.IsError, images: r.Images}
		if i == len(results)-1 {
			if extra != "" {
				ans.text += "\n\n" + extra
			}
			ans.images = append(append([]cursorImage(nil), ans.images...), extraImages...)
		}
		out[r.ToolUseID] = ans
	}
	return out
}

// answerPending sends an mcp_result for every call the last response ended
// with. A call without a matching tool_result gets an error result so the
// server is never left waiting.
func (c *CursorClient) answerPending(run *cursorLiveRun, sess *cursorSession, creq *cursorClaudeRequest, results []cursorClaudeBlock) error {
	byID := cursorResultsByID(results, creq.lastUserText(), creq.lastUserImageBlocks())
	for _, p := range sess.pending {
		ans, ok := byID[p.ToolUseID]
		if !ok {
			ans = cursorToolAnswer{text: "No result was returned for this tool call.", isError: true}
		}
		if err := run.answerTool(p, ans); err != nil {
			return fmt.Errorf("cursor: send tool result: %w", err)
		}
	}
	sess.pending = nil // Stream restores them if the Run fails before output
	return nil
}

// pump reads Run events into Claude events until the turn ends or the
// response must stop for tool results. Cursor dispatches the calls of one
// model step one at a time: the next mcp_args only arrives after the previous
// call's mcp_result (recorded fixtures two-tools*), so waiting for more calls
// cannot help. The response stops with tool_use as soon as a call is pending
// and nothing else is queued; a later call of the same step becomes the next
// response's tool_use. tool_requests_listed is not used: its count covers the
// whole step, including calls answered in earlier responses.
func (r *cursorLiveRun) pump(ctx context.Context, em *cursorClaudeEmitter, sess *cursorSession) (string, error) {
	em.priorOutput = r.reportedOutput
	for {
		if len(sess.pending) > 0 && r.events.empty() {
			r.reportedOutput += em.outputTokens
			return cursorStopToolUse, em.finish(cursorStopToolUse, nil)
		}
		ev, err := r.events.pop(ctx)
		if err != nil {
			return "", err
		}
		switch ev.kind {
		case cursorEvText:
			if err = em.text(ev.text); err != nil {
				return "", err
			}
		case cursorEvThinking:
			if err = em.thinking(ev.text); err != nil {
				return "", err
			}
		case cursorEvThinkingDone:
			if err = em.closeBlock(); err != nil {
				return "", err
			}
		case cursorEvTokens:
			em.outputTokens += ev.count
		case cursorEvToolCall:
			sess.pending = append(sess.pending, ev.call)
			if err = em.toolUse(ev.call); err != nil {
				return "", err
			}
		case cursorEvTurnEnded:
			return cursorStopEndTurn, em.finish(cursorStopEndTurn, ev.usage)
		case cursorEvEnd:
			if ev.err != nil {
				return "", ev.err
			}
			if !em.started {
				return "", errors.New("cursor: run ended without output")
			}
			return cursorStopEndTurn, em.finish(cursorStopEndTurn, nil)
		}
	}
}

func (q *cursorEventQueue) empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) == 0
}

const cursorIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func cursorRandomID(n int) string {
	var sb strings.Builder
	max := big.NewInt(int64(len(cursorIDAlphabet)))
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return strings.ReplaceAll(uuid.NewString(), "-", "")[:n]
		}
		sb.WriteByte(cursorIDAlphabet[v.Int64()])
	}
	return sb.String()
}

// newCursorToolUseID returns a Claude-shaped tool_use id.
var newCursorToolUseID = func() string { return "toolu_" + cursorRandomID(24) }

// newCursorMessageID returns a Claude-shaped message id.
var newCursorMessageID = func() string { return "msg_" + cursorRandomID(24) }

// cursorClaudeEmitter writes Claude stream events for one response.
type cursorClaudeEmitter struct {
	emit         func(CursorClaudeEvent) error
	model        string
	started      bool
	index        int
	open         string // type of the open content block, "" when none
	outputTokens int64
	priorOutput  int64 // output tokens earlier responses of the same Run reported
	// thinkingEnabled surfaces thinking deltas as thinking blocks; when the
	// request did not ask for thinking they are dropped.
	thinkingEnabled bool
}

func newCursorClaudeEmitter(model string, emit func(CursorClaudeEvent) error) *cursorClaudeEmitter {
	return &cursorClaudeEmitter{emit: emit, model: model}
}

func (e *cursorClaudeEmitter) send(typ string, data map[string]any) error {
	data["type"] = typ
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return e.emit(CursorClaudeEvent{Type: typ, Data: raw})
}

func (e *cursorClaudeEmitter) start() error {
	if e.started {
		return nil
	}
	e.started = true
	return e.send("message_start", map[string]any{"message": map[string]any{
		"id": newCursorMessageID(), "type": "message", "role": "assistant", "model": e.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	}})
}

func (e *cursorClaudeEmitter) openBlock(kind string, block map[string]any) error {
	if e.open == kind {
		return nil
	}
	if err := e.closeBlock(); err != nil {
		return err
	}
	if err := e.start(); err != nil {
		return err
	}
	e.open = kind
	return e.send("content_block_start", map[string]any{"index": e.index, "content_block": block})
}

func (e *cursorClaudeEmitter) closeBlock() error {
	if e.open == "" {
		return nil
	}
	e.open = ""
	err := e.send("content_block_stop", map[string]any{"index": e.index})
	e.index++
	return err
}

func (e *cursorClaudeEmitter) text(s string) error {
	if s == "" {
		return nil
	}
	if err := e.openBlock("text", map[string]any{"type": "text", "text": ""}); err != nil {
		return err
	}
	return e.send("content_block_delta", map[string]any{"index": e.index, "delta": map[string]any{"type": "text_delta", "text": s}})
}

func (e *cursorClaudeEmitter) thinking(s string) error {
	if s == "" || !e.thinkingEnabled {
		return nil
	}
	if err := e.openBlock("thinking", map[string]any{"type": "thinking", "thinking": ""}); err != nil {
		return err
	}
	return e.send("content_block_delta", map[string]any{"index": e.index, "delta": map[string]any{"type": "thinking_delta", "thinking": s}})
}

func (e *cursorClaudeEmitter) toolUse(c *cursorToolCall) error {
	if err := e.closeBlock(); err != nil {
		return err
	}
	if err := e.openBlock("tool_use", map[string]any{"type": "tool_use", "id": c.ToolUseID, "name": c.Name, "input": map[string]any{}}); err != nil {
		return err
	}
	if err := e.send("content_block_delta", map[string]any{"index": e.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(c.Args)}}); err != nil {
		return err
	}
	return e.closeBlock()
}

// finish closes the response. usage is the Run's turn_ended when the Run
// ended: input and cache counts are cumulative over the Run's steps and go on
// this final response; output is reduced by what earlier tool_use responses
// of the same Run already reported, so per-response usage sums to the Run's.
// A tool_use response only knows the output tokens streamed for it.
func (e *cursorClaudeEmitter) finish(stop string, usage *cursorpb.TurnEndedUpdate) error {
	if err := e.closeBlock(); err != nil {
		return err
	}
	if err := e.start(); err != nil {
		return err
	}
	u := map[string]any{"input_tokens": 0, "output_tokens": e.outputTokens}
	if usage != nil {
		cacheRead, cacheWrite := usage.GetCacheReadTokens(), usage.GetCacheWriteTokens()
		u["input_tokens"] = max(usage.GetInputTokens()-cacheRead-cacheWrite, 0)
		u["output_tokens"] = max(usage.GetOutputTokens()-e.priorOutput, 0)
		u["cache_read_input_tokens"] = cacheRead
		u["cache_creation_input_tokens"] = cacheWrite
	}
	if err := e.send("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": u}); err != nil {
		return err
	}
	return e.send("message_stop", map[string]any{})
}
