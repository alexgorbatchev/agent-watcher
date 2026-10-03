package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDefaultPiSessionsDir(t *testing.T) {
	t.Run("with PI_SESSIONS_DIR set", func(t *testing.T) {
		t.Setenv("PI_SESSIONS_DIR", "/direct/sessions/dir")
		got := DefaultPiSessionsDir()
		if got != "/direct/sessions/dir" {
			t.Errorf("DefaultPiSessionsDir() = %q, want %q", got, "/direct/sessions/dir")
		}
	})

	t.Run("with PI_CODING_AGENT_DIR set", func(t *testing.T) {
		t.Setenv("PI_CODING_AGENT_DIR", "/custom/pi/dir")
		got := DefaultPiSessionsDir()
		want := filepath.Join("/custom/pi/dir", "sessions")
		if got != want {
			t.Errorf("DefaultPiSessionsDir() = %q, want %q", got, want)
		}
	})

	t.Run("with PI_CODING_AGENT_DIR unset and valid HOME", func(t *testing.T) {
		t.Setenv("PI_CODING_AGENT_DIR", "")
		t.Setenv("PI_SESSIONS_DIR", "")
		t.Setenv("HOME", "/custom/user/home")
		got := DefaultPiSessionsDir()
		want := filepath.Join("/custom/user/home", ".pi", "agent", "sessions")
		if got != want {
			t.Errorf("DefaultPiSessionsDir() = %q, want %q", got, want)
		}
	})
}

