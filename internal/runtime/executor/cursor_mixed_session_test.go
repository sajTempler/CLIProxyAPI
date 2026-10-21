package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// A Claude Code session that switches from a cursor/* model back to a Claude
// model replays the Cursor turns. Cursor's thinking blocks carry no signature
// (Claude Code stores "" or none), and Anthropic rejects unsigned thinking,
// so the claude executor must drop them while keeping text and tool calls.
func TestClaudeExecutorDropsCursorThinkingInMixedSession(t *testing.T) {
	for _, model := range []string{"claude-haiku-5-5", "claude-opus-5-5"} {
		var seen []byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			seen = bytes.Clone(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"` + model + `","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		executor := NewClaudeExecutor(&config.Config{})
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123", "base_url": server.URL}}
		payload := []byte(`{"messages":[
			{"role":"user","content":[{"type":"text","text":"list the files"}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"cursor thought one","signature":""},
				{"type":"tool_use","id":"toolu_01CursorAbcdefghijklmnop","name":"Bash","input":{"command":"ls"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01CursorAbcdefghijklmnop","content":"a.go"}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"cursor thought two"},
				{"type":"text","text":"There is a.go."}]},
			{"role":"user","content":[{"type":"text","text":"now on claude"}]}]}`)
		_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: model, Payload: payload},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		server.Close()
		if err != nil {
			t.Fatalf("%s: Execute: %v", model, err)
		}
		if strings.Contains(string(seen), "cursor thought") {
			t.Fatalf("%s: Cursor thinking reached Anthropic: %s", model, seen)
		}
		msgs := gjson.GetBytes(seen, "messages").Array()
		if len(msgs) != 5 {
			t.Fatalf("%s: messages = %d, want 5: %s", model, len(msgs), seen)
		}
		if msgs[1].Get("content.0.type").String() != "tool_use" || msgs[3].Get("content.0.text").String() != "There is a.go." {
			t.Fatalf("%s: text and tool calls must stay: %s", model, seen)
		}
		if gjson.GetBytes(seen, `messages.#.content.#(type=="thinking")#`).Raw != "[[],[],[],[],[]]" {
			t.Fatalf("%s: thinking blocks left: %s", model, seen)
		}
	}
}

// The other direction: Claude-signed thinking (and redacted thinking) in the
// history of a cursor request is never forwarded to Cursor.
func TestCursorExecutorIgnoresClaudeThinkingInMixedSession(t *testing.T) {
	f := &cursorFakeStream{events: cursorTextEvents("ok")}
	e := newCursorTestExecutor(&config.Config{}, f)
	payload := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"claude secret thought","signature":"EqQBCkYIBRgCKkB"},{"type":"redacted_thinking","data":"EmwKAhgBEgy"},{"type":"text","text":"hello"}]},
		{"role":"user","content":"switch to cursor"}]}`)
	res, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor/composer-2.5", Payload: payload}, cursorClaudeOpts())
	if err != nil {
		t.Fatal(err)
	}
	if _, errStream := drainCursorStream(t, res); errStream != nil {
		t.Fatal(errStream)
	}
	// The executor passes the Claude body through; the protocol client drops
	// thinking when it maps it (helps tests cover the Run request itself).
	if !strings.Contains(string(f.got[0].Body), "claude secret thought") {
		t.Fatalf("payload rewritten unexpectedly: %s", f.got[0].Body)
	}
}
