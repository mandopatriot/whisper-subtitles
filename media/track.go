package media

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// languages maps ISO 639-1 codes to their ISO 639-2 bibliographic and
// terminology codes. It is HolyArrs's own table (internal/subtitles/lang.go,
// AGPL-3.0), so a track's language is read as HolyArrs reads a subtitle's.
var languages = [][3]string{
	{"af", "afr", "afr"}, {"ar", "ara", "ara"}, {"bg", "bul", "bul"}, {"bn", "ben", "ben"}, {"bs", "bos", "bos"},
	{"ca", "cat", "cat"}, {"cs", "cze", "ces"}, {"cy", "wel", "cym"}, {"da", "dan", "dan"}, {"de", "ger", "deu"},
	{"el", "gre", "ell"}, {"en", "eng", "eng"}, {"eo", "epo", "epo"}, {"es", "spa", "spa"}, {"et", "est", "est"},
	{"eu", "baq", "eus"}, {"fa", "per", "fas"}, {"fi", "fin", "fin"}, {"fr", "fre", "fra"}, {"ga", "gle", "gle"},
	{"gl", "glg", "glg"}, {"he", "heb", "heb"}, {"hi", "hin", "hin"}, {"hr", "hrv", "hrv"}, {"hu", "hun", "hun"},
	{"hy", "arm", "hye"}, {"id", "ind", "ind"}, {"is", "ice", "isl"}, {"it", "ita", "ita"}, {"ja", "jpn", "jpn"},
	{"ka", "geo", "kat"}, {"kk", "kaz", "kaz"}, {"km", "khm", "khm"}, {"ko", "kor", "kor"}, {"ku", "kur", "kur"},
	{"lt", "lit", "lit"}, {"lv", "lav", "lav"}, {"mk", "mac", "mkd"}, {"ml", "mal", "mal"}, {"mn", "mon", "mon"},
	{"ms", "may", "msa"}, {"my", "bur", "mya"}, {"nb", "nob", "nob"}, {"ne", "nep", "nep"}, {"nl", "dut", "nld"},
	{"nn", "nno", "nno"}, {"no", "nor", "nor"}, {"pa", "pan", "pan"}, {"pl", "pol", "pol"}, {"pt", "por", "por"},
	{"pt", "pob", "pob"}, {"ro", "rum", "ron"}, {"ru", "rus", "rus"}, {"si", "sin", "sin"}, {"sk", "slo", "slk"},
	{"sl", "slv", "slv"}, {"sq", "alb", "sqi"}, {"sr", "srp", "srp"}, {"sv", "swe", "swe"}, {"sw", "swa", "swa"},
	{"ta", "tam", "tam"}, {"te", "tel", "tel"}, {"th", "tha", "tha"}, {"tl", "tgl", "tgl"}, {"tr", "tur", "tur"},
	{"uk", "ukr", "ukr"}, {"ur", "urd", "urd"}, {"uz", "uzb", "uzb"}, {"vi", "vie", "vie"}, {"zh", "chi", "zho"},
	{"zh", "chs", "chs"}, {"zh", "cht", "cht"}, {"zh", "yue", "yue"},
}

var baseOfCode = func() map[string]string {
	m := map[string]string{}
	for _, l := range languages {
		m[l[0]], m[l[1]], m[l[2]] = l[0], l[0], l[0]
	}
	m["iw"], m["in"] = "he", "id" // old codes
	return m
}()

// BaseLanguage reads a language code (en, eng, pt-BR, es-419, zh-Hant) as
// its two-letter base: en, en, pt, es, zh. It returns "" for unknown codes
// and for und, zxx, mul and mis, which name no language.
func BaseLanguage(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if b, ok := baseOfCode[code]; ok {
		return b
	}
	if i := strings.IndexAny(code, "-_"); i > 0 {
		return baseOfCode[code[:i]]
	}
	return ""
}

// notDialogue are words in a track name that mark speech other than the
// film's dialogue: commentaries and audio description.
var notDialogue = []string{"comment", "comentario", "commento", "kommentar", "description", "descriptive", "described", "audiodeskription"}

