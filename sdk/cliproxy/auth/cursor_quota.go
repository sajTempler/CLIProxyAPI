package auth

import (
	"context"
	"strings"
	"sync"
	"time"
)

const cursorQuotaMaxPollError = 128

// Cursor on-demand limit kinds, matching the agent CLI's /usage view.
const (
	CursorLimitFixed       = "fixed"
	CursorLimitUnlimited   = "unlimited"
	CursorLimitDisabled    = "disabled"
	CursorLimitUnavailable = "unavailable"

	CursorLimitScopeTeamMember = "team_member"
	CursorLimitScopePersonal   = "personal"
)

// CursorOnDemand is the on-demand (usage-based) spend of one Cursor account.
type CursorOnDemand struct {
	LimitKind  string  `json:"limit_kind"`
	LimitScope string  `json:"limit_scope"` // set only for unlimited
	UsedUSD    float64 `json:"used_usd"`
	LimitUSD   float64 `json:"limit_usd"` // meaningful only for fixed
}

// CursorUsage is the agent CLI /usage model of one Cursor account for the
// current billing cycle. Percentages are 0-100 and not clamped.
type CursorUsage struct {
	IncludedPercent  float64        `json:"included_percent"`
	AutoPercent      float64        `json:"auto_percent"`
	APIPercent       float64        `json:"api_percent"`
	OnDemand         CursorOnDemand `json:"on_demand"`
	CycleStart       time.Time      `json:"cycle_start"`
	ResetsAt         time.Time      `json:"resets_at"`
	IncludedSpendUSD float64        `json:"included_spend_usd"`
	IncludedLimitUSD float64        `json:"included_limit_usd"`
	BonusUSD         float64        `json:"bonus_usd"`
	TotalSpendUSD    float64        `json:"total_spend_usd"`
	AutoBucketModels []string       `json:"auto_bucket_models"`
}

// CursorQuotaPlan is the plan read from DashboardService/GetPlanInfo.
type CursorQuotaPlan struct {
	Label     string    `json:"label"`
	Price     string    `json:"price"`
	FetchedAt time.Time `json:"fetched_at"`
}

// CursorQuotaSnapshot is the usage view of one Cursor credential.
type CursorQuotaSnapshot struct {
	AuthID        string           `json:"auth_id"`
	Usage         *CursorUsage     `json:"usage,omitempty"`
	ObservedAt    time.Time        `json:"observed_at,omitempty"`
	Plan          *CursorQuotaPlan `json:"plan,omitempty"`
	LastPollAt    time.Time        `json:"last_poll_at,omitempty"`
	PollStartedAt time.Time        `json:"poll_started_at,omitempty"` // non-zero while in flight
	NextPollAt    time.Time        `json:"next_poll_at,omitempty"`
	PollError     string           `json:"poll_error,omitempty"` // sanitized, no tokens
}

func (s *CursorQuotaSnapshot) clone() CursorQuotaSnapshot {
	out := *s
	if s.Usage != nil {
		usage := *s.Usage
		usage.AutoBucketModels = append([]string(nil), s.Usage.AutoBucketModels...)
		out.Usage = &usage
	}
	if s.Plan != nil {
		plan := *s.Plan
		out.Plan = &plan
	}
	return out
}

// CursorQuotaRefresher re-reads usage for one Cursor credential on demand.
type CursorQuotaRefresher interface {
	Refresh(ctx context.Context, authID string) error
	Interval() time.Duration
}

// QuotaBackoffError is the provider-neutral name of the usage-endpoint backoff error.
type QuotaBackoffError = ClaudeQuotaBackoffError

// CursorQuotaTracker holds the latest polled Cursor usage per credential.
// Display only: no routing decision reads it.
type CursorQuotaTracker struct {
	mu        sync.RWMutex
	byAuth    map[string]*CursorQuotaSnapshot
	now       func() time.Time
	refresher CursorQuotaRefresher
}

// NewCursorQuotaTracker creates an empty tracker using the wall clock.
func NewCursorQuotaTracker() *CursorQuotaTracker {
	return &CursorQuotaTracker{byAuth: make(map[string]*CursorQuotaSnapshot), now: time.Now}
}

