package watcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Database events have revisions rather than byte offsets. Their checkpoints
// stay separate from the transcript cursor format used by existing applications.
type eventCheckpoint struct {
	Fingerprint string `json:"fingerprint"`
	Sequence    uint64 `json:"sequence"`
}

type checkpointStore struct {
	mu        sync.Mutex
	path      string
	committed map[string]eventCheckpoint
	pending   map[string]map[string]uint64
	sequence  uint64
	dirty     bool
}

func newCheckpointStore(path string) (*checkpointStore, error) {
	s := &checkpointStore{path: path, committed: make(map[string]eventCheckpoint), pending: make(map[string]map[string]uint64)}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading database event checkpoints: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &s.committed); err != nil {
			return nil, fmt.Errorf("decoding database event checkpoints: %w", err)
		}
	}
	if s.committed == nil {
		s.committed = make(map[string]eventCheckpoint)
	}
	for _, checkpoint := range s.committed {
		if checkpoint.Sequence > s.sequence {
			s.sequence = checkpoint.Sequence
		}
	}
	return s, nil
}

func (s *checkpointStore) reserve(key, fingerprint string, immutable bool) (func(), func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.committed[key]; ok && (immutable || previous.Fingerprint == fingerprint) {
		return nil, nil, false
	}
	if versions := s.pending[key]; len(versions) > 0 && (immutable || versions[fingerprint] > 0) {
		return nil, nil, false
	}
	s.sequence++
	sequence := s.sequence
	if s.pending[key] == nil {
		s.pending[key] = make(map[string]uint64)
	}
	s.pending[key][fingerprint] = sequence
	ack := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if sequence > s.committed[key].Sequence {
			s.committed[key] = eventCheckpoint{Fingerprint: fingerprint, Sequence: sequence}
			s.dirty = true
		}
		delete(s.pending[key], fingerprint)
		if len(s.pending[key]) == 0 {
			delete(s.pending, key)
		}
	}
	reject := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.pending[key], fingerprint)
		if len(s.pending[key]) == 0 {
			delete(s.pending, key)
		}
	}
	return ack, reject, true
}

func (s *checkpointStore) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	data, err := json.Marshal(s.committed)
	if err != nil {
		return fmt.Errorf("encoding database checkpoints: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("creating database checkpoint directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".opencode-checkpoints-*")
	if err != nil {
		return fmt.Errorf("creating database checkpoint file: %w", err)
	}
	defer func() {
		_ = os.Remove(file.Name()) // Rename consumes the file on success; remove it on failure.
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close() // preserve the write error
		return fmt.Errorf("writing database checkpoints: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing database checkpoints: %w", err)
	}
	if err := os.Rename(file.Name(), s.path); err != nil {
		return fmt.Errorf("renaming database checkpoints: %w", err)
	}
	s.dirty = false
	return nil
}
