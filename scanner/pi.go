package scanner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

type PiSessionDescriptor struct {
	PID            int    `json:"pid"`
	SessionID      string `json:"sessionId"`
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcriptPath"`
	StartedAt      int64  `json:"startedAt"`
	LastActive     int64  `json:"lastActive"`
	HarnessVersion string `json:"harnessVersion"`
	IsActive       bool   `json:"isActive"`
}

type PiProcessInfo struct {
	PID        int
	CWD        string
	CreateTime int64 // unix millis
}

type PiHeader struct {
	Type      string      `json:"type"`
	Version   int         `json:"version"`
	ID        string      `json:"id"`
	Timestamp interface{} `json:"timestamp"`
	CWD       string      `json:"cwd"`
}

type PiProcessFinder func(ctx context.Context) ([]PiProcessInfo, error)

type piFileCacheEntry struct {
	size       int64
	modTime    time.Time
	header     *PiHeader
	lastActive int64
	dev        uint64
	ino        uint64
}

type PiScanner struct {
	sessionsDir   string
	processFinder PiProcessFinder
	mu            sync.Mutex
	cache         map[string]piFileCacheEntry
}

func DefaultPiSessionsDir() string {
	if custom := os.Getenv("PI_SESSIONS_DIR"); custom != "" {
		return custom
	}
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// EncodePiCWD implements Pi's directory naming rule:
// --${cwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--
func EncodePiCWD(cwd string) string {
	clean := filepath.Clean(cwd)
	clean = strings.TrimPrefix(clean, "/")
	clean = strings.TrimPrefix(clean, "\\")
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return "--" + r.Replace(clean) + "--"
}

// LivePiProcessFinder uses gopsutil/v4/process to discover running pi processes.
func LivePiProcessFinder(ctx context.Context) ([]PiProcessInfo, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}

	var results []PiProcessInfo
	for _, p := range procs {
		name, err := p.NameWithContext(ctx)
		if err != nil {
			continue
		}

		baseName := strings.ToLower(filepath.Base(name))
		if baseName != "pi" && baseName != "pi.exe" {
			// Also check cmdline in case it was run via node / npx / wrapper
			cmdline, cErr := p.CmdlineSliceWithContext(ctx)
			if cErr != nil || len(cmdline) == 0 {
				continue
			}
			isPi := false
			for _, arg := range cmdline {
				if filepath.Base(arg) == "pi" || strings.HasSuffix(arg, "/pi") {
					isPi = true
					break
				}
			}
			if !isPi {
				continue
			}
		}

		cwd, err := p.CwdWithContext(ctx)
		if err != nil || cwd == "" {
			continue
		}

		createTime, err := p.CreateTimeWithContext(ctx)
		if err != nil {
			createTime = 0
		}

		results = append(results, PiProcessInfo{
			PID:        int(p.Pid),
			CWD:        filepath.Clean(cwd),
			CreateTime: createTime,
		})
	}

	return results, nil
}

func NewPiScanner(sessionsDir string, finder PiProcessFinder) *PiScanner {
	if sessionsDir == "" {
		sessionsDir = DefaultPiSessionsDir()
	}
	if finder == nil {
		finder = LivePiProcessFinder
	}
	return &PiScanner{
		sessionsDir:   sessionsDir,
		processFinder: finder,
		cache:         make(map[string]piFileCacheEntry),
	}
}

func (s *PiScanner) SetProcessFinder(finder PiProcessFinder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processFinder = finder
}

func (s *PiScanner) SetProcessSnapshotProvider(provider ProcessSnapshotProvider) {
	if provider != nil {
		s.SetProcessFinder(SnapshotPiProcessFinder(provider))
	}
}

// ReadPiSessionHeader reads the first JSON line of a Pi session file.
func ReadPiSessionHeader(path string) (*PiHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = f.Close()
	}()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("empty session file")
	}

	var header PiHeader
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return nil, fmt.Errorf("unmarshaling session header: %w", err)
	}

	if header.Type != "session" || header.ID == "" {
		return nil, fmt.Errorf("invalid session header type: %s", header.Type)
	}

	return &header, nil
}

