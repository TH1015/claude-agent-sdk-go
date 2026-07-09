package sessionstore

import "testing"

// TestSimpleHashParity checks simpleHash against values produced by the Python
// SDK's _simple_hash (which mirrors the CLI's djb2 + base36 hash). Any drift
// silently misaligns project keys with on-disk directories, so the fixtures
// are generated directly from the Python implementation.
func TestSimpleHashParity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a", "2p"},
		{"", "0"},
		{"hello", "1n1e4y"},
		{"The quick brown fox", "srk09p"},
		{"/Users/foo/bar", "bhegyc"},
		{"/home/user/project-name", "d6xdq6"},
		{"C:\\Users\\foo", "6863kd"},
		{repeatStr("x", 250), "mmfaww"},
		{repeatStr("y", 201), "6lq4uf"},
		{repeatStr("z", 200), "bylzi8"},
		{"/path/with/émoji/😀/end", "ndq9ev"},
		{"café/münchen", "22mw01"},
		{"日本語ディレクトリ", "b9zhag"},
	}
	for _, tt := range cases {
		if got := simpleHash(tt.in); got != tt.want {
			t.Errorf("simpleHash(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSanitizePathParity checks SanitizePath against the Python SDK's
// _sanitize_path, including >200-char truncate-plus-hash and Unicode handling.
func TestSanitizePathParity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a", "a"},
		{"", ""},
		{"hello", "hello"},
		{"The quick brown fox", "The-quick-brown-fox"},
		{"/Users/foo/bar", "-Users-foo-bar"},
		{"/home/user/project-name", "-home-user-project-name"},
		{"C:\\Users\\foo", "C--Users-foo"},
		{repeatStr("z", 200), repeatStr("z", 200)},
		{repeatStr("x", 250), repeatStr("x", 200) + "-mmfaww"},
		{repeatStr("y", 201), repeatStr("y", 200) + "-6lq4uf"},
		{"/path/with/émoji/😀/end", "-path-with--moji---end"},
		{"café/münchen", "caf--m-nchen"},
		{"日本語ディレクトリ", "---------"},
	}
	for _, tt := range cases {
		if got := SanitizePath(tt.in); got != tt.want {
			t.Errorf("SanitizePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSanitizePathLongSegments covers a long path with repeated segments,
// asserting exact parity with the Python fixture.
func TestSanitizePathLongSegments(t *testing.T) {
	in := "/very/long/"
	for i := 0; i < 40; i++ {
		in += "segment/"
	}
	want := "-very-long-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segment-segme-gsot1z"
	if got := SanitizePath(in); got != want {
		t.Errorf("SanitizePath(long) = %q, want %q", got, want)
	}
}

func repeatStr(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
