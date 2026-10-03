package tailer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointIgnoresAcknowledgementsFromReplacedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	store, err := NewCursorStore(filepath.Join(dir, "cursors.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old line\n"), 0600); err != nil {
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
	var tail *Tailer
	var acknowledgements []func() error
	tail, err = NewTailerWithOffsetHandler(path, store, func(_ []byte, offset int64) error {
		_, ack := tail.Checkpoint(offset)
		acknowledgements = append(acknowledgements, ack)
		return nil
	}, WithWatcherRegistry(registry))
	if err != nil {
		t.Fatal(err)
	}
	if err := tail.readAvailable(); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement.jsonl")
	if err := os.WriteFile(replacement, []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := tail.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if err := acknowledgements[0](); err != nil {
		t.Fatal(err)
	}
	if tail.CommittedOffset() != 0 {
		t.Fatal("old acknowledgement advanced replacement cursor")
	}
	if err := acknowledgements[1](); err != nil {
		t.Fatal(err)
	}
	if tail.CommittedOffset() != 4 {
		t.Fatalf("new acknowledgement did not commit: %d", tail.CommittedOffset())
	}
}
