package scanner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// maxCodexTurnScan bounds the backward search through a turn's output. A longer
// turn reports unknown rather than reading the whole rollout on every scan.
const maxCodexTurnScan = 32 * 1024 * 1024

var codexTurnEventNames = [][]byte{[]byte("task_started"), []byte("turn_started"), []byte("task_complete"), []byte("turn_complete"), []byte("turn_aborted")}

// codexTurn is the last turn boundary in a rollout. Codex has no equivalent of
// Claude Code's session status, so this is the only record of whether it is working.
type codexTurn struct {
	state SessionState
	at    int64
}

type rawCodexEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type string `json:"type"`
	} `json:"payload"`
}

func readCodexTurn(path string) (codexTurn, error) {
	file, err := os.Open(path)
	if err != nil {
		return codexTurn{}, fmt.Errorf("opening codex rollout: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	stat, err := file.Stat()
	if err != nil {
		return codexTurn{}, fmt.Errorf("stat codex rollout: %w", err)
	}

	turn := codexTurn{state: SessionStateUnknown}
	err = scanLinesBackward(file, stat.Size(), maxCodexTurnScan, func(line []byte) bool {
		// Tool output is most of a rollout and costly to decode.
		if !mentionsCodexTurnEvent(line) {
			return true
		}
		var event rawCodexEvent
		if err := json.Unmarshal(line, &event); err != nil || event.Type != "event_msg" {
			return true
		}
		switch event.Payload.Type {
		case "task_started", "turn_started":
			turn.state = SessionStateWorking
		case "task_complete", "turn_complete", "turn_aborted":
			turn.state = SessionStateIdle
		default:
			return true
		}
		turn.at = parseRFC3339Time(event.Timestamp)
		return false
	})
	if err != nil {
		return codexTurn{}, err
	}
	return turn, nil
}

func mentionsCodexTurnEvent(line []byte) bool {
	for _, name := range codexTurnEventNames {
		if bytes.Contains(line, name) {
			return true
		}
	}
	return false
}
