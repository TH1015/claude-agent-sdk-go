package session

import (
	"os"
	"path/filepath"
)

func osMkdirAll(p string) error        { return os.MkdirAll(p, 0o755) }
func osWriteFile(p, c string) error    { return os.WriteFile(p, []byte(c), 0o644) }
func absPath(p string) (string, error) { return filepath.Abs(p) }
