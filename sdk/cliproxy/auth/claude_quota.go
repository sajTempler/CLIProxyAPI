package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClaudeQuotaWindowName identifies a tracked Anthropic subscription window.
type ClaudeQuotaWindowName string

const (
	// ClaudeWindowFiveHour is the rolling 5-hour session window (header prefix "5h").
	ClaudeWindowFiveHour ClaudeQuotaWindowName = "five_hour"
	// ClaudeWindowSevenDay is the overall weekly window (header prefix "7d").
	ClaudeWindowSevenDay ClaudeQuotaWindowName = "seven_day"
	// ClaudeWindowSevenDayFable is the Fable weekly window: header prefix "7d_oi",
	// endpoint limits[kind=weekly_scoped, scope.model.display_name=Fable].
	ClaudeWindowSevenDayFable ClaudeQuotaWindowName = "seven_day_fable"
)

// ClaudeQuotaSource values describe where a window observation came from.
const (
	ClaudeQuotaSourceHeaders = "headers"
	ClaudeQuotaSourcePoll    = "poll"
)

const (
	claudeQuotaPollQueueSize = 64
	claudeQuotaMaxPollError  = 200
)

// claudeHeaderWindows maps unified rate-limit header prefixes to tracked windows.
var claudeHeaderWindows = []struct {
	prefix string
	window ClaudeQuotaWindowName
}{
	{prefix: "5h", window: ClaudeWindowFiveHour},
	{prefix: "7d", window: ClaudeWindowSevenDay},
	{prefix: "7d_oi", window: ClaudeWindowSevenDayFable},
}

// claudeFableDisplayNames is the allowlist Claude Code uses to label the Fable weekly limit.
var claudeFableDisplayNames = map[string]struct{}{
	"fable":     {},
	"fable 5":   {},
	"fable 5.1": {},
}

// ClaudeQuotaWindow is one window's latest observation. UsedPercent is 0..100+
// (header utilization fractions are scaled by 100).
type ClaudeQuotaWindow struct {
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitempty"`
	Status      string    `json:"status,omitempty"` // allowed | allowed_warning | rejected | "" (endpoint has none)
	ObservedAt  time.Time `json:"observed_at"`
	Source      string    `json:"source"` // "headers" | "poll"
}

// Expired reports whether the window's reset time has passed at now.
func (w ClaudeQuotaWindow) Expired(now time.Time) bool {
	return !w.ResetsAt.IsZero() && !w.ResetsAt.After(now)
}

// effective returns the window as seen at now: a window whose reset time has
// passed counts as fresh (nothing used, reset unknown).
func (w ClaudeQuotaWindow) effective(now time.Time) ClaudeQuotaWindow {
	if !w.Expired(now) {
		return w
	}
	w.UsedPercent = 0
	w.ResetsAt = time.Time{}
	w.Status = ""
	return w
}

// ClaudeQuotaPlan is the subscription plan read from the OAuth profile endpoint.
type ClaudeQuotaPlan struct {
	Label            string    `json:"label"`             // "Max 5x", "Max 20x", "Pro", "Team", ""
	OrganizationType string    `json:"organization_type"` // raw
	RateLimitTier    string    `json:"rate_limit_tier"`   // raw
	FetchedAt        time.Time `json:"fetched_at"`
}

// ClaudeQuotaSnapshot is the merged quota view of one Claude credential.
type ClaudeQuotaSnapshot struct {
	AuthID        string                                      `json:"auth_id"`
	Windows       map[ClaudeQuotaWindowName]ClaudeQuotaWindow `json:"windows"`
	Plan          *ClaudeQuotaPlan                            `json:"plan,omitempty"`
	LastPollAt    time.Time                                   `json:"last_poll_at,omitempty"`
	PollStartedAt time.Time                                   `json:"poll_started_at,omitempty"` // non-zero while in flight
	NextPollAt    time.Time                                   `json:"next_poll_at,omitempty"`
	PollError     string                                      `json:"poll_error,omitempty"` // sanitized, no tokens
}

