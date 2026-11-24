package tailer

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPreparePreservesCursorAndDefersDelivery(t *testing.T) {
	for _, tt := range []struct {
		name        string
		startAtEnd  bool
		acknowledge bool
		wantOffset  int64
		wantLines   []string
	}{
		{"from_start", false, false, 0, []string{"old", "history", "new"}},
		{"skip_history", true, false, 12, []string{"new"}},
		{"stored_cursor", true, true, 4, []string{"history", "new"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "transcript.jsonl")
			if err := os.WriteFile(path, []byte("old\nhistory\n"), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := NewCursorStore(filepath.Join(dir, "cursors.json"))
			if err != nil {
				t.Fatal(err)
			}
			registry, err := NewWatcherRegistry()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := registry.Close(); err != nil {
					t.Error(err)
				}
			})
			var lines []string
			newTail := func() *Tailer {
				tail, err := NewTailer(path, store, func(line []byte) error {
					lines = append(lines, string(line))
					return nil
				}, WithWatcherRegistry(registry))
				if err != nil {
					t.Fatal(err)
				}
				return tail
			}
			tail := newTail()
			if tt.acknowledge {
				if err := tail.Prepare(); err != nil {
					t.Fatal(err)
				}
				if err := tail.Commit(4); err != nil {
					t.Fatal(err)
				}
				tail = newTail()
			}
			if tt.startAtEnd {
				WithStartAtEnd()(tail)
			}
			if err := tail.Prepare(); err != nil {
				t.Fatal(err)
			}
			if len(lines) != 0 || tail.CurrentOffset() != tt.wantOffset {
				t.Fatalf("prepare delivered %v at offset %d, want no lines at %d", lines, tail.CurrentOffset(), tt.wantOffset)
			}
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteString("new\n")
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("append: %v; close: %v", writeErr, closeErr)
			}
			if err := tail.readAvailable(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(lines, tt.wantLines) {
				t.Fatalf("delivered %v, want %v", lines, tt.wantLines)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := tail.Prepare(); !os.IsNotExist(err) {
				t.Fatalf("missing file prepare = %v", err)
			}
		})
	}
}
