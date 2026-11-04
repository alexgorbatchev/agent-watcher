//go:build linux

package scanner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

const renamedInterpreterEnv = "AGENT_WATCHER_TEST_RENAMED_INTERPRETER"

// TestRenamedInterpreterHelper is the child process for
// TestCaptureClassifiesRenamedInterpreter; it does nothing in a normal run.
func TestRenamedInterpreterHelper(t *testing.T) {
	if os.Getenv(renamedInterpreterEnv) != "1" {
		return
	}
	// Node 24 renames its main thread the same way.
	if err := os.WriteFile("/proc/self/comm", []byte("MainThread"), 0); err != nil {
		os.Exit(2)
	}
	select {}
}

func TestCaptureClassifiesRenamedInterpreter(t *testing.T) {
	dir := t.TempDir()
	interpreter := filepath.Join(dir, "node")
	copyExecutable(t, os.Args[0], interpreter)
	workspace := filepath.Join(dir, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(interpreter, "-test.run=^TestRenamedInterpreterHelper$", "--", filepath.Join(dir, "pi"))
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), renamedInterpreterEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for {
		comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
		if err == nil && strings.TrimSpace(string(comm)) == "MainThread" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper never renamed its thread: comm=%q err=%v", comm, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	snap, err := captureProcessIDs(context.Background(), []int32{int32(pid)}, process.NewProcessWithContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta, ok := snap.Process(pid); !ok || meta.Name != "MainThread" {
		t.Fatalf("process meta = %+v, %v; want the reported thread name kept", meta, ok)
	}
	procs := snap.PiProcesses()
	if len(procs) != 1 || procs[0].PID != pid || procs[0].CWD != workspace {
		t.Fatalf("pi processes = %+v, want pid %d in %s", procs, pid, workspace)
	}
}

func copyExecutable(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}
