package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// withCursorSession runs fn with the turn of the session whose key starts
// with prefix+"|" (keys are caller key + first-message identity).
func withCursorSession(st *CursorSessionStore, prefix string, fn func(*cursorSession)) bool {
	st.mu.Lock()
	var key string
	for k := range st.sessions {
		if strings.HasPrefix(k, prefix+"|") {
			key = k
		}
	}
	st.mu.Unlock()
	if key == "" {
		return false
	}
	s, err := st.acquire(context.Background(), key)
	if err != nil {
		return false
	}
	defer st.release(s)
	fn(s)
	return true
}

// cursorAssembled is a Claude response rebuilt from stream events, the way
// Claude Code assembles it.
type cursorAssembled struct {
	Blocks     []map[string]any
	StopReason string
	Usage      map[string]any
	Order      []string // event types in order
}

func (a *cursorAssembled) text() string {
	var sb strings.Builder
	for _, b := range a.Blocks {
		if b["type"] == "text" {
			sb.WriteString(b["text"].(string))
		}
	}
	return sb.String()
}

func (a *cursorAssembled) toolUses() []map[string]any {
	var out []map[string]any
	for _, b := range a.Blocks {
		if b["type"] == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

func (a *cursorAssembled) has(typ string) bool {
	for _, b := range a.Blocks {
		if b["type"] == typ {
			return true
		}
	}
	return false
}

// assembleCursorClaude validates event structure and rebuilds the message.
func assembleCursorClaude(events []CursorClaudeEvent) (*cursorAssembled, error) {
	out := &cursorAssembled{}
	partial := map[int]*strings.Builder{}
	for i, ev := range events {
		out.Order = append(out.Order, ev.Type)
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return nil, fmt.Errorf("event %d: %w", i, err)
		}
		if d["type"] != ev.Type {
			return nil, fmt.Errorf("event %d: type %v != %s", i, d["type"], ev.Type)
		}
		switch ev.Type {
		case "message_start":
			if i != 0 {
				return nil, fmt.Errorf("message_start at %d", i)
			}
		case "content_block_start":
			idx := int(d["index"].(float64))
			if idx != len(out.Blocks) {
				return nil, fmt.Errorf("event %d: block index %d, want %d", i, idx, len(out.Blocks))
			}
			out.Blocks = append(out.Blocks, d["content_block"].(map[string]any))
			partial[idx] = &strings.Builder{}
		case "content_block_delta":
			idx := int(d["index"].(float64))
			if idx != len(out.Blocks)-1 {
				return nil, fmt.Errorf("event %d: delta for closed block %d", i, idx)
			}
			delta := d["delta"].(map[string]any)
			b := out.Blocks[idx]
			switch delta["type"] {
			case "text_delta":
				b["text"] = b["text"].(string) + delta["text"].(string)
			case "thinking_delta":
				b["thinking"] = b["thinking"].(string) + delta["thinking"].(string)
			case "input_json_delta":
				partial[idx].WriteString(delta["partial_json"].(string))
			default:
				return nil, fmt.Errorf("event %d: delta type %v", i, delta["type"])
			}
		case "content_block_stop":
			idx := int(d["index"].(float64))
			b := out.Blocks[idx]
			if b["type"] == "tool_use" {
				var in any
				if err := json.Unmarshal([]byte(partial[idx].String()), &in); err != nil {
					return nil, fmt.Errorf("tool input: %w", err)
				}
				b["input"] = in
			}
		case "message_delta":
			out.StopReason = d["delta"].(map[string]any)["stop_reason"].(string)
			out.Usage = d["usage"].(map[string]any)
		case "message_stop":
			if i != len(events)-1 {
				return nil, fmt.Errorf("message_stop at %d of %d", i, len(events))
			}
		}
	}
	if len(out.Order) == 0 || out.Order[len(out.Order)-1] != "message_stop" {
		return nil, fmt.Errorf("stream does not end with message_stop: %v", out.Order)
	}
	return out, nil
}

// cursorTestTurn is one scripted Claude Code conversation step.
type cursorTestConversation struct {
	Thinking bool
	System   string
	Tools    []map[string]any
	Messages []map[string]any
}

func (c *cursorTestConversation) body(model string) []byte {
	req := map[string]any{"model": model, "max_tokens": 4096, "stream": true, "messages": c.Messages}
	if c.System != "" {
		req["system"] = []map[string]any{{"type": "text", "text": c.System}}
	}
	if len(c.Tools) > 0 {
		req["tools"] = c.Tools
	}
	if c.Thinking {
		req["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 2048}
	}
	raw, _ := json.Marshal(req)
	return raw
}

func (c *cursorTestConversation) user(text string) {
	c.Messages = append(c.Messages, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}})
}

