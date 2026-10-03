package watcher

import (
	parser "github.com/alexgorbatchev/agent-parser"
	"strings"
	"testing"
)

func TestParserDispatchForHarnessTypes(t *testing.T) {
	tests := []struct {
		name        string
		harness     Harness
		line        []byte
		cwd         string
		wantErr     bool
		errContains string
	}{
		{
			name:    "claude-code valid line",
			harness: HarnessClaudeCode,
			line:    []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`),
			wantErr: false,
		},
		{
			name:    "pi valid line",
			harness: HarnessPi,
			line:    []byte(`{"type":"session","id":"pi-sess-1","cwd":"/path"}`),
			cwd:     "/path",
			wantErr: false,
		},
		{
			name:    "codex valid line",
			harness: HarnessCodex,
			line:    []byte(`{"type":"session_meta","payload":{"id":"codex-1","cwd":"/path"}}`),
			wantErr: false,
		},
		{
			name:        "opencode in line tailer returns error",
			harness:     HarnessOpencode,
			line:        []byte(`{}`),
			wantErr:     true,
			errContains: "unsupported harness type",
		},
		{
			name:        "unknown harness returns error",
			harness:     Harness("unknown"),
			line:        []byte(`{}`),
			wantErr:     true,
			errContains: "unsupported harness type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, pErr := parseLineForHarness(tt.harness, tt.line, tt.cwd, nil, nil)

			if (pErr != nil) != tt.wantErr {
				t.Fatalf("parseLineForHarness(%v) error = %v, wantErr %v", tt.harness, pErr, tt.wantErr)
			}
			if tt.wantErr && tt.errContains != "" {
				if pErr == nil || !strings.Contains(pErr.Error(), tt.errContains) {
					t.Errorf("error %v does not contain %q", pErr, tt.errContains)
				}
			}
			if !tt.wantErr && pErr == nil && len(events) == 0 {
				t.Errorf("expected at least 1 parsed event for valid line")
			}
		})
	}

	t.Run("codex parser preserves state across lines", func(t *testing.T) {
		codexParser := parser.NewCodexParser()
		line1 := []byte(`{
			"type": "event_msg",
			"timestamp": "2026-06-12T16:08:43.000Z",
			"payload": {
				"type": "token_count",
				"info": { "total_token_usage": { "total_tokens": 100 } },
				"rate_limits": { "remaining_tokens": 0, "reset_tokens": "45s" }
			}
		}`)
		_, err := parseLineForHarness(HarnessCodex, line1, "", nil, codexParser)
		if err != nil {
			t.Fatalf("unexpected error line 1: %v", err)
		}

		line2 := []byte(`{
			"type": "event_msg",
			"timestamp": "2026-06-12T16:08:44.000Z",
			"payload": {
				"type": "error",
				"message": "Rate limit reached",
				"codex_error_info": "RateLimitExceeded"
			}
		}`)
		evs, err := parseLineForHarness(HarnessCodex, line2, "", nil, codexParser)
		if err != nil {
			t.Fatalf("unexpected error line 2: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("expected 1 event, got %d", len(evs))
		}
		if evs[0].Data.RetryAfterSeconds == nil || *evs[0].Data.RetryAfterSeconds != 44 {
			t.Fatalf("expected RetryAfterSeconds=44 (45s discounted by 1s) from preserved state, got %v", evs[0].Data.RetryAfterSeconds)
		}
	})
}
