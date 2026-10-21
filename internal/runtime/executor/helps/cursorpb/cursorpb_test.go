package cursorpb

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
)

// Field numbers the Phase 0 spike used on the wire (docs/cursor/SPIKE.md).
func TestWireNumbersMatchSpike(t *testing.T) {
	asm := (&AgentServerMessage{}).ProtoReflect().Descriptor().Fields()
	for name, num := range map[string]int32{"interaction_update": 1, "exec_server_message": 2, "conversation_checkpoint_update": 3, "kv_server_message": 4, "interaction_query": 7} {
		if f := asm.ByName(protoreflect.Name(name)); f == nil || int32(f.Number()) != num {
			t.Errorf("AgentServerMessage.%s: want #%d, got %v", name, num, f)
		}
	}
	esm := (&ExecServerMessage{}).ProtoReflect().Descriptor().Fields()
	for name, num := range map[string]int32{"id": 1, "exec_id": 15, "mcp_args": 11, "request_context_args": 10, "mcp_state_exec_args": 36} {
		if f := esm.ByName(protoreflect.Name(name)); f == nil || int32(f.Number()) != num {
			t.Errorf("ExecServerMessage.%s: want #%d, got %v", name, num, f)
		}
	}
}

func TestMcpArgsRoundTrip(t *testing.T) {
	v, err := structpb.NewValue("alpha")
	if err != nil {
		t.Fatal(err)
	}
	in := &AgentServerMessage{Message: &AgentServerMessage_ExecServerMessage{ExecServerMessage: &ExecServerMessage{
		Id:     3,
		ExecId: "exec-1",
		Message: &ExecServerMessage_McpArgs{McpArgs: &McpArgs{
			Name: "claude-code-lookup", ToolName: "lookup", ProviderIdentifier: "claude-code", ToolCallId: "tool_1",
			Args: map[string]*structpb.Value{"key": v},
		}},
	}}}
	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := &AgentServerMessage{}
	if err = proto.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, out) {
		t.Fatalf("round trip mismatch: %v", out)
	}
	if got := out.GetExecServerMessage().GetMcpArgs().GetArgs()["key"].GetStringValue(); got != "alpha" {
		t.Fatalf("args.key = %q", got)
	}
}
