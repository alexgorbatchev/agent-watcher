package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

func TestDefaultClaudeSessionDirs(t *testing.T) {
	t.Run("with XDG_DATA_HOME set", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/custom/xdg/data")
		dirs := DefaultClaudeSessionDirs()
		if len(dirs) < 2 {
			t.Fatalf("expected at least 2 dirs, got %d", len(dirs))
		}
		wantXDG := filepath.Join("/custom/xdg/data", "ai-registry", "claude-code", "sessions")
		if dirs[0] != wantXDG {
			t.Errorf("dirs[0] = %q, want %q", dirs[0], wantXDG)
		}
	})

	t.Run("with XDG_DATA_HOME unset", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		dirs := DefaultClaudeSessionDirs()
		if len(dirs) < 2 {
			t.Fatalf("expected at least 2 dirs, got %d", len(dirs))
		}
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("cannot get user home dir")
		}
		wantShare := filepath.Join(home, ".local", "share", "ai-registry", "claude-code", "sessions")
		if dirs[0] != wantShare {
			t.Errorf("dirs[0] = %q, want %q", dirs[0], wantShare)
		}
	})
}

func TestLocateTranscript(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	projectsBase := filepath.Join(tmpDir, "ai-registry", "claude-code", "projects")
	sessionID := "sess-slug-1234"

	// 1. Exact slug matching
	cwd := "/Users/dev/my-project"
	slug := "-Users-dev-my-project"
	slugDir := filepath.Join(projectsBase, slug)
	if err := os.MkdirAll(slugDir, 0755); err != nil {
		t.Fatal(err)
	}

	exactPath := filepath.Join(slugDir, sessionID+".jsonl")
	if err := os.WriteFile(exactPath, []byte(`{"type":"session"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if eval, err := filepath.EvalSymlinks(exactPath); err == nil {
		exactPath = eval
	}

	found := locateTranscript(sessionID, cwd, "")
	if found != exactPath {
		t.Errorf("locateTranscript exact slug = %q, want %q", found, exactPath)
	}

	// 1b. Exact slug without leading dash
	slugNoDash := "Users-dev-my-project-nodash"
	slugNoDashDir := filepath.Join(projectsBase, slugNoDash)
	_ = os.MkdirAll(slugNoDashDir, 0755)
	noDashPath := filepath.Join(slugNoDashDir, "sess-nodash.jsonl")
	_ = os.WriteFile(noDashPath, []byte(`{"type":"session"}`+"\n"), 0600)
	if eval, err := filepath.EvalSymlinks(noDashPath); err == nil {
		noDashPath = eval
	}
	foundNoDash := locateTranscript("sess-nodash", "/Users/dev/my-project-nodash", "")
	if foundNoDash != noDashPath {
		t.Errorf("locateTranscript exact slug no dash = %q, want %q", foundNoDash, noDashPath)
	}

	// 2. Nested project scanning (when cwd is unknown or different slug)
	nestedSessionID := "sess-nested-5678"
	otherDir := filepath.Join(projectsBase, "other-project-folder")
	if err := os.MkdirAll(otherDir, 0755); err != nil {
		t.Fatal(err)
	}
	nestedPath := filepath.Join(otherDir, nestedSessionID+".jsonl")
	if err := os.WriteFile(nestedPath, []byte(`{"type":"session"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if eval, err := filepath.EvalSymlinks(nestedPath); err == nil {
		nestedPath = eval
	}

	foundNested := locateTranscript(nestedSessionID, "/some/nonexistent/cwd", "")
	if foundNested != nestedPath {
		t.Errorf("locateTranscript nested search = %q, want %q", foundNested, nestedPath)
	}

	// 3. Fallback when not found anywhere
	notFound := locateTranscript("nonexistent-session-id", "/nowhere", "")
	if notFound != "" {
		t.Errorf("locateTranscript nonexistent = %q, want empty string", notFound)
	}

	t.Run("with CLAUDE_PROJECTS_DIR set", func(t *testing.T) {
		customProjDir := filepath.Join(tmpDir, "custom_projects")
		_ = os.MkdirAll(filepath.Join(customProjDir, "my-proj"), 0755)
		customTransPath := filepath.Join(customProjDir, "my-proj", "custom-sess.jsonl")
		_ = os.WriteFile(customTransPath, []byte("{}\n"), 0600)
		t.Setenv("CLAUDE_PROJECTS_DIR", customProjDir)

		expectedPath, err := filepath.EvalSymlinks(customTransPath)
		if err != nil {
			expectedPath = customTransPath
		}

		found := locateTranscript("custom-sess", "", "")
		if found != expectedPath && found != customTransPath {
			t.Errorf("locateTranscript with CLAUDE_PROJECTS_DIR = %q, want %q", found, expectedPath)
		}
	})

	t.Run("with CLAUDE_SESSIONS_DIR set", func(t *testing.T) {
		t.Setenv("CLAUDE_SESSIONS_DIR", "/custom/claude/sessions")
		dirs := DefaultClaudeSessionDirs()
		if len(dirs) == 0 || dirs[0] != "/custom/claude/sessions" {
			t.Errorf("expected /custom/claude/sessions first in dirs, got %v", dirs)
		}
	})
}

func TestClaudeCodeScannerEdgeCases(t *testing.T) {
	tmpDir := t.TempDir()
	realSessionsDir := filepath.Join(tmpDir, "real_sessions")
	if err := os.MkdirAll(realSessionsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create symlink to sessions dir
	symlinkSessionsDir := filepath.Join(tmpDir, "symlink_sessions")
	if err := os.Symlink(realSessionsDir, symlinkSessionsDir); err != nil {
		t.Fatal(err)
	}

	currentPID := os.Getpid()
	now := time.Now().UnixMilli()

	// 1. Valid session with StartedAt drift within tolerance
	p, err := process.NewProcess(int32(currentPID))
	var procCreateTime int64
	if err == nil {
		procCreateTime, _ = p.CreateTime()
	}
	if procCreateTime == 0 {
		procCreateTime = now
	}

	validDesc := SessionDescriptor{
		PID:            currentPID,
		SessionID:      "sess-valid",
		CWD:            tmpDir,
		Status:         "active",
		TranscriptPath: filepath.Join(tmpDir, "preset-transcript.jsonl"),
		StartedAt:      procCreateTime,
	}
	vBytes, _ := json.Marshal(validDesc)
	_ = os.WriteFile(filepath.Join(realSessionsDir, "valid.json"), vBytes, 0600)

	// Zero / negative PID session
	zeroDesc := SessionDescriptor{
		PID:       0,
		SessionID: "sess-zero",
	}
	zBytes, _ := json.Marshal(zeroDesc)
	_ = os.WriteFile(filepath.Join(realSessionsDir, "zero.json"), zBytes, 0600)

	// Duplicate PID session (should be skipped after valid.json)
	dupDesc := SessionDescriptor{
		PID:       currentPID,
		SessionID: "sess-dup",
		StartedAt: procCreateTime,
	}
	dupBytes, _ := json.Marshal(dupDesc)
	_ = os.WriteFile(filepath.Join(realSessionsDir, "z_dup.json"), dupBytes, 0600)

	// 2. Dead PID session
	deadDesc := SessionDescriptor{
		PID:       99999999,
		SessionID: "sess-dead",
		StartedAt: now,
	}
	dBytes, _ := json.Marshal(deadDesc)
	_ = os.WriteFile(filepath.Join(realSessionsDir, "dead.json"), dBytes, 0600)

	// 3. Proc drift > 60s
	driftDesc := SessionDescriptor{
		PID:       currentPID,
		SessionID: "sess-drift",
		StartedAt: procCreateTime + 120000, // 2 minutes drift
	}
	drBytes, _ := json.Marshal(driftDesc)
	_ = os.WriteFile(filepath.Join(realSessionsDir, "drift.json"), drBytes, 0600)

	// 4. Invalid json and ignored extensions
	_ = os.WriteFile(filepath.Join(realSessionsDir, "corrupt.json"), []byte("{not-json"), 0600)
	_ = os.WriteFile(filepath.Join(realSessionsDir, "auth.key"), []byte("secret"), 0600)
	_ = os.WriteFile(filepath.Join(realSessionsDir, "notes.txt"), []byte("hello"), 0600)

	// 5. Subdirectory inside sessions
	_ = os.MkdirAll(filepath.Join(realSessionsDir, "sub_folder"), 0755)

	// Scan through the symlinked directory
	scanner := NewClaudeCodeScanner([]string{symlinkSessionsDir, "/nonexistent-dir-to-skip"})
	sessions, err := scanner.Scan()
	if err != nil {
		t.Fatalf("scanner.Scan() error: %v", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("expected 1 valid session, got %d", len(sessions))
	}
	if sessions[0].SessionID != "sess-valid" {
		t.Errorf("sessions[0].SessionID = %q, want 'sess-valid'", sessions[0].SessionID)
	}

	// Test Scan with empty TranscriptPath to exercise locateTranscript invocation
	emptyTranscriptDir := filepath.Join(tmpDir, "empty_transcript_sessions")
	_ = os.MkdirAll(emptyTranscriptDir, 0755)
	emptyTransDesc := SessionDescriptor{
		PID:            currentPID,
		SessionID:      "sess-empty-trans",
		CWD:            tmpDir,
		Status:         "active",
		TranscriptPath: "",
		StartedAt:      procCreateTime,
	}
	etBytes, _ := json.Marshal(emptyTransDesc)
	_ = os.WriteFile(filepath.Join(emptyTranscriptDir, "et.json"), etBytes, 0600)
	etScanner := NewClaudeCodeScanner([]string{emptyTranscriptDir})
	etSessions, err := etScanner.Scan()
	if err != nil || len(etSessions) != 1 {
		t.Fatalf("expected 1 session from etScanner, got %d, err %v", len(etSessions), err)
	}

	// Test NewClaudeCodeScanner with nil/empty dirs
	defaultScanner := NewClaudeCodeScanner(nil)
	if len(defaultScanner.sessionDirs) == 0 {
		t.Errorf("expected default session dirs to be populated")
	}

	// Test DiscoverSubagents with empty TranscriptPath
	emptySubagents, err := scanner.DiscoverSubagents(SessionDescriptor{})
	if err != nil || len(emptySubagents) != 0 {
		t.Errorf("expected nil, nil for empty TranscriptPath, got %v, %v", emptySubagents, err)
	}

	// Test DiscoverSubagents with alternate candidate path (dir/subagents)
	altDir := filepath.Join(tmpDir, "alt_project")
	_ = os.MkdirAll(filepath.Join(altDir, "subagents"), 0755)
	_ = os.WriteFile(filepath.Join(altDir, "subagents", "sub-1.jsonl"), []byte("{}\n"), 0600)
	_ = os.WriteFile(filepath.Join(altDir, "subagents", "ignored.txt"), []byte("txt\n"), 0600)
	_ = os.MkdirAll(filepath.Join(altDir, "subagents", "nested_dir"), 0755)

	altDesc := SessionDescriptor{
		SessionID:      "alt-sess",
		TranscriptPath: filepath.Join(altDir, "alt-sess.jsonl"),
	}
	altSubagents, err := scanner.DiscoverSubagents(altDesc)
	if err != nil {
		t.Fatalf("DiscoverSubagents error: %v", err)
	}
	if len(altSubagents) != 1 || altSubagents[0].SubagentID != "sub-1" {
		t.Errorf("expected 1 subagent 'sub-1', got %+v", altSubagents)
	}
}

func TestProcessHelpers(t *testing.T) {
	currentPID := os.Getpid()

	t.Run("IsProcessAlive", func(t *testing.T) {
		if !IsProcessAlive(currentPID) {
			t.Errorf("current PID %d should be alive", currentPID)
		}
		if IsProcessAlive(-1) {
			t.Errorf("PID -1 should not be alive")
		}
		if IsProcessAlive(0) {
			t.Errorf("PID 0 should not be alive")
		}
		if IsProcessAlive(99999999) {
			t.Errorf("PID 99999999 should not be alive")
		}

		t.Setenv("AGENT_STATUS_TEST_MOCK_PROC", "1")
		if !IsProcessAlive(99999999) {
			t.Errorf("expected mock alive with AGENT_STATUS_TEST_MOCK_PROC=1")
		}
	})

	t.Run("VerifyProcessMatch", func(t *testing.T) {
		if VerifyProcessMatch(-1, 1000) {
			t.Errorf("VerifyProcessMatch(-1) should be false")
		}
		if VerifyProcessMatch(99999999, 1000) {
			t.Errorf("VerifyProcessMatch(deadPID) should be false")
		}
		// expectedStartTime <= 0 should return true for alive proc
		if !VerifyProcessMatch(currentPID, 0) {
			t.Errorf("VerifyProcessMatch(currentPID, 0) should be true")
		}

		p, _ := process.NewProcess(int32(currentPID))
		cTime, _ := p.CreateTime()
		if cTime > 0 {
			// Exact match
			if !VerifyProcessMatch(currentPID, cTime) {
				t.Errorf("VerifyProcessMatch with exact time should be true")
			}
			// 10s difference (tolerance is 60s)
			if !VerifyProcessMatch(currentPID, cTime+10000) {
				t.Errorf("VerifyProcessMatch with 10s diff should be true")
			}
			// 120s difference (exceeds tolerance)
			if VerifyProcessMatch(currentPID, cTime+120000) {
				t.Errorf("VerifyProcessMatch with 120s diff should be false")
			}
		}

		t.Setenv("AGENT_STATUS_TEST_MOCK_PROC", "1")
		if !VerifyProcessMatch(99999999, 12345) {
			t.Errorf("expected mock match with AGENT_STATUS_TEST_MOCK_PROC=1")
		}
	})

	t.Run("GetProcessCWD", func(t *testing.T) {
		cwd, err := GetProcessCWD(currentPID)
		if err != nil {
			t.Fatalf("GetProcessCWD(currentPID) error: %v", err)
		}
		if cwd == "" {
			t.Errorf("GetProcessCWD returned empty string")
		}

		_, err = GetProcessCWD(-1)
		if err == nil {
			t.Errorf("GetProcessCWD(-1) expected error")
		}
	})
}

func TestClaudeCodeScanner_WithSnapshotProvider(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "claude-sessions")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}

	currentPID := os.Getpid()
	now := time.Now().UnixMilli()

	validDesc := SessionDescriptor{
		PID:            currentPID,
		SessionID:      "sess-snap-valid",
		CWD:            tmpDir,
		Status:         "active",
		TranscriptPath: filepath.Join(tmpDir, "transcript.jsonl"),
		StartedAt:      now,
	}
	vBytes, _ := json.Marshal(validDesc)
	_ = os.WriteFile(filepath.Join(sessionsDir, "valid.json"), vBytes, 0600)

	deadDesc := SessionDescriptor{
		PID:            999999,
		SessionID:      "sess-snap-dead",
		CWD:            tmpDir,
		Status:         "active",
		TranscriptPath: filepath.Join(tmpDir, "transcript.jsonl"),
		StartedAt:      now,
	}
	dBytes, _ := json.Marshal(deadDesc)
	_ = os.WriteFile(filepath.Join(sessionsDir, "dead.json"), dBytes, 0600)

	driftDesc := SessionDescriptor{
		PID:            currentPID,
		SessionID:      "sess-snap-drift",
		CWD:            tmpDir,
		Status:         "active",
		TranscriptPath: filepath.Join(tmpDir, "transcript.jsonl"),
		StartedAt:      now + 200000,
	}
	drBytes, _ := json.Marshal(driftDesc)
	_ = os.WriteFile(filepath.Join(sessionsDir, "drift.json"), drBytes, 0600)

	mockSnap := &LiveProcessSnapshot{
		processes: map[int]ProcessMeta{
			currentPID: {
				PID:        currentPID,
				Name:       "node",
				CreateTime: now,
				Alive:      true,
			},
		},
	}

	provider := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
		return mockSnap, nil
	})

	scanner := NewClaudeCodeScanner([]string{sessionsDir})
	scanner.SetProcessSnapshotProvider(provider)

	sessions, err := scanner.Scan()
	if err != nil {
		t.Fatalf("scanner.Scan error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 valid session, got %d", len(sessions))
	}
	if sessions[0].SessionID != "sess-snap-valid" {
		t.Errorf("expected sess-snap-valid, got %s", sessions[0].SessionID)
	}

	// Test with provider returning nil / error fallback
	errProvider := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
		return nil, errors.New("err")
	})
	scanner.SetProcessSnapshotProvider(errProvider)
	sessionsFallback, err := scanner.Scan()
	if err != nil {
		t.Fatalf("scanner.Scan fallback error: %v", err)
	}
	if len(sessionsFallback) != 1 {
		t.Errorf("expected 1 session from fallback, got %d", len(sessionsFallback))
	}
}

