package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// Cursor provider (docs/cursor/PLAN.md, Phase 4). The pivot format is
// FormatClaude: a Claude Messages body is the business payload, and the
// Phase 2 protocol client (helps/cursor_*.go) turns it into an
// agent.v1.AgentService/Run and the Run back into Claude stream events.

var _ cliproxyauth.ProviderExecutor = (*CursorExecutor)(nil)

const (
	// cursorRateLimitCooldown is how long a credential cools down after Cursor
	// answers resource_exhausted (it sends no retry hint).
	cursorRateLimitCooldown = time.Minute
	// cursorSweepInterval closes Runs held for tool results that never came.
	cursorSweepInterval = time.Minute
	// cursorRunLogURL labels the upstream call in request logs.
	cursorRunLogURL = "cursor:/agent.v1.AgentService/Run"
)

// Conversation state and held Runs are process-wide: an executor is rebuilt
// on config reloads, and a Run held across a tool round-trip must survive that.
var (
	cursorTransport = &helps.CursorHTTPTransport{}
	cursorSessions  = helps.NewCursorSessionStore(0, 0, nil)
	cursorClient    = helps.NewCursorClient(cursorTransport, cursorSessions)
)

// cursorStreamFunc runs one Claude request on Cursor and emits Claude stream events.
type cursorStreamFunc func(ctx context.Context, req helps.CursorStreamRequest, emit func(helps.CursorClaudeEvent) error) error

// CursorExecutor serves `type: "cursor"` auths through Cursor's agent protocol.
type CursorExecutor struct {
	cfg    *config.Config
	stream cursorStreamFunc // tests replace it
}

// NewCursorExecutor creates a Cursor executor.
func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	return &CursorExecutor{cfg: cfg, stream: cursorClient.Stream}
}

// StartCursorVersionUpdater keeps x-cursor-client-version in step with the
// installed agent CLI.
func StartCursorVersionUpdater(ctx context.Context) {
	helps.StartCursorVersionUpdater(ctx)
}

// StartCursorSessionSweeper drops idle conversation state on a timer, closing
// Runs whose tool_result never arrived.
func StartCursorSessionSweeper(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	cursorSessions.StartSweeper(ctx, cursorSweepInterval)
}

// Identifier returns the provider key.
func (e *CursorExecutor) Identifier() string { return "cursor" }

// RequestToFormat makes the Claude Messages format the pivot.
func (e *CursorExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatClaude
}

func cursorAccessToken(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if auth.Attributes != nil {
		return strings.TrimSpace(auth.Attributes["access_token"])
	}
	return ""
}

// PrepareRequest adds the Cursor bearer token to a raw HTTP request.
func (e *CursorExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if token := cursorAccessToken(auth); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest sends a raw request with Cursor credentials.
func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("cursor executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	return helps.NewCursorHTTPClient(ctx, e.cfg, auth).Do(httpReq)
}

// Refresh delegates to CursorAuth.Refresh. Only API-key logins can be
// refreshed; the others get an error that says how to log in again. The error
// is not a 401, so the manager keeps using the access token until it expires.
func (e *CursorExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("cursor executor: auth is nil")
	}
	updated, err := cursorauth.NewCursorAuth(e.cfg).Refresh(ctx, auth)
	if err == nil {
		return updated, nil
	}
	if errors.Is(err, cursorauth.ErrReloginRequired) {
		kind, _ := auth.Metadata["auth_kind"].(string)
		if kind == cursorauth.AuthKindAgentImport {
			return nil, fmt.Errorf("cursor: a login imported from the agent CLI cannot be refreshed; run `agent login`, then --cursor-import-agent (or use --cursor-login / --cursor-api-key) before it expires: %w", err)
		}
		return nil, fmt.Errorf("cursor: a browser login cannot be refreshed; run --cursor-login (or --cursor-api-key) before it expires: %w", err)
	}
	return nil, fmt.Errorf("cursor refresh: %w", err)
}

// CountTokens estimates the prompt size (Cursor has no count endpoint).
func (e *CursorExecutor) CountTokens(ctx context.Context, _ *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	count := int64(len(req.Payload) / 4)
	raw := []byte(fmt.Sprintf(`{"input_tokens":%d}`, count))
	from := opts.SourceFormat
	if from == "" || from == sdktranslator.FormatClaude {
		return cliproxyexecutor.Response{Payload: raw}, nil
	}
	return cliproxyexecutor.Response{Payload: sdktranslator.TranslateTokenCount(ctx, sdktranslator.FormatClaude, from, count, raw)}, nil
}

