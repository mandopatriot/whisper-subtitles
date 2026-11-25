package media

import (
	"errors"
	"strings"
	"testing"
)

func TestBaseLanguage(t *testing.T) {
	for in, want := range map[string]string{
		"en": "en", "eng": "en", "ENG": "en", "en-US": "en", "pt-BR": "pt", "por": "pt", "pob": "pt",
		"es-419": "es", "es-ES": "es", "spa": "es", "ger": "de", "deu": "de", "fre": "fr", "fra": "fr",
		"zh-Hant": "zh", "chi": "zh", "zho": "zh", "iw": "he", "nob": "nb",
		"und": "", "zxx": "", "mul": "", "": "", "xx": "", "klingon": "",
	} {
		if got := BaseLanguage(in); got != want {
			t.Errorf("BaseLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func audio(num uint64, lang, codec string, ch uint64, bps int64, def bool, name string) Track {
	return Track{Number: num, Audio: true, Enabled: true, Language: lang, CodecID: codec, Channels: ch, BPS: bps, Default: def, Name: name}
}

func TestPickAudio(t *testing.T) {
	video := Track{Number: 0, Audio: false, Enabled: true, Language: "eng"}
	cases := []struct {
		name     string
		tracks   []Track
		sub      string
		want     uint64
		untagged bool
		err      string // part of the error; "" for a pick
	}{
		// The test library's own layouts.
		{"2012: main DTS, not the cheaper commentary", []Track{video,
			audio(1, "eng", "A_DTS", 6, 1_536_000, true, "Main Audio DTS 5.1 @ 1.5 mbps"),
			audio(2, "eng", "A_VORBIS", 2, 0, false, "Commentary Vorbis 2.0")}, "en", 1, false, ""},
		{"Cars: main AAC 5.1, not the two AAC 2.0 commentaries", []Track{video,
			audio(1, "eng", "A_AAC", 6, 0, true, ""),
			audio(2, "eng", "A_AAC", 2, 0, false, "Commentary 1"),
			audio(3, "eng", "A_AAC", 2, 0, false, "Commentary 2")}, "en", 1, false, ""},
		{"Apocalypse Z: English subtitle takes the English track", []Track{video,
			audio(1, "spa", "A_EAC3", 6, 576_000, true, ""),
			audio(2, "eng", "A_EAC3", 6, 640_000, false, "")}, "en", 2, false, ""},
		{"Apocalypse Z: Spanish subtitle takes the Spanish default", []Track{video,
			audio(1, "es-ES", "A_EAC3", 6, 576_000, true, ""),
			audio(2, "eng", "A_EAC3", 6, 640_000, false, "")}, "es-419", 1, false, ""},
		// Smallest of the matching dialogue tracks.
		{"stated bitrates: AC-3 over TrueHD", []Track{
			audio(1, "eng", "A_TRUEHD", 8, 4_500_000, true, ""),
			audio(2, "eng", "A_AC3", 6, 640_000, false, "")}, "en", 2, false, ""},
		{"no bitrates: codec order", []Track{
			audio(1, "eng", "A_DTS/LOSSLESS", 6, 0, true, ""),
			audio(2, "eng", "A_EAC3", 6, 0, false, ""),
			audio(3, "eng", "A_AC3", 6, 0, false, "")}, "en", 3, false, ""},
		{"one bitrate missing: codec order, not a mixed compare", []Track{
			audio(1, "eng", "A_AC3", 6, 0, true, ""),
			audio(2, "eng", "A_AAC", 2, 128_000, false, "")}, "en", 2, false, ""},
		{"same codec: fewer channels, then default", []Track{
			audio(1, "eng", "A_AAC", 6, 0, false, ""),
			audio(2, "eng", "A_AAC", 2, 0, false, ""),
			audio(3, "eng", "A_AAC", 2, 0, true, "")}, "en", 3, false, ""},
		{"pt-BR subtitle matches por", []Track{audio(1, "por", "A_AC3", 6, 0, true, "")}, "pt-BR", 1, false, ""},
		// Commentary and description by flag, and by name in other languages.
		{"commentary flag", []Track{
			func() Track { t := audio(1, "eng", "A_AAC", 2, 0, true, "Director"); t.Commentary = true; return t }(),
			audio(2, "eng", "A_DTS", 6, 0, false, "")}, "en", 2, false, ""},
		{"visually impaired flag", []Track{
			func() Track { t := audio(1, "eng", "A_AAC", 2, 0, true, ""); t.Described = true; return t }(),
			audio(2, "eng", "A_AC3", 6, 0, false, "")}, "en", 2, false, ""},
		{"Kommentar and audio description by name", []Track{
			audio(1, "ger", "A_AAC", 2, 0, true, "Kommentar des Regisseurs"),
			audio(2, "ger", "A_AAC", 2, 0, false, "Audiodeskription"),
			audio(3, "ger", "A_AC3", 6, 0, false, "Deutsch DD 5.1")}, "de", 3, false, ""},
		// Untagged tracks, and disabled ones.
		{"untagged when no track matches", []Track{
			audio(1, "und", "A_AC3", 6, 0, false, ""),
			audio(2, "", "A_AAC", 2, 0, true, "")}, "en", 2, true, ""},
		{"a tagged match beats an untagged track", []Track{
			audio(1, "und", "A_AAC", 2, 0, true, ""),
			audio(2, "eng", "A_DTS", 6, 0, false, "")}, "en", 2, false, ""},
		{"disabled track is never picked", []Track{
			func() Track { t := audio(1, "eng", "A_AAC", 2, 0, true, ""); t.Enabled = false; return t }(),
			audio(2, "eng", "A_DTS", 6, 0, false, "")}, "en", 2, false, ""},
		// Skips.
		{"only other languages", []Track{
			audio(1, "spa", "A_EAC3", 6, 0, true, ""),
			audio(2, "fre", "A_AC3", 6, 0, false, "")}, "en", 0, false, "the subtitle is en, the audio is spa, fre"},
		{"only a commentary in the language", []Track{
			audio(1, "spa", "A_EAC3", 6, 0, true, ""),
			audio(2, "eng", "A_AAC", 2, 0, false, "Commentary")}, "en", 0, false, "eng (commentary or description)"},
		{"untagged commentary is not dialogue", []Track{audio(1, "und", "A_AAC", 2, 0, true, "Commentary")}, "en", 0, false, "commentary"},
		{"no audio", []Track{video}, "en", 0, false, "no audio track"},
		{"unknown subtitle language", []Track{audio(1, "eng", "A_AAC", 2, 0, true, "")}, "xx", 0, false, "unknown"},
	}
	for _, c := range cases {
		got, err := PickAudio(c.tracks, c.sub)
		if c.err != "" {
			if !errors.Is(err, ErrNoTrack) || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: %+v, %v; want ErrNoTrack naming %q", c.name, got, err, c.err)
			}
			continue
		}
		if err != nil || got.Track.Number != c.want || got.Untagged != c.untagged {
			t.Errorf("%s: track %d untagged %t, %v; want track %d untagged %t", c.name, got.Track.Number, got.Untagged, err, c.want, c.untagged)
		}
	}
}
