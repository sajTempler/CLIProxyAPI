package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// CursorMetadataAPIKey is the LoginOptions.Metadata key that selects API key login.
const CursorMetadataAPIKey = "api_key"

// CursorAuthenticator implements the Cursor browser (PKCE poll) and API key logins.
type CursorAuthenticator struct {
	// newAuth builds the auth client; tests replace it to point at a fake server.
	newAuth func(cfg *config.Config) *cursorauth.CursorAuth
}

// NewCursorAuthenticator constructs a Cursor authenticator.
func NewCursorAuthenticator() Authenticator {
	return &CursorAuthenticator{}
}

// Provider returns the provider key for Cursor.
func (*CursorAuthenticator) Provider() string {
	return "cursor"
}

// RefreshLead starts refresh (API key logins) or re-login warnings (browser logins)
// a week before the ~60-day session JWT expires.
func (*CursorAuthenticator) RefreshLead() *time.Duration {
	lead := cursorauth.RefreshLead
	return &lead
}

// Login obtains a Cursor session JWT. With Metadata["api_key"] set it exchanges the
// key (a refreshable credential); otherwise it runs the browser login.
func (a *CursorAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	newAuth := a.newAuth
	if newAuth == nil {
		newAuth = cursorauth.NewCursorAuth
	}
	authSvc := newAuth(cfg)

	var (
		tokens *cursorauth.TokenPair
		apiKey = strings.TrimSpace(opts.Metadata[CursorMetadataAPIKey])
		kind   = cursorauth.AuthKindOAuth
	)
	if apiKey != "" {
		kind = cursorauth.AuthKindAPIKey
		exchanged, errExchange := authSvc.ExchangeAPIKey(ctx, apiKey)
		if errExchange != nil {
			return nil, errExchange
		}
		tokens = exchanged
	} else {
		session, errStart := authSvc.StartLogin()
		if errStart != nil {
			return nil, errStart
		}
		fmt.Printf("\nTo authenticate with Cursor, open:\n%s\n\n", session.LoginURL)
		if !opts.NoBrowser {
			if browser.IsAvailable() {
				if errOpen := browser.OpenURL(session.LoginURL); errOpen != nil {
					log.Warnf("Failed to open browser automatically: %v", errOpen)
				} else {
					fmt.Println("Browser opened automatically.")
				}
			} else {
				log.Warn("No browser available; please open the URL manually")
			}
		}
		fmt.Println("Waiting for Cursor login to complete...")
		polled, errWait := authSvc.WaitForTokens(ctx, session)
		if errWait != nil {
			return nil, errWait
		}
		tokens = polled
	}

	email, errEmail := authSvc.FetchEmail(ctx, tokens.AccessToken)
	if errEmail != nil {
		log.Debugf("cursor: email lookup failed: %v", errEmail)
	}
	record, errRecord := cursorauth.BuildAuthRecord(cursorauth.RecordInput{
		Tokens:   tokens,
		APIKey:   apiKey,
		Email:    email,
		AuthKind: kind,
	})
	if errRecord != nil {
		return nil, errRecord
	}
	fmt.Println("Cursor authentication successful")
	return record, nil
}
