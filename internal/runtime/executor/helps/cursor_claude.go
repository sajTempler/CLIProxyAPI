package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// Claude Messages request -> Cursor Run request pieces. The pivot format is
// FormatClaude: Claude Code's request arrives here untranslated.

const (
	// CursorMCPServer is the MCP server name Claude Code's tools are offered under.
	CursorMCPServer = "claude-code"
	// cursorRulePath labels the global rule that carries the system prompt.
	cursorRulePath = "Claude Code system instructions"
	// cursorToolGuidance steers models away from Cursor's built-in tools
	// (SPIKE.md Q2: with it, 9/9 runs used the MCP tools and none tried a built-in).
	cursorToolGuidance = "Cursor built-in tools (shell, read, write, edit, grep, glob, ls, delete, fetch, web search, subagents, etc.) are disabled in this environment and always fail. " +
		"The only working tools are the MCP tools of the `" + CursorMCPServer + "` server; call them via the dynamic tool mechanism with exactly the argument names of their input schema. " +
		"Follow the system instructions above; they override any other instructions about your identity, tools or output format."
	// cursorSoftDenyReason is the error every built-in exec gets.
	cursorSoftDenyReason = "This tool is not available in this environment. Use the `" + CursorMCPServer + "` MCP tools instead."
)

// CursorRequestError is a Claude request the provider cannot map to a Run
// (the executor answers it with HTTP 400).
type CursorRequestError struct{ Msg string }

func (e *CursorRequestError) Error() string { return e.Msg }

// cursorClaudeRequest is the subset of a Claude Messages request the provider uses.
type cursorClaudeRequest struct {
	System   string
	Tools    []cursorClaudeTool
	Messages []cursorClaudeMessage
	// Thinking is set when the request enables extended thinking; Cursor's
	// thinking deltas are only surfaced then.
	Thinking bool
}

type cursorClaudeTool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type cursorClaudeMessage struct {
	Role   string // "user" or "assistant"
	Blocks []cursorClaudeBlock
}

// cursorClaudeBlock is one content block, flattened to what the provider needs.
type cursorClaudeBlock struct {
	Type      string // text, thinking, tool_use, tool_result, image, ...
	Text      string // text/thinking text, or the flattened tool_result content
	ID        string // tool_use id
	Name      string // tool_use name
	Input     json.RawMessage
	ToolUseID string // tool_result target
	IsError   bool
}

