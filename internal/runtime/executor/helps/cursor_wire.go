package helps

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/proto"
)

// Cursor agent protocol transport: Connect streaming over HTTP/2 to
// agent.v1.AgentService/Run, plus host discovery through
// aiserver.v1.ServerConfigService/GetServerConfig. See docs/cursor/SPIKE.md.

const (
	// CursorBackendURL serves aiserver.v1.* (GetServerConfig) and auth.
	CursorBackendURL = "https://api2.cursor.sh"
	// CursorDefaultAgentURL is used when GetServerConfig fails or returns no agent URL.
	CursorDefaultAgentURL = "https://agentn.global.api5.cursor.sh"

	cursorRunPath          = "/agent.v1.AgentService/Run"
	cursorServerConfigPath = "/aiserver.v1.ServerConfigService/GetServerConfig"
	// cursorHostRefresh is how long a discovered agent host is reused.
	cursorHostRefresh = time.Hour
	// cursorMaxFrame bounds a single Connect frame (checkpoints and blobs can be large).
	cursorMaxFrame = 64 << 20

	connectFlagCompressed = 0x01
	connectFlagEndStream  = 0x02
)

// CursorTokenSource supplies the Cursor access token. The protocol layer never
// reads credentials itself; the executor (Phase 4) passes its auth through this.
type CursorTokenSource interface {
	CursorAccessToken(ctx context.Context) (string, error)
}

// CursorTokenFunc adapts a function to CursorTokenSource.
type CursorTokenFunc func(ctx context.Context) (string, error)

// CursorAccessToken implements CursorTokenSource.
func (f CursorTokenFunc) CursorAccessToken(ctx context.Context) (string, error) { return f(ctx) }

// CursorFrameObserver sees every message on a Run (cursorfixture.Recorder satisfies it).
type CursorFrameObserver interface {
	Client(m *cursorpb.AgentClientMessage) error
	Server(m *cursorpb.AgentServerMessage) error
	EndStream(raw []byte)
}

// CursorStatusError is a non-200 HTTP response or a Connect end-stream error.
type CursorStatusError struct {
	HTTPStatus int    // 0 for an end-stream error on a 200 stream
	Code       string // Connect code ("unauthenticated", "resource_exhausted", ...)
	Message    string
}

func (e *CursorStatusError) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("cursor: http %d %s: %s", e.HTTPStatus, e.Code, e.Message)
	}
	return fmt.Sprintf("cursor: %s: %s", e.Code, e.Message)
}

// cursorRunConn is one bidi Run stream. Send is safe for concurrent use.
// No deadline is ever set on it (AGENTS.md: no timeouts after connect): a
// held Run waits as long as Claude Code's tool runs; liveness comes from
// client heartbeats (cursor_run.go) and the server's own heartbeats, and an
// abandoned Run is closed when its session expires (cursor_session.go).
type cursorRunConn interface {
	Send(m *cursorpb.AgentClientMessage) error
	// Recv returns the next server message, io.EOF on a clean end, or a *CursorStatusError.
	Recv() (*cursorpb.AgentServerMessage, error)
	// CloseSend half-closes the request side.
	CloseSend() error
	// Close aborts the stream.
	Close()
}

// cursorTransport opens Run streams; tests replace it with a fixture replayer.
type cursorTransport interface {
	OpenRun(ctx context.Context, first *cursorpb.AgentClientMessage) (cursorRunConn, error)
}

// CursorHTTPTransport speaks Connect over HTTP/2 to the Cursor agent host.
type CursorHTTPTransport struct {
	// Client must support HTTP/2 full duplex. Nil uses a plain http2.Transport.
	// No client timeout may be set (AGENTS.md: no timeouts after connect).
	Client *http.Client
	Tokens CursorTokenSource
	// BackendURL defaults to CursorBackendURL; AgentURL skips discovery when set.
	BackendURL string
	AgentURL   string
	// ClientVersion returns the x-cursor-client-version value; defaults to CursorClientVersionHeader.
	ClientVersion func() string
	Now           func() time.Time

	mu         sync.Mutex
	discovered string
	discAt     time.Time
}

