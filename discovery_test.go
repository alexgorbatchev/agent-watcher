package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/alexgorbatchev/agent-watcher/scanner"
	"github.com/alexgorbatchev/agent-watcher/tailer"
)

func TestRunRetriesLifecycleDelivery(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessClaudeCode}
	_, discovery := transcriptFixture(t, cfg, HarnessClaudeCode)
	events := make(chan Event, 64)
	var startOnce, exitOnce sync.Once
	stop := startTestWatcher(t, cfg, func(ctx context.Context, event Event, ack func()) error {
		reject := false
		switch event.EventType {
		case "run_started":
			startOnce.Do(func() { reject = true })
		case "run_exited":
			exitOnce.Do(func() { reject = true })
		}
		if reject {
			return errors.New("delivery unavailable")
		}
		return collectEvents(events)(ctx, event, ack)
	})
	awaitEvent(t, events, "run_started")
	awaitEvent(t, events, "turn_end")
	if err := os.Remove(discovery); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, events, "run_exited")
	stop()
}

func TestRunExplicitClaudeProjectsPath(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessClaudeCode}
	t.Setenv("CLAUDE_PROJECTS_DIR", filepath.Join(t.TempDir(), "other"))
	before := os.Getenv("CLAUDE_PROJECTS_DIR")
	cwd := filepath.Dir(cfg.CursorPath)
	writeTestFile(t, filepath.Join(cfg.ClaudeSessionsDirs[0], "session.json"), fmtDescriptor(t, cwd))
	writeTestFile(t, filepath.Join(cfg.ClaudeProjectsDir, "project", "session.jsonl"), `{"type":"user","message":{"role":"user","content":"hello"}}`+"\n")
	events := make(chan Event, 64)
	stop := startTestWatcher(t, cfg, collectEvents(events))
	awaitEvent(t, events, "turn_start")
	stop()
	if os.Getenv("CLAUDE_PROJECTS_DIR") != before {
		t.Fatal("library changed process environment")
	}
}

func fmtDescriptor(t *testing.T, cwd string) string {
	t.Helper()
	data, err := json.Marshal(scanner.SessionDescriptor{SessionID: "session", PID: os.Getpid(), CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunPersistenceFailures(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessClaudeCode}
	transcriptFixture(t, cfg, HarnessClaudeCode)
	if err := os.Mkdir(cfg.CursorPath, 0755); err != nil {
		t.Fatal(err)
	}
	// Initializing from a directory must fail before any events are emitted.
	if err := Run(context.Background(), cfg, collectEvents(make(chan Event, 1))); err == nil {
		t.Fatal("invalid cursor path accepted")
	}
	if err := os.Remove(cfg.CursorPath); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, cfg.CursorPath+".opencode", "invalid")
	if err := Run(context.Background(), cfg, collectEvents(make(chan Event, 1))); err == nil {
		t.Fatal("invalid database checkpoints accepted")
	}
}

func TestRunnerPrunesAndReportsPersistenceErrors(t *testing.T) {
	cfg := testConfig(t)
	cursors, err := tailer.NewCursorStore(cfg.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := scanner.NewSessionTracker(cfg.SessionStatePath)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tailer.NewWatcherRegistry()
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := newCheckpointStore(cfg.CursorPath + ".opencode")
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{cfg: cfg, logger: cfg.Logger, cursors: cursors, tracker: tracker, registry: registry, checkpoints: checkpoints}
	cursors.Set(&tailer.Cursor{Key: "stale", Path: filepath.Join(t.TempDir(), "missing"), Offset: 10})
	if err := os.Mkdir(cfg.CursorPath, 0755); err != nil {
		t.Fatal(err)
	}
	tracker.Set("session", scanner.TrackedSession{SessionID: "session"})
	if err := os.Mkdir(cfg.SessionStatePath, 0755); err != nil {
		t.Fatal(err)
	}
	ack, _, _ := checkpoints.reserve("event", "fingerprint", false)
	ack()
	if err := os.Mkdir(cfg.CursorPath+".opencode", 0755); err != nil {
		t.Fatal(err)
	}
	r.prune()
	r.shutdown(func() {})
	if _, exists := cursors.Get("stale"); exists {
		t.Fatal("stale cursor was not pruned after persistence failure")
	}
}