// assistant appends the assembled response as Claude Code would.
func (c *cursorTestConversation) assistant(a *cursorAssembled) {
	blocks := make([]map[string]any, 0, len(a.Blocks))
	blocks = append(blocks, a.Blocks...)
	c.Messages = append(c.Messages, map[string]any{"role": "assistant", "content": blocks})
}

// toolResults appends a user message answering every tool_use of a.
func (c *cursorTestConversation) toolResults(a *cursorAssembled, run func(name string, input map[string]any) string) {
	var blocks []map[string]any
	for _, tu := range a.toolUses() {
		in, _ := tu["input"].(map[string]any)
		blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": tu["id"], "content": run(tu["name"].(string), in)})
	}
	c.Messages = append(c.Messages, map[string]any{"role": "user", "content": blocks})
}

var cursorTestTools = []map[string]any{
	{
		"name":        "lookup_secret",
		"description": "Look up the secret value stored in the vault for a key. Returns the value as text.",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"key": map[string]any{"type": "string", "description": "Vault key"}},
			"required":   []string{"key"},
		},
	},
	{
		"name":        "Read",
		"description": "Reads a file from the local filesystem. The file_path parameter must be an absolute path.",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"file_path": map[string]any{"type": "string", "description": "The absolute path to the file to read"}},
			"required":   []string{"file_path"},
		},
	},
}

// cursorTestToolRunner answers the scripted tools deterministically.
func cursorTestToolRunner(name string, input map[string]any) string {
	switch name {
	case "lookup_secret":
		switch input["key"] {
		case "alpha":
			return "PURPLE-42"
		case "beta":
			return "ORANGE-17"
		}
		return fmt.Sprintf("No value stored under %v.", input["key"])
	case "Read":
		return "     1\tpackage main\n     2\t\n     3\tfunc main() {}\n"
	}
	return "unknown tool"
}

// cursorBigSystemPrompt builds a synthetic ~15k-token system prompt shaped like
// Claude Code's (sections of policy text, env block) with two adherence markers:
// one rule at the start and one at the very end.
func cursorBigSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString("You are Claude Code, Anthropic's official CLI for Claude, NOT Cursor.\n")
	sb.WriteString("IMPORTANT: Begin every reply with the exact prefix 'ZQX> ' and write the rest in lowercase without markdown.\n\n")
	topics := []string{
		"Tone and style", "Proactiveness", "Following conventions", "Code style", "Task management",
		"Doing tasks", "Tool usage policy", "Code references", "Git safety", "Committing changes",
		"Creating pull requests", "Security", "Testing", "Refactoring", "Documentation",
	}
	para := "When working on %s, read the surrounding code first, prefer small focused changes, keep existing conventions, " +
		"verify the result with the project's own commands, and report what changed with file paths. Never invent APIs; check that a library is " +
		"already used before relying on it. Prefer editing existing files over creating new ones. Keep answers short unless the user asks for detail. " +
		"Run linters and type checkers when they exist. Ask before destructive operations such as deleting branches, force pushing or dropping data. "
	for i := 0; sb.Len() < 60000; i++ {
		topic := topics[i%len(topics)]
		fmt.Fprintf(&sb, "# %s (%d)\n", topic, i/len(topics)+1)
		for j := 0; j < 3; j++ {
			fmt.Fprintf(&sb, para, strings.ToLower(topic))
			fmt.Fprintf(&sb, "Example %d.%d: if the user asks about %s, answer directly and cite file_path:line_number.\n", i, j, strings.ToLower(topic))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("<env>\nWorking directory: /tmp/cursor-live-ws\nPlatform: darwin\n</env>\n")
	sb.WriteString("IMPORTANT: End every reply with the exact token '<<done>>'.\n")
	return sb.String()
}
