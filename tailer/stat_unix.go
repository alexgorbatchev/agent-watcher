//go:build darwin || linux

package tailer

import (
	"fmt"
	"os"
	"syscall"
)

func getFileInfoStat(fi os.FileInfo) (uint64, uint64, error) {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("unable to cast sys to syscall.Stat_t")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}
