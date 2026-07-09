package session

import (
	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

// ProjectKeyForDirectory derives the SessionStore project_key for a directory.
// Delegates to sessionstore.ProjectKeyForDirectory (the canonical
// implementation shared with the store path).
func ProjectKeyForDirectory(dir string) string {
	return sessionstore.ProjectKeyForDirectory(dir)
}

// projectsDirForEnv returns the projects directory, consulting envOverride's
// CLAUDE_CONFIG_DIR before the process environment.
func projectsDirForEnv(envOverride map[string]string) (string, error) {
	return sessionstore.ProjectsDirForEnv(envOverride)
}
