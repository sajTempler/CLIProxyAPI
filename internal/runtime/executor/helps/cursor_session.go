package helps

import (
	"context"
	"encoding/hex"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Per-conversation Cursor state. Cursor ignores client-sent history
// (SPIKE.md Q1), so the provider keeps what the server needs to continue a
// conversation: the last conversation checkpoint, the KV blobs it references,
// and, across a tool round-trip, the still-open Run itself.

const (
	// DefaultCursorMaxSessions bounds how many conversations keep state.
	DefaultCursorMaxSessions = 128
	// DefaultCursorSessionTTL is how long an idle conversation keeps state.
	// Expiry only costs a fallback (checkpoint resume or transcript seeding).
	DefaultCursorSessionTTL = time.Hour
	// DefaultCursorMaxBlobBytes bounds one conversation's KV blob store.
	DefaultCursorMaxBlobBytes = 64 << 20
)

// CursorSessionStore holds conversation state keyed by a caller-supplied
// conversation key. Bounded by count (LRU) and idle TTL; the clock is
// injectable so expiry is testable without sleeping.
type CursorSessionStore struct {
	maxSessions  int
	ttl          time.Duration
	maxBlobBytes int
	now          func() time.Time

	mu       sync.Mutex
	sessions map[string]*cursorSession
}

// NewCursorSessionStore returns a store; zero values pick the defaults and now may be nil.
func NewCursorSessionStore(maxSessions int, ttl time.Duration, now func() time.Time) *CursorSessionStore {
	if maxSessions <= 0 {
		maxSessions = DefaultCursorMaxSessions
	}
	if ttl <= 0 {
		ttl = DefaultCursorSessionTTL
	}
	if now == nil {
		now = time.Now
	}
	return &CursorSessionStore{maxSessions: maxSessions, ttl: ttl, maxBlobBytes: DefaultCursorMaxBlobBytes, now: now, sessions: map[string]*cursorSession{}}
}

// cursorSession is one conversation. turn serializes requests; mu guards the
// fields the Run reader goroutine also touches (checkpoint, blobs).
type cursorSession struct {
	key  string
	turn chan struct{}

	// Guarded by the store mutex.
	lastUsed time.Time
	inUse    int

	// Guarded by turn (only the request holding the turn reads or writes these).
	live      *cursorLiveRun    // Run held open across a tool round-trip, or nil
	finishing *cursorLiveRun    // Run past turn_ended, draining its last checkpoint
	pending   []*cursorToolCall // tool_use blocks the last response ended with
	nextLen   int               // message count the next request has if it continues this state

	mu             sync.Mutex
	conversationID string
	checkpoint     []byte
	blobs          map[string][]byte
	blobBytes      int
	blobOverflow   bool
	maxBlobBytes   int
}

func newCursorSession(key string, maxBlobBytes int) *cursorSession {
	s := &cursorSession{key: key, turn: make(chan struct{}, 1), maxBlobBytes: maxBlobBytes}
	s.resetState()
	return s
}

// resetState forgets everything the server knows about this conversation.
// Callers hold the turn and have closed any live Run.
func (s *cursorSession) resetState() {
	s.live, s.finishing, s.pending, s.nextLen = nil, nil, nil, -1
	s.mu.Lock()
	s.conversationID = uuid.NewString()
	s.checkpoint, s.blobs, s.blobBytes, s.blobOverflow = nil, map[string][]byte{}, 0, false
	s.mu.Unlock()
}

// closeRuns aborts the live and finishing Runs (eviction, reset, errors).
func (s *cursorSession) closeRuns() {
	if s.live != nil {
		s.live.close()
		s.live = nil
	}
	if s.finishing != nil {
		s.finishing.close()
		s.finishing = nil
	}
}

func (s *cursorSession) setCheckpoint(b []byte) {
	s.mu.Lock()
	s.checkpoint = b
	s.mu.Unlock()
}

// usableCheckpoint returns the checkpoint if every blob it may reference was kept.
func (s *cursorSession) usableCheckpoint() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blobOverflow {
		return nil
	}
	return s.checkpoint
}

func (s *cursorSession) convID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conversationID
}

func (s *cursorSession) setBlob(id, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := hex.EncodeToString(id)
	if old, ok := s.blobs[k]; ok {
		s.blobBytes -= len(old)
	}
	if s.blobBytes+len(data) > s.maxBlobBytes {
		if !s.blobOverflow {
			log.WithField("blob_bytes", s.blobBytes).Warn("cursor: session blob store full; the next turn falls back to transcript seeding")
		}
		s.blobOverflow = true
		delete(s.blobs, k)
		return
	}
	s.blobs[k] = append([]byte(nil), data...)
	s.blobBytes += len(data)
}

func (s *cursorSession) getBlob(id []byte) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[hex.EncodeToString(id)]
	return b, ok
}

// acquire returns the session for key with its turn held. It blocks while
// another request of the same conversation is in flight (ctx cancels the wait).
func (st *CursorSessionStore) acquire(ctx context.Context, key string) (*cursorSession, error) {
	st.mu.Lock()
	st.sweepLocked()
	s, ok := st.sessions[key]
	if !ok {
		s = newCursorSession(key, st.maxBlobBytes)
		st.sessions[key] = s
	}
	s.inUse++
	s.lastUsed = st.now()
	if !ok {
		st.evictLocked()
	}
	st.mu.Unlock()

	select {
	case s.turn <- struct{}{}:
		return s, nil
	case <-ctx.Done():
		st.mu.Lock()
		s.inUse--
		st.mu.Unlock()
		return nil, ctx.Err()
	}
}

// release ends a request's turn; the idle TTL counts from here.
func (st *CursorSessionStore) release(s *cursorSession) {
	st.mu.Lock()
	s.inUse--
	s.lastUsed = st.now()
	st.mu.Unlock()
	<-s.turn
}

// Sweep drops expired idle sessions and closes their held Runs. acquire calls
// it; a caller may also run it periodically so abandoned Runs close sooner.
func (st *CursorSessionStore) Sweep() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked()
}

// Len reports the number of sessions (tests, diagnostics).
func (st *CursorSessionStore) Len() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions)
}

func (st *CursorSessionStore) sweepLocked() {
	now := st.now()
	for k, s := range st.sessions {
		if s.inUse == 0 && now.Sub(s.lastUsed) >= st.ttl {
			st.dropLocked(k, s)
		}
	}
}

// evictLocked removes least-recently-used idle sessions above the bound.
func (st *CursorSessionStore) evictLocked() {
	for len(st.sessions) > st.maxSessions {
		var victim *cursorSession
		for _, s := range st.sessions {
			if s.inUse == 0 && (victim == nil || s.lastUsed.Before(victim.lastUsed)) {
				victim = s
			}
		}
		if victim == nil {
			return // everything is in use; the bound is exceeded until requests finish
		}
		st.dropLocked(victim.key, victim)
	}
}

// dropLocked removes an idle session. Its turn is free (inUse == 0), so no
// request touches its Runs; closing them here is race-free.
func (st *CursorSessionStore) dropLocked(key string, s *cursorSession) {
	delete(st.sessions, key)
	s.closeRuns()
}
