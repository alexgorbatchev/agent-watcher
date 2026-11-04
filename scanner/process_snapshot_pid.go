package scanner

import (
	"context"
	"path/filepath"

	"github.com/shirou/gopsutil/v4/process"
)

// Enumerate PIDs before constructing processes: gopsutil's constructor reads
// create time, so ProcessesWithContext would read metadata before filtering.
func captureProcessIDs(ctx context.Context, pids []int32, newProcess func(context.Context, int32) (*process.Process, error), include func(int) bool) (ProcessSnapshot, error) {
	snap := newCaptureSnapshot(len(pids), include)
	for _, id := range pids {
		pid := int(id)
		accepted, err := includeProcessPID(ctx, pid, include)
		if err != nil {
			return nil, err
		}
		if !accepted {
			continue
		}
		p, err := newProcess(ctx, id)
		if err != nil {
			continue
		}
		name, err := p.NameWithContext(ctx)
		if err != nil {
			continue
		}
		createTime, err := p.CreateTimeWithContext(ctx)
		if err != nil {
			createTime = 0
		}
		snap.processes[pid] = ProcessMeta{PID: pid, Name: name, CreateTime: createTime, Alive: true}
		snap.inspectCandidate(ctx, pid, classificationName(ctx, p, name), createTime, func() (*process.Process, error) {
			return p, nil
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snap, nil
}

func newCaptureSnapshot(count int, include func(int) bool) *LiveProcessSnapshot {
	// Scoped captures should allocate for accepted PIDs, not the whole host.
	if include != nil {
		count = 0
	}
	return &LiveProcessSnapshot{processes: make(map[int]ProcessMeta, count)}
}

func includeProcessPID(ctx context.Context, pid int, include func(int) bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if pid <= 0 {
		return false, nil
	}
	accepted := include == nil || include(pid)
	return accepted, ctx.Err()
}

// classificationName returns the name to classify a process by. On Linux the
// reported name is the main thread's, which runtimes rename: Node 24 reports
// "MainThread". When that name is not a harness or interpreter, the
// executable's name is used instead, so such a process is still inspected.
func classificationName(ctx context.Context, p *process.Process, name string) string {
	if isPlausibleAgentProc(name) {
		return name
	}
	exe, err := p.ExeWithContext(ctx)
	if err != nil || exe == "" {
		return name
	}
	return filepath.Base(exe)
}
