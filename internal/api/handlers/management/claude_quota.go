package management

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// claudeQuotaWindowOrder is the display order: Fable weekly, 5-hour, weekly overall.
var claudeQuotaWindowOrder = []coreauth.ClaudeQuotaWindowName{
	coreauth.ClaudeWindowSevenDayFable,
	coreauth.ClaudeWindowFiveHour,
	coreauth.ClaudeWindowSevenDay,
}

type claudeQuotaWindowJSON struct {
	UsedPercent      float64    `json:"used_percent"`
	RemainingPercent float64    `json:"remaining_percent"`
	ResetsAt         *time.Time `json:"resets_at"`
	Status           string     `json:"status"`
	ObservedAt       *time.Time `json:"observed_at"`
	Source           string     `json:"source"`
}

type claudeQuotaRoutingJSON struct {
	Rank          int        `json:"rank"`
	State         string     `json:"state"`
	WeeklyResetAt *time.Time `json:"weekly_reset_at"`
	KeySource     string     `json:"key_source"`
}

type claudeQuotaPlanJSON struct {
	Label            string `json:"label"`
	OrganizationType string `json:"organization_type"`
	RateLimitTier    string `json:"rate_limit_tier"`
}

type claudeQuotaCredentialJSON struct {
	AuthIndex     string                                  `json:"auth_index"`
	ID            string                                  `json:"id"`
	Name          string                                  `json:"name"`
	Email         string                                  `json:"email"`
	Label         string                                  `json:"label"`
	Disabled      bool                                    `json:"disabled"`
	Status        string                                  `json:"status"`
	CooldownUntil *time.Time                              `json:"cooldown_until"`
	Plan          *claudeQuotaPlanJSON                    `json:"plan"`
	Routing       *claudeQuotaRoutingJSON                 `json:"routing"`
	Windows       map[string]*claudeQuotaWindowJSON       `json:"windows"`
	LastPollAt    *time.Time                              `json:"last_poll_at"`
	NextPollAt    *time.Time                              `json:"next_poll_at"`
	PollStartedAt *time.Time                              `json:"poll_started_at"`
	PollError     string                                  `json:"poll_error"`
	windows       map[coreauth.ClaudeQuotaWindowName]bool // loaded windows (not serialized)
}

type claudeQuotaSegmentJSON struct {
	AuthIndex        string  `json:"auth_index"`
	RemainingPercent float64 `json:"remaining_percent"`
}

type claudeQuotaSummaryJSON struct {
	Credentials               int                      `json:"credentials"`
	Loaded                    int                      `json:"loaded"`
	WeeklyWindow              string                   `json:"weekly_window"`
	WeeklyRemainingPercentSum float64                  `json:"weekly_remaining_percent_sum"`
	WeeklyCapacityPercent     float64                  `json:"weekly_capacity_percent"`
	Segments                  []claudeQuotaSegmentJSON `json:"segments"`
	NextResetAt               *time.Time               `json:"next_reset_at"`
}

