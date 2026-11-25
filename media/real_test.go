package media

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRealFiles cuts the plugin's windows (WindowStarts) from every .mkv
// under $SUBTITLE_SYNC_REAL_DIR into $SUBTITLE_SYNC_OUT_DIR, for checking
// with ffmpeg. The input must be inside the test library $SUBTITLE_SYNC_LIB,
// which is only read; the output must be outside it and outside every folder
// in $SUBTITLE_SYNC_PROTECT (colon-separated, such as a real library). It is
// skipped unless all three are set.
func TestRealFiles(t *testing.T) {
	in, out, libEnv := os.Getenv("SUBTITLE_SYNC_REAL_DIR"), os.Getenv("SUBTITLE_SYNC_OUT_DIR"), os.Getenv("SUBTITLE_SYNC_LIB")
	if in == "" || out == "" || libEnv == "" {
		t.Skip("set SUBTITLE_SYNC_REAL_DIR, SUBTITLE_SYNC_OUT_DIR and SUBTITLE_SYNC_LIB")
	}
	within := func(p, dir string) bool {
		p, dir = filepath.Clean(p), filepath.Clean(dir)
		return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
	}
	lib, _ := filepath.Abs(libEnv)
	absIn, _ := filepath.Abs(in)
	absOut, _ := filepath.Abs(out)
	if !within(absIn, lib) {
		t.Fatalf("SUBTITLE_SYNC_REAL_DIR must be %s or below it, not %s", lib, absIn)
	}
	for _, bad := range append([]string{lib}, filepath.SplitList(os.Getenv("SUBTITLE_SYNC_PROTECT"))...) {
		if bad == "" {
			continue
		}
		if b, _ := filepath.Abs(bad); within(absOut, b) {
			t.Fatalf("SUBTITLE_SYNC_OUT_DIR must not be under %s", b)
		}
	}
	n := 0
	filepath.WalkDir(in, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".mkv") {
			return nil
		}
		n++
		f, err := os.Open(p)
		if err != nil {
			t.Error(err)
			return nil
		}
		defer f.Close()
		fi, _ := f.Stat()
		var peak uint64
		m, err := OpenMKV(f, fi.Size())
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
			return nil
		}
		ch, err := PickAudio(m.Tracks, "en")
		if err != nil || m.DurationMs <= 0 {
			t.Errorf("%s: %v (duration %d)", filepath.Base(p), err, m.DurationMs)
			return nil
		}
		tr := &ch.Track
		fmt.Printf("%s: picked track %d %s %s %q of %d\n", filepath.Base(p), tr.Number, tr.CodecID, tr.Language, tr.Name, len(m.Tracks))
		for i, start := range WindowStarts(m.DurationMs) {
			name := filepath.Join(out, fmt.Sprintf("%02d-w%02d.mka", n, i))
			o, err := os.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			win, err := m.Extract(*tr, start, WindowMs, 64<<20, o)
			o.Close()
			if err != nil {
				t.Errorf("%s window %d: %v", filepath.Base(p), i, err)
				continue
			}
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapInuse)
			fmt.Printf("%s\t%s\t%s\t%s\tbase=%d\tdur=%d\tbytes=%d\tcapped=%t\n",
				name, filepath.Base(p), tr.CodecID, tr.Language, win.BaseMs, win.DurationMs(), win.Bytes, win.Capped)
		}
		fmt.Printf("%s: %d tracks, %d cues, warnings %q, peak heap %d MiB\n", filepath.Base(p), len(m.Tracks), len(m.cues), m.Warnings, peak>>20)
		return nil
	})
	if n == 0 {
		t.Errorf("no .mkv under %s", in)
	}
}
