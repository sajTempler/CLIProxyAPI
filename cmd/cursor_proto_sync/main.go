// Command cursor_proto_sync extracts the protobuf schema from the installed
// Cursor agent CLI bundle and keeps internal/runtime/executor/helps/cursorpb in sync.
//
// The agent bundle (~/.local/bin/agent -> ~/.local/share/cursor-agent/versions/<ver>/)
// embeds protobuf-es compact descriptors. This tool resolves them into a
// schema, keeps the closure of the RPCs we call (see roots), and either
// regenerates the package or diffs against the committed SNAPSHOT.txt.
//
// Usage (from the repository root):
//
//	go run ./cmd/cursor_proto_sync            # regenerate .proto, .pb.go and SNAPSHOT.txt
//	go run ./cmd/cursor_proto_sync --check    # diff installed agent vs SNAPSHOT.txt
//
// Flags:
//
//	--agent <path>       agent launcher symlink (default ~/.local/bin/agent)
//	--bundle-dir <path>  bundle directory; overrides --agent
//	--out <dir>          output package dir (default internal/runtime/executor/helps/cursorpb)
//	--check              compare only; write nothing
//
// --check exit codes: 0 no breaking change, 1 breaking change in a used
// message, 2 the tool itself failed (bundle missing, parse error).
//
// Go code is generated in-process with google.golang.org/protobuf's
// protoc-gen-go (internal_gengo) at the version pinned in go.mod, so no
// protoc or plugin binary is needed and output is reproducible. See
// docs/cursor/MAINTENANCE.md.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// roots are the RPCs the provider calls; everything they reach is generated.
var roots = []string{
	"agent.v1.AgentService/Run",
	"agent.v1.AgentService/GetUsableModels",
	"aiserver.v1.ServerConfigService/GetServerConfig",
}

const execServerMessage = "agent.v1.ExecServerMessage"

// used lists the messages the provider reads or writes, each with the fields
// it relies on (the Phase 2 wire client in internal/runtime/executor/helps/cursor_*.go). --check
// fails when a used message disappears, when any field of a used message is
// renumbered or retyped, or when a listed field is removed. Removing an
// unlisted field is reported but not fatal (Cursor prunes fields often).
// Extend this when the provider starts using more of the protocol.
var used = map[string][]string{
	// Run envelope and request.
	"agent.v1.AgentClientMessage": {"run_request", "exec_client_message", "exec_client_control_message", "kv_client_message", "conversation_action", "client_heartbeat", "interaction_response"},
	"agent.v1.AgentServerMessage": {"interaction_update", "exec_server_message", "conversation_checkpoint_update", "kv_server_message", "interaction_query"},
	"agent.v1.AgentRunRequest":    {"conversation_state", "action", "requested_model", "mcp_tools", "conversation_id", "conversation_group_id", "run_id"},
	"agent.v1.ConversationAction": {"user_message_action", "resume_action", "cancel_action"},
	"agent.v1.UserMessageAction":  {"user_message", "request_context"},
	"agent.v1.UserMessage":        {"text", "message_id", "mode", "selected_context"},
	"agent.v1.SelectedContext":    {"selected_images"},
	"agent.v1.SelectedImage":      {"data", "uuid", "mime_type"},
	"agent.v1.ResumeAction":       {"request_context"},
	"agent.v1.CancelAction":       {"reason"},
	"agent.v1.RequestedModel":     {"model_id", "max_mode"},
	"agent.v1.McpTools":           {"mcp_tools"},
	// Checkpoint bytes are stored and sent back opaquely.
	"agent.v1.ConversationStateStructure": nil,
	"agent.v1.ClientHeartbeat":            nil,
	// Request context: rules (system prompt), env, MCP tool advertising.
	"agent.v1.RequestContext": {"rules", "env", "mcp_meta_tool_options", "mcp_file_system_options", "web_search_enabled", "web_fetch_enabled",
		"rules_info_complete", "env_info_complete", "repository_info_complete", "git_repo_info_complete", "mcp_info_complete",
		"custom_subagents_info_complete", "mcp_file_system_info_complete"},
	"agent.v1.RequestContextEnv":    {"workspace_paths", "process_working_directory"},
	"agent.v1.McpFileSystemOptions": {"enabled", "workspace_project_dir"},
	"agent.v1.CursorRule":           {"full_path", "content", "type"},
	"agent.v1.CursorRuleType":       {"global"},
	"agent.v1.CursorRuleTypeGlobal": nil,
	"agent.v1.McpMetaToolOptions":   {"enabled", "mcp_descriptors"},
	"agent.v1.McpDescriptor":        {"server_name", "server_identifier", "tools"},
	"agent.v1.McpToolDescriptor":    {"tool_name", "description", "input_schema"},
	"agent.v1.McpToolDefinition":    {"name", "description", "input_schema", "provider_identifier", "tool_name"},
	// Exec round-trips (MCP calls, MCP state, request context). Built-in execs
	// are soft-denied by reflection (foo_args -> foo_result's rejected/error
	// variant), so only the irregular shell_stream pair is listed.
	"agent.v1.ExecServerMessage":        {"id", "exec_id", "mcp_args", "mcp_state_exec_args", "request_context_args", "shell_stream_args"},
	"agent.v1.ExecClientMessage":        {"id", "exec_id", "mcp_result", "mcp_state_exec_result", "request_context_result", "shell_stream"},
	"agent.v1.ExecClientControlMessage": {"stream_close"},
	"agent.v1.ExecClientStreamClose":    {"id"},
	"agent.v1.McpArgs":                  {"name", "args", "tool_call_id", "tool_name"},
	"agent.v1.McpResult":                {"success"},
	"agent.v1.McpSuccess":               {"content", "is_error"},
	"agent.v1.McpToolResultContentItem": {"text", "image"},
	"agent.v1.McpImageContent":          {"data", "mime_type"},
	"agent.v1.McpTextContent":           {"text"},
	"agent.v1.McpStateExecArgs":         nil,
	"agent.v1.McpStateExecResult":       {"success"},
	"agent.v1.McpStateSuccess":          {"servers"},
	"agent.v1.McpStateServer":           {"server_name", "server_identifier", "tools"},
	"agent.v1.RequestContextArgs":       nil,
	"agent.v1.RequestContextResult":     {"success"},
	"agent.v1.RequestContextSuccess":    {"request_context"},
	// Streamed updates.
	"agent.v1.InteractionUpdate":       {"text_delta", "thinking_delta", "thinking_completed", "token_delta", "turn_ended"},
	"agent.v1.TextDeltaUpdate":         {"text"},
	"agent.v1.ThinkingDeltaUpdate":     {"text"},
	"agent.v1.ThinkingCompletedUpdate": nil,
	"agent.v1.TokenDeltaUpdate":        {"tokens"},
	"agent.v1.TurnEndedUpdate":         {"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"},
	"agent.v1.InteractionQuery":        {"id"},
	"agent.v1.InteractionResponse":     {"id"},
	// KV blob store.
	"agent.v1.KvServerMessage": {"id", "get_blob_args", "set_blob_args"},
	"agent.v1.KvClientMessage": {"id", "get_blob_result", "set_blob_result"},
	"agent.v1.GetBlobArgs":     {"blob_id"},
	"agent.v1.GetBlobResult":   {"blob_data"},
	"agent.v1.SetBlobArgs":     {"blob_id", "blob_data"},
	"agent.v1.SetBlobResult":   nil,
	// Models and host discovery.
	"agent.v1.GetUsableModelsRequest":     nil,
	"agent.v1.GetUsableModelsResponse":    {"models"},
	"agent.v1.ModelDetails":               {"model_id", "display_name", "max_mode"},
	"aiserver.v1.GetServerConfigRequest":  nil,
	"aiserver.v1.GetServerConfigResponse": {"agent_url_config"},
	"aiserver.v1.AgentUrlConfig":          {"agent_url", "agentn_url"},
}

