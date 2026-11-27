package main

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Setting defaults. They match holyarrs-plugin.json; manifest_test checks both.
const (
	defThresholdEpisodes = 96
	defThresholdMovies   = 86
	defJobs              = 1
	defJobTimeoutMinutes = 180
	defTranscribe        = true
	defSync              = true
)

// Setting limits.
const (
	minThreshold, maxThreshold   = 0, 101 // 101: never skip by score
	minJobs, maxJobs             = 1, 4
	minTimeoutMin, maxTimeoutMin = 1, 1440
)

// Settings is what the owner set on the plugin's settings sheet.
type Settings struct {
	ASRURL            string // "" until the owner sets it
	ThresholdEpisodes int
	ThresholdMovies   int
	Jobs              int
	JobTimeoutMinutes int
	CheckTitles       []int64
	CheckTitlesRaw    string // the saved text, to run a list only once
	CheckOnly         bool
	Transcribe        bool // be a subtitle site that writes subtitles from the audio
	Sync              bool // line up saved subtitles
}

// readSettings reads HOLYARRS_PLUGIN_SETTING_<KEY> from getenv. A value that
// is missing, unreadable or out of range falls back to its default; each
// problem is returned as a line for the log.
func readSettings(getenv func(string) string) (Settings, []string) {
	var warn []string
	get := func(key string) string {
		return strings.TrimSpace(getenv("HOLYARRS_PLUGIN_SETTING_" + strings.ToUpper(key)))
	}
	num := func(key string, def, lo, hi int) int {
		v := get(key)
		if v == "" {
			return def
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < float64(lo) || f > float64(hi) {
			warn = append(warn, fmt.Sprintf("setting %s=%q is not a whole number from %d to %d; using %d", key, v, lo, hi, def))
			return def
		}
		return int(f)
	}
	s := Settings{
		ThresholdEpisodes: num("threshold_episodes", defThresholdEpisodes, minThreshold, maxThreshold),
		ThresholdMovies:   num("threshold_movies", defThresholdMovies, minThreshold, maxThreshold),
		Jobs:              num("jobs", defJobs, minJobs, maxJobs),
		JobTimeoutMinutes: num("job_timeout_minutes", defJobTimeoutMinutes, minTimeoutMin, maxTimeoutMin),
	}
	if u := strings.TrimRight(get("asr_url"), "/"); u != "" {
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil || p.RawQuery != "" || p.Fragment != "" {
			warn = append(warn, fmt.Sprintf("setting asr_url=%q is not an http or https address such as http://whisper.example.lan:9000", u))
		} else {
			s.ASRURL = u
		}
	}
	toggle := func(key string, def bool) bool {
		switch v := strings.ToLower(get(key)); v {
		case "":
			return def
		case "true":
			return true
		case "false":
			return false
		default:
			warn = append(warn, fmt.Sprintf("setting %s=%q is not true or false; using %t", key, v, def))
			return def
		}
	}
	s.CheckOnly = toggle("check_only", false)
	s.Transcribe = toggle("transcribe", defTranscribe)
	s.Sync = toggle("sync", defSync)
	s.CheckTitlesRaw = get("check_titles")
	ids, bad := parseTitleIDs(s.CheckTitlesRaw)
	s.CheckTitles = ids
	for _, b := range bad {
		warn = append(warn, fmt.Sprintf("setting check_titles: %q is not a title id; left out", b))
	}
	return s, warn
}

// parseTitleIDs reads "12, 34 ,12" as [12 34]: positive ids, each once, in
// order. Anything else is returned in bad.
func parseTitleIDs(s string) (ids []int64, bad []string) {
	seen := map[int64]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '\t' }) {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil || id <= 0 {
			bad = append(bad, f)
			continue
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, bad
}
