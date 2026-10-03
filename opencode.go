package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/alexgorbatchev/agent-parser"
	"github.com/alexgorbatchev/agent-watcher/scanner"
)

type databaseEvent struct {
	parser.ParsedEvent
	key       string
	immutable bool
}

func (r *runner) pollOpencode(ctx context.Context, s session) error {
	path := r.cfg.OpencodeDBPath
	if path == "" {
		path = scanner.DefaultOpencodeDBPath()
	}
	db, err := parser.OpenOpencodeDB(path)
	if err != nil {
		r.logger.Warn("database_open_failed", "err", err)
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			r.logger.Warn("database_close_failed", "err", err)
		}
	}()
	record, err := db.GetSessionByID(ctx, s.SessionID)
	if err != nil {
		r.logger.Warn("database_session_read_failed", "sessionId", s.SessionID, "err", err)
		return err
	}
	messages, err := db.GetMessages(ctx, s.SessionID)
	if err != nil {
		r.logger.Warn("database_messages_read_failed", "sessionId", s.SessionID, "err", err)
		return err
	}
	parts, err := db.GetParts(ctx, s.SessionID)
	if err != nil {
		r.logger.Warn("database_parts_read_failed", "sessionId", s.SessionID, "err", err)
		return err
	}
	// Timestamp ties must remain stable across database query plans and restarts.
	sort.SliceStable(messages, func(i, j int) bool {
		if messages[i].TimeCreated == messages[j].TimeCreated {
			return messages[i].ID < messages[j].ID
		}
		return messages[i].TimeCreated < messages[j].TimeCreated
	})
	sort.SliceStable(parts, func(i, j int) bool {
		if parts[i].TimeCreated == parts[j].TimeCreated {
			return parts[i].ID < parts[j].ID
		}
		return parts[i].TimeCreated < parts[j].TimeCreated
	})
	partsByMessage := make(map[string][]parser.OpencodePart)
	for _, part := range parts {
		partsByMessage[part.MessageID] = append(partsByMessage[part.MessageID], part)
	}
	for _, message := range messages {
		for _, parsed := range opencodeMessageEvents(record, message, partsByMessage[message.ID]) {
			if err := r.emitDatabaseEvent(ctx, s, parsed); err != nil {
				r.logger.Warn("database_event_emit_failed", "sessionId", s.SessionID, "err", err)
				return err
			}
		}
	}
	return nil
}

func (r *runner) emitDatabaseEvent(ctx context.Context, s session, parsed databaseEvent) error {
	data, err := json.Marshal(parsed.ParsedEvent)
	if err != nil {
		return fmt.Errorf("encoding database event: %w", err)
	}
	digest := sha256.Sum256(data)
	key := s.SessionID + ":" + parsed.key
	ack, reject, emit := r.checkpoints.reserve(key, hex.EncodeToString(digest[:]), parsed.immutable)
	if !emit {
		return nil
	}
	event := s.Event
	event.ParsedEvent = parsed.ParsedEvent
	if err := r.handler(ctx, event, ack); err != nil {
		reject()
		return err
	}
	return nil
}

func opencodeMessageEvents(record *parser.OpencodeSession, message parser.OpencodeMessage, parts []parser.OpencodePart) []databaseEvent {
	var events []databaseEvent
	appendParsed := func(key string, immutable bool, parsed []parser.ParsedEvent) {
		for _, event := range parsed {
			if event.EventType != "run_started" {
				events = append(events, databaseEvent{ParsedEvent: event, key: key + ":" + event.EventType, immutable: immutable})
			}
		}
	}
	if message.Data.Role == "user" {
		appendParsed("message:"+message.ID, false, parser.ReconstructSessionEvents(record, []parser.OpencodeMessage{message}, parts))
		return events
	}
	// The parser is an offline reader and can synthesize turn_end from provisional
	// cost/token fields. Parse parts without completion fields while streaming.
	streaming := message
	streaming.Data.Tokens, streaming.Data.Cost, streaming.Data.Finish, streaming.Data.Error = nil, nil, nil, nil
	for _, part := range parts {
		parsed := parser.ReconstructSessionEvents(record, []parser.OpencodeMessage{streaming}, []parser.OpencodePart{part})
		appendParsed("part:"+part.ID, false, parsed)
	}
	parsed := parser.ReconstructSessionEvents(record, []parser.OpencodeMessage{message}, parts)
	completed := message.Data.Time != nil && message.Data.Time.Completed != nil && *message.Data.Time.Completed > 0
	completed = completed || (message.Data.Finish != nil && *message.Data.Finish != "")
	for i, event := range parsed {
		if event.EventType == "turn_end" && completed {
			appendParsed("message:"+message.ID, true, []parser.ParsedEvent{event})
		}
		if event.EventType == "error" && i == len(parsed)-1 && (message.Data.Error != nil || (message.Data.Finish != nil && *message.Data.Finish == "error")) {
			appendParsed("message:"+message.ID, false, []parser.ParsedEvent{event})
		}
	}
	return events
}
