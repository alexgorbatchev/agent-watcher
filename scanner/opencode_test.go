package scanner

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func TestDefaultOpencodeDBPath(t *testing.T) {
	t.Run("with OPENCODE_DB_PATH set", func(t *testing.T) {
		t.Setenv("OPENCODE_DB_PATH", "/custom/opencode.db")
		got := DefaultOpencodeDBPath()
		if got != "/custom/opencode.db" {
			t.Errorf("DefaultOpencodeDBPath() = %q, want %q", got, "/custom/opencode.db")
		}
	})

	t.Run("with XDG_DATA_HOME set", func(t *testing.T) {
		t.Setenv("OPENCODE_DB_PATH", "")
		t.Setenv("XDG_DATA_HOME", "/custom/xdg")
		got := DefaultOpencodeDBPath()
		want := filepath.Join("/custom/xdg", "opencode", "opencode.db")
		if got != want {
			t.Errorf("DefaultOpencodeDBPath() = %q, want %q", got, want)
		}
	})

	t.Run("with env unset and valid HOME", func(t *testing.T) {
		t.Setenv("OPENCODE_DB_PATH", "")
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/custom/user/home")
		got := DefaultOpencodeDBPath()
		want := filepath.Join("/custom/user/home", ".local", "share", "opencode", "opencode.db")
		if got != want {
			t.Errorf("DefaultOpencodeDBPath() = %q, want %q", got, want)
		}
	})
}

