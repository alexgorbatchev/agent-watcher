package tailer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Cursor struct {
	Key        string `json:"key"` // e.g. "device:inode"
	Path       string `json:"path"`
	Offset     int64  `json:"offset"`
	Size       int64  `json:"size"`
	Inode      uint64 `json:"inode"`
	Device     uint64 `json:"device"`
	PrefixHash string `json:"prefixHash"`
	UpdatedAt  int64  `json:"updatedAt"`
}

type CursorStore struct {
	mu                   sync.RWMutex
	saveMu               sync.Mutex
	filePath             string
	cursors              map[string]*Cursor
	dirty                bool
	mutationGen          uint64
	saveCount            int64
	beforeSaveCommitHook func()
}

var userHomeDirFunc = os.UserHomeDir

func DefaultCursorPath() string {
	if cacheHome := os.Getenv("XDG_CACHE_HOME"); cacheHome != "" {
		return filepath.Join(cacheHome, "agent-watcher", "observer-cursors.json")
	}
	home, err := userHomeDirFunc()
	if err != nil {
		return filepath.Join(os.TempDir(), "agent-watcher", "observer-cursors.json")
	}
	return filepath.Join(home, ".cache", "agent-watcher", "observer-cursors.json")
}

func NewCursorStore(filePath string) (*CursorStore, error) {
	if filePath == "" {
		filePath = DefaultCursorPath()
	}

	cs := &CursorStore{
		filePath: filePath,
		cursors:  make(map[string]*Cursor),
	}

	if err := cs.load(); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("loading cursor store: %w", err)
		}
	}

	return cs, nil
}

func (cs *CursorStore) load() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	data, err := os.ReadFile(cs.filePath)
	if err != nil {
		return err
	}

	var cursors map[string]*Cursor
	if err := json.Unmarshal(data, &cursors); err != nil {
		return fmt.Errorf("parsing cursor store json: %w", err)
	}

	if cursors != nil {
		cs.cursors = cursors
	}
	return nil
}

func (cs *CursorStore) Get(key string) (*Cursor, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	c, ok := cs.cursors[key]
	if !ok {
		return nil, false
	}
	// return copy
	cCopy := *c
	return &cCopy, true
}

func (cs *CursorStore) Set(c *Cursor) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cCopy := *c
	if cCopy.UpdatedAt == 0 {
		cCopy.UpdatedAt = time.Now().UnixMilli()
	}
	existing, ok := cs.cursors[c.Key]
	if !ok || existing == nil || existing.Offset != cCopy.Offset || existing.Size != cCopy.Size || existing.Inode != cCopy.Inode || existing.Device != cCopy.Device || existing.PrefixHash != cCopy.PrefixHash || existing.Path != cCopy.Path {
		cs.dirty = true
		cs.mutationGen++
	}
	cs.cursors[c.Key] = &cCopy
}

// Delete removes the cursor associated with key from the store.
func (cs *CursorStore) Delete(key string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, ok := cs.cursors[key]; ok {
		delete(cs.cursors, key)
		cs.dirty = true
		cs.mutationGen++
	}
}

// IsDirty returns true if there are unsaved modifications in the cursor store.
func (cs *CursorStore) IsDirty() bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.dirty
}

func defaultFileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return true // keep entry on permission or transient I/O errors until age retention expires
}

// Prune removes cursor entries whose file on disk no longer exists or whose
// UpdatedAt is older than olderThan (if olderThan > 0). It returns the number of
// pruned entries.
func (cs *CursorStore) Prune(olderThan time.Duration) int {
	return cs.PruneWithFileExists(olderThan, defaultFileExists)
}

