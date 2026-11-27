package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// evalPiece is one upload of a cached comparison run: it owns [Start, End)
// and its audio starts at Base.
type evalPiece struct{ Start, End, Base int64 }

func evalPlan(pc int64) []evalPiece {
	var ps []evalPiece
	for s := int64(0); s < evalMs; s += pc {
		e := s + pc
		if e >= evalMs {
			e = 1 << 62
		}
		ps = append(ps, evalPiece{s, e, max(0, s-chunkPadMs)})
	}
	return ps
}

func ratio(text string, gz bool) float64 {
	var b bytes.Buffer
	if gz {
		w := gzip.NewWriter(&b)
		w.Write([]byte(text))
		w.Close()
	} else {
		w := zlib.NewWriter(&b)
		w.Write([]byte(text))
		w.Close()
	}
	return float64(len(text)) / float64(b.Len())
}

// TestEdgeFillersCached measures the edge-filler rule and the compression
// ratio of each piece on the runs TestPieceLengths cached in
// $WHISPER_SUBTITLES_PIECES_OUT. No server is called.
func TestEdgeFillersCached(t *testing.T) {
	out := os.Getenv("WHISPER_SUBTITLES_PIECES_OUT")
	if out == "" {
		t.Skip("set WHISPER_SUBTITLES_PIECES_OUT")
	}
	refs, _ := filepath.Glob(filepath.Join(out, "*.ref.json"))
	var rep strings.Builder
	var drops, ratios, scores strings.Builder
	fmt.Fprintf(&ratios, "| Film | Piece | Piece owns | zlib ratio | gzip ratio | Text |\n|---|---|---|---|---|---|\n")
	fmt.Fprintf(&scores, "| Film | Piece | Lines (ref) | Missed | False cues | Starts measured | Median | Worst | Dropped |\n|---|---|---|---|---|---|---|---|---|\n")
	total := map[int64]int{}
	for _, rf := range refs {
		name := strings.TrimSuffix(filepath.Base(rf), ".ref.json")
		var ref []tSeg
		b, err := os.ReadFile(rf)
		if err != nil || json.Unmarshal(b, &ref) != nil {
			t.Fatalf("%s: %v", rf, err)
		}
		for _, pc := range evalPieces {
			var run evalRun
			b, err := os.ReadFile(filepath.Join(out, fmt.Sprintf("%s.%ds.json", name, pc/1000)))
			if err != nil || json.Unmarshal(b, &run) != nil || run.SRT == "" {
				t.Fatalf("%s %d: %v", name, pc, err)
			}
			cues, err := readCues([]byte(run.SRT), "srt")
			if err != nil {
				t.Fatal(err)
			}
			plan := evalPlan(pc)
			texts := make([][]string, len(plan))
			var kept []Cue
			nDrop := 0
			for _, c := range cues {
				pi := 0
				for i, p := range plan {
					if c.StartMs >= p.Start {
						pi = i
					}
				}
				p := plan[pi]
				texts[pi] = append(texts[pi], strings.ReplaceAll(c.Text, "\n", " "))
				rel := c.StartMs - p.Base
				if c.StartMs < evalMs && fillerLine(c.Text) && atEdge(rel) {
					nDrop++
					var said []string
					for _, s := range ref {
						for _, w := range s.Words {
							if w.StartMs >= c.StartMs-3000 && w.StartMs <= c.EndMs+3000 {
								said = append(said, fmt.Sprintf("%s%s", stamp(w.StartMs), w.Text))
							}
						}
					}
					fmt.Fprintf(&drops, "| %s | %s | %s | %q | %.2f s | %s |\n", name, pieceName(pc), stamp(c.StartMs),
						strings.ReplaceAll(c.Text, "\n", " / "), float64(rel)/1000, list(said))
					continue
				}
				kept = append(kept, c)
			}
			total[pc] += nDrop
			for i, p := range plan {
				txt := strings.Join(texts[i], " ")
				if txt == "" {
					continue
				}
				short := txt
				if r := []rune(short); len(r) > 90 {
					short = string(r[:90]) + "…"
				}
				fmt.Fprintf(&ratios, "| %s | %s | %s–%s | %.2f | %.2f | %s |\n", name, pieceName(pc), stamp(p.Start), stamp(min(p.End, evalMs)),
					ratio(txt, false), ratio(txt, true), strings.ReplaceAll(short, "|", "/"))
			}
			if pc == 90_000 {
				s := score(ref, kept)
				fmt.Fprintf(&scores, "| %s | %s | %d | %d | %d | %d | %.2f s | %.2f s | %d |\n", name, pieceName(pc), s.lines, len(s.missed), len(s.falses),
					s.measured, s.median/1000, s.worst/1000, nDrop)
				fmt.Fprintf(&rep, "\n### %s, 90 s with the rule\n\nFalse cues: %s\n", name, list(s.falses))
			}
		}
	}
	all := "## Would drop\n\n| Film | Piece | Cue | Text | Into upload | Parakeet words within 3 s |\n|---|---|---|---|---|---|\n" + drops.String() +
		fmt.Sprintf("\nTotals: %v\n\n## 90 s rescored\n\n", total) + scores.String() + rep.String() + "\n## Compression ratios\n\n" + ratios.String()
	if err := os.WriteFile(filepath.Join(out, "edges.md"), []byte(all), 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Println(all)
}
