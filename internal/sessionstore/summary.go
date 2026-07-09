package sessionstore

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// FoldSessionSummary folds a batch of appended entries into the running
// summary for a key. Stores call this from inside Append to keep a
// SummaryEntry sidecar up to date without re-reading the transcript. prev is
// the previous summary for the same key (nil on the first append).
//
// Do NOT call this for keys with a Subpath — subagent transcripts must not
// contribute to the main session's summary. Guard with key.Subpath == "".
//
// MTime is NOT set by the fold; it is the sidecar's storage write time and
// must be stamped by the adapter after persisting (it must share a clock with
// ListEntry.MTime). For a new session (prev == nil) the fold returns MTime 0
// as a placeholder for the adapter to overwrite.
//
// All derived state lives in the opaque Data map; stores persist it verbatim.
// Mirrors the Python SDK's fold_session_summary.
func FoldSessionSummary(prev *SummaryEntry, key SessionKey, entries []Entry) SummaryEntry {
	var summary SummaryEntry
	if prev != nil {
		summary = SummaryEntry{
			SessionID: prev.SessionID,
			MTime:     prev.MTime,
			Data:      cloneData(prev.Data),
		}
	} else {
		summary = SummaryEntry{SessionID: key.SessionID, MTime: 0, Data: map[string]any{}}
	}
	data := summary.Data

	for _, raw := range entries {
		var entry map[string]any
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}

		ms, hasMS := isoToEpochMS(entry["timestamp"])

		if _, ok := data["is_sidechain"]; !ok {
			data["is_sidechain"] = entry["isSidechain"] == true
		}
		if _, ok := data["created_at"]; !ok && hasMS {
			data["created_at"] = ms
		}
		if _, ok := data["cwd"]; !ok {
			if cwd, ok := entry["cwd"].(string); ok && cwd != "" {
				data["cwd"] = cwd
			}
		}

		foldFirstPrompt(data, entry)

		for src, dst := range lastWinsFields {
			if val, ok := entry[src].(string); ok {
				data[dst] = val
			}
		}

		if entry["type"] == "tag" {
			if tagVal, ok := entry["tag"].(string); ok && tagVal != "" {
				data["tag"] = tagVal
			} else {
				delete(data, "tag")
			}
		}
	}

	return summary
}

// lastWinsFields maps JSONL entry keys to SummaryEntry.Data keys for
// last-wins string fields. Each appended entry overwrites the previous value
// when present.
var lastWinsFields = map[string]string{
	"customTitle": "custom_title",
	"aiTitle":     "ai_title",
	"lastPrompt":  "last_prompt",
	"summary":     "summary_hint",
	"gitBranch":   "git_branch",
}

// summaryMaxFirstPromptLen mirrors the disk path's first-prompt truncation.
const summaryMaxFirstPromptLen = 200

// skipFirstPromptPattern and commandNamePattern mirror the session package's
// equivalents; duplicated here to keep the sessionstore package free of an
// import cycle with session (which imports sessionstore for the *FromStore
// variants).
var (
	summarySkipFirstPromptPattern = regexp.MustCompile(
		`^(?:<local-command-stdout>|<session-start-hook>|<tick>|<goal>|` +
			`\[Request interrupted by user[^\]]*\]|` +
			`\s*<ide_opened_file>[\s\S]*</ide_opened_file>\s*$|` +
			`\s*<ide_selection>[\s\S]*</ide_selection>\s*$)`,
	)
	summaryCommandNamePattern = regexp.MustCompile(`<command-name>(.*?)</command-name>`)
)

// foldFirstPrompt replicates the disk path's first-prompt extraction for a
// single parsed entry. Sets first_prompt + first_prompt_locked on a real
// match, or stashes a command_fallback for slash-command messages.
func foldFirstPrompt(data, entry map[string]any) {
	if data["first_prompt_locked"] == true {
		return
	}
	if entry["type"] != "user" {
		return
	}
	if entry["isMeta"] == true || entry["isCompactSummary"] == true {
		return
	}
	// Skip tool_result-carrying user messages.
	if message, ok := entry["message"].(map[string]any); ok {
		if content, ok := message["content"].([]any); ok {
			for _, b := range content {
				if block, ok := b.(map[string]any); ok && block["type"] == "tool_result" {
					return
				}
			}
		}
	}

	for _, raw := range entryTextBlocks(entry) {
		result := strings.TrimSpace(strings.ReplaceAll(raw, "\n", " "))
		if result == "" {
			continue
		}
		if m := summaryCommandNamePattern.FindStringSubmatch(result); m != nil {
			if _, ok := data["command_fallback"]; !ok {
				data["command_fallback"] = m[1]
			}
			continue
		}
		if summarySkipFirstPromptPattern.MatchString(result) {
			continue
		}
		if len([]rune(result)) > summaryMaxFirstPromptLen {
			result = strings.TrimRight(string([]rune(result)[:summaryMaxFirstPromptLen]), " ") + "…"
		}
		data["first_prompt"] = result
		data["first_prompt_locked"] = true
		return
	}
}

// entryTextBlocks extracts text strings from a type=="user" entry's message
// content.
func entryTextBlocks(entry map[string]any) []string {
	message, ok := entry["message"].(map[string]any)
	if !ok {
		return nil
	}
	content := message["content"]
	if s, ok := content.(string); ok {
		return []string{s}
	}
	if blocks, ok := content.([]any); ok {
		var texts []string
		for _, b := range blocks {
			if block, ok := b.(map[string]any); ok && block["type"] == "text" {
				if t, ok := block["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
		return texts
	}
	return nil
}

// isoToEpochMS parses an ISO-8601 timestamp to Unix epoch milliseconds.
func isoToEpochMS(ts any) (int64, bool) {
	s, ok := ts.(string)
	if !ok {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// cloneData makes a shallow copy of a summary Data map so folds never mutate
// the previous entry in place.
func cloneData(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
