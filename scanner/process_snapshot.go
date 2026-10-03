package scanner

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/shirou/gopsutil/v4/process"
)

// ProcessMeta stores lightweight scalar metadata for a single process.
type ProcessMeta struct {
	PID        int
	Name       string
	CreateTime int64
	Alive      bool
}

// ProcessSnapshot provides shared process information captured at a single point in time.
type ProcessSnapshot interface {
	IsProcessAlive(pid int) bool
	VerifyProcessMatch(pid int, expectedStartTime int64) bool
	PiProcesses() []PiProcessInfo
	CodexProcesses() []CodexProcessInfo
	OpencodeProcesses() []OpencodeProcessInfo
	Process(pid int) (ProcessMeta, bool)
}

// ProcessSnapshotProvider produces a ProcessSnapshot for a scan tick.
type ProcessSnapshotProvider interface {
	GetSnapshot(ctx context.Context) (ProcessSnapshot, error)
}

// ProcessSnapshotProviderFunc is an adapter to allow ordinary functions as a ProcessSnapshotProvider.
type ProcessSnapshotProviderFunc func(ctx context.Context) (ProcessSnapshot, error)

func (f ProcessSnapshotProviderFunc) GetSnapshot(ctx context.Context) (ProcessSnapshot, error) {
	return f(ctx)
}

// TickProcessSnapshotProvider memoizes a ProcessSnapshot within a single scan tick,
// ensuring the underlying provider is invoked at most once.
type TickProcessSnapshotProvider struct {
	underlying ProcessSnapshotProvider
	once       sync.Once
	snapshot   ProcessSnapshot
	err        error
}

func NewTickProcessSnapshotProvider(underlying ProcessSnapshotProvider) *TickProcessSnapshotProvider {
	if underlying == nil {
		underlying = LiveProcessSnapshotProvider{}
	}
	return &TickProcessSnapshotProvider{
		underlying: underlying,
	}
}

func (t *TickProcessSnapshotProvider) GetSnapshot(ctx context.Context) (ProcessSnapshot, error) {
	t.once.Do(func() {
		t.snapshot, t.err = t.underlying.GetSnapshot(ctx)
	})
	return t.snapshot, t.err
}

// LiveProcessSnapshot holds process metadata discovered in a single process table walk.
type LiveProcessSnapshot struct {
	processes     map[int]ProcessMeta
	piProcs       []PiProcessInfo
	codexProcs    []CodexProcessInfo
	opencodeProcs []OpencodeProcessInfo
}

func (s *LiveProcessSnapshot) IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if os.Getenv("AGENT_STATUS_TEST_MOCK_PROC") == "1" {
		return true
	}
	if s.processes == nil {
		return false
	}
	p, ok := s.processes[pid]
	return ok && p.Alive
}

func (s *LiveProcessSnapshot) VerifyProcessMatch(pid int, expectedStartTime int64) bool {
	if pid <= 0 {
		return false
	}
	if os.Getenv("AGENT_STATUS_TEST_MOCK_PROC") == "1" {
		return true
	}
	if !s.IsProcessAlive(pid) {
		return false
	}
	if expectedStartTime <= 0 {
		return true
	}

	p := s.processes[pid]
	if p.CreateTime <= 0 {
		return true
	}

	diff := math.Abs(float64(p.CreateTime - expectedStartTime))
	return diff < 60000
}

func (s *LiveProcessSnapshot) Process(pid int) (ProcessMeta, bool) {
	if s.processes == nil {
		return ProcessMeta{}, false
	}
	p, ok := s.processes[pid]
	return p, ok
}

func (s *LiveProcessSnapshot) PiProcesses() []PiProcessInfo {
	return s.piProcs
}

func (s *LiveProcessSnapshot) CodexProcesses() []CodexProcessInfo {
	return s.codexProcs
}

func (s *LiveProcessSnapshot) OpencodeProcesses() []OpencodeProcessInfo {
	return s.opencodeProcs
}

// isPlausibleAgentProc performs cheap filtering on process name before inspecting cmdline.
func isPlausibleAgentProc(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	base = strings.TrimSuffix(base, ".exe")

	switch base {
	case "pi", "codex", "codex-tui", "codex-cli", "opencode", "claude":
		return true
	}

	for _, prefix := range []string{"node", "bun", "electron"} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}

	return false
}

// isAgentInterpreter checks if the base process name matches known runtime wrappers/interpreters.
func isAgentInterpreter(baseName string) bool {
	for _, prefix := range []string{"node", "bun", "electron"} {
		if strings.HasPrefix(baseName, prefix) {
			return true
		}
	}
	return false
}

