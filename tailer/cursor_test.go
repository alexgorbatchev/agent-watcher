package tailer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCursorStore(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")

	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore() unexpected error = %v", err)
	}

	testKey := "123:456"
	c := &Cursor{
		Key:        testKey,
		Path:       "/path/to/session.jsonl",
		Offset:     1024,
		Size:       2048,
		Inode:      456,
		Device:     123,
		PrefixHash: "abcdef123456",
		UpdatedAt:  100000,
	}

	store.Set(c)

	if err := store.Save(); err != nil {
		t.Fatalf("store.Save() unexpected error = %v", err)
	}

	// Reload in a new store instance
	store2, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore() reload error = %v", err)
	}

	got, ok := store2.Get(testKey)
	if !ok {
		t.Fatalf("store2.Get(%q) not found", testKey)
	}

	if got.Offset != 1024 || got.PrefixHash != "abcdef123456" || got.Inode != 456 {
		t.Errorf("got cursor %+v, mismatch", got)
	}

	// Non-existent key
	if _, ok := store2.Get("nonexistent-key"); ok {
		t.Errorf("expected not found for nonexistent key")
	}
}

func TestCursorStoreSaveError(t *testing.T) {
	tmpDir := t.TempDir()
	blockingFile := filepath.Join(tmpDir, "file_as_dir")
	_ = os.WriteFile(blockingFile, []byte("blocker"), 0600)

	badStore := &CursorStore{
		filePath: filepath.Join(blockingFile, "sub", "cursors.json"),
		cursors:  make(map[string]*Cursor),
	}

	if err := badStore.Save(); err == nil {
		t.Errorf("expected error when saving to invalid dir path")
	}

	// Target is an existing directory -> Rename fails
	badStoreDir := &CursorStore{
		filePath: tmpDir,
		cursors:  make(map[string]*Cursor),
	}
	if err := badStoreDir.Save(); err == nil {
		t.Errorf("expected error when saving over an existing directory")
	}

	// Target directory is read-only -> WriteFile fails
	roDir := filepath.Join(tmpDir, "readonly")
	if err := os.Mkdir(roDir, 0555); err == nil {
		roStore := &CursorStore{
			filePath: filepath.Join(roDir, "cursors.json"),
			cursors:  make(map[string]*Cursor),
		}
		if err := roStore.Save(); err == nil || !strings.Contains(err.Error(), "writing cursor temp file") {
			t.Errorf("expected 'writing cursor temp file' error, got %v", err)
		}
		_ = os.Chmod(roDir, 0755)
	}
}

type dummyFileInfo struct {
	os.FileInfo
}

func (d dummyFileInfo) Sys() any {
	return "not-stat-t"
}

func TestGetFileInfoStat(t *testing.T) {
	_, _, err := getFileInfoStat(dummyFileInfo{})
	if err == nil {
		t.Errorf("expected error when Sys() cannot be cast to *syscall.Stat_t")
	}
}

func TestNewCursorStoreCorruptedFile(t *testing.T) {
	tmpDir := t.TempDir()
	badFile := filepath.Join(tmpDir, "corrupted-cursors.json")
	_ = os.WriteFile(badFile, []byte("{invalid-json"), 0600)

	_, err := NewCursorStore(badFile)
	if err == nil {
		t.Errorf("expected error for corrupted cursor store file")
	}
}

func TestCursorResetTriggers(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.jsonl")

	// Create content of >= 512 bytes
	content := make([]byte, 600)
	for i := range content {
		content[i] = 'A' + byte(i%26)
	}
	if err := os.WriteFile(filePath, content, 0600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat error: %v", err)
	}

	dev, ino, err := getFileInfoStat(fi)
	if err != nil {
		t.Fatalf("getFileInfoStat error: %v", err)
	}

	sum := sha256.Sum256(content[:512])
	pHash := hex.EncodeToString(sum[:])

	tests := []struct {
		name        string
		cursor      *Cursor
		currentSize int64
		currentIno  uint64
		currentHash string
		wantReset   bool
	}{
		{
			name:        "nil cursor",
			cursor:      nil,
			currentSize: 100,
			currentIno:  ino,
			currentHash: pHash,
			wantReset:   false,
		},
		{
			name: "valid cursor - no reset",
			cursor: &Cursor{
				Offset:     10,
				Size:       int64(len(content)),
				Inode:      ino,
				Device:     dev,
				PrefixHash: pHash,
			},
			currentSize: int64(len(content)),
			currentIno:  ino,
			currentHash: pHash,
			wantReset:   false,
		},
		{
			name: "inode changed - trigger reset",
			cursor: &Cursor{
				Offset:     10,
				Size:       int64(len(content)),
				Inode:      ino + 999,
				Device:     dev,
				PrefixHash: pHash,
			},
			currentSize: int64(len(content)),
			currentIno:  ino,
			currentHash: pHash,
			wantReset:   true,
		},
		{
			name: "size shrunk below offset - trigger reset",
			cursor: &Cursor{
				Offset:     100,
				Size:       200,
				Inode:      ino,
				Device:     dev,
				PrefixHash: pHash,
			},
			currentSize: 50,
			currentIno:  ino,
			currentHash: pHash,
			wantReset:   true,
		},
		{
			name: "prefix hash mismatch - trigger reset",
			cursor: &Cursor{
				Offset:     10,
				Size:       int64(len(content)),
				Inode:      ino,
				Device:     dev,
				PrefixHash: "0000000000000000000000000000000000000000000000000000000000000000",
			},
			currentSize: int64(len(content)),
			currentIno:  ino,
			currentHash: pHash,
			wantReset:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reset := ShouldResetCursor(tt.cursor, tt.currentIno, tt.currentSize, tt.currentHash)
			if reset != tt.wantReset {
				t.Errorf("ShouldResetCursor() = %v, want %v", reset, tt.wantReset)
			}
		})
	}
}

