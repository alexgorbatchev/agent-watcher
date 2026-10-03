package watcher

import (
	"context"
	"time"

	"github.com/alexgorbatchev/agent-parser"
	"github.com/alexgorbatchev/agent-watcher/scanner"
)

func (r *runner) scan(ctx context.Context) {
	provider := scanner.NewTickProcessSnapshotProvider(r.cfg.ProcessSnapshotProvider)
	r.claude.SetProcessSnapshotProvider(provider)
	r.pi.SetProcessSnapshotProvider(provider)
	r.codex.SetProcessSnapshotProvider(provider)
	r.opencode.SetProcessSnapshotProvider(provider)
	for _, harness := range r.cfg.Harnesses {
		sessions, err := r.discover(ctx, harness)
		if err != nil {
			r.logger.Warn("scan_failed", "harness", harness, "err", err)
			continue
		}
		r.reconcile(ctx, harness, sessions)
	}
	if err := r.tracker.Save(); err != nil {
		r.logger.Warn("session_tracker_save_error", "err", err)
	}
}

func (r *runner) discover(ctx context.Context, harness Harness) ([]session, error) {
	var sessions []session
	switch harness {
	case HarnessClaudeCode:
		found, err := r.claude.Scan()
		if err != nil {
			return nil, err
		}
		for _, desc := range found {
			s := session{Event: Event{Harness: harness, HarnessVersion: desc.HarnessVersion,
				SessionID: desc.SessionID, PID: desc.PID, ParsedEvent: parser.ParsedEvent{CWD: desc.CWD}},
				path: desc.TranscriptPath, startedAt: desc.StartedAt, active: true}
			if desc.NameSource == "custom" && desc.Name != "" {
				s.Data.Title = &desc.Name
			}
			s.Timestamp = startTimestamp(desc.StartedAt, 0)
			if desc.TranscriptPath != "" {
				if last, err := scanner.ExtractLastActiveTimestamp(desc.TranscriptPath); err == nil {
					s.Timestamp = startTimestamp(desc.StartedAt, last)
				}
			}
			s.subagents, err = r.claude.DiscoverSubagents(desc)
			if err != nil {
				r.logger.Warn("subagent_scan_failed", "sessionId", desc.SessionID, "err", err)
			}
			sessions = append(sessions, s)
		}
	case HarnessPi:
		found, err := r.pi.Scan(ctx)
		if err != nil {
			return nil, err
		}
		for _, desc := range found {
			sessions = append(sessions, session{Event: Event{Harness: harness, HarnessVersion: desc.HarnessVersion,
				SessionID: desc.SessionID, PID: desc.PID, ParsedEvent: parser.ParsedEvent{CWD: desc.CWD,
					Timestamp: startTimestamp(desc.StartedAt, desc.LastActive)}},
				path: desc.TranscriptPath, startedAt: desc.StartedAt, active: desc.IsActive})
		}
	case HarnessCodex:
		found, err := r.codex.Scan(ctx)
		if err != nil {
			return nil, err
		}
		for _, desc := range found {
			s := session{Event: Event{Harness: harness, HarnessVersion: desc.HarnessVersion,
				SessionID: desc.SessionID, PID: desc.PID, ParsedEvent: parser.ParsedEvent{CWD: desc.CWD,
					Timestamp: startTimestamp(desc.StartedAt, desc.LastActive)}},
				path: desc.TranscriptPath, startedAt: desc.StartedAt, active: desc.IsActive}
			if desc.IsSubagent && desc.ParentThreadID != "" {
				s.SubagentID = &desc.SessionID
			}
			sessions = append(sessions, s)
		}
	case HarnessOpencode:
		found, err := r.opencode.Scan(ctx)
		if err != nil {
			return nil, err
		}
		for _, desc := range found {
			s := session{Event: Event{Harness: harness, HarnessVersion: desc.HarnessVersion,
				SessionID: desc.SessionID, PID: desc.PID, ParsedEvent: parser.ParsedEvent{CWD: desc.CWD,
					Timestamp: startTimestamp(desc.StartedAt, 0)}},
				startedAt: desc.StartedAt, active: desc.IsActive}
			if desc.IsSubagent && desc.ParentID != "" {
				s.SubagentID = &desc.SessionID
			}
			if desc.Title != "" {
				s.Data.Title = &desc.Title
			}
			sessions = append(sessions, s)
		}
	}
	return sessions, nil
}

func startTimestamp(startedAt, lastActive int64) int64 {
	now := time.Now().UnixMilli()
	if lastActive > 0 && lastActive < now {
		return lastActive
	}
	if startedAt > 0 {
		return startedAt
	}
	return now
}

func (r *runner) reconcile(ctx context.Context, harness Harness, sessions []session) {
	previous := r.active[harness]
	next := make(map[string]session)
	for _, s := range sessions {
		if !s.active {
			if harness == HarnessOpencode && previous[s.SessionID].active {
				if err := r.pollOpencode(ctx, s); err != nil {
					next[s.SessionID] = previous[s.SessionID]
				}
			}
			continue
		}
		if !previous[s.SessionID].active {
			if err := r.emitLifecycle(ctx, s.Event, "run_started"); err != nil {
				continue
			}
		}
		next[s.SessionID] = s
		r.tracker.Set(s.SessionID, scanner.TrackedSession{SessionID: s.SessionID,
			Harness: string(harness), PID: s.PID, CWD: s.CWD, HarnessVersion: s.HarnessVersion,
			StartedAt: s.startedAt, IsActive: true})
		r.attach(ctx, s)
		r.attachSubagents(ctx, s)
		if harness == HarnessOpencode {
			_ = r.pollOpencode(ctx, s) // Read/delivery failures are logged and retried next tick.
		}
	}
	for id, old := range previous {
		if next[id].active {
			continue
		}
		event := old.Event
		event.Timestamp = time.Now().UnixMilli()
		event.Data = parser.EventData{}
		if err := r.emitLifecycle(ctx, event, "run_exited"); err != nil {
			next[id] = old
			continue
		}
		r.detach(harness, id)
		delete(r.subagents, id)
		r.tracker.Delete(id)
	}
	r.active[harness] = next
}

func (r *runner) emitLifecycle(ctx context.Context, event Event, eventType string) error {
	event.EventType = eventType
	if err := r.handler(ctx, event, nil); err != nil {
		r.logger.Warn("lifecycle_emit_failed", "harness", event.Harness, "sessionId", event.SessionID, "err", err)
		return err
	}
	return nil
}

func (r *runner) attachSubagents(ctx context.Context, parent session) {
	for _, sub := range parent.subagents {
		s := parent
		s.path = sub.TranscriptPath
		s.SubagentID = &sub.SubagentID
		s.Data = parser.EventData{}
		seen := r.subagents[parent.SessionID]
		if seen == nil {
			seen = make(map[string]bool)
			r.subagents[parent.SessionID] = seen
		}
		if !seen[sub.SubagentID] {
			s.Timestamp = time.Now().UnixMilli()
			if first, err := scanner.ExtractFirstActiveTimestamp(sub.TranscriptPath); err == nil && first > 0 && first < s.Timestamp {
				s.Timestamp = first
			}
			if s.startedAt > 0 && s.Timestamp < s.startedAt {
				s.Timestamp = s.startedAt
			}
			if err := r.emitLifecycle(ctx, s.Event, "run_started"); err != nil {
				continue
			}
			seen[sub.SubagentID] = true
		}
		r.attach(ctx, s)
	}
}
