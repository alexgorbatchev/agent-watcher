//go:build darwin

package scanner

import (
	"context"
	"reflect"
	"testing"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

func TestDarwinCaptureFiltersBeforeInspection(t *testing.T) {
	procs := []unix.KinfoProc{darwinCandidate(11, "node", 1), darwinCandidate(12, "node", 1), darwinCandidate(13, "pi", 5), darwinCandidate(0, "node", 1)}
	var reads []int32
	getProcess := func(ctx context.Context, pid int32) (*process.Process, error) {
		reads = append(reads, pid)
		return nil, process.ErrorProcessNotRunning
	}
	snap, err := captureDarwinProcesses(context.Background(), procs, getProcess, func(pid int) bool { return pid != 11 })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reads, []int32{12}) {
		t.Fatalf("per-process reads = %v, want only included live candidate 12", reads)
	}
	assertProcessExcluded(t, snap, 11)
	meta, ok := snap.Process(12)
	if !ok || meta.Name != "node" || meta.CreateTime != 1234000 || !meta.Alive {
		t.Errorf("included metadata = %+v, found=%t", meta, ok)
	}
	zombie, ok := snap.Process(13)
	if !ok || zombie.Alive {
		t.Errorf("zombie metadata = %+v, found=%t", zombie, ok)
	}
}

func TestDarwinFilteredCaptureCancellation(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		getProcess := func(context.Context, int32) (*process.Process, error) {
			t.Error("per-process read after cancellation")
			return nil, process.ErrorProcessNotRunning
		}
		snap, err := captureDarwinProcesses(ctx, []unix.KinfoProc{darwinCandidate(11, "node", 1)}, getProcess, func(int) bool {
			cancel()
			return accepted
		})
		cancel()
		assertCanceledCapture(t, snap, err)
	}
	t.Run("during last inspection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		getProcess := func(context.Context, int32) (*process.Process, error) {
			cancel()
			return nil, process.ErrorProcessNotRunning
		}
		snap, err := captureDarwinProcesses(ctx, []unix.KinfoProc{darwinCandidate(11, "node", 1)}, getProcess, nil)
		assertCanceledCapture(t, snap, err)
	})
}

func darwinCandidate(pid int32, name string, status int8) unix.KinfoProc {
	var proc unix.KinfoProc
	proc.Proc.P_pid = pid
	proc.Proc.P_stat = status
	proc.Proc.P_starttime.Sec = 1234
	copy(proc.Proc.P_comm[:], name)
	return proc
}

func TestParseComm(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"standard null terminated", []byte{'n', 'o', 'd', 'e', 0, 0, 0}, "node"},
		{"leading null", []byte{0, 'b', 'u', 'n', 0}, "bun"},
		{"no null full slice", []byte{'p', 'i'}, "pi"},
		{"empty slice", []byte{}, ""},
		{"all nulls", []byte{0, 0, 0, 0}, ""},
		{"claude binary", []byte{'c', 'l', 'a', 'u', 'd', 'e', 0}, "claude"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseComm(tt.in)
			if got != tt.want {
				t.Errorf("parseComm(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
