package scanner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/agent-parser"
)

func TestNativeActivityTimestamps(t *testing.T) {
	stamp := "2026-01-02T03:04:05.123+02:00"
	want := time.Date(2026, 1, 2, 1, 4, 5, 123000000, time.UTC).UnixMilli()
	for _, harness := range []struct {
		name     string
		parse    func([]byte) (int64, bool, error)
		activity string
		metadata string
	}{
		{"pi branch", parser.PiActivityTimestamp, `{"type":"branch_summary","timestamp":%q,"fromId":"m1","summary":"continued"}`, `{"type":"session","timestamp":"2026-01-01T00:00:00Z"}`},
		{"pi message", parser.PiActivityTimestamp, `{"type":"message","id":"m1","parentId":null,"timestamp":%q,"message":{"role":"user","content":"task","timestamp":1769904000000}}`, `{"type":"session_info","timestamp":"2026-01-01T00:00:00Z","name":"task"}`},
		{"codex", parser.CodexActivityTimestamp, `{"type":"event_msg","timestamp":%q,"payload":{"type":"task_complete","completed_at":1}}`, `{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z"}`},
		{"codex tool", parser.CodexActivityTimestamp, `{"type":"response_item","timestamp":%q,"payload":{"type":"function_call_output","call_id":"c1","output":"passed"}}`, `{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z"}`},
		{"claude", parser.ClaudeCodeActivityTimestamp, `{"type":"progress","timestamp":%q,"data":{"type":"agent_progress","agentId":"child"}}`, `{"type":"queue-operation","timestamp":"2026-01-01T00:00:00Z"}`},
	} {
		t.Run(harness.name, func(t *testing.T) {
			valid := fmt.Sprintf(harness.activity, stamp)
			for _, invalid := range []string{
				`{"type":"turn_start","timestamp":"2026-02-01T00:00:00Z"}`,
				`{"type":"question_prompt","timestamp":"2026-02-01T00:00:00Z"}`,
				`{"type":"event-msg","timestamp":"2026-02-01T00:00:00Z","payload":{"type":"agent_message"}}`,
				strings.Replace(valid, stamp, "2026-02-01T00:00:00", 1),
				strings.Replace(valid, fmt.Sprintf("%q", stamp), "1769904000", 1),
				strings.Replace(valid, fmt.Sprintf("%q", stamp), "1769904000000", 1),
				strings.Replace(valid, fmt.Sprintf("%q", stamp), "1769904000000000", 1),
				strings.Replace(valid, fmt.Sprintf("%q", stamp), "1769904000000000000", 1),
				`{"type":"message","message":{"timestamp":1769904000000}}`,
				`{"type":"assistant","created_at":"2026-02-01T00:00:00Z"}`,
				`{"type":"event_msg","timestamp":"2026-02-01T00:00:00Z","payload":{"type":"question_prompt"}}`,
				`{"type":"message","timestamp":`,
			} {
				path := writeActivityTranscript(t, harness.metadata+"\n"+valid+"\n"+invalid)
				got, err := ExtractLastActiveTimestamp(context.Background(), path, harness.parse)
				if err != nil || got != want {
					t.Errorf("tail %s: got %d, %v; want %d", invalid, got, err, want)
				}
				got, err = ExtractFirstActiveTimestamp(context.Background(), path, harness.parse)
				if err != nil || got != want {
					t.Errorf("first activity: got %d, %v; want %d", got, err, want)
				}
			}
		})
	}
}

func writeActivityTranscript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}
