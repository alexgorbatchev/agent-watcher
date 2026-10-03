package scanner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	tailChunkSize     = 256 * 1024      // 256KB chunk size for backward scanning
	maxLineBufferSize = 8 * 1024 * 1024 // 8MB safety bound for single un-newline chunk accumulation
)

var (
	primaryTimestampFields = []string{
		"timestamp",
		"created_at",
		"createdAt",
		"time",
		"date",
	}

	containerFields = []string{
		"payload",
		"data",
		"message",
		"time",
	}

	nestedTimestampFields = []string{
		"timestamp",
		"created_at",
		"createdAt",
		"created",
		"time",
		"date",
		"completed",
	}

	conversationalRecordTypes = map[string]bool{
		"user":                    true,
		"assistant":               true,
		"message":                 true,
		"custom_message":          true,
		"custom-message":          true,
		"event_msg":               true,
		"event-msg":               true,
		"response_item":           true,
		"response-item":           true,
		"turn_start":              true,
		"turn-start":              true,
		"turn_end":                true,
		"turn-end":                true,
		"turn_context":            true,
		"turn-context":            true,
		"agent_message":           true,
		"agent-message":           true,
		"user_message":            true,
		"user-message":            true,
		"tool_call":               true,
		"tool-call":               true,
		"tool_result":             true,
		"tool-result":             true,
		"tool_use":                true,
		"tool-use":                true,
		"function_call":           true,
		"function-call":           true,
		"function_call_output":    true,
		"function-call-output":    true,
		"custom_tool_call":        true,
		"custom-tool-call":        true,
		"custom_tool_call_output": true,
		"custom-tool-call-output": true,
		"thinking":                true,
		"question_prompt":         true,
		"question-prompt":         true,
		"error":                   true,
	}
)

// ExtractLastActiveTimestamp reads backwards in chunks from the end of a transcript file
// scanning for valid JSON lines carrying a recognizable timestamp from a conversational
// or turn-generating record per harness specs (e.g. user, assistant, message, event_msg,
// response_item, turn_start, turn_end, etc.). Any record not in the allowlist is ignored.
// If no valid conversational timestamp is found or BOF is reached, it falls back to the file's ModTime().
func ExtractLastActiveTimestamp(filePath string) (int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("opening transcript file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	stat, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat transcript file: %w", err)
	}

	mtime := stat.ModTime().UnixMilli()
	fileSize := stat.Size()
	if fileSize == 0 {
		return mtime, nil
	}

	cursor := fileSize
	var remainder []byte

	for cursor > 0 {
		readSize := int64(tailChunkSize)
		if cursor < readSize {
			readSize = cursor
		}
		readStart := cursor - readSize

		chunk := make([]byte, readSize)
		n, err := file.ReadAt(chunk, readStart)
		if err != nil && err != io.EOF {
			return 0, fmt.Errorf("reading transcript chunk at offset %d: %w", readStart, err)
		}
		chunk = chunk[:n]

		var data []byte
		if len(remainder) > 0 {
			data = make([]byte, len(chunk)+len(remainder))
			copy(data, chunk)
			copy(data[len(chunk):], remainder)
		} else {
			data = chunk
		}

		cursor = readStart

		lines := bytes.Split(data, []byte("\n"))
		var startIdx int
		if cursor > 0 {
			// When seeking into the middle of a file, the first slice before the first \n
			// is truncated at the buffer boundary. Retain it as remainder for the next backward chunk.
			remainder = lines[0]
			if len(remainder) > maxLineBufferSize {
				// Prevent unbounded memory growth if file has no newlines
				remainder = nil
			}
			startIdx = 1
		} else {
			// Reached beginning of file; all slices including lines[0] are complete lines.
			remainder = nil
			startIdx = 0
		}

		for i := len(lines) - 1; i >= startIdx; i-- {
			line := bytes.TrimSpace(lines[i])
			if len(line) == 0 {
				continue
			}

			var entry map[string]interface{}
			if err := json.Unmarshal(line, &entry); err != nil {
				continue
			}

			if !isConversationalRecord(entry) {
				continue
			}

			if ts, ok := extractTimestampFromMap(entry); ok {
				return ts, nil
			}
		}
	}

	return mtime, nil
}

