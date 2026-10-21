package cursorfixture

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const fakeJWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyXzEyMyJ9.c2lnbmF0dXJlLXNpZ25hdHVyZQ"

func TestRedactString(t *testing.T) {
	r := NewRedactor("jdoe", "jdoe-mbp")
	cases := map[string]string{
		"token " + fakeJWT + " end":                 "token REDACTED end",
		"Authorization: Bearer abcdefghijklmnop123": "Authorization: Bearer REDACTED",
		"mail jane.doe+x@corp.example.org now":      "mail user@example.com now",
		"/Users/jdoe/src/app/main.go":               "/Users/user/src/app/main.go",
		"/home/alice/.config":                       "/home/user/.config",
		`C:\Users\Alice\repo`:                       `C:\Users\user\repo`,
		"host JDOE-MBP.local said hi to jdoe":       "host user.local said hi to user",
		"sk-ant-abcdefghijklmnopqrstuvwxyz":         "REDACTED",
		"plain text stays":                          "plain text stays",
	}
	for in, want := range cases {
		if got := r.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactMessage(t *testing.T) {
	path, _ := structpb.NewValue("/Users/jdoe/secret/notes.txt")
	m := &cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_RunRequest{RunRequest: &cursorpb.AgentRunRequest{
		ConversationId:             proto.String("conv-1"),
		ClientLlmGatewayCredential: &cursorpb.ClientLlmGatewayCredential{BearerToken: "opaque-gateway-credential"},
		Action: &cursorpb.ConversationAction{Action: &cursorpb.ConversationAction_UserMessageAction{UserMessageAction: &cursorpb.UserMessageAction{
			UserMessage: &cursorpb.UserMessage{Text: "mail me at jdoe@corp.example.org"},
			RequestContext: &cursorpb.RequestContext{
				Env: &cursorpb.RequestContextEnv{WorkspacePaths: []string{"/Users/jdoe/repo"}},
			},
		}}},
	}}}
	srv := &cursorpb.ExecServerMessage{Message: &cursorpb.ExecServerMessage_McpArgs{McpArgs: &cursorpb.McpArgs{Args: map[string]*structpb.Value{"file_path": path}}}}
	blob := &cursorpb.SetBlobArgs{BlobId: []byte{1, 2, 3}, BlobData: []byte(`{"cwd":"/Users/jdoe/repo"}`)}

	r := NewRedactor("jdoe")
	r.Message(m)
	r.Message(srv)
	r.Message(blob)

	rr := m.GetRunRequest()
	if got := rr.GetClientLlmGatewayCredential().GetBearerToken(); got != RedactedSecret {
		t.Errorf("bearer_token = %q", got)
	}
	uma := rr.GetAction().GetUserMessageAction()
	if got := uma.GetUserMessage().GetText(); got != "mail me at user@example.com" {
		t.Errorf("text = %q", got)
	}
	if got := uma.GetRequestContext().GetEnv().GetWorkspacePaths()[0]; got != "/Users/user/repo" {
		t.Errorf("workspace path = %q", got)
	}
	if got := rr.GetConversationId(); got != "conv-1" {
		t.Errorf("conversation id changed: %q", got)
	}
	if got := srv.GetMcpArgs().GetArgs()["file_path"].GetStringValue(); got != "/Users/user/secret/notes.txt" {
		t.Errorf("mcp arg = %q", got)
	}
	if got := string(blob.GetBlobData()); got != `{"cwd":"/Users/user/repo"}` {
		t.Errorf("blob data = %q", got)
	}
	if !bytes.Equal(blob.GetBlobId(), []byte{1, 2, 3}) {
		t.Errorf("binary blob id changed: %v", blob.GetBlobId())
	}
}

func TestRecorderWriteLoadRoundTrip(t *testing.T) {
	ms := int64(0)
	now := func() time.Time { return time.UnixMilli(ms) }
	rec := NewRecorder("2026.10.19-6bbf2c4", "composer-2.5", "round trip", NewRedactor("jdoe"), now)
	if err := rec.Client(&cursorpb.AgentClientMessage{Message: &cursorpb.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &cursorpb.ClientHeartbeat{}}}); err != nil {
		t.Fatal(err)
	}
	ms = 42
	if err := rec.Server(&cursorpb.AgentServerMessage{Message: &cursorpb.AgentServerMessage_InteractionUpdate{InteractionUpdate: &cursorpb.InteractionUpdate{
		Message: &cursorpb.InteractionUpdate_TextDelta{TextDelta: &cursorpb.TextDeltaUpdate{Text: "see /Users/jdoe/x"}},
	}}}); err != nil {
		t.Fatal(err)
	}
	rec.EndStream([]byte(`{"error":{"code":"canceled","message":"bye jdoe"}}`))

	path := filepath.Join(t.TempDir(), "run.json")
	if err := rec.Fixture().Write(path); err != nil {
		t.Fatal(err)
	}
	fx, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := fx.Server()
	if len(srv) != 1 || srv[0].GetInteractionUpdate().GetTextDelta().GetText() != "see /Users/user/x" || fx.Frames[1].AtMs != 42 {
		t.Fatalf("server frames = %v (at %d)", srv, fx.Frames[1].AtMs)
	}
	if !strings.Contains(string(fx.EndStream), "bye user") {
		t.Fatalf("end stream not redacted: %s", fx.EndStream)
	}

	// Writing the same recording twice is byte-identical (stable review diffs).
	again := filepath.Join(t.TempDir(), "again.json")
	if err = rec.Fixture().Write(again); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(path)
	b, _ := os.ReadFile(again)
	if !bytes.Equal(a, b) {
		t.Fatal("fixture output is not deterministic")
	}

	stream, err := fx.ServerConnectStream()
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := proto.Marshal(srv[0])
	if stream[0] != 0 || binary.BigEndian.Uint32(stream[1:5]) != uint32(len(payload)) {
		t.Fatalf("bad first frame header % x", stream[:5])
	}
	end := stream[5+len(payload):]
	if end[0] != 0x02 || string(end[5:]) != string(fx.EndStream) {
		t.Fatalf("bad end-stream frame: % x", end[:5])
	}
}

