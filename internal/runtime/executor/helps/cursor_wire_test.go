package helps

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorfixture"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	"google.golang.org/protobuf/proto"
)

func TestConnectFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(encodeConnectFrame(0, []byte("plain")))
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	_, _ = zw.Write([]byte("zipped"))
	_ = zw.Close()
	buf.Write(encodeConnectFrame(connectFlagCompressed, z.Bytes()))
	buf.Write(encodeConnectFrame(connectFlagEndStream, []byte(`{}`)))

	for _, want := range []string{"plain", "zipped"} {
		_, payload, err := readConnectFrame(&buf)
		if err != nil || string(payload) != want {
			t.Fatalf("frame = %q, %v; want %q", payload, err, want)
		}
	}
	flags, payload, err := readConnectFrame(&buf)
	if err != nil || flags&connectFlagEndStream == 0 || !errors.Is(parseConnectEndStream(payload), io.EOF) {
		t.Fatalf("end frame: flags %x payload %q err %v", flags, payload, err)
	}
	if _, _, err = readConnectFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("after last frame: %v", err)
	}
	if _, _, err = readConnectFrame(bytes.NewReader([]byte{0, 0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestConnectErrors(t *testing.T) {
	var se *CursorStatusError
	if err := parseConnectEndStream([]byte(`{"error":{"code":"unauthenticated","message":"bad token"}}`)); !errors.As(err, &se) || se.Code != "unauthenticated" || se.HTTPStatus != 0 {
		t.Fatalf("end-stream error = %v", err)
	}
	if err := cursorUnaryError(401, []byte(`{"code":"unauthenticated","message":"no"}`)); !errors.As(err, &se) || se.HTTPStatus != 401 || se.Code != "unauthenticated" {
		t.Fatalf("unary error = %v", err)
	}
	if err := cursorUnaryError(502, []byte("<html>bad gateway</html>")); !errors.As(err, &se) || se.Code != "Bad Gateway" || !strings.Contains(se.Message, "bad gateway") {
		t.Fatalf("text error = %v", err)
	}
}

// The HTTP transport against a local HTTP/2 server: host discovery, headers,
// a full Run replayed from a recorded fixture, and a rejected Run.
func TestCursorHTTPTransportOverHTTP2(t *testing.T) {
	fx, err := cursorfixture.Load(filepath.Join("cursorfixture", "testdata", "runs", "plain-text.json"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := fx.ServerConnectStream()
	if err != nil {
		t.Fatal(err)
	}
	var configCalls atomic.Int32
	var srvURL string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("proto = %s", r.Proto)
		}
		if r.Header.Get("X-Cursor-Checksum") != "" {
			t.Error("x-cursor-checksum must not be sent")
		}
		if r.Header.Get("X-Cursor-Client-Version") != "cli-test" || r.Header.Get("X-Cursor-Client-Type") != "cli" || r.Header.Get("X-Ghost-Mode") != "true" {
			t.Errorf("headers = %v", r.Header)
		}
		if r.Header.Get("Authorization") != "Bearer tok-"+strings.TrimPrefix(r.URL.Path, "/") && r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"bad token"}`))
			return
		}
		switch r.URL.Path {
		case cursorServerConfigPath:
			configCalls.Add(1)
			out, _ := proto.Marshal(&cursorpb.GetServerConfigResponse{AgentUrlConfig: &cursorpb.AgentUrlConfig{AgentUrl: srvURL + "/"}})
			_, _ = w.Write(out)
		case cursorRunPath:
			if r.Header.Get("Content-Type") != "application/connect+proto" {
				t.Errorf("content-type = %s", r.Header.Get("Content-Type"))
			}
			_, payload, errRead := readConnectFrame(r.Body)
			first := &cursorpb.AgentClientMessage{}
			if errRead != nil || proto.Unmarshal(payload, first) != nil || first.GetRunRequest().GetRequestedModel().GetModelId() != "composer-2.5" {
				t.Errorf("first frame: %v", errRead)
			}
			w.Header().Set("Content-Type", "application/connect+proto")
			_, _ = w.Write(stream)
		default:
			http.NotFound(w, r)
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	srvURL = srv.URL

	token := "good"
	clock := &fakeCursorClock{t: time.Unix(0, 0)}
	tr := &CursorHTTPTransport{
		Client:        srv.Client(),
		Tokens:        CursorTokenFunc(func(context.Context) (string, error) { return token, nil }),
		BackendURL:    srv.URL,
		ClientVersion: func() string { return "cli-test" },
		Now:           clock.now,
	}
	if host := tr.AgentHost(context.Background()); host != srv.URL {
		t.Fatalf("agent host = %q", host)
	}
	tr.AgentHost(context.Background())
	if configCalls.Load() != 1 {
		t.Fatalf("GetServerConfig called %d times within the refresh window", configCalls.Load())
	}

	c := newCursorClient(tr, NewCursorSessionStore(0, 0, nil))
	c.Heartbeat = 0
	conv := &cursorTestConversation{System: cursorTestSystem, Tools: cursorTestTools}
	conv.user("Reply with one short sentence: what is 2+2?")
	a := streamCursor(t, c, "k", "composer-2.5", conv)
	if !strings.Contains(a.text(), "4") || a.StopReason != cursorStopEndTurn {
		t.Fatalf("response = %q %q", a.text(), a.StopReason)
	}

	token = "expired"
	err = c.Stream(context.Background(), CursorStreamRequest{Model: "composer-2.5", Body: conv.body("composer-2.5")}, func(CursorClaudeEvent) error { return nil })
	var se *CursorStatusError
	if !errors.As(err, &se) || se.HTTPStatus != http.StatusUnauthorized || se.Code != "unauthenticated" {
		t.Fatalf("rejected Run error = %v", err)
	}
}

func TestCursorAgentHostFallsBackOnDiscoveryFailure(t *testing.T) {
	tr := &CursorHTTPTransport{Tokens: CursorTokenFunc(func(context.Context) (string, error) { return "", errors.New("no auth") })}
	if host := tr.AgentHost(context.Background()); host != CursorDefaultAgentURL {
		t.Fatalf("host = %q", host)
	}
	if host := (&CursorHTTPTransport{AgentURL: "https://example.test/"}).AgentHost(context.Background()); host != "https://example.test" {
		t.Fatalf("override host = %q", host)
	}
}
