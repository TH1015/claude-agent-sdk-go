package session

import (
	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

// summaryEntryToSDKInfo converts a folded SessionSummaryEntry to SDKSessionInfo,
// applying the same filtering as the disk lite-parse: returns nil for sidechain
// sessions or sessions with no extractable summary. projectPath is used as a
// cwd fallback. Mirrors the Python SDK's summary_entry_to_sdk_info.
func summaryEntryToSDKInfo(entry sessionstore.SummaryEntry, projectPath string) *SDKSessionInfo {
	data := entry.Data
	if data == nil {
		return nil
	}
	if v, _ := data["is_sidechain"].(bool); v {
		return nil
	}

	firstPrompt := summaryFirstPrompt(data)
	customTitle := summaryCustomTitle(data)

	summary := summaryText(data, customTitle, firstPrompt)
	if summary == "" {
		return nil
	}

	info := &SDKSessionInfo{
		SessionID:    entry.SessionID,
		Summary:      summary,
		LastModified: entry.MTime,
		FileSize:     nil, // stores have no per-summary byte count
		CustomTitle:  customTitle,
		FirstPrompt:  firstPrompt,
		GitBranch:    strPtrOrNil(data, "git_branch"),
		Tag:          strPtrOrNil(data, "tag"),
	}
	if s, ok := data["cwd"].(string); ok && s != "" {
		info.Cwd = &s
	} else if projectPath != "" {
		p := projectPath
		info.Cwd = &p
	}
	info.CreatedAt = summaryCreatedAt(data)
	return info
}

// summaryFirstPrompt resolves the first-prompt display value: the locked first
// prompt, else the slash-command fallback.
func summaryFirstPrompt(data map[string]any) *string {
	if locked, _ := data["first_prompt_locked"].(bool); locked {
		if s, ok := data["first_prompt"].(string); ok && s != "" {
			return &s
		}
		return nil
	}
	if s, ok := data["command_fallback"].(string); ok && s != "" {
		return &s
	}
	return nil
}

// summaryCustomTitle resolves the custom title: user title beats AI title.
func summaryCustomTitle(data map[string]any) *string {
	if s, ok := data["custom_title"].(string); ok && s != "" {
		return &s
	}
	if s, ok := data["ai_title"].(string); ok && s != "" {
		return &s
	}
	return nil
}

// summaryText applies the summary priority: custom_title > last_prompt >
// summary_hint > first_prompt.
func summaryText(data map[string]any, customTitle, firstPrompt *string) string {
	if customTitle != nil {
		return *customTitle
	}
	if s, ok := data["last_prompt"].(string); ok && s != "" {
		return s
	}
	if s, ok := data["summary_hint"].(string); ok && s != "" {
		return s
	}
	if firstPrompt != nil {
		return *firstPrompt
	}
	return ""
}

// summaryCreatedAt extracts created_at as epoch ms (int64 or float64 forms).
func summaryCreatedAt(data map[string]any) *int64 {
	if ms, ok := data["created_at"].(int64); ok {
		return &ms
	}
	if f, ok := data["created_at"].(float64); ok {
		ms := int64(f)
		return &ms
	}
	return nil
}

// strPtrOrNil returns a pointer to the non-empty string at key, or nil.
func strPtrOrNil(data map[string]any, key string) *string {
	if s, ok := data[key].(string); ok && s != "" {
		return &s
	}
	return nil
}

// FoldSessionSummary is re-exported so callers within the session package (and
// adapters) can maintain the sidecar. Delegates to the sessionstore package.
func FoldSessionSummary(prev *sessionstore.SummaryEntry, key sessionstore.SessionKey, entries []sessionstore.Entry) sessionstore.SummaryEntry {
	return sessionstore.FoldSessionSummary(prev, key, entries)
}
