package scanner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureProcessSnapshotLiveCandidates(t *testing.T) {
	for _, tt := range []struct {
		name, argv0, harness string
	}{
		{"pi", "", "pi"},
		{"codex", "", "codex"},
		{"opencode", "", "opencode"},
		{"node", "pi", "pi"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cwd, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(cwd, "main.go")
			if err := os.WriteFile(source, []byte("package main\nimport \"time\"\nfunc main() { time.Sleep(time.Minute) }\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(cwd, "bin"), 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cwd, "bin", tt.name)
			// Copied system binaries exited before discovery on macOS; build a
			// native process fixture that stays alive until test cleanup.
			if output, err := exec.Command("go", "build", "-o", path, source).CombinedOutput(); err != nil {
				t.Fatalf("building process fixture: %v\n%s", err, output)
			}
			cmd := exec.Command(path)
			cmd.Dir = cwd
			if tt.argv0 != "" {
				cmd.Args[0] = tt.argv0
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				snapshot, err := CaptureProcessSnapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var foundCWD string
				var created int64
				switch tt.harness {
				case "pi":
					for _, proc := range snapshot.PiProcesses() {
						if proc.PID == cmd.Process.Pid {
							foundCWD, created = proc.CWD, proc.CreateTime
						}
					}
				case "codex":
					for _, proc := range snapshot.CodexProcesses() {
						if proc.PID == cmd.Process.Pid {
							foundCWD, created = proc.CWD, proc.CreateTime
						}
					}
				case "opencode":
					for _, proc := range snapshot.OpencodeProcesses() {
						if proc.PID == cmd.Process.Pid {
							foundCWD, created = proc.CWD, proc.CreateTime
						}
					}
				}
				if foundCWD != "" {
					if foundCWD != cwd || created <= 0 || !snapshot.VerifyProcessMatch(cmd.Process.Pid, created) {
						t.Fatalf("incorrect process pairing: cwd=%q created=%d", foundCWD, created)
					}
					return
				}
				select {
				case <-ctx.Done():
					meta, _ := snapshot.Process(cmd.Process.Pid)
					t.Fatalf("%s process %d was not discovered: %+v", tt.harness, cmd.Process.Pid, meta)
				case <-tick.C:
				}
			}
		})
	}
}
