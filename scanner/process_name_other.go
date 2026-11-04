//go:build !linux

package scanner

import (
	"context"

	"github.com/shirou/gopsutil/v4/process"
)

// classificationName returns the reported process name. Outside Linux that name
// comes from the executable, not a renamable thread name: Windows derives it
// from the image path itself, so reading the executable again would only repeat
// that lookup for every process on every scan.
func classificationName(_ context.Context, _ *process.Process, name string) string {
	return name
}
