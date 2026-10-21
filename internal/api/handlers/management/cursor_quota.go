package management

import (
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Contract: docs/cursor/USAGE-TRACKING.md section 5.

type cursorQuotaPercentWindowJSON struct {
	Kind             string     `json:"kind"`
	UsedPercent      float64    `json:"used_percent"`
	RemainingPercent float64    `json:"remaining_percent"`
	ResetsAt         *time.Time `json:"resets_at"`
	ObservedAt       *time.Time `json:"observed_at"`
	Source           string     `json:"source"`
}

type cursorQuotaSpendWindowJSON struct {
	Kind         string     `json:"kind"`
	LimitKind    string     `json:"limit_kind"`
	LimitScope   string     `json:"limit_scope"`
	UsedUSD      float64    `json:"used_usd"`
	LimitUSD     *float64   `json:"limit_usd"`
	RemainingUSD *float64   `json:"remaining_usd"`
	UsedPercent  *float64   `json:"used_percent"`
	ResetsAt     *time.Time `json:"resets_at"`
	ObservedAt   *time.Time `json:"observed_at"`
	Source       string     `json:"source"`
}

type cursorQuotaWindowsJSON struct {
	Included     *cursorQuotaPercentWindowJSON `json:"included"`
	IncludedAuto *cursorQuotaPercentWindowJSON `json:"included_auto"`
	IncludedAPI  *cursorQuotaPercentWindowJSON `json:"included_api"`
	OnDemand     *cursorQuotaSpendWindowJSON   `json:"on_demand"`
}

type cursorQuotaPlanJSON struct {
	Label string `json:"label"`
	Price string `json:"price"`
}

type cursorQuotaExtraJSON struct {
	BillingCycleStart *time.Time `json:"billing_cycle_start"`
	IncludedSpendUSD  float64    `json:"included_spend_usd"`
	IncludedLimitUSD  float64    `json:"included_limit_usd"`
	BonusUSD          float64    `json:"bonus_usd"`
	TotalSpendUSD     float64    `json:"total_spend_usd"`
	AutoBucketModels  []string   `json:"auto_bucket_models"`
}

type cursorQuotaCredentialJSON struct {
	AuthIndex     string                 `json:"auth_index"`
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Email         string                 `json:"email"`
	Label         string                 `json:"label"`
	Disabled      bool                   `json:"disabled"`
	Status        string                 `json:"status"`
	Plan          *cursorQuotaPlanJSON   `json:"plan"`
	Windows       cursorQuotaWindowsJSON `json:"windows"`
	Extra         *cursorQuotaExtraJSON  `json:"extra"`
	LastPollAt    *time.Time             `json:"last_poll_at"`
	NextPollAt    *time.Time             `json:"next_poll_at"`
	PollStartedAt *time.Time             `json:"poll_started_at"`
	PollError     string                 `json:"poll_error"`
}

type cursorQuotaSegmentJSON struct {
	AuthIndex        string  `json:"auth_index"`
	RemainingPercent float64 `json:"remaining_percent"`
}

type cursorQuotaSummaryJSON struct {
	Credentials         int                      `json:"credentials"`
	Loaded              int                      `json:"loaded"`
	Window              string                   `json:"window"`
	RemainingPercentSum float64                  `json:"remaining_percent_sum"`
	CapacityPercent     float64                  `json:"capacity_percent"`
	Segments            []cursorQuotaSegmentJSON `json:"segments"`
	OnDemandUsedUSDSum  float64                  `json:"on_demand_used_usd_sum"`
	NextResetAt         *time.Time               `json:"next_reset_at"`
}

type cursorQuotaResponseJSON struct {
	Provider               string                      `json:"provider"`
	GeneratedAt            time.Time                   `json:"generated_at"`
	RefreshIntervalSeconds int64                       `json:"refresh_interval_seconds"`
	Summary                cursorQuotaSummaryJSON      `json:"summary"`
	Credentials            []cursorQuotaCredentialJSON `json:"credentials"`
}

// cursorQuotaAuths returns the `type: "cursor"` credentials, ordered by file name.
func (h *Handler) cursorQuotaAuths() []*coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	var out []*coreauth.Auth
	for _, auth := range h.authManager.List() {
		if coreauth.IsCursorAuth(auth) {
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

func percentWindow(used float64, resetsAt, observedAt *time.Time) *cursorQuotaPercentWindowJSON {
	return &cursorQuotaPercentWindowJSON{
		Kind:             "percent",
		UsedPercent:      used,
		RemainingPercent: remainingPercent(used),
		ResetsAt:         resetsAt,
		ObservedAt:       observedAt,
		Source:           "poll",
	}
}

func spendWindow(od coreauth.CursorOnDemand, resetsAt, observedAt *time.Time) *cursorQuotaSpendWindowJSON {
	w := &cursorQuotaSpendWindowJSON{
		Kind:       "spend",
		LimitKind:  od.LimitKind,
		LimitScope: od.LimitScope,
		UsedUSD:    od.UsedUSD,
		ResetsAt:   resetsAt,
		ObservedAt: observedAt,
		Source:     "poll",
	}
	if od.LimitKind == coreauth.CursorLimitFixed && od.LimitUSD > 0 {
		limit := od.LimitUSD
		remaining := math.Max(0, limit-od.UsedUSD)
		used := od.UsedUSD / limit * 100
		w.LimitUSD, w.RemainingUSD, w.UsedPercent = &limit, &remaining, &used
	}
	return w
}

func buildCursorQuotaEntry(auth *coreauth.Auth, tracker *coreauth.CursorQuotaTracker) cursorQuotaCredentialJSON {
	entry := cursorQuotaCredentialJSON{
		AuthIndex: lockedAuthIndex(auth),
		ID:        auth.ID,
		Name:      claudeQuotaAuthName(auth),
		Email:     authEmail(auth),
		Label:     auth.Label,
		Disabled:  auth.Disabled,
		Status:    string(auth.Status),
	}
	snap, ok := tracker.Snapshot(auth.ID)
	if !ok {
		return entry
	}
	if snap.Plan != nil {
		entry.Plan = &cursorQuotaPlanJSON{Label: snap.Plan.Label, Price: snap.Plan.Price}
	}
	if u := snap.Usage; u != nil {
		resets, observed := timePtr(u.ResetsAt), timePtr(snap.ObservedAt)
		entry.Windows = cursorQuotaWindowsJSON{
			Included:     percentWindow(u.IncludedPercent, resets, observed),
			IncludedAuto: percentWindow(u.AutoPercent, resets, observed),
			IncludedAPI:  percentWindow(u.APIPercent, resets, observed),
			OnDemand:     spendWindow(u.OnDemand, resets, observed),
		}
		models := u.AutoBucketModels
		if models == nil {
			models = []string{}
		}
		entry.Extra = &cursorQuotaExtraJSON{
			BillingCycleStart: timePtr(u.CycleStart),
			IncludedSpendUSD:  u.IncludedSpendUSD,
			IncludedLimitUSD:  u.IncludedLimitUSD,
			BonusUSD:          u.BonusUSD,
			TotalSpendUSD:     u.TotalSpendUSD,
			AutoBucketModels:  models,
		}
	}
	entry.LastPollAt = timePtr(snap.LastPollAt)
	entry.NextPollAt = timePtr(snap.NextPollAt)
	entry.PollStartedAt = timePtr(snap.PollStartedAt)
	entry.PollError = snap.PollError
	return entry
}

func summarizeCursorQuota(entries []cursorQuotaCredentialJSON, now time.Time) cursorQuotaSummaryJSON {
	summary := cursorQuotaSummaryJSON{Credentials: len(entries), Window: "included", Segments: []cursorQuotaSegmentJSON{}}
	var next time.Time
	for _, e := range entries {
		w := e.Windows.Included
		if w == nil {
			continue
		}
		summary.Loaded++
		summary.RemainingPercentSum += w.RemainingPercent
		summary.CapacityPercent += 100
		summary.Segments = append(summary.Segments, cursorQuotaSegmentJSON{AuthIndex: e.AuthIndex, RemainingPercent: w.RemainingPercent})
		if e.Windows.OnDemand != nil {
			summary.OnDemandUsedUSDSum += e.Windows.OnDemand.UsedUSD
		}
		if w.ResetsAt != nil && w.ResetsAt.After(now) && (next.IsZero() || w.ResetsAt.Before(next)) {
			next = *w.ResetsAt
		}
	}
	summary.NextResetAt = timePtr(next)
	return summary
}

func (h *Handler) cursorQuotaInterval(tracker *coreauth.CursorQuotaTracker) time.Duration {
	if refresher := tracker.Refresher(); refresher != nil {
		return refresher.Interval()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return 0
	}
	return h.cfg.Routing.CursorQuota.EffectiveRefreshInterval()
}

// GetCursorQuota returns the per-credential Cursor usage view
// (GET /v8/management/quota/cursor). Emails are returned in full; the UI masks them.
func (h *Handler) GetCursorQuota(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	tracker := h.authManager.CursorQuotaTracker()
	now := tracker.Now()
	auths := h.cursorQuotaAuths()
	entries := make([]cursorQuotaCredentialJSON, 0, len(auths))
	for _, auth := range auths {
		entries = append(entries, buildCursorQuotaEntry(auth, tracker))
	}
	c.JSON(http.StatusOK, cursorQuotaResponseJSON{
		Provider:               "cursor",
		GeneratedAt:            now.UTC(),
		RefreshIntervalSeconds: int64(h.cursorQuotaInterval(tracker) / time.Second),
		Summary:                summarizeCursorQuota(entries, now),
		Credentials:            entries,
	})
}

// RefreshCursorQuota re-reads usage now (POST /v8/management/quota/cursor/refresh).
// Body {"auth_index": "1"} refreshes one credential; {} refreshes all enabled ones.
func (h *Handler) RefreshCursorQuota(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	tracker := h.authManager.CursorQuotaTracker()
	refresher := tracker.Refresher()
	if refresher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cursor quota poller is not running"})
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
	auths := h.cursorQuotaAuths()
	if index := strings.TrimSpace(body.AuthIndex); index != "" {
		var target *coreauth.Auth
		for _, auth := range auths {
			if lockedAuthIndex(auth) == index {
				target = auth
				break
			}
		}
		if target == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "cursor credential not found"})
			return
		}
		errRefresh := refresher.Refresh(c.Request.Context(), target.ID)
		var backoff *coreauth.QuotaBackoffError
		if errors.As(errRefresh, &backoff) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "backoff", "retry_after_seconds": int64(math.Ceil(backoff.RetryAfter.Seconds()))})
			return
		}
		auths = []*coreauth.Auth{target}
	} else {
		refreshQuotaAll(c.Request.Context(), refresher, auths)
	}
	out := make([]cursorQuotaCredentialJSON, 0, len(auths))
	for _, auth := range auths {
		if current, ok := h.authManager.GetByID(auth.ID); ok && current != nil {
			auth = current
		}
		if body.AuthIndex == "" && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
			continue
		}
		out = append(out, buildCursorQuotaEntry(auth, tracker))
	}
	c.JSON(http.StatusOK, gin.H{"credentials": out})
}
