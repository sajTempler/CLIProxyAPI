// Command cursor_canary checks the cursor provider end to end through the
// running proxy, the same way `claudep` (Claude Code via the proxy) uses it.
//
// One canary is two /v1/messages streaming turns on a Cursor model with one tool:
//
//  1. user asks to call lookup_secret{key:"alpha"} -> expect tool_use, stop_reason tool_use
//  2. send the tool_result "PURPLE-42" -> expect a final text answer containing it, end_turn
//
// Usage (from the repository root):
//
//	go run ./cmd/cursor_canary [--model cursor/composer-2.5] [--base-url http://127.0.0.1:8317] [--env-file .env]
//
// The proxy key is CPA_API_KEY from the environment, else from --env-file. It
// is sent as a Bearer token (like claudep) and never printed.
//
// Exit codes: 0 ok, 1 protocol broken, 2 auth expired, 3 cursor provider not
// registered (expected until the provider lands; must not alert), 4
// inconclusive (proxy down, key missing or rejected, rate limited).
//
// The --timeout deadline is deliberate: this is a scheduled utility, not
// the proxy runtime, and a hung canary must not block the launchd job.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultBaseURL    = "http://127.0.0.1:8317"
	cursorModelPrefix = "cursor/"
	canaryToolName    = "lookup_secret"
	canarySecret      = "PURPLE-42"
)

// preferredModels are tried in order when --model is not set (cheapest first).
var preferredModels = []string{"cursor/composer-2.5", "cursor/claude-haiku-4-5", "cursor/gpt-5.4-mini"}

func main() {
	res := run(os.Args[1:], os.Getenv)
	fmt.Println("cursor_canary:", res)
	os.Exit(res.Code)
}

func run(args []string, getenv func(string) string) Result {
	fs := flag.NewFlagSet("cursor_canary", flag.ContinueOnError)
	baseURL := fs.String("base-url", firstNonEmpty(getenv("CURSOR_CANARY_BASE_URL"), defaultBaseURL), "proxy base URL")
	envFile := fs.String("env-file", ".env", "file with CPA_API_KEY when it is not in the environment")
	model := fs.String("model", "", "cursor model id (default: cheapest offered)")
	timeout := fs.Duration("timeout", 3*time.Minute, "overall deadline")
	if err := fs.Parse(args); err != nil {
		return fail(ExitInconclusive, "bad flags: %v", err)
	}
	key := strings.TrimSpace(getenv("CPA_API_KEY"))
	if key == "" {
		var errKey error
		key, errKey = readEnvKey(*envFile, "CPA_API_KEY")
		if errKey != nil {
			return fail(ExitInconclusive, "CPA_API_KEY not set and not readable from %s: %v", *envFile, errKey)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c := &canary{baseURL: strings.TrimRight(*baseURL, "/"), key: key, client: http.DefaultClient}
	return c.Run(ctx, *model)
}

type canary struct {
	baseURL string
	key     string
	client  *http.Client
}

// Run executes the canary and returns its verdict.
func (c *canary) Run(ctx context.Context, wantModel string) Result {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return fail(ExitInconclusive, "proxy unreachable at %s: %v", c.baseURL, err)
	}
	model, res := classifyModels(status, body, wantModel)
	if res.Code != ExitOK {
		return res
	}

	user := map[string]any{"role": "user", "content": fmt.Sprintf("Call the %s tool with key \"alpha\". Then reply with exactly the value it returns and nothing else.", canaryToolName)}
	first, res := c.turn(ctx, model, []any{user})
	if res.Code != ExitOK {
		return res
	}
	call, res := classifyFirstTurn(first)
	if res.Code != ExitOK {
		return res
	}

	assistant := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": call.Input},
	}}
	result := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": call.ID, "content": canarySecret},
	}}
	final, res := c.turn(ctx, model, []any{user, assistant, result})
	if res.Code != ExitOK {
		return res
	}
	res = classifyFinalTurn(final)
	if res.Code == ExitOK {
		res.Reason = model + ": " + res.Reason
	}
	return res
}

func (c *canary) turn(ctx context.Context, model string, messages []any) (*Turn, Result) {
	req := map[string]any{
		"model":      model,
		"max_tokens": 1024,
		"stream":     true,
		"system":     "You are a protocol canary. Use the provided tool when asked; answer tersely.",
		"tools": []any{map[string]any{
			"name":        canaryToolName,
			"description": "Look up a secret value by key.",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"key": map[string]any{"type": "string"}},
				"required":   []string{"key"},
			},
		}},
		"messages": messages,
	}
	payload, errJSON := json.Marshal(req)
	if errJSON != nil {
		return nil, fail(ExitInconclusive, "encode request: %v", errJSON)
	}
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(payload))
	if errReq != nil {
		return nil, fail(ExitInconclusive, "build request: %v", errReq)
	}
	c.headers(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, errDo := c.client.Do(httpReq)
	if errDo != nil {
		if ctx.Err() != nil {
			return nil, fail(ExitProtocol, "no complete response before the deadline: %v", errDo)
		}
		return nil, fail(ExitInconclusive, "proxy unreachable at %s: %v", c.baseURL, errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, classifyHTTPError(resp.StatusCode, body)
	}
	t, errParse := ParseSSE(resp.Body)
	if errParse != nil {
		return nil, fail(ExitProtocol, "%v", errParse)
	}
	return t, ok("")
}

func (c *canary) do(ctx context.Context, method, path string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	c.headers(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, err
}

func (c *canary) headers(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", "cursor-canary")
}

// readEnvKey returns KEY's value from a dotenv file without logging it.
func readEnvKey(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "export ")
		if v, found := strings.CutPrefix(line, key+"="); found {
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if v == "" {
				break
			}
			return v, nil
		}
	}
	if err = sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s not found", key)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
