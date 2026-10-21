// Package cursorquota polls Cursor's DashboardService for the agent CLI's
// /usage numbers (monthly included %, Auto/API buckets, on-demand spend) per
// `type: "cursor"` credential and stores them in the auth package's
// CursorQuotaTracker. Display only: routing does not read the results.
//
// The poller never refreshes or rewrites tokens; an expired session JWT is
// reported as token_expired and left to the auth layer.
package cursorquota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultBaseURL is the Cursor backend serving aiserver.v1 RPCs.
	DefaultBaseURL = cursorauth.DefaultAPIBaseURL

	defaultBackoff    = 5 * time.Minute
	maxBackoff        = time.Hour
	planInterval      = 24 * time.Hour
	hardLimitInterval = 6 * time.Hour
	jitterFraction    = 0.10
	maxCheckPeriod    = 30 * time.Second
	maxResponseBody   = 1 << 20
)

// Sanitized poll errors. They never carry tokens or response bodies.
var (
	ErrUnauthorized     = errors.New("unauthorized")
	ErrTokenExpired     = errors.New("token_expired")
	ErrNoToken          = errors.New("no_access_token")
	ErrNotEligible      = errors.New("not_a_cursor_credential")
	ErrUnknownAuth      = errors.New("unknown_credential")
	ErrRequestFailed    = errors.New("request_failed")
	ErrBadResponse      = errors.New("invalid_response")
	ErrUsageUnavailable = errors.New("usage_unavailable")
	errInFlight         = errors.New("poll_in_flight")
)

// AuthSource lists credentials. *coreauth.Manager satisfies it (both return clones).
type AuthSource interface {
	List() []*coreauth.Auth
	GetByID(id string) (*coreauth.Auth, bool)
}

// Options configures a Poller.
type Options struct {
	Tracker *coreauth.CursorQuotaTracker
	Auths   AuthSource
	// Client returns the HTTP client for a credential (proxy-aware in production).
	// No client timeout is applied; polls are cancelled through their context.
	Client func(*coreauth.Auth) *http.Client
	// BaseURL defaults to DefaultBaseURL; tests point it at an httptest server.
	BaseURL string
	// ClientVersion returns the x-cursor-client-version value; "" omits the header.
	ClientVersion func() string
	// Interval is the background poll interval; 0 disables background polling.
	Interval time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
	// Rand returns a value in [0,1) for interval jitter; defaults to math/rand/v2.
	Rand func() float64
}

type pollCall struct {
	done chan struct{}
	err  error
}

type accountState struct {
	next         time.Time
	backoffUntil time.Time
	planAt       time.Time
	planCycleEnd time.Time
	hardLimitAt  time.Time
	hardLimit    *cursorauth.HardLimit
	inflight     *pollCall
}

// Poller periodically reads Cursor usage for cursor credentials.
type Poller struct {
	tracker       *coreauth.CursorQuotaTracker
	auths         AuthSource
	client        func(*coreauth.Auth) *http.Client
	baseURL       string
	clientVersion func() string
	now           func() time.Time
	rand          func() float64

	mu       sync.Mutex
	interval time.Duration
	accounts map[string]*accountState
	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	wg       sync.WaitGroup

	// onJoin is a test hook called when a manual refresh joins an in-flight poll.
	onJoin func()
}

