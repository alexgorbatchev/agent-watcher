package watcher

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/agent-watcher/scanner"
	"github.com/alexgorbatchev/agent-watcher/tailer"
)

const testWait = 3 * time.Second

type testSnapshot struct{ cwd string }

func (s testSnapshot) IsProcessAlive(pid int) bool              { return pid == os.Getpid() }
func (s testSnapshot) VerifyProcessMatch(pid int, _ int64) bool { return s.IsProcessAlive(pid) }
func (s testSnapshot) PiProcesses() []scanner.PiProcessInfo {
	return []scanner.PiProcessInfo{{PID: os.Getpid(), CWD: s.cwd}}
}
func (s testSnapshot) CodexProcesses() []scanner.CodexProcessInfo {
	return []scanner.CodexProcessInfo{{PID: os.Getpid(), CWD: s.cwd}}
}
func (s testSnapshot) OpencodeProcesses() []scanner.OpencodeProcessInfo {
	return []scanner.OpencodeProcessInfo{{PID: os.Getpid(), CWD: s.cwd}}
}
func (s testSnapshot) Process(pid int) (scanner.ProcessMeta, bool) {
	return scanner.ProcessMeta{PID: pid, Alive: s.IsProcessAlive(pid)}, s.IsProcessAlive(pid)
}

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		ClaudeSessionsDirs: []string{filepath.Join(dir, "claude")},
		ClaudeProjectsDir:  filepath.Join(dir, "projects"),
		PiSessionsDir:      filepath.Join(dir, "pi"), CodexSessionsDir: filepath.Join(dir, "codex"),
		OpencodeDBPath: filepath.Join(dir, "opencode.db"),
		CursorPath:     filepath.Join(dir, "cursors.json"), SessionStatePath: filepath.Join(dir, "sessions.json"),
		ScanInterval: 10 * time.Millisecond, CursorFlushInterval: 5 * time.Millisecond,
		CursorPruneInterval: 5 * time.Millisecond,
		ProcessSnapshotProvider: scanner.ProcessSnapshotProviderFunc(func(context.Context) (scanner.ProcessSnapshot, error) {
			return testSnapshot{cwd: dir}, nil
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func startTestWatcher(t *testing.T, cfg Config, handler Handler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, handler) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(testWait):
				t.Fatal("watcher shutdown did not join workers")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func collectEvents(events chan<- Event) Handler {
	return func(ctx context.Context, event Event, ack func()) error {
		select {
		case events <- event:
			if ack != nil {
				ack()
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func awaitEvent(t *testing.T, events <-chan Event, eventType string) Event {
	t.Helper()
	timer := time.NewTimer(testWait)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.EventType == eventType {
				return event
			}
		case <-timer.C:
			t.Fatalf("no %s event received", eventType)
		}
	}
}

func transcriptFixture(t *testing.T, cfg Config, harness Harness) (string, string) {
	t.Helper()
	cwd := filepath.Dir(cfg.CursorPath)
	stamp := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	var path, discoveryPath, content string
	switch harness {
	case HarnessClaudeCode:
		path = filepath.Join(cwd, "claude.jsonl")
		discoveryPath = filepath.Join(cfg.ClaudeSessionsDirs[0], "session.json")
		writeTestFile(t, discoveryPath, fmt.Sprintf(`{"pid":%d,"sessionId":"session","cwd":%q,"transcriptPath":%q,"version":"1.2","name":"Task","nameSource":"custom"}`, os.Getpid(), cwd, path))
		content = fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"hello"}}`, stamp) + "\n" +
			fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":"answer","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}}`, stamp) + "\n"
	case HarnessPi:
		path = filepath.Join(cfg.PiSessionsDir, scanner.EncodePiCWD(cwd), "time_session.jsonl")
		discoveryPath = path
		content = fmt.Sprintf(`{"type":"session","version":3,"id":"session","timestamp":%q,"cwd":%q}`, stamp, cwd) + "\n" +
			"invalid-json\n" +
			fmt.Sprintf(`{"type":"message","id":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`, stamp) + "\n" +
			fmt.Sprintf(`{"type":"message","id":"answer","timestamp":%q,"message":{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning"},{"type":"text","text":"done"}],"stopReason":"stop","usage":{"input":10,"output":2,"totalTokens":12}}}`, stamp) + "\n"
	case HarnessCodex:
		path = filepath.Join(cfg.CodexSessionsDir, "2026", "10", "02", "rollout-session.jsonl")
		discoveryPath = path
		content = fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":"session","cwd":%q,"timestamp":%q,"cli_version":"1.2"}}`, stamp, cwd, stamp) + "\n" +
			fmt.Sprintf(`{"type":"event_msg","timestamp":%q,"payload":{"type":"task_complete","last_agent_message":"done"}}`, stamp) + "\n"
	}
	writeTestFile(t, path, content)
	return path, discoveryPath
}

func TestRunStreamsTranscriptsAndLifecycle(t *testing.T) {
	for _, harness := range []Harness{HarnessClaudeCode, HarnessPi, HarnessCodex} {
		t.Run(string(harness), func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Harnesses = []Harness{harness}
			path, discoveryPath := transcriptFixture(t, cfg, harness)
			events := make(chan Event, 64)
			stop := startTestWatcher(t, cfg, collectEvents(events))
			start := awaitEvent(t, events, "run_started")
			if start.Harness != harness || start.SessionID != "session" || start.PID != os.Getpid() || start.CWD != filepath.Dir(cfg.CursorPath) {
				t.Fatalf("session identity lost: %+v", start)
			}
			awaitEvent(t, events, "turn_end")
			if err := os.Remove(discoveryPath); err != nil {
				t.Fatal(err)
			}
			awaitEvent(t, events, "run_exited")
			stop()
			data, err := os.ReadFile(cfg.CursorPath)
			if err != nil {
				t.Fatal(err)
			}
			var cursors map[string]tailer.Cursor
			if err := json.Unmarshal(data, &cursors); err != nil {
				t.Fatal(err)
			}
			for _, cursor := range cursors {
				if cursor.Path == path && cursor.Offset <= 0 {
					t.Fatalf("cursor did not advance: %+v", cursor)
				}
			}
		})
	}
}

