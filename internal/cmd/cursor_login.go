package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// DoCursorLogin runs the Cursor browser login (loginDeepControl + auth/poll).
func DoCursorLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}
	runCursorLogin(cfg, &sdkAuth.LoginOptions{NoBrowser: options.NoBrowser, Metadata: map[string]string{}})
}

// DoCursorAPIKeyLogin exchanges a Cursor user API key for a session JWT and stores
// both, so the credential can be refreshed without interaction. The key is read from
// CURSOR_API_KEY, else prompted for, so it never appears in argv.
func DoCursorAPIKeyLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}
	apiKey := strings.TrimSpace(os.Getenv("CURSOR_API_KEY"))
	if apiKey == "" {
		promptFn := options.Prompt
		if promptFn == nil {
			promptFn = defaultProjectPrompt()
		}
		entered, errPrompt := promptFn("Cursor API key (from cursor.com/settings; input is visible, or set CURSOR_API_KEY): ")
		if errPrompt != nil {
			log.Errorf("Cursor API key login failed: %v", errPrompt)
			return
		}
		apiKey = strings.TrimSpace(entered)
	}
	if apiKey == "" {
		log.Error("Cursor API key login failed: no API key given")
		return
	}
	runCursorLogin(cfg, &sdkAuth.LoginOptions{Metadata: map[string]string{sdkAuth.CursorMetadataAPIKey: apiKey}})
}

func runCursorLogin(cfg *config.Config, authOpts *sdkAuth.LoginOptions) {
	record, savedPath, errLogin := newAuthManager().Login(context.Background(), "cursor", cfg, authOpts)
	if errLogin != nil {
		log.Errorf("Cursor authentication failed: %v", errLogin)
		return
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil {
		printCursorRecordSummary(record)
	}
}

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
	printCursorRecordSummary(record)
}

func printCursorRecordSummary(record *coreauth.Auth) {
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