func TestEncodePiCWD(t *testing.T) {
	tests := []struct {
		cwd  string
		want string
	}{
		{
			cwd:  "/Users/agorbatchev/development/agent-status",
			want: "--Users-agorbatchev-development-agent-status--",
		},
		{
			cwd:  `C:\Users\dev\project`,
			want: "--C--Users-dev-project--",
		},
		{
			cwd:  "/home/user/workspace/repo",
			want: "--home-user-workspace-repo--",
		},
	}

	for _, tt := range tests {
		t.Run(tt.cwd, func(t *testing.T) {
			got := EncodePiCWD(tt.cwd)
			if got != tt.want {
				t.Fatalf("EncodePiCWD(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
		})
	}
}

func TestReadPiSessionHeader(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("nonexistent file", func(t *testing.T) {
		_, err := ReadPiSessionHeader(filepath.Join(tmpDir, "nonexistent.jsonl"))
		if err == nil {
			t.Errorf("expected error for nonexistent file")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		emptyFile := filepath.Join(tmpDir, "empty.jsonl")
		_ = os.WriteFile(emptyFile, []byte(""), 0600)
		_, err := ReadPiSessionHeader(emptyFile)
		if err == nil {
			t.Errorf("expected error for empty file")
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		invalidFile := filepath.Join(tmpDir, "invalid.jsonl")
		_ = os.WriteFile(invalidFile, []byte("{invalid-json\n"), 0600)
		_, err := ReadPiSessionHeader(invalidFile)
		if err == nil {
			t.Errorf("expected error for invalid json")
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		wrongTypeFile := filepath.Join(tmpDir, "wrong_type.jsonl")
		_ = os.WriteFile(wrongTypeFile, []byte(`{"type":"message","id":"m1"}`+"\n"), 0600)
		_, err := ReadPiSessionHeader(wrongTypeFile)
		if err == nil {
			t.Errorf("expected error for wrong header type")
		}
	})

	t.Run("empty ID", func(t *testing.T) {
		emptyIDFile := filepath.Join(tmpDir, "empty_id.jsonl")
		_ = os.WriteFile(emptyIDFile, []byte(`{"type":"session","id":""}`+"\n"), 0600)
		_, err := ReadPiSessionHeader(emptyIDFile)
		if err == nil {
			t.Errorf("expected error for empty session id")
		}
	})

	t.Run("valid header with numeric timestamp", func(t *testing.T) {
		validFileFloat := filepath.Join(tmpDir, "valid_float.jsonl")
		_ = os.WriteFile(validFileFloat, []byte(`{"type":"session","version":3,"id":"s-float","timestamp":1700000000000.0,"cwd":"/path"}`+"\n"), 0600)
		h, err := ReadPiSessionHeader(validFileFloat)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.ID != "s-float" {
			t.Errorf("expected s-float, got %s", h.ID)
		}

		validFileInt := filepath.Join(tmpDir, "valid_int.jsonl")
		_ = os.WriteFile(validFileInt, []byte(`{"type":"session","version":3,"id":"s-int","timestamp":1700000000000,"cwd":"/path"}`+"\n"), 0600)
		h2, err := ReadPiSessionHeader(validFileInt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h2.ID != "s-int" {
			t.Errorf("expected s-int, got %s", h2.ID)
		}
	})
}

func TestLivePiProcessFinder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	procs, err := LivePiProcessFinder(ctx)
	if err != nil {
		t.Logf("LivePiProcessFinder returned error (acceptable if no permission): %v", err)
	}
	t.Logf("LivePiProcessFinder found %d processes", len(procs))
}

func TestPiScannerDiscoveryAndPairing(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "sessions")

	testCWD := "/Users/test/projects/my-app"
	encodedDir := EncodePiCWD(testCWD)
	projectSessionsDir := filepath.Join(sessionsDir, encodedDir)
	if err := os.MkdirAll(projectSessionsDir, 0755); err != nil {
		t.Fatalf("mkdir error: %v", err)
	}

	// 1. Session with RFC3339Nano timestamp
	now := time.Now().UTC()
	sessionID1 := "01a0b64b-test-session-1"
	sessionHeader1 := PiHeader{
		Type:      "session",
		Version:   3,
		ID:        sessionID1,
		Timestamp: now.Format(time.RFC3339Nano),
		CWD:       testCWD,
	}
	h1Bytes, _ := json.Marshal(sessionHeader1)
	sessionFilePath1 := filepath.Join(projectSessionsDir, fmt.Sprintf("%d_%s.jsonl", now.UnixMilli(), sessionID1))
	_ = os.WriteFile(sessionFilePath1, []byte(string(h1Bytes)+"\n"), 0644)

	// 2. Session with standard RFC3339 timestamp (without nanos)
	sessionID2 := "02b0c75c-test-session-2"
	sessionHeader2 := PiHeader{
		Type:      "session",
		Version:   2,
		ID:        sessionID2,
		Timestamp: now.Format(time.RFC3339),
		CWD:       testCWD,
	}
	h2Bytes, _ := json.Marshal(sessionHeader2)
	sessionFilePath2 := filepath.Join(projectSessionsDir, fmt.Sprintf("%d_%s.jsonl", now.UnixMilli()+1000, sessionID2))
	_ = os.WriteFile(sessionFilePath2, []byte(string(h2Bytes)+"\n"), 0644)

	// 3. Session with numeric timestamp (int64/float64)
	sessionID3 := "03c0d86d-test-session-3"
	sessionFilePath3 := filepath.Join(projectSessionsDir, fmt.Sprintf("%d_%s.jsonl", now.UnixMilli()+2000, sessionID3))
	_ = os.WriteFile(sessionFilePath3, []byte(fmt.Sprintf(`{"type":"session","version":1,"id":%q,"timestamp":%d,"cwd":%q}`+"\n", sessionID3, now.UnixMilli(), testCWD)), 0644)

	// 4. Session with float64 timestamp
	sessionID4 := "04d0e97e-test-session-4"
	sessionFilePath4 := filepath.Join(projectSessionsDir, fmt.Sprintf("%d_%s.jsonl", now.UnixMilli()+3000, sessionID4))
	_ = os.WriteFile(sessionFilePath4, []byte(fmt.Sprintf(`{"type":"session","version":1,"id":%q,"timestamp":%f,"cwd":%q}`+"\n", sessionID4, float64(now.UnixMilli()), testCWD)), 0644)

	// 5. Session with RFC3339 timestamp (without nano)
	sessionID5 := "05e0f08f-test-session-5"
	sessionFilePath5 := filepath.Join(projectSessionsDir, fmt.Sprintf("%d_%s.jsonl", now.UnixMilli()+4000, sessionID5))
	_ = os.WriteFile(sessionFilePath5, []byte(fmt.Sprintf(`{"type":"session","version":1,"id":%q,"timestamp":%q,"cwd":%q}`+"\n", sessionID5, now.Format(time.RFC3339), testCWD)), 0644)

	// 6. Files to be ignored: subdirectories, non-jsonl, invalid headers
	_ = os.MkdirAll(filepath.Join(projectSessionsDir, "ignored_subdir"), 0755)
	_ = os.WriteFile(filepath.Join(projectSessionsDir, "notes.txt"), []byte("text"), 0644)
	_ = os.WriteFile(filepath.Join(projectSessionsDir, "corrupt.jsonl"), []byte("{bad\n"), 0644)
	_ = os.WriteFile(filepath.Join(sessionsDir, "file_in_root.txt"), []byte("ignored"), 0644)

	// Mock process finder with 2 candidate processes for testCWD
	mockFinder := func(ctx context.Context) ([]PiProcessInfo, error) {
		return []PiProcessInfo{
			{
				PID:        1001,
				CWD:        testCWD,
				CreateTime: now.UnixMilli() - 50, // very close to session 1
			},
			{
				PID:        1002,
				CWD:        testCWD,
				CreateTime: now.UnixMilli() + 950, // very close to session 2
			},
			{
				PID:        1003,
				CWD:        "/other/unrelated/cwd",
				CreateTime: now.UnixMilli(),
			},
			{
				PID:        1004,
				CWD:        testCWD,
				CreateTime: now.UnixMilli() + 100000, // far in future (>5s), should not be paired
			},
		}, nil
	}

	scanner := NewPiScanner(sessionsDir, mockFinder)
	sessions, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("scanner.Scan error: %v", err)
	}

	if len(sessions) != 5 {
		t.Fatalf("expected 5 sessions, got %d", len(sessions))
	}

	// Verify pairing
	activeCount := 0
	for _, s := range sessions {
		if s.IsActive {
			activeCount++
			if s.PID != 1001 && s.PID != 1002 {
				t.Errorf("unexpected active PID: %d", s.PID)
			}
		}
	}
	if activeCount != 2 {
		t.Errorf("expected 2 active paired sessions, got %d", activeCount)
	}

	// Test NewPiScanner with empty sessionsDir & nil finder
	defaultPi := NewPiScanner("", nil)
	if defaultPi.processFinder == nil {
		t.Errorf("expected default process finder to be set")
	}

	// Test Scan with empty sessionsDir
	emptyScanner := &PiScanner{sessionsDir: ""}
	emptySessions, err := emptyScanner.Scan(context.Background())
	if err != nil || len(emptySessions) != 0 {
		t.Errorf("expected nil, nil for empty sessionsDir, got %v, %v", emptySessions, err)
	}

	// Test Scan with nonexistent sessionsDir
	nonexistentScanner := NewPiScanner("/nonexistent/pi/dir", nil)
	nonexistentSessions, err := nonexistentScanner.Scan(context.Background())
	if err != nil || len(nonexistentSessions) != 0 {
		t.Errorf("expected nil, nil for nonexistent dir, got %v, %v", nonexistentSessions, err)
	}

	// Test Scan when sessionsDir is a regular file (triggers read error)
	tmpFile := filepath.Join(tmpDir, "file_as_dir")
	_ = os.WriteFile(tmpFile, []byte("not a dir"), 0644)
	errScanner := NewPiScanner(tmpFile, nil)
	_, scanErr := errScanner.Scan(context.Background())
	if scanErr == nil {
		t.Errorf("expected error when sessionsDir is a file")
	}
}

func TestPiScanner_MultipleSessionsAndResumed(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "sessions")

	testCWD := "/Users/test/projects/multi-resumed-app"
	encodedDir := EncodePiCWD(testCWD)
	projectSessionsDir := filepath.Join(sessionsDir, encodedDir)
	if err := os.MkdirAll(projectSessionsDir, 0755); err != nil {
		t.Fatalf("mkdir error: %v", err)
	}

	baseTime := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	threeDaysAgo := baseTime.Add(-72 * time.Hour)

	// 1. Older session in testCWD (lexicographically first filename: 1000_old)
	oldID := "01-older-session"
	oldHeader := PiHeader{
		Type:      "session",
		Version:   3,
		ID:        oldID,
		Timestamp: baseTime.Add(-10 * time.Minute).Format(time.RFC3339Nano),
		CWD:       testCWD,
	}
	oldBytes, _ := json.Marshal(oldHeader)
	oldFile := filepath.Join(projectSessionsDir, fmt.Sprintf("%d_%s.jsonl", baseTime.Add(-10*time.Minute).UnixMilli(), oldID))
	_ = os.WriteFile(oldFile, append(oldBytes, '\n'), 0644)
	_ = os.Chtimes(oldFile, baseTime.Add(-10*time.Minute), baseTime.Add(-10*time.Minute))

	// 2. Newer active session in same testCWD (lexicographically second filename: 2000_new)
	newID := "02-newer-session"
	newHeader := PiHeader{
		Type:      "session",
		Version:   3,
		ID:        newID,
		Timestamp: baseTime.Add(-1 * time.Minute).Format(time.RFC3339Nano),
		CWD:       testCWD,
	}
	newBytes, _ := json.Marshal(newHeader)
	newFile := filepath.Join(projectSessionsDir, fmt.Sprintf("%d_%s.jsonl", baseTime.Add(-1*time.Minute).UnixMilli(), newID))
	_ = os.WriteFile(newFile, append(newBytes, '\n'), 0644)
	_ = os.Chtimes(newFile, baseTime, baseTime)

	// 3. Resumed session in another CWD: header from 3 days ago, but file updated recently (now)
	resumedCWD := "/Users/test/projects/resumed-app"
	resumedEncodedDir := EncodePiCWD(resumedCWD)
	resumedProjectDir := filepath.Join(sessionsDir, resumedEncodedDir)
	_ = os.MkdirAll(resumedProjectDir, 0755)

	resumedID := "03-resumed-session"
	resumedHeader := PiHeader{
		Type:      "session",
		Version:   3,
		ID:        resumedID,
		Timestamp: threeDaysAgo.Format(time.RFC3339Nano),
		CWD:       resumedCWD,
	}
	resumedBytes, _ := json.Marshal(resumedHeader)
	resumedFile := filepath.Join(resumedProjectDir, fmt.Sprintf("%d_%s.jsonl", threeDaysAgo.UnixMilli(), resumedID))
	resumedMsg := fmt.Sprintf(`{"type":"message","timestamp":%q}`+"\n", baseTime.Format(time.RFC3339Nano))
	_ = os.WriteFile(resumedFile, append(append(resumedBytes, '\n'), []byte(resumedMsg)...), 0644)
	_ = os.Chtimes(resumedFile, baseTime, baseTime)

	// Mock process finder:
	// Process 5001 running in testCWD, started at baseTime - 10 minutes
	// Process 5002 running in resumedCWD, started at baseTime
	mockFinder := func(ctx context.Context) ([]PiProcessInfo, error) {
		return []PiProcessInfo{
			{
				PID:        5001,
				CWD:        testCWD,
				CreateTime: baseTime.Add(-10 * time.Minute).UnixMilli(),
			},
			{
				PID:        5002,
				CWD:        resumedCWD,
				CreateTime: baseTime.UnixMilli(),
			},
		}, nil
	}

	scanner := NewPiScanner(sessionsDir, mockFinder)
	sessions, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("scanner.Scan error: %v", err)
	}

	sessionByID := make(map[string]PiSessionDescriptor)
	for _, s := range sessions {
		sessionByID[s.SessionID] = s
	}

	// In testCWD, the newer session (02-newer-session) must be paired with PID 5001, NOT the older one
	newDesc, ok := sessionByID[newID]
	if !ok {
		t.Fatalf("session %s not found", newID)
	}
	if !newDesc.IsActive || newDesc.PID != 5001 {
		t.Errorf("expected newer session %s to be active with PID 5001, got active=%v PID=%d", newID, newDesc.IsActive, newDesc.PID)
	}

	oldDesc, ok := sessionByID[oldID]
	if !ok {
		t.Fatalf("session %s not found", oldID)
	}
	if oldDesc.IsActive {
		t.Errorf("expected older session %s to be inactive, got active=%v PID=%d", oldID, oldDesc.IsActive, oldDesc.PID)
	}

	// Resumed session (03-resumed-session) must be paired with PID 5002 despite header being 3 days old
	resumedDesc, ok := sessionByID[resumedID]
	if !ok {
		t.Fatalf("session %s not found", resumedID)
	}
	if !resumedDesc.IsActive || resumedDesc.PID != 5002 {
		t.Errorf("expected resumed session %s to be active with PID 5002, got active=%v PID=%d", resumedID, resumedDesc.IsActive, resumedDesc.PID)
	}
	if resumedDesc.LastActive != baseTime.UnixMilli() {
		t.Errorf("expected resumed session LastActive %d, got %d", baseTime.UnixMilli(), resumedDesc.LastActive)
	}
}

func TestPiScanner_ResumedSessionWithMarkerRecords(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "sessions")
	testCWD := "/Users/test/projects/my-resumed-pi"
	encodedDir := EncodePiCWD(testCWD)
	projectDir := filepath.Join(sessionsDir, encodedDir)
	_ = os.MkdirAll(projectDir, 0755)

	now := time.Now().Truncate(time.Millisecond)
	threeHoursAgo := now.Add(-3 * time.Hour)
	twoHoursAgo := now.Add(-2 * time.Hour)

	sessionID := "resumed-with-markers"
	header := PiHeader{
		Type:      "session",
		Version:   3,
		ID:        sessionID,
		Timestamp: threeHoursAgo.Format(time.RFC3339Nano),
		CWD:       testCWD,
	}
	headerBytes, _ := json.Marshal(header)
	transcriptPath := filepath.Join(projectDir, fmt.Sprintf("%d_%s.jsonl", threeHoursAgo.UnixMilli(), sessionID))

	// Conversational message at 2 hours ago, followed by trailing marker records written at resume time (now)
	lines := []string{
		string(headerBytes),
		fmt.Sprintf(`{"type":"message","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`, twoHoursAgo.Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"type":"last-prompt","timestamp":%q,"lastPrompt":"test"}`, now.Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"type":"queue-operation","timestamp":%q,"operation":"enqueue"}`, now.Format(time.RFC3339Nano)),
	}
	_ = os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0644)
	_ = os.Chtimes(transcriptPath, now, now)

	mockFinder := func(ctx context.Context) ([]PiProcessInfo, error) {
		return []PiProcessInfo{
			{
				PID:        7777,
				CWD:        testCWD,
				CreateTime: now.UnixMilli(),
			},
		}, nil
	}

	scanner := NewPiScanner(sessionsDir, mockFinder)
	sessions, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("scanner.Scan error: %v", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	s := sessions[0]
	if !s.IsActive || s.PID != 7777 {
		t.Errorf("expected resumed session to pair with PID 7777, got active=%v PID=%d", s.IsActive, s.PID)
	}
	// Crucial: LastActive must be the conversational message timestamp (2 hours ago), NOT the marker timestamp (now)
	if s.LastActive != twoHoursAgo.UnixMilli() {
		t.Errorf("expected LastActive %d (conversational), got %d (marker)", twoHoursAgo.UnixMilli(), s.LastActive)
	}
}

