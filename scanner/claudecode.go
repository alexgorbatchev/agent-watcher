package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type SessionDescriptor struct {
	PID            int    `json:"pid"`
	SessionID      string `json:"sessionId"`
	CWD            string `json:"cwd"`
	Status         string `json:"status"`
	TranscriptPath string `json:"transcriptPath"`
	StartedAt      int64  `json:"startedAt"`
	HarnessVersion string `json:"version"`
	Name           string `json:"name,omitempty"`
	NameSource     string `json:"nameSource,omitempty"`
}

type SubagentTranscript struct {
	SubagentID     string `json:"subagentId"`
	TranscriptPath string `json:"transcriptPath"`
}

var isPIDAliveProbe = isPIDAlive

type ClaudeCodeScanner struct {
	sessionDirs      []string
	projectsDir      string
	snapshotProvider ProcessSnapshotProvider
}

// SetProjectsDir overrides transcript lookup without changing process environment.
func (s *ClaudeCodeScanner) SetProjectsDir(dir string) {
	s.projectsDir = dir
}

func (s *ClaudeCodeScanner) SetProcessSnapshotProvider(provider ProcessSnapshotProvider) {
	s.snapshotProvider = provider
}

func DefaultClaudeSessionDirs() []string {
	var dirs []string
	if custom := os.Getenv("CLAUDE_SESSIONS_DIR"); custom != "" {
		dirs = append(dirs, custom)
	}
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		dirs = append(dirs, filepath.Join(dataHome, "ai-registry", "claude-code", "sessions"))
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "ai-registry", "claude-code", "sessions"))
	}

	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "sessions"))
	}

	return dirs
}

func NewClaudeCodeScanner(dirs []string) *ClaudeCodeScanner {
	if len(dirs) == 0 {
		dirs = DefaultClaudeSessionDirs()
	}
	return &ClaudeCodeScanner{sessionDirs: dirs}
}

func (s *ClaudeCodeScanner) Scan() ([]SessionDescriptor, error) {
	var candidates []SessionDescriptor

	for _, rawDir := range s.sessionDirs {
		dir, err := filepath.EvalSymlinks(rawDir)
		if err != nil {
			dir = rawDir
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // Skip missing directories
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasSuffix(name, ".key") || !strings.HasSuffix(name, ".json") {
				continue
			}

			filePath := filepath.Join(dir, name)
			data, err := os.ReadFile(filePath)
			if err != nil {
				continue
			}

			var desc SessionDescriptor
			if err := json.Unmarshal(data, &desc); err != nil {
				continue
			}

			if desc.PID <= 0 {
				continue
			}

			candidates = append(candidates, desc)
		}
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	hasAliveCandidate := false
	if os.Getenv("AGENT_STATUS_TEST_MOCK_PROC") == "1" {
		hasAliveCandidate = true
	} else {
		for _, desc := range candidates {
			if isPIDAliveProbe(desc.PID) {
				hasAliveCandidate = true
				break
			}
		}
	}

	if !hasAliveCandidate {
		return nil, nil
	}

	var snap ProcessSnapshot
	if s.snapshotProvider != nil && os.Getenv("AGENT_STATUS_TEST_MOCK_PROC") != "1" {
		snap, _ = s.snapshotProvider.GetSnapshot(context.Background())
	}

	seenPIDs := make(map[int]bool)
	var results []SessionDescriptor
	for _, desc := range candidates {
		if seenPIDs[desc.PID] {
			continue
		}

		if os.Getenv("AGENT_STATUS_TEST_MOCK_PROC") != "1" {
			if snap != nil {
				if !snap.IsProcessAlive(desc.PID) {
					continue
				}
				if desc.StartedAt > 0 && !snap.VerifyProcessMatch(desc.PID, desc.StartedAt) {
					continue
				}
			} else {
				if !IsProcessAlive(desc.PID) {
					continue
				}
				if desc.StartedAt > 0 && !VerifyProcessMatch(desc.PID, desc.StartedAt) {
					continue
				}
			}
		}

		if desc.TranscriptPath == "" && desc.SessionID != "" {
			desc.TranscriptPath = locateTranscript(desc.SessionID, desc.CWD, s.projectsDir)
		}

		seenPIDs[desc.PID] = true
		results = append(results, desc)
	}

	return results, nil
}

func (s *ClaudeCodeScanner) DiscoverSubagents(desc SessionDescriptor) ([]SubagentTranscript, error) {
	if desc.TranscriptPath == "" {
		return nil, nil
	}

	dir := filepath.Dir(desc.TranscriptPath)
	candidates := []string{
		filepath.Join(dir, desc.SessionID, "subagents"),
		filepath.Join(dir, "subagents"),
	}

	var results []SubagentTranscript
	for _, subagentsDir := range candidates {
		entries, err := os.ReadDir(subagentsDir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}

			subID := strings.TrimSuffix(entry.Name(), ".jsonl")
			results = append(results, SubagentTranscript{
				SubagentID:     subID,
				TranscriptPath: filepath.Join(subagentsDir, entry.Name()),
			})
		}
	}

	return results, nil
}

func locateTranscript(sessionID, cwd, projectsDir string) string {
	var projectsDirs []string

	if projectsDir != "" {
		projectsDirs = append(projectsDirs, projectsDir)
	} else if custom := os.Getenv("CLAUDE_PROJECTS_DIR"); custom != "" {
		projectsDirs = append(projectsDirs, custom)
	}

	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		projectsDirs = append(projectsDirs, filepath.Join(dataHome, "ai-registry", "claude-code", "projects"))
	} else if home, err := os.UserHomeDir(); err == nil {
		projectsDirs = append(projectsDirs, filepath.Join(home, ".local", "share", "ai-registry", "claude-code", "projects"))
	}

	if home, err := os.UserHomeDir(); err == nil {
		projectsDirs = append(projectsDirs, filepath.Join(home, ".claude", "projects"))
	}

	targetFileName := fmt.Sprintf("%s.jsonl", sessionID)

	// 1. If cwd is provided, check the exact slug folder first
	if cwd != "" {
		slug := strings.ReplaceAll(cwd, "/", "-")
		slug = strings.ReplaceAll(slug, ".", "-")
		if !strings.HasPrefix(slug, "-") {
			slug = "-" + slug
		}

		for _, pDir := range projectsDirs {
			resolvedDir, err := filepath.EvalSymlinks(pDir)
			if err != nil {
				resolvedDir = pDir
			}
			exactPath := filepath.Join(resolvedDir, slug, targetFileName)
			if _, err := os.Stat(exactPath); err == nil {
				return exactPath
			}
			// Also check without leading dash in slug
			exactPathNoDash := filepath.Join(resolvedDir, strings.TrimPrefix(slug, "-"), targetFileName)
			if _, err := os.Stat(exactPathNoDash); err == nil {
				return exactPathNoDash
			}
		}
	}

	// 2. Scan project folders across all candidate directories
	for _, pDir := range projectsDirs {
		resolvedDir, err := filepath.EvalSymlinks(pDir)
		if err != nil {
			resolvedDir = pDir
		}

		entries, err := os.ReadDir(resolvedDir)
		if err != nil {
			continue
		}

		for _, e := range entries {
			if e.IsDir() {
				candidate := filepath.Join(resolvedDir, e.Name(), targetFileName)
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
	}

	return ""
}
