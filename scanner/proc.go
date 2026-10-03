package scanner

import (
	"math"
	"os"

	"github.com/shirou/gopsutil/v4/process"
)

func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if os.Getenv("AGENT_STATUS_TEST_MOCK_PROC") == "1" {
		return true
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	running, err := p.IsRunning()
	if err != nil {
		return false
	}
	return running
}

func VerifyProcessMatch(pid int, expectedStartTime int64) bool {
	if pid <= 0 {
		return false
	}
	if os.Getenv("AGENT_STATUS_TEST_MOCK_PROC") == "1" {
		return true
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	running, err := p.IsRunning()
	if err != nil || !running {
		return false
	}

	if expectedStartTime <= 0 {
		return true
	}

	createTime, err := p.CreateTime()
	if err != nil {
		return true // If create time is unavailable, assume alive
	}

	// Allow up to 60 seconds clock / initialization drift between proc start and session record
	diff := math.Abs(float64(createTime - expectedStartTime))
	return diff < 60000
}

func GetProcessCWD(pid int) (string, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return "", err
	}
	return p.Cwd()
}