// cursorPrepared is one request turned into the final Claude business payload.
type cursorPrepared struct {
	baseModel      string // requested model without thinking suffix, e.g. cursor/composer-2.5
	from           sdktranslator.Format
	responseFormat sdktranslator.Format
	original       []byte
	body           []byte // final Claude body after payload rules
	request        helps.CursorStreamRequest
}

// prepare translates the request to the Claude pivot, applies thinking, then
// applies the user's payload rules last (AGENTS.md): nothing after the
// finalizer changes the business payload; the Cursor client only maps it.
func (e *CursorExecutor) prepare(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*cursorPrepared, error) {
	token := cursorAccessToken(auth)
	if token == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "cursor executor: auth has no access token; run --cursor-login or --cursor-import-agent", credentialScoped: true}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	cursorModel := strings.TrimPrefix(baseModel, registry.CursorModelPrefix)
	if cursorModel == "" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "cursor executor: model is required"}
	}
	from := opts.SourceFormat
	if from == "" {
		from = sdktranslator.FormatClaude
	}
	to := sdktranslator.FormatClaude
	original := req.Payload
	if len(opts.OriginalRequest) > 0 {
		original = opts.OriginalRequest
	}
	originalTranslated, body, err := helps.TranslateRequestPairReturningError(ctx, opts.Headers, e.cfg, from, to, baseModel, original, req.Payload, stream, false)
	if err != nil {
		return nil, err
	}
	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	// User payload rules: the final semantic step.
	body = helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "", originalTranslated, req, opts)(body)

	key, _ := helps.ClaudeCodeExecutionScope(ctx, original, opts.Headers)
	return &cursorPrepared{
		baseModel:      baseModel,
		from:           from,
		responseFormat: cliproxyexecutor.ResponseFormatOrSource(opts),
		original:       original,
		body:           body,
		request: helps.CursorStreamRequest{
			ConversationKey: key,
			Model:           cursorModel,
			ResponseModel:   req.Model,
			Body:            body,
		},
	}, nil
}

// callContext binds the auth's token and proxy-aware HTTP/2 client for the protocol client.
func (e *CursorExecutor) callContext(ctx context.Context, auth *cliproxyauth.Auth) context.Context {
	return helps.WithCursorCall(ctx, cursorAccessToken(auth), helps.NewCursorHTTPClient(ctx, e.cfg, auth))
}

func (e *CursorExecutor) recordRequest(ctx context.Context, auth *cliproxyauth.Auth, p *cursorPrepared) {
	var authID, authLabel string
	if auth != nil {
		authID, authLabel = auth.ID, auth.Label
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       cursorRunLogURL,
		Method:    http.MethodPost,
		Body:      p.body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  "oauth",
	})
}

// Execute serves a non-streaming request by collecting the Run's events.
func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	p, err := e.prepare(ctx, auth, req, opts, false)
	if err != nil {
		return resp, err
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, p.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	e.recordRequest(ctx, auth, p)

	var collected helps.CursorMessageCollector
	errStream := e.stream(e.callContext(ctx, auth), p.request, func(ev helps.CursorClaudeEvent) error {
		helps.AppendAPIResponseChunk(ctx, e.cfg, ev.SSE())
		collected.Add(ev)
		return nil
	})
	if errStream != nil {
		mapped := cursorStatusErr(errStream)
		helps.RecordAPIResponseError(ctx, e.cfg, mapped)
		return resp, mapped
	}
	msg, errMsg := collected.Message()
	if errMsg != nil {
		return resp, statusErr{code: http.StatusBadGateway, msg: "cursor: encode response: " + errMsg.Error()}
	}
	reporter.Publish(ctx, helps.ParseClaudeUsage(msg))
	if p.responseFormat == sdktranslator.FormatClaude {
		return cliproxyexecutor.Response{Payload: msg}, nil
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatClaude, p.responseFormat, req.Model, p.original, p.body, msg, &param)
	return cliproxyexecutor.Response{Payload: out}, nil
}

