package scanner

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/shirou/gopsutil/v4/process"
)

func TestCaptureProcessIDsFiltersBeforeMetadata(t *testing.T) {
	pid := int32(os.Getpid())
	var predicates, reads []int32
	getProcess := func(ctx context.Context, id int32) (*process.Process, error) {
		reads = append(reads, id)
		if id != pid {
			return nil, process.ErrorProcessNotRunning
		}
		return process.NewProcessWithContext(ctx, id)
	}
	snap, err := captureProcessIDs(context.Background(), []int32{-1, 0, pid + 1, pid, pid + 2}, getProcess, func(id int) bool {
		predicates = append(predicates, int32(id))
		return int32(id) == pid
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reads, []int32{pid}) {
		t.Errorf("metadata construction called for %v, want only %d", reads, pid)
	}
	if !reflect.DeepEqual(predicates, []int32{pid + 1, pid, pid + 2}) {
		t.Errorf("predicate PIDs = %v", predicates)
	}
	meta, ok := snap.Process(int(pid))
	if !ok || !meta.Alive || meta.Name == "" || meta.CreateTime <= 0 {
		t.Errorf("included process metadata = %+v, found=%t", meta, ok)
	}
	assertProcessExcluded(t, snap, int(pid+1))
}

func TestCaptureProcessIDsDisappearingProcess(t *testing.T) {
	pid := int32(os.Getpid())
	getProcess := func(ctx context.Context, id int32) (*process.Process, error) {
		if id == pid+1 {
			return nil, process.ErrorProcessNotRunning
		}
		return process.NewProcessWithContext(ctx, id)
	}
	snap, err := captureProcessIDs(context.Background(), []int32{pid + 1, pid}, getProcess, nil)
	if err != nil || snap == nil || !snap.IsProcessAlive(int(pid)) {
		t.Fatalf("process disappearance interrupted capture: snapshot_nil=%t err=%v", snap == nil, err)
	}
	assertProcessExcluded(t, snap, int(pid+1))
}

func TestCaptureFilteredProcessSnapshot(t *testing.T) {
	pid := os.Getpid()
	for _, tt := range []struct {
		name    string
		include func(int) bool
		alive   bool
	}{
		{"nil includes all", nil, true},
		{"self only", func(id int) bool { return id == pid }, true},
		{"exclude all", func(int) bool { return false }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := CaptureFilteredProcessSnapshot(context.Background(), tt.include)
			if err != nil {
				t.Fatal(err)
			}
			if snap.IsProcessAlive(pid) != tt.alive {
				t.Fatalf("IsProcessAlive(self) = %t", snap.IsProcessAlive(pid))
			}
			if !tt.alive {
				assertProcessExcluded(t, snap, pid)
				if len(snap.PiProcesses())+len(snap.CodexProcesses())+len(snap.OpencodeProcesses()) != 0 {
					t.Fatal("excluded candidates remained in snapshot")
				}
			}
		})
	}
}

func TestFilteredCaptureCancellation(t *testing.T) {
	t.Run("before capture", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		snap, err := CaptureFilteredProcessSnapshot(ctx, func(int) bool {
			t.Error("predicate called after cancellation")
			return true
		})
		assertCanceledCapture(t, snap, err)
	})
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected PID", true: "accepted PID"}[accepted], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			getProcess := func(context.Context, int32) (*process.Process, error) {
				t.Error("metadata read after predicate canceled capture")
				return nil, process.ErrorProcessNotRunning
			}
			snap, err := captureProcessIDs(ctx, []int32{1, 2}, getProcess, func(int) bool {
				cancel()
				return accepted
			})
			assertCanceledCapture(t, snap, err)
		})
	}
	t.Run("during last metadata read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		getProcess := func(context.Context, int32) (*process.Process, error) {
			cancel()
			return nil, process.ErrorProcessNotRunning
		}
		snap, err := captureProcessIDs(ctx, []int32{1}, getProcess, nil)
		assertCanceledCapture(t, snap, err)
	})
}

func assertCanceledCapture(t *testing.T, snap ProcessSnapshot, err error) {
	t.Helper()
	if snap != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("canceled capture returned snapshot_nil=%t err=%v", snap == nil, err)
	}
}

func assertProcessExcluded(t *testing.T, snap ProcessSnapshot, pid int) {
	t.Helper()
	_, ok := snap.Process(pid)
	if ok || snap.IsProcessAlive(pid) || snap.VerifyProcessMatch(pid, 0) {
		t.Errorf("excluded PID %d remained in snapshot", pid)
	}
}
