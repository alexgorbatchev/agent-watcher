package tailer

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var tailerPollInterval = 10 * time.Second

type LineHandler func(line []byte) error

type LineOffsetHandler func(line []byte, offset int64) error

type Tailer struct {
	filePath string
	store    *CursorStore
	onLine   LineHandler
	onOffset LineOffsetHandler

	registry        *WatcherRegistry
	stopCh          chan struct{}
	once            sync.Once
	wakeCount       int64
	mu              sync.Mutex
	readMu          sync.Mutex
	offset          int64
	committedOffset int64
	autoCommit      bool
	key             string
	prefixHash      string
	fileSize        int64
	inode           uint64
	device          uint64
	generation      uint64
}

// NewTailer creates a tailer using the classic LineHandler (auto-commits read offsets to cursor store).
func NewTailer(filePath string, store *CursorStore, onLine LineHandler, opts ...TailerOption) (*Tailer, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("resolving absolute path for %q: %w", filePath, err)
	}

	t := &Tailer{
		filePath:   absPath,
		store:      store,
		onLine:     onLine,
		stopCh:     make(chan struct{}),
		autoCommit: true,
	}
	for _, opt := range opts {
		opt(t)
	}
	if t.registry == nil {
		reg, err := DefaultWatcherRegistry()
		if err != nil {
			return nil, fmt.Errorf("getting default watcher registry: %w", err)
		}
		t.registry = reg
	}
	return t, nil
}

// NewTailerWithOffsetHandler creates a tailer using LineOffsetHandler with explicit ACK-committed delivery.
func NewTailerWithOffsetHandler(filePath string, store *CursorStore, onOffset LineOffsetHandler, opts ...TailerOption) (*Tailer, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("resolving absolute path for %q: %w", filePath, err)
	}

	t := &Tailer{
		filePath:   absPath,
		store:      store,
		onOffset:   onOffset,
		stopCh:     make(chan struct{}),
		autoCommit: false,
	}
	for _, opt := range opts {
		opt(t)
	}
	if t.registry == nil {
		reg, err := DefaultWatcherRegistry()
		if err != nil {
			return nil, fmt.Errorf("getting default watcher registry: %w", err)
		}
		t.registry = reg
	}
	return t, nil
}

func (t *Tailer) Start(ctx context.Context) error {
	sub, err := t.registry.Subscribe(t.filePath)
	if err != nil {
		return err
	}
	defer sub.Close()

	// Initial read
	_ = t.readAvailable()

	ticker := time.NewTicker(tailerPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.stopCh:
			return nil
		case <-sub.Done():
			return nil
		case <-sub.Events():
			atomic.AddInt64(&t.wakeCount, 1)
			_ = t.readAvailable()
		case <-ticker.C:
			_ = t.readAvailable()
		}
	}
}

func (t *Tailer) Stop() {
	t.once.Do(func() {
		close(t.stopCh)
	})
}

// WakeCount returns the number of times this tailer was woken up by a watcher event.
func (t *Tailer) WakeCount() int64 {
	return atomic.LoadInt64(&t.wakeCount)
}

// CurrentOffset returns the in-memory read position of the tailer.
func (t *Tailer) CurrentOffset() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.offset
}

// CommittedOffset returns the highest confirmed/ACKed offset persisted to CursorStore.
func (t *Tailer) CommittedOffset() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.committedOffset
}

// Commit updates the cursor store to the confirmed offset.
func (t *Tailer) Commit(offset int64) error {
	return t.commit(offset, 0)
}

// Checkpoint binds an acknowledgement to the current file generation. A late
// acknowledgement cannot advance a replacement or truncated file's cursor.
func (t *Tailer) Checkpoint(offset int64) (uint64, func() error) {
	t.mu.Lock()
	generation := t.generation
	t.mu.Unlock()
	return generation, func() error { return t.commit(offset, generation) }
}

