package auth

import (
	"context"
	"sort"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Ranking states reported by RankClaudeQuota.
const (
	ClaudeQuotaRankEligible       = "eligible"
	ClaudeQuotaRankUnknown        = "unknown"
	ClaudeQuotaRankSkipped5h      = "skipped_5h"
	ClaudeQuotaRankSkipped7d      = "skipped_7d"
	ClaudeQuotaRankSkipped7dFable = "skipped_7d_fable"
)

// Order key sources reported by RankClaudeQuota.
const (
	ClaudeQuotaKeyFable = "seven_day_fable"
	ClaudeQuotaKeyWeek  = "seven_day"
	ClaudeQuotaKeyFresh = "fresh"
)

const (
	defaultSoonestResetSkipFiveHourAt = 100.0
	soonestResetTieWindow             = time.Minute
	claudeQuotaFreshWeek              = 7 * 24 * time.Hour
)

// SoonestResetConfig tunes the soonest-reset selector.
type SoonestResetConfig struct {
	// SkipFiveHourAtPercent is the 5-hour used percent at which an account is skipped. Default 100.
	SkipFiveHourAtPercent float64
}

func (c SoonestResetConfig) skipFiveHourAt() float64 {
	if c.SkipFiveHourAtPercent <= 0 {
		return defaultSoonestResetSkipFiveHourAt
	}
	return c.SkipFiveHourAtPercent
}

// ClaudeQuotaRank is one candidate's place in the soonest-reset order. It is
// shared by the selector and the management endpoint (dry-run view).
type ClaudeQuotaRank struct {
	AuthID        string
	State         string    // eligible | unknown | skipped_5h | skipped_7d | skipped_7d_fable
	WeeklyResetAt time.Time // order key actually used (eligible), or the unblock time (skipped)
	KeySource     string    // seven_day_fable | seven_day | fresh ("" for unknown)

	auth            *Auth
	weeklyRemaining float64
	fiveHourUsed    float64
}

// isClaudeOAuthAuth reports whether auth is a Claude credential backed by an OAuth token.
func isClaudeOAuthAuth(auth *Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") && authAccessToken(auth) != ""
}

// IsClaudeOAuthAuth reports whether auth is a Claude credential backed by an OAuth token.
func IsClaudeOAuthAuth(auth *Auth) bool { return isClaudeOAuthAuth(auth) }

func isFableRouteModel(model string) bool {
	return strings.Contains(strings.ToLower(canonicalModelKey(model)), "fable")
}

// windowBlocks reports whether a window blocks new sessions at now.
func windowBlocks(w ClaudeQuotaWindow, ok bool, threshold float64, now time.Time) bool {
	if !ok || !w.ResetsAt.After(now) {
		return false
	}
	return w.Status == "rejected" || w.UsedPercent >= threshold
}

// RankClaudeQuota orders candidates for soonest-reset selection:
// eligible known accounts by weekly reset (soonest first), then accounts
// without quota data by ID, then skipped accounts by the time they unblock.
func RankClaudeQuota(auths []*Auth, model string, t *ClaudeQuotaTracker, cfg SoonestResetConfig, now time.Time) []ClaudeQuotaRank {
	threshold := cfg.skipFiveHourAt()
	fableRoute := isFableRouteModel(model)
	var eligible, unknown, skipped []ClaudeQuotaRank
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		rank := ClaudeQuotaRank{AuthID: auth.ID, auth: auth, State: ClaudeQuotaRankUnknown}
		var snap ClaudeQuotaSnapshot
		known := false
		if isClaudeOAuthAuth(auth) {
			snap, known = t.snapshotAt(auth.ID, now)
			known = known && len(snap.Windows) > 0
		}
		if !known {
			unknown = append(unknown, rank)
			continue
		}
		five, okFive := snap.Windows[ClaudeWindowFiveHour]
		week, okWeek := snap.Windows[ClaudeWindowSevenDay]
		fable, okFable := snap.Windows[ClaudeWindowSevenDayFable]

		var unblock time.Time
		block := func(state string, w ClaudeQuotaWindow) {
			if rank.State == ClaudeQuotaRankUnknown {
				rank.State = state
			}
			if w.ResetsAt.After(unblock) {
				unblock = w.ResetsAt
			}
		}
		if windowBlocks(five, okFive, threshold, now) {
			block(ClaudeQuotaRankSkipped5h, five)
		}
		if windowBlocks(week, okWeek, 100, now) {
			block(ClaudeQuotaRankSkipped7d, week)
		}
		if fableRoute && windowBlocks(fable, okFable, 100, now) {
			block(ClaudeQuotaRankSkipped7dFable, fable)
		}
		if rank.State != ClaudeQuotaRankUnknown {
			rank.WeeklyResetAt = unblock
			skipped = append(skipped, rank)
			continue
		}

		rank.State = ClaudeQuotaRankEligible
		rank.fiveHourUsed = five.UsedPercent
		switch {
		case okFable && fable.ResetsAt.After(now):
			rank.WeeklyResetAt, rank.KeySource = fable.ResetsAt, ClaudeQuotaKeyFable
			rank.weeklyRemaining = 100 - fable.UsedPercent
		case okWeek && week.ResetsAt.After(now):
			rank.WeeklyResetAt, rank.KeySource = week.ResetsAt, ClaudeQuotaKeyWeek
			rank.weeklyRemaining = 100 - week.UsedPercent
		default:
			// A fresh window starts on first use, so it resets last.
			rank.WeeklyResetAt, rank.KeySource = now.Add(claudeQuotaFreshWeek), ClaudeQuotaKeyFresh
			rank.weeklyRemaining = 100
			if okWeek {
				rank.weeklyRemaining = 100 - week.UsedPercent
			}
		}
		eligible = append(eligible, rank)
	}

	sortEligibleBySoonestReset(eligible)
	sort.SliceStable(unknown, func(i, j int) bool { return unknown[i].AuthID < unknown[j].AuthID })
	sort.SliceStable(skipped, func(i, j int) bool {
		a, b := skipped[i], skipped[j]
		if !a.WeeklyResetAt.Equal(b.WeeklyResetAt) {
			return a.WeeklyResetAt.Before(b.WeeklyResetAt)
		}
		return a.AuthID < b.AuthID
	})

	out := make([]ClaudeQuotaRank, 0, len(eligible)+len(unknown)+len(skipped))
	out = append(out, eligible...)
	out = append(out, unknown...)
	return append(out, skipped...)
}

