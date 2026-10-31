package watcher

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// Session describes a live agent session at one moment. Times are Unix milliseconds.
type Session struct {
	Harness        Harness `json:"harness"`
	HarnessVersion string  `json:"harnessVersion"`
	SessionID      string  `json:"sessionId"`
	PID            int     `json:"pid"`
	CWD            string  `json:"cwd"`
	Name           string  `json:"name,omitempty"`
	StartedAt      int64   `json:"startedAt"`
	LastActivity   int64   `json:"lastActivity"`
	Subagent       bool    `json:"subagent"`
	TranscriptPath string  `json:"transcriptPath,omitempty"`
}

// Snapshotter lists live sessions on demand. Reusing one keeps the scanners' file
// caches, so repeated snapshots cost less than repeated calls to Snapshot. It is
// safe for concurrent use.
type Snapshotter struct {
	cfg       Config
	discovery *discovery
	mu        sync.Mutex
}

// NewSnapshotter validates cfg. Snapshots read only Harnesses, the harness paths
// and ProcessSnapshotProvider; they need no cursor or session state paths.
func NewSnapshotter(cfg Config) (*Snapshotter, error) {
	cfg, err := cfg.withDiscoveryDefaults()
	if err != nil {
		return nil, err
	}
	return &Snapshotter{cfg: cfg, discovery: newDiscovery(cfg)}, nil
}

// Snapshot returns the sessions running now, ordered by harness and session ID,
// without reading transcript history or writing any state. A harness that fails
// does not hide the others: the sessions found are returned with the joined error.
// Claude Code subagents are not listed, because their transcripts remain after
// they finish and nothing distinguishes the ones still running.
func (s *Snapshotter) Snapshot(ctx context.Context) ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discovery.startTick(s.cfg.ProcessSnapshotProvider)
	var sessions []Session
	var errs []error
	for _, harness := range s.cfg.Harnesses {
		if err := ctx.Err(); err != nil {
			return sessions, err
		}
		found, err := s.discovery.discover(ctx, harness)
		if err != nil {
			errs = append(errs, fmt.Errorf("scanning %s: %w", harness, err))
			continue
		}
		for _, session := range found {
			if session.active {
				sessions = append(sessions, session.snapshot())
			}
		}
	}
	slices.SortFunc(sessions, func(a, b Session) int {
		return cmp.Or(cmp.Compare(a.Harness, b.Harness), cmp.Compare(a.SessionID, b.SessionID))
	})
	return sessions, errors.Join(errs...)
}

// Snapshot is the one-shot form of Snapshotter.Snapshot.
func Snapshot(ctx context.Context, cfg Config) ([]Session, error) {
	snapshotter, err := NewSnapshotter(cfg)
	if err != nil {
		return nil, err
	}
	return snapshotter.Snapshot(ctx)
}

func (s session) snapshot() Session {
	return Session{Harness: s.Harness, HarnessVersion: s.HarnessVersion, SessionID: s.SessionID,
		PID: s.PID, CWD: s.CWD, Name: s.name, StartedAt: s.startedAt,
		LastActivity: cmp.Or(s.lastActive, s.Timestamp), Subagent: s.SubagentID != nil,
		TranscriptPath: s.path}
}