func (s *ClaudeQuotaSnapshot) clone() ClaudeQuotaSnapshot {
	out := *s
	out.Windows = make(map[ClaudeQuotaWindowName]ClaudeQuotaWindow, len(s.Windows))
	for name, window := range s.Windows {
		out.Windows[name] = window
	}
	if s.Plan != nil {
		plan := *s.Plan
		out.Plan = &plan
	}
	return out
}

// ClaudeQuotaRefresher re-reads usage for one credential on demand. The usage
// poller implements it; the management API reaches it through the tracker so
// the auth package never depends on HTTP client code.
type ClaudeQuotaRefresher interface {
	Refresh(ctx context.Context, authID string) error
	Interval() time.Duration
}

// ClaudeQuotaTracker merges passive response headers and usage-endpoint polls
// into per-credential, per-window quota snapshots. It owns its own lock and
// never calls back into the Manager, so it is safe to feed from MarkResult.
type ClaudeQuotaTracker struct {
	mu        sync.RWMutex
	byAuth    map[string]*ClaudeQuotaSnapshot
	pollReq   chan string
	now       func() time.Time
	refresher ClaudeQuotaRefresher
}

// NewClaudeQuotaTracker creates an empty tracker using the wall clock.
func NewClaudeQuotaTracker() *ClaudeQuotaTracker {
	return &ClaudeQuotaTracker{
		byAuth:  make(map[string]*ClaudeQuotaSnapshot),
		pollReq: make(chan string, claudeQuotaPollQueueSize),
		now:     time.Now,
	}
}

// SetNowFunc replaces the clock used for window expiry. Intended for tests.
func (t *ClaudeQuotaTracker) SetNowFunc(now func() time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	t.now = now
}

// Now returns the tracker clock's current time.
func (t *ClaudeQuotaTracker) Now() time.Time {
	if t == nil {
		return time.Now()
	}
	t.mu.RLock()
	now := t.now
	t.mu.RUnlock()
	return now()
}

// SetRefresher registers the on-demand usage refresher (the poller).
func (t *ClaudeQuotaTracker) SetRefresher(r ClaudeQuotaRefresher) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.refresher = r
	t.mu.Unlock()
}

// Refresher returns the registered on-demand refresher, or nil.
func (t *ClaudeQuotaTracker) Refresher() ClaudeQuotaRefresher {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.refresher
}

func (t *ClaudeQuotaTracker) entryLocked(authID string) *ClaudeQuotaSnapshot {
	entry := t.byAuth[authID]
	if entry == nil {
		entry = &ClaudeQuotaSnapshot{AuthID: authID, Windows: make(map[ClaudeQuotaWindowName]ClaudeQuotaWindow)}
		t.byAuth[authID] = entry
	}
	return entry
}

// mergeWindowLocked stores next unless an observation newer than it is already held.
func mergeWindowLocked(entry *ClaudeQuotaSnapshot, name ClaudeQuotaWindowName, next ClaudeQuotaWindow) {
	if current, ok := entry.Windows[name]; ok && next.ObservedAt.Before(current.ObservedAt) {
		return
	}
	entry.Windows[name] = next
}