func TestRunReplaysUnacknowledgedEvents(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessPi}
	path, _ := transcriptFixture(t, cfg, HarnessPi)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("invalid-json\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 64)
	stop := startTestWatcher(t, cfg, func(ctx context.Context, event Event, _ func()) error {
		return collectEvents(events)(ctx, event, nil)
	})
	awaitEvent(t, events, "turn_end")
	stop()
	replayed := make(chan Event, 64)
	stop = startTestWatcher(t, cfg, collectEvents(replayed))
	awaitEvent(t, replayed, "turn_end")
	stop()
}

func TestRunReconcilesRestoredSessions(t *testing.T) {
	cfg := testConfig(t)
	tracker, err := scanner.NewSessionTracker(cfg.SessionStatePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, harness := range []Harness{HarnessClaudeCode, HarnessPi, HarnessCodex, HarnessOpencode} {
		tracker.Set(string(harness), scanner.TrackedSession{SessionID: string(harness), Harness: string(harness), CWD: filepath.Dir(cfg.CursorPath), IsActive: true})
	}
	tracker.Set("inactive", scanner.TrackedSession{SessionID: "inactive", Harness: string(HarnessPi)})
	if err := tracker.Save(); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 16)
	stop := startTestWatcher(t, cfg, collectEvents(events))
	seen := make(map[Harness]bool)
	for range 4 {
		event := awaitEvent(t, events, "run_exited")
		seen[event.Harness] = true
	}
	stop()
	if len(seen) != 4 {
		t.Fatalf("missing exits: %v", seen)
	}
	restored, err := scanner.NewSessionTracker(cfg.SessionStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.GetAll()) != 1 {
		t.Fatalf("exited sessions retained: %v", restored.GetAll())
	}
}