func TestClaudeCodeScanner_DefersProcessSnapshotOnDeadSessions(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "claude-sessions")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}

	deadPID := 99999999
	deadDesc := SessionDescriptor{
		PID:            deadPID,
		SessionID:      "sess-dead-only",
		CWD:            tmpDir,
		Status:         "active",
		TranscriptPath: filepath.Join(tmpDir, "transcript.jsonl"),
		StartedAt:      time.Now().UnixMilli(),
	}
	dBytes, _ := json.Marshal(deadDesc)
	if err := os.WriteFile(filepath.Join(sessionsDir, "dead.json"), dBytes, 0600); err != nil {
		t.Fatal(err)
	}

	calls := 0
	provider := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
		calls++
		return &LiveProcessSnapshot{}, nil
	})

	scanner := NewClaudeCodeScanner([]string{sessionsDir})
	scanner.SetProcessSnapshotProvider(provider)

	sessions, err := scanner.Scan()
	if err != nil {
		t.Fatalf("scanner.Scan error: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions from dead descriptor, got %d", len(sessions))
	}
	if calls != 0 {
		t.Errorf("expected snapshot provider calls = 0 when all sessions are dead, got %d", calls)
	}

	// Now add a live process candidate (current test runner process)
	currentPID := os.Getpid()
	liveDesc := SessionDescriptor{
		PID:            currentPID,
		SessionID:      "sess-live-added",
		CWD:            tmpDir,
		Status:         "active",
		TranscriptPath: filepath.Join(tmpDir, "transcript.jsonl"),
		StartedAt:      time.Now().UnixMilli(),
	}
	lBytes, _ := json.Marshal(liveDesc)
	if err := os.WriteFile(filepath.Join(sessionsDir, "live.json"), lBytes, 0600); err != nil {
		t.Fatal(err)
	}

	liveSnap := &LiveProcessSnapshot{
		processes: map[int]ProcessMeta{
			currentPID: {
				PID:        currentPID,
				Name:       "test-proc",
				CreateTime: liveDesc.StartedAt,
				Alive:      true,
			},
		},
	}
	liveProvider := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
		calls++
		return liveSnap, nil
	})
	scanner.SetProcessSnapshotProvider(liveProvider)

	sessions, err = scanner.Scan()
	if err != nil {
		t.Fatalf("scanner.Scan error: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("expected 1 session from live candidate, got %d", len(sessions))
	}
	if calls != 1 {
		t.Errorf("expected snapshot provider calls = 1 when live candidate is present, got %d", calls)
	}
}

func TestClaudeCodeDescriptor_NameAndNameSource(t *testing.T) {
	raw := `{"pid":1234,"sessionId":"ses-custom","cwd":"/repo","status":"idle","startedAt":1000,"version":"2.1.280","name":"custom-name","nameSource":"custom"}`
	var desc SessionDescriptor
	if err := json.Unmarshal([]byte(raw), &desc); err != nil {
		t.Fatalf("unmarshaling descriptor: %v", err)
	}
	if desc.Name != "custom-name" {
		t.Errorf("Name = %q, want 'custom-name'", desc.Name)
	}
	if desc.NameSource != "custom" {
		t.Errorf("NameSource = %q, want 'custom'", desc.NameSource)
	}
}