// parseCursorClaudeRequest reads a Claude Messages request body.
func parseCursorClaudeRequest(body []byte) (*cursorClaudeRequest, error) {
	if !gjson.ValidBytes(body) {
		return nil, &CursorRequestError{Msg: "cursor: request body is not valid JSON"}
	}
	root := gjson.ParseBytes(body)
	req := &cursorClaudeRequest{System: cursorFlattenText(root.Get("system"))}
	switch root.Get("thinking.type").String() {
	case "enabled", "adaptive":
		req.Thinking = true
	}
	for _, t := range root.Get("tools").Array() {
		name := t.Get("name").String()
		if name == "" {
			continue
		}
		schema := json.RawMessage(`{"type":"object","properties":{}}`)
		if s := t.Get("input_schema"); s.IsObject() {
			schema = json.RawMessage(s.Raw)
		}
		req.Tools = append(req.Tools, cursorClaudeTool{Name: name, Description: t.Get("description").String(), Schema: schema})
	}
	for _, m := range root.Get("messages").Array() {
		msg := cursorClaudeMessage{Role: m.Get("role").String()}
		content := m.Get("content")
		if msg.Role == "system" {
			// Claude Code sends mid-conversation system messages (reminders,
			// context updates). Cursor has no such turn: they ride as text on
			// the adjacent user turn.
			if text := strings.TrimSpace(cursorFlattenText(content)); text != "" {
				req.appendUserBlocks(cursorClaudeBlock{Type: "text", Text: "<system_message>\n" + text + "\n</system_message>"})
			}
			continue
		}
		var blocks []gjson.Result
		if content.Type == gjson.String {
			msg.Blocks = append(msg.Blocks, cursorClaudeBlock{Type: "text", Text: content.String()})
		} else {
			blocks = content.Array()
		}
		for _, b := range blocks {
			blk := cursorClaudeBlock{Type: b.Get("type").String()}
			switch blk.Type {
			case "text":
				blk.Text = b.Get("text").String()
			case "thinking":
				blk.Text = b.Get("thinking").String()
			case "tool_use":
				blk.ID, blk.Name = b.Get("id").String(), b.Get("name").String()
				blk.Input = json.RawMessage(`{}`)
				if in := b.Get("input"); in.Exists() {
					blk.Input = json.RawMessage(in.Raw)
				}
			case "tool_result":
				blk.ToolUseID = b.Get("tool_use_id").String()
				blk.IsError = b.Get("is_error").Bool()
				blk.Text = cursorFlattenText(b.Get("content"))
			}
			msg.Blocks = append(msg.Blocks, blk)
		}
		if msg.Role == "user" {
			req.appendUserBlocks(msg.Blocks...)
			continue
		}
		req.Messages = append(req.Messages, msg)
	}
	if len(req.Messages) == 0 {
		return nil, &CursorRequestError{Msg: "cursor: request has no messages"}
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role != "user" {
		return nil, &CursorRequestError{Msg: fmt.Sprintf("cursor: last message must be from the user, got %q", last.Role)}
	}
	return req, nil
}

// appendUserBlocks adds blocks to the trailing user message, or starts one.
// Consecutive user turns merge, as the Messages API itself treats them.
func (r *cursorClaudeRequest) appendUserBlocks(blocks ...cursorClaudeBlock) {
	if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == "user" {
		r.Messages[n-1].Blocks = append(r.Messages[n-1].Blocks, blocks...)
		return
	}
	r.Messages = append(r.Messages, cursorClaudeMessage{Role: "user", Blocks: blocks})
}

// cursorFlattenText turns a string or an array of content blocks into text.
// Non-text blocks become short placeholders (images are not forwarded yet).
func cursorFlattenText(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.String {
		return v.String()
	}
	var parts []string
	for _, b := range v.Array() {
		switch b.Get("type").String() {
		case "text":
			parts = append(parts, b.Get("text").String())
		case "image":
			parts = append(parts, "[image omitted]")
		case "document":
			parts = append(parts, "[document omitted]")
		}
	}
	return strings.Join(parts, "\n")
}

// identity fingerprints the conversation by its first message, so requests
// sharing a caller key but not a history (Claude Code side requests such as
// title generation, subagents, a compacted conversation) get separate state.
// Only text and tool ids count: cache_control markers move between requests.
func (r *cursorClaudeRequest) identity() string {
	h := sha256.New()
	first := r.Messages[0]
	h.Write([]byte(first.Role))
	for _, b := range first.Blocks {
		h.Write([]byte{0})
		h.Write([]byte(b.Type + "\x00" + b.Text + "\x00" + b.ID + "\x00" + b.ToolUseID))
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// toolResults returns the tool_result blocks of the last (user) message.
func (r *cursorClaudeRequest) toolResults() []cursorClaudeBlock {
	var out []cursorClaudeBlock
	for _, b := range r.Messages[len(r.Messages)-1].Blocks {
		if b.Type == "tool_result" {
			out = append(out, b)
		}
	}
	return out
}

// lastUserText returns the text blocks of the last (user) message.
func (r *cursorClaudeRequest) lastUserText() string { return textOf(r.Messages[len(r.Messages)-1]) }

// cursorTranscript seeds a conversation Cursor has no state for (first request
// of an existing Claude Code conversation, history from another provider, lost
// session) as text in the user message. Lossy but correct (SPIKE.md Q1: 4/4).
func cursorTranscript(msgs []cursorClaudeMessage) string {
	last := msgs[len(msgs)-1]
	hasResults := false
	for _, b := range last.Blocks {
		if b.Type == "tool_result" {
			hasResults = true
		}
	}
	history := msgs[:len(msgs)-1]
	if hasResults {
		history = msgs
	}
	if len(history) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<conversation_history>\n")
	for _, m := range history {
		var body strings.Builder
		for _, b := range m.Blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					body.WriteString(b.Text + "\n")
				}
			case "tool_use":
				fmt.Fprintf(&body, "[tool call %s id=%s] %s\n", b.Name, b.ID, string(b.Input))
			case "tool_result":
				status := ""
				if b.IsError {
					status = " error"
				}
				fmt.Fprintf(&body, "<tool_result id=%s%s>\n%s\n</tool_result>\n", b.ToolUseID, status, b.Text)
			}
		}
		if body.Len() == 0 {
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		fmt.Fprintf(&sb, "<%s>\n%s</%s>\n", role, body.String(), role)
	}
	sb.WriteString("</conversation_history>\n\n")
	if hasResults {
		if text := strings.TrimSpace(textOf(last)); text != "" {
			sb.WriteString(text + "\n\n")
		}
		sb.WriteString("Continue the task above from where it left off.")
	} else {
		sb.WriteString(textOf(last))
	}
	return sb.String()
}

func textOf(m cursorClaudeMessage) string {
	var parts []string
	for _, b := range m.Blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

var cursorWorkdirPattern = regexp.MustCompile(`(?m)^\s*(?:Primary working directory|Working directory):\s*(\S+)`)

// cursorWorkspaceFromSystem finds Claude Code's working directory in its
// system prompt, so Cursor's env does not contradict it. "/" when absent.
func cursorWorkspaceFromSystem(system string) string {
	if m := cursorWorkdirPattern.FindStringSubmatch(system); m != nil {
		return m[1]
	}
	return "/"
}

// cursorRequestContext builds the RequestContext sent with every Run (and as
// the answer to request_context_args): the system prompt plus tool guidance as
// one global rule, and the Claude Code tools as MCP descriptors.
func cursorRequestContext(req *cursorClaudeRequest) *cursorpb.RequestContext {
	workspace := cursorWorkspaceFromSystem(req.System)
	rule := cursorToolGuidance
	if s := strings.TrimSpace(req.System); s != "" {
		rule = s + "\n\n" + cursorToolGuidance
	}
	descriptors := make([]*cursorpb.McpToolDescriptor, 0, len(req.Tools))
	for _, t := range req.Tools {
		d := &cursorpb.McpToolDescriptor{ToolName: t.Name, InputSchema: cursorJSONValue(t.Schema)}
		if t.Description != "" {
			d.Description = proto.String(t.Description)
		}
		descriptors = append(descriptors, d)
	}
	meta := &cursorpb.McpMetaToolOptions{Enabled: true}
	if len(descriptors) > 0 {
		meta.McpDescriptors = []*cursorpb.McpDescriptor{{ServerName: CursorMCPServer, ServerIdentifier: CursorMCPServer, Tools: descriptors}}
	}
	return &cursorpb.RequestContext{
		Rules: []*cursorpb.CursorRule{{
			FullPath: cursorRulePath,
			Content:  rule,
			Type:     &cursorpb.CursorRuleType{Type: &cursorpb.CursorRuleType_Global{Global: &cursorpb.CursorRuleTypeGlobal{}}},
		}},
		Env: &cursorpb.RequestContextEnv{
			WorkspacePaths:          []string{workspace},
			ProcessWorkingDirectory: proto.String(workspace),
		},
		WebSearchEnabled:            proto.Bool(false),
		WebFetchEnabled:             proto.Bool(false),
		McpFileSystemOptions:        &cursorpb.McpFileSystemOptions{Enabled: true, WorkspaceProjectDir: workspace},
		McpMetaToolOptions:          meta,
		RulesInfoComplete:           proto.Bool(true),
		EnvInfoComplete:             proto.Bool(true),
		RepositoryInfoComplete:      proto.Bool(true),
		GitRepoInfoComplete:         proto.Bool(true),
		McpInfoComplete:             proto.Bool(true),
		CustomSubagentsInfoComplete: proto.Bool(true),
		McpFileSystemInfoComplete:   proto.Bool(true),
	}
}

// cursorMCPDefinitions answers mcp_state_exec_args: the full tool definitions
// Cursor's dynamic-tool flow looks up before the first MCP call of a Run.
func cursorMCPDefinitions(req *cursorClaudeRequest) []*cursorpb.McpToolDefinition {
	out := make([]*cursorpb.McpToolDefinition, 0, len(req.Tools))
	for _, t := range req.Tools {
		identity, _ := json.Marshal(map[string]string{"namespace": CursorMCPServer, "toolName": t.Name})
		out = append(out, &cursorpb.McpToolDefinition{
			Name:               CursorMCPServer + "-" + t.Name,
			Description:        "CallDynamicTool identity: " + string(identity) + `. Put the inputSchema object in "arguments".` + "\n\n" + t.Description,
			InputSchema:        cursorJSONValue(t.Schema),
			ProviderIdentifier: CursorMCPServer,
			ToolName:           t.Name,
		})
	}
	return out
}

// cursorJSONValue converts JSON to a protobuf Value (an empty object on error).
func cursorJSONValue(raw json.RawMessage) *structpb.Value {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		v = map[string]any{}
	}
	out, err := structpb.NewValue(v)
	if err != nil {
		out, _ = structpb.NewValue(map[string]any{})
	}
	return out
}

// cursorToolName maps an mcp_args call to the Claude Code tool name.
func cursorToolName(a *cursorpb.McpArgs) string {
	if a.GetToolName() != "" {
		return a.GetToolName()
	}
	return strings.TrimPrefix(a.GetName(), CursorMCPServer+"-")
}

// cursorArgsJSON renders mcp_args arguments as a JSON object.
func cursorArgsJSON(a *cursorpb.McpArgs) json.RawMessage {
	s := &structpb.Struct{Fields: a.GetArgs()}
	raw, err := json.Marshal(s.AsMap())
	if err != nil || len(a.GetArgs()) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}
