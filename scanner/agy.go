package scanner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alexgorbatchev/agent-parser"
)

// AgySessionDescriptor describes a discovered Antigravity (`agy`) session.
type AgySessionDescriptor struct {
	PID             int          `json:"pid"`
	SessionID       string       `json:"sessionId"`
	CWD             string       `json:"cwd"`
	TranscriptPath  string       `json:"transcriptPath"`
	StartedAt       int64        `json:"startedAt"`
	LastActive      int64        `json:"lastActive"`
	HarnessVersion  string       `json:"harnessVersion"`
	IsActive        bool         `json:"isActive"`
	Title           string       `json:"title,omitempty"`
	ParentSessionID string       `json:"parentSessionId,omitempty"`
	IsSubagent      bool         `json:"isSubagent"`
	State           SessionState `json:"state,omitempty"`
	StateUpdatedAt  int64        `json:"stateUpdatedAt,omitempty"`
}

// AgyProcessInfo describes a running `agy` CLI process.
type AgyProcessInfo struct {
	PID        int
	CWD        string
	CreateTime int64 // unix millis
}

// AgyProcessFinder locates running `agy` processes.
type AgyProcessFinder func(ctx context.Context) ([]AgyProcessInfo, error)

type agyFileCacheEntry struct {
	size       int64
	modTime    time.Time
	lastActive int64
}

// AgyScanner discovers Antigravity (`agy`) sessions and correlates them with
// live `agy` processes.
type AgyScanner struct {
	sessionsDir   string
	processFinder AgyProcessFinder
	mu            sync.Mutex
	cache         map[string]agyFileCacheEntry
}

// DefaultAgySessionsDir returns the default Antigravity brain directory.
func DefaultAgySessionsDir() string {
	if custom := os.Getenv("AGY_SESSIONS_DIR"); custom != "" {
		return custom
	}
	if geminiHome := os.Getenv("GEMINI_HOME"); geminiHome != "" {
		return filepath.Join(geminiHome, "antigravity-cli", "brain")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "brain")
}

// AgySessionState maps Antigravity conversation summary status values.
func AgySessionState(status string, notFullyIdle bool) SessionState {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "RUNNING", "ACTIVE":
		return SessionStateWorking
	case "WAITING":
		return SessionStateWaiting
	case "IDLE", "DONE", "SUCCESS":
		if notFullyIdle {
			return SessionStateWorking
		}
		return SessionStateIdle
	default:
		if notFullyIdle {
			return SessionStateWorking
		}
		return SessionStateUnknown
	}
}

// LiveAgyProcessFinder discovers running `agy` processes via a live process snapshot.
func LiveAgyProcessFinder(ctx context.Context) ([]AgyProcessInfo, error) {
	return SnapshotAgyProcessFinder(LiveProcessSnapshotProvider{})(ctx)
}

// NewAgyScanner creates an Antigravity session scanner.
func NewAgyScanner(sessionsDir string, finder AgyProcessFinder) *AgyScanner {
	if sessionsDir == "" {
		sessionsDir = DefaultAgySessionsDir()
	}
	if finder == nil {
		finder = LiveAgyProcessFinder
	}
	return &AgyScanner{
		sessionsDir:   sessionsDir,
		processFinder: finder,
		cache:         make(map[string]agyFileCacheEntry),
	}
}

// SetProcessFinder replaces the process finder.
func (s *AgyScanner) SetProcessFinder(finder AgyProcessFinder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processFinder = finder
}

// SetProcessSnapshotProvider wires the scanner to a shared tick snapshot provider.
func (s *AgyScanner) SetProcessSnapshotProvider(provider ProcessSnapshotProvider) {
	if provider != nil {
		s.SetProcessFinder(SnapshotAgyProcessFinder(provider))
	}
}