// PruneWithFileExists removes cursor entries using a custom fileExists predicate.
// It checks age expiration and missing files without holding the store lock during
// file system operations.
func (cs *CursorStore) PruneWithFileExists(olderThan time.Duration, fileExists func(path string) bool) int {
	var cutoff int64
	checkAge := olderThan > 0
	if checkAge {
		cutoff = time.Now().Add(-olderThan).UnixMilli()
	}

	type pruneCandidate struct {
		key       string
		updatedAt int64
	}
	var candidates []pruneCandidate

	cs.mu.RLock()
	type entry struct {
		key       string
		path      string
		updatedAt int64
	}
	entries := make([]entry, 0, len(cs.cursors))
	for key, c := range cs.cursors {
		if c == nil {
			candidates = append(candidates, pruneCandidate{key: key})
			continue
		}
		entries = append(entries, entry{key: key, path: c.Path, updatedAt: c.UpdatedAt})
	}
	cs.mu.RUnlock()

	for _, e := range entries {
		if checkAge && e.updatedAt < cutoff {
			candidates = append(candidates, pruneCandidate{key: e.key, updatedAt: e.updatedAt})
			continue
		}
		if fileExists != nil && !fileExists(e.path) {
			candidates = append(candidates, pruneCandidate{key: e.key, updatedAt: e.updatedAt})
		}
	}

	if len(candidates) == 0 {
		return 0
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()
	pruned := 0
	for _, cand := range candidates {
		c, ok := cs.cursors[cand.key]
		if !ok {
			continue
		}
		if c == nil {
			delete(cs.cursors, cand.key)
			pruned++
			continue
		}
		if c.UpdatedAt == cand.updatedAt {
			delete(cs.cursors, cand.key)
			pruned++
		}
	}
	if pruned > 0 {
		cs.dirty = true
		cs.mutationGen++
	}
	return pruned
}

func (cs *CursorStore) Save() error {
	cs.saveMu.Lock()
	defer cs.saveMu.Unlock()
	cs.saveCount++

	cs.mu.RLock()
	snapGen := cs.mutationGen
	data, err := json.Marshal(cs.cursors)
	cs.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshaling cursors: %w", err)
	}

	dir := filepath.Dir(cs.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating cursor directory %q: %w", dir, err)
	}

	tmpFile := fmt.Sprintf("%s.tmp.%d", cs.filePath, time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("writing cursor temp file: %w", err)
	}

	if err := os.Rename(tmpFile, cs.filePath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("renaming cursor file: %w", err)
	}

	if cs.beforeSaveCommitHook != nil {
		cs.beforeSaveCommitHook()
	}

	cs.mu.Lock()
	if cs.mutationGen == snapGen {
		cs.dirty = false
	}
	cs.mu.Unlock()

	return nil
}

// Flush persists cursor offsets to disk only if there are unsaved changes.
func (cs *CursorStore) Flush() error {
	cs.mu.RLock()
	dirty := cs.dirty
	cs.mu.RUnlock()
	if !dirty {
		return nil
	}
	return cs.Save()
}

// StartAutoFlush starts a background goroutine that periodically flushes dirty cursors to disk
// until ctx is cancelled. An optional onError callback can be provided to handle flush errors.
func (cs *CursorStore) StartAutoFlush(ctx context.Context, interval time.Duration, onError ...func(err error)) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	var errHandler func(err error)
	if len(onError) > 0 && onError[0] != nil {
		errHandler = onError[0]
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := cs.Flush(); err != nil && errHandler != nil {
					errHandler(err)
				}
			}
		}
	}()
}

func (cs *CursorStore) SaveCount() int64 {
	cs.saveMu.Lock()
	defer cs.saveMu.Unlock()
	return cs.saveCount
}

func ShouldResetCursor(c *Cursor, currentIno uint64, currentSize int64, currentHash string) bool {
	if c == nil {
		return false
	}
	// Trigger 1: Inode change
	if currentIno != 0 && c.Inode != 0 && c.Inode != currentIno {
		return true
	}
	// Trigger 2: Size shrunk below recorded offset
	if currentSize < c.Offset {
		return true
	}
	// Trigger 3: Prefix SHA-256 mismatch (when size >= 512)
	if c.PrefixHash != "" && currentHash != "" && c.PrefixHash != currentHash {
		return true
	}
	return false
}

func ComputePrefixHash(file io.ReaderAt, size int64) (string, error) {
	if size < 512 {
		return "", nil
	}
	buf := make([]byte, 512)
	n, err := file.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	if n < 512 {
		return "", nil
	}
	sum := sha256.Sum256(buf[:n])
	return hex.EncodeToString(sum[:]), nil
}
