//go:build cursorlive

package helps

// Live Cursor harness: records golden fixtures through the real wire client.
// Never runs in `go test ./...` (build tag). Usage, from the repo root:
//
//	CURSOR_LIVE_SCENARIO=plain-text go test -tags cursorlive -run TestCursorLive -v ./internal/runtime/executor/helps/
//
// The Cursor token is read from the macOS keychain (service
// cursor-access-token, account cursor-user) at runtime and never printed or
// written. Every Run counts against temp/cursor-phase2/live-runs.txt (cap 30).
// Fixtures go to $CURSOR_FIXTURE_OUT (default: cursorfixture/testdata/runs).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorfixture"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const cursorLiveBudget = 30

func cursorKeychainToken(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", "cursor-access-token", "-a", "cursor-user", "-w").Output()
	if err != nil {
		return "", fmt.Errorf("keychain lookup failed")
	}
	return strings.TrimSpace(string(out)), nil
}

// budgetTransport counts Runs against the live budget before opening them.
type budgetTransport struct {
	inner   cursorTransport
	counter string
	t       *testing.T
}

func (b *budgetTransport) OpenRun(ctx context.Context, first *cursorpb.AgentClientMessage) (cursorRunConn, error) {
	raw, _ := os.ReadFile(b.counter)
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if n >= cursorLiveBudget {
		return nil, fmt.Errorf("live run budget exhausted (%d)", n)
	}
	if err := os.WriteFile(b.counter, []byte(strconv.Itoa(n+1)), 0o644); err != nil {
		return nil, err
	}
	b.t.Logf("live run #%d", n+1)
	return b.inner.OpenRun(ctx, first)
}

type cursorLiveScenario struct {
	model       string
	description string
	system      string
	tools       []map[string]any
	turns       []string
	// loseRunAfterToolUse closes the held Run after the first tool_use to exercise resume.
	loseRunAfterToolUse bool
	// expectError records a Run that fails before output.
	expectError bool
}

func cursorLiveScenarios() map[string]cursorLiveScenario {
	sys := "You are Claude Code, Anthropic's official CLI for Claude. Be concise.\n\n<env>\nWorking directory: /tmp/cursor-live-ws\nPlatform: darwin\n</env>"
	return map[string]cursorLiveScenario{
		"plain-text": {model: "composer-2.5", description: "Plain text answer, no tool use (tools offered).", system: sys, tools: cursorTestTools,
			turns: []string{"Reply with one short sentence: what is 2+2?"}},
		"tool-roundtrip": {model: "composer-2.5", description: "Single MCP tool round-trip on a held-open Run, then a new user turn on the checkpoint.", system: sys, tools: cursorTestTools,
			turns: []string{"Use the lookup_secret tool to get the secret for key 'alpha' and tell me the value.", "Thanks. Now say the value backwards, no tools."}},
		"two-tools": {model: "composer-2.5", description: "Two lookup_secret calls requested in one step.", system: sys, tools: cursorTestTools,
			turns: []string{"Call lookup_secret for key 'alpha' and for key 'beta' in parallel (two tool calls in the same step, do not wait between them), then report both values."}},
		"two-tools-sonnet": {model: "claude-sonnet-5-low", description: "Two lookup_secret calls requested in one step (claude-sonnet-5-low).", system: sys, tools: cursorTestTools,
			turns: []string{"Call lookup_secret for key 'alpha' and for key 'beta' in parallel (two tool calls in the same step, do not wait between them), then report both values."}},
		"two-tools-gpt": {model: "gpt-5.4-mini", description: "Two lookup_secret calls requested in one step (gpt-5.4-mini, reasoning).", system: sys, tools: cursorTestTools,
			turns: []string{"Call lookup_secret for key 'alpha' and for key 'beta' in parallel (two tool calls in the same step, do not wait between them), then report both values."}},
		"resume-lost-run": {model: "composer-2.5", description: "Held Run lost after tool_use; the tool_result resumes from the checkpoint.", system: sys, tools: cursorTestTools, loseRunAfterToolUse: true,
			turns: []string{"Use the lookup_secret tool to get the secret for key 'alpha' and tell me the value."}},
		"big-prompt": {model: "composer-2.5", description: "~15k-token Claude Code-sized system prompt as the global rule; rule adherence check.", system: cursorBigSystemPrompt(), tools: cursorTestTools,
			turns: []string{"Who are you, and which product are you running in? Answer in one sentence."}},
	}
}

