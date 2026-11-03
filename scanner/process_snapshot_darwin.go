//go:build darwin

package scanner

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

// parseComm extracts the process command name from a null-terminated byte slice.
func parseComm(comm []byte) string {
	start := -1
	end := -1
	for i, b := range comm {
		if start == -1 {
			if b == 0 {
				continue
			}
			start = i
		}
		if b == 0 {
			end = i
			break
		}
	}
	if start == -1 {
		return ""
	}
	if end == -1 {
		end = len(comm)
	}
	return string(comm[start:end])
}

// captureProcessSnapshot queries all processes in a single bulk syscall on Darwin
// using unix.SysctlKinfoProcSlice("kern.proc.all").
func captureProcessSnapshot(ctx context.Context, include func(int) bool) (ProcessSnapshot, error) {
	kprocs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("enumerating processes via sysctl: %w", err)
	}

	return captureDarwinProcesses(ctx, kprocs, process.NewProcessWithContext, include)
}

func captureDarwinProcesses(ctx context.Context, kprocs []unix.KinfoProc, newProcess func(context.Context, int32) (*process.Process, error), include func(int) bool) (ProcessSnapshot, error) {
	snap := newCaptureSnapshot(len(kprocs), include)
	for i := range kprocs {
		kp := &kprocs[i]
		pid := int(kp.Proc.P_pid)
		accepted, err := includeProcessPID(ctx, pid, include)
		if err != nil {
			return nil, err
		}
		if !accepted {
			continue
		}

		// In BSD/Darwin, SZOMB is 5 (zombie/dead process)
		alive := kp.Proc.P_stat != 5
		name := parseComm(kp.Proc.P_comm[:])
		createTime := kp.Proc.P_starttime.Sec*1000 + int64(kp.Proc.P_starttime.Usec)/1000

		snap.processes[pid] = ProcessMeta{
			PID:        pid,
			Name:       name,
			CreateTime: createTime,
			Alive:      alive,
		}

		if !alive {
			continue
		}

		snap.inspectCandidate(ctx, pid, name, createTime, func() (*process.Process, error) {
			return newProcess(ctx, int32(pid))
		})
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snap, nil
}
