package scanner

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

func TestIsPlausibleAgentProc(t *testing.T) {
	tests := []struct {
		name     string
		procName string
		want     bool
	}{
		{"pi binary", "pi", true},
		{"pi exe", "pi.exe", true},
		{"pi path", "/usr/local/bin/pi", true},
		{"codex binary", "codex", true},
		{"codex exe", "codex.exe", true},
		{"codex-tui", "codex-tui", true},
		{"codex-cli", "codex-cli", true},
		{"opencode binary", "opencode", true},
		{"opencode exe", "opencode.exe", true},
		{"claude binary", "claude", true},
		{"node runtime", "node", true},
		{"node20", "node20", true},
		{"bun runtime", "bun", true},
		{"python runtime", "python", false},
		{"python3 runtime", "python3", false},
		{"python3.11", "python3.11", false},
		{"electron", "electron", true},
		{"electron helper", "electron-helper", true},
		{"non-agent WindowServer", "WindowServer", false},
		{"non-agent Slack", "Slack", false},
		{"non-agent Chrome", "Google Chrome", false},
		{"non-agent bash", "bash", false},
		{"non-agent zsh", "zsh", false},
		{"non-agent systemd", "systemd", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPlausibleAgentProc(tt.procName)
			if got != tt.want {
				t.Errorf("isPlausibleAgentProc(%q) = %v, want %v", tt.procName, got, tt.want)
			}
		})
	}
}

type testDummySnapshot struct {
	alivePID int
}

func (s *testDummySnapshot) IsProcessAlive(pid int) bool {
	return pid == s.alivePID
}

func (s *testDummySnapshot) VerifyProcessMatch(pid int, expectedStartTime int64) bool {
	return pid == s.alivePID
}

func (s *testDummySnapshot) Process(pid int) (ProcessMeta, bool) {
	if pid == s.alivePID {
		return ProcessMeta{PID: pid, Alive: true}, true
	}
	return ProcessMeta{}, false
}

func (s *testDummySnapshot) PiProcesses() []PiProcessInfo {
	return []PiProcessInfo{{PID: 101, CWD: "/pi"}}
}

func (s *testDummySnapshot) CodexProcesses() []CodexProcessInfo {
	return []CodexProcessInfo{{PID: 102, CWD: "/codex"}}
}

func (s *testDummySnapshot) OpencodeProcesses() []OpencodeProcessInfo {
	return []OpencodeProcessInfo{{PID: 103, CWD: "/opencode"}}
}

func TestTickProcessSnapshotProvider(t *testing.T) {
	t.Run("memoizes snapshot across multiple calls", func(t *testing.T) {
		calls := 0
		underlying := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
			calls++
			return &testDummySnapshot{alivePID: 100}, nil
		})

		tickProvider := NewTickProcessSnapshotProvider(underlying)

		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				snap, err := tickProvider.GetSnapshot(context.Background())
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if !snap.IsProcessAlive(100) {
					t.Errorf("expected PID 100 to be alive")
				}
			}()
		}
		wg.Wait()

		if calls != 1 {
			t.Errorf("expected underlying provider to be called exactly 1 time, got %d", calls)
		}
	})

	t.Run("propagates error and memoizes it", func(t *testing.T) {
		calls := 0
		testErr := errors.New("snapshot failure")
		underlying := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
			calls++
			return nil, testErr
		})

		tickProvider := NewTickProcessSnapshotProvider(underlying)

		snap, err := tickProvider.GetSnapshot(context.Background())
		if !errors.Is(err, testErr) {
			t.Errorf("expected testErr, got %v", err)
		}
		if snap != nil {
			t.Errorf("expected nil snap on error, got %v", snap)
		}

		// Second call returns cached error
		_, err2 := tickProvider.GetSnapshot(context.Background())
		if !errors.Is(err2, testErr) {
			t.Errorf("expected testErr on second call, got %v", err2)
		}
		if calls != 1 {
			t.Errorf("expected calls=1, got %d", calls)
		}
	})

	t.Run("defaults to LiveProcessSnapshotProvider if underlying is nil", func(t *testing.T) {
		tickProvider := NewTickProcessSnapshotProvider(nil)
		if tickProvider.underlying == nil {
			t.Errorf("expected underlying provider to be initialized, got nil")
		}
	})
}