// inspectCandidate filters candidates on cheap p_comm / name first,
// querying cmdline and cwd only for plausible agent processes.
func (s *LiveProcessSnapshot) inspectCandidate(
	ctx context.Context,
	pid int,
	name string,
	createTime int64,
	procGetter func() (*process.Process, error),
) {
	if !isPlausibleAgentProc(name) {
		return
	}

	baseName := strings.ToLower(filepath.Base(name))
	baseName = strings.TrimSuffix(baseName, ".exe")

	// Claude Code sessions are tracked via descriptor files; "claude" process names
	// do not require cmdline inspection or inclusion in Pi/Codex/Opencode process lists.
	if baseName == "claude" {
		return
	}

	isPi := baseName == "pi"
	isCodex := baseName == "codex" || baseName == "codex-tui" || baseName == "codex-cli"
	isOpencode := baseName == "opencode"

	var p *process.Process
	getProc := func() (*process.Process, error) {
		if p != nil {
			return p, nil
		}
		var err error
		p, err = procGetter()
		return p, err
	}

	if !isPi && !isCodex && !isOpencode {
		if !isAgentInterpreter(baseName) {
			return
		}

		proc, err := getProc()
		if err != nil {
			return
		}

		cmdline, cErr := proc.CmdlineSliceWithContext(ctx)
		if cErr != nil || len(cmdline) == 0 {
			return
		}

		for _, arg := range cmdline {
			baseArg := strings.ToLower(filepath.Base(arg))
			if baseArg == "pi" || strings.HasSuffix(arg, "/pi") {
				isPi = true
			}
			if baseArg == "codex" || baseArg == "codex-cli" || strings.HasSuffix(arg, "/codex") {
				isCodex = true
			}
			if baseArg == "opencode" || strings.HasSuffix(arg, "/opencode") {
				isOpencode = true
			}
		}
	}

	if !isPi && !isCodex && !isOpencode {
		return
	}

	proc, err := getProc()
	if err != nil {
		return
	}

	cwd, err := proc.CwdWithContext(ctx)
	if err != nil || cwd == "" {
		return
	}
	cleanCWD := filepath.Clean(cwd)

	if isPi {
		s.piProcs = append(s.piProcs, PiProcessInfo{
			PID:        pid,
			CWD:        cleanCWD,
			CreateTime: createTime,
		})
	}
	if isCodex {
		s.codexProcs = append(s.codexProcs, CodexProcessInfo{
			PID:        pid,
			CWD:        cleanCWD,
			CreateTime: createTime,
		})
	}
	if isOpencode {
		s.opencodeProcs = append(s.opencodeProcs, OpencodeProcessInfo{
			PID:        pid,
			CWD:        cleanCWD,
			CreateTime: createTime,
		})
	}
}

// CaptureProcessSnapshot walks the process table once using cheap filtering first,
// populating process candidates for all supported harnesses.
func CaptureProcessSnapshot(ctx context.Context) (ProcessSnapshot, error) {
	return captureProcessSnapshot(ctx)
}

// LiveProcessSnapshotProvider implements ProcessSnapshotProvider by calling CaptureProcessSnapshot.
type LiveProcessSnapshotProvider struct{}

func (p LiveProcessSnapshotProvider) GetSnapshot(ctx context.Context) (ProcessSnapshot, error) {
	return CaptureProcessSnapshot(ctx)
}

func NewLiveProcessSnapshotProvider() ProcessSnapshotProvider {
	return LiveProcessSnapshotProvider{}
}

// SnapshotPiProcessFinder returns a PiProcessFinder backed by a ProcessSnapshotProvider.
func SnapshotPiProcessFinder(provider ProcessSnapshotProvider) PiProcessFinder {
	return func(ctx context.Context) ([]PiProcessInfo, error) {
		snap, err := provider.GetSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		return snap.PiProcesses(), nil
	}
}

// SnapshotCodexProcessFinder returns a CodexProcessFinder backed by a ProcessSnapshotProvider.
func SnapshotCodexProcessFinder(provider ProcessSnapshotProvider) CodexProcessFinder {
	return func(ctx context.Context) ([]CodexProcessInfo, error) {
		snap, err := provider.GetSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		return snap.CodexProcesses(), nil
	}
}

// SnapshotOpencodeProcessFinder returns an OpencodeProcessFinder backed by a ProcessSnapshotProvider.
func SnapshotOpencodeProcessFinder(provider ProcessSnapshotProvider) OpencodeProcessFinder {
	return func(ctx context.Context) ([]OpencodeProcessInfo, error) {
		snap, err := provider.GetSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		return snap.OpencodeProcesses(), nil
	}
}
