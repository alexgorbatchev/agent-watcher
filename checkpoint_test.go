package watcher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointAcknowledgementAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.json")
	s, err := newCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _, emit := s.reserve("part", "first", false)
	if !emit {
		t.Fatal("new event suppressed")
	}
	if _, _, emit := s.reserve("part", "first", false); emit {
		t.Fatal("pending event duplicated")
	}
	second, _, emit := s.reserve("part", "second", false)
	if !emit {
		t.Fatal("updated event suppressed")
	}
	second()
	first()
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	s, err = newCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, emit := s.reserve("part", "second", false); emit {
		t.Fatal("late acknowledgement overwrote newer checkpoint")
	}
	if _, _, emit := s.reserve("part", "second", false); emit {
		t.Fatal("acknowledged event duplicated after restart")
	}
	_, reject, emit := s.reserve("rejected", "event", false)
	if !emit {
		t.Fatal("new event suppressed")
	}
	reject()
	ack, _, emit := s.reserve("rejected", "event", false)
	if !emit {
		t.Fatal("rejected event could not retry")
	}
	ack()
	immutable, _, emit := s.reserve("usage", "initial", true)
	if !emit {
		t.Fatal("new completed usage suppressed")
	}
	if _, _, emit := s.reserve("usage", "changed", true); emit {
		t.Fatal("pending usage duplicated")
	}
	immutable()
	if _, _, emit := s.reserve("usage", "changed", true); emit {
		t.Fatal("acknowledged usage duplicated")
	}
}

func TestCheckpointErrors(t *testing.T) {
	t.Run("invalid checkpoint", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "checkpoints.json")
		writeTestFile(t, path, "broken")
		if _, err := newCheckpointStore(path); err == nil {
			t.Fatal("invalid checkpoint accepted")
		}
	})
	t.Run("checkpoint is directory", func(t *testing.T) {
		if _, err := newCheckpointStore(t.TempDir()); err == nil {
			t.Fatal("directory accepted")
		}
	})
	t.Run("flush preserves dirty state on failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "checkpoints.json")
		s, err := newCheckpointStore(path)
		if err != nil {
			t.Fatal(err)
		}
		ack, _, _ := s.reserve("event", "value", false)
		ack()
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := s.flush(); err == nil {
			t.Fatal("flush over directory succeeded")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := s.flush(); err != nil {
			t.Fatal(err)
		}
		restored, err := newCheckpointStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, emit := restored.reserve("event", "value", false); emit {
			t.Fatal("failed flush lost checkpoint")
		}
	})
}