// ExtractFirstActiveTimestamp reads from the beginning of a transcript file
// and returns the timestamp of the first timestamped line.
// If no valid timestamp is found, it falls back to the file's ModTime().
func ExtractFirstActiveTimestamp(filePath string) (int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("opening transcript file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	stat, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat transcript file: %w", err)
	}

	mtime := stat.ModTime().UnixMilli()
	if stat.Size() == 0 {
		return mtime, nil
	}

	reader := bufio.NewReader(file)
	for {
		line, rErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var entry map[string]interface{}
				if err := json.Unmarshal(trimmed, &entry); err == nil {
					if ts, ok := extractFirstTimestampFromMap(entry); ok && ts > 0 {
						return ts, nil
					}
				}
			}
		}
		if rErr != nil {
			break
		}
	}

	return mtime, nil
}

func extractFirstTimestampFromMap(entry map[string]interface{}) (int64, bool) {
	if ts, ok := extractTimestampFromMap(entry); ok && ts > 0 {
		return ts, true
	}

	for _, field := range []string{"started_at", "startedAt", "start"} {
		if val, exists := entry[field]; exists {
			if ts, ok := parseTimestampValue(val); ok && ts > 0 {
				return ts, true
			}
		}
	}

	for _, containerKey := range containerFields {
		if container, exists := entry[containerKey].(map[string]interface{}); exists {
			for _, field := range []string{"started_at", "startedAt", "start"} {
				if val, exists := container[field]; exists {
					if ts, ok := parseTimestampValue(val); ok && ts > 0 {
						return ts, true
					}
				}
			}
		}
	}

	return 0, false
}

func isConversationalType(t string) bool {
	clean := strings.ToLower(strings.TrimSpace(t))
	if clean == "" {
		return false
	}
	if conversationalRecordTypes[clean] {
		return true
	}
	return conversationalRecordTypes[strings.ReplaceAll(clean, "_", "-")]
}

func isConversationalRecord(entry map[string]interface{}) bool {
	if payload, ok := entry["payload"].(map[string]interface{}); ok {
		if pType, exists := payload["type"]; exists {
			if s, ok := pType.(string); ok {
				return isConversationalType(s)
			}
		}
	}

	for _, key := range []string{"type", "event_type", "event"} {
		if val, exists := entry[key]; exists {
			if s, ok := val.(string); ok && isConversationalType(s) {
				return true
			}
		}
	}
	return false
}

func extractTimestampFromMap(entry map[string]interface{}) (int64, bool) {
	for _, field := range primaryTimestampFields {
		if val, exists := entry[field]; exists {
			if ts, ok := parseTimestampValue(val); ok {
				return ts, true
			}
		}
	}

	for _, containerKey := range containerFields {
		if container, exists := entry[containerKey].(map[string]interface{}); exists {
			for _, field := range nestedTimestampFields {
				if val, exists := container[field]; exists {
					if ts, ok := parseTimestampValue(val); ok {
						return ts, true
					}
				}
			}
		}
	}

	return 0, false
}

func parseTimestampValue(v interface{}) (int64, bool) {
	if v == nil {
		return 0, false
	}

	switch val := v.(type) {
	case float64:
		return normalizeNumericTimestamp(val)
	case int64:
		return normalizeNumericTimestamp(float64(val))
	case int:
		return normalizeNumericTimestamp(float64(val))
	case json.Number:
		if f, err := val.Float64(); err == nil {
			return normalizeNumericTimestamp(f)
		}
		return 0, false
	case string:
		val = strings.TrimSpace(val)
		if val == "" {
			return 0, false
		}

		if t, err := time.Parse(time.RFC3339Nano, val); err == nil {
			if ts := t.UnixMilli(); ts > 0 {
				return ts, true
			}
		}
		if t, err := time.Parse(time.RFC3339, val); err == nil {
			if ts := t.UnixMilli(); ts > 0 {
				return ts, true
			}
		}
		if t, err := time.Parse("2006-01-02T15:04:05.999999999", val); err == nil {
			if ts := t.UnixMilli(); ts > 0 {
				return ts, true
			}
		}
		if t, err := time.Parse("2006-01-02T15:04:05", val); err == nil {
			if ts := t.UnixMilli(); ts > 0 {
				return ts, true
			}
		}
		if t, err := time.Parse(time.DateTime, val); err == nil {
			if ts := t.UnixMilli(); ts > 0 {
				return ts, true
			}
		}

		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return normalizeNumericTimestamp(f)
		}
		return 0, false
	default:
		return 0, false
	}
}

func normalizeNumericTimestamp(v float64) (int64, bool) {
	if v <= 0 {
		return 0, false
	}
	if v >= 1e17 {
		return int64(v / 1e6), true
	}
	if v >= 1e14 {
		return int64(v / 1e3), true
	}
	if v >= 1e11 {
		return int64(v), true
	}
	if v >= 1e9 {
		return int64(v * 1000), true
	}
	return 0, false
}
