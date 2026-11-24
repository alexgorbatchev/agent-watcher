package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRunStartAtEndCapturesInputWrittenDuringLifecycle(t *testing.T) {
	for _, subagent := range []bool{false, true} {
		t.Run(fmt.Sprintf("subagent=%t", subagent), func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Harnesses = []Harness{HarnessClaudeCode}
			cfg.StartAtEnd = true
			path, _ := transcriptFixture(t, cfg, HarnessClaudeCode)
			if subagent {
				path = filepath.Join(filepath.Dir(cfg.CursorPath), "subagents", "worker.jsonl")
				writeTestFile(t, path, `{"type":"user","message":{"role":"user","content":"history"}}`+"\n")
			}
			events := make(chan Event, 64)
			startTestWatcher(t, cfg, func(ctx context.Context, event Event, ack func()) error {
				if event.EventType == "run_started" && (event.SubagentID != nil) == subagent {
					file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, writeErr := file.WriteString(liveTranscriptLine(HarnessClaudeCode) + "\n")
					closeErr := file.Close()
					if writeErr != nil {
						return writeErr
					}
					if closeErr != nil {
						return closeErr
					}
				}
				return collectEvents(events)(ctx, event, ack)
			})
			awaitEvent(t, events, "run_started")
			event := awaitEvent(t, events, "turn_start")
			if (event.SubagentID != nil) != subagent {
				t.Fatalf("input delivered for wrong session: %+v", event)
			}
			if event.Data.Content == nil || *event.Data.Content != "later" {
				t.Fatalf("history delivered instead of new input: %+v", event)
			}
		})
	}
}
