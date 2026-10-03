//go:build unix || darwin || linux

package scanner

import (
	"errors"

	"golang.org/x/sys/unix"
)

func isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
