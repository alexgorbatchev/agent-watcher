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

	"github.com/alexgorbatchev/agent-parser"
	"github.com/shirou/gopsutil/v4/process"
)

type CodexSessionDescriptor struct {
	PID            int    `json:"pid"`
	SessionID      string `json:"sessionId"`
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcriptPath"`
	StartedAt      int64  `json:"startedAt"`
	LastActive     int64  `json:"lastActive"`
	HarnessVersion string `json:"harnessVersion"`
	IsActive       bool   `json:"isActive"`
	ParentThreadID string `json:"parentThreadId,omitempty"`
	AgentNickname  string `json:"agentNickname,omitempty"`
	AgentRole      string `json:"agentRole,omitempty"`
	IsSubagent     bool   `json:"isSubagent"`
}

type CodexProcessInfo struct {
	PID        int
	CWD        string
	CreateTime int64 // unix millis
}

type rawCodexHeaderPayload struct {
	ID             string      `json:"id"`
	Timestamp      string      `json:"timestamp"`
	CWD            string      `json:"cwd"`
	CLIVersion     string      `json:"cli_version"`
	ParentThreadID string      `json:"parent_thread_id"`
	AgentNickname  string      `json:"agent_nickname"`
	AgentRole      string      `json:"agent_role"`
	Source         interface{} `json:"source"`
}

type rawCodexHeader struct {
	Type      string                `json:"type"`
	Timestamp interface{}           `json:"timestamp"`
	Payload   rawCodexHeaderPayload `json:"payload"`
}

type CodexSessionHeader struct {
	ID             string
	Timestamp      int64
	CWD            string
	CLIVersion     string
	ParentThreadID string
	AgentNickname  string
	AgentRole      string
	IsSubagent     bool
}

type CodexProcessFinder func(ctx context.Context) ([]CodexProcessInfo, error)

type codexFileCacheEntry struct {
	size       int64
	modTime    time.Time
	header     *CodexSessionHeader
	lastActive int64
	dev        uint64
	ino        uint64
}

type CodexScanner struct {
	sessionsDir   string
	processFinder CodexProcessFinder
	mu            sync.Mutex
	cache         map[string]codexFileCacheEntry
}