func TestLiveProcessSnapshot(t *testing.T) {
	currentPID := os.Getpid()
	now := time.Now().UnixMilli()

	p, _ := process.NewProcess(int32(currentPID))
	var procCreateTime int64
	if p != nil {
		procCreateTime, _ = p.CreateTime()
	}
	if procCreateTime == 0 {
		procCreateTime = now
	}

	snap := &LiveProcessSnapshot{
		processes: map[int]ProcessMeta{
			currentPID: {
				PID:        currentPID,
				Name:       "test-proc",
				CreateTime: procCreateTime,
				Alive:      true,
			},
			5001: {
				PID:        5001,
				Name:       "pi",
				CreateTime: now,
				Alive:      true,
			},
		},
		piProcs: []PiProcessInfo{
			{PID: 5001, CWD: "/pi/repo", CreateTime: now},
		},
		codexProcs: []CodexProcessInfo{
			{PID: 5002, CWD: "/codex/repo", CreateTime: now},
		},
		opencodeProcs: []OpencodeProcessInfo{
			{PID: 5003, CWD: "/opencode/repo", CreateTime: now},
		},
	}

	t.Run("IsProcessAlive", func(t *testing.T) {
		if snap.IsProcessAlive(0) {
			t.Errorf("PID 0 should not be alive")
		}
		if snap.IsProcessAlive(-1) {
			t.Errorf("PID -1 should not be alive")
		}
		if !snap.IsProcessAlive(currentPID) {
			t.Errorf("currentPID should be alive")
		}
		if !snap.IsProcessAlive(5001) {
			t.Errorf("PID 5001 should be alive")
		}
		if snap.IsProcessAlive(99999999) {
			t.Errorf("PID 99999999 should not be alive")
		}

		t.Setenv("AGENT_STATUS_TEST_MOCK_PROC", "1")
		if !snap.IsProcessAlive(99999999) {
			t.Errorf("expected mock alive with AGENT_STATUS_TEST_MOCK_PROC=1")
		}
	})

	t.Run("VerifyProcessMatch", func(t *testing.T) {
		if snap.VerifyProcessMatch(0, 1000) {
			t.Errorf("PID 0 should not match")
		}
		if snap.VerifyProcessMatch(-1, 1000) {
			t.Errorf("PID -1 should not match")
		}
		if snap.VerifyProcessMatch(99999999, 1000) {
			t.Errorf("non-existent PID should not match")
		}
		if !snap.VerifyProcessMatch(currentPID, 0) {
			t.Errorf("expectedStartTime <= 0 should return true")
		}

		// Exact match with known createTime
		if !snap.VerifyProcessMatch(5001, now) {
			t.Errorf("exact createTime match should be true")
		}
		// 10s difference (drift tolerance is 60s)
		if !snap.VerifyProcessMatch(5001, now+10000) {
			t.Errorf("10s drift should be within tolerance")
		}
		// 120s difference (exceeds tolerance)
		if snap.VerifyProcessMatch(5001, now+120000) {
			t.Errorf("120s drift should exceed tolerance")
		}

		// Verify on currentPID where createTime is fetched dynamically
		if p != nil && procCreateTime > 0 {
			if !snap.VerifyProcessMatch(currentPID, procCreateTime) {
				t.Errorf("currentPID dynamic createTime match should be true")
			}
		}

		// Verify with missing/zero createTime
		snap.processes[6001] = ProcessMeta{PID: 6001, Alive: true, CreateTime: 0}
		if !snap.VerifyProcessMatch(6001, 12345) {
			t.Errorf("alive PID with zero createTime should return true as fallback")
		}

		t.Setenv("AGENT_STATUS_TEST_MOCK_PROC", "1")
		if !snap.VerifyProcessMatch(99999999, 12345) {
			t.Errorf("expected mock match with AGENT_STATUS_TEST_MOCK_PROC=1")
		}
	})

	t.Run("Harness process lists", func(t *testing.T) {
		if len(snap.PiProcesses()) != 1 || snap.PiProcesses()[0].PID != 5001 {
			t.Errorf("PiProcesses mismatch: %+v", snap.PiProcesses())
		}
		if len(snap.CodexProcesses()) != 1 || snap.CodexProcesses()[0].PID != 5002 {
			t.Errorf("CodexProcesses mismatch: %+v", snap.CodexProcesses())
		}
		if len(snap.OpencodeProcesses()) != 1 || snap.OpencodeProcesses()[0].PID != 5003 {
			t.Errorf("OpencodeProcesses mismatch: %+v", snap.OpencodeProcesses())
		}
	})

	t.Run("Process method", func(t *testing.T) {
		meta, ok := snap.Process(currentPID)
		if !ok || meta.PID != currentPID {
			t.Errorf("snap.Process(currentPID) = %+v, %v", meta, ok)
		}
		_, ok = snap.Process(99999999)
		if ok {
			t.Errorf("snap.Process(99999999) expected false")
		}
		var emptySnap LiveProcessSnapshot
		_, ok = emptySnap.Process(1)
		if ok {
			t.Errorf("emptySnap.Process(1) expected false")
		}
	})
}