func createOpencodeFixture(t *testing.T, cfg Config) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+cfg.OpencodeDBPath+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	schema := `CREATE TABLE session (
		id TEXT PRIMARY KEY, project_id TEXT, parent_id TEXT, slug TEXT, directory TEXT NOT NULL,
		title TEXT, version TEXT, agent TEXT, model TEXT, cost REAL,
		tokens_input INTEGER DEFAULT 0, tokens_output INTEGER DEFAULT 0, tokens_reasoning INTEGER DEFAULT 0,
		tokens_cache_read INTEGER DEFAULT 0, tokens_cache_write INTEGER DEFAULT 0,
		time_created INTEGER, time_updated INTEGER, time_archived INTEGER, metadata TEXT);
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, data TEXT);
		CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, data TEXT);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session (id,directory,title,version,model,time_created,time_updated) VALUES ('session',?,'Task','1.2','model',1,2)`, filepath.Dir(cfg.CursorPath)); err != nil {
		t.Fatal(err)
	}
	return db
}

func execTestSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestRunWatchesOpencodeMessages(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessOpencode}
	db := createOpencodeFixture(t, cfg)
	execTestSQL(t, db, `INSERT INTO message VALUES ('user','session',2,'{"role":"user"}')`)
	execTestSQL(t, db, `INSERT INTO part VALUES ('prompt','user','session',2,'{"type":"text","text":"hello"}')`)
	execTestSQL(t, db, `INSERT INTO message VALUES ('assistant','session',3,'{"role":"assistant","cost":0,"tokens":{"input":0,"output":0,"cache":{"read":0,"write":0}}}')`)
	execTestSQL(t, db, `INSERT INTO part VALUES ('thought','assistant','session',3,'{"type":"reasoning","text":"working"}')`)
	events := make(chan Event, 64)
	stop := startTestWatcher(t, cfg, collectEvents(events))
	start := awaitEvent(t, events, "run_started")
	if start.Data.Title == nil || *start.Data.Title != "Task" {
		t.Fatalf("missing title: %+v", start)
	}
	awaitEvent(t, events, "user_prompt")
	awaitEvent(t, events, "thinking")
	execTestSQL(t, db, `UPDATE part SET data='{"type":"reasoning","text":"working harder"}' WHERE id='thought'`)
	changed := awaitEvent(t, events, "thinking")
	if changed.Data.Content == nil || *changed.Data.Content != "working harder" {
		t.Fatalf("part update not delivered: %+v", changed)
	}
	execTestSQL(t, db, `UPDATE message SET data='{"role":"assistant","modelID":"model","time":{"created":3,"completed":5},"finish":"stop","tokens":{"input":10,"output":2,"total":12,"cache":{"read":0,"write":0}}}' WHERE id='assistant'`)
	completed := awaitEvent(t, events, "turn_end")
	if completed.Data.Usage == nil || completed.Data.Usage.Total != 12 {
		t.Fatalf("incorrect completed usage: %+v", completed)
	}
	execTestSQL(t, db, `UPDATE session SET time_archived=6 WHERE id='session'`)
	awaitEvent(t, events, "run_exited")
	stop()
}

func TestRunInvalidCursor(t *testing.T) {
	cfg := testConfig(t)
	writeTestFile(t, cfg.CursorPath, "invalid json")
	if err := Run(context.Background(), cfg, collectEvents(make(chan Event, 1))); err == nil {
		t.Fatal("invalid cursors accepted")
	}
}

func TestRunClaudeSubagents(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessClaudeCode}
	transcriptFixture(t, cfg, HarnessClaudeCode)
	path := filepath.Join(filepath.Dir(cfg.CursorPath), "subagents", "worker.jsonl")
	stamp := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	writeTestFile(t, path, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"subtask"}}`, stamp)+"\n")
	events := make(chan Event, 64)
	stop := startTestWatcher(t, cfg, collectEvents(events))
	for {
		event := awaitEvent(t, events, "run_started")
		if event.SubagentID != nil {
			if *event.SubagentID != "worker" {
				t.Fatalf("wrong subagent: %+v", event)
			}
			break
		}
	}
	stop()
}

func TestStartTimestamp(t *testing.T) {
	now := time.Now().UnixMilli()
	for _, tt := range []struct{ start, last, want int64 }{{10, 20, 20}, {10, now + 10000, 10}, {10, 0, 10}} {
		if got := startTimestamp(tt.start, tt.last); got != tt.want {
			t.Fatalf("startTimestamp(%d,%d) = %d, want %d", tt.start, tt.last, got, tt.want)
		}
	}
}

var _ scanner.ProcessSnapshot = testSnapshot{}
