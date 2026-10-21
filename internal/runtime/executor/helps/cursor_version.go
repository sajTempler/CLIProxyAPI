package helps

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// CursorVersionRefreshInterval is how often the installed agent CLI version is re-read.
	// Reading a symlink is cheap; the agent auto-updates about weekly.
	CursorVersionRefreshInterval = 10 * time.Minute
	// cursorClientVersionPrefix is what the CLI prepends in x-cursor-client-version.
	cursorClientVersionPrefix = "cli-"
)

// cursorSnapshot is the committed proto snapshot; its agent_version is the
// fallback when no agent CLI is installed (CI, other machines).
//
//go:embed cursorpb/SNAPSHOT.txt
var cursorSnapshot []byte

var cursorAgentVersionPattern = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}-[0-9a-f]+$`)

var (
	cursorVersionMu        sync.RWMutex
	cachedCursorVersion    string
	cursorAgentPath        = defaultCursorAgentPath
	cursorTicker           = newRealCursorTicker
	cursorUpdaterCancel    context.CancelFunc
	cursorVersionRefreshed chan struct{}
)

// DefaultCursorFallbackVersion returns the agent version recorded in cursorpb/SNAPSHOT.txt.
func DefaultCursorFallbackVersion() string {
	sc := bufio.NewScanner(bytes.NewReader(cursorSnapshot))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "agent_version: "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// GetCursorClientVersion returns the agent CLI version (e.g. "2026.10.19-6bbf2c4").
// It is the installed agent's version once the updater has run, else the snapshot version.
func GetCursorClientVersion() string {
	cursorVersionMu.RLock()
	v := cachedCursorVersion
	cursorVersionMu.RUnlock()
	if v != "" {
		return v
	}
	return DefaultCursorFallbackVersion()
}

// CursorClientVersionHeader returns the value for the x-cursor-client-version header.
func CursorClientVersionHeader() string {
	return cursorClientVersionPrefix + GetCursorClientVersion()
}

// StartCursorVersionUpdater reads the installed agent version now and then on
// every CursorVersionRefreshInterval tick. A later call replaces the previous updater.
func StartCursorVersionUpdater(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	cursorVersionMu.Lock()
	if cursorUpdaterCancel != nil {
		cursorUpdaterCancel()
	}
	cursorUpdaterCancel = cancel
	newTicker := cursorTicker
	cursorVersionMu.Unlock()

	ticks, stop := newTicker(CursorVersionRefreshInterval)
	go func() {
		defer stop()
		RefreshCursorClientVersion()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticks:
				RefreshCursorClientVersion()
			}
		}
	}()
}

// RefreshCursorClientVersion re-reads the agent symlink once and returns the current version.
// On failure it keeps the last good version (or the snapshot fallback).
func RefreshCursorClientVersion() string {
	defer notifyCursorVersionRefreshed()
	cursorVersionMu.RLock()
	pathFn := cursorAgentPath
	cursorVersionMu.RUnlock()

	version, errRead := ReadCursorAgentVersion(pathFn())
	if errRead != nil {
		log.WithError(errRead).Debug("cursor agent CLI version not readable, keeping cached/snapshot version")
		return GetCursorClientVersion()
	}
	cursorVersionMu.Lock()
	changed := cachedCursorVersion != version
	cachedCursorVersion = version
	cursorVersionMu.Unlock()
	if changed {
		log.WithField("version", version).Info("cursor agent CLI version detected")
	}
	return version
}

// ReadCursorAgentVersion resolves the agent launcher symlink
// (~/.local/bin/agent -> .../cursor-agent/versions/<ver>/cursor-agent) and returns <ver>.
func ReadCursorAgentVersion(agentPath string) (string, error) {
	if agentPath == "" {
		return "", fmt.Errorf("no agent path")
	}
	target, errEval := filepath.EvalSymlinks(agentPath)
	if errEval != nil {
		return "", fmt.Errorf("resolve %s: %w", agentPath, errEval)
	}
	version := filepath.Base(filepath.Dir(target))
	if filepath.Base(filepath.Dir(filepath.Dir(target))) != "versions" || !cursorAgentVersionPattern.MatchString(version) {
		return "", fmt.Errorf("agent target %s is not under versions/<yyyy.mm.dd-hash>/", target)
	}
	return version, nil
}

func defaultCursorAgentPath() string {
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return ""
	}
	return filepath.Join(home, ".local", "bin", "agent")
}

func newRealCursorTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

func notifyCursorVersionRefreshed() {
	cursorVersionMu.RLock()
	ch := cursorVersionRefreshed
	cursorVersionMu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// SetCursorVersionSourceForTest overrides the agent path, the ticker and the refresh hook,
// and clears the cached version. The returned func restores the previous state.
func SetCursorVersionSourceForTest(agentPath string, ticks <-chan time.Time, refreshed chan struct{}) func() {
	cursorVersionMu.Lock()
	oldPath, oldTicker, oldHook, oldVersion := cursorAgentPath, cursorTicker, cursorVersionRefreshed, cachedCursorVersion
	cursorAgentPath = func() string { return agentPath }
	if ticks != nil {
		cursorTicker = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	}
	cursorVersionRefreshed = refreshed
	cachedCursorVersion = ""
	cursorVersionMu.Unlock()
	return func() {
		cursorVersionMu.Lock()
		if cursorUpdaterCancel != nil {
			cursorUpdaterCancel()
			cursorUpdaterCancel = nil
		}
		cursorAgentPath, cursorTicker, cursorVersionRefreshed, cachedCursorVersion = oldPath, oldTicker, oldHook, oldVersion
		cursorVersionMu.Unlock()
	}
}