// Scan enumerates Pi sessions under sessionsDir, finds running Pi processes,
// and pairs processes to their corresponding session files.
func (s *PiScanner) Scan(ctx context.Context) ([]PiSessionDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache == nil {
		s.cache = make(map[string]piFileCacheEntry)
	}

	if s.sessionsDir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(s.sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			s.cache = make(map[string]piFileCacheEntry)
			return nil, nil
		}
		return nil, fmt.Errorf("reading pi sessions dir: %w", err)
	}

	type piCandidate struct {
		desc        PiSessionDescriptor
		headerTime  int64
		fileModTime int64
		lastActive  int64
		cleanCWD    string
	}
	var candidates []piCandidate
	seenFiles := make(map[string]bool)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		subDir := filepath.Join(s.sessionsDir, entry.Name())
		files, err := os.ReadDir(subDir)
		if err != nil {
			continue
		}

		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}

			filePath := filepath.Join(subDir, f.Name())
			seenFiles[filePath] = true

			info, err := f.Info()
			if err != nil {
				delete(s.cache, filePath)
				continue
			}

			size := info.Size()
			fileModTime := info.ModTime().UnixMilli()
			dev, ino := getFileDevIno(info)

			var header *PiHeader
			var lastActive int64

			cached, found := s.cache[filePath]
			if found && cached.header != nil && cached.size == size && cached.modTime.Equal(info.ModTime()) && cached.dev == dev && cached.ino == ino {
				header = cached.header
				lastActive = cached.lastActive
			} else {
				h, err := ReadPiSessionHeader(filePath)
				if err != nil {
					delete(s.cache, filePath)
					continue
				}
				header = h

				la, sErr := ExtractLastActiveTimestamp(filePath)
				if sErr != nil {
					la = fileModTime
				}
				lastActive = la

				s.cache[filePath] = piFileCacheEntry{
					size:       size,
					modTime:    info.ModTime(),
					header:     header,
					lastActive: lastActive,
					dev:        dev,
					ino:        ino,
				}
			}

			headerTime := int64(0)
			if header.Timestamp != nil {
				switch ts := header.Timestamp.(type) {
				case float64:
					headerTime = int64(ts)
				case int64:
					headerTime = ts
				case string:
					if t, perr := time.Parse(time.RFC3339Nano, ts); perr == nil {
						headerTime = t.UnixMilli()
					} else if t, perr := time.Parse(time.RFC3339, ts); perr == nil {
						headerTime = t.UnixMilli()
					}
				}
			}
			if headerTime == 0 {
				headerTime = lastActive
			}
			if lastActive == 0 {
				lastActive = headerTime
			}

			cleanCWD := filepath.Clean(header.CWD)
			desc := PiSessionDescriptor{
				PID:            0,
				SessionID:      header.ID,
				CWD:            cleanCWD,
				TranscriptPath: filePath,
				StartedAt:      headerTime,
				LastActive:     lastActive,
				HarnessVersion: fmt.Sprintf("v%d", header.Version),
				IsActive:       false,
			}

			candidates = append(candidates, piCandidate{
				desc:        desc,
				headerTime:  headerTime,
				fileModTime: fileModTime,
				lastActive:  lastActive,
				cleanCWD:    cleanCWD,
			})
		}
	}

	// Prune cache entries for files that were not seen during the scan.
	for path := range s.cache {
		if !seenFiles[path] {
			delete(s.cache, path)
		}
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	var activeProcs []PiProcessInfo
	if s.processFinder != nil {
		activeProcs, _ = s.processFinder(ctx)
	}

	// Map procs by CWD
	procsByCWD := make(map[string][]PiProcessInfo)
	for _, proc := range activeProcs {
		cleanCWD := filepath.Clean(proc.CWD)
		procsByCWD[cleanCWD] = append(procsByCWD[cleanCWD], proc)
	}

	// Sort candidates by most recent activity or modification descending so newest and resumed sessions take precedence.
	sort.SliceStable(candidates, func(i, j int) bool {
		recI := candidates[i].lastActive
		if candidates[i].fileModTime > recI {
			recI = candidates[i].fileModTime
		}
		recJ := candidates[j].lastActive
		if candidates[j].fileModTime > recJ {
			recJ = candidates[j].fileModTime
		}
		if recI != recJ {
			return recI > recJ
		}
		return candidates[i].headerTime > candidates[j].headerTime
	})

	var descriptors []PiSessionDescriptor
	usedPIDs := make(map[int]bool)

	for i := range candidates {
		cand := &candidates[i]
		candidateProcs := procsByCWD[cand.cleanCWD]
		var bestProc *PiProcessInfo
		var minDiff int64 = 1<<62 - 1

		for _, proc := range candidateProcs {
			if usedPIDs[proc.PID] {
				continue
			}

			// Check if process started at or before the session was created or modified,
			// allowing resumed sessions (where headerTime is in the past, but file was modified or had recent activity).
			diff := cand.headerTime - proc.CreateTime
			recentTime := cand.lastActive
			if cand.fileModTime > recentTime {
				recentTime = cand.fileModTime
			}
			if diff < -5000 && recentTime-proc.CreateTime >= -5000 {
				diff = recentTime - proc.CreateTime
			}
			if diff >= -5000 {
				absDiff := diff
				if absDiff < 0 {
					absDiff = -absDiff
				}
				if absDiff < minDiff {
					minDiff = absDiff
					procCopy := proc
					bestProc = &procCopy
				}
			}
		}

		if bestProc != nil {
			cand.desc.PID = bestProc.PID
			cand.desc.IsActive = true
			if bestProc.CreateTime > 0 {
				cand.desc.StartedAt = bestProc.CreateTime
			}
			usedPIDs[bestProc.PID] = true
		} else if os.Getenv("AGENT_STATUS_TEST_MOCK_PROC") == "1" {
			cand.desc.PID = 99992
			cand.desc.IsActive = true
		}

		descriptors = append(descriptors, cand.desc)
	}

	return descriptors, nil
}
