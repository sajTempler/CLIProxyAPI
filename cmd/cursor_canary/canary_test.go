package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		typ := ""
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(e), &probe) == nil {
			typ = probe.Type
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", typ, e)
	}
	return b.String()
}

func toolUseStream(id string) string {
	return sse(
		`{"type":"message_start","message":{"id":"m1","role":"assistant","content":[]}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"`+id+`","name":"lookup_secret","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"key\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"alpha\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	)
}

func textStream(text, stop string) string {
	return sse(
		`{"type":"message_start","message":{"id":"m2","role":"assistant","content":[]}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"`+stop+`"}}`,
		`{"type":"message_stop"}`,
	)
}

const modelsWithCursor = `{"object":"list","data":[{"id":"claude-haiku-5-5","owned_by":"anthropic"},{"id":"cursor/gpt-5.4-mini","owned_by":"cursor"},{"id":"cursor/composer-2.5","owned_by":"cursor"}]}`

// fakeProxy serves /v1/models and two /v1/messages turns.
type fakeProxy struct {
	models       string
	turns        []func(w http.ResponseWriter, body []byte)
	calls        int
	lastAuth     string
	secondTurnOK bool
}

func (f *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.lastAuth = r.Header.Get("Authorization")
	switch r.URL.Path {
	case "/v1/models":
		_, _ = io.WriteString(w, f.models)
	case "/v1/messages":
		body, _ := io.ReadAll(r.Body)
		if f.calls >= len(f.turns) {
			http.Error(w, "unexpected turn", http.StatusInternalServerError)
			return
		}
		turn := f.turns[f.calls]
		f.calls++
		turn(w, body)
	default:
		http.NotFound(w, r)
	}
}

func stream(s string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, s)
	}
}

func status(code int, body string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func runAgainst(t *testing.T, p *fakeProxy) Result {
	t.Helper()
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := &canary{baseURL: srv.URL, key: "test-key", client: srv.Client()}
	return c.Run(context.Background(), "")
}

func TestCanaryHappyPath(t *testing.T) {
	p := &fakeProxy{models: modelsWithCursor}
	p.turns = []func(http.ResponseWriter, []byte){
		stream(toolUseStream("toolu_1")),
		func(w http.ResponseWriter, body []byte) {
			// Turn 2 must replay the tool_use and carry the tool_result for the same id.
			var req struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			_ = json.Unmarshal(body, &req)
			if req.Model == "cursor/composer-2.5" && len(req.Messages) == 3 &&
				strings.Contains(string(req.Messages[1].Content), `"id":"toolu_1"`) &&
				strings.Contains(string(req.Messages[2].Content), `"tool_use_id":"toolu_1"`) {
				p.secondTurnOK = true
			}
			stream(textStream("PURPLE-42", "end_turn"))(w, body)
		},
	}
	res := runAgainst(t, p)
	if res.Code != ExitOK {
		t.Fatalf("result = %v", res)
	}
	if !p.secondTurnOK {
		t.Fatal("second turn did not carry the tool_use/tool_result pair for composer-2.5")
	}
	if p.lastAuth != "Bearer test-key" {
		t.Fatal("canary did not authenticate with the proxy key")
	}
}

func TestCanaryClassification(t *testing.T) {
	cases := []struct {
		name   string
		models string
		turns  []func(http.ResponseWriter, []byte)
		want   int
	}{
		{"no cursor models", `{"data":[{"id":"claude-haiku-5-5","owned_by":"anthropic"}]}`, nil, ExitNotRegistered},
		{"unknown provider", modelsWithCursor, []func(http.ResponseWriter, []byte){
			status(400, `{"type":"error","error":{"type":"invalid_request_error","message":"unknown provider for model cursor/composer-2.5"}}`),
		}, ExitNotRegistered},
		{"upstream 401", modelsWithCursor, []func(http.ResponseWriter, []byte){
			status(401, `{"type":"error","error":{"type":"authentication_error","message":"cursor: unauthenticated"}}`),
		}, ExitAuthExpired},
		{"proxy key rejected", modelsWithCursor, []func(http.ResponseWriter, []byte){
			status(401, `{"error":"Invalid API key"}`),
		}, ExitInconclusive},
		{"no auth available", modelsWithCursor, []func(http.ResponseWriter, []byte){
			status(503, `{"error":{"message":"auth_unavailable: no auth available"}}`),
		}, ExitAuthExpired},
		{"rate limited", modelsWithCursor, []func(http.ResponseWriter, []byte){
			status(429, `{"error":{"message":"slow down"}}`),
		}, ExitInconclusive},
		{"upstream 502", modelsWithCursor, []func(http.ResponseWriter, []byte){
			status(502, `{"error":{"message":"connect: protocol error"}}`),
		}, ExitProtocol},
		{"text instead of tool_use", modelsWithCursor, []func(http.ResponseWriter, []byte){
			stream(textStream("I cannot call tools", "end_turn")),
		}, ExitProtocol},
		{"truncated stream", modelsWithCursor, []func(http.ResponseWriter, []byte){
			stream(sse(`{"type":"message_start","message":{}}`)),
		}, ExitProtocol},
		{"stream error event", modelsWithCursor, []func(http.ResponseWriter, []byte){
			stream(sse(`{"type":"message_start","message":{}}`, `{"type":"error","error":{"type":"api_error","message":"exec type unknown"}}`)),
		}, ExitProtocol},
		{"tool called again", modelsWithCursor, []func(http.ResponseWriter, []byte){
			stream(toolUseStream("toolu_1")), stream(toolUseStream("toolu_2")),
		}, ExitProtocol},
		{"answer missing secret", modelsWithCursor, []func(http.ResponseWriter, []byte){
			stream(toolUseStream("toolu_1")), stream(textStream("no idea", "end_turn")),
		}, ExitProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runAgainst(t, &fakeProxy{models: tc.models, turns: tc.turns})
			if res.Code != tc.want {
				t.Fatalf("result = %v, want exit %d", res, tc.want)
			}
		})
	}
}

func TestCanaryProxyDownIsInconclusive(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &canary{baseURL: url, key: "k", client: http.DefaultClient}
	if res := c.Run(context.Background(), ""); res.Code != ExitInconclusive {
		t.Fatalf("result = %v", res)
	}
}

func TestRunReadsKeyFromEnvFileWithoutPrintingIt(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("OTHER=1\nCPA_API_KEY=\"sekrit-value\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &fakeProxy{models: `{"data":[]}`}
	srv := httptest.NewServer(p)
	defer srv.Close()
	res := run([]string{"--base-url", srv.URL, "--env-file", envPath}, func(string) string { return "" })
	if res.Code != ExitNotRegistered {
		t.Fatalf("result = %v", res)
	}
	if p.lastAuth != "Bearer sekrit-value" {
		t.Fatal("key from env file not used")
	}
	if strings.Contains(res.String(), "sekrit") {
		t.Fatal("result leaks the key")
	}

	res = run([]string{"--env-file", filepath.Join(dir, "missing")}, func(string) string { return "" })
	if res.Code != ExitInconclusive {
		t.Fatalf("missing key result = %v", res)
	}
}
