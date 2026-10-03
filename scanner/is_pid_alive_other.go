//go:build !unix && !darwin && !linux

package scanner

import "os"

func isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc != nil
}
