package session

import "testing"

// TestEncodeCwdShortPathUnchanged confirms encodeCwd produces the plain
// per-character replacement for short ASCII paths (backward compatibility with
// existing on-disk directories and fixtures).
func TestEncodeCwdShortPathUnchanged(t *testing.T) {
	cases := map[string]string{
		"/home/user/project": "-home-user-project",
		"/a/b/c":             "-a-b-c",
		"simple":             "simple",
	}
	for in, want := range cases {
		if got := encodeCwd(in); got != want {
			t.Errorf("encodeCwd(%q) = %q, want %q", in, got, want)
		}
	}
}