type dummySysFileInfo struct {
	os.FileInfo
}

func (d dummySysFileInfo) Sys() any {
	return "not-syscall-stat"
}

func TestGetFileDevIno(t *testing.T) {
	dev, ino := getFileDevIno(nil)
	if dev != 0 || ino != 0 {
		t.Errorf("expected 0, 0 for nil FileInfo, got %d, %d", dev, ino)
	}

	dev, ino = getFileDevIno(dummySysFileInfo{})
	if dev != 0 || ino != 0 {
		t.Errorf("expected 0, 0 for invalid sys type, got %d, %d", dev, ino)
	}

	tmpFile := filepath.Join(t.TempDir(), "stat_test.txt")
	if err := os.WriteFile(tmpFile, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	info, err := os.Stat(tmpFile)
	if err != nil {
		t.Fatalf("failed to stat temp file: %v", err)
	}
	dev, ino = getFileDevIno(info)
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if ino == 0 {
			t.Errorf("expected non-zero inode on unix, got 0")
		}
	} else {
		if dev != 0 || ino != 0 {
			t.Errorf("expected 0, 0 on non-unix, got %d, %d", dev, ino)
		}
	}
}

func TestPiScanner_CacheUnchangedSessionFiles(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "sessions")
	testCWD := "/Users/test/projects/cached-app"
	projectSessionsDir := filepath.Join(sessionsDir, EncodePiCWD(testCWD))
	if err := os.MkdirAll(projectSessionsDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}

	origID := "session-original-id-12345"
	modID := "session-modified-id-12345" // exact same length (26 chars)
	origTime := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

	origHeader := fmt.Sprintf(`{"type":"session","version":1,"id":%q,"cwd":%q,"timestamp":%q}`+"\n", origID, testCWD, origTime.Format(time.RFC3339))
	sessionFilePath := filepath.Join(projectSessionsDir, fmt.Sprintf("%d_%s.jsonl", origTime.UnixMilli(), origID))
	if err := os.WriteFile(sessionFilePath, []byte(origHeader), 0644); err != nil {
		t.Fatalf("failed to write initial session file: %v", err)
	}
	if err := os.Chtimes(sessionFilePath, origTime, origTime); err != nil {
		t.Fatalf("failed to set mtime: %v", err)
	}

	scanner := NewPiScanner(sessionsDir, nil)

	// 1. First scan - should read file and populate cache
	sessions, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("first scan error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session on first scan, got %d", len(sessions))
	}
	if sessions[0].SessionID != origID {
		t.Fatalf("expected session ID %q, got %q", origID, sessions[0].SessionID)
	}

	scanner.mu.Lock()
	cachedEntry, inCache := scanner.cache[sessionFilePath]
	cacheCount := len(scanner.cache)
	scanner.mu.Unlock()

	if cacheCount != 1 || !inCache {
		t.Fatalf("expected 1 cache entry after first scan, got count=%d, inCache=%v", cacheCount, inCache)
	}
	if cachedEntry.header == nil || cachedEntry.header.ID != origID {
		t.Fatalf("expected cached header ID %q, got %v", origID, cachedEntry.header)
	}

	// 2. Overwrite file content in place keeping exact same byte length and restoring mtime
	modHeader := fmt.Sprintf(`{"type":"session","version":1,"id":%q,"cwd":%q,"timestamp":%q}`+"\n", modID, testCWD, origTime.Format(time.RFC3339))
	if len(modHeader) != len(origHeader) {
		t.Fatalf("length mismatch: orig=%d, mod=%d", len(origHeader), len(modHeader))
	}
	if err := os.WriteFile(sessionFilePath, []byte(modHeader), 0644); err != nil {
		t.Fatalf("failed to overwrite session file: %v", err)
	}
	if err := os.Chtimes(sessionFilePath, origTime, origTime); err != nil {
		t.Fatalf("failed to restore mtime: %v", err)
	}

	// Verify stat is identical
	fi, err := os.Stat(sessionFilePath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if fi.Size() != cachedEntry.size || !fi.ModTime().Equal(origTime) {
		t.Fatalf("stat changed unexpectedly: size=%d (want %d), modTime=%v (want %v)", fi.Size(), cachedEntry.size, fi.ModTime(), origTime)
	}

	// Second scan - file has unchanged size and mtime: MUST reuse cache and NOT re-read
	sessions2, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("second scan error: %v", err)
	}
	if len(sessions2) != 1 {
		t.Fatalf("expected 1 session on second scan, got %d", len(sessions2))
	}
	if sessions2[0].SessionID != origID {
		t.Errorf("expected cached session ID %q across unchanged scans, but got %q (file was re-read!)", origID, sessions2[0].SessionID)
	}

	// 3. Unchanged file with chmod 0000 on Unix is served from cache without reopening
	if runtime.GOOS != "windows" {
		if err := os.Chmod(sessionFilePath, 0000); err != nil {
			t.Fatalf("chmod 0000 failed: %v", err)
		}
		defer func() { _ = os.Chmod(sessionFilePath, 0644) }()

		sessionsChmod, err := scanner.Scan(context.Background())
		if err != nil {
			t.Fatalf("scan with chmod 0000 error: %v", err)
		}
		if len(sessionsChmod) != 1 || sessionsChmod[0].SessionID != origID {
			t.Errorf("expected session from cache with chmod 0000, got len=%d", len(sessionsChmod))
		}

		_ = os.Chmod(sessionFilePath, 0644)
	}

	// 4. Append to file - invalidates cache, file is re-parsed
	appendTime := origTime.Add(1 * time.Hour)
	appendLine := fmt.Sprintf(`{"type":"message","timestamp":%q}`+"\n", appendTime.Format(time.RFC3339Nano))
	f, err := os.OpenFile(sessionFilePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open for append error: %v", err)
	}
	if _, err := f.Write([]byte(appendLine)); err != nil {
		_ = f.Close()
		t.Fatalf("append write error: %v", err)
	}
	_ = f.Close()
	if err := os.Chtimes(sessionFilePath, appendTime, appendTime); err != nil {
		t.Fatalf("failed to update mtime after append: %v", err)
	}

	sessions3, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("third scan error: %v", err)
	}
	if len(sessions3) != 1 {
		t.Fatalf("expected 1 session on third scan, got %d", len(sessions3))
	}
	// After cache invalidation, re-parsed header reflects modID
	if sessions3[0].SessionID != modID {
		t.Errorf("expected re-parsed session ID %q after file append, got %q", modID, sessions3[0].SessionID)
	}

	scanner.mu.Lock()
	cachedEntry3 := scanner.cache[sessionFilePath]
	scanner.mu.Unlock()
	if cachedEntry3.header == nil || cachedEntry3.header.ID != modID {
		t.Errorf("expected cache updated with modID %q, got %v", modID, cachedEntry3.header)
	}

	// 5. Delete file - evicted from cache on next scan
	if err := os.Remove(sessionFilePath); err != nil {
		t.Fatalf("failed to remove session file: %v", err)
	}

	sessions4, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("fourth scan error: %v", err)
	}
	if len(sessions4) != 0 {
		t.Errorf("expected 0 sessions after file deletion, got %d", len(sessions4))
	}

	scanner.mu.Lock()
	_, stillCached := scanner.cache[sessionFilePath]
	remainingCacheLen := len(scanner.cache)
	scanner.mu.Unlock()

	if stillCached || remainingCacheLen != 0 {
		t.Errorf("expected deleted file to be evicted from cache, stillCached=%v, cacheLen=%d", stillCached, remainingCacheLen)
	}

	// 6. Test invalid header in previously cached file causes cache removal
	corruptFile := filepath.Join(projectSessionsDir, "corrupt_test.jsonl")
	validHeader := fmt.Sprintf(`{"type":"session","version":1,"id":"valid-before-corrupt","cwd":%q}`+"\n", testCWD)
	if err := os.WriteFile(corruptFile, []byte(validHeader), 0644); err != nil {
		t.Fatalf("failed to write valid file: %v", err)
	}
	sessionsCorrupt1, err := scanner.Scan(context.Background())
	if err != nil || len(sessionsCorrupt1) != 1 {
		t.Fatalf("expected 1 session before corruption, got %d, err=%v", len(sessionsCorrupt1), err)
	}
	// Corrupt the file content and change mtime
	if err := os.WriteFile(corruptFile, []byte("invalid-json\n"), 0644); err != nil {
		t.Fatalf("failed to corrupt file: %v", err)
	}
	sessionsCorrupt2, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan after corruption failed: %v", err)
	}
	if len(sessionsCorrupt2) != 0 {
		t.Errorf("expected 0 sessions after corruption, got %d", len(sessionsCorrupt2))
	}
	scanner.mu.Lock()
	_, corruptInCache := scanner.cache[corruptFile]
	scanner.mu.Unlock()
	if corruptInCache {
		t.Errorf("expected corrupt file to be evicted from cache")
	}
	_ = os.Remove(corruptFile)

	// 7. Directory removal clears cache entirely
	recreateFile := filepath.Join(projectSessionsDir, "dummy.jsonl")
	_ = os.WriteFile(recreateFile, []byte(validHeader), 0644)
	sessionsPopulated, err := scanner.Scan(context.Background())
	if err != nil || len(sessionsPopulated) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessionsPopulated))
	}
	scanner.mu.Lock()
	populatedLen := len(scanner.cache)
	scanner.mu.Unlock()
	if populatedLen != 1 {
		t.Fatalf("expected cache size 1, got %d", populatedLen)
	}
	// Remove entire sessionsDir
	if err := os.RemoveAll(sessionsDir); err != nil {
		t.Fatalf("failed to remove sessionsDir: %v", err)
	}
	sessionsCleared, err := scanner.Scan(context.Background())
	if err != nil || len(sessionsCleared) != 0 {
		t.Fatalf("expected 0 sessions on missing dir, got %d, err=%v", len(sessionsCleared), err)
	}
	scanner.mu.Lock()
	clearedLen := len(scanner.cache)
	scanner.mu.Unlock()
	if clearedLen != 0 {
		t.Errorf("expected cache to be cleared when sessionsDir does not exist, got len=%d", clearedLen)
	}
}

