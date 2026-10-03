package watcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestOpencodeAcknowledgementControlsRestartReplay(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(map[bool]string{false: "unacknowledged", true: "acknowledged"}[acknowledged], func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Harnesses = []Harness{HarnessOpencode}
			db := createOpencodeFixture(t, cfg)
			execTestSQL(t, db, `INSERT INTO message VALUES ('user','session',2,'{"role":"user"}')`)
			execTestSQL(t, db, `INSERT INTO part VALUES ('prompt','user','session',2,'{"type":"text","text":"hello"}')`)
			events := make(chan Event, 32)
			stop := startTestWatcher(t, cfg, func(ctx context.Context, event Event, ack func()) error {
				if !acknowledged {
					ack = nil
				}
				return collectEvents(events)(ctx, event, ack)
			})
			awaitEvent(t, events, "user_prompt")
			stop()
			if acknowledged {
				execTestSQL(t, db, `INSERT INTO message VALUES ('next','session',3,'{"role":"user"}')`)
				execTestSQL(t, db, `INSERT INTO part VALUES ('next-prompt','next','session',3,'{"type":"text","text":"next"}')`)
			}
			replayed := make(chan Event, 32)
			stop = startTestWatcher(t, cfg, collectEvents(replayed))
			event := awaitEvent(t, replayed, "user_prompt")
			stop()
			want := "hello"
			if acknowledged {
				want = "next"
			}
			if event.Data.Content == nil || *event.Data.Content != want {
				t.Fatalf("unexpected restart event: %+v", event)
			}
		})
	}
}

func TestOpencodeStreamingToolAndUsage(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessOpencode}
	db := createOpencodeFixture(t, cfg)
	execTestSQL(t, db, `INSERT INTO message VALUES ('assistant','session',3,'{"role":"assistant","cost":0,"tokens":{"input":0,"output":0,"cache":{"read":0,"write":0}}}')`)
	execTestSQL(t, db, `INSERT INTO part VALUES ('tool','assistant','session',3,'{"type":"tool","tool":"bash","callID":"call","state":{"status":"running","input":{"command":"pwd"}}}')`)
	events := make(chan Event, 64)
	stop := startTestWatcher(t, cfg, collectEvents(events))
	awaitEvent(t, events, "tool_use")
	execTestSQL(t, db, `UPDATE part SET data='{"type":"tool","tool":"bash","callID":"call","state":{"status":"completed","input":{"command":"pwd"},"output":"workspace"}}' WHERE id='tool'`)
	result := awaitEvent(t, events, "tool_result")
	if result.Data.ToolOutput == nil || *result.Data.ToolOutput != "workspace" {
		t.Fatalf("missing tool output: %+v", result)
	}
	select {
	case event := <-events:
		if event.EventType == "turn_end" {
			t.Fatal("streaming message emitted completion")
		}
	case <-time.After(30 * time.Millisecond):
	}
	execTestSQL(t, db, `UPDATE message SET data='{"role":"assistant","time":{"created":3,"completed":5},"finish":"stop","tokens":{"input":10,"output":2,"total":12,"cache":{"read":0,"write":0}}}' WHERE id='assistant'`)
	awaitEvent(t, events, "turn_end")
	execTestSQL(t, db, `UPDATE message SET data='{"role":"assistant","time":{"created":3,"completed":5},"finish":"stop","tokens":{"input":10,"output":3,"total":13,"cache":{"read":0,"write":0}}}' WHERE id='assistant'`)
	execTestSQL(t, db, `INSERT INTO part VALUES ('new','assistant','session',6,'{"type":"text","text":"after completion"}')`)
	awaitEvent(t, events, "text")
	stop()
	for len(events) > 0 {
		if event := <-events; event.EventType == "turn_end" {
			t.Fatal("completed usage counted twice")
		}
	}
}

func TestOpencodeRetriesRejectedEventsAndEmitsErrors(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessOpencode}
	db := createOpencodeFixture(t, cfg)
	execTestSQL(t, db, `INSERT INTO message VALUES ('assistant','session',3,'{"role":"assistant","finish":"error","error":{"name":"APIError","data":{"message":"limited","statusCode":429,"isRetryable":true}}}')`)
	events := make(chan Event, 64)
	var once sync.Once
	stop := startTestWatcher(t, cfg, func(ctx context.Context, event Event, ack func()) error {
		reject := false
		if event.EventType == "error" {
			once.Do(func() { reject = true })
		}
		if reject {
			return errors.New("consumer unavailable")
		}
		return collectEvents(events)(ctx, event, ack)
	})
	event := awaitEvent(t, events, "error")
	if event.Data.IsRateLimit == nil || !*event.Data.IsRateLimit {
		t.Fatalf("rate-limit metadata lost: %+v", event)
	}
	stop()
}

func TestOpencodeReadFailures(t *testing.T) {
	for _, table := range []string{"message", "part", "session"} {
		t.Run(table, func(t *testing.T) {
			cfg := testConfig(t)
			db := createOpencodeFixture(t, cfg)
			execTestSQL(t, db, "DROP TABLE "+table)
			checkpoints, err := newCheckpointStore(cfg.CursorPath + ".opencode")
			if err != nil {
				t.Fatal(err)
			}
			r := &runner{cfg: cfg, logger: cfg.Logger, checkpoints: checkpoints,
				handler: func(context.Context, Event, func()) error {
					t.Error("event emitted after failed database read")
					return nil
				}}
			if err := r.pollOpencode(context.Background(), session{Event: Event{SessionID: "session"}}); err == nil {
				t.Fatal("missing table did not produce a read error")
			}
		})
	}
}

func TestOpencodeDrainsCompletionBeforeExit(t *testing.T) {
	cfg := testConfig(t)
	cfg.Harnesses = []Harness{HarnessOpencode}
	db := createOpencodeFixture(t, cfg)
	execTestSQL(t, db, `INSERT INTO message VALUES ('assistant','session',3,'{"role":"assistant"}')`)
	execTestSQL(t, db, `INSERT INTO part VALUES ('thought','assistant','session',3,'{"type":"reasoning","text":"working"}')`)
	events := make(chan Event, 32)
	var once sync.Once
	stop := startTestWatcher(t, cfg, func(ctx context.Context, event Event, ack func()) error {
		if event.EventType == "thinking" {
			once.Do(func() {
				execTestSQL(t, db, `UPDATE message SET data='{"role":"assistant","time":{"created":3,"completed":5},"finish":"stop","tokens":{"input":10,"output":2,"total":12,"cache":{"read":0,"write":0}}}' WHERE id='assistant'`)
				execTestSQL(t, db, `UPDATE session SET time_archived=6 WHERE id='session'`)
			})
		}
		return collectEvents(events)(ctx, event, ack)
	})
	defer stop()
	awaitEvent(t, events, "thinking")
	select {
	case event := <-events:
		if event.EventType != "turn_end" || event.Data.Usage == nil || event.Data.Usage.Total != 12 {
			t.Fatalf("final usage lost before exit: %+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("final completion was not delivered")
	}
	awaitEvent(t, events, "run_exited")
}
