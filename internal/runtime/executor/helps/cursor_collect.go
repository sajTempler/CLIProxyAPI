package helps

import (
	"encoding/json"

	"github.com/tidwall/gjson"
)

// CursorMessageCollector folds the Claude stream events of one response into
// the equivalent non-streaming Claude message (for non-stream requests).
type CursorMessageCollector struct {
	message map[string]any
	blocks  []map[string]any
	partial map[int]string // tool_use input JSON by block index
	seen    bool
}

// Add records one event.
func (c *CursorMessageCollector) Add(ev CursorClaudeEvent) {
	data := gjson.ParseBytes(ev.Data)
	switch ev.Type {
	case "message_start":
		c.seen = true
		c.message = map[string]any{}
		if m, ok := data.Get("message").Value().(map[string]any); ok {
			c.message = m
		}
	case "content_block_start":
		idx := int(data.Get("index").Int())
		block, _ := data.Get("content_block").Value().(map[string]any)
		if block == nil {
			block = map[string]any{}
		}
		for len(c.blocks) <= idx {
			c.blocks = append(c.blocks, nil)
		}
		c.blocks[idx] = block
	case "content_block_delta":
		idx := int(data.Get("index").Int())
		if idx < 0 || idx >= len(c.blocks) || c.blocks[idx] == nil {
			return
		}
		b := c.blocks[idx]
		delta := data.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			b["text"] = stringField(b, "text") + delta.Get("text").String()
		case "thinking_delta":
			b["thinking"] = stringField(b, "thinking") + delta.Get("thinking").String()
		case "input_json_delta":
			if c.partial == nil {
				c.partial = map[int]string{}
			}
			c.partial[idx] += delta.Get("partial_json").String()
		}
	case "message_delta":
		if c.message == nil {
			c.message = map[string]any{}
		}
		if v := data.Get("delta.stop_reason"); v.Exists() {
			c.message["stop_reason"] = v.Value()
		}
		if v := data.Get("delta.stop_sequence"); v.Exists() {
			c.message["stop_sequence"] = v.Value()
		}
		if u, ok := data.Get("usage").Value().(map[string]any); ok {
			c.message["usage"] = u
		}
	}
}

// Started reports whether a message_start was seen.
func (c *CursorMessageCollector) Started() bool { return c.seen }

// Message returns the collected Claude message JSON.
func (c *CursorMessageCollector) Message() ([]byte, error) {
	msg := c.message
	if msg == nil {
		msg = map[string]any{"type": "message", "role": "assistant"}
	}
	content := make([]any, 0, len(c.blocks))
	for i, b := range c.blocks {
		if b == nil {
			continue
		}
		if raw, ok := c.partial[i]; ok {
			var input any = map[string]any{}
			if raw != "" && json.Valid([]byte(raw)) {
				_ = json.Unmarshal([]byte(raw), &input)
			}
			b["input"] = input
		}
		content = append(content, b)
	}
	msg["content"] = content
	return json.Marshal(msg)
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
