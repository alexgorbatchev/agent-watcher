package scanner

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAgyScanner(t *testing.T) {
	root := t.TempDir()
	brainDir := filepath.Join(root, "brain")
	convDir := filepath.Join(root, "conversations")
	parentID := "11111111-1111-4111-8111-111111111111"
	childID := "22222222-2222-4222-8222-222222222222"
	otherID := "33333333-3333-4333-8333-333333333333"
	workDir := filepath.Join(root, "workspace")

	for _, id := range []string{parentID, childID, otherID} {
		logsDir := filepath.Join(brainDir, id, ".system_generated", "logs")
		if err := os.MkdirAll(logsDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(brainDir, parentID, ".system_generated", "logs", "transcript_full.jsonl"),
		[]byte(`{"step_index":0,"type":"USER_INPUT","created_at":"2026-11-22T14:38:26Z","content":"hi"}`+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(brainDir, childID, ".system_generated", "logs", "transcript.jsonl"),
		[]byte(`{"step_index":0,"type":"USER_INPUT","created_at":"2026-11-22T14:38:27Z","content":"child"}`+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(brainDir, otherID, ".system_generated", "logs", "transcript_full.jsonl"),
		[]byte(`{"step_index":0,"type":"USER_INPUT","created_at":"2026-11-22T14:38:25Z","content":"old"}`+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	subDir := filepath.Join(brainDir, parentID, ".system_generated", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, childID+".json"), []byte(`{"conversationId":"`+childID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write .meta fallback for otherID.
	metaBytes, _ := json.Marshal(map[string]string{"cwd": filepath.Join(root, "other")})
	if err := os.WriteFile(filepath.Join(convDir, otherID+".meta"), metaBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Write conversation_summaries.db for parentID.
	db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			workspace_uris TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			parent_conversation_id TEXT NOT NULL DEFAULT '',
			last_modified_time DATETIME NOT NULL DEFAULT '',
			not_fully_idle NUMERIC NOT NULL DEFAULT 0,
			killed NUMERIC NOT NULL DEFAULT 0
		);
		INSERT INTO conversation_summaries VALUES ('` + parentID + `', 'Active Session', '["file://` + filepath.ToSlash(workDir) + `"]', 'RUNNING', '', '2026-11-22T14:38:28Z', 1, 0);
	`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	sc := NewAgyScanner(brainDir, func(context.Context) ([]AgyProcessInfo, error) {
		return []AgyProcessInfo{{PID: 4242, CWD: workDir, CreateTime: 1795358300000}}, nil
	})

	sessions, err := sc.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3: %+v", len(sessions), sessions)
	}
	byID := map[string]AgySessionDescriptor{}
	for _, s := range sessions {
		byID[s.SessionID] = s
	}
	if p := byID[parentID]; !p.IsActive || p.PID != 4242 || p.Title != "Active Session" || p.State != SessionStateWorking || p.CWD != filepath.Clean(workDir) {
		t.Fatalf("unexpected parent session: %+v", p)
	}
	if c := byID[childID]; !c.IsActive || !c.IsSubagent || c.ParentSessionID != parentID || c.PID != 4242 {
		t.Fatalf("unexpected child session: %+v", c)
	}
	if o := byID[otherID]; o.IsActive || o.CWD != filepath.Clean(filepath.Join(root, "other")) {
		t.Fatalf("unexpected other session: %+v", o)
	}

	// Second scan hits the file timestamp cache.
	if again, err := sc.Scan(context.Background()); err != nil || len(again) != 3 {
		t.Fatalf("cached scan: len=%d err=%v", len(again), err)
	}

	// Snapshot provider wiring + missing directory + cancelled context.
	sc.SetProcessSnapshotProvider(ProcessSnapshotProviderFunc(func(context.Context) (ProcessSnapshot, error) {
		return &testDummySnapshot{alivePID: 104}, nil
	}))
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if missing, err := NewAgyScanner(filepath.Join(root, "nope"), nil).Scan(context.Background()); err != nil || len(missing) != 0 {
		t.Fatalf("missing dir: %v %v", missing, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sc.Scan(ctx); err == nil {
		t.Fatal("expected context error")
	}
}

func TestAgySessionStateAndDefaults(t *testing.T) {
	tests := []struct {
		status       string
		notFullyIdle bool
		want         SessionState
	}{
		{"RUNNING", false, SessionStateWorking},
		{"ACTIVE", false, SessionStateWorking},
		{"WAITING", false, SessionStateWaiting},
		{"IDLE", false, SessionStateIdle},
		{"IDLE", true, SessionStateWorking},
		{"", true, SessionStateWorking},
		{"UNKNOWN", false, SessionStateUnknown},
	}
	for _, tt := range tests {
		if got := AgySessionState(tt.status, tt.notFullyIdle); got != tt.want {
			t.Fatalf("AgySessionState(%q, %v) = %q, want %q", tt.status, tt.notFullyIdle, got, tt.want)
		}
	}

	t.Setenv("AGY_SESSIONS_DIR", "/custom/agy")
	if got := DefaultAgySessionsDir(); got != "/custom/agy" {
		t.Fatalf("AGY_SESSIONS_DIR = %q", got)
	}
	t.Setenv("AGY_SESSIONS_DIR", "")
	t.Setenv("GEMINI_HOME", "/custom/gemini")
	if got := DefaultAgySessionsDir(); got != filepath.Join("/custom/gemini", "antigravity-cli", "brain") {
		t.Fatalf("GEMINI_HOME = %q", got)
	}
	t.Setenv("GEMINI_HOME", "")
	if got := DefaultAgySessionsDir(); got == "" {
		t.Fatal("default agy sessions dir should not be empty")
	}
	if !isPlausibleAgentProc("agy") || !isPlausibleAgentProc("agy.exe") {
		t.Fatal("agy should be plausible agent proc")
	}
}

func TestLiveAgyProcessFinder(t *testing.T) {
	if _, err := LiveAgyProcessFinder(context.Background()); err != nil {
		t.Fatalf("LiveAgyProcessFinder failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LiveAgyProcessFinder(ctx); err == nil {
		t.Fatal("expected context error from LiveAgyProcessFinder")
	}
}