// sortEligibleBySoonestReset orders eligible ranks by weekly reset, soonest
// first. Resets within soonestResetTieWindow of a group's soonest reset tie and
// are broken by more weekly remaining, less 5h used, then auth ID. Grouping by
// an anchor keeps the order transitive: a pairwise "within 1 min" comparator
// would let chained near-ties depend on input order.
func sortEligibleBySoonestReset(eligible []ClaudeQuotaRank) {
	sort.SliceStable(eligible, func(i, j int) bool {
		if !eligible[i].WeeklyResetAt.Equal(eligible[j].WeeklyResetAt) {
			return eligible[i].WeeklyResetAt.Before(eligible[j].WeeklyResetAt)
		}
		return eligible[i].AuthID < eligible[j].AuthID
	})
	for start := 0; start < len(eligible); {
		anchor := eligible[start].WeeklyResetAt
		end := start + 1
		for end < len(eligible) && eligible[end].WeeklyResetAt.Sub(anchor) <= soonestResetTieWindow {
			end++
		}
		group := eligible[start:end]
		sort.SliceStable(group, func(i, j int) bool {
			a, b := group[i], group[j]
			if a.weeklyRemaining != b.weeklyRemaining {
				return a.weeklyRemaining > b.weeklyRemaining
			}
			if a.fiveHourUsed != b.fiveHourUsed {
				return a.fiveHourUsed < b.fiveHourUsed
			}
			return a.AuthID < b.AuthID
		})
		start = end
	}
}

// SoonestResetSelector picks the Claude account whose weekly window resets
// soonest, skipping accounts at their 5-hour (or weekly) limit. It is a
// custom selector: the scheduler fast path is bypassed and session affinity
// wraps it, so the order applies only to new or failed-over sessions.
type SoonestResetSelector struct {
	tracker *ClaudeQuotaTracker
	cfg     SoonestResetConfig
	now     func() time.Time // nil -> tracker clock (time.Now without a tracker)
}

// NewSoonestResetSelector builds a soonest-reset selector over the tracker.
func NewSoonestResetSelector(t *ClaudeQuotaTracker, cfg SoonestResetConfig) *SoonestResetSelector {
	return &SoonestResetSelector{tracker: t, cfg: cfg}
}

func (s *SoonestResetSelector) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	if s.tracker != nil {
		return s.tracker.Now()
	}
	return time.Now()
}

// Pick implements Selector.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.clock()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	ranks := RankClaudeQuota(available, model, s.tracker, s.cfg, now)
	if len(ranks) == 0 {
		return available[0], nil
	}
	first := ranks[0]
	if first.State == ClaudeQuotaRankUnknown && isClaudeOAuthAuth(first.auth) {
		s.tracker.RequestPoll(first.AuthID)
	}
	return first.auth, nil
}
