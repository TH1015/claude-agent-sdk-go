package sessionstore

import (
	"path/filepath"
	"strings"
)

// FilePathToSessionKey derives a SessionKey from an absolute transcript file
// path, given the projects directory the subprocess writes under.
//
// Main transcripts:     <projectsDir>/<projectKey>/<sessionID>.jsonl
// Subagent transcripts: <projectsDir>/<projectKey>/<sessionID>/subagents/.../agent-<id>.jsonl
//
// Returns (SessionKey{}, false) if filePath is not under projectsDir or has an
// unrecognized shape. Mirrors the Python SDK's file_path_to_session_key.
func FilePathToSessionKey(filePath, projectsDir string) (SessionKey, bool) {
	rel, err := filepath.Rel(projectsDir, filePath)
	if err != nil {
		// Different drives on Windows, etc. — treat as "not under projectsDir".
		return SessionKey{}, false
	}
	// Split into path components using the OS separator (Rel returns
	// OS-native separators).
	rel = filepath.Clean(rel)
	if rel == "." || filepath.IsAbs(rel) {
		return SessionKey{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 || parts[0] == ".." || parts[0] == "" {
		return SessionKey{}, false
	}
	if len(parts) < 2 {
		return SessionKey{}, false
	}

	projectKey := parts[0]
	second := parts[1]

	// Main transcript: <projectKey>/<sessionID>.jsonl
	if len(parts) == 2 && strings.HasSuffix(second, ".jsonl") {
		return SessionKey{
			ProjectKey: projectKey,
			SessionID:  strings.TrimSuffix(second, ".jsonl"),
		}, true
	}

	// Subagent transcript: <projectKey>/<sessionID>/subagents/.../agent-<id>.jsonl
	if len(parts) >= 4 {
		subpathParts := append([]string(nil), parts[2:]...)
		last := subpathParts[len(subpathParts)-1]
		if strings.HasSuffix(last, ".jsonl") {
			subpathParts[len(subpathParts)-1] = strings.TrimSuffix(last, ".jsonl")
		}
		// Subpaths are always "/"-joined regardless of OS separator so keys
		// are portable across platforms.
		return SessionKey{
			ProjectKey: projectKey,
			SessionID:  second,
			Subpath:    strings.Join(subpathParts, "/"),
		}, true
	}

	return SessionKey{}, false
}
