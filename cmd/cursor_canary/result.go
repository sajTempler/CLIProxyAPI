package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// Exit codes. The launchd runner (scripts/cursor/maintenance.sh) depends on them.
const (
	ExitOK            = 0 // tool_use round-trip worked end to end
	ExitProtocol      = 1 // Cursor protocol broken: bad status, malformed stream, missing tool_use or answer
	ExitAuthExpired   = 2 // Cursor credentials rejected upstream; re-login needed
	ExitNotRegistered = 3 // the proxy has no cursor provider (expected until Phase 4); never notifies
	ExitInconclusive  = 4 // canary could not run: proxy down, CPA_API_KEY missing/invalid, rate limited
)

// Result is the canary verdict.
type Result struct {
	Code   int
	Reason string
}

func (r Result) String() string {
	return fmt.Sprintf("%s (exit %d): %s", codeName(r.Code), r.Code, r.Reason)
}

func codeName(code int) string {
	switch code {
	case ExitOK:
		return "OK"
	case ExitProtocol:
		return "PROTOCOL_BROKEN"
	case ExitAuthExpired:
		return "AUTH_EXPIRED"
	case ExitNotRegistered:
		return "NOT_REGISTERED"
	default:
		return "INCONCLUSIVE"
	}
}

func ok(reason string) Result { return Result{Code: ExitOK, Reason: reason} }

func fail(code int, format string, args ...any) Result {
	return Result{Code: code, Reason: fmt.Sprintf(format, args...)}
}

// classifyModels decides from GET /v1/models whether a cursor provider is registered.
// It returns the model to use, or a non-OK result.
func classifyModels(status int, body []byte, want string) (string, Result) {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "", fail(ExitInconclusive, "proxy rejected CPA_API_KEY on /v1/models (HTTP %d)", status)
	case status != http.StatusOK:
		return "", fail(ExitInconclusive, "/v1/models returned HTTP %d", status)
	}
	var cursorModels []string
	for _, m := range gjson.GetBytes(body, "data").Array() {
		id := m.Get("id").String()
		if strings.HasPrefix(id, cursorModelPrefix) || m.Get("owned_by").String() == "cursor" {
			cursorModels = append(cursorModels, id)
		}
	}
	if len(cursorModels) == 0 {
		return "", fail(ExitNotRegistered, "cursor provider not registered: no %s* models in /v1/models", cursorModelPrefix)
	}
	if want != "" {
		for _, id := range cursorModels {
			if id == want {
				return id, ok("")
			}
		}
		return "", fail(ExitProtocol, "model %s not offered; cursor models: %s", want, strings.Join(cursorModels, ", "))
	}
	for _, pref := range preferredModels {
		for _, id := range cursorModels {
			if id == pref {
				return id, ok("")
			}
		}
	}
	return cursorModels[0], ok("")
}

// classifyHTTPError maps a non-2xx /v1/messages response to a verdict.
func classifyHTTPError(status int, body []byte) Result {
	msg := gjson.GetBytes(body, "error.message").String()
	if msg == "" {
		msg = gjson.GetBytes(body, "error").String()
	}
	if msg == "" {
		msg = truncate(string(body), 200)
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "unknown provider for model"):
		return fail(ExitNotRegistered, "cursor provider not registered: %s", msg)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if strings.Contains(lower, "invalid api key") {
			return fail(ExitInconclusive, "proxy rejected CPA_API_KEY: %s", msg)
		}
		return fail(ExitAuthExpired, "upstream auth rejected (HTTP %d): %s", status, msg)
	case strings.Contains(lower, "auth_unavailable") || strings.Contains(lower, "no auth available") || strings.Contains(lower, "token expired") || strings.Contains(lower, "re-login"):
		return fail(ExitAuthExpired, "no usable cursor credentials (HTTP %d): %s", status, msg)
	case status == http.StatusTooManyRequests:
		return fail(ExitInconclusive, "rate limited (HTTP 429): %s", msg)
	default:
		return fail(ExitProtocol, "HTTP %d: %s", status, msg)
	}
}

// classifyFirstTurn checks that the model asked for the canary tool.
func classifyFirstTurn(t *Turn) (*ToolUse, Result) {
	if t.Error != "" {
		return nil, classifyStreamError(t.Error)
	}
	var call *ToolUse
	for i := range t.ToolUses {
		if t.ToolUses[i].Name == canaryToolName {
			call = &t.ToolUses[i]
		}
	}
	switch {
	case call == nil && len(t.ToolUses) > 0:
		return nil, fail(ExitProtocol, "turn 1 called %q, not %s", t.ToolUses[0].Name, canaryToolName)
	case call == nil:
		return nil, fail(ExitProtocol, "turn 1 has no tool_use (stop_reason=%q, text=%q)", t.StopReason, truncate(t.Text, 120))
	case call.ID == "":
		return nil, fail(ExitProtocol, "turn 1 tool_use has no id")
	case t.StopReason != "tool_use":
		return nil, fail(ExitProtocol, "turn 1 stop_reason=%q, want tool_use", t.StopReason)
	}
	return call, ok("")
}

// classifyFinalTurn checks that the model used the tool_result in a final answer.
func classifyFinalTurn(t *Turn) Result {
	if t.Error != "" {
		return classifyStreamError(t.Error)
	}
	switch {
	case len(t.ToolUses) > 0:
		return fail(ExitProtocol, "turn 2 called %q again instead of answering (tool_result likely lost)", t.ToolUses[0].Name)
	case !strings.Contains(t.Text, canarySecret):
		return fail(ExitProtocol, "turn 2 answer lacks %s: %q", canarySecret, truncate(t.Text, 120))
	case t.StopReason != "end_turn":
		return fail(ExitProtocol, "turn 2 stop_reason=%q, want end_turn", t.StopReason)
	}
	return ok("tool_use -> tool_result -> final answer")
}

func classifyStreamError(msg string) Result {
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "unauthenticated") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "expired") {
		return fail(ExitAuthExpired, "stream error: %s", msg)
	}
	return fail(ExitProtocol, "stream error: %s", msg)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
