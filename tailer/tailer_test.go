package tailer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestTailerReadAndAppend(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	targetFile := filepath.Join(tmpDir, "session.jsonl")
	initialLines := []string{
		`{"type":"user","message":"hello"}`,
		`{"type":"assistant","message":"hi there"}`,
	}

	f, err := os.Create(targetFile)
	if err != nil {
		t.Fatalf("create file error: %v", err)
	}
	for _, l := range initialLines {
		_, _ = f.WriteString(l + "\n")
	}
	_ = f.Close()

	var received []string
	var mu sync.Mutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tail, err := NewTailer(targetFile, store, func(line []byte) error {
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

	// Wait for initial read
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

	mu.Lock()
	if len(received) != 2 {
		t.Fatalf("expected 2 lines initially, got %d", len(received))
	}
	if received[0] != initialLines[0] || received[1] != initialLines[1] {
		t.Errorf("initial lines mismatch: got %v", received)
	}
	mu.Unlock()

	// Append more lines
	fAppend, err := os.OpenFile(targetFile, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open append error: %v", err)
	}
	appendedLine := `{"type":"tool_call","tool":"Bash"}`
	_, _ = fAppend.WriteString(appendedLine + "\n")
	_ = fAppend.Sync()
	_ = fAppend.Close()

	// Wait for append
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	if len(received) != 3 {
		t.Fatalf("expected 3 lines after append, got %d", len(received))
	}
	if received[2] != appendedLine {
		t.Errorf("appended line mismatch: got %q, want %q", received[2], appendedLine)
	}
	mu.Unlock()

	tail.Stop()
}

func TestTailerLargeLine16MBBuffer(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	targetFile := filepath.Join(tmpDir, "large.jsonl")

	// Create a line of 2MB (larger than default 64KB bufio.Scanner buffer)
	largeData := make([]byte, 2*1024*1024)
	for i := range largeData {
		largeData[i] = 'a'
	}
	largeLine := fmt.Sprintf(`{"type":"tool_result","content":"%s"}`, string(largeData))

	if err := os.WriteFile(targetFile, []byte(largeLine+"\n"), 0600); err != nil {
		t.Fatalf("write large file error: %v", err)
	}

	var received []string
	var mu sync.Mutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tail, err := NewTailer(targetFile, store, func(line []byte) error {
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
		if count >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 large line, got %d", len(received))
	}
	if len(received[0]) != len(largeLine) {
		t.Errorf("received line length %d != expected %d", len(received[0]), len(largeLine))
	}

	tail.Stop()
}

func TestTailerFileTruncationAndHandlerError(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	targetFile := filepath.Join(tmpDir, "truncate.jsonl")

	// Initial long file
	_ = os.WriteFile(targetFile, []byte("line 1 long long long long\nline 2 long long long long\n"), 0600)

	var mu sync.Mutex
	var received []string

	tail, err := NewTailer(targetFile, store, func(line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// First read
	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable error: %v", err)
	}

	mu.Lock()
	if len(received) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(received))
	}
	mu.Unlock()

	// Truncate file to shorter size
	_ = os.WriteFile(targetFile, []byte("short\n"), 0600)

	// Second read: should detect shrink and reset offset
	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable after truncate error: %v", err)
	}

	mu.Lock()
	if len(received) != 3 || received[2] != "short" {
		t.Errorf("expected 3 lines with 'short', got %v", received)
	}
	mu.Unlock()

	// Read on non-existent file
	missingTail, _ := NewTailer(filepath.Join(tmpDir, "missing.jsonl"), store, nil)
	if err := missingTail.readAvailable(); err == nil {
		t.Errorf("expected error for missing file in readAvailable")
	}
}

func TestTailerStartStopMethod(t *testing.T) {
	origInterval := tailerPollInterval
	tailerPollInterval = 20 * time.Millisecond
	defer func() { tailerPollInterval = origInterval }()

	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	targetFile := filepath.Join(tmpDir, "stop.jsonl")
	_ = os.WriteFile(targetFile, []byte("initial\n"), 0600)

	var mu sync.Mutex
	var count int
	tail, err := NewTailer(targetFile, store, func(line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- tail.Start(context.Background())
	}()

	// Wait for initial read and ticker poll
	time.Sleep(50 * time.Millisecond)

	// Append via ticker check
	f, _ := os.OpenFile(targetFile, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("second\n")
	_ = f.Close()

	time.Sleep(60 * time.Millisecond)

	tail.Stop()

	select {
	case err := <-doneCh:
		if err != nil {
			t.Errorf("expected nil error on tail.Stop(), got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for tail.Start to exit after tail.Stop()")
	}

	mu.Lock()
	if count != 2 {
		t.Errorf("expected 2 lines read, got %d", count)
	}
	mu.Unlock()
}

func TestTailerEmptyLinesAndNilHandler(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	targetFile := filepath.Join(tmpDir, "empty_lines.jsonl")

	// File containing empty lines and whitespace lines
	_ = os.WriteFile(targetFile, []byte("\n\nvalid line\n\n\n"), 0600)

	var received []string
	tail, err := NewTailer(targetFile, store, func(line []byte) error {
		received = append(received, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable error: %v", err)
	}

	if len(received) != 1 || received[0] != "valid line" {
		t.Errorf("expected 1 'valid line', got %v", received)
	}

	// Test with nil onLine handler
	tailNilHandler, err := NewTailer(targetFile, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tailNilHandler.readAvailable(); err != nil {
		t.Fatalf("readAvailable with nil handler error: %v", err)
	}
}

func TestTailerResumeFromExistingCursor(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	targetFile := filepath.Join(tmpDir, "resume.jsonl")

	line1 := "first line 123456789\n"
	line2 := "second line 987654321\n"
	_ = os.WriteFile(targetFile, []byte(line1+line2), 0600)

	// Tailer 1 reads both lines and saves cursor
	tail1, err := NewTailer(targetFile, store, func(line []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := tail1.readAvailable(); err != nil {
		t.Fatal(err)
	}

	// Tailer 2 opens same file and should read 0 lines because cursor is up to date
	var read2 []string
	tail2, err := NewTailer(targetFile, store, func(line []byte) error {
		read2 = append(read2, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tail2.readAvailable(); err != nil {
		t.Fatal(err)
	}

	if len(read2) != 0 {
		t.Errorf("expected 0 lines read on resume with up-to-date cursor, got %d", len(read2))
	}

	// Append a 3rd line
	line3 := "third line after resume\n"
	f, _ := os.OpenFile(targetFile, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(line3)
	_ = f.Close()

	if err := tail2.readAvailable(); err != nil {
		t.Fatal(err)
	}

	if len(read2) != 1 || read2[0] != "third line after resume" {
		t.Errorf("expected 1 line 'third line after resume', got %v", read2)
	}
}

func TestTailerCloseWatcherEventsDirectly(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	targetFile := filepath.Join(tmpDir, "close_watcher.jsonl")
	_ = os.WriteFile(targetFile, []byte("line\n"), 0600)

	reg, err := NewWatcherRegistry()
	if err != nil {
		t.Fatal(err)
	}

	tail, err := NewTailer(targetFile, store, func(line []byte) error { return nil }, WithWatcherRegistry(reg))
	if err != nil {
		t.Fatal(err)
	}

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- tail.Start(context.Background())
	}()

	time.Sleep(50 * time.Millisecond)

	// Closing watcher directly causes events channel to close and Start to return nil
	_ = tail.registry.Close()

	select {
	case err := <-doneCh:
		if err != nil {
			t.Errorf("expected nil on watcher close, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for tailer to exit on watcher close")
	}
}

func TestTailerStartInvalidWatchDir(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	invalidFile := filepath.Join(tmpDir, "deeply", "nested", "nonexistent", "file.jsonl")
	tail, err := NewTailer(invalidFile, store, func(line []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err = tail.Start(ctx)
	if err == nil {
		t.Errorf("expected error when starting tailer on nonexistent directory")
	}
}

func TestTailerEventsChannel(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	targetFile := filepath.Join(tmpDir, "events.jsonl")
	_ = os.WriteFile(targetFile, []byte("line 1\n"), 0600)

	var mu sync.Mutex
	var lines []string
	tail, err := NewTailer(targetFile, store, func(l []byte) error {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, string(l))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = tail.Start(ctx)
	}()

	time.Sleep(30 * time.Millisecond)

	// Append data and trigger write event
	f, _ := os.OpenFile(targetFile, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("line 2\n")
	_ = f.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		c := len(lines)
		mu.Unlock()
		if c >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	if len(lines) < 2 {
		t.Errorf("expected at least 2 lines, got %d", len(lines))
	}
	mu.Unlock()

	tail.Stop()
}

func TestTailerBackpressureHandlerErrorPausesReads(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	targetFile := filepath.Join(tmpDir, "backpressure.jsonl")

	line1 := "line 1 payload\n"
	line2 := "line 2 payload\n"
	line3 := "line 3 payload\n"
	_ = os.WriteFile(targetFile, []byte(line1+line2+line3), 0600)

	var mu sync.Mutex
	var received []string
	shouldFailLine2 := true

	tail, err := NewTailer(targetFile, store, func(line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if string(line) == "line 2 payload" && shouldFailLine2 {
			return errors.New("simulated backpressure / shipper full")
		}
		received = append(received, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("NewTailer error: %v", err)
	}

	// 1. First readAvailable pass: Line 1 succeeds, Line 2 fails with backpressure
	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable error: %v", err)
	}

	mu.Lock()
	if len(received) != 1 || received[0] != "line 1 payload" {
		t.Fatalf("expected only line 1 to be received on backpressure, got %v", received)
	}
	mu.Unlock()

	// Offset must pause at line 1 and not skip line 2
	expectedOffset1 := int64(len(line1))
	if tail.CurrentOffset() != expectedOffset1 {
		t.Errorf("CurrentOffset() = %d, want %d", tail.CurrentOffset(), expectedOffset1)
	}

	// 2. Clear backpressure condition and read again
	mu.Lock()
	shouldFailLine2 = false
	mu.Unlock()

	if err := tail.readAvailable(); err != nil {
		t.Fatalf("second readAvailable error: %v", err)
	}

	mu.Lock()
	if len(received) != 3 {
		t.Fatalf("expected all 3 lines to be received after unblocking, got %d: %v", len(received), received)
	}
	if received[0] != "line 1 payload" || received[1] != "line 2 payload" || received[2] != "line 3 payload" {
		t.Errorf("unexpected received lines: %v", received)
	}
	mu.Unlock()

	totalBytes := int64(len(line1 + line2 + line3))
	if tail.CurrentOffset() != totalBytes {
		t.Errorf("final CurrentOffset() = %d, want %d", tail.CurrentOffset(), totalBytes)
	}
}

func TestTailerCursorCommittedDelivery(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "cursors.json")
	store, _ := NewCursorStore(cursorPath)
	targetFile := filepath.Join(tmpDir, "committed.jsonl")

	line1 := "line 1 committed\n"
	line2 := "line 2 committed\n"
	_ = os.WriteFile(targetFile, []byte(line1+line2), 0600)

	var mu sync.Mutex
	var received []string
	var offsets []int64

	tail, err := NewTailerWithOffsetHandler(targetFile, store, func(line []byte, offset int64) error {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, string(line))
		offsets = append(offsets, offset)
		return nil
	})
	if err != nil {
		t.Fatalf("NewTailerWithOffsetHandler error: %v", err)
	}

	// Read from disk
	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable error: %v", err)
	}

	mu.Lock()
	if len(received) != 2 {
		t.Fatalf("expected 2 lines received, got %d", len(received))
	}
	mu.Unlock()

	// In committed delivery mode, CurrentOffset is advanced in memory but CommittedOffset remains 0 until ACK
	totalBytes := int64(len(line1 + line2))
	if tail.CurrentOffset() != totalBytes {
		t.Errorf("CurrentOffset() = %d, want %d", tail.CurrentOffset(), totalBytes)
	}
	if tail.CommittedOffset() != 0 {
		t.Errorf("CommittedOffset() = %d, want 0 before commit", tail.CommittedOffset())
	}

	// Cursor store should not have committed offset yet
	if c, ok := store.Get(tail.key); ok && c.Offset != 0 {
		t.Errorf("store cursor offset = %d, want 0", c.Offset)
	}

	// Now simulate ACK for line 1
	line1Offset := int64(len(line1))
	if err := tail.Commit(line1Offset); err != nil {
		t.Fatalf("tail.Commit error: %v", err)
	}
	if tail.CommittedOffset() != line1Offset {
		t.Errorf("CommittedOffset() = %d, want %d", tail.CommittedOffset(), line1Offset)
	}

	c, ok := store.Get(tail.key)
	if !ok || c.Offset != line1Offset {
		t.Errorf("store.Get(%q) = %v (ok: %v), want offset %d", tail.key, c, ok, line1Offset)
	}

	// Re-reading from disk without new file writes should be a no-op
	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable no-op error: %v", err)
	}
	if tail.CurrentOffset() != totalBytes {
		t.Errorf("CurrentOffset() = %d, want %d", tail.CurrentOffset(), totalBytes)
	}

	// Now simulate ACK for line 2
	if err := tail.Commit(totalBytes); err != nil {
		t.Fatalf("tail.Commit line 2 error: %v", err)
	}
	if tail.CommittedOffset() != totalBytes {
		t.Errorf("CommittedOffset() = %d, want %d", tail.CommittedOffset(), totalBytes)
	}

	// Lower offset commit should be ignored (monotonic)
	if err := tail.Commit(line1Offset); err != nil {
		t.Fatalf("tail.Commit lower offset error: %v", err)
	}
	if tail.CommittedOffset() != totalBytes {
		t.Errorf("CommittedOffset() after lower commit = %d, want %d", tail.CommittedOffset(), totalBytes)
	}
}

func TestTailerCommitInsideOffsetCallback(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "cursors.json")
	store, _ := NewCursorStore(cursorPath)
	targetFile := filepath.Join(tmpDir, "zero_events.jsonl")

	line1 := "{\"type\":\"session\",\"id\":\"s1\"}\n"
	line2 := "{\"type\":\"thinking_level_change\"}\n" // line with 0 events
	line3 := "{\"type\":\"message\",\"text\":\"hello\"}\n"
	_ = os.WriteFile(targetFile, []byte(line1+line2+line3), 0600)

	var tail *Tailer
	var err error
	var processedLines []string

	tail, err = NewTailerWithOffsetHandler(targetFile, store, func(line []byte, offset int64) error {
		str := string(line)
		processedLines = append(processedLines, str)
		// For line2 (zero events), Commit directly inside the callback, exactly as air-observer does.
		if strings.Contains(str, "thinking_level_change") {
			if cErr := tail.Commit(offset); cErr != nil {
				return cErr
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("NewTailerWithOffsetHandler error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- tail.readAvailable()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("readAvailable error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("DEADLOCK detected: readAvailable() deadlocked when Commit() was called inside onOffset callback")
	}

	if len(processedLines) != 3 {
		t.Fatalf("expected 3 processed lines, got %d: %v", len(processedLines), processedLines)
	}
}

func TestTailerAtomicFileReplacementNewInode(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	targetFile := filepath.Join(tmpDir, "session.jsonl")

	// Initial file: 22 bytes
	_ = os.WriteFile(targetFile, []byte("old line 1\nold line 2\n"), 0600)

	var mu sync.Mutex
	var received []string

	tail, err := NewTailer(targetFile, store, func(line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable error: %v", err)
	}

	mu.Lock()
	if len(received) != 2 {
		t.Fatalf("expected 2 lines initially, got %d", len(received))
	}
	mu.Unlock()

	// Atomic replacement (new inode) with content larger than old offset
	tmpFile := filepath.Join(tmpDir, "session.jsonl.tmp")
	_ = os.WriteFile(tmpFile, []byte("compacted line 1\ncompacted line 2\ncompacted line 3\n"), 0600)
	if err := os.Rename(tmpFile, targetFile); err != nil {
		t.Fatalf("atomic rename failed: %v", err)
	}

	// Next readAvailable should detect inode change and reset offset to 0, reading from start of compacted file
	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable after atomic replace error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Total received should be 2 old lines + 3 compacted lines = 5
	if len(received) != 5 {
		t.Fatalf("expected 5 lines after atomic replace, got %d: %v", len(received), received)
	}
	if received[2] != "compacted line 1" {
		t.Errorf("expected received[2] to be 'compacted line 1', got %q", received[2])
	}
}

func TestTailerSharedDirectoryWatcher(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	fileA := filepath.Join(tmpDir, "subagent-1.jsonl")
	fileB := filepath.Join(tmpDir, "subagent-2.jsonl")
	_ = os.WriteFile(fileA, []byte("init a\n"), 0600)
	_ = os.WriteFile(fileB, []byte("init b\n"), 0600)

	reg, err := NewWatcherRegistry()
	if err != nil {
		t.Fatalf("NewWatcherRegistry error: %v", err)
	}
	defer func() { _ = reg.Close() }()

	tailA, err := NewTailer(fileA, store, func([]byte) error { return nil }, WithWatcherRegistry(reg))
	if err != nil {
		t.Fatalf("NewTailer A error: %v", err)
	}
	tailB, err := NewTailer(fileB, store, func([]byte) error { return nil }, WithWatcherRegistry(reg))
	if err != nil {
		t.Fatalf("NewTailer B error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = tailA.Start(ctx) }()
	go func() { _ = tailB.Start(ctx) }()

	// Wait for tailers to start and register directory watches
	time.Sleep(100 * time.Millisecond)

	// Ref count for tmpDir must be 2
	refCount := reg.DirRefCount(tmpDir)
	if refCount != 2 {
		t.Errorf("expected DirRefCount = 2 for shared directory, got %d", refCount)
	}

	// Underlying fsnotify watcher must only have 1 watched path (tmpDir)
	watchList := reg.WatchList()
	if len(watchList) != 1 {
		t.Errorf("expected 1 fsnotify watch in registry, got %d: %v", len(watchList), watchList)
	}
}

func TestTailerSelectiveDispatchNoWakeup(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	fileA := filepath.Join(tmpDir, "subagent-1.jsonl")
	fileB := filepath.Join(tmpDir, "subagent-2.jsonl")
	_ = os.WriteFile(fileA, []byte("init a\n"), 0600)
	_ = os.WriteFile(fileB, []byte("init b\n"), 0600)

	reg, err := NewWatcherRegistry()
	if err != nil {
		t.Fatalf("NewWatcherRegistry error: %v", err)
	}
	defer func() { _ = reg.Close() }()

	var linesA []string
	var muA sync.Mutex
	tailA, err := NewTailer(fileA, store, func(line []byte) error {
		muA.Lock()
		defer muA.Unlock()
		linesA = append(linesA, string(line))
		return nil
	}, WithWatcherRegistry(reg))
	if err != nil {
		t.Fatal(err)
	}

	var linesB []string
	var muB sync.Mutex
	tailB, err := NewTailer(fileB, store, func(line []byte) error {
		muB.Lock()
		defer muB.Unlock()
		linesB = append(linesB, string(line))
		return nil
	}, WithWatcherRegistry(reg))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = tailA.Start(ctx) }()
	go func() { _ = tailB.Start(ctx) }()

	// Allow initial read
	time.Sleep(100 * time.Millisecond)

	initialWakeA := tailA.WakeCount()
	initialWakeB := tailB.WakeCount()

	// Append to fileA only
	fA, err := os.OpenFile(fileA, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fA.WriteString("appended to a\n")
	_ = fA.Close()

	// Wait for Tailer A to read the new line
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		muA.Lock()
		count := len(linesA)
		muA.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	muA.Lock()
	if len(linesA) != 2 {
		t.Errorf("expected 2 lines in tailA, got %d", len(linesA))
	}
	muA.Unlock()

	// Verify Tailer A was woken up
	wakeDeltaA := tailA.WakeCount() - initialWakeA
	if wakeDeltaA < 1 {
		t.Errorf("expected tailA wakeCount to increase, delta = %d", wakeDeltaA)
	}

	// Verify Tailer B was NOT woken up
	wakeDeltaB := tailB.WakeCount() - initialWakeB
	if wakeDeltaB != 0 {
		t.Errorf("quadratic wakeup detected: tailB wakeCount increased by %d after write to fileA", wakeDeltaB)
	}

	muB.Lock()
	if len(linesB) != 1 {
		t.Errorf("expected tailB to have only initial line, got %d: %v", len(linesB), linesB)
	}
	muB.Unlock()
}

func TestTailerDirectoryWatchCleanupOnStop(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewCursorStore(filepath.Join(tmpDir, "cursors.json"))
	fileA := filepath.Join(tmpDir, "subagent-1.jsonl")
	fileB := filepath.Join(tmpDir, "subagent-2.jsonl")
	_ = os.WriteFile(fileA, []byte("init a\n"), 0600)
	_ = os.WriteFile(fileB, []byte("init b\n"), 0600)

	reg, err := NewWatcherRegistry()
	if err != nil {
		t.Fatalf("NewWatcherRegistry error: %v", err)
	}
	defer func() { _ = reg.Close() }()

	tailA, _ := NewTailer(fileA, store, func([]byte) error { return nil }, WithWatcherRegistry(reg))
	tailB, _ := NewTailer(fileB, store, func([]byte) error { return nil }, WithWatcherRegistry(reg))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = tailA.Start(ctx) }()
	go func() { _ = tailB.Start(ctx) }()

	time.Sleep(100 * time.Millisecond)

	if rc := reg.DirRefCount(tmpDir); rc != 2 {
		t.Fatalf("expected initial DirRefCount = 2, got %d", rc)
	}

	// Stop Tailer A
	tailA.Stop()
	time.Sleep(50 * time.Millisecond)

	if rc := reg.DirRefCount(tmpDir); rc != 1 {
		t.Errorf("expected DirRefCount = 1 after stopping tailA, got %d", rc)
	}
	if wl := reg.WatchList(); len(wl) != 1 {
		t.Errorf("expected 1 watch still active in fsnotify, got %d: %v", len(wl), wl)
	}

	// Stop Tailer B
	tailB.Stop()
	time.Sleep(50 * time.Millisecond)

	if rc := reg.DirRefCount(tmpDir); rc != 0 {
		t.Errorf("expected DirRefCount = 0 after stopping all tailers, got %d", rc)
	}
	if wl := reg.WatchList(); len(wl) != 0 {
		t.Errorf("expected 0 watches in fsnotify after stopping all tailers, got %d: %v", len(wl), wl)
	}
}

func TestWatcherRegistryUnit(t *testing.T) {
	// Test nil WatchList
	var nilReg *WatcherRegistry
	if nilReg.WatchList() != nil {
		t.Errorf("expected nil WatchList for nil registry")
	}

	reg, err := NewWatcherRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()

	if reg.WatchedDirCount() != 0 {
		t.Errorf("expected 0 watched dirs, got %d", reg.WatchedDirCount())
	}
	if reg.SubscriberCount() != 0 {
		t.Errorf("expected 0 subscribers, got %d", reg.SubscriberCount())
	}
	if reg.DirRefCount("/nonexistent/dir") != 0 {
		t.Errorf("expected 0 ref count, got %d", reg.DirRefCount("/nonexistent/dir"))
	}

	tmpDir := t.TempDir()
	f1 := filepath.Join(tmpDir, "file1.txt")
	f2 := filepath.Join(tmpDir, "file2.txt")
	_ = os.WriteFile(f1, []byte("hello"), 0600)
	_ = os.WriteFile(f2, []byte("world"), 0600)

	sub1, err := reg.Subscribe(f1)
	if err != nil {
		t.Fatal(err)
	}
	defer sub1.Close()

	sub2, err := reg.Subscribe(f2)
	if err != nil {
		t.Fatal(err)
	}
	defer sub2.Close()

	// Another subscriber on f1
	sub1b, err := reg.Subscribe(f1)
	if err != nil {
		t.Fatal(err)
	}
	defer sub1b.Close()

	if reg.WatchedDirCount() != 1 {
		t.Errorf("expected 1 watched dir, got %d", reg.WatchedDirCount())
	}
	if reg.SubscriberCount() != 3 {
		t.Errorf("expected 3 subscribers, got %d", reg.SubscriberCount())
	}

	// Test dispatch for relative path fallback
	reg.dispatchEvent(filepath.Base(f1))
	select {
	case <-sub1.Events():
	case <-time.After(100 * time.Millisecond):
		t.Errorf("expected sub1 to receive event on base name dispatch")
	}
	select {
	case <-sub1b.Events():
	case <-time.After(100 * time.Millisecond):
		t.Errorf("expected sub1b to receive event on base name dispatch")
	}

	// Sub2 should not receive event for f1
	select {
	case <-sub2.Events():
		t.Errorf("sub2 should not receive event for f1")
	default:
	}

	// Close registry and verify Subscribe returns ErrRegistryClosed
	_ = reg.Close()
	_, err = reg.Subscribe(f1)
	if !errors.Is(err, ErrRegistryClosed) {
		t.Errorf("expected ErrRegistryClosed, got %v", err)
	}
}

func TestWatcherRegistryDefaultAndReset(t *testing.T) {
	ResetDefaultWatcherRegistry()
	reg1, err := DefaultWatcherRegistry()
	if err != nil {
		t.Fatal(err)
	}
	reg2, err := DefaultWatcherRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if reg1 != reg2 {
		t.Errorf("expected DefaultWatcherRegistry to return same instance")
	}
	ResetDefaultWatcherRegistry()
	// Reset again when already nil
	ResetDefaultWatcherRegistry()
}

func TestWatcherRegistryFallbackRealFilesystem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping directory permission test when running as root")
	}

	tmpDir := t.TempDir()
	restrictedDir := filepath.Join(tmpDir, "restricted")
	if err := os.Mkdir(restrictedDir, 0700); err != nil {
		t.Fatal(err)
	}
	fileA := filepath.Join(restrictedDir, "subagent-1.jsonl")
	fileB := filepath.Join(restrictedDir, "subagent-2.jsonl")
	if err := os.WriteFile(fileA, []byte("init a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("init b\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// 0311: write + execute, no read permission -> directory watching fails, file watching succeeds
	if err := os.Chmod(restrictedDir, 0311); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(restrictedDir, 0700) }()

	reg, err := NewWatcherRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()

	subA, err := reg.Subscribe(fileA)
	if err != nil {
		t.Fatalf("Subscribe fileA error: %v", err)
	}
	defer subA.Close()

	// Verify cleanDir is NOT stored in r.dirs
	if reg.WatchedDirCount() != 0 {
		t.Errorf("expected WatchedDirCount = 0 when dir watch fails, got %d", reg.WatchedDirCount())
	}
	if reg.DirRefCount(restrictedDir) != 0 {
		t.Errorf("expected DirRefCount = 0 for restrictedDir, got %d", reg.DirRefCount(restrictedDir))
	}

	subB, err := reg.Subscribe(fileB)
	if err != nil {
		t.Fatalf("Subscribe fileB error: %v", err)
	}
	defer subB.Close()

	// Both files must be individually watched in fsnotify
	watchList := reg.WatchList()
	if len(watchList) != 2 {
		t.Errorf("expected 2 file watches in WatchList, got %d: %v", len(watchList), watchList)
	}
}

type mockWatcher struct {
	mu        sync.Mutex
	watchlist map[string]struct{}
	addErr    func(path string) error
	events    chan fsnotify.Event
	errors    chan error
	closed    bool
}

func newMockWatcher(addErr func(path string) error) *mockWatcher {
	return &mockWatcher{
		watchlist: make(map[string]struct{}),
		addErr:    addErr,
		events:    make(chan fsnotify.Event, 10),
		errors:    make(chan error, 10),
	}
}

func (m *mockWatcher) Add(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addErr != nil {
		if err := m.addErr(name); err != nil {
			return err
		}
	}
	m.watchlist[name] = struct{}{}
	return nil
}

func (m *mockWatcher) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.watchlist, name)
	return nil
}

func (m *mockWatcher) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.events)
		close(m.errors)
	}
	return nil
}

func (m *mockWatcher) WatchList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	paths := make([]string, 0, len(m.watchlist))
	for p := range m.watchlist {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func TestWatcherRegistryFallbackBehavior(t *testing.T) {
	tmpDir := t.TempDir()
	dir := filepath.Join(tmpDir, "subagents")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file1 := filepath.Join(dir, "subagent-1.jsonl")
	file2 := filepath.Join(dir, "subagent-2.jsonl")
	if err := os.WriteFile(file1, []byte("line1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file2, []byte("line2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Mock watcher where directory Add fails, but file Add succeeds
	mock := newMockWatcher(func(path string) error {
		if path == dir {
			return errors.New("directory watching not supported or permission denied")
		}
		return nil
	})

	reg := newWatcherRegistry(mock, mock.events, mock.errors)
	defer func() { _ = reg.Close() }()

	// 1. Subscribe to file1
	sub1, err := reg.Subscribe(file1)
	if err != nil {
		t.Fatalf("unexpected error subscribing to file1: %v", err)
	}

	// Verify cleanDir is NOT stored in r.dirs
	if reg.WatchedDirCount() != 0 {
		t.Errorf("expected WatchedDirCount = 0, got %d", reg.WatchedDirCount())
	}
	if reg.DirRefCount(dir) != 0 {
		t.Errorf("expected DirRefCount = 0, got %d", reg.DirRefCount(dir))
	}

	// Verify file1 is tracked in fallback files
	if reg.FallbackFileCount() != 1 {
		t.Errorf("expected FallbackFileCount = 1, got %d", reg.FallbackFileCount())
	}
	if reg.FileRefCount(file1) != 1 {
		t.Errorf("expected FileRefCount(file1) = 1, got %d", reg.FileRefCount(file1))
	}
	wl := reg.WatchList()
	if len(wl) != 1 || wl[0] != file1 {
		t.Errorf("expected WatchList = [%s], got %v", file1, wl)
	}

	// 2. Subscribe to file2 in the SAME directory
	// Must NOT assume directory is watched; must attach its own file watch to r.watcher
	sub2, err := reg.Subscribe(file2)
	if err != nil {
		t.Fatalf("unexpected error subscribing to file2: %v", err)
	}

	// Verify cleanDir is STILL NOT stored in r.dirs
	if reg.WatchedDirCount() != 0 {
		t.Errorf("expected WatchedDirCount = 0 after sub2, got %d", reg.WatchedDirCount())
	}
	if reg.DirRefCount(dir) != 0 {
		t.Errorf("expected DirRefCount = 0 after sub2, got %d", reg.DirRefCount(dir))
	}

	// Verify both files are tracked in fallback files
	if reg.FallbackFileCount() != 2 {
		t.Errorf("expected FallbackFileCount = 2, got %d", reg.FallbackFileCount())
	}
	if reg.FileRefCount(file2) != 1 {
		t.Errorf("expected FileRefCount(file2) = 1, got %d", reg.FileRefCount(file2))
	}
	wl = reg.WatchList()
	if len(wl) != 2 {
		t.Errorf("expected 2 watches in WatchList, got %d: %v", len(wl), wl)
	}

	// 3. Second subscription to file1
	sub1b, err := reg.Subscribe(file1)
	if err != nil {
		t.Fatalf("unexpected error subscribing to file1 again: %v", err)
	}
	if reg.FileRefCount(file1) != 2 {
		t.Errorf("expected FileRefCount(file1) = 2, got %d", reg.FileRefCount(file1))
	}
	if reg.SubscriberCount() != 3 {
		t.Errorf("expected SubscriberCount = 3, got %d", reg.SubscriberCount())
	}

	// 4. Test event dispatch for file1
	mock.events <- fsnotify.Event{Name: file1, Op: fsnotify.Write}
	select {
	case <-sub1.Events():
	case <-time.After(100 * time.Millisecond):
		t.Errorf("sub1 did not receive event for file1")
	}
	select {
	case <-sub1b.Events():
	case <-time.After(100 * time.Millisecond):
		t.Errorf("sub1b did not receive event for file1")
	}
	select {
	case <-sub2.Events():
		t.Errorf("sub2 should not have received event for file1")
	default:
	}

	// 5. Test event dispatch for file2
	mock.events <- fsnotify.Event{Name: file2, Op: fsnotify.Write}
	select {
	case <-sub2.Events():
	case <-time.After(100 * time.Millisecond):
		t.Errorf("sub2 did not receive event for file2")
	}
	select {
	case <-sub1.Events():
		t.Errorf("sub1 should not have received event for file2")
	default:
	}

	// 6. Unsubscribe sub1
	sub1.Close()
	if reg.FileRefCount(file1) != 1 {
		t.Errorf("expected FileRefCount(file1) = 1 after sub1.Close(), got %d", reg.FileRefCount(file1))
	}
	if len(reg.WatchList()) != 2 {
		t.Errorf("expected 2 watches remaining in WatchList, got %d", len(reg.WatchList()))
	}

	// 7. Unsubscribe sub1b
	sub1b.Close()
	if reg.FileRefCount(file1) != 0 {
		t.Errorf("expected FileRefCount(file1) = 0 after sub1b.Close(), got %d", reg.FileRefCount(file1))
	}
	wl = reg.WatchList()
	if len(wl) != 1 || wl[0] != file2 {
		t.Errorf("expected WatchList = [%s], got %v", file2, wl)
	}

	// 8. Unsubscribe sub2
	sub2.Close()
	if reg.FileRefCount(file2) != 0 {
		t.Errorf("expected FileRefCount(file2) = 0 after sub2.Close(), got %d", reg.FileRefCount(file2))
	}
	if reg.FallbackFileCount() != 0 {
		t.Errorf("expected FallbackFileCount = 0 after all closed, got %d", reg.FallbackFileCount())
	}
	if len(reg.WatchList()) != 0 {
		t.Errorf("expected WatchList to be empty, got %v", reg.WatchList())
	}

	// 9. Verify error when both dir and file watch fail
	mockBothFail := newMockWatcher(func(path string) error {
		return errors.New("cannot watch")
	})
	regBothFail := newWatcherRegistry(mockBothFail, mockBothFail.events, mockBothFail.errors)
	defer func() { _ = regBothFail.Close() }()

	_, err = regBothFail.Subscribe(file1)
	if err == nil {
		t.Fatalf("expected error when both dir and file watch fail")
	}

	// 10. Nil registry checks for fallback methods
	var nilReg *WatcherRegistry
	if nilReg.FallbackFileCount() != 0 {
		t.Errorf("expected 0 for nilReg.FallbackFileCount()")
	}
	if nilReg.FileRefCount(file1) != 0 {
		t.Errorf("expected 0 for nilReg.FileRefCount()")
	}
	if nilReg.WatchedDirCount() != 0 {
		t.Errorf("expected 0 for nilReg.WatchedDirCount()")
	}
	if nilReg.DirRefCount(dir) != 0 {
		t.Errorf("expected 0 for nilReg.DirRefCount()")
	}
	if nilReg.SubscriberCount() != 0 {
		t.Errorf("expected 0 for nilReg.SubscriberCount()")
	}
}

func TestTailerCommitDebouncedFlush(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	targetFile := filepath.Join(tmpDir, "session.jsonl")
	f, err := os.Create(targetFile)
	if err != nil {
		t.Fatalf("create file failed: %v", err)
	}
	var totalBytes int64
	for i := 0; i < 100; i++ {
		n, _ := fmt.Fprintf(f, `{"type":"event","index":%d}`+"\n", i)
		totalBytes += int64(n)
	}
	_ = f.Close()

	tail, err := NewTailerWithOffsetHandler(targetFile, store, func(line []byte, offset int64) error {
		return nil
	})
	if err != nil {
		t.Fatalf("NewTailerWithOffsetHandler error: %v", err)
	}

	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable error: %v", err)
	}

	initialSaves := store.SaveCount()
	for i := 1; i <= 100; i++ {
		offset := int64(i) * totalBytes / 100
		if i == 100 {
			offset = totalBytes
		}
		if err := tail.Commit(offset); err != nil {
			t.Fatalf("Commit failed: %v", err)
		}
	}

	savesDuringCommits := store.SaveCount() - initialSaves
	if savesDuringCommits == 100 {
		t.Fatalf("expected committing 100 lines NOT to perform 100 atomic file writes, got %d", savesDuringCommits)
	}
	if savesDuringCommits != 0 {
		t.Errorf("expected 0 atomic file writes during commits before flush, got %d", savesDuringCommits)
	}

	// Verify in-memory cursor was updated immediately
	c, ok := store.Get(tail.key)
	if !ok || c.Offset != totalBytes {
		t.Fatalf("store.Get(%q) = %+v, want offset %d", tail.key, c, totalBytes)
	}

	if !store.IsDirty() {
		t.Errorf("store should be dirty after commits")
	}

	// Start auto-flush loop and verify debounced flush behavior
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.StartAutoFlush(ctx, 30*time.Millisecond)

	time.Sleep(80 * time.Millisecond)

	if store.SaveCount() != initialSaves+1 {
		t.Errorf("expected 1 save after debounced flush tick, got %d", store.SaveCount()-initialSaves)
	}
	if store.IsDirty() {
		t.Errorf("store should not be dirty after successful flush")
	}

	// Another tick without commits must be a no-op (no redundant file writes)
	time.Sleep(80 * time.Millisecond)
	if store.SaveCount() != initialSaves+1 {
		t.Errorf("expected no additional saves on clean tick, got %d", store.SaveCount()-initialSaves)
	}

	// Verify compact JSON serialization (no indentations) in cursor file
	cursorBytes, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("reading cursor file: %v", err)
	}
	if bytes.Contains(cursorBytes, []byte("\n  ")) {
		t.Errorf("expected compact JSON without indentation, got:\n%s", string(cursorBytes))
	}
}

func TestTailerAutoCommitDebouncedFlush(t *testing.T) {
	tmpDir := t.TempDir()
	cursorPath := filepath.Join(tmpDir, "cursors.json")
	store, err := NewCursorStore(cursorPath)
	if err != nil {
		t.Fatalf("NewCursorStore error: %v", err)
	}

	targetFile := filepath.Join(tmpDir, "session.jsonl")
	f, err := os.Create(targetFile)
	if err != nil {
		t.Fatalf("create file failed: %v", err)
	}
	lineData := []byte("{\"type\":\"msg\",\"data\":\"hello world\"}\n")
	_, _ = f.Write(lineData)
	_ = f.Close()

	tail, err := NewTailer(targetFile, store, func(line []byte) error {
		return nil
	})
	if err != nil {
		t.Fatalf("NewTailer error: %v", err)
	}

	// In autoCommit mode, readAvailable updates store in-memory without synchronous save
	if err := tail.readAvailable(); err != nil {
		t.Fatalf("readAvailable error: %v", err)
	}

	if store.SaveCount() != 0 {
		t.Errorf("expected 0 saves during autoCommit readAvailable, got %d", store.SaveCount())
	}
	if !store.IsDirty() {
		t.Errorf("expected store to be dirty after autoCommit")
	}

	c, ok := store.Get(tail.key)
	if !ok || c.Offset != int64(len(lineData)) {
		t.Fatalf("store.Get(%q) = %+v, want offset %d", tail.key, c, len(lineData))
	}

	if err := store.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}
	if store.SaveCount() != 1 {
		t.Errorf("expected 1 save after Flush, got %d", store.SaveCount())
	}
	if store.IsDirty() {
		t.Errorf("expected store to not be dirty after Flush")
	}

	// Repeated flush when clean is no-op
	if err := store.Flush(); err != nil {
		t.Fatalf("second Flush error: %v", err)
	}
	if store.SaveCount() != 1 {
		t.Errorf("expected still 1 save after second Flush, got %d", store.SaveCount())
	}
}
