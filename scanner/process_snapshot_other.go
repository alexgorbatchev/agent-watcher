//go:build !darwin

package scanner

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/process"
)

// captureProcessSnapshot enumerates PIDs before reading metadata on non-Darwin platforms.
func captureProcessSnapshot(ctx context.Context, include func(int) bool) (ProcessSnapshot, error) {
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerating processes: %w", err)
	}

	return captureProcessIDs(ctx, pids, process.NewProcessWithContext, include)
}
