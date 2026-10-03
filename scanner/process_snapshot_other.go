//go:build !darwin

package scanner

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/process"
)

// captureProcessSnapshot enumerates processes on non-Darwin platforms using gopsutil,
// filtering candidates on cheap process name first.
func captureProcessSnapshot(ctx context.Context) (ProcessSnapshot, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerating processes: %w", err)
	}

	snap := &LiveProcessSnapshot{
		processes: make(map[int]ProcessMeta, len(procs)),
	}

	for _, p := range procs {
		pid := int(p.Pid)
		if pid <= 0 {
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

		snap.processes[pid] = ProcessMeta{
			PID:        pid,
			Name:       name,
			CreateTime: createTime,
			Alive:      true,
		}

		pRef := p
		snap.inspectCandidate(ctx, pid, name, createTime, func() (*process.Process, error) {
			return pRef, nil
		})
	}

	return snap, nil
}