func TestComputePrefixHash(t *testing.T) {
	t.Run("size < 512", func(t *testing.T) {
		r := bytes.NewReader([]byte("short content"))
		h, err := ComputePrefixHash(r, int64(len("short content")))
		if err != nil {
			t.Fatal(err)
		}
		if h != "" {
			t.Errorf("expected empty hash for size < 512, got %q", h)
		}
	})

	t.Run("size >= 512 with short read", func(t *testing.T) {
		r := bytes.NewReader([]byte("short")) // len 5 < 512
		h, err := ComputePrefixHash(r, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if h != "" {
			t.Errorf("expected empty hash for short read, got %q", h)
		}
	})

	t.Run("size >= 512", func(t *testing.T) {
		data := make([]byte, 1000)
		for i := range data {
			data[i] = byte(i % 256)
		}
		r := bytes.NewReader(data)
		h, err := ComputePrefixHash(r, 1000)
		if err != nil {
			t.Fatal(err)
		}
		expectedSum := sha256.Sum256(data[:512])
		expectedHex := hex.EncodeToString(expectedSum[:])
		if h != expectedHex {
			t.Errorf("ComputePrefixHash() = %q, want %q", h, expectedHex)
		}
	})
}

func TestDefaultCursorPath(t *testing.T) {
	t.Run("with XDG_CACHE_HOME set", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "/custom/cache")
		path := DefaultCursorPath()
		if path != "/custom/cache/agent-watcher/observer-cursors.json" {
			t.Errorf("got DefaultCursorPath = %q, want %q", path, "/custom/cache/agent-watcher/observer-cursors.json")
		}
	})

	t.Run("with XDG_CACHE_HOME unset", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("cannot get user home dir")
		}
		path := DefaultCursorPath()
		want := filepath.Join(home, ".cache", "agent-watcher", "observer-cursors.json")
		if path != want {
			t.Errorf("DefaultCursorPath() = %q, want %q", path, want)
		}
	})

	t.Run("with XDG_CACHE_HOME unset and UserHomeDir error", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "")
		origHome := userHomeDirFunc
		defer func() { userHomeDirFunc = origHome }()
		userHomeDirFunc = func() (string, error) {
			return "", os.ErrNotExist
		}

		path := DefaultCursorPath()
		want := filepath.Join(os.TempDir(), "agent-watcher", "observer-cursors.json")
		if path != want {
			t.Errorf("DefaultCursorPath() fallback = %q, want %q", path, want)
		}
	})

	t.Run("NewCursorStore with empty string uses default", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
		store, err := NewCursorStore("")
		if err != nil {
			t.Fatalf("NewCursorStore(\"\") error: %v", err)
		}
		if store.filePath == "" {
			t.Errorf("expected store.filePath to be set")
		}
	})
}

func TestCursorStoreConcurrentSave(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			store.Set(&Cursor{
				Key:       fmt.Sprintf("key-%d", idx),
				Path:      fmt.Sprintf("/path/%d.jsonl", idx),
				Offset:    int64(idx * 100),
				UpdatedAt: time.Now().UnixMilli(),
			})
			_ = store.Save()
		}(i)
	}
	wg.Wait()

	storeReloaded, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("reloading cursor store: %v", err)
	}
	for i := 0; i < 20; i++ {
		if _, ok := storeReloaded.Get(fmt.Sprintf("key-%d", i)); !ok {
			t.Errorf("missing key-%d in reloaded cursor store", i)
		}
	}
}

func TestCursorStoreDelete(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	c := &Cursor{
		Key:       "dev:123",
		Path:      filepath.Join(tmpDir, "session.jsonl"),
		Offset:    100,
		UpdatedAt: time.Now().UnixMilli(),
	}
	store.Set(c)

	if _, ok := store.Get("dev:123"); !ok {
		t.Fatalf("expected cursor to exist before delete")
	}

	store.Delete("dev:123")

	if _, ok := store.Get("dev:123"); ok {
		t.Errorf("expected cursor to be deleted")
	}

	// Deleting a nonexistent key should not panic
	store.Delete("nonexistent-key")
}

