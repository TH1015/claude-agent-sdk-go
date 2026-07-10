package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

// importMaxBatchEntries and importMaxBatchBytes bound each store.Append call.
const (
	importMaxBatchEntries = 500
	importMaxBatchBytes   = 1 << 20
)

// ImportSessionToStore replays a local on-disk session transcript into a
// SessionStore — the inverse of resume materialization. It streams the JSONL
// line-by-line and calls store.Append in batches. The destination project_key
// is the on-disk project directory name, so an imported session is
// indistinguishable from a live-mirrored one and resumable from the original
// cwd. When includeSubagents is true, subagent transcripts and their
// .meta.json sidecars are also imported. Mirrors the Python SDK's
// import_session_to_store.
func ImportSessionToStore(ctx context.Context, sessionID string, store sessionstore.Store, directory string, includeSubagents bool, batchSize int) error {
	if !sessionstore.ValidateUUID(sessionID) {
		return fmt.Errorf("invalid session_id: %s", sessionID)
	}
	o := defaultOpts()
	if directory != "" {
		o.directory = directory
	}
	resolved, err := findSessionFile(sessionID, o)
	if err != nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	// The destination project_key MUST match what the *_from_store readers and
	// resume materialization compute for the same directory, otherwise an
	// imported session is written under one key and looked up under another.
	// Readers use ProjectKeyForDirectory (realpath + NFC canonicalization), so
	// when a directory is given we key the same way. When no directory is given
	// (all-projects search) fall back to the on-disk directory name, which is
	// already the CLI's canonical encoding for the project the file was found in.
	var projectKey string
	if directory != "" {
		projectKey = sessionstore.ProjectKeyForDirectory(directory)
	} else {
		projectKey = filepath.Base(filepath.Dir(resolved))
	}
	if batchSize <= 0 {
		batchSize = importMaxBatchEntries
	}

	mainKey := sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sessionID}
	if err := appendJSONLFileInBatches(ctx, resolved, mainKey, store, batchSize); err != nil {
		return err
	}

	if !includeSubagents {
		return nil
	}

	sessionDir := strings.TrimSuffix(resolved, ".jsonl")
	subagentsDir := filepath.Join(sessionDir, "subagents")
	for _, filePath := range collectJSONLFiles(subagentsDir) {
		rel, err := filepath.Rel(sessionDir, filePath)
		if err != nil {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		parts[len(parts)-1] = strings.TrimSuffix(parts[len(parts)-1], ".jsonl")
		subKey := sessionstore.SessionKey{
			ProjectKey: projectKey,
			SessionID:  sessionID,
			Subpath:    strings.Join(parts, "/"),
		}
		if err := appendJSONLFileInBatches(ctx, filePath, subKey, store, batchSize); err != nil {
			return err
		}
		// The on-disk .jsonl has no agent_metadata; import the .meta.json
		// sidecar as a synthetic agent_metadata entry so resume can recreate it.
		metaPath := strings.TrimSuffix(filePath, ".jsonl") + ".meta.json"
		if metaBytes, err := os.ReadFile(metaPath); err == nil { //nolint:gosec // path derived from resolved session file
			var meta map[string]any
			if json.Unmarshal(metaBytes, &meta) == nil {
				meta["type"] = "agent_metadata"
				if b, err := json.Marshal(meta); err == nil {
					if err := store.Append(ctx, subKey, []sessionstore.Entry{sessionstore.Entry(b)}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// appendJSONLFileInBatches stream-reads a JSONL file and flushes to
// store.Append in batches of batchSize entries (or importMaxBatchBytes of line
// text). Skips blank lines.
func appendJSONLFileInBatches(ctx context.Context, filePath string, key sessionstore.SessionKey, store sessionstore.Store, batchSize int) error {
	f, err := os.Open(filepath.Clean(filePath))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var batch []sessionstore.Entry
	nbytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := store.Append(ctx, key, batch); err != nil {
			return err
		}
		batch = nil
		nbytes = 0
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\n")
		if line == "" {
			continue
		}
		// Validate each line is JSON; store entries are opaque raw bytes.
		if !json.Valid([]byte(line)) {
			continue
		}
		batch = append(batch, sessionstore.Entry(line))
		nbytes += len(line)
		if len(batch) >= batchSize || nbytes >= importMaxBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// collectJSONLFiles recursively collects *.jsonl files under baseDir, sorted
// per directory for deterministic order. Returns nil if baseDir is absent.
func collectJSONLFiles(baseDir string) []string {
	var out []string
	var walk func(dir string)
	walk = func(dir string) {
		names, err := readDirNames(dir)
		if err != nil {
			return
		}
		sort.Strings(names)
		for _, name := range names {
			full := filepath.Join(dir, name)
			fi, err := os.Stat(full)
			if err != nil {
				continue
			}
			if fi.IsDir() {
				walk(full)
			} else if strings.HasSuffix(name, ".jsonl") {
				out = append(out, full)
			}
		}
	}
	walk(baseDir)
	return out
}
