// Package watcher discovers AI coding agent sessions and streams their lifecycle
// and transcript events without imposing a transport or dashboard data model.
package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/alexgorbatchev/agent-parser"
	"github.com/alexgorbatchev/agent-watcher/scanner"
	"github.com/alexgorbatchev/agent-watcher/tailer"
)

// Harness identifies a supported agent harness.
type Harness string

const (
	HarnessClaudeCode      Harness = "claude-code"
	HarnessPi              Harness = "pi"
	HarnessCodex           Harness = "codex"
	HarnessOpencode        Harness = "opencode"
	defaultScanInterval            = 2 * time.Second
	defaultCursorRetention         = 24 * time.Hour
	defaultPruneInterval           = time.Hour
	defaultFlushInterval           = 2 * time.Second
)

// Event combines a parser event with the session that produced it.
type Event struct {
	parser.ParsedEvent
	Harness        Harness `json:"harness"`
	HarnessVersion string  `json:"harnessVersion"`
	SessionID      string  `json:"sessionId"`
	SubagentID     *string `json:"subagentId,omitempty"`
	PID            int     `json:"pid"`
}

// Handler accepts an event. Calls from different transcripts may run concurrently.
// Returning an error applies backpressure without advancing the transcript offset.
// The last event in each transcript line carries acknowledge; call it after all
// events from that line have been delivered, in transcript order. Lifecycle events
// have no acknowledgement. Every OpenCode database event carries its own
// acknowledgement. Accepted but unacknowledged events replay after restart;
// acknowledging before returning an error violates the delivery contract.
// Handlers must respect cancellation of ctx.
type Handler func(ctx context.Context, event Event, acknowledge func()) error

// Config controls discovery and checkpoint persistence. Empty paths use harness
// environment variables and default locations. Each concurrent Run needs its own
// CursorPath, OpencodeCheckpointPath and SessionStatePath. Run never changes
// environment variables.
type Config struct {
	Harnesses          []Harness
	ScanInterval       time.Duration
	ClaudeSessionsDirs []string
	ClaudeProjectsDir  string
	PiSessionsDir      string
	CodexSessionsDir   string
	OpencodeDBPath     string
	CursorPath         string
	// OpencodeCheckpointPath defaults to the resolved CursorPath + ".opencode".
	OpencodeCheckpointPath  string
	SessionStatePath        string
	CursorRetention         time.Duration
	CursorPruneInterval     time.Duration
	CursorFlushInterval     time.Duration
	ProcessSnapshotProvider scanner.ProcessSnapshotProvider
	Logger                  *slog.Logger
}

type session struct {
	Event
	path      string
	startedAt int64
	active    bool
	subagents []scanner.SubagentTranscript
}

type activeTail struct {
	tailer    *tailer.Tailer
	cancel    context.CancelFunc
	sessionID string
	harness   Harness
}

type runner struct {
	cfg         Config
	handler     Handler
	logger      *slog.Logger
	cursors     *tailer.CursorStore
	checkpoints *checkpointStore
	tracker     *scanner.SessionTracker
	registry    *tailer.WatcherRegistry
	claude      *scanner.ClaudeCodeScanner
	pi          *scanner.PiScanner
	codex       *scanner.CodexScanner
	opencode    *scanner.OpencodeScanner
	active      map[Harness]map[string]session
	tails       map[string]*activeTail
	subagents   map[string]map[string]bool
	wg          sync.WaitGroup
}

// Run blocks until ctx is cancelled or initialization fails. It joins its
// background workers and flushes checkpoints before returning. Normal shutdown
// returns nil; an already cancelled context returns its error without doing I/O.
func Run(ctx context.Context, cfg Config, handler Handler) error {
	if handler == nil {
		return fmt.Errorf("event handler is required")
	}
	if len(cfg.Harnesses) == 0 {
		cfg.Harnesses = []Harness{HarnessClaudeCode, HarnessPi, HarnessCodex, HarnessOpencode}
	}
	for _, harness := range cfg.Harnesses {
		switch harness {
		case HarnessClaudeCode, HarnessPi, HarnessCodex, HarnessOpencode:
		default:
			return fmt.Errorf("unsupported harness: %q", harness)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg.ScanInterval = positiveDuration(cfg.ScanInterval, defaultScanInterval)
	cfg.CursorRetention = positiveDuration(cfg.CursorRetention, defaultCursorRetention)
	cfg.CursorPruneInterval = positiveDuration(cfg.CursorPruneInterval, defaultPruneInterval)
	cfg.CursorFlushInterval = positiveDuration(cfg.CursorFlushInterval, defaultFlushInterval)
	if cfg.ProcessSnapshotProvider == nil {
		cfg.ProcessSnapshotProvider = scanner.NewLiveProcessSnapshotProvider()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	cursors, err := tailer.NewCursorStore(cfg.CursorPath)
	if err != nil {
		return fmt.Errorf("initializing cursor store: %w", err)
	}
	if cfg.OpencodeCheckpointPath == "" {
		path := cfg.CursorPath
		if path == "" {
			path = tailer.DefaultCursorPath()
		}
		cfg.OpencodeCheckpointPath = path + ".opencode"
	}
	checkpoints, err := newCheckpointStore(cfg.OpencodeCheckpointPath)
	if err != nil {
		return fmt.Errorf("initializing database checkpoints: %w", err)
	}
	tracker, err := scanner.NewSessionTracker(cfg.SessionStatePath)
	if err != nil {
		return fmt.Errorf("initializing session tracker: %w", err)
	}
	registry, err := tailer.NewWatcherRegistry()
	if err != nil {
		return fmt.Errorf("initializing watcher registry: %w", err)
	}
	r := &runner{
		cfg: cfg, handler: handler, logger: logger, cursors: cursors,
		tracker: tracker, registry: registry, checkpoints: checkpoints,
		claude:   scanner.NewClaudeCodeScanner(cfg.ClaudeSessionsDirs),
		pi:       scanner.NewPiScanner(cfg.PiSessionsDir, nil),
		codex:    scanner.NewCodexScanner(cfg.CodexSessionsDir, nil),
		opencode: scanner.NewOpencodeScanner(cfg.OpencodeDBPath, nil),
		active:   make(map[Harness]map[string]session), tails: make(map[string]*activeTail),
		subagents: make(map[string]map[string]bool),
	}
	r.claude.SetProjectsDir(cfg.ClaudeProjectsDir)
	for id, tracked := range tracker.GetAll() {
		if tracked.IsActive {
			harness := Harness(tracked.Harness)
			if r.active[harness] == nil {
				r.active[harness] = make(map[string]session)
			}
			r.active[harness][id] = session{
				Event: Event{Harness: harness, HarnessVersion: tracked.HarnessVersion,
					SessionID: id, PID: tracked.PID, ParsedEvent: parser.ParsedEvent{CWD: tracked.CWD}},
				startedAt: tracked.StartedAt, active: true,
			}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer r.shutdown(cancel)
	r.prune()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.persistPeriodically(ctx)
	}()
	r.scan(ctx)
	ticker := time.NewTicker(cfg.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.scan(ctx)
		}
	}
}

func positiveDuration(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}
