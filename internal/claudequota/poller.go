// Package claudequota polls the Anthropic OAuth usage endpoint for Claude
// subscription credentials and feeds the results into the auth package's
// ClaudeQuotaTracker. Passive response headers remain the primary source; the
// poller covers idle accounts and the Fable window.
package claudequota

import (
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

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultBaseURL is the Anthropic API origin serving the OAuth usage endpoints.
	DefaultBaseURL = "https://api.anthropic.com"
	// DefaultUserAgent matches the Claude Code CLI default fingerprint.
	DefaultUserAgent = "claude-cli/2.1.280 (external, cli)"

	usagePath      = "/api/oauth/usage"
	profilePath    = "/api/oauth/profile"
	oauthBetaValue = "oauth-2025-04-20"

	defaultBackoff  = 5 * time.Minute
	maxBackoff      = time.Hour
	profileInterval = 24 * time.Hour
	jitterFraction  = 0.10
	maxCheckPeriod  = 30 * time.Second
	maxResponseBody = 1 << 20
)

// Sanitized poll errors. They never carry tokens or response bodies.
var (
	ErrUnauthorized  = errors.New("unauthorized")
	ErrTokenExpired  = errors.New("token_expired")
	ErrNoToken       = errors.New("no_access_token")
	ErrNotEligible   = errors.New("not_a_claude_oauth_credential")
	ErrUnknownAuth   = errors.New("unknown_credential")
	ErrRequestFailed = errors.New("request_failed")
	ErrBadResponse   = errors.New("invalid_response")
	errInFlight      = errors.New("poll_in_flight")
)

// AuthSource lists credentials. *coreauth.Manager satisfies it (both return clones).
type AuthSource interface {
	List() []*coreauth.Auth
	GetByID(id string) (*coreauth.Auth, bool)
}

// Options configures a Poller.
type Options struct {
	Tracker *coreauth.ClaudeQuotaTracker
	Auths   AuthSource
	// Client returns the HTTP client for a credential (proxy-aware in production).
	// No client timeout is applied; polls are cancelled through their context.
	Client func(*coreauth.Auth) *http.Client
	// BaseURL defaults to DefaultBaseURL; tests point it at an httptest server.
	BaseURL string
	// UserAgent returns the User-Agent to send; nil or "" uses DefaultUserAgent.
	UserAgent func() string
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
	profileAt    time.Time
	inflight     *pollCall
}

// Poller periodically reads OAuth usage for Claude credentials.
type Poller struct {
	tracker   *coreauth.ClaudeQuotaTracker
	auths     AuthSource
	client    func(*coreauth.Auth) *http.Client
	baseURL   string
	userAgent func() string
	now       func() time.Time
	rand      func() float64

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
		tracker:   opts.Tracker,
		auths:     opts.Auths,
		client:    opts.Client,
		baseURL:   strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		userAgent: opts.UserAgent,
		now:       opts.Now,
		rand:      opts.Rand,
		interval:  opts.Interval,
		accounts:  make(map[string]*accountState),
		wake:      make(chan struct{}, 1),
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

// SetInterval applies a hot-reloaded interval. Scheduled polls are re-planned
// on the next check.
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
	if p.tracker.Refresher() == coreauth.ClaudeQuotaRefresher(p) {
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
	requests := p.tracker.PollRequests()
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
		case id := <-requests:
			p.pollRequested(ctx, id, false)
		}
	}
}

// pollDue starts a poll for every eligible credential that is due. With wait
// the call blocks until those polls finish (tests); the loop never waits so a
// hung poll delays only its own account.
func (p *Poller) pollDue(ctx context.Context, wait bool) {
	if p.Interval() <= 0 || p.auths == nil {
		return
	}
	var wg sync.WaitGroup
	auths := p.auths.List()
	p.pruneAccounts(auths)
	for _, auth := range auths {
		if !p.eligibleForBackground(auth) || !p.due(auth.ID) {
			continue
		}
		p.launch(ctx, auth.ID, &wg)
	}
	if wait {
		wg.Wait()
	}
}

// pollRequested handles a selector RequestPoll: it polls only when the
// account is due, so repeated unknown picks cannot hammer the endpoint.
func (p *Poller) pollRequested(ctx context.Context, authID string, wait bool) {
	if p.Interval() <= 0 || p.auths == nil {
		return
	}
	auth, ok := p.auths.GetByID(authID)
	if !ok || !p.eligibleForBackground(auth) || !p.due(authID) {
		return
	}
	var wg sync.WaitGroup
	p.launch(ctx, authID, &wg)
	if wait {
		wg.Wait()
	}
}

