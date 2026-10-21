package helps

import "testing"

func TestParseCursorClaudeRequest(t *testing.T) {
	body := []byte(`{"system":"sys\nPrimary working directory: /work/repo\n","thinking":{"type":"adaptive"},
		"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"hello"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"text","text":"boom"},{"type":"image","source":{}}]},{"type":"text","text":"<system-reminder>x</system-reminder>"}]}]}`)
	req, err := parseCursorClaudeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Thinking || req.Tools[0].Name != "Bash" || req.Messages[0].Blocks[0].Text != "hello" {
		t.Fatalf("parsed = %+v", req)
	}
	res := req.toolResults()
	if len(res) != 1 || res[0].ToolUseID != "toolu_1" || !res[0].IsError || res[0].Text != "boom\n[image omitted]" {
		t.Fatalf("tool results = %+v", res)
	}
	if req.lastUserText() != "<system-reminder>x</system-reminder>" {
		t.Fatalf("last user text = %q", req.lastUserText())
	}
	if got := cursorWorkspaceFromSystem(req.System); got != "/work/repo" {
		t.Fatalf("workspace = %q", got)
	}
	if got := cursorWorkspaceFromSystem("no env here"); got != "/" {
		t.Fatalf("default workspace = %q", got)
	}
}

// The conversation identity ignores cache_control markers, which Claude Code
// moves between requests, but changes with the first message's content.
func TestCursorClaudeIdentity(t *testing.T) {
	parse := func(body string) string {
		req, err := parseCursorClaudeRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return req.identity()
	}
	a := parse(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	b := parse(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`)
	c := parse(`{"messages":[{"role":"user","content":[{"type":"text","text":"other"}]}]}`)
	if a != b || a == c {
		t.Fatalf("identities: %s %s %s", a, b, c)
	}
}
