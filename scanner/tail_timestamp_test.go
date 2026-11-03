package scanner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/agent-parser"
)

func TestActivityFileReaders(t *testing.T) {
	for _, reader := range []struct {
		name string
		read func(context.Context, string, func([]byte) (int64, bool, error)) (int64, error)
	}{
		{"first", ExtractFirstActiveTimestamp},
		{"last", ExtractLastActiveTimestamp},
	} {
		t.Run(reader.name, func(t *testing.T) {
			for _, content := range []string{"", "\n \nnot-json\n", `{"type":"session","timestamp":"2026-01-01T00:00:00Z"}`, `{"type":"message","timestamp":"invalid"}`} {
				path := writeActivityTranscript(t, content)
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				got, err := reader.read(context.Background(), path, parser.PiActivityTimestamp)
				if err != nil || got != info.ModTime().UnixMilli() {
					t.Fatalf("no activity %q: got %d, %v; want mtime %d", content, got, err, info.ModTime().UnixMilli())
				}
			}
			for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
				if _, err := reader.read(context.Background(), path, parser.PiActivityTimestamp); err == nil {
					t.Fatalf("expected file error for %s", path)
				}
			}
			path := writeActivityTranscript(t, `{"type":"message","timestamp":"2026-01-01T00:00:00Z"}`+"\n")
			t.Run("pre-canceled", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				got, err := reader.read(ctx, path, func([]byte) (int64, bool, error) { t.Fatal("parser called after cancellation"); return 0, false, nil })
				if got != 0 || !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled: %d, %v", got, err)
				}
			})
			for _, found := range []bool{false, true} {
				t.Run(fmt.Sprintf("cancel-during-parse-%t", found), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					calls := 0
					got, err := reader.read(ctx, path, func([]byte) (int64, bool, error) { calls++; cancel(); return 1, found, nil })
					if calls != 1 || got != 0 || !errors.Is(err, context.Canceled) {
						t.Fatalf("canceled: %d, %v; calls %d", got, err, calls)
					}
				})
			}
			// File position, not the greatest clock value, defines first/last.
			content := "\n" + `{"type":"message","timestamp":"2026-01-02T00:00:00Z"}` + "\n \n" + `{"type":"message","timestamp":"2026-01-01T00:00:00Z"}`
			path = writeActivityTranscript(t, content)
			want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
			if reader.name == "first" {
				want += int64((24 * time.Hour) / time.Millisecond)
			}
			got, err := reader.read(context.Background(), path, parser.PiActivityTimestamp)
			if err != nil || got != want {
				t.Fatalf("file order: %d, %v; want %d", got, err, want)
			}
		})
	}
}

func TestActivityBackwardChunkBoundaries(t *testing.T) {
	stamp := "2026-01-02T00:00:00Z"
	want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).UnixMilli()
	valid := fmt.Sprintf(`{"type":"message","timestamp":%q}`, stamp)
	largeValid := fmt.Sprintf(`{"type":"message","timestamp":%q,"message":{"role":"assistant","content":"%s"}}`, stamp, strings.Repeat("A", 2*tailChunkSize))
	marker := `{"type":"session_info","timestamp":"2099-01-01T00:00:00Z","name":"` + strings.Repeat("X", 2*tailChunkSize) + `"}`
	for _, tt := range []struct {
		name, content string
		found         bool
	}{
		{"line crosses chunks", largeValid + "\n" + marker + "\n", true},
		{"valid EOF without newline", marker + "\n" + largeValid, true},
		{"truncated final record", valid + "\n" + `{"type":"message","timestamp":`, true},
		{"oversized unbroken tail", valid + "\n" + strings.Repeat("X", maxLineBufferSize+tailChunkSize), true},
		{"oversized unbroken file", strings.Repeat("X", maxLineBufferSize+tailChunkSize), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeActivityTranscript(t, tt.content)
			expected := want
			if !tt.found {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				expected = info.ModTime().UnixMilli()
			}
			got, err := ExtractLastActiveTimestamp(context.Background(), path, parser.PiActivityTimestamp)
			if err != nil || got != expected {
				t.Fatalf("got %d, %v; want %d", got, err, expected)
			}
		})
	}
}

func TestBackwardReaderError(t *testing.T) {
	file, err := os.Open(writeActivityTranscript(t, "line"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := scanLinesBackward(file, 4, 0, func([]byte) bool { t.Fatal("visited after read error"); return false }); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read error = %v, want closed file", err)
	}
}
