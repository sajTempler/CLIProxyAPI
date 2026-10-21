package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

// ToolUse is one tool_use block from a Claude response.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Turn is a Claude /v1/messages response assembled from its SSE stream.
type Turn struct {
	Text       string
	ToolUses   []ToolUse
	StopReason string
	Error      string // SSE "error" event message, if any
}

// ParseSSE reads a Claude Messages SSE stream into a Turn.
// It returns an error only for unreadable or malformed streams.
func ParseSSE(r io.Reader) (*Turn, error) {
	turn := &Turn{}
	type block struct {
		kind  string
		tool  ToolUse
		input strings.Builder
	}
	blocks := map[int64]*block{}
	var order []int64
	var sawStart, sawStop bool

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		data, isData := strings.CutPrefix(line, "data:")
		if !isData {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		if !gjson.Valid(data) {
			return nil, fmt.Errorf("malformed SSE data: %s", truncate(data, 120))
		}
		ev := gjson.Parse(data)
		switch ev.Get("type").String() {
		case "message_start":
			sawStart = true
		case "content_block_start":
			idx := ev.Get("index").Int()
			cb := ev.Get("content_block")
			b := &block{kind: cb.Get("type").String()}
			if b.kind == "tool_use" {
				b.tool = ToolUse{ID: cb.Get("id").String(), Name: cb.Get("name").String()}
			}
			if b.kind == "text" {
				turn.Text += cb.Get("text").String()
			}
			blocks[idx] = b
			order = append(order, idx)
		case "content_block_delta":
			b := blocks[ev.Get("index").Int()]
			if b == nil {
				return nil, fmt.Errorf("delta for unknown content block %d", ev.Get("index").Int())
			}
			d := ev.Get("delta")
			switch d.Get("type").String() {
			case "text_delta":
				turn.Text += d.Get("text").String()
			case "input_json_delta":
				b.input.WriteString(d.Get("partial_json").String())
			}
		case "message_delta":
			if sr := ev.Get("delta.stop_reason").String(); sr != "" {
				turn.StopReason = sr
			}
		case "message_stop":
			sawStop = true
		case "error":
			turn.Error = ev.Get("error.message").String()
			if turn.Error == "" {
				turn.Error = truncate(data, 200)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}
	if turn.Error != "" {
		return turn, nil
	}
	if !sawStart || !sawStop {
		return nil, fmt.Errorf("incomplete SSE stream (message_start=%v, message_stop=%v)", sawStart, sawStop)
	}
	for _, idx := range order {
		b := blocks[idx]
		if b.kind != "tool_use" {
			continue
		}
		raw := b.input.String()
		if raw == "" {
			raw = "{}"
		}
		if !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("tool_use %s has invalid input JSON", b.tool.Name)
		}
		b.tool.Input = json.RawMessage(raw)
		turn.ToolUses = append(turn.ToolUses, b.tool)
	}
	return turn, nil
}