// ObserveHeaders records the anthropic-ratelimit-unified-* windows carried by
// one response. A window is updated only when its utilization or reset header
// is present, so a response without the Fable (7d_oi) headers never clears the
// Fable window. Non-unified headers are ignored. model is accepted for future
// per-model scoping and is currently unused.
func (t *ClaudeQuotaTracker) ObserveHeaders(authID, model string, h http.Header, at time.Time) {
	_ = model
	authID = strings.TrimSpace(authID)
	if t == nil || authID == "" || len(h) == 0 {
		return
	}
	if at.IsZero() {
		at = t.Now()
	}
	observed := make(map[ClaudeQuotaWindowName]ClaudeQuotaWindow, len(claudeHeaderWindows))
	for _, spec := range claudeHeaderWindows {
		base := "Anthropic-Ratelimit-Unified-" + spec.prefix
		rawUtil := strings.TrimSpace(h.Get(base + "-Utilization"))
		rawReset := strings.TrimSpace(h.Get(base + "-Reset"))
		if rawUtil == "" && rawReset == "" {
			continue
		}
		window := ClaudeQuotaWindow{
			UsedPercent: math.NaN(),
			Status:      strings.ToLower(strings.TrimSpace(h.Get(base + "-Status"))),
			ObservedAt:  at,
			Source:      ClaudeQuotaSourceHeaders,
		}
		if rawUtil != "" {
			if fraction, errParse := strconv.ParseFloat(rawUtil, 64); errParse == nil && !math.IsNaN(fraction) && !math.IsInf(fraction, 0) {
				window.UsedPercent = roundPercent(fraction * 100)
			}
		}
		if rawReset != "" {
			window.ResetsAt = parseClaudeResetValue(rawReset)
		}
		observed[spec.window] = window
	}
	if len(observed) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entryLocked(authID)
	for name, window := range observed {
		if math.IsNaN(window.UsedPercent) {
			// Reset without utilization: keep the last known usage for this window.
			window.UsedPercent = 0
			if current, ok := entry.Windows[name]; ok {
				window.UsedPercent = current.UsedPercent
			}
			if window.Status == "rejected" && window.UsedPercent < 100 {
				window.UsedPercent = 100
			}
		}
		mergeWindowLocked(entry, name, window)
	}
}

// ClaudeUsageResponse is the subset of GET /api/oauth/usage we read.
type ClaudeUsageResponse struct {
	FiveHour *ClaudeUsageWindow `json:"five_hour"`
	SevenDay *ClaudeUsageWindow `json:"seven_day"`
	Limits   []ClaudeUsageLimit `json:"limits"`
}

