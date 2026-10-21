package helps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	"google.golang.org/protobuf/proto"
)

func TestCursorGetUsableModelsUsesCallCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cursorUsableModelsPath || r.Header.Get("Authorization") != "Bearer call-token" || r.Header.Get("Content-Type") != "application/proto" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.ReadAll(r.Body)
		raw, _ := proto.Marshal(&cursorpb.GetUsableModelsResponse{Models: []*cursorpb.ModelDetails{{ModelId: "composer-2.5", DisplayName: "Composer 2.5"}}})
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	tr := &CursorHTTPTransport{BackendURL: srv.URL, ClientVersion: func() string { return "cli-test" }}
	if _, err := tr.GetUsableModels(context.Background()); err == nil {
		t.Fatal("without bound credentials the call must fail")
	}
	ctx := WithCursorCall(context.Background(), "call-token", srv.Client())
	models, err := tr.GetUsableModels(ctx)
	if err != nil {
		t.Fatalf("GetUsableModels: %v", err)
	}
	if len(models) != 1 || models[0].GetModelId() != "composer-2.5" || models[0].GetDisplayName() != "Composer 2.5" {
		t.Fatalf("models = %v", models)
	}
}

func TestCursorGetUsableModelsMapsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"expired"}`))
	}))
	defer srv.Close()
	tr := &CursorHTTPTransport{BackendURL: srv.URL}
	_, err := tr.GetUsableModels(WithCursorCall(context.Background(), "t", srv.Client()))
	se, ok := err.(*CursorStatusError)
	if !ok || se.HTTPStatus != http.StatusUnauthorized || se.Code != "unauthenticated" {
		t.Fatalf("err = %#v", err)
	}
}

func TestNewCursorHTTPClientIsHTTP2CapableWithoutTimeout(t *testing.T) {
	direct := NewCursorHTTPClient(context.Background(), &config.Config{}, nil)
	if direct.Timeout != 0 {
		t.Fatal("cursor client must not set a timeout")
	}
	if tr, ok := direct.Transport.(*http.Transport); !ok || !tr.ForceAttemptHTTP2 {
		t.Fatalf("direct transport must attempt HTTP/2: %T", direct.Transport)
	}
	proxied := NewCursorHTTPClient(context.Background(), &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://127.0.0.1:3128"}}, nil)
	tr, ok := proxied.Transport.(*http.Transport)
	if !ok || !tr.ForceAttemptHTTP2 || tr.Proxy == nil || proxied.Timeout != 0 {
		t.Fatalf("proxied transport must attempt HTTP/2 through the proxy: %T", proxied.Transport)
	}
}