// Every committed fixture must load and must not contain secrets or machine identity.
func TestCommittedFixturesAreValidAndRedacted(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "runs", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no fixtures found")
	}
	for _, p := range paths {
		fx, errLoad := Load(p)
		if errLoad != nil {
			t.Errorf("%s: %v", p, errLoad)
			continue
		}
		if fx.Source != "recorded" && fx.Source != "reconstructed" {
			t.Errorf("%s: source %q", p, fx.Source)
		}
		raw, _ := os.ReadFile(p)
		if hits := Scan(raw, DefaultLiterals()...); len(hits) > 0 {
			t.Errorf("%s is not redacted: %v", p, hits)
		}
	}
}

// The reconstructed fixtures encode the flow the Phase 0 spike observed.
func TestReconstructedFixtureFlow(t *testing.T) {
	fx, err := Load(filepath.Join("testdata", "runs", "q2-mcp-read-composer-2.5.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := fx.Server()
	var execs []string
	var ended *cursorpb.TurnEndedUpdate
	for _, m := range srv {
		if e := m.GetExecServerMessage(); e != nil {
			switch {
			case e.GetMcpStateExecArgs() != nil:
				execs = append(execs, "mcp_state_exec_args")
			case e.GetMcpArgs() != nil:
				execs = append(execs, "mcp_args:"+e.GetMcpArgs().GetToolName())
			}
		}
		if te := m.GetInteractionUpdate().GetTurnEnded(); te != nil {
			ended = te
		}
	}
	if strings.Join(execs, ",") != "mcp_state_exec_args,mcp_args:Read" {
		t.Fatalf("exec sequence = %v", execs)
	}
	if ended.GetInputTokens() != 25671 {
		t.Fatalf("turn_ended = %v", ended)
	}
	cli, _ := fx.Client()
	if cli[0].GetRunRequest().GetAction().GetUserMessageAction().GetRequestContext().GetRules()[0].GetType().GetGlobal() == nil {
		t.Fatal("system prompt is not a global rule")
	}
}