// New builds a Poller. Call Start to begin background polling.
func New(opts Options) *Poller {
	p := &Poller{
		tracker:       opts.Tracker,
		auths:         opts.Auths,
		client:        opts.Client,
		baseURL:       strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		clientVersion: opts.ClientVersion,
		now:           opts.Now,
		rand:          opts.Rand,
		interval:      opts.Interval,
		accounts:      make(map[string]*accountState),
		wake:          make(chan struct{}, 1),
	}
	if p.baseURL == "" {
		p.baseURL = DefaultBaseURL
	}
	if p.client == nil {
		p.client = func(*coreauth.Auth) *http.Client { return &http.Client{} }
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.rand == nil {
		p.rand = rand.Float64
	}
	if p.interval < 0 {
		p.interval = 0
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	return p
}

// Interval returns the current background poll interval (0 = off).
func (p *Poller) Interval() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interval
}

// SetInterval applies a hot-reloaded interval; schedules are re-planned.
func (p *Poller) SetInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	p.mu.Lock()
	changed := p.interval != d
	p.interval = d
	if changed {
		for _, st := range p.accounts {
			st.next = time.Time{}
		}
	}
	p.mu.Unlock()
	if changed {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// Start launches the background loop and registers the poller as the
// tracker's on-demand refresher. The loop stops when ctx ends or Stop is called.
func (p *Poller) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	parent := p.ctx
	p.mu.Unlock()
	if parent.Err() != nil {
		return
	}
	stop := context.AfterFunc(ctx, p.cancel)
	p.tracker.SetRefresher(p)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer stop()
		p.loop(parent)
	}()
}

// Stop cancels in-flight polls, stops the loop, and waits for both.
func (p *Poller) Stop() {
	p.cancel()
	if p.tracker.Refresher() == coreauth.CursorQuotaRefresher(p) {
		p.tracker.SetRefresher(nil)
	}
	p.wg.Wait()
}

func (p *Poller) checkPeriod() time.Duration {
	interval := p.Interval()
	if interval <= 0 {
		return maxCheckPeriod
	}
	period := interval / 10
	if period < time.Second {
		period = time.Second
	}
	if period > maxCheckPeriod {
		period = maxCheckPeriod
	}
	return period
}

func (p *Poller) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			p.pollDue(ctx, false)
			timer.Reset(p.checkPeriod())
		case <-p.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(0)
		}
	}
}

// pollDue starts a poll for every eligible credential that is due. With wait
// the call blocks until those polls finish (tests).
func (p *Poller) pollDue(ctx context.Context, wait bool) {
	if p.Interval() <= 0 || p.auths == nil {
		return
	}
	var wg sync.WaitGroup
	auths := p.auths.List()
	p.pruneAccounts(auths)
	for _, auth := range auths {
		if !eligibleForBackground(auth) || !p.due(auth.ID) {
			continue
		}
		wg.Add(1)
		p.wg.Add(1)
		go func(id string) {
			defer p.wg.Done()
			defer wg.Done()
			_ = p.poll(ctx, id, false)
		}(auth.ID)
	}
	if wait {
		wg.Wait()
	}
}

