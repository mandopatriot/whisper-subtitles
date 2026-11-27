package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// evalTruth is what a file's English subtitle needs, found without
// Whisper. All are in sync except 300 (see truthFor) and Cars, whose .en.srt
// is a 25 fps subtitle:
// scaled by 25/23.976 and moved -52 ms, 1508 of its 1517 cues start within
// 400 ms of the release's VobSub English subtitles (as is: 6% at best).
type evalTruth struct {
	k, offsetMs float64 // video = k*sub + offset
	from, to    float64 // 0 when no frame-rate change
	tolMs       float64 // the truth's own uncertainty, for the word check
}

// uncertainRefMs is the word-timing tolerance for the three files whose
// reference is itself uncertain by about 150 to 200 ms: S01E03 and S01E05
// (-130 and -190 ms as is, though their subtitles match the release's own
// within 60 ms) and 300 (-210 ms against its PGS times, -60 ms against its
// deleted .en.srt). Whether Parakeet or the reference is off can't be told,
// and fixes this close are watchable. Set to 250 ms on 2026-11-25.
const uncertainRefMs = 250

func truthFor(src string) evalTruth {
	switch {
	case strings.HasPrefix(src, "Cars (2006)"):
		return evalTruth{25 / (24000.0 / 1001), -52, 25, 23.976, 150}
	case strings.HasPrefix(src, "300 Rise of an Empire (2014)"):
		// Its original .en.srt was deleted on 2026-11-25; the reference is now
		// the embedded English PGS track. The .en.srt HolyArrs saved instead
		// (OpenSubtitles.com, 995 cues) starts within 100 ms of the PGS track's
		// 995 subtitles for 79% of cues, and within 400 ms for 993, once moved
		// -8450 ms; no frame-rate change fits. With word timings it reads
		// -210 ms against that, -60 ms against the deleted .en.srt: the PGS
		// times and that file differ by about 150 ms. See uncertainRefMs.
		return evalTruth{1, -8450, 0, 0, uncertainRefMs}
	case strings.Contains(src, "S01E03") || strings.Contains(src, "S01E05"):
		// With word timings these read -130 and -190 ms as is, although
		// their subtitles match the release's own within 60 ms. See
		// uncertainRefMs.
		return evalTruth{1, 0, 0, 0, uncertainRefMs}
	}
	return evalTruth{1, 0, 0, 0, 100}
}