func (t *CursorHTTPTransport) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return defaultCursorHTTPClient
}

var defaultCursorHTTPClient = &http.Client{Transport: &http2.Transport{}}

func (t *CursorHTTPTransport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *CursorHTTPTransport) backend() string {
	if t.BackendURL != "" {
		return strings.TrimRight(t.BackendURL, "/")
	}
	return CursorBackendURL
}

func (t *CursorHTTPTransport) headers(h http.Header, token string) {
	version := CursorClientVersionHeader()
	if t.ClientVersion != nil {
		version = t.ClientVersion()
	}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Connect-Protocol-Version", "1")
	h.Set("User-Agent", "connect-es/1.6.1")
	h.Set("X-Cursor-Client-Type", "cli")
	h.Set("X-Cursor-Client-Version", version)
	// Ghost (privacy) mode: Cursor must not retain the conversation for training.
	h.Set("X-Ghost-Mode", "true")
	h.Set("X-Request-Id", uuid.NewString())
}

// AgentHost returns the Run host from GetServerConfig, cached for an hour.
// Discovery failures fall back to CursorDefaultAgentURL and are retried next call.
func (t *CursorHTTPTransport) AgentHost(ctx context.Context) string {
	if t.AgentURL != "" {
		return strings.TrimRight(t.AgentURL, "/")
	}
	t.mu.Lock()
	if t.discovered != "" && t.now().Sub(t.discAt) < cursorHostRefresh {
		host := t.discovered
		t.mu.Unlock()
		return host
	}
	t.mu.Unlock()

	host, err := t.discoverAgentHost(ctx)
	if err != nil {
		log.WithError(err).Warn("cursor: GetServerConfig failed, using default agent host")
		return CursorDefaultAgentURL
	}
	t.mu.Lock()
	t.discovered, t.discAt = host, t.now()
	t.mu.Unlock()
	return host
}

func (t *CursorHTTPTransport) discoverAgentHost(ctx context.Context) (string, error) {
	token, err := t.token(ctx)
	if err != nil {
		return "", err
	}
	body, err := proto.Marshal(&cursorpb.GetServerConfigRequest{})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.backend()+cursorServerConfigPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	t.headers(req.Header, token)
	req.Header.Set("Content-Type", "application/proto")
	resp, err := t.client().Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("cursor: close GetServerConfig body")
		}
	}()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorMaxFrame))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", cursorUnaryError(resp.StatusCode, raw)
	}
	out := &cursorpb.GetServerConfigResponse{}
	if err = proto.Unmarshal(raw, out); err != nil {
		return "", fmt.Errorf("decode GetServerConfig: %w", err)
	}
	return cursorAgentURLFromConfig(out.GetAgentUrlConfig())
}

// cursorAgentURLFromConfig mirrors the CLI: ghost/privacy mode uses agent_url,
// otherwise agentn_url. We always send x-ghost-mode, so agent_url wins.
func cursorAgentURLFromConfig(c *cursorpb.AgentUrlConfig) (string, error) {
	for _, u := range []string{c.GetAgentUrl(), c.GetAgentnUrl()} {
		if strings.HasPrefix(u, "https://") {
			return strings.TrimRight(u, "/"), nil
		}
	}
	return "", errors.New("GetServerConfig has no agent url")
}

func (t *CursorHTTPTransport) token(ctx context.Context) (string, error) {
	if t.Tokens == nil {
		return "", errors.New("cursor: no token source")
	}
	token, err := t.Tokens.CursorAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("cursor token: %w", err)
	}
	if token == "" {
		return "", errors.New("cursor token: empty")
	}
	return token, nil
}

