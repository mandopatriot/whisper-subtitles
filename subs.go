package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Limits on a subtitle read for its timing. HolyArrs saves at most 5 MB
// (subtext.MaxText).
const (
	maxSubBytes = 5 << 20
	maxCues     = 100_000
)

// Cue is one subtitle line: its times and plain text.
type Cue struct {
	StartMs, EndMs int64
	Text           string
}

var errNoCues = errors.New("no timed lines found")

// readCues reads SRT, VTT, ASS or SSA text (UTF-8, as HolyArrs saves it)
// into cues in file order.
func readCues(b []byte, format string) ([]Cue, error) {
	if len(b) > maxSubBytes {
		return nil, fmt.Errorf("subtitle is %d bytes, over %d", len(b), maxSubBytes)
	}
	b = bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))
	var cues []Cue
	var err error
	switch format {
	case "srt", "vtt":
		cues, err = readSRT(b)
	case "ass", "ssa":
		cues, err = readASS(b)
	default:
		return nil, fmt.Errorf("can't read .%s timing", format)
	}
	if err == nil && len(cues) == 0 {
		err = errNoCues
	}
	return cues, err
}

// srtTime reads 00:01:02,345, 00:01:02.345 or (VTT) 01:02.345.
var srtTime = regexp.MustCompile(`^(?:(\d{1,3}):)?(\d{1,2}):(\d{1,2})[,.](\d{1,3})$`)

func parseSRTTime(s string) (int64, bool) {
	m := srtTime.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	var h int64
	if m[1] != "" { // VTT may leave the hours out
		h, _ = strconv.ParseInt(m[1], 10, 64)
	}
	mi, _ := strconv.ParseInt(m[2], 10, 64)
	sec, _ := strconv.ParseInt(m[3], 10, 64)
	frac := m[4]
	for len(frac) < 3 {
		frac += "0"
	}
	ms, _ := strconv.ParseInt(frac, 10, 64)
	if mi > 59 || sec > 59 {
		return 0, false
	}
	return ((h*60+mi)*60+sec)*1000 + ms, true
}

func readSRT(b []byte) ([]Cue, error) {
	var cues []Cue
	var cur *Cue
	var text []string
	flush := func() {
		if cur != nil {
			cur.Text = strings.Join(text, " ")
			cues = append(cues, *cur)
		}
		cur, text = nil, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxSubBytes)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if a, rest, ok := strings.Cut(line, "-->"); ok {
			endField := strings.Fields(rest)
			if len(endField) == 0 {
				continue
			}
			st, ok1 := parseSRTTime(a)
			en, ok2 := parseSRTTime(endField[0])
			if !ok1 || !ok2 {
				continue
			}
			flush()
			if len(cues) >= maxCues {
				return nil, fmt.Errorf("over %d cues", maxCues)
			}
			cur = &Cue{StartMs: st, EndMs: en}
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if cur != nil {
			text = append(text, strings.TrimSpace(line))
		}
	}
	flush()
	return cues, sc.Err()
}

// assTime reads 0:01:02.34.
func parseASSTime(s string) (int64, bool) {
	p := strings.Split(strings.TrimSpace(s), ":")
	if len(p) != 3 {
		return 0, false
	}
	h, e1 := strconv.ParseInt(p[0], 10, 64)
	m, e2 := strconv.ParseInt(p[1], 10, 64)
	sf, e3 := strconv.ParseFloat(p[2], 64)
	if e1 != nil || e2 != nil || e3 != nil || h < 0 || m < 0 || m > 59 || sf < 0 || sf >= 60 {
		return 0, false
	}
	return (h*60+m)*60_000 + int64(sf*1000+0.5), true
}

var assOverride = regexp.MustCompile(`\{[^}]*\}`)

func readASS(b []byte) ([]Cue, error) {
	var cues []Cue
	start, end, text, nf := 1, 2, 9, 10 // the standard Format order
	inEvents := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxSubBytes)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimRight(sc.Text(), "\r"))
		if strings.HasPrefix(line, "[") {
			inEvents = strings.EqualFold(line, "[Events]")
			continue
		}
		if !inEvents {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "format":
			fields := strings.Split(val, ",")
			nf = len(fields)
			for i, f := range fields {
				switch strings.ToLower(strings.TrimSpace(f)) {
				case "start":
					start = i
				case "end":
					end = i
				case "text":
					text = i
				}
			}
		case "dialogue":
			f := strings.SplitN(val, ",", nf)
			if len(f) < nf || text != nf-1 {
				continue
			}
			st, ok1 := parseASSTime(f[start])
			en, ok2 := parseASSTime(f[end])
			if !ok1 || !ok2 {
				continue
			}
			if len(cues) >= maxCues {
				return nil, fmt.Errorf("over %d cues", maxCues)
			}
			t := assOverride.ReplaceAllString(f[text], "")
			t = strings.NewReplacer(`\N`, " ", `\n`, " ", `\h`, " ").Replace(t)
			cues = append(cues, Cue{StartMs: st, EndMs: en, Text: strings.TrimSpace(t)})
		}
	}
	return cues, sc.Err()
}
