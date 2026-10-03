package scanner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type TrackedSession struct {
	SessionID      string `json:"sessionId"`
	Harness        string `json:"harness"`
	PID            int    `json:"pid"`
	CWD            string `json:"cwd"`
	HarnessVersion string `json:"harnessVersion"`
	StartedAt      int64  `json:"startedAt"`
	IsActive       bool   `json:"isActive"`
}

type SessionTracker struct {
	mu       sync.Mutex
	filePath string
	sessions map[string]TrackedSession
	dirty    bool
}

func DefaultSessionStatePath() string {
	if cacheHome := os.Getenv("XDG_CACHE_HOME"); cacheHome != "" {
		return filepath.Join(cacheHome, "agent-watcher", "observer-sessions.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agent-watcher", "observer-sessions.json")
	}
	return filepath.Join(home, ".cache", "agent-watcher", "observer-sessions.json")
}

func NewSessionTracker(filePath string) (*SessionTracker, error) {
	if filePath == "" {
		filePath = DefaultSessionStatePath()
	}

	st := &SessionTracker{
		filePath: filePath,
		sessions: make(map[string]TrackedSession),
	}

	data, err := os.ReadFile(filePath)
	if err == nil {
		_ = json.Unmarshal(data, &st.sessions)
	}

	return st, nil
}

func (st *SessionTracker) GetAll() map[string]TrackedSession {
	st.mu.Lock()
	defer st.mu.Unlock()

	copied := make(map[string]TrackedSession, len(st.sessions))
	for k, v := range st.sessions {
		copied[k] = v
	}
	return copied
}

func (st *SessionTracker) Set(sessionID string, s TrackedSession) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if existing, ok := st.sessions[sessionID]; ok && existing == s {
		return
	}
	st.sessions[sessionID] = s
	st.dirty = true
}

func (st *SessionTracker) Delete(sessionID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.sessions[sessionID]; ok {
		delete(st.sessions, sessionID)
		st.dirty = true
	}
}

// IsDirty returns true if there are unsaved modifications in the session tracker.
func (st *SessionTracker) IsDirty() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.dirty
}

func (st *SessionTracker) Save() error {
	st.mu.Lock()
	defer st.mu.Unlock()

	if !st.dirty {
		return nil
	}

	dir := filepath.Dir(st.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.Marshal(st.sessions)
	if err != nil {
		return err
	}

	tmpFile := st.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}

	if err := os.Rename(tmpFile, st.filePath); err != nil {
		_ = os.Remove(tmpFile)
		return err
	}

	st.dirty = false
	return nil
}