// OpenRun starts a Run stream and sends first. It returns once response
// headers arrive; a non-200 status becomes a *CursorStatusError.
func (t *CursorHTTPTransport) OpenRun(ctx context.Context, first *cursorpb.AgentClientMessage) (cursorRunConn, error) {
	token, err := t.token(ctx)
	if err != nil {
		return nil, err
	}
	host := t.AgentHost(ctx)
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+cursorRunPath, pr)
	if err != nil {
		return nil, err
	}
	t.headers(req.Header, token)
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Accept-Encoding", "gzip")
	req.Header.Set("X-Cursor-Streaming", "true")

	c := &cursorHTTPConn{pw: pw}
	// The body pipe blocks until the transport reads it, so the first frame
	// is written concurrently with Do, which returns at response headers.
	sent := make(chan error, 1)
	go func() { sent <- c.Send(first) }()
	resp, err := t.client().Do(req)
	if err != nil {
		_ = pw.CloseWithError(err)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		_ = pw.CloseWithError(errors.New("run rejected"))
		return nil, cursorUnaryError(resp.StatusCode, raw)
	}
	if err = <-sent; err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	c.body = resp.Body
	c.rd = bufio.NewReaderSize(resp.Body, 64<<10)
	return c, nil
}

type cursorHTTPConn struct {
	wmu    sync.Mutex
	pw     *io.PipeWriter
	closed bool
	body   io.ReadCloser
	rd     *bufio.Reader
}

func (c *cursorHTTPConn) Send(m *cursorpb.AgentClientMessage) error {
	payload, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return io.ErrClosedPipe
	}
	_, err = c.pw.Write(encodeConnectFrame(0, payload))
	return err
}

func (c *cursorHTTPConn) Recv() (*cursorpb.AgentServerMessage, error) {
	flags, payload, err := readConnectFrame(c.rd)
	if err != nil {
		return nil, err
	}
	if flags&connectFlagEndStream != 0 {
		return nil, parseConnectEndStream(payload)
	}
	m := &cursorpb.AgentServerMessage{}
	if err = proto.Unmarshal(payload, m); err != nil {
		return nil, fmt.Errorf("cursor: decode server message: %w", err)
	}
	return m, nil
}

func (c *cursorHTTPConn) CloseSend() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.pw.Close()
}

func (c *cursorHTTPConn) Close() {
	_ = c.CloseSend()
	if c.body != nil {
		_ = c.body.Close()
	}
}

func encodeConnectFrame(flags byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// readConnectFrame reads one Connect envelope and gunzips it if flagged.
func readConnectFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, fmt.Errorf("cursor: truncated frame header: %w", err)
		}
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > cursorMaxFrame {
		return 0, nil, fmt.Errorf("cursor: frame of %d bytes exceeds limit", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("cursor: truncated frame: %w", err)
	}
	if hdr[0]&connectFlagCompressed != 0 {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return 0, nil, fmt.Errorf("cursor: gzip frame: %w", err)
		}
		payload, err = io.ReadAll(io.LimitReader(zr, cursorMaxFrame))
		if err != nil {
			return 0, nil, fmt.Errorf("cursor: gunzip frame: %w", err)
		}
	}
	return hdr[0], payload, nil
}

// parseConnectEndStream turns the end-stream JSON into io.EOF or a *CursorStatusError.
func parseConnectEndStream(raw []byte) error {
	var end struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return io.EOF
	}
	if err := json.Unmarshal(raw, &end); err != nil {
		return fmt.Errorf("cursor: bad end-stream frame: %w", err)
	}
	if end.Error == nil {
		return io.EOF
	}
	return &CursorStatusError{Code: end.Error.Code, Message: end.Error.Message}
}

// cursorUnaryError maps a non-200 Connect response (JSON error body) to *CursorStatusError.
func cursorUnaryError(status int, raw []byte) error {
	e := &CursorStatusError{HTTPStatus: status}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil && (body.Code != "" || body.Message != "") {
		e.Code, e.Message = body.Code, body.Message
	} else {
		e.Message = strings.TrimSpace(string(raw))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300]
		}
	}
	if e.Code == "" {
		e.Code = http.StatusText(status)
	}
	return e
}