func TestPiScanner_SkipsProcessFinderWhenNoCandidateSessions(t *testing.T) {
	tmpDir := t.TempDir()
	emptySessionsDir := filepath.Join(tmpDir, "empty_sessions")
	if err := os.MkdirAll(emptySessionsDir, 0755); err != nil {
		t.Fatalf("failed to create empty sessions dir: %v", err)
	}

	finderCalled := 0
	mockFinder := func(ctx context.Context) ([]PiProcessInfo, error) {
		finderCalled++
		return nil, nil
	}

	scanner := NewPiScanner(emptySessionsDir, mockFinder)
	sessions, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("unexpected error from Scan: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
	if finderCalled != 0 {
		t.Errorf("expected process finder NOT to be called when sessions dir has 0 candidate files, but it was called %d times", finderCalled)
	}
}

func TestPiScanner_SetProcessFinderAndProvider(t *testing.T) {
	scanner := NewPiScanner("/dummy", nil)

	customFinder := func(ctx context.Context) ([]PiProcessInfo, error) {
		return []PiProcessInfo{{PID: 123}}, nil
	}
	scanner.SetProcessFinder(customFinder)
	procs, err := scanner.processFinder(context.Background())
	if err != nil || len(procs) != 1 || procs[0].PID != 123 {
		t.Errorf("SetProcessFinder failed: %v, %v", procs, err)
	}

	snapProvider := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
		return &testDummySnapshot{alivePID: 101}, nil
	})
	scanner.SetProcessSnapshotProvider(snapProvider)
	procs, err = scanner.processFinder(context.Background())
	if err != nil || len(procs) != 1 || procs[0].PID != 101 {
		t.Errorf("SetProcessSnapshotProvider failed: %v, %v", procs, err)
	}
}
