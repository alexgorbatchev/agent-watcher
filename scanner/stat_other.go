//go:build !darwin && !linux

package scanner

import "os"

func getFileDevIno(fi os.FileInfo) (uint64, uint64) {
	return 0, 0
}
