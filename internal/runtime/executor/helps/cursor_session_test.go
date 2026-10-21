package helps

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
)

type fakeCursorClock struct{ t time.Time }

func (c *fakeCursorClock) now() time.Time { return c.t }

func heldRun(t *testing.T, sess *cursorSession) (*cursorLiveRun, *cursorReplayConn) {
	t.Helper()
	conn := scriptConn()
	conn.eofAtEnd = false // no frames, no end: Recv blocks like an idle held Run
	run, err := startCursorRun(context.Background(), &scriptTransport{conn: conn}, sess, &cursorpb.AgentClientMessage{}, cursorRunContext{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return run, conn
}

func TestCursorSessionStoreTTLClosesHeldRun(t *testing.T) {
	clock := &fakeCursorClock{t: time.Unix(1000, 0)}
	st := NewCursorSessionStore(10, time.Minute, clock.now)
	s, err := st.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	run, conn := heldRun(t, s)
	s.live = run
	st.release(s)

	clock.t = clock.t.Add(59 * time.Second)
	st.Sweep()
	if st.Len() != 1 || conn.isClosed() {
		t.Fatal("session expired early")
	}
	clock.t = clock.t.Add(time.Second)
	st.Sweep()
	if st.Len() != 0 {
		t.Fatal("expired session kept")
	}
	<-run.done
	if !conn.isClosed() {
		t.Fatal("held Run of an expired session left open")
	}
}

func TestCursorSessionStoreInUseNeverExpires(t *testing.T) {
	clock := &fakeCursorClock{t: time.Unix(1000, 0)}
	st := NewCursorSessionStore(10, time.Minute, clock.now)
	s, _ := st.acquire(context.Background(), "a")
	clock.t = clock.t.Add(time.Hour)
	st.Sweep()
	if st.Len() != 1 {
		t.Fatal("in-use session dropped")
	}
	st.release(s)
	clock.t = clock.t.Add(time.Minute)
	st.Sweep()
	if st.Len() != 0 {
		t.Fatal("idle TTL does not count from release")
	}
}

func TestCursorSessionStoreLRUBound(t *testing.T) {
	clock := &fakeCursorClock{t: time.Unix(1000, 0)}
	st := NewCursorSessionStore(2, time.Hour, clock.now)
	for i, k := range []string{"a", "b", "c"} {
		clock.t = time.Unix(int64(1000+i), 0)
		s, _ := st.acquire(context.Background(), k)
		st.release(s)
	}
	st.mu.Lock()
	_, hasA := st.sessions["a"]
	_, hasC := st.sessions["c"]
	st.mu.Unlock()
	if st.Len() != 2 || hasA || !hasC {
		t.Fatalf("LRU eviction wrong: len %d, a %v, c %v", st.Len(), hasA, hasC)
	}
}

func TestCursorSessionAcquireSerializesAndHonoursContext(t *testing.T) {
	st := NewCursorSessionStore(0, 0, nil)
	s, _ := st.acquire(context.Background(), "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.acquire(ctx, "a"); err == nil {
		t.Fatal("second acquire of a busy conversation did not wait")
	}
	st.release(s)
	s2, err := st.acquire(context.Background(), "a")
	if err != nil || s2 != s {
		t.Fatalf("acquire after release: %v", err)
	}
	st.release(s2)
}

func TestCursorSessionBlobOverflowDisablesCheckpoint(t *testing.T) {
	s := newCursorSession("k", 8)
	s.setCheckpoint([]byte{1})
	s.setBlob([]byte("a"), []byte("12345"))
	if s.usableCheckpoint() == nil {
		t.Fatal("checkpoint unusable before overflow")
	}
	s.setBlob([]byte("b"), []byte("12345"))
	if s.usableCheckpoint() != nil {
		t.Fatal("checkpoint usable although a blob was dropped")
	}
	if b, ok := s.getBlob([]byte("a")); !ok || string(b) != "12345" {
		t.Fatal("kept blob lost")
	}
	s.resetState()
	if s.usableCheckpoint() != nil || s.nextLen != -1 {
		t.Fatal("reset kept state")
	}
}
