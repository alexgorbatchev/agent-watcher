package watcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAgyWatcherRunAndSnapshot(t *testing.T) {
	cfg := testConfig(t)
	root := filepath.Dir(cfg.AgySessionsDir)
	brainDir := filepath.Join(root, "agy", "brain")
	convDir := filepath.Join(root, "agy", "conversations")
	cfg.AgySessionsDir = brainDir
	cfg.Harnesses = []Harness{HarnessAgy}

	sessionID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	logsDir := filepath.Join(brainDir, sessionID, ".system_generated", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]string{"cwd": root})
	if err := os.WriteFile(filepath.Join(convDir, sessionID+".meta"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(logsDir, "transcript_full.jsonl")
	line := `{"step_index":0,"type":"USER_INPUT","created_at":"2026-11-22T14:38:26Z","content":"hello agy"}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	sessions, err := Snapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Harness != HarnessAgy || sessions[0].SessionID != sessionID {
		t.Fatalf("unexpected agy snapshot: %+v", sessions)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var gotTurnStart bool
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, func(_ context.Context, ev Event, ack func()) error {
			if ack != nil {
				ack()
			}
			mu.Lock()
			if ev.Harness == HarnessAgy && ev.EventType == "turn_start" {
				gotTurnStart = true
				cancel()
			}
			mu.Unlock()
			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testWait):
		cancel()
		t.Fatal("timed out waiting for agy turn_start event")
	}
	mu.Lock()
	ok := gotTurnStart
	mu.Unlock()
	if !ok {
		t.Fatal("expected agy turn_start event")
	}
}