func (p *Poller) launch(ctx context.Context, authID string, wg *sync.WaitGroup) {
	wg.Add(1)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer wg.Done()
		_ = p.poll(ctx, authID, false)
	}()
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

func (p *Poller) eligibleForBackground(auth *coreauth.Auth) bool {
	return auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled && coreauth.IsClaudeOAuthAuth(auth)
}

func (p *Poller) stateLocked(authID string) *accountState {
	st := p.accounts[authID]
	if st == nil {
		st = &accountState{}
		p.accounts[authID] = st
	}
	return st
}

// due reports whether the account should be polled now. Fresh passive data
// for all three windows postpones the poll until that data ages out.
func (p *Poller) due(authID string) bool {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	interval := p.interval
	st := p.stateLocked(authID)
	if st.inflight != nil || st.backoffUntil.After(now) || st.next.After(now) {
		return false
	}
	if freshUntil, fresh := p.passiveFreshUntil(authID, interval, now); fresh {
		st.next = freshUntil
		return false
	}
	return true
}

func (p *Poller) passiveFreshUntil(authID string, interval time.Duration, now time.Time) (time.Time, bool) {
	snap, ok := p.tracker.Snapshot(authID)
	if !ok {
		return time.Time{}, false
	}
	var oldest time.Time
	for _, name := range []coreauth.ClaudeQuotaWindowName{coreauth.ClaudeWindowFiveHour, coreauth.ClaudeWindowSevenDay, coreauth.ClaudeWindowSevenDayFable} {
		w, okWindow := snap.Windows[name]
		if !okWindow || w.Source != coreauth.ClaudeQuotaSourceHeaders || now.Sub(w.ObservedAt) >= interval {
			return time.Time{}, false
		}
		if oldest.IsZero() || w.ObservedAt.Before(oldest) {
			oldest = w.ObservedAt
		}
	}
	return oldest.Add(interval), true
}

// Refresh polls one credential now (manual refresh). Concurrent calls for the
// same credential share one upstream request. During backoff it returns a
// *coreauth.ClaudeQuotaBackoffError without calling upstream.
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
		return &coreauth.ClaudeQuotaBackoffError{RetryAfter: retry}
	}
	call := &pollCall{done: make(chan struct{})}
	st.inflight = call
	p.mu.Unlock()

	call.err = p.doPoll(ctx, authID)

	p.mu.Lock()
	st.inflight = nil
	p.mu.Unlock()
	close(call.done)
	return call.err
}

func (p *Poller) nextAfter(now time.Time) time.Time {
	p.mu.Lock()
	interval := p.interval
	p.mu.Unlock()
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
	if r < 0 {
		r = 0
	}
	if r > 1 {
		r = 1
	}
	return d + time.Duration((2*r-1)*jitterFraction*float64(d))
}

func (p *Poller) finish(authID string, started time.Time, err error, next time.Time) error {
	p.mu.Lock()
	p.stateLocked(authID).next = next
	p.mu.Unlock()
	p.tracker.ObservePollResult(authID, started, p.now(), err, next)
	return err
}

