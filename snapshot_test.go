package watcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSnapshotListsLiveSessionsWithoutState(t *testing.T) {
	for _, harness := range []Harness{HarnessClaudeCode, HarnessPi, HarnessCodex} {
		t.Run(string(harness), func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Harnesses = []Harness{harness}
			transcript, _ := transcriptFixture(t, cfg, harness)

			sessions, err := Snapshot(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) != 1 {
				t.Fatalf("sessions = %+v, want one", sessions)
			}
			got := sessions[0]
			if got.Harness != harness || got.SessionID != "session" || got.PID == 0 || got.Subagent {
				t.Fatalf("session = %+v", got)
			}
			if got.CWD != filepath.Dir(cfg.CursorPath) || got.TranscriptPath != transcript {
				t.Fatalf("paths = %q, %q", got.CWD, got.TranscriptPath)
			}
			if got.HarnessVersion == "" && harness != HarnessPi {
				t.Fatalf("session has no harness version: %+v", got)
			}
			if got.LastActivity <= 0 || got.LastActivity > time.Now().UnixMilli() {
				t.Fatalf("last activity = %d", got.LastActivity)
			}

			for _, path := range []string{cfg.CursorPath, cfg.CursorPath + ".opencode", cfg.SessionStatePath} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("snapshot wrote %s: %v", path, err)
				}
			}
		})
	}
}

func TestSnapshotReportsNameAndOmitsDeadSessions(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessClaudeCode}
	cwd := filepath.Dir(cfg.CursorPath)
	live := fmt.Sprintf(`{"pid":%d,"sessionId":"live","cwd":%q,"name":"derived-name","nameSource":"derived"}`, os.Getpid(), cwd)
	writeTestFile(t, filepath.Join(cfg.ClaudeSessionsDirs[0], "live.json"), live)
	writeTestFile(t, filepath.Join(cfg.ClaudeSessionsDirs[0], "dead.json"), `{"pid":99999999,"sessionId":"dead","cwd":"/tmp"}`)

	sessions, err := Snapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "live" || sessions[0].Name != "derived-name" {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestSnapshotMarksCodexSubagents(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessCodex}
	transcript, _ := transcriptFixture(t, cfg, HarnessCodex)
	cwd := filepath.Dir(cfg.CursorPath)
	stamp := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	meta := fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":"child","cwd":%q,"timestamp":%q,"parent_thread_id":"session","agent_nickname":"Euler"}}`, stamp, cwd, stamp)
	writeTestFile(t, filepath.Join(filepath.Dir(transcript), "rollout-child.jsonl"), meta+"\n")

	sessions, err := Snapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v, want parent and subagent", sessions)
	}
	child, parent := sessions[0], sessions[1]
	if !child.Subagent || child.Name != "Euler" || child.PID != parent.PID || parent.Subagent {
		t.Fatalf("child = %+v, parent = %+v", child, parent)
	}
}

func TestSnapshotOpencodeUsesRecordedActivity(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessOpencode}
	createOpencodeFixture(t, cfg)

	sessions, err := Snapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) == 0 {
		t.Fatal("no OpenCode sessions")
	}
	for _, session := range sessions {
		if session.Name == "" || session.LastActivity <= 0 {
			t.Fatalf("session = %+v", session)
		}
	}
}

func TestSnapshotReturnsHealthyHarnessesWhenOneFails(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessCodex, HarnessClaudeCode}
	transcriptFixture(t, cfg, HarnessClaudeCode)
	notADirectory := filepath.Join(t.TempDir(), "file")
	writeTestFile(t, notADirectory, "")
	cfg.CodexSessionsDir = filepath.Join(notADirectory, "sessions")

	sessions, err := Snapshot(context.Background(), cfg)
	if err == nil {
		t.Fatal("failed harness returned no error")
	}
	if len(sessions) != 1 || sessions[0].Harness != HarnessClaudeCode {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestSnapshotRejectsUnknownHarness(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{"unknown"}
	if _, err := Snapshot(context.Background(), cfg); err == nil {
		t.Fatal("unknown harness accepted")
	}
}

func TestSnapshotStopsOnCancelledContext(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessClaudeCode}
	transcriptFixture(t, cfg, HarnessClaudeCode)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Snapshot(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("Snapshot error = %v, want context.Canceled", err)
	}
}

func TestSnapshotterIsSafeForConcurrentUse(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessClaudeCode, HarnessCodex}
	transcriptFixture(t, cfg, HarnessCodex)
	snapshotter, err := NewSnapshotter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			sessions, err := snapshotter.Snapshot(context.Background())
			if err != nil || len(sessions) != 1 {
				t.Errorf("Snapshot = %+v, %v", sessions, err)
			}
		})
	}
	wg.Wait()
}
