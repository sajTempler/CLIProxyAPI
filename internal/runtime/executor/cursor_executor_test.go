package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func cursorTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: "cursor-test.json", Provider: "cursor", Metadata: map[string]any{"type": "cursor", "access_token": "test-token", "auth_kind": "agent_import"}}
}

func cursorEv(typ, data string) helps.CursorClaudeEvent {
	return helps.CursorClaudeEvent{Type: typ, Data: []byte(data)}
}

// cursorTextEvents is a complete one-text-block response.
func cursorTextEvents(text string) []helps.CursorClaudeEvent {
	return []helps.CursorClaudeEvent{
		cursorEv("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"cursor/composer-2.5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`),
		cursorEv("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		cursorEv("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`),
		cursorEv("content_block_stop", `{"type":"content_block_stop","index":0}`),
		cursorEv("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":12,"output_tokens":3}}`),
		cursorEv("message_stop", `{"type":"message_stop"}`),
	}
}

type cursorFakeStream struct {
	got    []helps.CursorStreamRequest
	events []helps.CursorClaudeEvent
	err    error
}

func (f *cursorFakeStream) stream(_ context.Context, req helps.CursorStreamRequest, emit func(helps.CursorClaudeEvent) error) error {
	f.got = append(f.got, req)
	for _, ev := range f.events {
		if err := emit(ev); err != nil {
			return err
		}
	}
	return f.err
}

func newCursorTestExecutor(cfg *config.Config, f *cursorFakeStream) *CursorExecutor {
	e := NewCursorExecutor(cfg)
	e.stream = f.stream
	return e
}

func cursorClaudeOpts() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude, Stream: true}
}

func drainCursorStream(t *testing.T, res *cliproxyexecutor.StreamResult) (string, error) {
	t.Helper()
	var sb strings.Builder
	var lastErr error
	for chunk := range res.Chunks {
		if chunk.Err != nil {
			lastErr = chunk.Err
			continue
		}
		sb.Write(chunk.Payload)
	}
	return sb.String(), lastErr
}

// The payload rules are the last semantic step: an override must survive the
// built-in thinking applier (the model suffix turns thinking on) and reach the
// Cursor client unchanged.
func TestCursorExecutorPayloadRulesApplyLast(t *testing.T) {
	body := `{"model":"cursor/composer-2.5","max_tokens":1024,"system":"original system","messages":[{"role":"user","content":"hi"}]}`
	run := func(cfg *config.Config) helps.CursorStreamRequest {
		f := &cursorFakeStream{events: cursorTextEvents("ok")}
		e := newCursorTestExecutor(cfg, f)
		res, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor/composer-2.5(high)", Payload: []byte(body)}, cursorClaudeOpts())
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		if _, errDrain := drainCursorStream(t, res); errDrain != nil {
			t.Fatalf("stream error: %v", errDrain)
		}
		if len(f.got) != 1 {
			t.Fatalf("stream calls = %d, want 1", len(f.got))
		}
		return f.got[0]
	}

	// Control: without rules the suffix enables thinking.
	plain := run(&config.Config{})
	if typ := gjson.GetBytes(plain.Body, "thinking.type").String(); typ != "enabled" && typ != "adaptive" {
		t.Fatalf("control: thinking.type = %q, want enabled/adaptive (body %s)", typ, plain.Body)
	}
	if plain.Model != "composer-2.5" {
		t.Fatalf("cursor model = %q, want composer-2.5", plain.Model)
	}

	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "cursor/*", Protocol: "claude"}},
		Params: map[string]any{"system": "RULE-SYSTEM", "thinking": map[string]any{"type": "disabled"}},
	}}}}
	got := run(cfg)
	if s := gjson.GetBytes(got.Body, "system").String(); s != "RULE-SYSTEM" {
		t.Fatalf("system = %q, want the payload rule's value (body %s)", s, got.Body)
	}
	if typ := gjson.GetBytes(got.Body, "thinking.type").String(); typ != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled from the payload rule (body %s)", typ, got.Body)
	}
}

func TestCursorExecutorStreamsClaudeEventsAndKeys(t *testing.T) {
	f := &cursorFakeStream{events: cursorTextEvents("hello")}
	e := newCursorTestExecutor(&config.Config{}, f)
	opts := cursorClaudeOpts()
	opts.Headers = http.Header{"X-Claude-Code-Session-Id": []string{"sess-1"}}
	res, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor/composer-2.5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	out, errStream := drainCursorStream(t, res)
	if errStream != nil {
		t.Fatalf("stream error: %v", errStream)
	}
	if !strings.Contains(out, "event: message_start\n") || !strings.Contains(out, `"text":"hello"`) || !strings.Contains(out, "event: message_stop\n") {
		t.Fatalf("unexpected SSE: %s", out)
	}
	req := f.got[0]
	if req.ConversationKey != "claude:sess-1:agent:main" {
		t.Fatalf("conversation key = %q", req.ConversationKey)
	}
	if req.ResponseModel != "cursor/composer-2.5" {
		t.Fatalf("response model = %q", req.ResponseModel)
	}
}

func TestCursorExecutorErrorBeforeOutputIsStatus(t *testing.T) {
	cases := []struct {
		err        error
		status     int
		retryAfter bool
	}{
		{&helps.CursorStatusError{Code: "unauthenticated", Message: "bad token"}, http.StatusUnauthorized, false},
		{&helps.CursorStatusError{HTTPStatus: 401, Code: "Unauthorized"}, http.StatusUnauthorized, false},
		{&helps.CursorStatusError{Code: "resource_exhausted", Message: "slow down"}, http.StatusTooManyRequests, true},
		{&helps.CursorStatusError{Code: "permission_denied"}, http.StatusForbidden, false},
		{&helps.CursorStatusError{Code: "invalid_argument"}, http.StatusBadRequest, false},
		{&helps.CursorStatusError{Code: "internal"}, http.StatusBadGateway, false},
		{errors.New("dial failed"), http.StatusBadGateway, false},
		{&helps.CursorRequestError{Msg: "cursor: request has no messages"}, http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		f := &cursorFakeStream{err: tc.err}
		e := newCursorTestExecutor(&config.Config{}, f)
		res, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor/composer-2.5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, cursorClaudeOpts())
		if res != nil || err == nil {
			t.Fatalf("%v: want an error before any chunk, got res=%v err=%v", tc.err, res, err)
		}
		se, ok := err.(statusErr)
		if !ok || se.StatusCode() != tc.status {
			t.Fatalf("%v: got %T %v, want status %d", tc.err, err, err, tc.status)
		}
		if tc.retryAfter && (se.RetryAfter() == nil || *se.RetryAfter() != cursorRateLimitCooldown) {
			t.Fatalf("%v: want a %s cooldown", tc.err, cursorRateLimitCooldown)
		}
	}
}

func TestCursorExecutorMidStreamErrorEndsWithErrorChunk(t *testing.T) {
	f := &cursorFakeStream{events: cursorTextEvents("partial")[:3], err: &helps.CursorStatusError{Code: "unavailable", Message: "gone"}}
	e := newCursorTestExecutor(&config.Config{}, f)
	res, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor/composer-2.5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, cursorClaudeOpts())
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	out, errStream := drainCursorStream(t, res)
	if !strings.Contains(out, "partial") {
		t.Fatalf("partial output missing: %s", out)
	}
	se, ok := errStream.(statusErr)
	if !ok || se.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("final chunk error = %T %v, want 503 statusErr", errStream, errStream)
	}
}

func TestCursorExecutorExecuteCollectsMessage(t *testing.T) {
	events := []helps.CursorClaudeEvent{
		cursorTextEvents("")[0],
		cursorEv("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		cursorEv("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me look."}}`),
		cursorEv("content_block_stop", `{"type":"content_block_stop","index":0}`),
		cursorEv("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}`),
		cursorEv("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"README.md\"}"}}`),
		cursorEv("content_block_stop", `{"type":"content_block_stop","index":1}`),
		cursorEv("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":7}}`),
		cursorEv("message_stop", `{"type":"message_stop"}`),
	}
	f := &cursorFakeStream{events: events}
	e := newCursorTestExecutor(&config.Config{}, f)
	opts := cursorClaudeOpts()
	opts.Stream = false
	resp, err := e.Execute(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor/composer-2.5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	msg := gjson.ParseBytes(resp.Payload)
	if msg.Get("stop_reason").String() != "tool_use" || msg.Get("content.0.text").String() != "Let me look." ||
		msg.Get("content.1.input.file_path").String() != "README.md" || msg.Get("usage.output_tokens").Int() != 7 {
		t.Fatalf("collected message = %s", resp.Payload)
	}
}

