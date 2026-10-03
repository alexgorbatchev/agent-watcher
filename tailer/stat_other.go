//go:build !darwin && !linux

package tailer

import "os"

func getFileInfoStat(fi os.FileInfo) (uint64, uint64, error) {
	return 0, 0, nil
}