// Scan discovers Antigravity sessions under sessionsDir and matches active ones
// against running `agy` processes by workspace CWD.
func (s *AgyScanner) Scan(ctx context.Context) ([]AgySessionDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	dir := s.sessionsDir
	finder := s.processFinder
	s.mu.Unlock()

	if dir == "" {
		return nil, nil
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("accessing agy sessions dir: %w", err)
	}

	procs, err := finder(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding agy processes: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading agy sessions dir: %w", err)
	}

	appDataDir := filepath.Dir(dir)
	summaries, _ := parser.ReadAgySummaries(ctx, filepath.Join(appDataDir, "conversation_summaries.db"))
	conversationsDir := filepath.Join(appDataDir, "conversations")

	// Collect detached subagent parent links from brain/<parent>/.system_generated/subagents/*.json.
	parents := make(map[string]string)
	for id, sum := range summaries {
		if sum.ParentConversationID != "" {
			parents[id] = sum.ParentConversationID
		}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		parentID := entry.Name()
		subEntries, subErr := os.ReadDir(filepath.Join(dir, parentID, ".system_generated", "subagents"))
		if subErr != nil {
			continue
		}
		for _, sub := range subEntries {
			if !sub.IsDir() && strings.HasSuffix(sub.Name(), ".json") {
				if childID := strings.TrimSuffix(sub.Name(), ".json"); childID != "" {
					parents[childID] = parentID
				}
			}
		}
	}

	var sessions []AgySessionDescriptor
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() {
			continue
		}
		convID := entry.Name()
		transcriptPath, info := selectAgyTranscript(filepath.Join(dir, convID, ".system_generated", "logs"))
		if transcriptPath == "" {
			continue
		}

		lastActive, err := s.cachedLastActive(ctx, transcriptPath, info)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastActive = info.ModTime().UnixMilli()
		}

		var cwd, title string
		var state SessionState
		var stateUpdatedAt int64
		if sum, ok := summaries[convID]; ok {
			title = sum.Title
			if len(sum.WorkspacePaths) > 0 {
				cwd = filepath.Clean(sum.WorkspacePaths[0])
			}
			state = AgySessionState(sum.Status, sum.NotFullyIdle)
			if !sum.LastModified.IsZero() {
				stateUpdatedAt = sum.LastModified.UnixMilli()
			}
		}
		if cwd == "" {
			if paths, wsErr := parser.ReadAgyConversationWorkspaces(ctx, conversationsDir, convID); wsErr == nil && len(paths) > 0 {
				cwd = filepath.Clean(paths[0])
			}
		}

		parentID := parents[convID]
		sessions = append(sessions, AgySessionDescriptor{
			SessionID:       convID,
			CWD:             cwd,
			TranscriptPath:  transcriptPath,
			StartedAt:       info.ModTime().UnixMilli(),
			LastActive:      lastActive,
			Title:           title,
			ParentSessionID: parentID,
			IsSubagent:      parentID != "",
			State:           state,
			StateUpdatedAt:  stateUpdatedAt,
		})
	}

	// Match live processes to the most recently active session in each process's CWD.
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].LastActive != sessions[j].LastActive {
			return sessions[i].LastActive > sessions[j].LastActive
		}
		return sessions[i].SessionID < sessions[j].SessionID
	})

	usedSession := make(map[int]bool)
	activePIDBySession := make(map[string]int)
	for _, proc := range procs {
		procCWD := filepath.Clean(proc.CWD)
		matchedIdx := -1
		for i := range sessions {
			if usedSession[i] || sessions[i].IsSubagent {
				continue
			}
			if sessions[i].CWD != "" && filepath.Clean(sessions[i].CWD) == procCWD {
				matchedIdx = i
				break
			}
		}
		if matchedIdx >= 0 {
			usedSession[matchedIdx] = true
			sessions[matchedIdx].PID = proc.PID
			sessions[matchedIdx].IsActive = true
			if proc.CreateTime > 0 {
				sessions[matchedIdx].StartedAt = proc.CreateTime
			}
			activePIDBySession[sessions[matchedIdx].SessionID] = proc.PID
		}
	}

	// Propagate active PID to subagents whose parent session is active.
	for i := range sessions {
		if sessions[i].IsSubagent && !sessions[i].IsActive {
			if pid, ok := activePIDBySession[sessions[i].ParentSessionID]; ok {
				sessions[i].PID = pid
				sessions[i].IsActive = true
			}
		}
	}

	return sessions, nil
}

func (s *AgyScanner) cachedLastActive(ctx context.Context, path string, info os.FileInfo) (int64, error) {
	s.mu.Lock()
	if cached, ok := s.cache[path]; ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		s.mu.Unlock()
		return cached.lastActive, nil
	}
	s.mu.Unlock()

	lastActive, err := ExtractLastActiveTimestamp(ctx, path, parser.AgyActivityTimestamp)
	if err != nil {
		return 0, err
	}
	if lastActive == 0 {
		lastActive = info.ModTime().UnixMilli()
	}

	s.mu.Lock()
	s.cache[path] = agyFileCacheEntry{size: info.Size(), modTime: info.ModTime(), lastActive: lastActive}
	s.mu.Unlock()
	return lastActive, nil
}

func selectAgyTranscript(logsDir string) (string, os.FileInfo) {
	for _, name := range []string{"transcript_full.jsonl", "transcript.jsonl"} {
		candidate := filepath.Join(logsDir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, info
		}
	}
	return "", nil
}
