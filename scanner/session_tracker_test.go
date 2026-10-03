package scanner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionTracker_SaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sessions.json")

	st, err := NewSessionTracker(filePath)
	if err != nil {
		t.Fatalf("NewSessionTracker failed: %v", err)
	}

	st.Set("sess-1", TrackedSession{
		SessionID:      "sess-1",
		Harness:        "pi",
		PID:            1234,
		CWD:            "/tmp/test",
		HarnessVersion: "0.1.0",
		StartedAt:      1000,
		IsActive:       true,
	})
	st.Set("sess-2", TrackedSession{
		SessionID:      "sess-2",
		Harness:        "claude-code",
		PID:            5678,
		CWD:            "/tmp/test2",
		HarnessVersion: "1.0.0",
		StartedAt:      2000,
		IsActive:       true,
	})

	if err := st.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Reload from file
	st2, err := NewSessionTracker(filePath)
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	all := st2.GetAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(all))
	}
	s1, exists := all["sess-1"]
	if !exists || s1.PID != 1234 || s1.Harness != "pi" {
		t.Errorf("sess-1 mismatch: %+v", s1)
	}

	// Delete
	st2.Delete("sess-1")
	if err := st2.Save(); err != nil {
		t.Fatalf("Save after delete failed: %v", err)
	}

	st3, err := NewSessionTracker(filePath)
	if err != nil {
		t.Fatalf("Reload 3 failed: %v", err)
	}
	if len(st3.GetAll()) != 1 {
		t.Errorf("expected 1 session after delete, got %d", len(st3.GetAll()))
	}
}

func TestSessionTracker_CorruptedFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "corrupted.json")
	_ = os.WriteFile(filePath, []byte("{invalid json"), 0644)

	st, err := NewSessionTracker(filePath)
	if err != nil {
		t.Fatalf("expected graceful fallback on corrupted json, got: %v", err)
	}
	if len(st.GetAll()) != 0 {
		t.Errorf("expected empty sessions on corrupted file")
	}
}

func TestSessionTracker_DefaultPath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/custom/cache")
	p := DefaultSessionStatePath()
	if p != "/custom/cache/agent-watcher/observer-sessions.json" {
		t.Errorf("expected /custom/cache path, got %s", p)
	}

	t.Setenv("XDG_CACHE_HOME", "")
	p2 := DefaultSessionStatePath()
	if p2 == "" {
		t.Errorf("expected non-empty default path")
	}

	// NewSessionTracker with empty path
	tmpDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmpDir)
	st, err := NewSessionTracker("")
	if err != nil {
		t.Fatalf("NewSessionTracker(\"\") failed: %v", err)
	}
	if st == nil {
		t.Fatalf("expected non-nil tracker")
	}
}

func TestSessionTracker_SaveNoOpWhenUnmodified(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sessions.json")

	st, err := NewSessionTracker(filePath)
	if err != nil {
		t.Fatalf("NewSessionTracker failed: %v", err)
	}

	if st.IsDirty() {
		t.Errorf("newly created session tracker should not be dirty")
	}

	sess := TrackedSession{
		SessionID:      "sess-1",
		Harness:        "claude-code",
		PID:            1001,
		CWD:            "/tmp/work",
		HarnessVersion: "1.0.0",
		StartedAt:      1000,
		IsActive:       true,
	}
	st.Set("sess-1", sess)

	if !st.IsDirty() {
		t.Errorf("session tracker should be dirty after Set()")
	}

	if err := st.Save(); err != nil {
		t.Fatalf("initial Save failed: %v", err)
	}

	if st.IsDirty() {
		t.Errorf("session tracker should not be dirty after successful Save()")
	}

	fi1, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}

	// Sleep slightly so that any unexpected file rewrite would change mtime
	time.Sleep(20 * time.Millisecond)

	// Calling Save multiple times without modifying sessions must be a no-op
	for i := 0; i < 3; i++ {
		if err := st.Save(); err != nil {
			t.Fatalf("subsequent Save failed: %v", err)
		}
	}

	fi2, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}

	if fi2.ModTime() != fi1.ModTime() {
		t.Errorf("expected file mtime to remain unchanged on clean Save(), got %v want %v", fi2.ModTime(), fi1.ModTime())
	}

	// Setting identical session must not mark tracker dirty
	time.Sleep(20 * time.Millisecond)
	st.Set("sess-1", sess)
	if err := st.Save(); err != nil {
		t.Fatalf("Save after setting identical session failed: %v", err)
	}
	fi3, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if fi3.ModTime() != fi1.ModTime() {
		t.Errorf("expected file mtime unchanged after setting identical session, got %v want %v", fi3.ModTime(), fi1.ModTime())
	}

	// Verify compact JSON serialization (no indentations)
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading sessions file: %v", err)
	}
	if bytes.Contains(content, []byte("\n  ")) {
		t.Errorf("expected compact JSON without indentation, got:\n%s", string(content))
	}
}

func TestSessionTracker_SaveCleansTempFileOnRenameError(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sessions.json")

	st, err := NewSessionTracker(filePath)
	if err != nil {
		t.Fatalf("NewSessionTracker failed: %v", err)
	}

	st.Set("sess-1", TrackedSession{
		SessionID: "sess-1",
		IsActive:  true,
	})

	// Create a directory at filePath so that os.Rename(tmpFile, filePath) fails with EISDIR
	if err := os.Mkdir(filePath, 0755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	err = st.Save()
	if err == nil {
		t.Fatal("expected Save() to fail when target filePath is a directory")
	}

	// Verify no orphaned .tmp.* files remain in tmpDir
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("found leaked temp file %q in cache directory after rename failure", entry.Name())
		}
	}
}
