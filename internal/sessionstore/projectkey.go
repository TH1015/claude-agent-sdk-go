package sessionstore

import (
	"os"
	"path/filepath"

	"golang.org/x/text/unicode/norm"
)

// maxSanitizedLength is the maximum length for a single filesystem path
// component. Most filesystems limit individual components to 255 bytes; 200
// leaves room for the hash suffix and separator. Matches the Python SDK's
// MAX_SANITIZED_LENGTH.
const maxSanitizedLength = 200

// simpleHash computes a 32-bit integer hash of s and renders it in base36,
// matching the CLI's directory naming (and the Python SDK's _simple_hash).
//
// The loop emulates JavaScript's `hash = (hash << 5) - hash + char; hash |= 0`
// where `|= 0` coerces the running value to a 32-bit signed integer on every
// iteration. Getting this bit-exact matters: any deviation produces a
// different directory suffix and the SessionStore key silently fails to line
// up with the CLI's on-disk transcript directory.
func simpleHash(s string) string {
	var h int32
	for _, ch := range s {
		// (h << 5) - h + ch, in 32-bit signed arithmetic. Go's int32 wraps on
		// overflow exactly like JS's `| 0` coercion, so no explicit masking.
		h = (h << 5) - h + int32(ch)
	}
	// JS Math.abs(hash). Use int64 to avoid overflow when h == math.MinInt32.
	n := int64(h)
	if n < 0 {
		n = -n
	}
	return toBase36(n)
}

// toBase36 renders a non-negative integer in base36 using the same digit set
// as JavaScript's Number.prototype.toString(36).
func toBase36(n int64) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}

// SanitizePath makes a string safe for use as a directory name.
//
// Every non-alphanumeric character (ASCII a-z, A-Z, 0-9) is replaced with a
// hyphen. When the result exceeds 200 runes it is truncated and a simpleHash
// suffix of the ORIGINAL name is appended so long paths stay unique. Matches
// the Python SDK's _sanitize_path and the CLI's directory naming. The
// replacement operates on Unicode code points, so a multi-byte rune becomes a
// single hyphen and the length check counts runes, not bytes.
func SanitizePath(name string) string {
	runes := []rune(name)
	sanitized := make([]rune, len(runes))
	for i, r := range runes {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sanitized[i] = r
		} else {
			sanitized[i] = '-'
		}
	}
	if len(sanitized) <= maxSanitizedLength {
		return string(sanitized)
	}
	h := simpleHash(name)
	return string(sanitized[:maxSanitizedLength]) + "-" + h
}

// CanonicalizePath resolves a directory path to its canonical form using
// EvalSymlinks (the Go equivalent of realpath) plus Unicode NFC normalization.
// On resolution failure it falls back to an absolute path (or the raw input),
// still NFC-normalized, matching the Python SDK's _canonicalize_path tolerance
// of missing directories.
func CanonicalizePath(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return norm.NFC.String(resolved)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return norm.NFC.String(abs)
	}
	return norm.NFC.String(dir)
}

// ProjectKeyForDirectory derives the SessionStore project_key for a directory.
//
// It applies the same realpath + NFC normalization + hashed sanitization the
// CLI uses for project directory names, so keys match between local-disk
// transcripts and store-mirrored transcripts even on filesystems that
// decompose Unicode (e.g. macOS HFS+). When dir is empty, the current working
// directory is used. Mirrors the Python SDK's project_key_for_directory.
func ProjectKeyForDirectory(dir string) string {
	target := dir
	if target == "" {
		target = "."
	}
	return SanitizePath(CanonicalizePath(target))
}

// ProjectsDirForEnv returns the projects directory, consulting envOverride's
// CLAUDE_CONFIG_DIR before the process environment. Callers that pass
// CLAUDE_CONFIG_DIR to the subprocess via options.ExtraEnv must resolve the
// same directory the subprocess writes to. Mirrors the Python SDK's
// _get_projects_dir.
func ProjectsDirForEnv(envOverride map[string]string) (string, error) {
	if envOverride != nil {
		if override := envOverride["CLAUDE_CONFIG_DIR"]; override != "" {
			return filepath.Join(norm.NFC.String(override), "projects"), nil
		}
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(norm.NFC.String(dir), "projects"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(norm.NFC.String(filepath.Join(home, ".claude")), "projects"), nil
}