func TestCursorStorePruneMissingPath(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	realFile := filepath.Join(tmpDir, "exists.jsonl")
	if err := os.WriteFile(realFile, []byte("content\n"), 0600); err != nil {
		t.Fatalf("writing realFile: %v", err)
	}

	missingFile := filepath.Join(tmpDir, "does-not-exist.jsonl")

	now := time.Now().UnixMilli()
	store.Set(&Cursor{
		Key:       "key-real",
		Path:      realFile,
		Offset:    8,
		UpdatedAt: now,
	})
	store.Set(&Cursor{
		Key:       "key-missing",
		Path:      missingFile,
		Offset:    100,
		UpdatedAt: now,
	})
	store.Set(&Cursor{
		Key:       "key-empty-path",
		Path:      "",
		Offset:    50,
		UpdatedAt: now,
	})

	pruned := store.Prune(24 * time.Hour)
	if pruned != 2 {
		t.Errorf("Prune() = %d, want 2", pruned)
	}

	if _, ok := store.Get("key-real"); !ok {
		t.Errorf("expected key-real to be kept")
	}
	if _, ok := store.Get("key-missing"); ok {
		t.Errorf("expected key-missing to be pruned")
	}
	if _, ok := store.Get("key-empty-path"); ok {
		t.Errorf("expected key-empty-path to be pruned")
	}
}

func TestCursorStorePruneByAge(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	fileOld := filepath.Join(tmpDir, "old.jsonl")
	if err := os.WriteFile(fileOld, []byte("old content\n"), 0600); err != nil {
		t.Fatalf("writing fileOld: %v", err)
	}
	fileNew := filepath.Join(tmpDir, "new.jsonl")
	if err := os.WriteFile(fileNew, []byte("new content\n"), 0600); err != nil {
		t.Fatalf("writing fileNew: %v", err)
	}

	fiOld, err := os.Stat(fileOld)
	if err != nil {
		t.Fatalf("stat fileOld: %v", err)
	}
	devOld, inoOld, _ := getFileInfoStat(fiOld)

	fiNew, err := os.Stat(fileNew)
	if err != nil {
		t.Fatalf("stat fileNew: %v", err)
	}
	devNew, inoNew, _ := getFileInfoStat(fiNew)

	// key-old: existing file, matching inode, older than retention window (48h ago).
	// Must NOT be pruned by age alone.
	store.Set(&Cursor{
		Key:       fmt.Sprintf("%d:%d", devOld, inoOld),
		Path:      fileOld,
		Offset:    10,
		Inode:     inoOld,
		Device:    devOld,
		UpdatedAt: time.Now().Add(-48 * time.Hour).UnixMilli(),
	})
	// key-new: existing file, matching inode, recent.
	store.Set(&Cursor{
		Key:       fmt.Sprintf("%d:%d", devNew, inoNew),
		Path:      fileNew,
		Offset:    20,
		Inode:     inoNew,
		Device:    devNew,
		UpdatedAt: time.Now().UnixMilli(),
	})
	// key-mismatched-inode: points to fileOld path, but has an old/replaced inode that no longer matches on disk.
	// Must be pruned because its file no longer exists at this path.
	store.Set(&Cursor{
		Key:       "dev:99999999",
		Path:      fileOld,
		Offset:    5,
		Inode:     99999999,
		Device:    devOld,
		UpdatedAt: time.Now().Add(-48 * time.Hour).UnixMilli(),
	})

	pruned := store.Prune(24 * time.Hour)
	if pruned != 1 {
		t.Errorf("Prune() = %d, want 1", pruned)
	}

	if _, ok := store.Get(fmt.Sprintf("%d:%d", devOld, inoOld)); !ok {
		t.Errorf("expected key-old (existing file with matching inode) to be kept")
	}
	if _, ok := store.Get(fmt.Sprintf("%d:%d", devNew, inoNew)); !ok {
		t.Errorf("expected key-new to be kept")
	}
	if _, ok := store.Get("dev:99999999"); ok {
		t.Errorf("expected key-mismatched-inode to be pruned")
	}
}

