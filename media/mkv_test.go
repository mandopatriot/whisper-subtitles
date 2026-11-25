package media

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// srcOpts shapes a synthetic source file.
type srcOpts struct {
	noCues          bool
	unknownClusters bool // clusters of unknown size, as from a live recorder
	unknownSegment  bool
	clusterMs       int64 // cluster length
	lengthMs        int64
	tags            bool
}

// srcBlock is a block as stored in the source.
type srcBlock struct {
	track uint64
	ms    int64
	flags byte
	group bool // stored in a BlockGroup
}

func blockData(track uint64, ms int64) []byte {
	return []byte(fmt.Sprintf("track %d at %d ms;", track, ms))
}

func simpleBlock(id uint32, track uint64, rel int64, flags byte, data []byte) []byte {
	p := ebmlSize(nil, track)
	p = append(p, byte(rel>>8), byte(rel), flags)
	return elemBytes(nil, id, append(p, data...))
}

// buildMKV makes a file with a video track 1 (a frame every 40 ms), an
// English AAC track 2 (every 21 ms; every 50th block in a BlockGroup, every
// 7th laced) and a Spanish AC-3 track 3 (every 32 ms). It returns the file
// and the blocks of track 2.
func buildMKV(o srcOpts) ([]byte, []srcBlock) {
	if o.clusterMs == 0 {
		o.clusterMs = 5000
	}
	if o.lengthMs == 0 {
		o.lengthMs = 60_000
	}
	var hdr []byte
	hdr = elemBytes(hdr, idEBML, elemBytes(nil, idDocType, []byte("matroska")))

	var info []byte
	info = elemUint(info, idTimescale, 1_000_000)
	info = elemFloat(info, idDuration, float64(o.lengthMs))
	track := func(num, uid, typ uint64, codec, lang string, priv []byte, enc []byte) []byte {
		var t []byte
		t = elemUint(t, idTrackNumber, num)
		t = elemUint(t, idTrackUID, uid)
		t = elemUint(t, idTrackType, typ)
		t = elemBytes(t, idCodecID, []byte(codec))
		if lang != "" {
			t = elemBytes(t, idLanguage, []byte(lang))
		}
		if priv != nil {
			t = elemBytes(t, idCodecPriv, priv)
		}
		if enc != nil {
			t = elemBytes(t, idEncodings, enc)
		}
		if typ == 2 {
			var a []byte
			a = elemFloat(a, idSampleRate, 48000)
			a = elemUint(a, idChannels, 6)
			t = elemBytes(t, idAudio, a)
		}
		return elemBytes(nil, idTrackEntry, t)
	}
	var tracks []byte
	tracks = append(tracks, track(1, 101, 1, "V_MPEGH/ISO/HEVC", "und", nil, nil)...)
	tracks = append(tracks, track(2, 102, 2, "A_AAC", "eng", []byte{0x11, 0xB0}, []byte{0x62, 0x40, 0x81, 0x00})...)
	tracks = append(tracks, track(3, 103, 2, "A_AC3", "spa", nil, nil)...)

	// Clusters with every block in time order.
	var all []srcBlock
	for ms := int64(0); ms < o.lengthMs; ms += 40 {
		all = append(all, srcBlock{track: 1, ms: ms, flags: 0x80})
	}
	n := 0
	for ms := int64(0); ms < o.lengthMs; ms += 21 {
		b := srcBlock{track: 2, ms: ms, flags: 0x80}
		if n%7 == 3 {
			b.flags |= 0x02 // Xiph lacing bit, kept as stored
		}
		b.group = n%50 == 10
		all = append(all, b)
		n++
	}
	for ms := int64(0); ms < o.lengthMs; ms += 32 {
		all = append(all, srcBlock{track: 3, ms: ms, flags: 0x80})
	}
	sortBlocks(all)

	type clusterPos struct{ ms, rel int64 }
	var clusters [][]byte
	var cms []int64
	for start := int64(0); start < o.lengthMs; start += o.clusterMs {
		var body []byte
		body = elemUint(body, idTimestamp, uint64(start))
		for _, b := range all {
			if b.ms < start || b.ms >= start+o.clusterMs {
				continue
			}
			sb := simpleBlock(idSimpleBlock, b.track, b.ms-start, b.flags, blockData(b.track, b.ms))
			if b.group {
				g := simpleBlock(idBlock, b.track, b.ms-start, b.flags&0x06, blockData(b.track, b.ms))
				g = elemUint(g, 0x9B, 21) // BlockDuration
				sb = elemBytes(nil, idBlockGroup, g)
			}
			body = append(body, sb...)
		}
		var c []byte
		if o.unknownClusters {
			c = ebmlID(nil, idCluster)
			c = append(c, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
			c = append(c, body...)
		} else {
			c = elemBytes(nil, idCluster, body)
		}
		clusters = append(clusters, c)
		cms = append(cms, start)
	}

	// Segment payload: SeekHead (fixed size, filled in last), Info, Tracks,
	// clusters, Cues, Tags.
	seekLen := len(seekHead(0, 0))
	var seg []byte
	seg = append(seg, make([]byte, seekLen)...)
	seg = elemBytes(seg, idInfo, info)
	seg = elemBytes(seg, idTracks, tracks)
	var pos []clusterPos
	for i, c := range clusters {
		pos = append(pos, clusterPos{cms[i], int64(len(seg))})
		seg = append(seg, c...)
	}
	cuesRel := int64(len(seg))
	if !o.noCues {
		var cues []byte
		for _, p := range pos {
			var cp []byte
			cp = elemUint(cp, idCueTime, uint64(p.ms))
			cp = elemBytes(cp, idCuePos, append(elemUint(nil, 0xF7, 2), elemUint(nil, idCueCluster, uint64(p.rel))...))
			cues = elemBytes(cues, idCuePoint, cp)
		}
		seg = elemBytes(seg, idCues, cues)
	}
	tagsRel := int64(len(seg))
	if o.tags {
		tag := func(uid uint64, bps string) []byte {
			var t []byte
			t = elemBytes(t, idTargets, elemUint(nil, idTagTrackUID, uid))
			st := append(elemBytes(nil, idTagName, []byte("BPS")), elemBytes(nil, idTagString, []byte(bps))...)
			t = elemBytes(t, idSimpleTag, st)
			return elemBytes(nil, idTag, t)
		}
		seg = elemBytes(seg, idTags, append(tag(102, "192000"), tag(103, "640000")...))
	}
	if !o.tags {
		tagsRel = -1
	}
	if o.noCues {
		cuesRel = -1
	}
	sh := seekHead(cuesRel, tagsRel)
	copy(seg, sh)

	out := append([]byte(nil), hdr...)
	if o.unknownSegment {
		out = ebmlID(out, idSegment)
		out = append(out, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
		out = append(out, seg...)
	} else {
		out = elemBytes(out, idSegment, seg)
	}
	var want []srcBlock
	for _, b := range all {
		if b.track == 2 {
			want = append(want, b)
		}
	}
	return out, want
}

// seekHead has a fixed size so it can be filled in after the layout is
// known. A negative position leaves a Void entry in its place.
func seekHead(cues, tags int64) []byte {
	var entry func(id uint32, pos int64) []byte
	entry = func(id uint32, pos int64) []byte {
		if pos < 0 { // a Void of exactly the same length as the entry it stands for
			return elemBytes(nil, 0xEC, make([]byte, len(entry(id, 0))-2))
		}
		var p [8]byte
		for i := range 8 {
			p[i] = byte(pos >> (56 - 8*i))
		}
		s := elemBytes(nil, idSeekID, ebmlID(nil, id))
		s = append(s, elemBytes(nil, idSeekPos, p[:])...)
		return elemBytes(nil, idSeek, s)
	}
	return elemBytes(nil, idSeekHead, append(entry(idCues, cues), entry(idTags, tags)...))
}

func sortBlocks(b []srcBlock) {
	for i := 1; i < len(b); i++ { // stable insertion sort by time
		for j := i; j > 0 && b[j].ms < b[j-1].ms; j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}

// outBlock is a block read back from an output .mka.
type outBlock struct {
	ms    int64
	flags byte
	data  string
}

// readBack opens an output .mka and lists its blocks.
func readBack(t *testing.T, b []byte) (*MKV, []outBlock) {
	t.Helper()
	m, err := OpenMKV(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("output does not open: %v", err)
	}
	var out []outBlock
	pos := m.firstCluster
	for pos >= 0 && pos < int64(len(b)) {
		c, err := m.f.header(pos, int64(len(b)))
		if err != nil || c.ID != idCluster || c.Size == unknownSize {
			t.Fatalf("output cluster at %d: %+v %v", pos, c, err)
		}
		var cms int64
		m.clusterBlocks(c, func(e elem) (bool, error) {
			switch e.ID {
			case idTimestamp:
				v, _ := m.f.uint(e)
				cms = int64(v)
			case idSimpleBlock:
				num, rel, flags, off, err := m.blockHeader(e)
				if err != nil || num != 1 {
					t.Fatalf("output block %+v: track %d, %v", e, num, err)
				}
				d := make([]byte, e.End()-off)
				m.f.readFull(off, d)
				out = append(out, outBlock{cms + int64(rel), flags, string(d)})
			default:
				t.Fatalf("unexpected element %#x in output cluster", e.ID)
			}
			return true, nil
		})
		pos = c.End()
	}
	return m, out
}

func TestOpenMKV(t *testing.T) {
	b, _ := buildMKV(srcOpts{tags: true})
	m, err := OpenMKV(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if m.DurationMs != 60_000 || len(m.Tracks) != 3 || len(m.cues) != 12 || m.ownIndex {
		t.Fatalf("duration %d, %d tracks, %d cues, own index %t", m.DurationMs, len(m.Tracks), len(m.cues), m.ownIndex)
	}
	a, s := m.Tracks[1], m.Tracks[2]
	if !a.Audio || a.Number != 2 || a.CodecID != "A_AAC" || a.Language != "eng" || a.BPS != 192000 ||
		!bytes.Equal(a.CodecPrivate, []byte{0x11, 0xB0}) || a.SampleRate != 48000 || a.Channels != 6 || len(a.Encodings) != 4 {
		t.Errorf("track 2 = %+v", a)
	}
	if !s.Audio || s.Language != "spa" || s.BPS != 640000 || m.Tracks[0].Audio {
		t.Errorf("track 3 = %+v; track 1 audio %t", s, m.Tracks[0].Audio)
	}
}

func TestOpenMKVLanguageDefault(t *testing.T) {
	// Matroska: a track without Language is English; LanguageIETF wins.
	var te []byte
	te = elemUint(te, idTrackNumber, 1)
	te = elemUint(te, idTrackType, 2)
	f := minimalMKV(elemBytes(nil, idTrackEntry, te))
	m, err := OpenMKV(bytes.NewReader(f), int64(len(f)))
	if err != nil || m.Tracks[0].Language != "eng" {
		t.Fatalf("no Language: %+v, %v", m, err)
	}
	te = elemBytes(te, idLanguage, []byte("por"))
	te = elemBytes(te, idLangIETF, []byte("pt-BR"))
	f = minimalMKV(elemBytes(nil, idTrackEntry, te))
	m, err = OpenMKV(bytes.NewReader(f), int64(len(f)))
	if err != nil || m.Tracks[0].Language != "pt-BR" {
		t.Fatalf("LanguageIETF: %+v, %v", m, err)
	}
}

// minimalMKV wraps track entries in a file with one empty cluster.
func minimalMKV(entries []byte) []byte {
	var seg []byte
	seg = elemBytes(seg, idTracks, entries)
	seg = elemBytes(seg, idCluster, elemUint(nil, idTimestamp, 0))
	out := elemBytes(nil, idEBML, elemBytes(nil, idDocType, []byte("matroska")))
	return elemBytes(out, idSegment, seg)
}

func checkWindow(t *testing.T, name string, src []byte, want []srcBlock, startMs, durMs int64) {
	t.Helper()
	m, err := OpenMKV(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out bytes.Buffer
	win, err := m.Extract(m.Tracks[1], startMs, durMs, 64<<20, &out)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var exp []srcBlock
	for _, b := range want {
		if b.ms >= startMs && b.ms < startMs+durMs {
			exp = append(exp, b)
		}
	}
	if win.BaseMs != exp[0].ms || win.EndMs != exp[len(exp)-1].ms || win.Blocks != len(exp) || win.Capped || win.Bytes != int64(out.Len()) {
		t.Fatalf("%s: window %+v, want base %d end %d, %d blocks, %d bytes", name, win, exp[0].ms, exp[len(exp)-1].ms, len(exp), out.Len())
	}
	om, got := readBack(t, out.Bytes())
	ot := om.Tracks[0]
	if len(om.Tracks) != 1 || ot.Number != 1 || ot.CodecID != "A_AAC" || !bytes.Equal(ot.CodecPrivate, []byte{0x11, 0xB0}) ||
		ot.Language != "eng" || ot.SampleRate != 48000 || ot.Channels != 6 || !bytes.Equal(ot.Encodings, []byte{0x62, 0x40, 0x81, 0x00}) {
		t.Errorf("%s: output track %+v", name, ot)
	}
	if len(got) != len(exp) {
		t.Fatalf("%s: %d blocks out, want %d", name, len(got), len(exp))
	}
	groups := 0
	for i, e := range exp {
		g := got[i]
		if g.ms != e.ms-win.BaseMs || g.data != string(blockData(2, e.ms)) || g.flags != 0x80|e.flags&0x06 {
			t.Fatalf("%s: block %d = %+v, want %d ms, flags %#x, %q", name, i, g, e.ms-win.BaseMs, 0x80|e.flags&0x06, blockData(2, e.ms))
		}
		if e.group {
			groups++
		}
	}
	if groups == 0 {
		t.Errorf("%s: the window holds no BlockGroup block; widen it", name)
	}
}

func TestExtractWindow(t *testing.T) {
	src, want := buildMKV(srcOpts{})
	checkWindow(t, "cues", src, want, 10_000, 7_000)
	checkWindow(t, "from the start", src, want, 0, 3_000)
	checkWindow(t, "cluster edge", src, want, 14_990, 40_000)

	src, want = buildMKV(srcOpts{noCues: true})
	checkWindow(t, "no cues", src, want, 30_000, 9_000)

	src, want = buildMKV(srcOpts{unknownClusters: true, unknownSegment: true})
	checkWindow(t, "unknown sizes", src, want, 20_000, 12_000)

	src, want = buildMKV(srcOpts{unknownClusters: true, unknownSegment: true, noCues: true})
	checkWindow(t, "unknown sizes, no cues", src, want, 41_000, 6_000)
}

func TestExtractNoCuesBuildsIndex(t *testing.T) {
	src, _ := buildMKV(srcOpts{noCues: true})
	m, err := OpenMKV(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if !m.ownIndex || len(m.cues) != 0 {
		t.Fatalf("own index %t, %d cues", m.ownIndex, len(m.cues))
	}
	m.Extract(m.Tracks[1], 30_000, 2_000, 64<<20, io.Discard)
	if len(m.cues) < 7 || m.clusterStart(31_000) == m.firstCluster {
		t.Errorf("after one window: %d clusters known; a later window must not start from the first cluster", len(m.cues))
	}
}

func TestExtractByteCap(t *testing.T) {
	src, _ := buildMKV(srcOpts{})
	m, _ := OpenMKV(bytes.NewReader(src), int64(len(src)))
	var out bytes.Buffer
	win, err := m.Extract(m.Tracks[1], 0, 60_000, 4000, &out)
	if err != nil || !win.Capped || win.Bytes > 4000 || int64(out.Len()) != win.Bytes || win.DurationMs() >= 60_000 {
		t.Fatalf("window %+v, %d bytes written, %v", win, out.Len(), err)
	}
	_, got := readBack(t, out.Bytes())
	if len(got) != win.Blocks {
		t.Errorf("%d blocks read back, window says %d", len(got), win.Blocks)
	}
}

func TestExtractPastEnd(t *testing.T) {
	src, _ := buildMKV(srcOpts{})
	m, _ := OpenMKV(bytes.NewReader(src), int64(len(src)))
	win, err := m.Extract(m.Tracks[1], 70_000, 5_000, 64<<20, io.Discard)
	if err != nil || win.Blocks != 0 {
		t.Errorf("window past the end: %+v, %v", win, err)
	}
}

// allocs runs fn and returns the bytes it allocated.
func allocs(fn func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	fn()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// TestCorruptSizesRefused feeds sizes that are wrong or huge. Each must be
// refused with ErrCorrupt, and none may cause a large allocation.
func TestCorruptSizesRefused(t *testing.T) {
	good, _ := buildMKV(srcOpts{})
	ones := []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE} // 2^56-2 bytes
	// long writes an element with an 8-byte size field.
	long := func(id uint32, payload []byte) []byte {
		b := ebmlID(nil, id)
		b = append(b, 0x01, 0, 0, 0, 0, 0, 0, byte(len(payload)))
		return append(b, payload...)
	}
	withTrack := func(entry []byte) []byte { return minimalMKV(entry) }
	var te []byte
	te = elemUint(te, idTrackNumber, 1)
	te = elemUint(te, idTrackType, 2)

	cases := map[string][]byte{
		"empty file":         {},
		"not EBML":           []byte("RIFF....AVI LIST"),
		"zero length byte":   append(ebmlID(nil, idEBML), 0x00, 0x42, 0x82),
		"id longer than 4":   {0x08, 0x01, 0x02, 0x03, 0x04, 0x81, 0x00},
		"EBML header huge":   append(ebmlID(nil, idEBML), ones...),
		"segment past end":   append(elemBytes(nil, idEBML, elemBytes(nil, idDocType, []byte("matroska"))), append(ebmlID(nil, idSegment), 0x01, 0, 0, 0, 0, 0, 0x10, 0)...),
		"doc type not mkv":   append(elemBytes(nil, idEBML, elemBytes(nil, idDocType, []byte("qt  "))), elemBytes(ebmlID(nil, idSegment), 0, nil)...),
		"CodecPrivate huge":  withTrack(elemBytes(nil, idTrackEntry, append(te, append(ebmlID(nil, idCodecPriv), ones...)...))),
		"CodecPrivate 2 MiB": withTrack(elemBytes(nil, idTrackEntry, append(te, elemBytes(nil, idCodecPriv, make([]byte, 2<<20))...))),
		"string 1 MiB":       withTrack(elemBytes(nil, idTrackEntry, append(te, elemBytes(nil, idCodecID, make([]byte, 1<<20))...))),
		"integer 9 bytes":    withTrack(elemBytes(nil, idTrackEntry, append(elemBytes(nil, idTrackNumber, make([]byte, 9)), elemUint(nil, idTrackType, 2)...))),
		"child past parent":  withTrack(elemBytes(nil, idTrackEntry, append(te, long(idCodecID, []byte("A_AAC"))[:12]...))),
		"track number 0":     withTrack(elemBytes(nil, idTrackEntry, elemUint(elemUint(nil, idTrackNumber, 0), idTrackType, 2))),
		"unknown-size Info":  append(elemBytes(nil, idEBML, elemBytes(nil, idDocType, []byte("matroska"))), elemBytes(ebmlID(nil, idSegment), 0, append(append(ebmlID(nil, idInfo), 0xFF), 0x81, 0x00))...),
	}
	// Cues and a cluster inside a real file, each given the largest known
	// size its size field can hold: past the end of the file.
	for _, id := range []uint32{idCues, idCluster} {
		b := append([]byte(nil), good...)
		idb := ebmlID(nil, id)
		// The element itself, not its id inside the SeekHead: search from
		// the first cluster on.
		m0, _ := OpenMKV(bytes.NewReader(good), int64(len(good)))
		i := int(m0.firstCluster) + bytes.Index(b[m0.firstCluster:], idb)
		l := vintLen(b[i+len(idb)])
		copy(b[i+len(idb):], sizeOfLen(1<<(7*l)-2, l))
		if id == idCues {
			// Cues are found through the SeekHead; bad ones are worked around
			// by walking the clusters, with a warning, not refused.
			var m *MKV
			var err error
			n := allocs(func() {
				if m, err = OpenMKV(bytes.NewReader(b), int64(len(b))); err == nil {
					_, err = m.Extract(m.Tracks[1], 30_000, 2_000, 64<<20, io.Discard)
				}
			})
			if err != nil {
				t.Errorf("cues past the file: %v, want them worked around", err)
				continue
			}
			if !m.ownIndex || len(m.Warnings) != 1 || !strings.Contains(m.Warnings[0], "cues unreadable") || n > 8<<20 {
				t.Errorf("cues past the file: err %v, own index %v, warnings %q, %d bytes allocated", err, m != nil && m.ownIndex, m.Warnings, n)
			}
			continue
		}
		cases[fmt.Sprintf("element %#x size past the file", id)] = b
	}
	blockFile := func(blk []byte) []byte {
		var seg []byte
		seg = elemBytes(seg, idTracks, elemBytes(nil, idTrackEntry, te))
		seg = elemBytes(seg, idCluster, append(elemUint(nil, idTimestamp, 0), blk...))
		out := elemBytes(nil, idEBML, elemBytes(nil, idDocType, []byte("matroska")))
		return elemBytes(out, idSegment, seg)
	}
	// A block whose 8-byte size runs past its cluster.
	cases["block past its cluster"] = blockFile(append(append(ebmlID(nil, idSimpleBlock), ones...), 0x81, 0, 0, 0x80, 1, 2, 3))
	// A block of 17 MiB, inside the file but over the 16 MiB cap.
	cases["block over the cap"] = blockFile(simpleBlock(idSimpleBlock, 1, 0, 0x80, make([]byte, 17<<20)))
	// A block too short to hold its own header.
	cases["block too short"] = blockFile(elemBytes(nil, idSimpleBlock, []byte{0x81, 0}))
	for name, b := range cases {
		var err error
		n := allocs(func() {
			var m *MKV
			m, err = OpenMKV(bytes.NewReader(b), int64(len(b)))
			if err == nil {
				for _, tr := range m.Tracks {
					if tr.Audio {
						_, err = m.Extract(tr, 0, 60_000, 64<<20, io.Discard)
						break
					}
				}
			}
		})
		if !errors.Is(err, ErrCorrupt) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
		if n > 8<<20 {
			t.Errorf("%s: allocated %d MiB", name, n>>20)
		}
	}
}

// sizeOfLen encodes v as a size vint of exactly l bytes.
func sizeOfLen(v uint64, l int) []byte {
	v |= 1 << (7 * l)
	b := make([]byte, l)
	for i := range l {
		b[i] = byte(v >> (8 * (l - 1 - i)))
	}
	return b
}

// TestSparse50GB extracts from a 50 GB file whose middle is one huge Void
// element, the way a real film's clusters sit between the header and the
// cues. Only headers are read, and allocations stay small. The file is
// sparse, so it takes no disk space.
func TestSparse50GB(t *testing.T) {
	const gap = 50 << 30
	small, want := buildMKV(srcOpts{lengthMs: 10_000})
	m0, err := OpenMKV(bytes.NewReader(small), int64(len(small)))
	if err != nil {
		t.Fatal(err)
	}
	segStart := m0.seg.DataOff
	firstCluster := m0.firstCluster
	_ = want

	// Layout: [EBML][Segment unknown size][SeekHead Void][Info][Tracks]
	// [Void of 50 GB][clusters of the small file]. Cues are left out, so the
	// walk must skip the Void by its size.
	var head []byte
	head = elemBytes(head, idEBML, elemBytes(nil, idDocType, []byte("matroska")))
	head = ebmlID(head, idSegment)
	head = append(head, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	head = append(head, small[segStart+int64(len(seekHead(0, 0))):firstCluster]...) // Info and Tracks
	void := ebmlID(nil, 0xEC)
	void = append(void, ebmlSize(nil, gap)...)
	head = append(head, void...)
	clusters := small[firstCluster:]
	if i := bytes.Index(clusters, ebmlID(nil, idCues)); i > 0 {
		clusters = clusters[:i]
	}

	p := filepath.Join(t.TempDir(), "big.mkv")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(head); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(clusters, int64(len(head))+gap); err != nil {
		t.Skipf("sparse files not supported here: %v", err)
	}
	fi, _ := f.Stat()
	if fi.Size() < gap {
		t.Fatalf("file is %d bytes", fi.Size())
	}

	var win Window
	var out bytes.Buffer
	n := allocs(func() {
		m, err := OpenMKV(f, fi.Size())
		if err != nil {
			t.Fatal(err)
		}
		win, err = m.Extract(m.Tracks[1], 2_000, 4_000, 64<<20, &out)
		if err != nil {
			t.Fatal(err)
		}
	})
	if win.Blocks == 0 || win.BaseMs < 2_000 || win.EndMs >= 6_000 {
		t.Errorf("window %+v", win)
	}
	if n > 32<<20 {
		t.Errorf("allocated %d MiB for a 50 GB file", n>>20)
	}
	if !strings.HasPrefix(out.String(), string(ebmlID(nil, idEBML))) {
		t.Error("output is not Matroska")
	}
}

// countReader counts ReadAt calls.
type countReader struct {
	r     io.ReaderAt
	calls int
}

func (c *countReader) ReadAt(p []byte, off int64) (int, error) {
	c.calls++
	return c.r.ReadAt(p, off)
}

// TestOpenReadsNoClusters: opening reads the elements before the first
// cluster and what the SeekHead points to, never the clusters themselves,
// and reads cues and tags in one piece each, so it costs the same number of
// reads for 12 clusters or 1200 (a film over NFS).
func TestOpenReadsNoClusters(t *testing.T) {
	calls := func(clusterMs int64, noCues bool) int {
		b, _ := buildMKV(srcOpts{clusterMs: clusterMs, tags: true, noCues: noCues})
		cr := &countReader{r: bytes.NewReader(b)}
		if _, err := OpenMKV(cr, int64(len(b))); err != nil {
			t.Fatal(err)
		}
		return cr.calls
	}
	if few, many := calls(5000, false), calls(50, false); few != many || few > 100 {
		t.Errorf("open made %d reads with 12 clusters and %d with 1200; want the same, and few", few, many)
	}
	if few, many := calls(5000, true), calls(50, true); few != many {
		t.Errorf("open without cues made %d reads with 12 clusters and %d with 1200; it must stop at the first", few, many)
	}
}

// TestClusterLeadingElements: a cluster may start with CRC-32 or Void
// before its Timestamp (Secret Invasion's encodes do); only a block before
// the Timestamp is an error.
func TestClusterLeadingElements(t *testing.T) {
	var te []byte
	te = elemUint(te, idTrackNumber, 1)
	te = elemUint(te, idTrackType, 2)
	file := func(cluster []byte) []byte {
		var seg []byte
		seg = elemBytes(seg, idTracks, elemBytes(nil, idTrackEntry, te))
		seg = elemBytes(seg, idCluster, cluster)
		out := elemBytes(nil, idEBML, elemBytes(nil, idDocType, []byte("matroska")))
		return elemBytes(out, idSegment, seg)
	}
	var good []byte
	good = elemBytes(good, 0xBF, []byte{1, 2, 3, 4}) // CRC-32
	good = elemBytes(good, 0xEC, make([]byte, 3))    // Void
	good = elemUint(good, idTimestamp, 1000)
	good = append(good, simpleBlock(idSimpleBlock, 1, 5, 0x80, []byte("x"))...)
	b := file(good)
	m, err := OpenMKV(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	win, err := m.Extract(m.Tracks[0], 0, 5000, 1<<20, io.Discard)
	if err != nil || win.Blocks != 1 || win.BaseMs != 1005 {
		t.Errorf("leading CRC-32 and Void: %+v, %v", win, err)
	}
	bad := append(simpleBlock(idSimpleBlock, 1, 5, 0x80, []byte("x")), elemUint(nil, idTimestamp, 1000)...)
	b = file(bad)
	m, _ = OpenMKV(bytes.NewReader(b), int64(len(b)))
	if _, err := m.Extract(m.Tracks[0], 0, 5000, 1<<20, io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Errorf("block before the timestamp: %v, want refused", err)
	}
}