// ClaudeUsageWindow is a top-level usage window; utilization is percent used (0..100).
type ClaudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// ClaudeUsageLimit is one entry of the usage response's limits[] array.
type ClaudeUsageLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	IsActive bool     `json:"is_active"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

func (l ClaudeUsageLimit) isFable() bool {
	if !strings.EqualFold(strings.TrimSpace(l.Kind), "weekly_scoped") || l.Scope == nil || l.Scope.Model == nil {
		return false
	}
	_, ok := claudeFableDisplayNames[strings.ToLower(strings.TrimSpace(l.Scope.Model.DisplayName))]
	return ok
}

// fableLimit returns the Fable weekly limit, preferring an active entry.
func (u ClaudeUsageResponse) fableLimit() *ClaudeUsageLimit {
	var first *ClaudeUsageLimit
	for i := range u.Limits {
		limit := &u.Limits[i]
		if !limit.isFable() || limit.Percent == nil {
			continue
		}
		if limit.IsActive {
			return limit
		}
		if first == nil {
			first = limit
		}
	}
	return first
}

// ObserveUsage records the windows reported by the OAuth usage endpoint.
// A null resets_at is stored as the zero time ("no reset pending").
func (t *ClaudeQuotaTracker) ObserveUsage(authID string, u ClaudeUsageResponse, at time.Time) {
	authID = strings.TrimSpace(authID)
	if t == nil || authID == "" {
		return
	}
	if at.IsZero() {
		at = t.Now()
	}
	observed := make(map[ClaudeQuotaWindowName]ClaudeQuotaWindow, 3)
	add := func(name ClaudeQuotaWindowName, used *float64, resetsAt *string) {
		if used == nil {
			return
		}
		window := ClaudeQuotaWindow{UsedPercent: roundPercent(*used), ObservedAt: at, Source: ClaudeQuotaSourcePoll}
		if resetsAt != nil {
			window.ResetsAt = parseClaudeResetValue(*resetsAt)
		}
		observed[name] = window
	}
	if u.FiveHour != nil {
		add(ClaudeWindowFiveHour, u.FiveHour.Utilization, u.FiveHour.ResetsAt)
	}
	if u.SevenDay != nil {
		add(ClaudeWindowSevenDay, u.SevenDay.Utilization, u.SevenDay.ResetsAt)
	}
	if fable := u.fableLimit(); fable != nil {
		add(ClaudeWindowSevenDayFable, fable.Percent, fable.ResetsAt)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entryLocked(authID)
	for name, window := range observed {
		mergeWindowLocked(entry, name, window)
	}
}

// ObservePlan stores the credential's subscription plan.
func (t *ClaudeQuotaTracker) ObservePlan(authID string, p ClaudeQuotaPlan) {
	authID = strings.TrimSpace(authID)
	if t == nil || authID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	plan := p
	t.entryLocked(authID).Plan = &plan
}

// ObservePollResult records poll bookkeeping. A zero finished time marks a poll
// as in flight since started. Otherwise the poll is complete: err (already
// sanitized by the caller) becomes PollError, success sets LastPollAt.
func (t *ClaudeQuotaTracker) ObservePollResult(authID string, started, finished time.Time, err error, next time.Time) {
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
		if len(msg) > claudeQuotaMaxPollError {
			msg = msg[:claudeQuotaMaxPollError]
		}
		entry.PollError = msg
		return
	}
	entry.PollError = ""
	entry.LastPollAt = finished
}

// Snapshot returns a deep copy of the credential's quota view with expired
// windows reported as fresh (0 % used, reset unknown).
func (t *ClaudeQuotaTracker) Snapshot(authID string) (ClaudeQuotaSnapshot, bool) {
	if t == nil {
		return ClaudeQuotaSnapshot{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	entry := t.byAuth[strings.TrimSpace(authID)]
	if entry == nil {
		return ClaudeQuotaSnapshot{}, false
	}
	out := entry.clone()
	now := t.now()
	for name, window := range out.Windows {
		out.Windows[name] = window.effective(now)
	}
	return out, true
}

// Forget drops all state for a removed credential.
func (t *ClaudeQuotaTracker) Forget(authID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.byAuth, strings.TrimSpace(authID))
	t.mu.Unlock()
}

// RequestPoll asks the poller to refresh authID soon. It never blocks; when the
// queue is full the request is dropped and the regular schedule covers it.
func (t *ClaudeQuotaTracker) RequestPoll(authID string) {
	authID = strings.TrimSpace(authID)
	if t == nil || authID == "" {
		return
	}
	select {
	case t.pollReq <- authID:
	default:
	}
}

// PollRequests exposes the poll request queue to the poller.
func (t *ClaudeQuotaTracker) PollRequests() <-chan string {
	if t == nil {
		return nil
	}
	return t.pollReq
}

// SetClaudeQuotaTracker attaches the Claude quota tracker fed by MarkResult.
func (m *Manager) SetClaudeQuotaTracker(t *ClaudeQuotaTracker) {
	if m == nil {
		return
	}
	m.claudeQuota.Store(t)
}

// ClaudeQuotaTracker returns the attached Claude quota tracker, or nil.
func (m *Manager) ClaudeQuotaTracker() *ClaudeQuotaTracker {
	if m == nil {
		return nil
	}
	return m.claudeQuota.Load()
}

// observeClaudeQuota feeds one result's response headers to the tracker.
func (m *Manager) observeClaudeQuota(result Result, authID string, headers http.Header, at time.Time) {
	if m == nil || len(headers) == 0 || !strings.EqualFold(strings.TrimSpace(result.Provider), "claude") {
		return
	}
	if t := m.claudeQuota.Load(); t != nil {
		t.ObserveHeaders(authID, result.Model, headers, at)
	}
}

// forgetClaudeQuota drops tracker state for a removed credential.
func (m *Manager) forgetClaudeQuota(authID string) {
	if m == nil {
		return
	}
	if t := m.claudeQuota.Load(); t != nil {
		t.Forget(authID)
	}
}

// parseClaudeResetValue accepts unix seconds (integer or fractional) or RFC 3339.
func parseClaudeResetValue(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if seconds, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
		if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return time.Time{}
		}
		whole, frac := math.Modf(seconds)
		return time.Unix(int64(whole), int64(frac*1e9)).UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999-07:00"} {
		if parsed, errParse := time.Parse(layout, raw); errParse == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// roundPercent removes float noise from header fractions (0.55*100 = 55.00000000000001).
func roundPercent(v float64) float64 {
	return math.Round(v*100) / 100
}