// isDialogue reports whether a track carries the film's own dialogue.
func (t Track) isDialogue() bool {
	if t.Commentary || t.Described {
		return false
	}
	n := strings.ToLower(t.Name)
	for _, w := range notDialogue {
		if strings.Contains(n, w) {
			return false
		}
	}
	return true
}

// codecRank orders codecs by their usual bitrate, smallest first, for
// tracks whose bitrate the file does not state.
func codecRank(id string) int {
	switch {
	case strings.HasPrefix(id, "A_OPUS"), strings.HasPrefix(id, "A_VORBIS"):
		return 1
	case strings.HasPrefix(id, "A_AAC"), strings.HasPrefix(id, "A_MPEG"):
		return 2
	case id == "A_AC3":
		return 3
	case id == "A_EAC3":
		return 4
	case id == "A_DTS" || id == "A_DTS/EXPRESS":
		return 5
	case strings.HasPrefix(id, "A_FLAC"), strings.HasPrefix(id, "A_ALAC"):
		return 6
	case strings.HasPrefix(id, "A_TRUEHD"), strings.HasPrefix(id, "A_MLP"), strings.HasPrefix(id, "A_DTS"):
		return 7
	case strings.HasPrefix(id, "A_PCM"):
		return 8
	}
	return 9
}

// ErrNoTrack means no audio track carries dialogue in the subtitle's
// language, or could (an untagged track).
var ErrNoTrack = errors.New("no audio track in the subtitle's language")

// Choice is the audio track picked for a subtitle.
type Choice struct {
	Track Track
	// Untagged is true when no track was tagged with the subtitle's language
	// and an untagged one was taken: Whisper's detected language decides.
	Untagged bool
}

// PickAudio picks the audio track to compare a subtitle in subLang with:
//   - enabled audio tracks carrying dialogue (no commentary or audio
//     description) whose language has subLang's base (pt-BR matches por);
//     of those, the smallest: by stated bitrate when every one states it,
//     else by codec, then fewer channels; default first, then file order;
//   - else such an untagged track (und or empty), the default one first;
//   - else ErrNoTrack, naming the languages the file has.
func PickAudio(tracks []Track, subLang string) (Choice, error) {
	want := BaseLanguage(subLang)
	if want == "" {
		return Choice{}, fmt.Errorf("%w: the subtitle's language %q is unknown", ErrNoTrack, subLang)
	}
	var match, untagged []Track
	var have []string
	for _, t := range tracks {
		if !t.Audio || !t.Enabled {
			continue
		}
		label := t.Language
		if !t.isDialogue() {
			label += " (commentary or description)"
		}
		have = append(have, label)
		if !t.isDialogue() {
			continue
		}
		switch BaseLanguage(t.Language) {
		case want:
			match = append(match, t)
		case "":
			untagged = append(untagged, t)
		}
	}
	if len(match) > 0 {
		allBPS := true
		for _, t := range match {
			allBPS = allBPS && t.BPS > 0
		}
		sort.SliceStable(match, func(i, j int) bool {
			a, b := match[i], match[j]
			if allBPS && a.BPS != b.BPS {
				return a.BPS < b.BPS
			}
			if !allBPS {
				if ra, rb := codecRank(a.CodecID), codecRank(b.CodecID); ra != rb {
					return ra < rb
				}
				if a.Channels != b.Channels {
					return a.Channels < b.Channels
				}
			}
			return a.Default && !b.Default
		})
		return Choice{Track: match[0]}, nil
	}
	if len(untagged) > 0 {
		sort.SliceStable(untagged, func(i, j int) bool { return untagged[i].Default && !untagged[j].Default })
		return Choice{Track: untagged[0], Untagged: true}, nil
	}
	if len(have) == 0 {
		return Choice{}, fmt.Errorf("%w: the file has no audio track", ErrNoTrack)
	}
	return Choice{}, fmt.Errorf("%w: the subtitle is %s, the audio is %s", ErrNoTrack, subLang, strings.Join(have, ", "))
}
