package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLibrary returns $SUBTITLE_SYNC_LIB, the test library the gated
// real-file tests read from (never write to). They are skipped without it.
func testLibrary(t *testing.T) string {
	t.Helper()
	lib := os.Getenv("SUBTITLE_SYNC_LIB")
	if lib == "" {
		t.Skip("set SUBTITLE_SYNC_LIB to the test library (read only)")
	}
	abs, err := filepath.Abs(lib)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(abs)
}

// within reports whether p is dir or below it.
func within(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}
