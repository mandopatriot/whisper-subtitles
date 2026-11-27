package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLibrary returns $WHISPER_SUBTITLES_LIB, the test library the gated
// real-file tests read from (never write to). They are skipped without it.
func testLibrary(t *testing.T) string {
	t.Helper()
	lib := os.Getenv("WHISPER_SUBTITLES_LIB")
	if lib == "" {
		t.Skip("set WHISPER_SUBTITLES_LIB to the test library (read only)")
	}
	abs, err := filepath.Abs(lib)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(abs)
}

// writableOut fails the test when out is inside the test library lib or a
// folder in $WHISPER_SUBTITLES_PROTECT (colon-separated, such as a real
// library): the real-file tests only read those.
func writableOut(t *testing.T, out, lib string) {
	t.Helper()
	if b := protectedBy(out, lib); b != "" {
		t.Fatalf("%s is inside %s, which the tests only read", out, b)
	}
}

// protectedBy is the folder out is inside of, lib or one in
// $WHISPER_SUBTITLES_PROTECT, or "" when out may be written.
func protectedBy(out, lib string) string {
	abs, err := filepath.Abs(out)
	if err != nil {
		return out
	}
	for _, bad := range append([]string{lib}, filepath.SplitList(os.Getenv("WHISPER_SUBTITLES_PROTECT"))...) {
		if bad == "" {
			continue
		}
		if b, err := filepath.Abs(bad); err == nil && within(abs, b) {
			return b
		}
	}
	return ""
}

func TestProtectedBy(t *testing.T) {
	t.Setenv("WHISPER_SUBTITLES_PROTECT", "/srv/media:/mnt/films")
	for out, want := range map[string]string{
		"/lib/Movies/x":        "/lib",
		"/srv/media":           "/srv/media",
		"/mnt/films/Cars/out":  "/mnt/films",
		"/mnt/filmsx/out":      "",
		"/tmp/whisper-results": "",
	} {
		if got := protectedBy(out, "/lib"); got != want {
			t.Errorf("protectedBy(%q) = %q, want %q", out, got, want)
		}
	}
	t.Setenv("WHISPER_SUBTITLES_PROTECT", "")
	if got := protectedBy("/srv/media/x", "/lib"); got != "" {
		t.Errorf("with nothing protected: %q", got)
	}
}

// within reports whether p is dir or below it.
func within(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}
