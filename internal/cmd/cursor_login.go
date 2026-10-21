package cmd

import (
	"context"
	"fmt"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// DoCursorImportAgent imports the login of the installed Cursor agent CLI (macOS
// keychain, else its auth.json) into a `type: "cursor"` auth file. Token values
// are never printed.
func DoCursorImportAgent(cfg *config.Config, _ *LoginOptions) {
	ctx := context.Background()
	authSvc := cursorauth.NewCursorAuth(cfg)
	creds, tokens, errImport := authSvc.ImportAgentRecord(ctx, cursorauth.AgentImportOptions{})
	if errImport != nil {
		log.Errorf("Cursor agent import failed: %v", errImport)
		return
	}
	email := creds.Email
	if email == "" {
		if fetched, errEmail := authSvc.FetchEmail(ctx, tokens.AccessToken); errEmail == nil {
			email = fetched
		} else {
			log.Debugf("cursor: email lookup failed: %v", errEmail)
		}
	}
	record, errRecord := cursorauth.BuildAuthRecord(cursorauth.RecordInput{
		Tokens:   tokens,
		APIKey:   creds.APIKey,
		Email:    email,
		AuthKind: cursorauth.AuthKindAgentImport,
	})
	if errRecord != nil {
		log.Errorf("Cursor agent import failed: %v", errRecord)
		return
	}
	fmt.Printf("Read Cursor agent login from %s\n", creds.Source)
	saveCursorRecord(cfg, record)
}

func saveCursorRecord(cfg *config.Config, record *coreauth.Auth) {
	store := sdkAuth.GetTokenStore()
	if setter, ok := store.(interface{ SetBaseDir(string) }); ok && cfg != nil {
		setter.SetBaseDir(cfg.AuthDir)
	}
	path, errSave := store.Save(context.Background(), record)
	if errSave != nil {
		log.Errorf("Cursor: save credential failed: %v", errSave)
		return
	}
	fmt.Printf("Authentication saved to %s\n", path)
	if record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	if expiry, ok := record.ExpirationTime(); ok {
		fmt.Printf("Session token expires %s\n", expiry.Format("2006-01-02"))
	}
	if apiKey, _ := record.Metadata["api_key"].(string); apiKey == "" {
		fmt.Println("Note: this credential has no API key, so it cannot be refreshed; log in again before it expires.")
	}
}