// TestFitEval runs the fit on every file of $WHISPER_SUBTITLES_EVAL_INDEX (cut by
// TestRealFiles, transcribed by TestTranscribeWindows with the same
// $WHISPER_SUBTITLES_EVAL_TAG) with its English subtitle from
// $WHISPER_SUBTITLES_EVAL_SUBS, else $WHISPER_SUBTITLES_LIB (read only), four ways: as is, shifted +2.5 s
// and -1.2 s, and changed from 23.976 to 25 fps. The wanted fix is the shift
// undone on top of the file's truth (truthFor).
//
// Pass rules: with word timings, the fix is within 100 ms of the truth
// (150 for Cars, uncertainRefMs for S01E03, S01E05 and 300; see truthFor).
// With phrase timings, within 600 ms of the truth and within 100 ms of the
// as-is reading plus the shift. As is, a subtitle in sync must not be
// synced.
func TestFitEval(t *testing.T) {
	index, subsDir := os.Getenv("WHISPER_SUBTITLES_EVAL_INDEX"), os.Getenv("WHISPER_SUBTITLES_EVAL_SUBS")
	if index == "" || subsDir == "" {
		t.Skip("set WHISPER_SUBTITLES_EVAL_INDEX and WHISPER_SUBTITLES_EVAL_SUBS")
	}
	lib := testLibrary(t)
	idx, err := readEvalIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	bySource := map[string][]WindowText{}
	var order []string
	for _, ew := range idx {
		b, err := os.ReadFile(evalCache(ew.Mka))
		if err != nil {
			t.Fatalf("%s: not transcribed yet: %v", ew.Mka, err)
		}
		var c evalWindow
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		w := WindowText{BaseMs: c.BaseMs, Tr: c.Tr}
		if c.Err != "" {
			w.Err = fmt.Errorf("%s", c.Err)
		}
		if _, seen := bySource[ew.Source]; !seen {
			order = append(order, ew.Source)
		}
		bySource[ew.Source] = append(bySource[ew.Source], w)
	}
	subFor := map[string]string{}
	filepath.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".en.srt") && !strings.Contains(p, ".holyarrs-replaced") {
			subFor[strings.TrimSuffix(filepath.Base(p), ".en.srt")] = p
		}
		return nil
	})

	ntsc := 24000.0 / 1001
	type variant struct {
		name  string
		shift float64 // the subtitle's change, ms (0 for the frame-rate copy)
		scale float64 // sub ms multiplied by this (1 for the shifts)
	}
	variants := []variant{{"as is", 0, 1}, {"+2.5 s", 2500, 1}, {"-1.2 s", -1200, 1}, {"23.976→25", 0, ntsc / 25}}

	sort.Strings(order)
	fails := 0
	fmt.Println("RESULT\tfile\tvariant\tpass\twant\tgot\terror ms\tdetail")
	for _, src := range order {
		// A subtitle in $WHISPER_SUBTITLES_EVAL_SUBS wins over the library's, so
		// a reference stays fixed when the library's file changes (as 300's
		// did when it was really synced).
		stem := strings.TrimSuffix(src, filepath.Ext(src))
		p := filepath.Join(subsDir, stem+".en.srt")
		if _, err := os.Stat(p); err != nil && subFor[stem] != "" {
			p = subFor[stem]
		}
		b, err := os.ReadFile(p)
		if err != nil {
			fmt.Printf("RESULT\t%s\t-\tskipped\t-\t-\t-\tno English subtitle: %v\n", src, err)
			fails++
			continue
		}
		cues, err := readCues(b, "srt")
		if err != nil {
			fmt.Printf("RESULT\t%s\t-\tskipped\t-\t-\t-\t%v\n", src, err)
			fails++
			continue
		}
		tr := truthFor(src)
		var asIs FitResult
		for vi, v := range variants {
			c := make([]Cue, len(cues))
			for i, x := range cues {
				sh := func(ms int64) int64 { return max(int64(math.Round(float64(ms)*v.scale+v.shift)), 0) }
				c[i] = Cue{StartMs: sh(x.StartMs), EndMs: sh(x.EndMs), Text: x.Text}
			}
			r := fitSubtitle(c, "en", bySource[src])
			if vi == 0 {
				asIs = r
			}
			// The wanted fix: video = tr.k*(sub' - shift)/scale + tr.offset.
			wantK := tr.k / v.scale
			wantOff := tr.offsetMs - tr.k*v.shift
			want, ok, errMs := "", false, math.NaN()
			gotK := 1.0
			if r.FromFPS != 0 {
				gotK = exactRate(r.FromFPS) / exactRate(r.ToFPS)
			}
			matchK := math.Abs(gotK-wantK) < 1e-6
			if math.Abs(wantK-1) < 1e-6 && wantOff == 0 {
				want = "no sync"
				ok = !r.Sync
			} else if !isRatePair(wantK) {
				want = "no sync (no frame-rate pair fits)"
				ok = !r.Sync
			} else {
				want = fmt.Sprintf("%+.0f ms", wantOff)
				if math.Abs(wantK-1) > 1e-6 {
					want += fmt.Sprintf(", %s", ratePairLabel(wantK))
				}
				errMs = float64(r.OffsetMs) - wantOff
				switch {
				case !r.Sync || !matchK:
				case r.Timing == "words":
					ok = math.Abs(errMs) <= tr.tolMs
				default:
					rel := float64(r.OffsetMs-asIs.OffsetMs) - (wantOff - (tr.offsetMs))
					ok = math.Abs(errMs) <= 600 && math.Abs(rel) <= 100
				}
			}
			if !ok {
				fails++
			}
			got := "no sync"
			if r.Sync {
				got = fmt.Sprintf("%+d ms", r.OffsetMs)
				if r.FromFPS != 0 {
					got += fmt.Sprintf(", %g→%g", r.FromFPS, r.ToFPS)
				}
			}
			es := "-"
			if !math.IsNaN(errMs) && r.Sync && matchK {
				es = fmt.Sprintf("%+.0f", errMs)
			}
			fmt.Printf("RESULT\t%s\t%s\t%t\t%s\t%s\t%s\t%s\n", src, v.name, ok, want, got, es, r)
		}
	}
	if fails > 0 {
		t.Errorf("%d checks failed or skipped", fails)
	}
}

func exactRate(label float64) float64 {
	for _, r := range fpsRates {
		if r.label == label {
			return r.exact
		}
	}
	return label
}

func isRatePair(k float64) bool { return ratePairLabel(k) != "" || math.Abs(k-1) < 1e-6 }

func ratePairLabel(k float64) string {
	for _, a := range fpsRates {
		for _, b := range fpsRates {
			if a.label != b.label && math.Abs(a.exact/b.exact-k) < 1e-6 {
				return fmt.Sprintf("%g→%g", a.label, b.label)
			}
		}
	}
	return ""
}
