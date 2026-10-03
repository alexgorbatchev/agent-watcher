package scanner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractLastActiveTimestamp(t *testing.T) {
	t.Run("non-existent file returns error", func(t *testing.T) {
		_, err := ExtractLastActiveTimestamp("/path/does/not/exist/transcript.jsonl")
		if err == nil {
			t.Fatal("expected error for non-existent file, got nil")
		}
	})

	t.Run("empty file falls back to mtime", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "empty.jsonl")
		if err := os.WriteFile(filePath, []byte{}, 0644); err != nil {
			t.Fatalf("writing empty file: %v", err)
		}

		fi, err := os.Stat(filePath)
		if err != nil {
			t.Fatalf("stat empty file: %v", err)
		}
		expectedMtime := fi.ModTime().UnixMilli()

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != expectedMtime {
			t.Fatalf("ExtractLastActiveTimestamp(empty) = %d, want mtime %d", got, expectedMtime)
		}
	})

	t.Run("non-JSON file falls back to mtime", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "plain.log")
		content := "This is a plain text file without any JSON\nSecond line of plain text\n"
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("writing non-JSON file: %v", err)
		}

		fi, err := os.Stat(filePath)
		if err != nil {
			t.Fatalf("stat non-JSON file: %v", err)
		}
		expectedMtime := fi.ModTime().UnixMilli()

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != expectedMtime {
			t.Fatalf("ExtractLastActiveTimestamp(plain) = %d, want mtime %d", got, expectedMtime)
		}
	})

	t.Run("file with invalid timestamps falls back to mtime", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "invalid_ts.jsonl")
		content := strings.Join([]string{
			`{"timestamp":"not-a-valid-date"}`,
			`{"timestamp":null}`,
			`{"timestamp":-100}`,
			`{"type":"last-prompt","content":"no timestamp field"}`,
		}, "\n") + "\n"
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("writing invalid timestamp file: %v", err)
		}

		fi, err := os.Stat(filePath)
		if err != nil {
			t.Fatalf("stat invalid timestamp file: %v", err)
		}
		expectedMtime := fi.ModTime().UnixMilli()

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != expectedMtime {
			t.Fatalf("ExtractLastActiveTimestamp(invalid) = %d, want mtime %d", got, expectedMtime)
		}
	})

	t.Run("multi-line JSONL extracts latest tail entry", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "multiline.jsonl")

		t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
		t2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC).UnixMilli()
		t3 := time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC).UnixMilli()

		content := fmt.Sprintf(
			"{\"type\":\"message\",\"timestamp\":%d}\n{\"type\":\"message\",\"timestamp\":%d}\n{\"type\":\"message\",\"timestamp\":%d}\n",
			t1, t2, t3,
		)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("writing multiline file: %v", err)
		}

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != t3 {
			t.Fatalf("ExtractLastActiveTimestamp(multiline) = %d, want latest %d", got, t3)
		}
	})

	t.Run("multi-line JSONL with timestamp-less last line extracts preceding valid line", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "multiline_last_prompt.jsonl")

		t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
		t2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC).UnixMilli()

		content := fmt.Sprintf(
			"{\"type\":\"message\",\"timestamp\":%d}\n{\"type\":\"message\",\"timestamp\":%d}\n{\"type\":\"last-prompt\",\"content\":\"nav pointer\"}\n",
			t1, t2,
		)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("writing file: %v", err)
		}

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != t2 {
			t.Fatalf("ExtractLastActiveTimestamp = %d, want %d", got, t2)
		}
	})

	t.Run("truncated last line falls back to preceding valid line", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "crashed_write.jsonl")

		expectedTS := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
		content := fmt.Sprintf("{\"type\":\"assistant\",\"timestamp\":%d}\n{\"type\":\"assistant\",\"message\":{\"role\":\"assist", expectedTS)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("writing crashed write file: %v", err)
		}

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != expectedTS {
			t.Fatalf("ExtractLastActiveTimestamp = %d, want %d", got, expectedTS)
		}
	})

	t.Run("ISO-8601 timestamps parsed correctly", func(t *testing.T) {
		tests := []struct {
			name      string
			tsString  string
			wantMilli int64
		}{
			{
				name:      "RFC3339Nano UTC",
				tsString:  "2026-09-18T20:53:30.007Z",
				wantMilli: time.Date(2026, 9, 18, 20, 53, 30, 7000000, time.UTC).UnixMilli(),
			},
			{
				name:      "RFC3339 with offset",
				tsString:  "2026-09-18T22:53:30.007+02:00",
				wantMilli: time.Date(2026, 9, 18, 20, 53, 30, 7000000, time.UTC).UnixMilli(),
			},
			{
				name:      "RFC3339 standard Z",
				tsString:  "2026-09-18T20:53:30Z",
				wantMilli: time.Date(2026, 9, 18, 20, 53, 30, 0, time.UTC).UnixMilli(),
			},
			{
				name:      "ISO format without offset with millis",
				tsString:  "2026-09-18T20:53:30.007",
				wantMilli: time.Date(2026, 9, 18, 20, 53, 30, 7000000, time.UTC).UnixMilli(),
			},
			{
				name:      "ISO format without offset",
				tsString:  "2026-09-18T20:53:30",
				wantMilli: time.Date(2026, 9, 18, 20, 53, 30, 0, time.UTC).UnixMilli(),
			},
			{
				name:      "standard DateTime",
				tsString:  "2026-09-18 20:53:30",
				wantMilli: time.Date(2026, 9, 18, 20, 53, 30, 0, time.UTC).UnixMilli(),
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tmpDir := t.TempDir()
				filePath := filepath.Join(tmpDir, "iso.jsonl")
				content := fmt.Sprintf("{\"type\":\"message\",\"timestamp\":%q}\n", tt.tsString)
				if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
					t.Fatalf("writing file: %v", err)
				}

				got, err := ExtractLastActiveTimestamp(filePath)
				if err != nil {
					t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
				}
				if got != tt.wantMilli {
					t.Fatalf("ExtractLastActiveTimestamp(%s) = %d, want %d", tt.tsString, got, tt.wantMilli)
				}
			})
		}
	})

	t.Run("unix epoch milliseconds and seconds", func(t *testing.T) {
		tests := []struct {
			name      string
			line      string
			wantMilli int64
		}{
			{
				name:      "milliseconds numeric",
				line:      `{"type":"message","timestamp":1789851210007}`,
				wantMilli: 1789851210007,
			},
			{
				name:      "seconds numeric",
				line:      `{"type":"message","timestamp":1789851210}`,
				wantMilli: 1789851210000,
			},
			{
				name:      "milliseconds string",
				line:      `{"type":"message","timestamp":"1789851210007"}`,
				wantMilli: 1789851210007,
			},
			{
				name:      "seconds string",
				line:      `{"type":"message","timestamp":"1789851210"}`,
				wantMilli: 1789851210000,
			},
			{
				name:      "created_at field",
				line:      `{"type":"message","created_at":1789851210007}`,
				wantMilli: 1789851210007,
			},
			{
				name:      "createdAt field",
				line:      `{"type":"message","createdAt":"2026-09-18T20:53:30.007Z"}`,
				wantMilli: time.Date(2026, 9, 18, 20, 53, 30, 7000000, time.UTC).UnixMilli(),
			},
			{
				name:      "time field numeric",
				line:      `{"type":"message","time":1789851210007}`,
				wantMilli: 1789851210007,
			},
			{
				name:      "nested payload timestamp",
				line:      `{"type":"event_msg","payload":{"type":"agent_message","timestamp":"2026-06-12T16:08:36.123Z"}}`,
				wantMilli: time.Date(2026, 6, 12, 16, 8, 36, 123000000, time.UTC).UnixMilli(),
			},
			{
				name:      "nested time object created",
				line:      `{"type":"message","time":{"created":1789851210007}}`,
				wantMilli: 1789851210007,
			},
			{
				name:      "microseconds numeric",
				line:      `{"type":"message","timestamp":1789851210007000}`,
				wantMilli: 1789851210007,
			},
			{
				name:      "nanoseconds numeric",
				line:      `{"type":"message","timestamp":1789851210007000000}`,
				wantMilli: 1789851210007,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tmpDir := t.TempDir()
				filePath := filepath.Join(tmpDir, "epoch.jsonl")
				if err := os.WriteFile(filePath, []byte(tt.line+"\n"), 0644); err != nil {
					t.Fatalf("writing file: %v", err)
				}

				got, err := ExtractLastActiveTimestamp(filePath)
				if err != nil {
					t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
				}
				if got != tt.wantMilli {
					t.Fatalf("ExtractLastActiveTimestamp = %d, want %d", got, tt.wantMilli)
				}
			})
		}
	})

	t.Run("truncated lines at 256KB buffer boundary", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "large_boundary.jsonl")

		// Construct a file > 256KB where a line crosses the 256KB tail boundary.
		// Total size around 300KB.
		// 256KB = 262144 bytes.
		// Head: 40KB of padding lines.
		// Crossing line: starts before byte 40000, spans across byte 40000 to 50000.
		// Followed by valid lines in the tail.
		f, err := os.Create(filePath)
		if err != nil {
			t.Fatalf("creating file: %v", err)
		}

		// Write 50KB of prefix lines with an early timestamp
		earlyTS := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
		prefixLine := fmt.Sprintf("{\"type\":\"message\",\"timestamp\":%d,\"padding\":\"%s\"}\n", earlyTS, strings.Repeat("A", 1000))
		for i := 0; i < 50; i++ {
			if _, err := f.WriteString(prefixLine); err != nil {
				t.Fatalf("writing prefix: %v", err)
			}
		}

		// Write padding bytes to position exactly near 256KB boundary from end
		// Write 300KB of lines
		midTS := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
		midLine := fmt.Sprintf("{\"type\":\"message\",\"timestamp\":%d,\"padding\":\"%s\"}\n", midTS, strings.Repeat("B", 2000))
		for i := 0; i < 150; i++ {
			if _, err := f.WriteString(midLine); err != nil {
				t.Fatalf("writing mid: %v", err)
			}
		}

		// Now add the final line with the latest timestamp
		expectedLatest := time.Date(2026, 3, 1, 15, 30, 0, 0, time.UTC).UnixMilli()
		finalLine := fmt.Sprintf("{\"type\":\"message\",\"timestamp\":%d,\"msg\":\"final event\"}\n", expectedLatest)
		if _, err := f.WriteString(finalLine); err != nil {
			t.Fatalf("writing final line: %v", err)
		}
		_ = f.Close()

		fi, err := os.Stat(filePath)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Size() <= 256*1024 {
			t.Fatalf("file size %d <= 256KB, need > 256KB for boundary test", fi.Size())
		}

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != expectedLatest {
			t.Fatalf("ExtractLastActiveTimestamp(boundary) = %d, want latest %d", got, expectedLatest)
		}
	})

	t.Run("large file with single truncated line in tail buffer falls back to mtime", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "single_huge_line.jsonl")

		f, err := os.Create(filePath)
		if err != nil {
			t.Fatalf("creating file: %v", err)
		}

		// Write a 300KB file consisting of a single line without any newlines
		// The 256KB tail buffer will therefore only see a truncated segment without newlines.
		chunk := strings.Repeat("X", 300*1024)
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatalf("writing single huge line: %v", err)
		}
		_ = f.Close()

		fi, err := os.Stat(filePath)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		expectedMtime := fi.ModTime().UnixMilli()

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != expectedMtime {
			t.Fatalf("ExtractLastActiveTimestamp(huge line) = %d, want mtime %d", got, expectedMtime)
		}
	})

	t.Run("trailing marker records do not advance last-active timestamp", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "markers.jsonl")

		conversationalTS := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
		markerTS1 := conversationalTS + 10000
		markerTS2 := conversationalTS + 20000
		markerTS3 := conversationalTS + 30000
		markerTS4 := conversationalTS + 40000

		lines := []string{
			fmt.Sprintf(`{"type":"user","timestamp":%d,"message":{"role":"user","content":"hello"}}`, conversationalTS-5000),
			fmt.Sprintf(`{"type":"assistant","timestamp":%d,"message":{"role":"assistant","content":"hi"}}`, conversationalTS),
			fmt.Sprintf(`{"type":"last-prompt","timestamp":%d,"lastPrompt":"hello"}`, markerTS1),
			fmt.Sprintf(`{"type":"queue-operation","timestamp":%d,"operation":"enqueue"}`, markerTS2),
			fmt.Sprintf(`{"type":"file-history-delta","timestamp":%d,"trackingPath":"foo.txt"}`, markerTS3),
			fmt.Sprintf(`{"type":"pr-link","timestamp":%d,"prUrl":"https://github.com/..."}`, markerTS4),
		}
		content := strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("writing file: %v", err)
		}

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != conversationalTS {
			t.Fatalf("ExtractLastActiveTimestamp with trailing markers = %d, want conversational %d", got, conversationalTS)
		}
	})

	t.Run("oversized record greater than 256KB parsed via multi-chunk backward read", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "oversized_tail.jsonl")

		expectedTS := time.Date(2026, 7, 10, 15, 30, 0, 0, time.UTC).UnixMilli()
		prefixTS := expectedTS - 60000

		// Write a conversational prefix line, then an oversized conversational record (> 256KB)
		oversizedPayload := strings.Repeat("Z", 300*1024) // 300KB
		content := fmt.Sprintf(
			"{\"type\":\"user\",\"timestamp\":%d,\"content\":\"start\"}\n{\"type\":\"assistant\",\"timestamp\":%d,\"output\":%q}\n",
			prefixTS, expectedTS, oversizedPayload,
		)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("writing oversized file: %v", err)
		}

		// Ensure file mod time is set to a distinctly different time
		distinctMtime := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
		_ = os.Chtimes(filePath, distinctMtime, distinctMtime)

		fi, err := os.Stat(filePath)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Size() <= 256*1024 {
			t.Fatalf("file size %d <= 256KB, need > 256KB for oversized test", fi.Size())
		}

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != expectedTS {
			t.Fatalf("ExtractLastActiveTimestamp(oversized) = %d, want %d (mtime was %d)", got, expectedTS, distinctMtime.UnixMilli())
		}
	})

	t.Run("oversized trailing marker record skips backwards past 256KB to preceding conversational record", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "oversized_marker.jsonl")

		expectedTS := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC).UnixMilli()
		markerTS := expectedTS + 50000

		oversizedPayload := strings.Repeat("M", 300*1024)
		content := fmt.Sprintf(
			"{\"type\":\"assistant\",\"timestamp\":%d,\"message\":{\"role\":\"assistant\",\"content\":\"done\"}}\n{\"type\":\"last-prompt\",\"timestamp\":%d,\"padding\":%q}\n",
			expectedTS, markerTS, oversizedPayload,
		)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("writing oversized marker file: %v", err)
		}

		distinctMtime := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
		_ = os.Chtimes(filePath, distinctMtime, distinctMtime)

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != expectedTS {
			t.Fatalf("ExtractLastActiveTimestamp(oversized marker) = %d, want conversational %d", got, expectedTS)
		}
	})

	t.Run("start time fields do not advance last-active timestamp", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "start_times.jsonl")

		conversationalTS := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC).UnixMilli()
		futureStartTS := conversationalTS + 100000

		lines := []string{
			fmt.Sprintf(`{"type":"user","timestamp":%d,"message":{"role":"user","content":"work"}}`, conversationalTS),
			fmt.Sprintf(`{"type":"session","started_at":%d}`, futureStartTS),
			fmt.Sprintf(`{"type":"session_info","startedAt":%d}`, futureStartTS+1000),
			fmt.Sprintf(`{"type":"task_start","payload":{"start":%d}}`, futureStartTS+2000),
		}
		content := strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("writing start times file: %v", err)
		}

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != conversationalTS {
			t.Fatalf("ExtractLastActiveTimestamp with start times = %d, want conversational %d", got, conversationalTS)
		}
	})

	t.Run("event_type and payload.type marker records are excluded", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "more_markers.jsonl")

		expectedTS := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
		lines := []string{
			fmt.Sprintf(`{"type":"assistant","timestamp":%d,"content":"ok"}`, expectedTS),
			`{"type":12345}`,
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"queue-operation"},"timestamp":%d}`, expectedTS+1000),
			fmt.Sprintf(`{"event_type":"last-prompt","timestamp":%d}`, expectedTS+2000),
		}
		if err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
			t.Fatalf("writing file: %v", err)
		}

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		if got != expectedTS {
			t.Fatalf("ExtractLastActiveTimestamp(more markers) = %d, want %d", got, expectedTS)
		}
	})

	t.Run("parseTimestampValue unit variations", func(t *testing.T) {
		if ts, ok := parseTimestampValue(nil); ok || ts != 0 {
			t.Errorf("expected nil to return 0, false; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue(int64(1789851210007)); !ok || ts != 1789851210007 {
			t.Errorf("expected int64 to return 1789851210007, true; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue(int64(-50)); ok || ts != 0 {
			t.Errorf("expected negative int64 to return 0, false; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue(int(1789851210)); !ok || ts != 1789851210000 {
			t.Errorf("expected int to return 1789851210000, true; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue(json.Number("1789851210007")); !ok || ts != 1789851210007 {
			t.Errorf("expected json.Number to return 1789851210007, true; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue(json.Number("invalid")); ok || ts != 0 {
			t.Errorf("expected invalid json.Number to return 0, false; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue(true); ok || ts != 0 {
			t.Errorf("expected bool to return 0, false; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue(""); ok || ts != 0 {
			t.Errorf("expected empty string to return 0, false; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue(float64(50)); ok || ts != 0 {
			t.Errorf("expected small float to return 0, false; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue(float64(-100)); ok || ts != 0 {
			t.Errorf("expected negative float to return 0, false; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue("  2026-09-18T20:53:30Z  "); !ok || ts != 1789764810000 {
			t.Errorf("expected trimmed RFC3339 string to return 1789764810000, true; got %d, %v", ts, ok)
		}
		if ts, ok := parseTimestampValue("1789851210007.5"); !ok || ts != 1789851210007 {
			t.Errorf("expected float string to return 1789851210007, true; got %d, %v", ts, ok)
		}
	})

	t.Run("allowlist of conversational record types", func(t *testing.T) {
		allowlistTypes := []string{
			"user",
			"assistant",
			"message",
			"custom_message",
			"custom-message",
			"event_msg",
			"event-msg",
			"response_item",
			"response-item",
			"turn_start",
			"turn-start",
			"turn_end",
			"turn-end",
			"turn_context",
			"turn-context",
			"agent_message",
			"agent-message",
			"user_message",
			"user-message",
			"tool_call",
			"tool-call",
			"tool_result",
			"tool-result",
			"tool_use",
			"tool-use",
			"function_call",
			"function-call",
			"function_call_output",
			"function-call-output",
			"custom_tool_call",
			"custom-tool-call",
			"custom_tool_call_output",
			"custom-tool-call-output",
			"thinking",
			"question_prompt",
			"question-prompt",
			"error",
		}

		for _, recType := range allowlistTypes {
			t.Run("type_"+recType, func(t *testing.T) {
				tmpDir := t.TempDir()
				filePath := filepath.Join(tmpDir, "allowlist.jsonl")

				expectedTS := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
				var line string
				if strings.Contains(recType, "agent_message") || strings.Contains(recType, "user_message") {
					line = fmt.Sprintf(`{"type":"event_msg","timestamp":%d,"payload":{"type":%q,"message":"hi"}}`, expectedTS, recType)
				} else {
					line = fmt.Sprintf(`{"type":%q,"timestamp":%d}`, recType, expectedTS)
				}
				if err := os.WriteFile(filePath, []byte(line+"\n"), 0644); err != nil {
					t.Fatalf("writing file: %v", err)
				}

				got, err := ExtractLastActiveTimestamp(filePath)
				if err != nil {
					t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
				}
				if got != expectedTS {
					t.Errorf("expected timestamp %d for allowlisted type %q, got %d", expectedTS, recType, got)
				}
			})
		}

		nonAllowlistTypes := []string{
			"last-prompt",
			"last_prompt",
			"queue-operation",
			"queue_operation",
			"pr-link",
			"pr_link",
			"file-history-delta",
			"file_history_delta",
			"file-history-snapshot",
			"file_history_snapshot",
			"session",
			"session_meta",
			"session_info",
			"progress",
			"ai-title",
			"ai_title",
			"permission-mode",
			"worktree-state",
			"agent-setting",
			"heartbeat",
			"custom_unknown_type",
		}

		t.Run("non-allowlisted types are ignored", func(t *testing.T) {
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "non_allowlist.jsonl")

			conversationalTS := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
			lines := []string{
				fmt.Sprintf(`{"type":"user","timestamp":%d,"content":"hello"}`, conversationalTS),
			}
			futureTS := conversationalTS + 1000
			for _, nonType := range nonAllowlistTypes {
				lines = append(lines, fmt.Sprintf(`{"type":%q,"timestamp":%d}`, nonType, futureTS))
				lines = append(lines, fmt.Sprintf(`{"type":"event_msg","payload":{"type":%q},"timestamp":%d}`, nonType, futureTS+1))
				futureTS += 10
			}
			// Also add a bare timestamp record without type
			lines = append(lines, fmt.Sprintf(`{"timestamp":%d}`, futureTS))

			if err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
				t.Fatalf("writing file: %v", err)
			}

			got, err := ExtractLastActiveTimestamp(filePath)
			if err != nil {
				t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
			}
			if got != conversationalTS {
				t.Errorf("expected last-active timestamp %d from conversational record, got %d", conversationalTS, got)
			}
		})
	})

	t.Run("ExtractFirstActiveTimestamp", func(t *testing.T) {
		t.Run("non-existent file returns error", func(t *testing.T) {
			_, err := ExtractFirstActiveTimestamp("/path/does/not/exist/surely.jsonl")
			if err == nil {
				t.Error("expected error for non-existent file, got nil")
			}
		})

		t.Run("empty file falls back to mtime", func(t *testing.T) {
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "empty.jsonl")
			if err := os.WriteFile(filePath, []byte(""), 0644); err != nil {
				t.Fatalf("writing empty file: %v", err)
			}
			expectedMtime := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
			_ = os.Chtimes(filePath, expectedMtime, expectedMtime)

			got, err := ExtractFirstActiveTimestamp(filePath)
			if err != nil {
				t.Fatalf("ExtractFirstActiveTimestamp failed: %v", err)
			}
			if got != expectedMtime.UnixMilli() {
				t.Errorf("expected mtime %d, got %d", expectedMtime.UnixMilli(), got)
			}
		})

		t.Run("multi-line transcript extracts first timestamp ignoring later lines", func(t *testing.T) {
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "first_ts.jsonl")

			firstTS := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC).UnixMilli()
			secondTS := time.Date(2026, 6, 1, 9, 5, 0, 0, time.UTC).UnixMilli()
			thirdTS := time.Date(2026, 6, 1, 9, 10, 0, 0, time.UTC).UnixMilli()

			lines := []string{
				fmt.Sprintf(`{"type":"user","timestamp":%d,"message":"start subagent task"}`, firstTS),
				fmt.Sprintf(`{"type":"assistant","timestamp":%d,"message":"working"}`, secondTS),
				fmt.Sprintf(`{"type":"assistant","timestamp":%d,"message":"finished"}`, thirdTS),
			}
			if err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
				t.Fatalf("writing file: %v", err)
			}

			got, err := ExtractFirstActiveTimestamp(filePath)
			if err != nil {
				t.Fatalf("ExtractFirstActiveTimestamp failed: %v", err)
			}
			if got != firstTS {
				t.Errorf("expected first timestamp %d, got %d", firstTS, got)
			}
		})

		t.Run("first line with started_at or startedAt extracts start time", func(t *testing.T) {
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "started_at.jsonl")

			startTS := time.Date(2026, 4, 1, 8, 30, 0, 0, time.UTC).UnixMilli()
			laterTS := startTS + 60000

			lines := []string{
				fmt.Sprintf(`{"type":"session","started_at":%d}`, startTS),
				fmt.Sprintf(`{"type":"assistant","timestamp":%d,"message":"hi"}`, laterTS),
			}
			if err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
				t.Fatalf("writing file: %v", err)
			}

			got, err := ExtractFirstActiveTimestamp(filePath)
			if err != nil {
				t.Fatalf("ExtractFirstActiveTimestamp failed: %v", err)
			}
			if got != startTS {
				t.Errorf("expected start time %d, got %d", startTS, got)
			}
		})

		t.Run("blank lines or invalid first line advances to first valid timestamped line", func(t *testing.T) {
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "skips_blank.jsonl")

			validTS := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC).UnixMilli()
			lines := []string{
				"",
				"   ",
				`{"not_a_json_with_timestamp": true}`,
				fmt.Sprintf(`{"type":"user","timestamp":%d}`, validTS),
				fmt.Sprintf(`{"type":"assistant","timestamp":%d}`, validTS+5000),
			}
			if err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
				t.Fatalf("writing file: %v", err)
			}

			got, err := ExtractFirstActiveTimestamp(filePath)
			if err != nil {
				t.Fatalf("ExtractFirstActiveTimestamp failed: %v", err)
			}
			if got != validTS {
				t.Errorf("expected valid first timestamp %d, got %d", validTS, got)
			}
		})

		t.Run("transcript with no timestamps falls back to mtime", func(t *testing.T) {
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "no_timestamps.jsonl")

			lines := []string{
				`{"type":"info","text":"no timestamp here"}`,
				`{"type":"debug","data":"still none"}`,
			}
			if err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
				t.Fatalf("writing file: %v", err)
			}
			expectedMtime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
			_ = os.Chtimes(filePath, expectedMtime, expectedMtime)

			got, err := ExtractFirstActiveTimestamp(filePath)
			if err != nil {
				t.Fatalf("ExtractFirstActiveTimestamp failed: %v", err)
			}
			if got != expectedMtime.UnixMilli() {
				t.Errorf("expected mtime %d, got %d", expectedMtime.UnixMilli(), got)
			}
		})
	})

	t.Run("len(remainder) > maxLineBufferSize resets remainder to nil", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "huge_unbroken_line.jsonl")

		expectedTS := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
		prefixLine := fmt.Sprintf("{\"type\":\"assistant\",\"timestamp\":%d}\n", expectedTS)

		// Create a file where prefix line is followed by > 8MB (maxLineBufferSize) of un-newline content
		f, err := os.Create(filePath)
		if err != nil {
			t.Fatalf("create file: %v", err)
		}
		if _, err := f.WriteString(prefixLine); err != nil {
			_ = f.Close()
			t.Fatalf("write prefix: %v", err)
		}

		// Write 8.5MB of data without a newline
		hugeChunk := bytes.Repeat([]byte("X"), 512*1024) // 512KB
		for i := 0; i < 17; i++ {                        // 17 * 512KB = 8.5MB
			if _, err := f.Write(hugeChunk); err != nil {
				_ = f.Close()
				t.Fatalf("write huge chunk: %v", err)
			}
		}
		_ = f.Close()

		distinctMtime := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
		_ = os.Chtimes(filePath, distinctMtime, distinctMtime)

		got, err := ExtractLastActiveTimestamp(filePath)
		if err != nil {
			t.Fatalf("ExtractLastActiveTimestamp failed: %v", err)
		}
		// The 8.5MB unbroken chunk exceeded maxLineBufferSize, reset remainder to nil,
		// and backward read reached the prefixLine with expectedTS!
		if got != expectedTS {
			t.Errorf("expected timestamp %d from prefix line after remainder reset, got %d", expectedTS, got)
		}
	})
}
