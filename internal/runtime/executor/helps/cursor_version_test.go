package helps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeAgentInstall builds <root>/versions/<ver>/cursor-agent and <root>/bin/agent -> it.
func fakeAgentInstall(t *testing.T, root, version string) string {
	t.Helper()
	dir := filepath.Join(root, "versions", version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "cursor-agent")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "agent")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func waitRefreshed(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cursor version refresh")
	}
}

func TestReadCursorAgentVersion(t *testing.T) {
	root := t.TempDir()
	link := fakeAgentInstall(t, root, "2026.10.19-6bbf2c4")
	got, err := ReadCursorAgentVersion(link)
	if err != nil || got != "2026.10.19-6bbf2c4" {
		t.Fatalf("ReadCursorAgentVersion = %q, %v", got, err)
	}

	if _, err = ReadCursorAgentVersion(filepath.Join(root, "missing")); err == nil {
		t.Fatal("expected error for missing agent")
	}

	odd := filepath.Join(root, "odd")
	if err = os.WriteFile(odd, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadCursorAgentVersion(odd); err == nil {
		t.Fatal("expected error for a non-versioned target")
	}
}

func TestCursorVersionFallsBackToSnapshot(t *testing.T) {
	fallback := DefaultCursorFallbackVersion()
	if !cursorAgentVersionPattern.MatchString(fallback) {
		t.Fatalf("snapshot fallback version %q is malformed", fallback)
	}
	restore := SetCursorVersionSourceForTest(filepath.Join(t.TempDir(), "no-agent"), nil, nil)
	defer restore()

	if got := RefreshCursorClientVersion(); got != fallback {
		t.Fatalf("version without agent = %q, want snapshot %q", got, fallback)
	}
	if got := CursorClientVersionHeader(); got != "cli-"+fallback {
		t.Fatalf("header = %q", got)
	}
}

func TestCursorVersionUpdaterFollowsSymlinkOnTick(t *testing.T) {
	root := t.TempDir()
	link := fakeAgentInstall(t, root, "2026.10.19-6bbf2c4")
	ticks := make(chan time.Time)
	refreshed := make(chan struct{}, 1)
	restore := SetCursorVersionSourceForTest(link, ticks, refreshed)
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartCursorVersionUpdater(ctx)
	waitRefreshed(t, refreshed)
	if got := GetCursorClientVersion(); got != "2026.10.19-6bbf2c4" {
		t.Fatalf("initial version = %q", got)
	}

	// The agent auto-updates: the symlink moves to a new version directory.
	fakeAgentInstall(t, root, "2026.10.24-0a1b2c3")
	if got := GetCursorClientVersion(); got != "2026.10.19-6bbf2c4" {
		t.Fatalf("version changed before the tick: %q", got)
	}
	ticks <- time.Time{}
	waitRefreshed(t, refreshed)
	if got := CursorClientVersionHeader(); got != "cli-2026.10.24-0a1b2c3" {
		t.Fatalf("header after tick = %q", got)
	}

	// A broken symlink keeps the last good version instead of reverting to the snapshot.
	if err := os.RemoveAll(filepath.Join(root, "versions")); err != nil {
		t.Fatal(err)
	}
	ticks <- time.Time{}
	waitRefreshed(t, refreshed)
	if got := GetCursorClientVersion(); got != "2026.10.24-0a1b2c3" {
		t.Fatalf("version after broken symlink = %q", got)
	}
}
