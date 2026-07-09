package sessionstore

import (
	"path/filepath"
	"testing"
)

func TestFilePathToSessionKey(t *testing.T) {
	projectsDir := filepath.Join("/home", "u", ".claude", "projects")
	pk := "-Users-foo-bar"
	sid := "550e8400-e29b-41d4-a716-446655440000"

	tests := []struct {
		name     string
		filePath string
		want     SessionKey
		ok       bool
	}{
		{
			name:     "main transcript",
			filePath: filepath.Join(projectsDir, pk, sid+".jsonl"),
			want:     SessionKey{ProjectKey: pk, SessionID: sid},
			ok:       true,
		},
		{
			name:     "subagent transcript",
			filePath: filepath.Join(projectsDir, pk, sid, "subagents", "agent-abc.jsonl"),
			want:     SessionKey{ProjectKey: pk, SessionID: sid, Subpath: "subagents/agent-abc"},
			ok:       true,
		},
		{
			name:     "nested subagent transcript",
			filePath: filepath.Join(projectsDir, pk, sid, "subagents", "workflows", "run1", "agent-x.jsonl"),
			want:     SessionKey{ProjectKey: pk, SessionID: sid, Subpath: "subagents/workflows/run1/agent-x"},
			ok:       true,
		},
		{
			name:     "outside projects dir",
			filePath: filepath.Join("/tmp", "other", "x.jsonl"),
			want:     SessionKey{},
			ok:       false,
		},
		{
			name:     "too few components",
			filePath: filepath.Join(projectsDir, "only-one.jsonl"),
			want:     SessionKey{},
			ok:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FilePathToSessionKey(tt.filePath, projectsDir)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Fatalf("key = %+v, want %+v", got, tt.want)
			}
		})
	}
}
