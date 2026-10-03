package watcher

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestTranscriptBackpressurePreservesParsedBatch(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessPi}
	path, _ := transcriptFixture(t, cfg, HarnessPi)
	rejected := make(chan struct{}, 1)
	events := make(chan Event, 64)
	failed := false
	stop := startTestWatcher(t, cfg, func(ctx context.Context, event Event, ack func()) error {
		if event.EventType == "turn_end" && !failed {
			failed = true
			rejected <- struct{}{}
			return errors.New("backpressure")
		}
		return collectEvents(events)(ctx, event, ack)
	})
	select {
	case <-rejected:
	case <-time.After(testWait):
		t.Fatal("handler not called")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, events, "turn_end")
	stop()
}

func TestDeliveryQueueCommitOrderAndRotation(t *testing.T) {
	var queue deliveryQueue
	var committed []int
	first := queue.add(1, func() error { committed = append(committed, 1); return nil })
	second := queue.add(1, func() error { committed = append(committed, 2); return nil })
	if err := second(); err != nil {
		t.Fatal(err)
	}
	if len(committed) != 0 {
		t.Fatal("later acknowledgement skipped an unacknowledged line")
	}
	if err := first(); err != nil {
		t.Fatal(err)
	}
	if len(committed) != 2 || committed[0] != 1 || committed[1] != 2 {
		t.Fatalf("incorrect commit order: %v", committed)
	}
	old := queue.add(1, func() error { t.Error("old file generation committed"); return nil })
	current := queue.add(2, func() error { return nil })
	if err := old(); err != nil {
		t.Fatal(err)
	}
	if err := current(); err != nil {
		t.Fatal(err)
	}
	fail := queue.add(2, func() error { return errors.New("commit failed") })
	if err := fail(); err == nil {
		t.Fatal("commit error hidden")
	}
}
