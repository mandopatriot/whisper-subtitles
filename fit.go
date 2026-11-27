package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"whisper-subtitles/media"
)

// Fit constants (plan section 4). Each is tested at its edge in fit_test.go.
const (
	fitLangProbMin       = 0.80   // a window counts when Whisper is this sure of the language
	fitMinLangWindows    = 4      // windows in the subtitle's language
	fitMinTokens         = 3      // shared tokens for an anchor
	fitMinJaccard        = 0.50   // token overlap for an anchor
	fitMinMargin         = 0.15   // over the best match at another start
	fitSearchMs          = 600000 // HolyArrs's ±10 minute limit
	fitMinAnchors        = 3      // anchors for a window to be usable
	fitInlierTolMs       = 600    // a window's offset this close to the median agrees
	fitMinUsable         = 4      // usable windows
	fitMinInliers        = 5      // agreeing windows
	fitMinInlierShare    = 0.75   // of the usable windows
	fitMinInliersFPS     = 6      // agreeing windows for a frame-rate change
	fitFPSAdvantage      = 2      // inliers a frame-rate change must beat no change by
	fitMinShiftWordsMs   = 300    // with word timings, smaller offsets (and no frame-rate change) are left alone; real files read up to 190 ms as is
	fitMinShiftPhrasesMs = 1000   // with phrase timings only: INTEL-WHISPER's were off by up to 0.5 s per file
	fitLeadTokens        = 3      // a cue's first tokens, found in the words to time its start
	fitTolMs             = 1000   // an anchor within this of the model fits it
	fitMinFitShare       = 0.60   // of all anchors, after the fix
	fitMinImprove        = 0.25   // fit share after minus before, for a frame-rate change
	fitMaxOffsetMs       = 600000 // HolyArrs's Sync limit
	fitMaxNegativeCues   = 2      // cues the fix may push before 0
)

// fpsRates are the frame rates tried, with exact values for the NTSC ones.
var fpsRates = []struct {
	label float64 // what is sent to HolyArrs
	exact float64
}{{23.976, 24000.0 / 1001}, {24, 24}, {25, 25}, {29.97, 30000.0 / 1001}, {30, 30}}

// WindowText is one window's transcript, placed in the video.
type WindowText struct {
	BaseMs int64 // video time of the upload's first block
	Tr     Transcript
	Err    error // the window failed; it does not count
}

// FitResult is the outcome. Sync is true only when every rule holds; Reason
// then or otherwise says why, with the numbers, for the log.
type FitResult struct {
	Sync     bool
	Timing   string // "words" or "phrases": what the anchors were timed by
	OffsetMs int64
	FromFPS  float64 // 0 when no frame-rate change
	ToFPS    float64
	Reason   string
	// Numbers behind the decision, for the log and the evaluation.
	LangWindows, Usable, Inliers, Anchors int
	SpreadMs                              int64
	FitAfter, FitBefore                   float64
}

func (r FitResult) String() string {
	fix := fmt.Sprintf("offset %+d ms", r.OffsetMs)
	if r.FromFPS != 0 {
		fix += fmt.Sprintf(", %g to %g fps", r.FromFPS, r.ToFPS)
	}
	verdict := "no sync"
	if r.Sync {
		verdict = "sync"
	}
	return fmt.Sprintf("%s (%s): %s; timing %s, windows in language %d, usable %d, agreeing %d, anchors %d, spread %d ms, fit %.2f (before %.2f)",
		verdict, fix, r.Reason, r.Timing, r.LangWindows, r.Usable, r.Inliers, r.Anchors, r.SpreadMs, r.FitAfter, r.FitBefore)
}

// tokens normalises text for matching: lower case, letters and digits,
// without hearing-impaired notes ([music], (sighs)) or speaker labels.
var (
	hiNotes      = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)|<[^>]*>|♪`)
	speakerLabel = regexp.MustCompile(`(^|[-–—]\s*|\s)[\p{Lu}][\p{Lu}\s.'-]*:`)
)

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range tokenList(s) {
		out[f] = true
	}
	return out
}