// ExecuteStream serves a streaming request. An error before the first event
// is returned as an HTTP status; a later one ends the stream with an error
// chunk, which the Claude handler writes as a Claude `error` event.
func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	p, err := e.prepare(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, p.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	e.recordRequest(ctx, auth, p)

	out := make(chan cliproxyexecutor.StreamChunk)
	started := make(chan struct{})
	finished := make(chan error, 1)
	var usageBuf helps.StreamUsageBuffer
	var param any
	send := func(payload []byte) error {
		select {
		case out <- cliproxyexecutor.StreamChunk{Payload: payload}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	emitted := false
	emit := func(ev helps.CursorClaudeEvent) error {
		if !emitted {
			emitted = true
			close(started)
		}
		sse := ev.SSE()
		helps.AppendAPIResponseChunk(ctx, e.cfg, sse)
		usageBuf.ObserveClaudeStream(append([]byte("data: "), ev.Data...))
		if p.responseFormat == sdktranslator.FormatClaude {
			return send(sse)
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(sse), "\n"), "\n") {
			for _, chunk := range sdktranslator.TranslateStream(ctx, sdktranslator.FormatClaude, p.responseFormat, req.Model, p.original, p.body, []byte(line), &param) {
				if errSend := send(chunk); errSend != nil {
					return errSend
				}
			}
		}
		return nil
	}

	go func() {
		errStream := e.stream(e.callContext(ctx, auth), p.request, emit)
		if !emitted {
			finished <- errStream
			close(out)
			return
		}
		defer close(out)
		if errStream == nil {
			usageBuf.Publish(ctx, reporter)
			return
		}
		mapped := cursorStatusErr(errStream)
		helps.RecordAPIResponseError(ctx, e.cfg, mapped)
		usageBuf.PublishFailure(ctx, reporter, mapped)
		if ctx.Err() != nil {
			return
		}
		select {
		case out <- cliproxyexecutor.StreamChunk{Err: mapped}:
		case <-ctx.Done():
		}
	}()

	select {
	case <-started:
		return &cliproxyexecutor.StreamResult{Chunks: out}, nil
	case errStream := <-finished:
		if errStream == nil {
			errStream = statusErr{code: http.StatusBadGateway, msg: "cursor: run ended without output"}
		}
		mapped := cursorStatusErr(errStream)
		helps.RecordAPIResponseError(ctx, e.cfg, mapped)
		return nil, mapped
	}
}

// cursorStatusErr maps protocol client errors to HTTP statuses the auth
// manager understands: rejected credentials are 401, resource_exhausted is a
// 429 with a cooldown. Context errors pass through unchanged.
func cursorStatusErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var already statusErr
	if errors.As(err, &already) {
		return err
	}
	var reqErr *helps.CursorRequestError
	if errors.As(err, &reqErr) {
		return statusErr{code: http.StatusBadRequest, msg: reqErr.Error()}
	}
	var se *helps.CursorStatusError
	if !errors.As(err, &se) {
		return statusErr{code: http.StatusBadGateway, msg: err.Error()}
	}
	msg := se.Error()
	code := strings.ToLower(se.Code)
	switch {
	case code == "unauthenticated" || se.HTTPStatus == http.StatusUnauthorized:
		return statusErr{code: http.StatusUnauthorized, msg: msg, credentialScoped: true}
	case code == "resource_exhausted" || se.HTTPStatus == http.StatusTooManyRequests:
		cooldown := cursorRateLimitCooldown
		return statusErr{code: http.StatusTooManyRequests, msg: msg, retryAfter: &cooldown, credentialScoped: true}
	case code == "permission_denied" || se.HTTPStatus == http.StatusForbidden:
		return statusErr{code: http.StatusForbidden, msg: msg, credentialScoped: true}
	case code == "invalid_argument" || code == "failed_precondition":
		return statusErr{code: http.StatusBadRequest, msg: msg}
	case code == "not_found":
		return statusErr{code: http.StatusNotFound, msg: msg}
	case code == "unavailable":
		return statusErr{code: http.StatusServiceUnavailable, msg: msg}
	case code == "deadline_exceeded":
		return statusErr{code: http.StatusGatewayTimeout, msg: msg}
	case se.HTTPStatus >= 400:
		return statusErr{code: se.HTTPStatus, msg: msg}
	default:
		return statusErr{code: http.StatusBadGateway, msg: msg}
	}
}

// FetchCursorModels lists the models auth may use (AgentService/GetUsableModels).
func FetchCursorModels(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) ([]*registry.ModelInfo, error) {
	token := cursorAccessToken(auth)
	if token == "" {
		return nil, fmt.Errorf("cursor: auth has no access token")
	}
	callCtx := helps.WithCursorCall(ctx, token, helps.NewCursorHTTPClient(ctx, cfg, auth))
	details, err := cursorTransport.GetUsableModels(callCtx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(details))
	models := make([]*registry.ModelInfo, 0, len(details))
	for _, d := range details {
		id := strings.TrimSpace(d.GetModelId())
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, registry.NewCursorModelInfo(id, d.GetDisplayName()))
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("cursor: GetUsableModels returned no models")
	}
	log.WithField("models", len(models)).Debug("cursor: fetched usable models")
	return models, nil
}