func TestSnapshotProcessFinders(t *testing.T) {
	dummy := &testDummySnapshot{alivePID: 100}

	okProvider := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
		return dummy, nil
	})

	errProvider := ProcessSnapshotProviderFunc(func(ctx context.Context) (ProcessSnapshot, error) {
		return nil, errors.New("failed")
	})

	t.Run("SnapshotPiProcessFinder", func(t *testing.T) {
		finder := SnapshotPiProcessFinder(okProvider)
		procs, err := finder(context.Background())
		if err != nil || len(procs) != 1 || procs[0].PID != 101 {
			t.Errorf("SnapshotPiProcessFinder got procs=%+v, err=%v", procs, err)
		}

		errFinder := SnapshotPiProcessFinder(errProvider)
		_, err = errFinder(context.Background())
		if err == nil {
			t.Errorf("expected error from SnapshotPiProcessFinder")
		}
	})

	t.Run("SnapshotCodexProcessFinder", func(t *testing.T) {
		finder := SnapshotCodexProcessFinder(okProvider)
		procs, err := finder(context.Background())
		if err != nil || len(procs) != 1 || procs[0].PID != 102 {
			t.Errorf("SnapshotCodexProcessFinder got procs=%+v, err=%v", procs, err)
		}

		errFinder := SnapshotCodexProcessFinder(errProvider)
		_, err = errFinder(context.Background())
		if err == nil {
			t.Errorf("expected error from SnapshotCodexProcessFinder")
		}
	})

	t.Run("SnapshotOpencodeProcessFinder", func(t *testing.T) {
		finder := SnapshotOpencodeProcessFinder(okProvider)
		procs, err := finder(context.Background())
		if err != nil || len(procs) != 1 || procs[0].PID != 103 {
			t.Errorf("SnapshotOpencodeProcessFinder got procs=%+v, err=%v", procs, err)
		}

		errFinder := SnapshotOpencodeProcessFinder(errProvider)
		_, err = errFinder(context.Background())
		if err == nil {
			t.Errorf("expected error from SnapshotOpencodeProcessFinder")
		}
	})
}

func TestCaptureProcessSnapshot(t *testing.T) {
	snap, err := CaptureProcessSnapshot(context.Background())
	if err != nil {
		t.Fatalf("CaptureProcessSnapshot error: %v", err)
	}
	if snap == nil {
		t.Fatalf("expected non-nil snapshot")
	}

	// Current PID must be recognized as alive
	currentPID := os.Getpid()
	if !snap.IsProcessAlive(currentPID) {
		t.Errorf("expected current process PID %d to be recognized as alive", currentPID)
	}

	// LiveProcessSnapshotProvider test
	liveProvider := NewLiveProcessSnapshotProvider()
	liveSnap, err := liveProvider.GetSnapshot(context.Background())
	if err != nil || liveSnap == nil {
		t.Fatalf("LiveProcessSnapshotProvider.GetSnapshot error: %v", err)
	}
}

func TestInspectCandidate_Filtering(t *testing.T) {
	ctx := context.Background()

	t.Run("non-agent process does not call procGetter", func(t *testing.T) {
		snap := &LiveProcessSnapshot{}
		procGetterCalled := false
		snap.inspectCandidate(ctx, 123, "python3", 1000, func() (*process.Process, error) {
			procGetterCalled = true
			return nil, nil
		})
		if procGetterCalled {
			t.Errorf("expected procGetter not called for python3")
		}
	})

	t.Run("claude process does not call procGetter or cmdline", func(t *testing.T) {
		snap := &LiveProcessSnapshot{}
		procGetterCalled := false
		snap.inspectCandidate(ctx, 456, "claude", 1000, func() (*process.Process, error) {
			procGetterCalled = true
			return nil, nil
		})
		if procGetterCalled {
			t.Errorf("expected procGetter not called for claude")
		}
		if len(snap.piProcs) != 0 || len(snap.codexProcs) != 0 || len(snap.opencodeProcs) != 0 {
			t.Errorf("claude process should not be added to Pi/Codex/Opencode lists")
		}
	})

	t.Run("non-interpreter candidate does not fetch cmdline", func(t *testing.T) {
		// e.g. a process named "pi" does not need cmdline inspection
		// procGetter will be called for CWD only
		snap := &LiveProcessSnapshot{}
		snap.inspectCandidate(ctx, 789, "pi", 1000, func() (*process.Process, error) {
			// Returns nil process so CWD fails gracefully
			return nil, errors.New("simulated proc error")
		})
		if len(snap.piProcs) != 0 {
			t.Errorf("expected 0 piProcs when proc getter fails")
		}
	})
}

func TestIsPIDAliveProbe(t *testing.T) {
	currentPID := os.Getpid()
	if !isPIDAlive(currentPID) {
		t.Errorf("isPIDAlive(%d) = false, want true", currentPID)
	}
	if isPIDAlive(-1) {
		t.Errorf("isPIDAlive(-1) = true, want false")
	}
	if isPIDAlive(0) {
		t.Errorf("isPIDAlive(0) = true, want false")
	}
	if isPIDAlive(99999999) {
		t.Errorf("isPIDAlive(99999999) = true, want false")
	}
}