// pruneAccounts drops scheduling state for removed credentials.
func (p *Poller) pruneAccounts(auths []*coreauth.Auth) {
	present := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if auth != nil {
			present[auth.ID] = struct{}{}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, st := range p.accounts {
		if _, ok := present[id]; !ok && st.inflight == nil {
			delete(p.accounts, id)
		}
	}
}

func eligibleForBackground(auth *coreauth.Auth) bool {
	return auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled && coreauth.IsCursorAuth(auth)
}

func (p *Poller) stateLocked(authID string) *accountState {
	st := p.accounts[authID]
	if st == nil {
		st = &accountState{}
		p.accounts[authID] = st
	}
	return st
}

func (p *Poller) due(authID string) bool {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stateLocked(authID)
	return st.inflight == nil && !st.backoffUntil.After(now) && !st.next.After(now)
}

// Refresh polls one credential now (manual refresh). Concurrent calls for the
// same credential share one upstream request. During backoff it returns a
// *coreauth.QuotaBackoffError without calling upstream.
func (p *Poller) Refresh(ctx context.Context, authID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	parent := p.ctx
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(parent, cancel)
	defer stop()
	return p.poll(ctx, strings.TrimSpace(authID), true)
}

func (p *Poller) poll(ctx context.Context, authID string, manual bool) error {
	now := p.now()
	p.mu.Lock()
	st := p.stateLocked(authID)
	if call := st.inflight; call != nil {
		p.mu.Unlock()
		if !manual {
			return errInFlight
		}
		if p.onJoin != nil {
			p.onJoin()
		}
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if st.backoffUntil.After(now) {
		retry := st.backoffUntil.Sub(now)
		p.mu.Unlock()
		return &coreauth.QuotaBackoffError{RetryAfter: retry}
	}
	call := &pollCall{done: make(chan struct{})}
	st.inflight = call
	p.mu.Unlock()

	call.err = p.doPoll(ctx, authID, st)

	p.mu.Lock()
	st.inflight = nil
	p.mu.Unlock()
	close(call.done)
	return call.err
}

func (p *Poller) nextAfter(now time.Time) time.Time {
	interval := p.Interval()
	if interval <= 0 {
		return time.Time{}
	}
	return now.Add(Jitter(interval, p.rand()))
}

// Jitter spreads d by ±10 % using r in [0,1).
func Jitter(d time.Duration, r float64) time.Duration {
	if d <= 0 {
		return d
	}
	r = min(max(r, 0), 1)
	return d + time.Duration((2*r-1)*jitterFraction*float64(d))
}

func (p *Poller) finish(authID string, started time.Time, err error, next time.Time) error {
	p.mu.Lock()
	p.stateLocked(authID).next = next
	p.mu.Unlock()
	p.tracker.ObservePollResult(authID, started, p.now(), err, next)
	return err
}

func (p *Poller) doPoll(ctx context.Context, authID string, st *accountState) error {
	started := p.now()
	if p.auths == nil {
		return ErrUnknownAuth
	}
	auth, ok := p.auths.GetByID(authID)
	if !ok || auth == nil {
		return ErrUnknownAuth
	}
	if !coreauth.IsCursorAuth(auth) {
		return ErrNotEligible
	}
	token := accessToken(auth)
	if token == "" {
		return p.finish(authID, started, ErrNoToken, p.nextAfter(started))
	}
	if exp, okExp := auth.AccessTokenExpirationTime(); okExp && !exp.IsZero() && !exp.After(started) {
		return p.finish(authID, started, ErrTokenExpired, p.nextAfter(started))
	}
	p.tracker.ObservePollResult(authID, started, time.Time{}, nil, time.Time{})

	var usage cursorauth.CurrentPeriodUsage
	status, header, errCall := p.call(ctx, auth, token, cursorauth.GetCurrentPeriodUsagePath, &usage)
	now := p.now()
	switch {
	case errCall != nil && status == 0:
		if ctx.Err() != nil {
			return p.finish(authID, started, ctx.Err(), p.nextAfter(now))
		}
		log.WithField("auth_id", authID).Debugf("cursor quota poll failed: %v", errCall)
		return p.finish(authID, started, ErrRequestFailed, p.nextAfter(now))
	case status == http.StatusTooManyRequests || status == http.StatusForbidden:
		backoff := retryAfter(header, now)
		p.mu.Lock()
		st.backoffUntil = now.Add(backoff)
		p.mu.Unlock()
		// Backoff only delays: the next background poll is never earlier than the regular schedule.
		next := now.Add(backoff)
		if regular := p.nextAfter(now); regular.After(next) {
			next = regular
		}
		return p.finish(authID, started, &coreauth.QuotaBackoffError{Status: status, RetryAfter: backoff}, next)
	case status == http.StatusUnauthorized:
		return p.finish(authID, started, ErrUnauthorized, p.nextAfter(now))
	case status != http.StatusOK:
		return p.finish(authID, started, fmt.Errorf("http_%d", status), p.nextAfter(now))
	case errCall != nil:
		return p.finish(authID, started, ErrBadResponse, p.nextAfter(now))
	}

	planCycleEnd := p.maybeFetchPlan(ctx, auth, token, st, now)
	hardLimit := p.maybeFetchHardLimit(ctx, auth, token, st, usage, now)
	mapped, okUsage := cursorauth.MapUsage(usage, hardLimit, planCycleEnd)
	now = p.now()
	if !okUsage {
		return p.finish(authID, started, ErrUsageUnavailable, p.nextAfter(now))
	}
	p.tracker.ObserveUsage(authID, mapped, now)
	return p.finish(authID, started, nil, p.nextAfter(now))
}

// maybeFetchPlan reads GetPlanInfo on the first poll and then every 24 h.
// Failures are tolerated (the CLI does the same) and retried on the next poll.
func (p *Poller) maybeFetchPlan(ctx context.Context, auth *coreauth.Auth, token string, st *accountState, now time.Time) time.Time {
	p.mu.Lock()
	due := st.planAt.IsZero() || now.Sub(st.planAt) >= planInterval
	cycleEnd := st.planCycleEnd
	p.mu.Unlock()
	if !due {
		return cycleEnd
	}
	var info cursorauth.PlanInfo
	status, _, errCall := p.call(ctx, auth, token, cursorauth.GetPlanInfoPath, &info)
	if errCall != nil || status != http.StatusOK {
		log.WithField("auth_id", auth.ID).Debugf("cursor plan fetch failed: status=%d", status)
		return cycleEnd
	}
	cycleEnd = info.CycleEnd()
	p.mu.Lock()
	st.planAt = now
	st.planCycleEnd = cycleEnd
	p.mu.Unlock()
	p.tracker.ObservePlan(auth.ID, info.Plan(now))
	return cycleEnd
}

// maybeFetchHardLimit reads GetHardLimit every 6 h when the on-demand limit
// depends on it. A failed call keeps the last good value (nil if none).
func (p *Poller) maybeFetchHardLimit(ctx context.Context, auth *coreauth.Auth, token string, st *accountState, usage cursorauth.CurrentPeriodUsage, now time.Time) *cursorauth.HardLimit {
	if !usage.NeedsHardLimit() {
		return nil
	}
	p.mu.Lock()
	due := st.hardLimitAt.IsZero() || now.Sub(st.hardLimitAt) >= hardLimitInterval
	cached := st.hardLimit
	p.mu.Unlock()
	if !due {
		return cached
	}
	var hard cursorauth.HardLimit
	status, _, errCall := p.call(ctx, auth, token, cursorauth.GetHardLimitPath, &hard)
	if errCall != nil || status != http.StatusOK {
		log.WithField("auth_id", auth.ID).Debugf("cursor hard limit fetch failed: status=%d", status)
		return cached
	}
	p.mu.Lock()
	st.hardLimitAt = now
	st.hardLimit = &hard
	p.mu.Unlock()
	return &hard
}

// call performs a Connect unary JSON RPC and decodes a 200 body into out.
func (p *Poller) call(ctx context.Context, auth *coreauth.Auth, token, path string, out any) (int, http.Header, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader([]byte("{}")))
	if errReq != nil {
		return 0, nil, errReq
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("x-cursor-client-type", "cli")
	if p.clientVersion != nil {
		if v := strings.TrimSpace(p.clientVersion()); v != "" {
			req.Header.Set("x-cursor-client-version", v)
		}
	}
	resp, errDo := p.client(auth).Do(req)
	if errDo != nil {
		return 0, nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("cursor quota: close response body: %v", errClose)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return resp.StatusCode, resp.Header, nil
	}
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); errDecode != nil {
		return resp.StatusCode, resp.Header, errDecode
	}
	return resp.StatusCode, resp.Header, nil
}

// retryAfter reads Retry-After (seconds or HTTP date), defaulting to 5 min and capped at 1 h.
func retryAfter(h http.Header, now time.Time) time.Duration {
	d := defaultBackoff
	if raw := strings.TrimSpace(h.Get("Retry-After")); raw != "" {
		if seconds, errParse := strconv.Atoi(raw); errParse == nil && seconds > 0 {
			d = time.Duration(seconds) * time.Second
		} else if at, errDate := http.ParseTime(raw); errDate == nil && at.After(now) {
			d = at.Sub(now)
		}
	}
	return min(d, maxBackoff)
}

func accessToken(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{"access_token", "accessToken"} {
		if v, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