type claudeQuotaResponseJSON struct {
	Provider               string                      `json:"provider"`
	GeneratedAt            time.Time                   `json:"generated_at"`
	Strategy               string                      `json:"strategy"`
	RefreshIntervalSeconds int64                       `json:"refresh_interval_seconds"`
	Summary                claudeQuotaSummaryJSON      `json:"summary"`
	Credentials            []claudeQuotaCredentialJSON `json:"credentials"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func remainingPercent(used float64) float64 {
	return math.Max(0, 100-used)
}

// claudeQuotaAuths returns the Claude OAuth credentials, ordered by name.
func (h *Handler) claudeQuotaAuths() []*coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	var out []*coreauth.Auth
	for _, auth := range h.authManager.List() {
		if coreauth.IsClaudeOAuthAuth(auth) {
			out = append(out, auth)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ni, nj := claudeQuotaAuthName(out[i]), claudeQuotaAuthName(out[j])
		if ni != nj {
			return ni < nj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func claudeQuotaAuthName(auth *coreauth.Auth) string {
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return name
	}
	return auth.ID
}

func (h *Handler) claudeQuotaSettings() (strategy string, cfg coreauth.SoonestResetConfig, interval time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return "round-robin", cfg, 0
	}
	strategy, ok := normalizeRoutingStrategy(h.cfg.Routing.Strategy)
	if !ok {
		strategy = strings.TrimSpace(h.cfg.Routing.Strategy)
	}
	cfg.SkipFiveHourAtPercent = h.cfg.Routing.ClaudeQuota.EffectiveSkipFiveHourAt()
	interval = h.cfg.Routing.ClaudeQuota.EffectiveRefreshInterval(h.cfg.Routing.Strategy)
	return strategy, cfg, interval
}

func claudeQuotaCooldownUntil(auth *coreauth.Auth, now time.Time) *time.Time {
	var until time.Time
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
		until = auth.Quota.NextRecoverAt
	}
	if auth.Unavailable && auth.NextRetryAfter.After(until) && auth.NextRetryAfter.After(now) {
		until = auth.NextRetryAfter
	}
	return timePtr(until)
}

func (h *Handler) buildClaudeQuotaEntries(auths []*coreauth.Auth, tracker *coreauth.ClaudeQuotaTracker, rankCfg coreauth.SoonestResetConfig, now time.Time) []claudeQuotaCredentialJSON {
	ranks := coreauth.RankClaudeQuota(auths, "", tracker, rankCfg, now)
	rankByID := make(map[string]int, len(ranks))
	for i, r := range ranks {
		rankByID[r.AuthID] = i
	}
	entries := make([]claudeQuotaCredentialJSON, 0, len(auths))
	for _, auth := range auths {
		entry := claudeQuotaCredentialJSON{
			AuthIndex:     lockedAuthIndex(auth),
			ID:            auth.ID,
			Name:          claudeQuotaAuthName(auth),
			Email:         authEmail(auth),
			Label:         auth.Label,
			Disabled:      auth.Disabled,
			Status:        string(auth.Status),
			CooldownUntil: claudeQuotaCooldownUntil(auth, now),
			Windows:       make(map[string]*claudeQuotaWindowJSON, len(claudeQuotaWindowOrder)),
			windows:       map[coreauth.ClaudeQuotaWindowName]bool{},
		}
		for _, name := range claudeQuotaWindowOrder {
			entry.Windows[string(name)] = nil
		}
		if i, ok := rankByID[auth.ID]; ok {
			r := ranks[i]
			entry.Routing = &claudeQuotaRoutingJSON{Rank: i + 1, State: r.State, WeeklyResetAt: timePtr(r.WeeklyResetAt), KeySource: r.KeySource}
		}
		if snap, ok := tracker.Snapshot(auth.ID); ok {
			for _, name := range claudeQuotaWindowOrder {
				w, okWindow := snap.Windows[name]
				if !okWindow {
					continue
				}
				entry.windows[name] = true
				entry.Windows[string(name)] = &claudeQuotaWindowJSON{
					UsedPercent:      w.UsedPercent,
					RemainingPercent: remainingPercent(w.UsedPercent),
					ResetsAt:         timePtr(w.ResetsAt),
					Status:           w.Status,
					ObservedAt:       timePtr(w.ObservedAt),
					Source:           w.Source,
				}
			}
			if snap.Plan != nil {
				entry.Plan = &claudeQuotaPlanJSON{Label: snap.Plan.Label, OrganizationType: snap.Plan.OrganizationType, RateLimitTier: snap.Plan.RateLimitTier}
			}
			entry.LastPollAt = timePtr(snap.LastPollAt)
			entry.NextPollAt = timePtr(snap.NextPollAt)
			entry.PollStartedAt = timePtr(snap.PollStartedAt)
			entry.PollError = snap.PollError
		}
		entries = append(entries, entry)
	}
	return entries
}

// summarizeClaudeQuota sums remaining weekly quota across loaded credentials.
// The weekly window is Fable when every loaded credential has it, else the
// overall 7-day window. A loaded credential without that window counts as untouched.
func summarizeClaudeQuota(entries []claudeQuotaCredentialJSON, now time.Time) claudeQuotaSummaryJSON {
	summary := claudeQuotaSummaryJSON{Credentials: len(entries), Segments: []claudeQuotaSegmentJSON{}}
	allFable := true
	for _, e := range entries {
		if len(e.windows) == 0 {
			continue
		}
		summary.Loaded++
		if !e.windows[coreauth.ClaudeWindowSevenDayFable] {
			allFable = false
		}
	}
	weekly := coreauth.ClaudeWindowSevenDay
	if summary.Loaded > 0 && allFable {
		weekly = coreauth.ClaudeWindowSevenDayFable
	}
	summary.WeeklyWindow = string(weekly)
	var next time.Time
	for _, e := range entries {
		if len(e.windows) == 0 {
			continue
		}
		remaining := 100.0
		if w := e.Windows[string(weekly)]; w != nil {
			remaining = w.RemainingPercent
			if w.ResetsAt != nil && w.ResetsAt.After(now) && (next.IsZero() || w.ResetsAt.Before(next)) {
				next = *w.ResetsAt
			}
		}
		summary.WeeklyRemainingPercentSum += remaining
		summary.WeeklyCapacityPercent += 100
		summary.Segments = append(summary.Segments, claudeQuotaSegmentJSON{AuthIndex: e.AuthIndex, RemainingPercent: remaining})
	}
	summary.NextResetAt = timePtr(next)
	return summary
}

// GetClaudeQuota returns the normalized per-credential Claude quota view
// (GET /v8/management/quota/claude). Emails are returned in full; the UI masks them.
func (h *Handler) GetClaudeQuota(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	tracker := h.authManager.ClaudeQuotaTracker()
	strategy, rankCfg, interval := h.claudeQuotaSettings()
	if refresher := tracker.Refresher(); refresher != nil {
		interval = refresher.Interval()
	}
	now := tracker.Now()
	entries := h.buildClaudeQuotaEntries(h.claudeQuotaAuths(), tracker, rankCfg, now)
	c.JSON(http.StatusOK, claudeQuotaResponseJSON{
		Provider:               "claude",
		GeneratedAt:            now.UTC(),
		Strategy:               strategy,
		RefreshIntervalSeconds: int64(interval / time.Second),
		Summary:                summarizeClaudeQuota(entries, now),
		Credentials:            entries,
	})
}

// RefreshClaudeQuota re-reads usage now (POST /v8/management/quota/claude/refresh).
// Body {"auth_index": "1"} refreshes one credential; {} refreshes all.
func (h *Handler) RefreshClaudeQuota(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	tracker := h.authManager.ClaudeQuotaTracker()
	refresher := tracker.Refresher()
	if refresher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "claude quota poller is not running"})
		return
	}
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	if c.Request.ContentLength != 0 {
		if errBind := c.ShouldBindJSON(&body); errBind != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
	}
	auths := h.claudeQuotaAuths()
	if index := strings.TrimSpace(body.AuthIndex); index != "" {
		var target *coreauth.Auth
		for _, auth := range auths {
			if lockedAuthIndex(auth) == index {
				target = auth
				break
			}
		}
		if target == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "claude credential not found"})
			return
		}
		errRefresh := refresher.Refresh(c.Request.Context(), target.ID)
		var backoff *coreauth.ClaudeQuotaBackoffError
		if errors.As(errRefresh, &backoff) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "backoff", "retry_after_seconds": int64(math.Ceil(backoff.RetryAfter.Seconds()))})
			return
		}
		auths = []*coreauth.Auth{target}
	} else {
		refreshClaudeQuotaAll(c.Request.Context(), refresher, auths)
	}
	_, rankCfg, _ := h.claudeQuotaSettings()
	// Rank against every credential, then return only the refreshed ones.
	all := h.claudeQuotaAuths()
	entries := h.buildClaudeQuotaEntries(all, tracker, rankCfg, tracker.Now())
	wanted := make(map[string]bool, len(auths))
	for _, auth := range auths {
		wanted[auth.ID] = true
	}
	out := make([]claudeQuotaCredentialJSON, 0, len(auths))
	for _, e := range entries {
		if wanted[e.ID] {
			out = append(out, e)
		}
	}
	c.JSON(http.StatusOK, gin.H{"credentials": out})
}

func refreshClaudeQuotaAll(ctx context.Context, refresher coreauth.ClaudeQuotaRefresher, auths []*coreauth.Auth) {
	var wg sync.WaitGroup
	for _, auth := range auths {
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			// Per-credential failures (backoff, 401) are reported through poll_error.
			_ = refresher.Refresh(ctx, id)
		}(auth.ID)
	}
	wg.Wait()
}
