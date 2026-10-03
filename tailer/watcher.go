package tailer

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

// ErrRegistryClosed is returned when attempting to subscribe to a closed registry.
var ErrRegistryClosed = errors.New("watcher registry is closed")

var (
	defaultRegistryMu sync.Mutex
	defaultRegistry   *WatcherRegistry
)

// DefaultWatcherRegistry returns the shared process-wide WatcherRegistry.
func DefaultWatcherRegistry() (*WatcherRegistry, error) {
	defaultRegistryMu.Lock()
	defer defaultRegistryMu.Unlock()
	if defaultRegistry == nil || defaultRegistry.closed.Load() {
		reg, err := NewWatcherRegistry()
		if err != nil {
			return nil, err
		}
		defaultRegistry = reg
	}
	return defaultRegistry, nil
}

// ResetDefaultWatcherRegistry closes and resets the default registry.
func ResetDefaultWatcherRegistry() {
	defaultRegistryMu.Lock()
	defer defaultRegistryMu.Unlock()
	if defaultRegistry != nil {
		_ = defaultRegistry.Close()
		defaultRegistry = nil
	}
}

// TailerOption configures a Tailer.
type TailerOption func(*Tailer)

// WithWatcherRegistry specifies a custom WatcherRegistry for a Tailer.
func WithWatcherRegistry(reg *WatcherRegistry) TailerOption {
	return func(t *Tailer) {
		t.registry = reg
	}
}

type fsWatcher interface {
	Add(name string) error
	Remove(name string) error
	Close() error
	WatchList() []string
}

// WatcherRegistry coordinates directory-level filesystem watches across multiple tailers,
// multiplexing fsnotify events and dispatching notifications only to the tailer that owns
// the target file path.
type WatcherRegistry struct {
	watcher     fsWatcher
	eventsCh    <-chan fsnotify.Event
	errorsCh    <-chan error
	mu          sync.Mutex
	dirs        map[string]int
	files       map[string]int
	subscribers map[string]map[*Subscription]struct{}
	closed      atomic.Bool
	closeOnce   sync.Once
	wg          sync.WaitGroup
}

// Subscription represents an active file-change event listener attached to a WatcherRegistry.
type Subscription struct {
	filePath string
	registry *WatcherRegistry
	notifyCh chan struct{}
	doneCh   chan struct{}
	once     sync.Once
	isDir    bool
}

// Events returns a channel signaling file modification or creation events for the subscribed path.
func (s *Subscription) Events() <-chan struct{} {
	return s.notifyCh
}

// Done returns a channel that is closed when the subscription or the underlying registry is closed.
func (s *Subscription) Done() <-chan struct{} {
	return s.doneCh
}

// Close unregisters the subscription, decrements the directory watch reference count,
// and removes the fsnotify directory watch if the count drops to zero.
func (s *Subscription) Close() {
	s.once.Do(func() {
		close(s.doneCh)
		if s.registry != nil {
			s.registry.unsubscribe(s)
		}
	})
}

// NewWatcherRegistry creates and starts a new WatcherRegistry.
func NewWatcherRegistry() (*WatcherRegistry, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating fsnotify watcher: %w", err)
	}

	return newWatcherRegistry(w, w.Events, w.Errors), nil
}

func newWatcherRegistry(w fsWatcher, events <-chan fsnotify.Event, errors <-chan error) *WatcherRegistry {
	r := &WatcherRegistry{
		watcher:     w,
		eventsCh:    events,
		errorsCh:    errors,
		dirs:        make(map[string]int),
		files:       make(map[string]int),
		subscribers: make(map[string]map[*Subscription]struct{}),
	}
	r.wg.Add(1)
	go r.loop()
	return r
}

func (r *WatcherRegistry) loop() {
	defer r.wg.Done()
	for {
		select {
		case event, ok := <-r.eventsCh:
			if !ok {
				r.closeSubscriptions()
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				cleanEventPath := filepath.Clean(event.Name)
				r.dispatchEvent(cleanEventPath)
			}
		case _, ok := <-r.errorsCh:
			if !ok {
				r.closeSubscriptions()
				return
			}
		}
	}
}

func (r *WatcherRegistry) closeSubscriptions() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, subs := range r.subscribers {
		for sub := range subs {
			sub.once.Do(func() {
				close(sub.doneCh)
			})
		}
	}
	r.subscribers = make(map[string]map[*Subscription]struct{})
	r.dirs = make(map[string]int)
	r.files = make(map[string]int)
}

// Close stops the registry, closes the underlying fsnotify watcher, and unregisters all subscribers.
func (r *WatcherRegistry) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		if r.watcher != nil {
			err = r.watcher.Close()
		}
	})
	r.wg.Wait()
	r.closeSubscriptions()
	return err
}