// tokenList is tokens in order, repeats kept.
func tokenList(s string) []string {
	s = hiNotes.ReplaceAllString(s, " ")
	s = speakerLabel.ReplaceAllString(s, " ")
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}) {
		if f = strings.Trim(f, "'"); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func overlap(a, b map[string]bool) (shared int, jaccard float64) {
	for t := range a {
		if b[t] {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0, 0
	}
	return shared, float64(shared) / float64(union)
}

// cueCand is one cue, or two neighbouring cues merged (Whisper often
// hears two lines as one segment).
type cueCand struct {
	first   int // index of the first cue, which gives the start
	startMs int64
	toks    map[string]bool
	lead    []string // the first cue's first tokens, to find its start among words
}

func candidates(cues []Cue) []cueCand {
	toks := make([]map[string]bool, len(cues))
	leads := make([][]string, len(cues))
	for i, c := range cues {
		toks[i] = tokens(c.Text)
		l := tokenList(c.Text)
		leads[i] = l[:min(len(l), fitLeadTokens)]
	}
	var out []cueCand
	for i, c := range cues {
		out = append(out, cueCand{i, c.StartMs, toks[i], leads[i]})
		if i+1 < len(cues) {
			m := map[string]bool{}
			for t := range toks[i] {
				m[t] = true
			}
			for t := range toks[i+1] {
				m[t] = true
			}
			out = append(out, cueCand{i, c.StartMs, m, leads[i]})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].startMs < out[b].startMs })
	return out
}

// anchor is a segment matched to a cue: d is segment start - k*cue start.
type anchor struct {
	window int
	d      float64
}

// anchorsFor matches every segment of the counting windows to the cues,
// for frame-rate ratio k. With words, the anchor is timed by the word where
// the matched cue begins (leadWord); otherwise by the segment's start.
func anchorsFor(cands []cueCand, wins []WindowText, counts []bool, k float64, words bool) []anchor {
	var out []anchor
	for wi, w := range wins {
		if !counts[wi] {
			continue
		}
		for _, s := range w.Tr.Segments {
			st := tokens(s.Text)
			if len(st) < fitMinTokens {
				continue
			}
			segMs := float64(w.BaseMs) + s.Start*1000
			// Cues whose scaled start is within the search range.
			lo := sort.Search(len(cands), func(i int) bool { return k*float64(cands[i].startMs) >= segMs-fitSearchMs })
			best, second := -1.0, -1.0
			var bestC cueCand
			var bestShared int
			for i := lo; i < len(cands) && k*float64(cands[i].startMs) <= segMs+fitSearchMs; i++ {
				sh, j := overlap(st, cands[i].toks)
				switch {
				case j > best:
					if best >= 0 && cands[i].first != bestC.first {
						second = max(second, best)
					}
					best, bestC, bestShared = j, cands[i], sh
				case cands[i].first != bestC.first && j > second:
					second = j
				}
			}
			if best < fitMinJaccard || bestShared < fitMinTokens || best-max(second, 0) < fitMinMargin {
				continue
			}
			at := segMs
			if words {
				wd, ok := leadWord(s.Words, bestC.lead)
				if !ok {
					continue
				}
				at = float64(w.BaseMs) + wd.Start*1000
			}
			out = append(out, anchor{wi, at - k*float64(bestC.startMs)})
		}
	}
	return out
}

// leadWord finds where a cue begins among a segment's words: the word that
// is the cue's first token and is followed by the most of its next tokens
// (the earliest such word on a tie). At least two of the lead tokens must
// follow in order, or all of them when the cue has fewer than two, so a
// common first word ("you") elsewhere in the segment is not taken.
func leadWord(words []Word, lead []string) (Word, bool) {
	if len(lead) == 0 {
		return Word{}, false
	}
	wt := make([]string, len(words))
	for i, w := range words {
		if l := tokenList(w.Word); len(l) > 0 {
			wt[i] = l[0]
		}
	}
	need := min(2, len(lead))
	bestAt, bestN := -1, 0
	for p := range wt {
		n := 0
		for n < len(lead) && p+n < len(wt) && wt[p+n] == lead[n] {
			n++
		}
		if n >= need && n > bestN {
			bestAt, bestN = p, n
		}
	}
	if bestAt < 0 {
		return Word{}, false
	}
	return words[bestAt], true
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// kFit is the consensus for one frame-rate ratio.
type kFit struct {
	k             float64
	from, to      float64
	anchors       []anchor
	usable        int
	inlierWins    map[int]bool
	b             float64
	spread        float64
	inlierAnchors int
}

func consensus(k, from, to float64, anchors []anchor, nWins int) kFit {
	f := kFit{k: k, from: from, to: to, anchors: anchors, inlierWins: map[int]bool{}}
	per := make([][]float64, nWins)
	for _, a := range anchors {
		per[a.window] = append(per[a.window], a.d)
	}
	bw := map[int]float64{}
	var bs []float64
	for w, ds := range per {
		if len(ds) >= fitMinAnchors {
			bw[w] = median(ds)
			bs = append(bs, bw[w])
		}
	}
	f.usable = len(bs)
	if f.usable == 0 {
		return f
	}
	bk := median(bs)
	lo, hi := math.Inf(1), math.Inf(-1)
	for w, b := range bw {
		if math.Abs(b-bk) <= fitInlierTolMs {
			f.inlierWins[w] = true
			lo, hi = min(lo, b), max(hi, b)
		}
	}
	var ds []float64
	for _, a := range anchors {
		if f.inlierWins[a.window] {
			ds = append(ds, a.d)
		}
	}
	f.inlierAnchors = len(ds)
	f.b = median(ds)
	if len(f.inlierWins) > 0 {
		f.spread = hi - lo
	}
	return f
}

// share is the part of anchors within fitTolMs of offset b.
func share(anchors []anchor, b float64) float64 {
	if len(anchors) == 0 {
		return 0
	}
	n := 0
	for _, a := range anchors {
		if math.Abs(a.d-b) <= fitTolMs {
			n++
		}
	}
	return float64(n) / float64(len(anchors))
}

// fitSubtitle decides whether and how to sync cues in subLang to the speech
// heard in wins. It never syncs unless every rule in plan section 4 holds.
func fitSubtitle(cues []Cue, subLang string, wins []WindowText) FitResult {
	var r FitResult
	want := media.BaseLanguage(subLang)
	counts := make([]bool, len(wins))
	var heard []string
	for i, w := range wins {
		if w.Err != nil {
			continue
		}
		heard = append(heard, fmt.Sprintf("%s %.2f", w.Tr.Language, w.Tr.Probability))
		if want != "" && media.BaseLanguage(w.Tr.Language) == want && w.Tr.Probability >= fitLangProbMin {
			counts[i] = true
			r.LangWindows++
		}
	}
	if r.LangWindows < fitMinLangWindows {
		r.Reason = fmt.Sprintf("only %d of %d windows are in the subtitle's language %s (heard %s); a subtitle in another language than the audio is not checked",
			r.LangWindows, len(wins), subLang, strings.Join(heard, ", "))
		return r
	}
	cands := candidates(cues)
	// Word timings are used when every counting window that heard speech
	// has them; otherwise all anchors are timed by phrases.
	words, heardAny := true, false
	for i, w := range wins {
		if counts[i] && len(w.Tr.Segments) > 0 {
			heardAny = true
			words = words && w.Tr.Words
		}
	}
	words = words && heardAny
	minShift := int64(fitMinShiftPhrasesMs)
	r.Timing = "phrases"
	if words {
		r.Timing, minShift = "words", fitMinShiftWordsMs
	}

	none := consensus(1, 0, 0, anchorsFor(cands, wins, counts, 1, words), len(wins))
	best := none
	for _, a := range fpsRates {
		for _, b := range fpsRates {
			if a.label == b.label {
				continue
			}
			k := a.exact / b.exact
			f := consensus(k, a.label, b.label, anchorsFor(cands, wins, counts, k, words), len(wins))
			better := len(f.inlierWins) > len(best.inlierWins) ||
				len(f.inlierWins) == len(best.inlierWins) && best.k != 1 && (f.inlierAnchors > best.inlierAnchors ||
					f.inlierAnchors == best.inlierAnchors && f.spread < best.spread)
			if better {
				best = f
			}
		}
	}
	if best.k != 1 && len(best.inlierWins) < len(none.inlierWins)+fitFPSAdvantage {
		best = none
	}

	r.Usable, r.Inliers, r.Anchors = best.usable, len(best.inlierWins), len(best.anchors)
	r.SpreadMs = int64(math.Round(best.spread))
	r.OffsetMs = int64(math.Round(best.b/10) * 10)
	if best.k != 1 {
		r.FromFPS, r.ToFPS = best.from, best.to
	}
	r.FitAfter = share(best.anchors, best.b)
	r.FitBefore = share(none.anchors, 0)

	fail := func(f string, a ...any) FitResult {
		r.Reason = fmt.Sprintf(f, a...)
		return r
	}
	switch {
	case r.Usable < fitMinUsable:
		return fail("only %d windows have %d or more matched lines (need %d)", r.Usable, fitMinAnchors, fitMinUsable)
	case r.Inliers < fitMinInliers || float64(r.Inliers) < fitMinInlierShare*float64(r.Usable):
		return fail("only %d of %d usable windows agree on the offset (need %d and %.0f%%)", r.Inliers, r.Usable, fitMinInliers, fitMinInlierShare*100)
	case best.k != 1 && r.Inliers < fitMinInliersFPS:
		return fail("a frame-rate change needs %d agreeing windows, has %d", fitMinInliersFPS, r.Inliers)
	case best.k == 1 && abs64(r.OffsetMs) < minShift:
		return fail("already in sync (offset %d ms is under %d ms with %s timings)", r.OffsetMs, minShift, r.Timing)
	case r.FitAfter < fitMinFitShare:
		return fail("only %.0f%% of matched lines fit the fix (need %.0f%%)", r.FitAfter*100, fitMinFitShare*100)
	case best.k != 1 && r.FitAfter-r.FitBefore < fitMinImprove:
		// At a 1 s tolerance a small offset already "fits" before the fix,
		// so this rule only judges frame-rate changes. An offset is backed by
		// the agreeing windows and the minimum shift instead.
		return fail("the frame-rate change improves the fit by only %.2f (need %.2f)", r.FitAfter-r.FitBefore, fitMinImprove)
	case abs64(r.OffsetMs) > fitMaxOffsetMs:
		return fail("offset %d ms is over HolyArrs's %d ms limit", r.OffsetMs, fitMaxOffsetMs)
	}
	neg := 0
	for _, c := range cues {
		if best.k*float64(c.StartMs)+float64(r.OffsetMs) < 0 {
			neg++
		}
	}
	if neg > fitMaxNegativeCues {
		return fail("the fix would move %d lines before the start (at most %d)", neg, fitMaxNegativeCues)
	}
	r.Sync = true
	r.Reason = "every rule holds"
	return r
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
