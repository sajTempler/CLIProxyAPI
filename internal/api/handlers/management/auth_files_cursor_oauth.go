package management

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	log "github.com/sirupsen/logrus"
)

// newCursorAuth builds the Cursor auth client; tests replace it to use a fake server.
var newCursorAuth = cursorauth.NewCursorAuth

// RequestCursorToken starts a Cursor browser login (loginDeepControl + auth/poll).
// It returns the login URL immediately and polls in the background, like Meta's
// device flow: the client follows /oauth/status with the returned state.
func (h *Handler) RequestCursorToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	authSvc := newCursorAuth(h.cfg)
	session, errStart := authSvc.StartLogin()
	if errStart != nil {
		log.Errorf("Failed to start Cursor login: %v", errStart)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start cursor login"})
		return
	}

	state := fmt.Sprintf("cursor-%d", time.Now().UnixNano())
	RegisterOAuthSession(state, "cursor")

	go func() {
		pollCtx, cancelPoll := context.WithCancel(ctx)
		defer cancelPoll()
		go watchOAuthSessionCancel(pollCtx, cancelPoll, state, "cursor")

		tokens, errWait := authSvc.WaitForTokens(pollCtx, session)
		if errWait != nil {
			if !IsOAuthSessionPending(state, "cursor") {
				return
			}
			log.Errorf("Cursor authentication failed: %v", errWait)
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Authentication failed", errWait))
			return
		}
		if !IsOAuthSessionPending(state, "cursor") {
			return
		}

		email, errEmail := authSvc.FetchEmail(pollCtx, tokens.AccessToken)
		if errEmail != nil {
			log.Debugf("cursor: email lookup failed: %v", errEmail)
		}
		record, errRecord := cursorauth.BuildAuthRecord(cursorauth.RecordInput{
			Tokens:   tokens,
			Email:    email,
			AuthKind: cursorauth.AuthKindOAuth,
		})
		if errRecord != nil {
			log.Errorf("Cursor authentication failed: %v", errRecord)
			SetOAuthSessionError(state, "Failed to build credential")
			return
		}
		if errGuard := guardOAuthSessionPendingForSave(state, "cursor"); errGuard != nil {
			return
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save Cursor token to file: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}
		CompleteOAuthSession(state)
		log.Infof("Cursor authentication successful; token saved to %s", savedPath)
	}()

	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"url":        session.LoginURL,
		"state":      state,
		"flow":       "device",
		"expires_in": int(cursorauth.MaxPollDuration / time.Second),
	})
}