func TestLiveOpencodeProcessFinder(t *testing.T) {
	tmpDir := t.TempDir()
	opencodeBin := filepath.Join(tmpDir, "opencode")
	if sleepData, err := os.ReadFile("/bin/sleep"); err == nil {
		if err := os.WriteFile(opencodeBin, sleepData, 0755); err == nil {
			cmd := exec.Command(opencodeBin, "5")
			if err := cmd.Start(); err == nil {
				defer func() {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}()
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	procs, err := LiveOpencodeProcessFinder(ctx)
	if err != nil {
		t.Logf("LiveOpencodeProcessFinder returned error (acceptable if no permission): %v", err)
	}
	t.Logf("LiveOpencodeProcessFinder found %d processes", len(procs))
}

func createTestOpencodeDB(t *testing.T, dbPath string) {
	t.Helper()
	var dsn string
	if dbPath == ":memory:" {
		dsn = "file::memory:?cache=shared"
	} else {
		dsn = "file:" + dbPath + "?mode=rwc"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("creating test opencode db: %v", err)
	}
	defer func() {
		_ = db.Close()
	}()

	schema := `
		CREATE TABLE IF NOT EXISTS session (
			id TEXT PRIMARY KEY,
			project_id TEXT,
			parent_id TEXT,
			slug TEXT,
			directory TEXT NOT NULL,
			title TEXT,
			version TEXT,
			agent TEXT,
			model TEXT,
			cost REAL,
			tokens_input INTEGER,
			tokens_output INTEGER,
			tokens_reasoning INTEGER,
			tokens_cache_read INTEGER,
			tokens_cache_write INTEGER,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			time_archived INTEGER,
			metadata TEXT
		);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("executing schema: %v", err)
	}
}

func TestOpencodeScannerDiscoveryAndPairing(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpencodeDB(t, dbPath)

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("opening sqlite db: %v", err)
	}
	defer func() {
		_ = db.Close()
	}()

	now := time.Now().UnixMilli()
	testCWD := "/Users/test/projects/opencode-app"

	// Insert 1 active session, 1 archived session, 1 subagent session
	insertQuery := `
		INSERT INTO session (
			id, project_id, parent_id, slug, directory, title,
			version, agent, model, cost, tokens_input, tokens_output,
			tokens_reasoning, tokens_cache_read, tokens_cache_write,
			time_created, time_updated, time_archived, metadata
		) VALUES
		('ses_1', 'proj_1', NULL, 'ses-1', ?, 'Active Session', '1.0.0', 'default', '{"id":"gpt-5"}', 0.15, 100, 50, 0, 0, 0, ?, ?, NULL, '{}'),
		('ses_2', 'proj_1', NULL, 'ses-2', ?, 'Archived Session', '1.0.0', 'default', '{"id":"gpt-5"}', 0.20, 200, 80, 0, 0, 0, ?, ?, ?, '{}'),
		('ses_3', 'proj_1', 'ses_1', 'ses-3', ?, 'Subagent Session', '1.0.0', 'planner', '{"id":"gpt-5"}', 0.05, 50, 20, 0, 0, 0, ?, ?, NULL, '{}');
	`
	_, err = db.Exec(insertQuery,
		testCWD, now-1000, now,
		testCWD, now-5000, now-4000, now-4000,
		testCWD, now-500, now,
	)
	if err != nil {
		t.Fatalf("inserting test sessions: %v", err)
	}

	mockFinder := func(ctx context.Context) ([]OpencodeProcessInfo, error) {
		return []OpencodeProcessInfo{
			{
				PID:        3001,
				CWD:        testCWD,
				CreateTime: now - 1050,
			},
			{
				PID:        3002,
				CWD:        testCWD,
				CreateTime: now - 520,
			},
			{
				PID:        3003,
				CWD:        "/unrelated/cwd",
				CreateTime: now,
			},
		}, nil
	}

	scanner := NewOpencodeScanner(dbPath, mockFinder)
	sessions, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("scanner.Scan error: %v", err)
	}

	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(sessions))
	}

	activeCount := 0
	subagentCount := 0
	for _, s := range sessions {
		if s.IsActive {
			activeCount++
			if s.PID != 3001 && s.PID != 3002 {
				t.Errorf("unexpected active PID: %d", s.PID)
			}
		}
		if s.IsSubagent {
			subagentCount++
			if s.ParentID != "ses_1" {
				t.Errorf("expected ParentID 'ses_1', got %q", s.ParentID)
			}
		}
	}

	if activeCount != 2 {
		t.Errorf("expected 2 active sessions, got %d", activeCount)
	}
	if subagentCount != 1 {
		t.Errorf("expected 1 subagent session, got %d", subagentCount)
	}

	// Test with AGENT_STATUS_TEST_MOCK_PROC = "1"
	t.Run("with AGENT_STATUS_TEST_MOCK_PROC", func(t *testing.T) {
		t.Setenv("AGENT_STATUS_TEST_MOCK_PROC", "1")
		mockEmptyFinder := func(ctx context.Context) ([]OpencodeProcessInfo, error) {
			return nil, nil
		}
		mockScanner := NewOpencodeScanner(dbPath, mockEmptyFinder)
		mockSessions, err := mockScanner.Scan(context.Background())
		if err != nil {
			t.Fatalf("mockScanner.Scan error: %v", err)
		}
		for _, s := range mockSessions {
			switch s.SessionID {
			case "ses_1", "ses_3":
				if !s.IsActive || s.PID != 99994 {
					t.Errorf("expected active session PID 99994, got %+v", s)
				}
			case "ses_2":
				if s.IsActive {
					t.Errorf("expected archived session to remain inactive")
				}
			}
		}
	})

	// Test NewOpencodeScanner with empty dbPath
	defaultScanner := NewOpencodeScanner("", nil)
	if defaultScanner.processFinder == nil {
		t.Errorf("expected default process finder to be set")
	}

	// Test Scan with empty dbPath
	emptyScanner := &OpencodeScanner{dbPath: ""}
	emptySessions, err := emptyScanner.Scan(context.Background())
	if err != nil || len(emptySessions) != 0 {
		t.Errorf("expected nil, nil for empty dbPath, got %v, %v", emptySessions, err)
	}

	// Test Scan with nonexistent dbPath
	nonexistentScanner := NewOpencodeScanner("/nonexistent/opencode.db", nil)
	nonexistentSessions, err := nonexistentScanner.Scan(context.Background())
	if err != nil || len(nonexistentSessions) != 0 {
		t.Errorf("expected nil, nil for nonexistent db, got %v, %v", nonexistentSessions, err)
	}
}

func TestOpencodeScanner_MultipleSessionsAndResumed(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpencodeDB(t, dbPath)

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("opening sqlite db: %v", err)
	}
	defer func() {
		_ = db.Close()
	}()

	baseTime := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC).UnixMilli()
	threeDaysAgo := baseTime - 3*24*3600*1000

	testCWD := "/Users/test/projects/opencode-multi"
	resumedCWD := "/Users/test/projects/opencode-resumed"

	// 1. Older session in testCWD: created 10m ago, updated 5m ago
	// 2. Newer session in testCWD: created 1m ago, updated now
	// 3. Resumed session in resumedCWD: created 3 days ago, updated now
	insertQuery := `
		INSERT INTO session (
			id, project_id, parent_id, slug, directory, title,
			version, agent, model, cost, tokens_input, tokens_output,
			tokens_reasoning, tokens_cache_read, tokens_cache_write,
			time_created, time_updated, time_archived, metadata
		) VALUES
		('ses_old', 'proj_1', NULL, 'old', ?, 'Old Session', '1.0.0', 'default', '{"id":"gpt-5"}', 0.1, 10, 10, 0, 0, 0, ?, ?, NULL, '{}'),
		('ses_new', 'proj_1', NULL, 'new', ?, 'New Session', '1.0.0', 'default', '{"id":"gpt-5"}', 0.1, 10, 10, 0, 0, 0, ?, ?, NULL, '{}'),
		('ses_resumed', 'proj_2', NULL, 'resumed', ?, 'Resumed Session', '1.0.0', 'default', '{"id":"gpt-5"}', 0.1, 10, 10, 0, 0, 0, ?, ?, NULL, '{}');
	`
	_, err = db.Exec(insertQuery,
		testCWD, baseTime-600000, baseTime-300000,
		testCWD, baseTime-60000, baseTime,
		resumedCWD, threeDaysAgo, baseTime,
	)
	if err != nil {
		t.Fatalf("inserting test sessions: %v", err)
	}

	mockFinder := func(ctx context.Context) ([]OpencodeProcessInfo, error) {
		return []OpencodeProcessInfo{
			{
				PID:        7001,
				CWD:        testCWD,
				CreateTime: baseTime - 600000,
			},
			{
				PID:        7002,
				CWD:        resumedCWD,
				CreateTime: baseTime,
			},
		}, nil
	}

	scanner := NewOpencodeScanner(dbPath, mockFinder)
	sessions, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("scanner.Scan error: %v", err)
	}

	sessionByID := make(map[string]OpencodeSessionDescriptor)
	for _, s := range sessions {
		sessionByID[s.SessionID] = s
	}

	// In testCWD, the newer session (ses_new) with latest TimeUpdated must pair with PID 7001
	newDesc, ok := sessionByID["ses_new"]
	if !ok {
		t.Fatalf("session ses_new not found")
	}
	if !newDesc.IsActive || newDesc.PID != 7001 {
		t.Errorf("expected newer session ses_new to be active with PID 7001, got active=%v PID=%d", newDesc.IsActive, newDesc.PID)
	}

	oldDesc, ok := sessionByID["ses_old"]
	if !ok {
		t.Fatalf("session ses_old not found")
	}
	if oldDesc.IsActive {
		t.Errorf("expected older session ses_old to be inactive, got active=%v PID=%d", oldDesc.IsActive, oldDesc.PID)
	}

	// Resumed session (ses_resumed) must pair with PID 7002 despite created 3 days ago
	resumedDesc, ok := sessionByID["ses_resumed"]
	if !ok {
		t.Fatalf("session ses_resumed not found")
	}
	if !resumedDesc.IsActive || resumedDesc.PID != 7002 {
		t.Errorf("expected resumed session ses_resumed to be active with PID 7002, got active=%v PID=%d", resumedDesc.IsActive, resumedDesc.PID)
	}
}

func TestOpencodeScanner_SkipsProcessFinderWhenNoCandidateSessions(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpencodeDB(t, dbPath)

	finderCalled := 0
	mockFinder := func(ctx context.Context) ([]OpencodeProcessInfo, error) {
		finderCalled++
		return nil, nil
	}

	scanner := NewOpencodeScanner(dbPath, mockFinder)
	sessions, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("unexpected error from Scan: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
	if finderCalled != 0 {
		t.Errorf("expected process finder NOT to be called when DB has 0 sessions, but it was called %d times", finderCalled)
	}

	// Also test when all sessions in DB are archived
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("opening sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UnixMilli()
	insertQuery := `
		INSERT INTO session (
			id, project_id, parent_id, slug, directory, title,
			version, agent, model, cost, tokens_input, tokens_output,
			tokens_reasoning, tokens_cache_read, tokens_cache_write,
			time_created, time_updated, time_archived, metadata
		) VALUES
		('archived_1', 'proj_1', NULL, 'archived-1', '/test', 'Archived', '1.0.0', 'default', '{}', 0.0, 0, 0, 0, 0, 0, ?, ?, ?, '{}');
	`
	_, err = db.Exec(insertQuery, now-1000, now, now)
	if err != nil {
		t.Fatalf("inserting archived session: %v", err)
	}

	finderCalled = 0
	sessions, err = scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("unexpected error from Scan: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("expected 1 session, got %d", len(sessions))
	}
	if finderCalled != 0 {
		t.Errorf("expected process finder NOT to be called when all sessions are archived, but it was called %d times", finderCalled)
	}
}

func TestOpencodeScanner_SetProcessFinderAndProvider(t *testing.T) {
	scanner := NewOpencodeScanner("/dummy", nil)

	customFinder := func(ctx context.Context) ([]OpencodeProcessInfo, error) {
		return []OpencodeProcessInfo{{PID: 345}}, nil
	}
	scanner.SetProcessFinder(customFinder)
	procs, err := scanner.processFinder(context.Background())
	if err != nil || len(procs) != 1 || procs[0].PID != 345 {
		t.Errorf("SetProcessFinder failed: %v, %v", procs, err)
	}

	snapProvider := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
		return &testDummySnapshot{alivePID: 103}, nil
	})
	scanner.SetProcessSnapshotProvider(snapProvider)
	procs, err = scanner.processFinder(context.Background())
	if err != nil || len(procs) != 1 || procs[0].PID != 103 {
		t.Errorf("SetProcessSnapshotProvider failed: %v, %v", procs, err)
	}
}