func TestCursorLive(t *testing.T) {
	name := os.Getenv("CURSOR_LIVE_SCENARIO")
	sc, ok := cursorLiveScenarios()[name]
	if !ok {
		t.Skipf("set CURSOR_LIVE_SCENARIO to one of the scenarios")
	}
	repo, _ := filepath.Abs("../../../..")
	counter := filepath.Join(repo, "temp", "cursor-phase2", "live-runs.txt")
	if err := os.MkdirAll(filepath.Dir(counter), 0o755); err != nil {
		t.Fatal(err)
	}
	outDir := os.Getenv("CURSOR_FIXTURE_OUT")
	if outDir == "" {
		outDir = filepath.Join("cursorfixture", "testdata", "runs")
	}

	httpT := &CursorHTTPTransport{Tokens: CursorTokenFunc(cursorKeychainToken)}
	host := httpT.AgentHost(context.Background())
	t.Logf("agent host: %s, client version: %s", host, CursorClientVersionHeader())
	client := newCursorClient(&budgetTransport{inner: httpT, counter: counter, t: t}, NewCursorSessionStore(0, 0, nil))

	var mu sync.Mutex
	var recorders []*lockedRecorder
	client.Observe = func(_, model string) CursorFrameObserver {
		mu.Lock()
		defer mu.Unlock()
		inner := cursorfixture.NewRecorder(GetCursorClientVersion(), model, sc.description, nil, nil)
		inner.ElideBlobs = true
		rec := &lockedRecorder{rec: inner, t: t, n: len(recorders) + 1}
		recorders = append(recorders, rec)
		return rec
	}
	writeAll := func() {
		mu.Lock()
		defer mu.Unlock()
		for i, rec := range recorders {
			path := filepath.Join(outDir, fmt.Sprintf("%s-%d.json", name, i+1))
			if len(sc.turns) == 1 && !sc.loseRunAfterToolUse {
				path = filepath.Join(outDir, name+".json")
			}
			if _, err := rec.write(path); err != nil {
				t.Error(err)
			}
		}
	}

	// Snapshot fixtures every few seconds so a hung Run still leaves its frames.
	stopSnap := make(chan struct{})
	defer close(stopSnap)
	go func() {
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopSnap:
				return
			case <-tick.C:
				writeAll()
			}
		}
	}()

	conv := &cursorTestConversation{System: sc.system, Tools: sc.tools, Thinking: true}
	key := "live-" + name
	lostOnce := false
	for ti, turn := range sc.turns {
		conv.user(turn)
		for step := 0; step < 6; step++ {
			var events []CursorClaudeEvent
			start := time.Now()
			var first time.Duration
			err := client.Stream(context.Background(), CursorStreamRequest{ConversationKey: key, Model: sc.model, Body: conv.body(sc.model)}, func(ev CursorClaudeEvent) error {
				if first == 0 && ev.Type == "content_block_delta" {
					first = time.Since(start)
				}
				events = append(events, ev)
				return nil
			})
			if err != nil && sc.expectError && len(events) == 0 {
				t.Logf("expected error: %v", err)
				writeAll()
				return
			}
			if err != nil {
				writeAll()
				t.Fatalf("turn %d step %d: %v", ti, step, err)
			}
			a, err := assembleCursorClaude(events)
			if err != nil {
				t.Fatalf("turn %d step %d: %v", ti, step, err)
			}
			var kinds []string
			for _, b := range a.Blocks {
				k := b["type"].(string)
				if k == "tool_use" {
					k += fmt.Sprintf("(%s %v)", b["name"], b["input"])
				}
				kinds = append(kinds, k)
			}
			t.Logf("turn %d step %d: first delta %v, total %v, stop=%s blocks=%v usage=%v", ti, step, first.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), a.StopReason, kinds, a.Usage)
			t.Logf("  text: %q", a.text())
			writeAll()
			conv.assistant(a)
			if a.StopReason != cursorStopToolUse {
				break
			}
			if sc.loseRunAfterToolUse && !lostOnce {
				lostOnce = true
				withCursorSession(client.sessions, key, func(s *cursorSession) {
					s.live.close()
					<-s.live.done
				})
				t.Logf("  held Run closed to simulate a lost stream")
			}
			conv.toolResults(a, cursorTestToolRunner)
		}
	}
	// Let the last Run drain its final checkpoint before writing fixtures.
	withCursorSession(client.sessions, key, func(s *cursorSession) { _ = client.awaitFinishing(context.Background(), s) })

	writeAll()
}

// lockedRecorder serializes a Recorder (reader, heartbeat and request
// goroutines all report frames) and traces frame kinds to the test log.
type lockedRecorder struct {
	mu  sync.Mutex
	rec *cursorfixture.Recorder
	t   *testing.T
	n   int
}

func (l *lockedRecorder) Client(m *cursorpb.AgentClientMessage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if os.Getenv("CURSOR_LIVE_TRACE") != "" {
		l.t.Logf("    run%d -> %s", l.n, cursorTraceKind(m))
	}
	return l.rec.Client(m)
}

func (l *lockedRecorder) Server(m *cursorpb.AgentServerMessage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if os.Getenv("CURSOR_LIVE_TRACE") != "" {
		l.t.Logf("    run%d <- %s", l.n, cursorTraceKind(m))
	}
	return l.rec.Server(m)
}

func (l *lockedRecorder) EndStream(raw []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rec.EndStream(raw)
}

func (l *lockedRecorder) write(path string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.rec.Fixture().Frames), l.rec.Fixture().Write(path)
}

// cursorTraceKind names a message by its nested oneof path (no content).
func cursorTraceKind(m proto.Message) string {
	var parts []string
	rm := m.ProtoReflect()
	for depth := 0; depth < 3; depth++ {
		ods := rm.Descriptor().Oneofs()
		var next protoreflect.Message
		for i := 0; i < ods.Len(); i++ {
			if fd := rm.WhichOneof(ods.Get(i)); fd != nil {
				parts = append(parts, string(fd.Name()))
				if fd.Message() != nil {
					next = rm.Get(fd).Message()
				}
				break
			}
		}
		if next == nil {
			break
		}
		rm = next
	}
	return strings.Join(parts, ".")
}
