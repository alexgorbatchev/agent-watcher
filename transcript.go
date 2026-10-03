package watcher

import (
	"context"
	"fmt"

	"github.com/alexgorbatchev/agent-parser"
	"github.com/alexgorbatchev/agent-watcher/tailer"
)

func parseLineForHarness(harness Harness, line []byte, cwd string, ccParser *parser.ClaudeCodeParser, codexParser *parser.CodexParser) ([]parser.ParsedEvent, error) {
	switch harness {
	case HarnessClaudeCode:
		if ccParser == nil {
			ccParser = parser.NewClaudeCodeParser()
		}
		return ccParser.ParseLine(line)
	case HarnessPi:
		return parser.ParsePiLine(line, cwd)
	case HarnessCodex:
		if codexParser == nil {
			codexParser = parser.NewCodexParser()
		}
		return codexParser.ParseLine(line)
	default:
		return nil, fmt.Errorf("unsupported harness type: %q", harness)
	}
}

func (r *runner) attach(ctx context.Context, s session) {
	if s.path == "" || r.tails[s.path] != nil {
		return
	}
	tailCtx, cancel := context.WithCancel(ctx)
	reader := &transcriptReader{runner: r, session: s}
	options := tailer.WithWatcherRegistry(r.registry)
	t, err := tailer.NewTailerWithOffsetHandler(s.path, r.cursors, func(line []byte, offset int64) error {
		return reader.process(tailCtx, line, offset)
	}, options)
	if err != nil {
		cancel()
		r.logger.Warn("tailer_attach_failed", "path", s.path, "err", err)
		return
	}
	reader.tailer = t
	r.tails[s.path] = &activeTail{tailer: t, cancel: cancel, sessionID: s.SessionID, harness: s.Harness}
	attrs := []any{"harness", s.Harness, "sessionId", s.SessionID, "pid", s.PID, "cwd", s.CWD, "path", s.path}
	if s.SubagentID != nil {
		attrs = append(attrs, "subagentId", *s.SubagentID)
	}
	r.logger.Info("session_attached", attrs...)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := t.Start(tailCtx); err != nil && tailCtx.Err() == nil {
			r.logger.Warn("tailer_failed", "path", s.path, "err", err)
		}
	}()
}

type transcriptReader struct {
	runner     *runner
	session    session
	tailer     *tailer.Tailer
	queue      deliveryQueue
	batch      *transcriptBatch
	generation uint64
	claude     *parser.ClaudeCodeParser
	codex      *parser.CodexParser
}

func (r *transcriptReader) process(ctx context.Context, line []byte, offset int64) error {
	generation, commit := r.tailer.Checkpoint(offset)
	if r.batch == nil || r.batch.offset != offset || r.batch.generation != generation {
		if r.generation != generation {
			r.claude, r.codex = parser.NewClaudeCodeParser(), parser.NewCodexParser()
			r.generation = generation
		}
		batch := &transcriptBatch{offset: offset, generation: generation, acknowledge: r.queue.add(generation, commit)}
		parsed, err := parseLineForHarness(r.session.Harness, line, r.session.CWD, r.claude, r.codex)
		if err != nil {
			r.runner.logger.Warn("transcript_line_parse_failed", "harness", r.session.Harness, "path", r.session.path, "err", err)
			return batch.acknowledge()
		}
		for _, parsedEvent := range parsed {
			if parsedEvent.EventType == "run_started" {
				continue
			}
			event := r.session.Event
			event.ParsedEvent = parsedEvent
			if event.CWD == "" {
				event.CWD = r.session.CWD
			}
			batch.events = append(batch.events, event)
		}
		if len(batch.events) == 0 {
			return batch.acknowledge()
		}
		r.batch = batch
	}
	batch := r.batch
	for batch.next < len(batch.events) {
		var acknowledge func()
		if batch.next == len(batch.events)-1 {
			acknowledge = func() {
				if err := batch.acknowledge(); err != nil {
					r.runner.logger.Warn("cursor_commit_error", "err", err)
				}
			}
		}
		if err := r.runner.handler(ctx, batch.events[batch.next], acknowledge); err != nil {
			return err
		}
		batch.next++
	}
	r.batch = nil
	return nil
}

func (r *runner) detach(harness Harness, sessionID string) {
	for path, active := range r.tails {
		if active.harness == harness && active.sessionID == sessionID {
			active.cancel()
			active.tailer.Stop()
			delete(r.tails, path)
		}
	}
}