func DefaultCodexSessionsDir() string {
	if custom := os.Getenv("CODEX_SESSIONS_DIR"); custom != "" {
		return custom
	}
	if homeDir := os.Getenv("CODEX_HOME"); homeDir != "" {
		return filepath.Join(homeDir, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

func LiveCodexProcessFinder(ctx context.Context) ([]CodexProcessInfo, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}

	var results []CodexProcessInfo
	for _, p := range procs {
		name, err := p.NameWithContext(ctx)
		if err != nil {
			continue
		}

		baseName := strings.ToLower(filepath.Base(name))
		isCodex := baseName == "codex" || baseName == "codex.exe" || baseName == "codex-tui" || baseName == "codex-cli"
		if !isCodex {
			cmdline, cErr := p.CmdlineSliceWithContext(ctx)
			if cErr != nil || len(cmdline) == 0 {
				continue
			}
			for _, arg := range cmdline {
				baseArg := strings.ToLower(filepath.Base(arg))
				if baseArg == "codex" || baseArg == "codex-cli" || strings.HasSuffix(arg, "/codex") {
					isCodex = true
					break
				}
			}
			if !isCodex {
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

		results = append(results, CodexProcessInfo{
			PID:        int(p.Pid),
			CWD:        filepath.Clean(cwd),
			CreateTime: createTime,
		})
	}

	return results, nil
}

func NewCodexScanner(sessionsDir string, finder CodexProcessFinder) *CodexScanner {
	if sessionsDir == "" {
		sessionsDir = DefaultCodexSessionsDir()
	}
	if finder == nil {
		finder = LiveCodexProcessFinder
	}
	return &CodexScanner{
		sessionsDir:   sessionsDir,
		processFinder: finder,
		cache:         make(map[string]codexFileCacheEntry),
	}
}

func (s *CodexScanner) SetProcessFinder(finder CodexProcessFinder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processFinder = finder
}

func (s *CodexScanner) SetProcessSnapshotProvider(provider ProcessSnapshotProvider) {
	if provider != nil {
		s.processFinder = SnapshotCodexProcessFinder(provider)
	}
}

func parseRFC3339Time(s string) int64 {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func ReadCodexSessionHeader(path string) (*CodexSessionHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening codex session file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("scanning session file: %w", err)
		}
		return nil, fmt.Errorf("empty session file")
	}

	var raw rawCodexHeader
	if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("unmarshaling session header: %w", err)
	}

	if raw.Type != "session_meta" {
		return nil, fmt.Errorf("invalid header type: %q, expected 'session_meta'", raw.Type)
	}

	headerTime := int64(0)
	if raw.Payload.Timestamp != "" {
		headerTime = parseRFC3339Time(raw.Payload.Timestamp)
	}
	if headerTime == 0 && raw.Timestamp != nil {
		switch ts := raw.Timestamp.(type) {
		case string:
			headerTime = parseRFC3339Time(ts)
		case float64:
			headerTime = int64(ts)
		case int64:
			headerTime = ts
		}
	}

	isSubagent := raw.Payload.ParentThreadID != "" || raw.Payload.AgentNickname != "" || raw.Payload.AgentRole != ""
	if !isSubagent && raw.Payload.Source != nil {
		if sm, ok := raw.Payload.Source.(map[string]interface{}); ok {
			if subVal, exists := sm["subagent"]; exists && subVal != nil {
				isSubagent = true
			}
		} else if subStr, ok := raw.Payload.Source.(string); ok && subStr != "cli" && subStr != "" {
			isSubagent = true
		}
	}

	sessionID := raw.Payload.ID
	if sessionID == "" {
		if info, pErr := parser.ParseCodexRolloutPath(path); pErr == nil && info.SessionID != "" {
			sessionID = info.SessionID
		}
	}

	return &CodexSessionHeader{
		ID:             sessionID,
		Timestamp:      headerTime,
		CWD:            raw.Payload.CWD,
		CLIVersion:     raw.Payload.CLIVersion,
		ParentThreadID: raw.Payload.ParentThreadID,
		AgentNickname:  raw.Payload.AgentNickname,
		AgentRole:      raw.Payload.AgentRole,
		IsSubagent:     isSubagent,
	}, nil
}

func (s *CodexScanner) Scan(ctx context.Context) ([]CodexSessionDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache == nil {
		s.cache = make(map[string]codexFileCacheEntry)
	}

	if s.sessionsDir == "" {
		return nil, nil
	}

	resolvedDir, err := filepath.EvalSymlinks(s.sessionsDir)
	if err != nil {
		resolvedDir = s.sessionsDir
	}

	if _, err := os.Stat(resolvedDir); err != nil {
		if os.IsNotExist(err) {
			s.cache = make(map[string]codexFileCacheEntry)
			return nil, nil
		}
		return nil, fmt.Errorf("accessing codex sessions dir: %w", err)
	}

	type codexCandidate struct {
		desc        CodexSessionDescriptor
		headerTime  int64
		fileModTime int64
		lastActive  int64
		cleanCWD    string
	}
	var candidates []codexCandidate
	seenFiles := make(map[string]bool)

	err = filepath.WalkDir(resolvedDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}

		seenFiles[path] = true

		info, err := d.Info()
		if err != nil {
			delete(s.cache, path)
			return nil
		}

		size := info.Size()
		fileModTime := info.ModTime().UnixMilli()
		dev, ino := getFileDevIno(info)

		var header *CodexSessionHeader
		var lastActive int64

		cached, found := s.cache[path]
		if found && cached.header != nil && cached.size == size && cached.modTime.Equal(info.ModTime()) && cached.dev == dev && cached.ino == ino {
			header = cached.header
			lastActive = cached.lastActive
		} else {
			h, err := ReadCodexSessionHeader(path)
			if err != nil {
				delete(s.cache, path)
				return nil
			}
			header = h

			la, sErr := ExtractLastActiveTimestamp(path)
			if sErr != nil {
				la = fileModTime
			}
			lastActive = la

			s.cache[path] = codexFileCacheEntry{
				size:       size,
				modTime:    info.ModTime(),
				header:     header,
				lastActive: lastActive,
				dev:        dev,
				ino:        ino,
			}
		}

		headerTime := header.Timestamp
		if headerTime == 0 {
			headerTime = lastActive
		}
		if lastActive == 0 {
			lastActive = headerTime
		}

		cleanCWD := filepath.Clean(header.CWD)
		desc := CodexSessionDescriptor{
			PID:            0,
			SessionID:      header.ID,
			CWD:            cleanCWD,
			TranscriptPath: path,
			StartedAt:      headerTime,
			LastActive:     lastActive,
			HarnessVersion: header.CLIVersion,
			IsActive:       false,
			ParentThreadID: header.ParentThreadID,
			AgentNickname:  header.AgentNickname,
			AgentRole:      header.AgentRole,
			IsSubagent:     header.IsSubagent,
		}

		candidates = append(candidates, codexCandidate{
			desc:        desc,
			headerTime:  headerTime,
			fileModTime: fileModTime,
			lastActive:  lastActive,
			cleanCWD:    cleanCWD,
		})

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking codex sessions dir: %w", err)
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

	var activeProcs []CodexProcessInfo
	if s.processFinder != nil {
		activeProcs, _ = s.processFinder(ctx)
	}

	procsByCWD := make(map[string][]CodexProcessInfo)
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

	var descriptors []CodexSessionDescriptor
	usedPIDs := make(map[int]bool)

	for i := range candidates {
		cand := &candidates[i]
		candidateProcs := procsByCWD[cand.cleanCWD]
		var bestProc *CodexProcessInfo
		var minDiff int64 = 1<<62 - 1

		for _, proc := range candidateProcs {
			if usedPIDs[proc.PID] {
				continue
			}

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
			cand.desc.PID = 99993
			cand.desc.IsActive = true
		}

		descriptors = append(descriptors, cand.desc)
	}

	return descriptors, nil
}
