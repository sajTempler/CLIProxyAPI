package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Keychain item names used by the agent CLI (`../cli-credentials` in its bundle).
const (
	KeychainAccount             = "cursor-user"
	KeychainServiceAccessToken  = "cursor-access-token"
	KeychainServiceRefreshToken = "cursor-refresh-token"
	KeychainServiceAPIKey       = "cursor-api-key"
)

// ErrAgentLoginNotFound reports that no agent CLI login was found in any store.
var ErrAgentLoginNotFound = errors.New("cursor: no agent CLI login found (run `agent login` first)")

// AgentCredentials is the agent CLI's stored login. Values are secrets: never log them.
type AgentCredentials struct {
	AccessToken  string
	RefreshToken string
	APIKey       string
	// Email comes from the non-secret cli-config.json authInfo, when present.
	Email string
	// Source names where the tokens were read from ("keychain" or a file path).
	Source string
}

// KeychainReader returns the password of a generic keychain item, or "" when absent.
type KeychainReader func(ctx context.Context, service, account string) (string, error)

// AgentImportOptions overrides the stores read by ImportAgentLogin (tests).
type AgentImportOptions struct {
	// Keychain reads keychain items; nil uses /usr/bin/security on macOS and skips elsewhere.
	Keychain KeychainReader
	// AuthFile is the agent CLI's file store; empty uses the platform default.
	AuthFile string
	// ConfigFile is the agent CLI's cli-config.json; empty uses ~/.cursor/cli-config.json.
	ConfigFile string
}

// ImportAgentLogin reads the agent CLI login: the macOS keychain first, then the
// file store used with AGENT_CLI_CREDENTIAL_STORE=file.
func ImportAgentLogin(ctx context.Context, opts AgentImportOptions) (*AgentCredentials, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	keychain := opts.Keychain
	if keychain == nil && runtime.GOOS == "darwin" {
		keychain = securityKeychainReader
	}

	var creds *AgentCredentials
	if keychain != nil {
		access, errAccess := keychain(ctx, KeychainServiceAccessToken, KeychainAccount)
		if errAccess != nil {
			return nil, fmt.Errorf("cursor import: read keychain: %w", errAccess)
		}
		refresh, errRefresh := keychain(ctx, KeychainServiceRefreshToken, KeychainAccount)
		if errRefresh != nil {
			return nil, fmt.Errorf("cursor import: read keychain: %w", errRefresh)
		}
		apiKey, errKey := keychain(ctx, KeychainServiceAPIKey, KeychainAccount)
		if errKey != nil {
			return nil, fmt.Errorf("cursor import: read keychain: %w", errKey)
		}
		if strings.TrimSpace(access) != "" || strings.TrimSpace(apiKey) != "" {
			creds = &AgentCredentials{
				AccessToken:  strings.TrimSpace(access),
				RefreshToken: strings.TrimSpace(refresh),
				APIKey:       strings.TrimSpace(apiKey),
				Source:       "keychain",
			}
		}
	}

	if creds == nil {
		authFile := opts.AuthFile
		if authFile == "" {
			authFile = defaultAgentAuthFile()
		}
		fromFile, errFile := readAgentAuthFile(authFile)
		if errFile != nil {
			return nil, errFile
		}
		creds = fromFile
	}
	if creds == nil {
		return nil, ErrAgentLoginNotFound
	}

	configFile := opts.ConfigFile
	if configFile == "" {
		if home, errHome := os.UserHomeDir(); errHome == nil {
			configFile = filepath.Join(home, ".cursor", "cli-config.json")
		}
	}
	creds.Email = readAgentEmail(configFile)
	return creds, nil
}

// securityKeychainReader shells out to /usr/bin/security. The password goes to a
// pipe, never to a terminal or log. A missing item (exit 44) yields "".
func securityKeychainReader(ctx context.Context, service, account string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-w")
	out, errRun := cmd.Output()
	if errRun != nil {
		var exitErr *exec.ExitError
		if errors.As(errRun, &exitErr) && exitErr.ExitCode() == 44 {
			return "", nil
		}
		return "", fmt.Errorf("security find-generic-password -s %s: %w", service, errRun)
	}
	return strings.TrimSpace(string(out)), nil
}

func defaultAgentAuthFile() string {
	switch runtime.GOOS {
	case "darwin":
		if home, errHome := os.UserHomeDir(); errHome == nil {
			return filepath.Join(home, ".cursor", "auth.json")
		}
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Cursor", "auth.json")
		}
	default:
		if dir, errDir := os.UserConfigDir(); errDir == nil {
			return filepath.Join(dir, "cursor", "auth.json")
		}
	}
	return ""
}

// readAgentAuthFile returns nil, nil when the file does not exist.
func readAgentAuthFile(path string) (*AgentCredentials, error) {
	if path == "" {
		return nil, nil
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cursor import: read %s: %w", path, errRead)
	}
	var file struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		APIKey       string `json:"apiKey"`
	}
	if errJSON := json.Unmarshal(data, &file); errJSON != nil {
		return nil, fmt.Errorf("cursor import: parse %s: %w", path, errJSON)
	}
	if strings.TrimSpace(file.AccessToken) == "" && strings.TrimSpace(file.APIKey) == "" {
		return nil, nil
	}
	return &AgentCredentials{
		AccessToken:  strings.TrimSpace(file.AccessToken),
		RefreshToken: strings.TrimSpace(file.RefreshToken),
		APIKey:       strings.TrimSpace(file.APIKey),
		Source:       path,
	}, nil
}

func readAgentEmail(path string) string {
	if path == "" {
		return ""
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return ""
	}
	var cfg struct {
		AuthInfo struct {
			Email string `json:"email"`
		} `json:"authInfo"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg.AuthInfo.Email)
}

// ImportAgentRecord turns the agent CLI login into a `type: "cursor"` auth record.
// When the store holds only an API key, it is exchanged for a session JWT first.
// When the agent holds an API key next to its JWT, the key is kept so the record
// stays refreshable.
func (a *CursorAuth) ImportAgentRecord(ctx context.Context, opts AgentImportOptions) (*AgentCredentials, *TokenPair, error) {
	creds, errImport := ImportAgentLogin(ctx, opts)
	if errImport != nil {
		return nil, nil, errImport
	}
	if creds.AccessToken != "" {
		return creds, &TokenPair{AccessToken: creds.AccessToken, RefreshToken: creds.RefreshToken}, nil
	}
	tokens, errExchange := a.ExchangeAPIKey(ctx, creds.APIKey)
	if errExchange != nil {
		return nil, nil, errExchange
	}
	return creds, tokens, nil
}