func TestCursorStorePruneReappearingFileStartsAtOffsetZero(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	filePath := filepath.Join(tmpDir, "session.jsonl")
	if err := os.WriteFile(filePath, []byte("line1\nline2\n"), 0600); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	var mu sync.Mutex
	var received []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tail, err := NewTailer(filePath, store, func(line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("NewTailer error: %v", err)
	}

	go func() {
		_ = tail.Start(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tail.Stop()

	// Verify offset is committed
	fi, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat filePath: %v", err)
	}
	dev, ino, err := getFileInfoStat(fi)
	if err != nil {
		t.Fatalf("getFileInfoStat error: %v", err)
	}
	key := fmt.Sprintf("%d:%d", dev, ino)

	c, ok := store.Get(key)
	if !ok {
		t.Fatalf("cursor not found in store for %q", key)
	}
	if c.Offset != fi.Size() {
		t.Fatalf("expected cursor offset %d, got %d", fi.Size(), c.Offset)
	}

	// Delete file
	if err := os.Remove(filePath); err != nil {
		t.Fatalf("failed to remove filePath: %v", err)
	}

	// Prune missing file
	pruned := store.Prune(24 * time.Hour)
	if pruned != 1 {
		t.Fatalf("expected 1 pruned cursor, got %d", pruned)
	}
	if _, ok := store.Get(key); ok {
		t.Fatalf("expected cursor to be pruned from store")
	}

	// Recreate file with new content
	newContent := []byte("new-start-line1\nnew-start-line2\nnew-start-line3\n")
	if err := os.WriteFile(filePath, newContent, 0600); err != nil {
		t.Fatalf("failed to recreate file: %v", err)
	}

	// Reappearing file tailed with same store: must start at offset 0
	var receivedReappearing []string
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	tail2, err := NewTailer(filePath, store, func(line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		receivedReappearing = append(receivedReappearing, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("NewTailer2 error: %v", err)
	}

	go func() {
		_ = tail2.Start(ctx2)
	}()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(receivedReappearing)
		mu.Unlock()
		if count >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	tail2.Stop()

	mu.Lock()
	linesRead := len(receivedReappearing)
	mu.Unlock()

	if linesRead != 3 {
		t.Fatalf("expected 3 lines read from offset 0, got %d (%v)", linesRead, receivedReappearing)
	}
	if receivedReappearing[0] != "new-start-line1" {
		t.Errorf("first line = %q, want %q", receivedReappearing[0], "new-start-line1")
	}
}

func TestCursorStorePruneNoOp(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	// Empty store prune
	if pruned := store.Prune(24 * time.Hour); pruned != 0 {
		t.Errorf("Prune() on empty store = %d, want 0", pruned)
	}

	// Store with valid recent file
	f := filepath.Join(tmpDir, "valid.jsonl")
	if err := os.WriteFile(f, []byte("test\n"), 0600); err != nil {
		t.Fatalf("writing valid: %v", err)
	}
	store.Set(&Cursor{
		Key:       "valid",
		Path:      f,
		Offset:    5,
		UpdatedAt: time.Now().UnixMilli(),
	})
	if pruned := store.Prune(24 * time.Hour); pruned != 0 {
		t.Errorf("Prune() with all valid entries = %d, want 0", pruned)
	}
}

func TestCursorStorePruneConcurrentUpdateSafety(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	initialTime := time.Now().UnixMilli() - 100000
	// Entry 1: missing file candidate
	store.Set(&Cursor{
		Key:       "key-missing-file",
		Path:      filepath.Join(tmpDir, "missing.jsonl"),
		Offset:    10,
		UpdatedAt: initialTime,
	})
	// Entry 2: stale age candidate
	store.Set(&Cursor{
		Key:       "key-stale-age",
		Path:      filepath.Join(tmpDir, "exists.jsonl"),
		Offset:    50,
		UpdatedAt: time.Now().Add(-48 * time.Hour).UnixMilli(),
	})

	fileExistsCalled := 0
	pruned := store.PruneWithFileExists(24*time.Hour, func(path string) bool {
		fileExistsCalled++
		// Verify lock is not held: TryRLock should succeed
		if !store.mu.TryRLock() {
			t.Errorf("expected store.mu not to be locked during fileExists")
		} else {
			store.mu.RUnlock()
		}

		// Concurrently update both candidates during fileExists
		store.Set(&Cursor{
			Key:       "key-missing-file",
			Path:      path,
			Offset:    20,
			UpdatedAt: time.Now().UnixMilli(),
		})
		store.Set(&Cursor{
			Key:       "key-stale-age",
			Path:      filepath.Join(tmpDir, "exists.jsonl"),
			Offset:    60,
			UpdatedAt: time.Now().UnixMilli(),
		})
		// Return false indicating file doesn't exist
		return false
	})

	if fileExistsCalled != 2 {
		t.Errorf("fileExists was called %d times, want 2", fileExistsCalled)
	}
	if pruned != 0 {
		t.Errorf("PruneWithFileExists() = %d, want 0 because both cursors were concurrently updated", pruned)
	}

	c1, ok := store.Get("key-missing-file")
	if !ok {
		t.Fatalf("expected key-missing-file to be retained after concurrent update")
	}
	if c1.Offset != 20 {
		t.Errorf("c1 offset = %d, want 20", c1.Offset)
	}

	c2, ok := store.Get("key-stale-age")
	if !ok {
		t.Fatalf("expected key-stale-age to be retained after concurrent update")
	}
	if c2.Offset != 60 {
		t.Errorf("c2 offset = %d, want 60", c2.Offset)
	}
}

func TestDefaultFileExists(t *testing.T) {
	tmpDir := t.TempDir()

	// Empty path
	if defaultFileExists("") {
		t.Errorf("defaultFileExists(\"\") = true, want false")
	}

	// Non-existent path
	missingPath := filepath.Join(tmpDir, "missing.jsonl")
	if defaultFileExists(missingPath) {
		t.Errorf("defaultFileExists(missingPath) = true, want false")
	}

	// Existing file
	realFile := filepath.Join(tmpDir, "real.jsonl")
	if err := os.WriteFile(realFile, []byte("data"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if !defaultFileExists(realFile) {
		t.Errorf("defaultFileExists(realFile) = false, want true")
	}

	// Permission error / inaccessible path: should return true to keep cursor until age retention expires
	subDir := filepath.Join(tmpDir, "restricted")
	if err := os.Mkdir(subDir, 0700); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	inaccessibleFile := filepath.Join(subDir, "restricted.jsonl")
	if err := os.WriteFile(inaccessibleFile, []byte("secret"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Remove search/read permissions on parent directory so os.Stat returns permission denied
	if err := os.Chmod(subDir, 0000); err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}
	defer func() {
		_ = os.Chmod(subDir, 0700)
	}()

	// Verify stat actually produces permission denied when run as non-root
	if _, err := os.Stat(inaccessibleFile); err != nil && !os.IsNotExist(err) {
		if !defaultFileExists(inaccessibleFile) {
			t.Errorf("defaultFileExists(inaccessibleFile) = false, want true (transient/permission error)")
		}
	}
}

func TestCursorStorePruneKeepsEntryOnPermissionError(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	subDir := filepath.Join(tmpDir, "noperm")
	if err := os.Mkdir(subDir, 0700); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	targetFile := filepath.Join(subDir, "target.jsonl")
	if err := os.WriteFile(targetFile, []byte("content"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	store.Set(&Cursor{
		Key:       "key-inaccessible",
		Path:      targetFile,
		Offset:    7,
		UpdatedAt: time.Now().UnixMilli(),
	})

	if err := os.Chmod(subDir, 0000); err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}
	defer func() {
		_ = os.Chmod(subDir, 0700)
	}()

	if _, statErr := os.Stat(targetFile); statErr != nil && !os.IsNotExist(statErr) {
		pruned := store.Prune(24 * time.Hour)
		if pruned != 0 {
			t.Errorf("Prune() = %d, want 0 (file with permission error must be kept)", pruned)
		}
		if _, ok := store.Get("key-inaccessible"); !ok {
			t.Errorf("expected key-inaccessible to be retained on permission error")
		}
	}
}

func TestCursorStorePruneWithCorruptedNilCursor(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	validFile := filepath.Join(tmpDir, "valid.jsonl")
	if err := os.WriteFile(validFile, []byte("line1\n"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	store.Set(&Cursor{
		Key:       "valid-key",
		Path:      validFile,
		Offset:    6,
		UpdatedAt: time.Now().UnixMilli(),
	})

	// Inject corrupted nil cursor directly into map
	store.mu.Lock()
	store.cursors["corrupted-nil-key"] = nil
	store.mu.Unlock()

	// Prune must not panic and must prune the corrupted nil cursor
	pruned := store.Prune(24 * time.Hour)
	if pruned != 1 {
		t.Errorf("Prune() = %d, want 1", pruned)
	}

	store.mu.RLock()
	_, corruptedExists := store.cursors["corrupted-nil-key"]
	store.mu.RUnlock()
	if corruptedExists {
		t.Errorf("expected corrupted-nil-key to be deleted from store.cursors")
	}

	if _, ok := store.Get("valid-key"); !ok {
		t.Errorf("expected valid-key to be retained")
	}
}

func TestCursorStorePruneCorruptedNilCursorFromJSON(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	validFile := filepath.Join(tmpDir, "session.jsonl")
	if err := os.WriteFile(validFile, []byte("data\n"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	now := time.Now().UnixMilli()
	jsonContent := fmt.Sprintf(`{
  "corrupted-key": null,
  "valid-key": {
    "key": "valid-key",
    "path": %q,
    "offset": 5,
    "updatedAt": %d
  }
}`, validFile, now)

	if err := os.WriteFile(cursorPath, []byte(jsonContent), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	pruned := store.Prune(24 * time.Hour)
	if pruned != 1 {
		t.Errorf("Prune() = %d, want 1", pruned)
	}

	store.mu.RLock()
	_, exists := store.cursors["corrupted-key"]
	store.mu.RUnlock()
	if exists {
		t.Errorf("expected corrupted-key to be pruned")
	}

	if _, ok := store.Get("valid-key"); !ok {
		t.Errorf("expected valid-key to be retained")
	}
}

func TestCursorStoreDirtyTrackingAndFlush(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "cursors.json")

	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore failed: %v", err)
	}

	if store.IsDirty() {
		t.Errorf("newly created store should not be dirty")
	}
	if store.SaveCount() != 0 {
		t.Errorf("save count should be 0 initially, got %d", store.SaveCount())
	}

	// Flush when not dirty is a no-op
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush when not dirty failed: %v", err)
	}
	if store.SaveCount() != 0 {
		t.Errorf("Flush when not dirty should not save, got %d", store.SaveCount())
	}

	// Set cursor -> dirty
	c := &Cursor{
		Key:        "dev:ino1",
		Path:       "/tmp/test.jsonl",
		Offset:     100,
		Size:       200,
		Inode:      123,
		Device:     456,
		PrefixHash: "hash1",
		UpdatedAt:  1000,
	}
	store.Set(c)
	if !store.IsDirty() {
		t.Errorf("store should be dirty after Set")
	}

	// Flush -> saves, clears dirty
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if store.SaveCount() != 1 {
		t.Errorf("expected save count 1 after Flush, got %d", store.SaveCount())
	}
	if store.IsDirty() {
		t.Errorf("store should not be dirty after Flush")
	}

	// Setting identical cursor does not mark dirty
	store.Set(c)
	if store.IsDirty() {
		t.Errorf("setting identical cursor should not mark dirty")
	}

	// Delete non-existent key does not mark dirty
	store.Delete("nonexistent")
	if store.IsDirty() {
		t.Errorf("deleting nonexistent key should not mark dirty")
	}

	// Delete existing key marks dirty
	store.Delete("dev:ino1")
	if !store.IsDirty() {
		t.Errorf("deleting existing key should mark dirty")
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush after Delete failed: %v", err)
	}
	if store.SaveCount() != 2 {
		t.Errorf("expected save count 2 after Delete flush, got %d", store.SaveCount())
	}

	// Compact JSON verification
	data, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("reading cursor file: %v", err)
	}
	if bytes.Contains(data, []byte("\n  ")) {
		t.Errorf("expected compact JSON, got:\n%s", string(data))
	}
}

func TestCursorStoreStartAutoFlush(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "cursors.json")

	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store.StartAutoFlush(ctx, 25*time.Millisecond)

	// No modifications -> no flushes
	time.Sleep(60 * time.Millisecond)
	if store.SaveCount() != 0 {
		t.Errorf("expected 0 saves with no changes, got %d", store.SaveCount())
	}

	// Make modification
	store.Set(&Cursor{
		Key:       "dev:ino2",
		Path:      "/tmp/file.jsonl",
		Offset:    500,
		UpdatedAt: 2000,
	})

	time.Sleep(60 * time.Millisecond)
	if store.SaveCount() != 1 {
		t.Errorf("expected 1 save after auto-flush interval, got %d", store.SaveCount())
	}
	if store.IsDirty() {
		t.Errorf("expected clean store after auto-flush")
	}

	// Cancel context stops flusher
	cancel()
	time.Sleep(20 * time.Millisecond)

	store.Set(&Cursor{
		Key:       "dev:ino2",
		Path:      "/tmp/file.jsonl",
		Offset:    900,
		UpdatedAt: 3000,
	})

	time.Sleep(60 * time.Millisecond)
	// Since auto flusher was stopped, SaveCount should still be 1
	if store.SaveCount() != 1 {
		t.Errorf("expected auto flusher to stop after ctx cancellation, got %d saves", store.SaveCount())
	}
}

func TestCursorStoreSaveConcurrentMutationPreservesDirty(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "cursors.json")

	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore failed: %v", err)
	}

	c1 := &Cursor{
		Key:       "key1",
		Path:      "/tmp/key1.jsonl",
		Offset:    100,
		UpdatedAt: 1000,
	}
	store.Set(c1)
	if !store.IsDirty() {
		t.Fatalf("expected store to be dirty after initial Set")
	}

	c2 := &Cursor{
		Key:       "key2",
		Path:      "/tmp/key2.jsonl",
		Offset:    200,
		UpdatedAt: 2000,
	}

	hookRan := false
	store.beforeSaveCommitHook = func() {
		hookRan = true
		// While Save() file I/O has occurred, another mutation arrives
		store.Set(c2)
	}

	if err := store.Save(); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}
	if !hookRan {
		t.Fatalf("expected beforeSaveCommitHook to run")
	}

	// Verify dirty is preserved because mutation occurred during Save()
	if !store.IsDirty() {
		t.Errorf("expected store to remain dirty when mutation arrived during Save()")
	}

	// Verify file on disk only has c1, not c2 yet
	diskData, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("failed reading cursor file: %v", err)
	}
	var diskCursors map[string]*Cursor
	if err := json.Unmarshal(diskData, &diskCursors); err != nil {
		t.Fatalf("failed unmarshaling disk cursors: %v", err)
	}
	if _, ok := diskCursors["key2"]; ok {
		t.Errorf("expected disk file after first Save() NOT to contain key2")
	}

	// Next Flush() must save updated state and clear dirty
	store.beforeSaveCommitHook = nil
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush() failed: %v", err)
	}

	if store.IsDirty() {
		t.Errorf("expected store to not be dirty after Flush()")
	}

	diskData2, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("failed reading cursor file: %v", err)
	}
	var diskCursors2 map[string]*Cursor
	if err := json.Unmarshal(diskData2, &diskCursors2); err != nil {
		t.Fatalf("failed unmarshaling disk cursors: %v", err)
	}
	if diskCursors2["key1"] == nil || diskCursors2["key1"].Offset != 100 {
		t.Errorf("expected disk file to contain key1 with offset 100")
	}
	if diskCursors2["key2"] == nil || diskCursors2["key2"].Offset != 200 {
		t.Errorf("expected disk file to contain key2 with offset 200")
	}
}

func TestCursorStoreStartAutoFlush_OnError(t *testing.T) {
	tmpDir := t.TempDir()
	dirPath := filepath.Join(tmpDir, "blocked_dir")
	cursorPath := filepath.Join(dirPath, "cursors.json")

	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore failed: %v", err)
	}

	// Create a regular file where the directory should be, causing Save() to fail on MkdirAll
	if err := os.WriteFile(dirPath, []byte("blocker"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	store.Set(&Cursor{
		Key:    "key1",
		Offset: 100,
	})

	errCh := make(chan error, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store.StartAutoFlush(ctx, 15*time.Millisecond, func(err error) {
		errCh <- err
	})

	select {
	case err := <-errCh:
		if err == nil {
			t.Errorf("expected non-nil error in onError callback")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for onError callback in StartAutoFlush")
	}
}

func TestCursorStoreStartAutoFlush_NonPositiveIntervalDefaults(t *testing.T) {
	for _, interval := range []time.Duration{0, -1, -1 * time.Second} {
		t.Run(fmt.Sprintf("interval=%v", interval), func(t *testing.T) {
			tmpDir := t.TempDir()
			cursorPath := filepath.Join(tmpDir, "cursors.json")
			store, err := NewCursorStore(cursorPath)
			if err != nil {
				t.Fatalf("NewCursorStore failed: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			store.StartAutoFlush(ctx, interval)

			// Allow background goroutine to spawn and ticker to initialize without panicking
			time.Sleep(50 * time.Millisecond)

			if store.SaveCount() != 0 {
				t.Errorf("expected 0 saves initially, got %d", store.SaveCount())
			}
		})
	}
}

func TestCursorStorePruneWithFileExistsOldCursorSurvives(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	testPath := filepath.Join(tmpDir, "transcript.jsonl")
	if err := os.WriteFile(testPath, []byte("some initial content\n"), 0600); err != nil {
		t.Fatalf("writing testPath: %v", err)
	}

	fi, err := os.Stat(testPath)
	if err != nil {
		t.Fatalf("stat testPath: %v", err)
	}
	dev, ino, err := getFileInfoStat(fi)
	if err != nil {
		t.Fatalf("getFileInfoStat error: %v", err)
	}
	key := fmt.Sprintf("%d:%d", dev, ino)

	oldTime := time.Now().Add(-48 * time.Hour).UnixMilli()
	store.Set(&Cursor{
		Key:       key,
		Path:      testPath,
		Offset:    int64(len("some initial content\n")),
		Size:      fi.Size(),
		Inode:     ino,
		Device:    dev,
		UpdatedAt: oldTime,
	})

	// File exists: custom fileExists predicate returns true
	pruned := store.PruneWithFileExists(24*time.Hour, func(path string) bool {
		return path == testPath
	})

	if pruned != 0 {
		t.Errorf("PruneWithFileExists() = %d, want 0 (old cursor for existing file must survive)", pruned)
	}

	c, ok := store.Get(key)
	if !ok {
		t.Fatalf("cursor was pruned from store, expected to survive")
	}
	if c.Offset != int64(len("some initial content\n")) {
		t.Errorf("cursor offset = %d, want %d", c.Offset, len("some initial content\n"))
	}
}

func TestTailerRestartAfterPruneSweepResumesFromCommittedOffset(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	filePath := filepath.Join(tmpDir, "session.jsonl")
	content := []byte("line1\nline2\nline3\n")
	if err := os.WriteFile(filePath, content, 0600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	var mu sync.Mutex
	var received []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tail, err := NewTailer(filePath, store, func(line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("NewTailer error: %v", err)
	}

	go func() {
		_ = tail.Start(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	tail.Stop()

	mu.Lock()
	initialCount := len(received)
	mu.Unlock()
	if initialCount != 3 {
		t.Fatalf("initial read lines = %d, want 3", initialCount)
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat filePath: %v", err)
	}
	dev, ino, err := getFileInfoStat(fi)
	if err != nil {
		t.Fatalf("getFileInfoStat error: %v", err)
	}
	key := fmt.Sprintf("%d:%d", dev, ino)

	c, ok := store.Get(key)
	if !ok {
		t.Fatalf("cursor not found in store for %q", key)
	}
	if c.Offset != fi.Size() {
		t.Fatalf("cursor offset = %d, want %d", c.Offset, fi.Size())
	}

	// Age the cursor past the retention window (e.g. 48 hours ago with 24h retention)
	agedCursor := *c
	agedCursor.UpdatedAt = time.Now().Add(-48 * time.Hour).UnixMilli()
	store.Set(&agedCursor)

	// Run prune sweep
	pruned := store.Prune(24 * time.Hour)
	if pruned != 0 {
		t.Fatalf("Prune() = %d, want 0 (cursor for existing file must not be pruned)", pruned)
	}

	if _, ok := store.Get(key); !ok {
		t.Fatalf("expected cursor to survive prune sweep, but was removed")
	}

	// Restart tailer on the same file with the same store
	var receivedAfterRestart []string
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	tail2, err := NewTailer(filePath, store, func(line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		receivedAfterRestart = append(receivedAfterRestart, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("NewTailer restart error: %v", err)
	}

	go func() {
		_ = tail2.Start(ctx2)
	}()

	// Wait briefly to confirm no duplicate lines are read
	time.Sleep(100 * time.Millisecond)
	tail2.Stop()

	mu.Lock()
	replayedCount := len(receivedAfterRestart)
	mu.Unlock()

	if replayedCount != 0 {
		t.Errorf("tailer restart replayed %d lines (%v), want 0 (must resume from committed offset %d)",
			replayedCount, receivedAfterRestart, fi.Size())
	}
}

func TestCursorStorePruneZeroDeviceOrNonUnixSurvives(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	testFile := filepath.Join(tmpDir, "active.jsonl")
	if err := os.WriteFile(testFile, []byte("active content\n"), 0600); err != nil {
		t.Fatalf("writing testFile: %v", err)
	}

	fi, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("stat testFile: %v", err)
	}
	dev, ino, err := getFileInfoStat(fi)
	if err != nil {
		t.Fatalf("getFileInfoStat error: %v", err)
	}

	oldTime := time.Now().Add(-48 * time.Hour).UnixMilli()

	// 1. Cursor with Device = 0, matching Inode.
	// Must survive because inode matches and zero device does not trigger false mismatch.
	keyZeroDev := fmt.Sprintf("0:%d", ino)
	store.Set(&Cursor{
		Key:       keyZeroDev,
		Path:      testFile,
		Offset:    10,
		Inode:     ino,
		Device:    0,
		UpdatedAt: oldTime,
	})

	// 2. Cursor with Device = 0, Inode = 0 (simulates non-Unix zero return or legacy cursor format).
	// Must survive because file exists on disk and zero metadata does not trigger false mismatch.
	keyZeroBoth := "zero-metadata-key"
	store.Set(&Cursor{
		Key:       keyZeroBoth,
		Path:      testFile,
		Offset:    10,
		Inode:     0,
		Device:    0,
		UpdatedAt: oldTime,
	})

	// 3. Cursor with Device = 0 in struct, but key is "dev:ino" so dev is extracted from key.
	// Must survive because key provides dev fallback and inode matches.
	keyFallback := fmt.Sprintf("%d:%d", dev, ino)
	store.Set(&Cursor{
		Key:       keyFallback,
		Path:      testFile,
		Offset:    10,
		Inode:     ino,
		Device:    0, // zero in struct, extracted from key
		UpdatedAt: oldTime,
	})

	// 4. Cursor with Device = 0, but mismatched Inode (e.g. 99999999).
	// Must be pruned because inode does not match the file on disk.
	keyMismatchedIno := "0:99999999"
	store.Set(&Cursor{
		Key:       keyMismatchedIno,
		Path:      testFile,
		Offset:    10,
		Inode:     99999999,
		Device:    0,
		UpdatedAt: oldTime,
	})

	pruned := store.Prune(24 * time.Hour)
	if pruned != 1 {
		t.Errorf("Prune() = %d, want 1 (only mismatched inode should be pruned)", pruned)
	}

	if _, ok := store.Get(keyZeroDev); !ok {
		t.Errorf("expected cursor with Device=0 and matching Inode to survive prune")
	}
	if _, ok := store.Get(keyZeroBoth); !ok {
		t.Errorf("expected cursor with Device=0, Inode=0 to survive prune when file exists")
	}
	if _, ok := store.Get(keyFallback); !ok {
		t.Errorf("expected cursor with key-fallback device to survive prune")
	}
	if _, ok := store.Get(keyMismatchedIno); ok {
		t.Errorf("expected cursor with mismatched inode to be pruned")
	}
}

func TestCursorStorePruneWithFileExistsNilPredicate(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "observer-cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	testFile := filepath.Join(tmpDir, "exists.jsonl")
	if err := os.WriteFile(testFile, []byte("data\n"), 0600); err != nil {
		t.Fatalf("writing testFile: %v", err)
	}

	fi, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("stat testFile: %v", err)
	}
	dev, ino, err := getFileInfoStat(fi)
	if err != nil {
		t.Fatalf("getFileInfoStat error: %v", err)
	}

	oldTime := time.Now().Add(-48 * time.Hour).UnixMilli()

	// Existing file cursor older than retention window: should survive
	keyExisting := fmt.Sprintf("%d:%d", dev, ino)
	store.Set(&Cursor{
		Key:       keyExisting,
		Path:      testFile,
		Offset:    5,
		Inode:     ino,
		Device:    dev,
		UpdatedAt: oldTime,
	})

	// Cursor with Device=0 and matching Inode: should survive
	keyZeroDev := "zero-dev-nil-test"
	store.Set(&Cursor{
		Key:       keyZeroDev,
		Path:      testFile,
		Offset:    5,
		Inode:     ino,
		Device:    0,
		UpdatedAt: oldTime,
	})

	// Non-existent file cursor: should be pruned
	missingFile := filepath.Join(tmpDir, "missing.jsonl")
	keyMissing := "key-missing-nil-test"
	store.Set(&Cursor{
		Key:       keyMissing,
		Path:      missingFile,
		Offset:    10,
		UpdatedAt: oldTime,
	})

	// Mismatched inode cursor: should be pruned
	keyMismatched := "key-mismatched-nil-test"
	store.Set(&Cursor{
		Key:       keyMismatched,
		Path:      testFile,
		Offset:    5,
		Inode:     ino + 99999,
		Device:    dev,
		UpdatedAt: oldTime,
	})

	// PruneWithFileExists with nil predicate must default to defaultFileExists
	pruned := store.PruneWithFileExists(24*time.Hour, nil)
	if pruned != 2 {
		t.Errorf("PruneWithFileExists(24h, nil) = %d, want 2", pruned)
	}

	if _, ok := store.Get(keyExisting); !ok {
		t.Errorf("expected existing file cursor to survive when fileExists is nil")
	}
	if _, ok := store.Get(keyZeroDev); !ok {
		t.Errorf("expected cursor with Device=0 to survive when fileExists is nil")
	}
	if _, ok := store.Get(keyMissing); ok {
		t.Errorf("expected missing file cursor to be pruned when fileExists is nil")
	}
	if _, ok := store.Get(keyMismatched); ok {
		t.Errorf("expected mismatched inode cursor to be pruned when fileExists is nil")
	}
}
