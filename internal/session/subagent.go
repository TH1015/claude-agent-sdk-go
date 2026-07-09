package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

// jsonlEntriesToMessages converts visible transcript entries into Messages.
// Shared by the main and subagent chain builders.
func jsonlEntriesToMessages(sessionID string, visible []jsonlEntry) []Message {
	messages := make([]Message, 0, len(visible))
	for _, e := range visible {
		msg := Message{
			Type:      e.entryType,
			SessionID: sessionID,
		}
		if uuid, ok := e.raw["uuid"].(string); ok {
			msg.UUID = uuid
		}
		if meta, ok := e.raw["isMeta"].(bool); ok && meta {
			msg.IsMeta = true
		}
		if rawMsg, ok := e.raw["message"].(map[string]any); ok {
			msg.RawMessage = rawMsg
			msg.Content = parseMessageContent(rawMsg)
		}
		messages = append(messages, msg)
	}
	return messages
}

// buildSubagentMessages builds a subagent conversation chain and converts it to
// Messages. Subagent transcripts are linear (no compaction/sidechains): find
// the last user/assistant entry and walk parentUuid links back to the root.
// Mirrors the Python SDK's _build_subagent_chain + _entries_to_subagent_messages.
func buildSubagentMessages(sessionID string, entries []jsonlEntry) []Message {
	if len(entries) == 0 {
		return nil
	}
	byUUID := make(map[string]int, len(entries))
	for i, e := range entries {
		if uuid := entryUUID(e); uuid != "" {
			byUUID[uuid] = i
		}
	}

	// Leaf = last user/assistant entry.
	leafIdx := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].entryType == entryTypeUser || entries[i].entryType == entryTypeAssistant {
			leafIdx = i
			break
		}
	}
	if leafIdx < 0 {
		return nil
	}

	var chain []jsonlEntry
	seen := make(map[string]bool)
	cur := leafIdx
	for cur >= 0 {
		e := entries[cur]
		uuid := entryUUID(e)
		if seen[uuid] {
			break
		}
		seen[uuid] = true
		chain = append(chain, e)
		parent := entryParentUUID(e)
		if parent == "" {
			break
		}
		pi, ok := byUUID[parent]
		if !ok {
			break
		}
		cur = pi
	}
	// Reverse to chronological order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	// Subagent chain keeps user/assistant entries directly (no visibility
	// filter beyond type — matches _entries_to_subagent_messages).
	var visible []jsonlEntry
	for _, e := range chain {
		if e.entryType == entryTypeUser || e.entryType == entryTypeAssistant {
			visible = append(visible, e)
		}
	}
	return jsonlEntriesToMessages(sessionID, visible)
}

// isoToEpochMSSession parses an ISO-8601 timestamp to Unix epoch milliseconds.
func isoToEpochMSSession(ts string) (int64, bool) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// resolveSubagentsDir resolves the subagents directory for a session:
// <projectDir>/<sessionID>/subagents/. Returns "" if the session file is not
// found. Reuses the project-dir resolution used by findSessionFile.
func resolveSubagentsDir(sessionID string, o sessionOpts) string {
	path, err := findSessionFile(sessionID, o)
	if err != nil {
		return ""
	}
	// Strip the .jsonl suffix to derive the session directory.
	sessionDir := trimJSONL(path)
	return joinPath(sessionDir, "subagents")
}

// ListSubagents lists subagent IDs for a session by scanning the subagents
// directory tree (agent-*.jsonl files, possibly nested). Returns an empty slice
// if the session is not found or the session_id is not a valid UUID. Mirrors
// the Python SDK's list_subagents.
func ListSubagents(sessionID string, opts ...Option) []string {
	if !validateSessionUUID(sessionID) {
		return nil
	}
	o := defaultOpts()
	for _, fn := range opts {
		fn(&o)
	}
	dir := resolveSubagentsDir(sessionID, o)
	if dir == "" {
		return nil
	}
	var ids []string
	for _, af := range collectAgentFiles(dir) {
		ids = append(ids, af.agentID)
	}
	return ids
}

// GetSubagentMessages reads a subagent's conversation messages from its JSONL
// transcript. Returns an empty slice if the session/subagent is not found or
// session_id is invalid. Mirrors the Python SDK's get_subagent_messages.
func GetSubagentMessages(sessionID, agentID string, opts ...Option) []Message {
	if !validateSessionUUID(sessionID) || agentID == "" {
		return nil
	}
	o := defaultOpts()
	for _, fn := range opts {
		fn(&o)
	}
	dir := resolveSubagentsDir(sessionID, o)
	if dir == "" {
		return nil
	}
	var match string
	for _, af := range collectAgentFiles(dir) {
		if af.agentID == agentID {
			match = af.path
			break
		}
	}
	if match == "" {
		return nil
	}
	entries, err := parseJSONLFile(match)
	if err != nil {
		return nil
	}
	messages := buildSubagentMessages(sessionID, entries)
	return pageMessages(messages, o.limit, o.offset)
}

// agentFile pairs a subagent ID with its transcript file path.
type agentFile struct {
	agentID string
	path    string
}

// collectAgentFiles recursively collects agent-*.jsonl files from a directory
// tree (subagent transcripts may be nested under workflows/<runId>/). Mirrors
// the Python SDK's _collect_agent_files.
func collectAgentFiles(baseDir string) []agentFile {
	var results []agentFile
	var walk func(dir string)
	walk = func(dir string) {
		names, err := readDirNames(dir)
		if err != nil {
			return
		}
		sortStrings(names)
		for _, name := range names {
			full := joinPath(dir, name)
			fi, err := statPath(full)
			if err != nil {
				continue
			}
			if fi.IsDir() {
				walk(full)
				continue
			}
			if hasPrefix(name, "agent-") && hasSuffix(name, ".jsonl") {
				agentID := name[len("agent-") : len(name)-len(".jsonl")]
				results = append(results, agentFile{agentID: agentID, path: full})
			}
		}
	}
	walk(baseDir)
	return results
}

func trimJSONL(path string) string           { return strings.TrimSuffix(path, ".jsonl") }
func joinPath(parts ...string) string        { return filepath.Join(parts...) }
func statPath(p string) (os.FileInfo, error) { return os.Stat(p) }
func sortStrings(ss []string)                { sort.Strings(ss) }
func hasPrefix(s, p string) bool             { return strings.HasPrefix(s, p) }
func hasSuffix(s, p string) bool             { return strings.HasSuffix(s, p) }
func validateSessionUUID(s string) bool      { return sessionstore.ValidateUUID(s) }
