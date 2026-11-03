package watcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoveryUsesNativeActivity(t *testing.T) {
	for _, tt := range []struct {
		harness  Harness
		activity string
	}{
		{HarnessPi, `{"type":"compaction","timestamp":%q,"summary":"compacted"}`},
		{HarnessCodex, `{"type":"event_msg","timestamp":%q,"payload":{"type":"task_complete"}}`},
		{HarnessClaudeCode, `{"type":"progress","timestamp":%q,"data":{"type":"agent_progress"}}`},
	} {
		t.Run(string(tt.harness), func(t *testing.T) {
			cfg := testConfig(t)
			path, _ := transcriptFixture(t, cfg, tt.harness)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			stamp := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
			content = append(content, []byte(fmt.Sprintf(tt.activity, stamp.Format(time.RFC3339Nano))+"\n"+`{"type":"turn_start","timestamp":"2099-01-01T00:00:00Z"}`+"\n")...)
			writeTestFile(t, path, string(content))
			d := newDiscovery(cfg)
			d.startTick(cfg.ProcessSnapshotProvider)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if sessions, err := d.discover(ctx, tt.harness); !errors.Is(err, context.Canceled) || len(sessions) != 0 {
				t.Fatalf("canceled discovery = %+v, %v", sessions, err)
			}
			sessions, err := d.discover(context.Background(), tt.harness)
			if err != nil || len(sessions) != 1 {
				t.Fatalf("discovery = %+v, %v", sessions, err)
			}
			if sessions[0].Timestamp != stamp.UnixMilli() {
				t.Fatalf("activity = %d, want %d", sessions[0].Timestamp, stamp.UnixMilli())
			}
		})
	}
}

func TestClaudeSubagentStartsAtFirstNativeActivity(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessClaudeCode}
	transcriptFixture(t, cfg, HarnessClaudeCode)
	stamp := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	path := filepath.Join(filepath.Dir(cfg.CursorPath), "subagents", "worker.jsonl")
	content := `{"type":"queue-operation","timestamp":"2020-01-01T00:00:00Z"}` + "\n" +
		fmt.Sprintf(`{"type":"progress","timestamp":%q,"data":{"type":"agent_progress"}}`, stamp.Format(time.RFC3339Nano)) + "\n" +
		`{"type":"user","timestamp":"2099-01-01T00:00:00Z","message":{"role":"user","content":"task"}}` + "\n"
	writeTestFile(t, path, content)
	events := make(chan Event, 64)
	stop := startTestWatcher(t, cfg, collectEvents(events))
	defer stop()
	for {
		event := awaitEvent(t, events, "run_started")
		if event.SubagentID == nil {
			continue
		}
		if *event.SubagentID != "worker" || event.Timestamp != stamp.UnixMilli() {
			t.Fatalf("subagent start = %+v, want timestamp %d", event, stamp.UnixMilli())
		}
		return
	}
}