func (p *Poller) doPoll(ctx context.Context, authID string) error {
	started := p.now()
	if p.auths == nil {
		return ErrUnknownAuth
	}
	auth, ok := p.auths.GetByID(authID)
	if !ok || auth == nil {
		return ErrUnknownAuth
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return ErrNotEligible
	}
	if !coreauth.IsClaudeOAuthAuth(auth) {
		return p.finish(authID, started, ErrNoToken, p.nextAfter(started))
	}
	if exp, okExp := auth.AccessTokenExpirationTime(); okExp && !exp.IsZero() && !exp.After(started) {
		return p.finish(authID, started, ErrTokenExpired, p.nextAfter(started))
	}
	token := accessToken(auth)
	p.tracker.ObservePollResult(authID, started, time.Time{}, nil, time.Time{})

	var usage coreauth.ClaudeUsageResponse
	status, header, errGet := p.get(ctx, auth, token, usagePath, &usage)
	now := p.now()
	switch {
	case errGet != nil && status == 0:
		if ctx.Err() != nil {
			return p.finish(authID, started, ctx.Err(), p.nextAfter(now))
		}
		log.WithField("auth_id", authID).Debugf("claude quota poll failed: %v", errGet)
		return p.finish(authID, started, ErrRequestFailed, p.nextAfter(now))
	case status == http.StatusTooManyRequests || status == http.StatusForbidden:
		backoff := retryAfter(header, now)
		p.mu.Lock()
		p.stateLocked(authID).backoffUntil = now.Add(backoff)
		p.mu.Unlock()
		return p.finish(authID, started, &coreauth.ClaudeQuotaBackoffError{Status: status, RetryAfter: backoff}, now.Add(backoff))
	case status == http.StatusUnauthorized:
		return p.finish(authID, started, ErrUnauthorized, p.nextAfter(now))
	case status != http.StatusOK:
		return p.finish(authID, started, fmt.Errorf("http_%d", status), p.nextAfter(now))
	case errGet != nil:
		return p.finish(authID, started, ErrBadResponse, p.nextAfter(now))
	}
	p.tracker.ObserveUsage(authID, usage, now)
	p.maybeFetchProfile(ctx, auth, token, now)
	return p.finish(authID, started, nil, p.nextAfter(now))
}

func (p *Poller) maybeFetchProfile(ctx context.Context, auth *coreauth.Auth, token string, now time.Time) {
	p.mu.Lock()
	st := p.stateLocked(auth.ID)
	due := st.profileAt.IsZero() || now.Sub(st.profileAt) >= profileInterval
	p.mu.Unlock()
	if !due {
		return
	}
	var profile profileResponse
	status, _, errGet := p.get(ctx, auth, token, profilePath, &profile)
	if errGet != nil || status != http.StatusOK {
		log.WithField("auth_id", auth.ID).Debugf("claude profile fetch failed: status=%d", status)
		return
	}
	p.mu.Lock()
	st.profileAt = now
	p.mu.Unlock()
	p.tracker.ObservePlan(auth.ID, profile.plan(now))
}

// get performs an authenticated GET and decodes a 200 body into out.
func (p *Poller) get(ctx context.Context, auth *coreauth.Auth, token, path string, out any) (int, http.Header, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if errReq != nil {
		return 0, nil, errReq
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBetaValue)
	req.Header.Set("Accept", "application/json")
	ua := DefaultUserAgent
	if p.userAgent != nil {
		if v := strings.TrimSpace(p.userAgent()); v != "" {
			ua = v
		}
	}
	req.Header.Set("User-Agent", ua)
	resp, errDo := p.client(auth).Do(req)
	if errDo != nil {
		return 0, nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("claude quota: close response body: %v", errClose)
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
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
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

type profileResponse struct {
	Account struct {
		HasClaudeMax bool `json:"has_claude_max"`
		HasClaudePro bool `json:"has_claude_pro"`
	} `json:"account"`
	Organization struct {
		OrganizationType string `json:"organization_type"`
		RateLimitTier    string `json:"rate_limit_tier"`
	} `json:"organization"`
}

func (r profileResponse) plan(now time.Time) coreauth.ClaudeQuotaPlan {
	return coreauth.ClaudeQuotaPlan{
		Label:            PlanLabel(r.Organization.RateLimitTier, r.Organization.OrganizationType, r.Account.HasClaudeMax, r.Account.HasClaudePro),
		OrganizationType: r.Organization.OrganizationType,
		RateLimitTier:    r.Organization.RateLimitTier,
		FetchedAt:        now,
	}
}

// PlanLabel derives a display label ("Max 5x", "Max 20x", "Max", "Pro", "Team", "Enterprise").
func PlanLabel(rateLimitTier, organizationType string, hasMax, hasPro bool) string {
	tier := strings.ToLower(rateLimitTier)
	switch {
	case strings.Contains(tier, "max_20x"):
		return "Max 20x"
	case strings.Contains(tier, "max_5x"):
		return "Max 5x"
	case hasMax || strings.Contains(tier, "max"):
		return "Max"
	case hasPro || strings.Contains(tier, "pro"):
		return "Pro"
	}
	switch strings.ToLower(strings.TrimSpace(organizationType)) {
	case "claude_team":
		return "Team"
	case "claude_enterprise":
		return "Enterprise"
	}
	return ""
}