func usedMessages() []string { return sortedKeys(used) }

const defaultOut = "internal/runtime/executor/helps/cursorpb"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("cursor_proto_sync", flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	agent := fs.String("agent", filepath.Join(home, ".local", "bin", "agent"), "agent launcher symlink")
	bundleDir := fs.String("bundle-dir", "", "bundle directory (overrides --agent)")
	out := fs.String("out", defaultOut, "output package directory")
	check := fs.Bool("check", false, "diff installed agent against SNAPSHOT.txt; write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir, version, err := locateBundle(*agent, *bundleDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cursor_proto_sync:", err)
		return 2
	}
	schema, err := LoadBundle(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cursor_proto_sync:", err)
		return 2
	}
	msgs, enums, err := schema.Closure(append(append([]string{}, roots...), usedMessages()...))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cursor_proto_sync: closure:", err)
		return 2
	}
	snap := BuildSnapshot(schema, version, msgs, enums)
	snap.RPCs = RPCLines(schema, roots)

	if *check {
		raw, errRead := os.ReadFile(filepath.Join(*out, "SNAPSHOT.txt"))
		if errRead != nil {
			fmt.Fprintln(os.Stderr, "cursor_proto_sync: read snapshot:", errRead)
			return 2
		}
		old, errParse := ParseSnapshot(bytes.NewReader(raw))
		if errParse != nil {
			fmt.Fprintln(os.Stderr, "cursor_proto_sync: parse snapshot:", errParse)
			return 2
		}
		rep := Diff(old, snap, used)
		rep.Print(os.Stdout)
		if rep.HasBreaking() {
			return 1
		}
		return 0
	}

	if missing := snap.MissingUsed(used); len(missing) > 0 {
		fmt.Fprintln(os.Stderr, "cursor_proto_sync: used list names unknown fields:", strings.Join(missing, ", "))
		return 2
	}
	if err = Generate(schema, snap, msgs, enums, *out); err != nil {
		fmt.Fprintln(os.Stderr, "cursor_proto_sync: generate:", err)
		return 2
	}
	fmt.Printf("generated %d messages, %d enums from agent %s into %s\n", len(msgs), len(enums), version, *out)
	if len(schema.Warnings) > 0 {
		fmt.Printf("%d bundle descriptors could not be resolved (outside the generated closure, ignored)\n", len(schema.Warnings))
	}
	return 0
}

// locateBundle resolves the bundle directory and the agent version (its directory name).
func locateBundle(agent, bundleDir string) (dir, version string, err error) {
	if bundleDir == "" {
		target, errLink := filepath.EvalSymlinks(agent)
		if errLink != nil {
			return "", "", fmt.Errorf("resolve agent symlink %s: %w", agent, errLink)
		}
		bundleDir = filepath.Dir(target)
	}
	if _, errStat := os.Stat(filepath.Join(bundleDir, "index.js")); errStat != nil {
		return "", "", errors.New("no index.js in bundle dir " + bundleDir)
	}
	version = filepath.Base(bundleDir)
	if version == "" || strings.ContainsAny(version, " \t") {
		return "", "", fmt.Errorf("cannot derive version from %s", bundleDir)
	}
	return bundleDir, version, nil
}