func TestCursorExecutorMissingTokenIs401(t *testing.T) {
	e := newCursorTestExecutor(&config.Config{}, &cursorFakeStream{})
	_, err := e.ExecuteStream(context.Background(), &cliproxyauth.Auth{Provider: "cursor"}, cliproxyexecutor.Request{Model: "cursor/composer-2.5", Payload: []byte(`{"messages":[]}`)}, cursorClaudeOpts())
	if se, ok := err.(statusErr); !ok || se.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("err = %v, want 401", err)
	}
}

func TestCursorExecutorRefreshAgentImportExplains(t *testing.T) {
	e := NewCursorExecutor(&config.Config{})
	auth := cursorTestAuth()
	auth.Metadata["expired"] = time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	_, err := e.Refresh(context.Background(), auth)
	if err == nil || !errors.Is(err, cursorauth.ErrReloginRequired) {
		t.Fatalf("err = %v, want ErrReloginRequired", err)
	}
	if !strings.Contains(err.Error(), "agent CLI") || !strings.Contains(err.Error(), "--cursor-import-agent") {
		t.Fatalf("err = %v, want re-login instructions for agent imports", err)
	}
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) && sc.StatusCode() == http.StatusUnauthorized {
		t.Fatalf("refresh error must not be a 401 (the access token is still valid)")
	}
}

func TestCursorExecutorCountTokensEstimates(t *testing.T) {
	e := NewCursorExecutor(&config.Config{})
	payload := []byte(strings.Repeat("x", 400))
	resp, err := e.CountTokens(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor/composer-2.5", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got != 100 {
		t.Fatalf("input_tokens = %d, want 100", got)
	}
}

func TestCursorExecutorIdentity(t *testing.T) {
	e := NewCursorExecutor(&config.Config{})
	if e.Identifier() != "cursor" || e.RequestToFormat(cliproxyexecutor.Request{}, cliproxyexecutor.Options{}) != sdktranslator.FormatClaude {
		t.Fatal("cursor executor must be identified as cursor with the Claude pivot format")
	}
}
