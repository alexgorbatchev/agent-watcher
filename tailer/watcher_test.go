package tailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/fsnotify/fsnotify"
)

func TestWatcherErrors(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("file_watch=%t", fallback), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var logs bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&logs, nil))
				mock := newMockWatcher(func(path string) error {
					if fallback && filepath.Ext(path) == "" {
						return errors.New("directory watch unavailable")
					}
					return nil
				})
				reg := newWatcherRegistry(mock, mock.events, mock.errors, WithWatcherLogger(logger))
				defer func() { _ = reg.Close() }()
				dir := t.TempDir()
				paths := []string{filepath.Join(dir, "a", "session.jsonl"), filepath.Join(dir, "b", "session.jsonl")}
				var subs []*Subscription
				for _, path := range []string{paths[0], paths[0], paths[1]} {
					sub, err := reg.Subscribe(path)
					if err != nil {
						t.Fatal(err)
					}
					subs = append(subs, sub)
				}
				ordinary := errors.New("watch failed")
				mock.errors <- ordinary
				synctest.Wait()
				assertWatcherLog(t, &logs, ordinary)
				for _, sub := range subs {
					assertNoWake(t, sub)
				}

				// An already pending wake must not block recovery of other files.
				subs[0].notifyCh <- struct{}{}
				overflow := fmt.Errorf("backend: %w", fsnotify.ErrEventOverflow)
				mock.errors <- overflow
				synctest.Wait()
				assertWatcherLog(t, &logs, overflow)
				for _, sub := range subs {
					assertWake(t, sub)
					assertNoWake(t, sub)
					select {
					case <-sub.Done():
						t.Fatal("overflow closed a subscription")
					default:
					}
				}
				if reg.SubscriberCount() != len(subs) || len(reg.WatchList()) != 2 {
					t.Fatal("overflow lost subscriptions or watches")
				}

				mock.events <- fsnotify.Event{Name: paths[0], Op: fsnotify.Write}
				synctest.Wait()
				assertWake(t, subs[0])
				assertWake(t, subs[1])
				assertNoWake(t, subs[2])
				subs[0].Close()
				mock.errors <- fsnotify.ErrEventOverflow
				synctest.Wait()
				assertWatcherLog(t, &logs, fsnotify.ErrEventOverflow)
				assertNoWake(t, subs[0])
				assertWake(t, subs[1])
				assertWake(t, subs[2])
			})
		})
	}
}

func TestWatcherDefaultLogger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
		defer slog.SetDefault(previous)
		mock := newMockWatcher(nil)
		reg := newWatcherRegistry(mock, mock.events, mock.errors, WithWatcherLogger(nil))
		defer func() { _ = reg.Close() }()
		err := errors.New("default logger error")
		mock.errors <- err
		synctest.Wait()
		assertWatcherLog(t, &logs, err)
	})
}

func TestWatcherClosedChannelEndsSubscriptions(t *testing.T) {
	for _, channel := range []string{"events", "errors"} {
		t.Run(channel, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mock := newMockWatcher(nil)
				events := make(chan fsnotify.Event)
				errs := make(chan error)
				reg := newWatcherRegistry(mock, events, errs)
				defer func() { _ = reg.Close() }()
				path := filepath.Join(t.TempDir(), "session.jsonl")
				sub, err := reg.Subscribe(path)
				if err != nil {
					t.Fatal(err)
				}
				if channel == "events" {
					close(events)
				} else {
					close(errs)
				}
				synctest.Wait()
				select {
				case <-sub.Done():
				default:
					t.Fatal("channel closure did not end subscription")
				}
				if _, err := reg.Subscribe(path); !errors.Is(err, ErrRegistryClosed) {
					t.Fatalf("Subscribe after channel closure = %v", err)
				}
			})
		})
	}
}

func TestWatcherDispatchRequiresExactPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mock := newMockWatcher(nil)
		reg := newWatcherRegistry(mock, mock.events, mock.errors)
		defer func() { _ = reg.Close() }()
		sub, err := reg.Subscribe(filepath.Join(t.TempDir(), "session.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		mock.events <- fsnotify.Event{Name: "session.jsonl", Op: fsnotify.Write}
		synctest.Wait()
		assertNoWake(t, sub)
	})
}

func TestWatcherShutdownDuringSubscriptionClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mock := newMockWatcher(nil)
		reg := newWatcherRegistry(mock, mock.events, mock.errors)
		sub, err := reg.Subscribe(filepath.Join(t.TempDir(), "session.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		// Hold registry bookkeeping so Close signals Done before it can detach.
		reg.mu.Lock()
		done := make(chan struct{})
		go func() {
			sub.Close()
			close(done)
		}()
		<-sub.Done()
		reg.mu.Unlock()
		reg.closeSubscriptions()
		<-done
		if err := reg.Close(); err != nil {
			t.Fatal(err)
		}
		if reg.SubscriberCount() != 0 {
			t.Fatal("shutdown retained subscriptions")
		}
	})
}

func TestTailerReadsAfterWatcherOverflow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mock := newMockWatcher(nil)
		reg := newWatcherRegistry(mock, mock.events, mock.errors)
		defer func() { _ = reg.Close() }()
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte("first\n"), 0600); err != nil {
			t.Fatal(err)
		}
		lines := make(chan string, 2)
		store, err := NewCursorStore(filepath.Join(t.TempDir(), "cursors.json"))
		if err != nil {
			t.Fatal(err)
		}
		tailer, err := NewTailer(path, store, func(line []byte) error {
			lines <- string(line)
			return nil
		}, WithWatcherRegistry(reg))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tailer.Start(ctx) }()
		if got := <-lines; got != "first" {
			t.Fatalf("initial line = %q", got)
		}
		synctest.Wait()
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("second\n"); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		mock.errors <- fsnotify.ErrEventOverflow
		synctest.Wait()
		select {
		case got := <-lines:
			if got != "second" {
				t.Fatalf("recovered line = %q", got)
			}
		default:
			t.Error("overflow did not re-read appended data before polling")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Start after cancellation = %v", err)
		}
		if reg.SubscriberCount() != 0 {
			t.Fatal("cancellation retained subscription")
		}
	})
}

func assertWatcherLog(t *testing.T, logs *bytes.Buffer, want error) {
	t.Helper()
	var record map[string]any
	if err := json.NewDecoder(logs).Decode(&record); err != nil {
		t.Errorf("missing structured watcher error: %v", err)
		return
	}
	if record["msg"] != "watcher_error" || record["level"] != "WARN" || record["err"] != want.Error() {
		t.Errorf("watcher log = %v", record)
	}
}

func assertWake(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case <-sub.Events():
	default:
		t.Error("subscription was not woken")
	}
}

func assertNoWake(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case <-sub.Events():
		t.Error("unexpected subscription wake")
	default:
	}
}