// Subscribe attaches a listener for filePath, adding a directory watch with reference counting
// if the directory is not yet watched. When directory watching fails, it falls back to watching
// the specific file path without caching the directory, allowing subsequent subscriptions for
// other files in the same directory to attach their own file watch.
func (r *WatcherRegistry) Subscribe(filePath string) (*Subscription, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("resolving absolute path for %q: %w", filePath, err)
	}
	cleanFile := filepath.Clean(absPath)
	cleanDir := filepath.Dir(cleanFile)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}

	var isDir bool
	if count, ok := r.dirs[cleanDir]; ok {
		r.dirs[cleanDir] = count + 1
		isDir = true
	} else {
		if err := r.watcher.Add(cleanDir); err == nil {
			r.dirs[cleanDir] = 1
			isDir = true
		} else {
			if fCount, fOk := r.files[cleanFile]; fOk {
				r.files[cleanFile] = fCount + 1
			} else {
				if fErr := r.watcher.Add(cleanFile); fErr != nil {
					return nil, fmt.Errorf("watching directory %q: %w; watching file: %v", cleanDir, err, fErr)
				}
				r.files[cleanFile] = 1
			}
			isDir = false
		}
	}

	sub := &Subscription{
		filePath: cleanFile,
		registry: r,
		notifyCh: make(chan struct{}, 1),
		doneCh:   make(chan struct{}),
		isDir:    isDir,
	}

	if r.subscribers[cleanFile] == nil {
		r.subscribers[cleanFile] = make(map[*Subscription]struct{})
	}
	r.subscribers[cleanFile][sub] = struct{}{}

	return sub, nil
}

func (r *WatcherRegistry) unsubscribe(s *Subscription) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cleanFile := s.filePath
	if subs, ok := r.subscribers[cleanFile]; ok {
		delete(subs, s)
		if len(subs) == 0 {
			delete(r.subscribers, cleanFile)
		}
	}

	if s.isDir {
		cleanDir := filepath.Dir(cleanFile)
		if count, ok := r.dirs[cleanDir]; ok {
			count--
			if count <= 0 {
				if r.watcher != nil && !r.closed.Load() {
					_ = r.watcher.Remove(cleanDir)
				}
				delete(r.dirs, cleanDir)
			} else {
				r.dirs[cleanDir] = count
			}
		}
	} else {
		if count, ok := r.files[cleanFile]; ok {
			count--
			if count <= 0 {
				if r.watcher != nil && !r.closed.Load() {
					_ = r.watcher.Remove(cleanFile)
				}
				delete(r.files, cleanFile)
			} else {
				r.files[cleanFile] = count
			}
		}
	}
}

func (r *WatcherRegistry) dispatchEvent(cleanEventPath string) {
	r.mu.Lock()
	subs := r.subscribers[cleanEventPath]
	if len(subs) == 0 {
		baseName := filepath.Base(cleanEventPath)
		for subPath, fileSubs := range r.subscribers {
			if filepath.Base(subPath) == baseName {
				if filepath.Dir(cleanEventPath) == "." || filepath.Dir(cleanEventPath) == filepath.Dir(subPath) {
					for sub := range fileSubs {
						select {
						case sub.notifyCh <- struct{}{}:
						default:
						}
					}
				}
			}
		}
		r.mu.Unlock()
		return
	}

	for sub := range subs {
		select {
		case sub.notifyCh <- struct{}{}:
		default:
		}
	}
	r.mu.Unlock()
}

// WatchList returns the list of paths actively watched by the underlying fsnotify watcher.
func (r *WatcherRegistry) WatchList() []string {
	if r == nil || r.watcher == nil {
		return nil
	}
	return r.watcher.WatchList()
}

// WatchedDirCount returns the number of unique directories currently tracked in the registry.
func (r *WatcherRegistry) WatchedDirCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dirs)
}

// DirRefCount returns the current reference count for a watched directory.
func (r *WatcherRegistry) DirRefCount(dir string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	cleanDir := filepath.Clean(abs)
	return r.dirs[cleanDir]
}

// FallbackFileCount returns the number of individual files actively watched via fallback.
func (r *WatcherRegistry) FallbackFileCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.files)
}

// FileRefCount returns the current reference count for a fallback watched file.
func (r *WatcherRegistry) FileRefCount(file string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	abs, err := filepath.Abs(file)
	if err != nil {
		abs = file
	}
	cleanFile := filepath.Clean(abs)
	return r.files[cleanFile]
}

// SubscriberCount returns the total number of active subscriptions across all files.
func (r *WatcherRegistry) SubscriberCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, subs := range r.subscribers {
		count += len(subs)
	}
	return count
}