// SetNowFunc replaces the tracker clock. Intended for tests.
func (t *CursorQuotaTracker) SetNowFunc(now func() time.Time) {
	if t == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	t.mu.Lock()
	t.now = now
	t.mu.Unlock()
}

// Now returns the tracker clock's current time.
func (t *CursorQuotaTracker) Now() time.Time {
	if t == nil {
		return time.Now()
	}
	t.mu.RLock()
	now := t.now
	t.mu.RUnlock()
	return now()
}

// SetRefresher registers the on-demand usage refresher (the poller).
func (t *CursorQuotaTracker) SetRefresher(r CursorQuotaRefresher) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.refresher = r
	t.mu.Unlock()
}

// Refresher returns the registered refresher, or nil.
func (t *CursorQuotaTracker) Refresher() CursorQuotaRefresher {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.refresher
}

func (t *CursorQuotaTracker) entryLocked(authID string) *CursorQuotaSnapshot {
	entry := t.byAuth[authID]
	if entry == nil {
		entry = &CursorQuotaSnapshot{AuthID: authID}
		t.byAuth[authID] = entry
	}
	return entry
}

// ObserveUsage stores a successfully polled usage view.
func (t *CursorQuotaTracker) ObserveUsage(authID string, u CursorUsage, at time.Time) {
	authID = strings.TrimSpace(authID)
	if t == nil || authID == "" {
		return
	}
	usage := u
	usage.AutoBucketModels = append([]string(nil), u.AutoBucketModels...)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entryLocked(authID)
	if !entry.ObservedAt.IsZero() && at.Before(entry.ObservedAt) {
		return
	}
	entry.Usage = &usage
	entry.ObservedAt = at
}

// ObservePlan stores the credential's plan.
func (t *CursorQuotaTracker) ObservePlan(authID string, p CursorQuotaPlan) {
	authID = strings.TrimSpace(authID)
	if t == nil || authID == "" {
		return
	}
	plan := p
	t.mu.Lock()
	t.entryLocked(authID).Plan = &plan
	t.mu.Unlock()
}

// ObservePollResult records poll bookkeeping. A zero finished time marks a poll
// as in flight since started. Otherwise err (already sanitized) becomes
// PollError and success sets LastPollAt.
func (t *CursorQuotaTracker) ObservePollResult(authID string, started, finished time.Time, err error, next time.Time) {
	authID = strings.TrimSpace(authID)
	if t == nil || authID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entryLocked(authID)
	if finished.IsZero() {
		entry.PollStartedAt = started
		return
	}
	entry.PollStartedAt = time.Time{}
	entry.NextPollAt = next
	if err != nil {
		msg := err.Error()
		if len(msg) > cursorQuotaMaxPollError {
			msg = msg[:cursorQuotaMaxPollError]
		}
		entry.PollError = msg
		return
	}
	entry.PollError = ""
	entry.LastPollAt = finished
}

// Snapshot returns a deep copy of the credential's usage view.
func (t *CursorQuotaTracker) Snapshot(authID string) (CursorQuotaSnapshot, bool) {
	if t == nil {
		return CursorQuotaSnapshot{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	entry := t.byAuth[strings.TrimSpace(authID)]
	if entry == nil {
		return CursorQuotaSnapshot{}, false
	}
	return entry.clone(), true
}

// Forget drops all state for a removed credential.
func (t *CursorQuotaTracker) Forget(authID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.byAuth, strings.TrimSpace(authID))
	t.mu.Unlock()
}

// IsCursorAuth reports whether auth is a Cursor credential (`type: "cursor"`).
func IsCursorAuth(auth *Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "cursor")
}

// SetCursorQuotaTracker attaches the Cursor quota tracker.
func (m *Manager) SetCursorQuotaTracker(t *CursorQuotaTracker) {
	if m == nil {
		return
	}
	m.cursorQuota.Store(t)
}

// CursorQuotaTracker returns the attached Cursor quota tracker, or nil.
func (m *Manager) CursorQuotaTracker() *CursorQuotaTracker {
	if m == nil {
		return nil
	}
	return m.cursorQuota.Load()
}

func (m *Manager) forgetCursorQuota(authID string) {
	if m == nil {
		return
	}
	if t := m.cursorQuota.Load(); t != nil {
		t.Forget(authID)
	}
}
