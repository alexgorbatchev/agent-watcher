//go:build darwin || linux

package scanner

import (
	"os"
	"syscall"
)

func getFileDevIno(fi os.FileInfo) (uint64, uint64) {
	if fi == nil {
		return 0, 0
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(stat.Dev), uint64(stat.Ino)
}