func (t *Tailer) commit(offset int64, generation uint64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if (generation != 0 && generation != t.generation) || offset <= t.committedOffset {
		return nil
	}
	t.committedOffset = offset
	key := t.key
	prefixHash := t.prefixHash
	size := t.fileSize
	ino := t.inode
	dev := t.device
	store := t.store
	filePath := t.filePath

	if store != nil && key != "" {
		if size == 0 && ino == 0 {
			if fi, err := os.Stat(filePath); err == nil {
				size = fi.Size()
				dev, ino, _ = getFileInfoStat(fi)
			}
		}
		store.Set(&Cursor{
			Key:        key,
			Path:       filePath,
			Offset:     offset,
			Size:       size,
			Inode:      ino,
			Device:     dev,
			PrefixHash: prefixHash,
			UpdatedAt:  time.Now().UnixMilli(),
		})
	}
	return nil
}

func (t *Tailer) readAvailable() error {
	t.readMu.Lock()
	defer t.readMu.Unlock()

	f, err := os.Open(t.filePath)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	dev, ino, _ := getFileInfoStat(fi)
	key := fmt.Sprintf("%d:%d", dev, ino)
	if dev == 0 && ino == 0 {
		key = t.filePath
	}

	prefixHash, _ := ComputePrefixHash(f, fi.Size())

	t.mu.Lock()
	if t.key == "" || t.key != key || t.offset > fi.Size() || (t.prefixHash != "" && prefixHash != "" && t.prefixHash != prefixHash) {
		t.generation++
		t.offset = 0
		t.committedOffset = 0
	}
	t.key = key
	t.prefixHash = prefixHash
	t.fileSize = fi.Size()
	t.device = dev
	t.inode = ino

	// Initialize offset from cursor store on first run
	if t.offset == 0 && t.committedOffset == 0 {
		if c, ok := t.store.Get(key); ok {
			if ShouldResetCursor(c, ino, fi.Size(), prefixHash) {
				t.offset = 0
				t.committedOffset = 0
			} else {
				t.offset = c.Offset
				t.committedOffset = c.Offset
			}
		}
	}

	if t.offset > fi.Size() {
		t.offset = 0
		t.committedOffset = 0
	}

	startOffset := t.offset
	t.mu.Unlock()

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return fmt.Errorf("seeking to offset %d: %w", startOffset, err)
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	bytesReadTotal := startOffset

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] == '\n' {
				lineBytes := int64(len(line))
				cleanLine := bytes.TrimRight(line, "\r\n")
				if len(cleanLine) > 0 {
					newLineOffset := bytesReadTotal + lineBytes
					var hErr error
					if t.onOffset != nil {
						hErr = t.onOffset(cleanLine, newLineOffset)
					} else if t.onLine != nil {
						hErr = t.onLine(cleanLine)
					}

					if hErr != nil {
						// Handler signaled backpressure or error; stop reading immediately
						// without advancing read offset past this unhandled line.
						break
					}
				}
				bytesReadTotal += lineBytes
				t.mu.Lock()
				t.offset = bytesReadTotal
				t.mu.Unlock()
			} else {
				// Partial line, stop reading for now, wait for newline
				break
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	// In autoCommit mode, update CursorStore upon reading
	if t.autoCommit {
		t.mu.Lock()
		shouldSave := bytesReadTotal > t.committedOffset
		if shouldSave {
			t.committedOffset = bytesReadTotal
		}
		key := t.key
		store := t.store
		filePath := t.filePath
		t.mu.Unlock()

		if shouldSave && store != nil && key != "" {
			store.Set(&Cursor{
				Key:        key,
				Path:       filePath,
				Offset:     bytesReadTotal,
				Size:       fi.Size(),
				Inode:      ino,
				Device:     dev,
				PrefixHash: prefixHash,
				UpdatedAt:  time.Now().UnixMilli(),
			})
		}
	}

	return nil
}
