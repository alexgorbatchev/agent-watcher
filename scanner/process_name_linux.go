//go:build linux

package scanner

import (
	"context"
	"path/filepath"

	"github.com/shirou/gopsutil/v4/process"
)

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
