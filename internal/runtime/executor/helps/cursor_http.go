package helps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

const cursorUsableModelsPath = "/agent.v1.AgentService/GetUsableModels"

// cursorDirectTransport serves Cursor calls without a configured proxy. It
// keeps the environment proxy and the default dial/TLS handshake limits
// (connection setup only); nothing bounds a stream once it is established.
var cursorDirectTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = true
	return t
}()

var cursorProxyTransports = NewTransportCache[string](DefaultTransportCacheCapacity)

// NewCursorHTTPClient returns an HTTP/2 capable client for the Cursor
// backend that honours the request, auth and global proxy settings. A Run is
// a full-duplex Connect stream, so the client sets no timeout (AGENTS.md).
func NewCursorHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) *http.Client {
	proxyURL := effectiveProxyURL(ctx, cfg, auth)
	if proxyURL == "" {
		return &http.Client{Transport: cursorDirectTransport}
	}
	t, err := cursorProxyTransports.Get(proxyURL, func() (*http.Transport, error) {
		base := buildProxyTransport(proxyURL)
		if base == nil {
			return nil, fmt.Errorf("invalid proxy url")
		}
		c := base.Clone()
		c.ForceAttemptHTTP2 = true
		return c, nil
	})
	if err != nil || t == nil {
		log.Warn("cursor: proxy transport unavailable; connecting directly")
		return &http.Client{Transport: cursorDirectTransport}
	}
	return &http.Client{Transport: t}
}

// GetUsableModels lists the models the account may use
// (agent.v1.AgentService/GetUsableModels, Connect unary on the backend host).
func (t *CursorHTTPTransport) GetUsableModels(ctx context.Context) ([]*cursorpb.ModelDetails, error) {
	token, err := t.token(ctx)
	if err != nil {
		return nil, err
	}
	body, err := proto.Marshal(&cursorpb.GetUsableModelsRequest{})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.backend()+cursorUsableModelsPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	t.headers(req.Header, token)
	req.Header.Set("Content-Type", "application/proto")
	resp, err := t.client(ctx).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("cursor: close GetUsableModels body")
		}
	}()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorMaxFrame))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursorUnaryError(resp.StatusCode, raw)
	}
	out := &cursorpb.GetUsableModelsResponse{}
	if err = proto.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("decode GetUsableModels: %w", err)
	}
	return out.GetModels(), nil
}
