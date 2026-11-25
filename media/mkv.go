package media

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Matroska element ids used here.
const (
	idEBML        = 0x1A45DFA3
	idDocType     = 0x4282
	idSegment     = 0x18538067
	idSeekHead    = 0x114D9B74
	idSeek        = 0x4DBB
	idSeekID      = 0x53AB
	idSeekPos     = 0x53AC
	idInfo        = 0x1549A966
	idTimescale   = 0x2AD7B1
	idDuration    = 0x4489
	idTracks      = 0x1654AE6B
	idTrackEntry  = 0xAE
	idTrackNumber = 0xD7
	idTrackUID    = 0x73C5
	idTrackType   = 0x83
	idFlagDefault = 0x88
	idFlagEnabled = 0xB9
	idFlagComment = 0x55AF // FlagCommentary
	idFlagVisual  = 0x55AB // FlagVisualImpaired: audio description
	idName        = 0x536E
	idLanguage    = 0x22B59C
	idLangIETF    = 0x22B59D
	idCodecID     = 0x86
	idCodecPriv   = 0x63A2
	idCodecDelay  = 0x56AA
	idSeekPreRoll = 0x56BB
	idAudio       = 0xE1
	idSampleRate  = 0xB5
	idOutRate     = 0x78B5
	idChannels    = 0x9F
	idBitDepth    = 0x6264
	idEncodings   = 0x6D80
	idCluster     = 0x1F43B675
	idTimestamp   = 0xE7
	idSimpleBlock = 0xA3
	idBlockGroup  = 0xA0
	idBlock       = 0xA1
	idCues        = 0x1C53BB6B
	idCuePoint    = 0xBB
	idCueTime     = 0xB3
	idCuePos      = 0xB7
	idCueCluster  = 0xF1
	idTags        = 0x1254C367
	idTag         = 0x7373
	idTargets     = 0x63C0
	idTagTrackUID = 0x63C5
	idSimpleTag   = 0x67C8
	idTagName     = 0x45A3
	idTagString   = 0x4487
	idChapters    = 0x1043A770
	idAttachments = 0x1941A469
)

// isSegmentChild reports ids that end an unknown-size Cluster.
func isSegmentChild(id uint32) bool {
	switch id {
	case idCluster, idCues, idTags, idChapters, idAttachments, idSeekHead, idInfo, idTracks, idEBML, idSegment:
		return true
	}
	return false
}

// Track is one track of the file.
type Track struct {
	Number       uint64
	UID          uint64
	Audio        bool
	CodecID      string // such as A_AAC, A_AC3, A_EAC3, A_DTS, A_TRUEHD, A_OPUS
	CodecPrivate []byte
	Encodings    []byte // ContentEncodings payload, copied as is
	CodecDelay   uint64
	SeekPreRoll  uint64
	Language     string // Language, or LanguageIETF when set
	Default      bool
	Enabled      bool
	Commentary   bool // FlagCommentary
	Described    bool // FlagVisualImpaired: audio description for the blind
	Name         string
	SampleRate   float64
	OutRate      float64
	Channels     uint64
	BitDepth     uint64
	BPS          int64 // bits per second from the file's tags; 0 unknown
}

type cue struct {
	ms  int64 // cluster start
	pos int64 // absolute file offset of the cluster
}

// MKV is an opened Matroska or WebM file.
type MKV struct {
	f            *file
	seg          elem
	segEnd       int64
	scale        int64 // nanoseconds per tick
	DurationMs   int64
	Tracks       []Track
	cues         []cue
	ownIndex     bool // cues were built while reading clusters, not from the file
	firstCluster int64
	// Warnings are problems worked around, such as cues that can't be read,
	// for the log.
	Warnings []string
}

// OpenMKV reads a Matroska file's header, tracks, cues and tags. It never
// reads clusters, so its cost does not depend on the file's size.
func OpenMKV(r io.ReaderAt, size int64) (*MKV, error) {
	f := &file{r: r, size: size}
	h, err := f.header(0, size)
	if err != nil {
		return nil, err
	}
	if h.ID != idEBML || h.Size == unknownSize {
		return nil, corrupt("not a Matroska file")
	}
	docOK := false
	if err := f.children(h, nil, func(c elem) (int64, error) {
		if c.ID == idDocType {
			s, err := f.string(c)
			docOK = s == "matroska" || s == "webm"
			return 0, err
		}
		return 0, nil
	}); err != nil {
		return nil, err
	}
	if !docOK {
		return nil, corrupt("not a Matroska or WebM document")
	}
	seg, err := f.header(h.End(), size)
	if err != nil {
		return nil, err
	}
	if seg.ID != idSegment {
		return nil, corrupt("no Segment after the EBML header")
	}
	m := &MKV{f: f, seg: seg, segEnd: seg.End(), scale: 1_000_000, firstCluster: -1}
	if m.segEnd < 0 {
		m.segEnd = size
	}
	var seekCues, seekTags int64 = -1, -1
	var info, tracks, cues, tags *elem
	err = f.children(seg, nil, func(c elem) (int64, error) {
		c2 := c
		switch c.ID {
		case idSeekHead:
			return 0, m.readSeekHead(c, &seekCues, &seekTags)
		case idInfo:
			info = &c2
		case idTracks:
			tracks = &c2
		case idCues:
			cues = &c2
		case idTags:
			tags = &c2
		case idCluster:
			// Only the elements before the clusters are walked; cues and tags
			// after them are reached through the SeekHead.
			m.firstCluster = c.Off
			return 0, errStop{at: c.Off}
		}
		if c.Size == unknownSize {
			return 0, corrupt("element %#x at %d has an unknown size", c.ID, c.Off)
		}
		return 0, nil
	})
	if _, stopped := err.(errStop); stopped {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	at := func(rel int64, id uint32, what string) *elem {
		if rel < 0 {
			return nil
		}
		e, err := f.header(seg.DataOff+rel, m.segEnd)
		if err == nil && e.ID != id {
			err = corrupt("no %s element where the index points", what)
		}
		if err != nil {
			m.Warnings = append(m.Warnings, fmt.Sprintf("%s unreadable (%v); going without", what, err))
			return nil
		}
		return &e
	}
	if cues == nil {
		cues = at(seekCues, idCues, "cues")
	}
	if tags == nil {
		tags = at(seekTags, idTags, "tags")
	}
	if info != nil {
		if err := m.readInfo(*info); err != nil {
			return nil, err
		}
	}
	if tracks == nil {
		return nil, corrupt("no Tracks element before the first cluster")
	}
	if err := m.readTracks(*tracks); err != nil {
		return nil, err
	}
	if cues != nil {
		if err := m.readCues(*cues); err != nil {
			// Clusters can still be found by walking them, so bad cues cost
			// time, not the window.
			m.cues = nil
			m.Warnings = append(m.Warnings, fmt.Sprintf("cues unreadable (%v); reading clusters in order instead", err))
		}
	}
	if tags != nil {
		_ = m.readTags(*tags) // tags only help choose a track; a bad one is ignored
	}
	if m.firstCluster < 0 && len(m.cues) == 0 {
		return nil, corrupt("no clusters")
	}
	if len(m.cues) == 0 {
		m.ownIndex = true
	}
	return m, nil
}

func (m *MKV) readSeekHead(e elem, cues, tags *int64) error {
	return m.f.children(e, nil, func(s elem) (int64, error) {
		if s.ID != idSeek {
			return 0, nil
		}
		var id, pos uint64
		var hasPos bool
		err := m.f.children(s, nil, func(c elem) (int64, error) {
			var err error
			switch c.ID {
			case idSeekID:
				var b []byte
				if b, err = m.f.payload(c, 4); err == nil {
					for _, x := range b {
						id = id<<8 | uint64(x)
					}
				}
			case idSeekPos:
				pos, err = m.f.uint(c)
				hasPos = true
			}
			return 0, err
		})
		if err == nil && hasPos && pos < uint64(m.f.size) {
			switch id {
			case idCues:
				*cues = int64(pos)
			case idTags:
				*tags = int64(pos)
			}
		}
		return 0, err
	})
}

func (m *MKV) readInfo(e elem) error {
	var dur float64
	err := m.f.children(e, nil, func(c elem) (int64, error) {
		var err error
		switch c.ID {
		case idTimescale:
			var v uint64
			if v, err = m.f.uint(c); err == nil {
				if v == 0 || v > 1_000_000_000 {
					return 0, corrupt("timestamp scale %d", v)
				}
				m.scale = int64(v)
			}
		case idDuration:
			dur, err = m.f.float(c)
		}
		return 0, err
	})
	if err != nil {
		return err
	}
	if ms := dur * float64(m.scale) / 1e6; ms > 0 && ms < 1e12 {
		m.DurationMs = int64(ms)
	}
	return nil
}

func (m *MKV) readTracks(e elem) error {
	return m.f.children(e, nil, func(te elem) (int64, error) {
		if te.ID != idTrackEntry {
			return 0, nil
		}
		if len(m.Tracks) >= maxTrackEntries {
			return 0, corrupt("over %d tracks", maxTrackEntries)
		}
		t := Track{Language: "eng", Default: true, Enabled: true} // Matroska defaults
		lang, ietf := "", ""
		err := m.f.children(te, nil, func(c elem) (int64, error) {
			var err error
			var v uint64
			switch c.ID {
			case idTrackNumber:
				t.Number, err = m.f.uint(c)
			case idTrackUID:
				t.UID, err = m.f.uint(c)
			case idTrackType:
				v, err = m.f.uint(c)
				t.Audio = v == 2
			case idFlagDefault:
				v, err = m.f.uint(c)
				t.Default = v != 0
			case idFlagEnabled:
				v, err = m.f.uint(c)
				t.Enabled = v != 0
			case idFlagComment:
				v, err = m.f.uint(c)
				t.Commentary = v != 0
			case idFlagVisual:
				v, err = m.f.uint(c)
				t.Described = v != 0
			case idName:
				t.Name, err = m.f.string(c)
			case idLanguage:
				lang, err = m.f.string(c)
			case idLangIETF:
				ietf, err = m.f.string(c)
			case idCodecID:
				t.CodecID, err = m.f.string(c)
			case idCodecPriv:
				t.CodecPrivate, err = m.f.payload(c, maxCodecPrivate)
			case idCodecDelay:
				t.CodecDelay, err = m.f.uint(c)
			case idSeekPreRoll:
				t.SeekPreRoll, err = m.f.uint(c)
			case idEncodings:
				t.Encodings, err = m.f.payload(c, maxEncodings)
			case idAudio:
				err = m.f.children(c, nil, func(a elem) (int64, error) {
					var err error
					switch a.ID {
					case idSampleRate:
						t.SampleRate, err = m.f.float(a)
					case idOutRate:
						t.OutRate, err = m.f.float(a)
					case idChannels:
						t.Channels, err = m.f.uint(a)
					case idBitDepth:
						t.BitDepth, err = m.f.uint(a)
					}
					return 0, err
				})
			}
			return 0, err
		})
		if err != nil {
			return 0, err
		}
		if lang != "" {
			t.Language = lang
		}
		if ietf != "" {
			t.Language = ietf
		}
		if t.Number == 0 || t.Number > 1<<56 {
			return 0, corrupt("track number %d", t.Number)
		}
		m.Tracks = append(m.Tracks, t)
		return 0, nil
	})
}

// inMemory reads a whole element of at most max bytes and returns a file
// over its payload alone, with the element as its root, so parsing it makes
// no more reads.
func (m *MKV) inMemory(e elem, max int64) (*file, elem, error) {
	b, err := m.f.payload(e, max)
	if err != nil {
		return nil, elem{}, err
	}
	return &file{r: bytes.NewReader(b), size: int64(len(b))}, elem{ID: e.ID, Size: int64(len(b))}, nil
}

func (m *MKV) readCues(e elem) error {
	f, root, err := m.inMemory(e, maxElementRead)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	err = f.children(root, nil, func(cp elem) (int64, error) {
		if cp.ID != idCuePoint {
			return 0, nil
		}
		var t uint64
		var pos int64 = -1
		err := f.children(cp, nil, func(c elem) (int64, error) {
			var err error
			switch c.ID {
			case idCueTime:
				t, err = f.uint(c)
			case idCuePos:
				err = f.children(c, nil, func(p elem) (int64, error) {
					if p.ID != idCueCluster {
						return 0, nil
					}
					v, err := f.uint(p)
					if err == nil && v < uint64(m.f.size) {
						pos = m.seg.DataOff + int64(v)
					}
					return 0, err
				})
			}
			return 0, err
		})
		if err != nil {
			return 0, err
		}
		if pos >= 0 && !seen[pos] {
			if len(m.cues) >= maxCuePoints {
				return 0, corrupt("over %d cue points", maxCuePoints)
			}
			seen[pos] = true
			m.cues = append(m.cues, cue{ms: m.ticksMs(int64(min(t, math.MaxInt64/2))), pos: pos})
		}
		return 0, nil
	})
	sort.Slice(m.cues, func(i, j int) bool { return m.cues[i].ms < m.cues[j].ms })
	return err
}

func (m *MKV) readTags(e elem) error {
	f, root, err := m.inMemory(e, maxTagsRead)
	if err != nil {
		return err
	}
	return f.children(root, nil, func(tag elem) (int64, error) {
		if tag.ID != idTag {
			return 0, nil
		}
		var uid uint64
		var bps int64
		err := f.children(tag, nil, func(c elem) (int64, error) {
			switch c.ID {
			case idTargets:
				return 0, f.children(c, nil, func(t elem) (int64, error) {
					if t.ID == idTagTrackUID {
						v, err := f.uint(t)
						uid = v
						return 0, err
					}
					return 0, nil
				})
			case idSimpleTag:
				var name, val string
				err := f.children(c, nil, func(s elem) (int64, error) {
					var err error
					switch s.ID {
					case idTagName:
						name, err = f.string(s)
					case idTagString:
						val, err = f.string(s)
					}
					return 0, err
				})
				if err == nil && strings.EqualFold(name, "BPS") {
					if v, perr := strconv.ParseInt(strings.TrimSpace(val), 10, 64); perr == nil && v > 0 && v < 1<<40 {
						bps = v
					}
				}
				return 0, err
			}
			return 0, nil
		})
		if err == nil && uid != 0 && bps > 0 {
			for i := range m.Tracks {
				if m.Tracks[i].UID == uid {
					m.Tracks[i].BPS = bps
				}
			}
		}
		return 0, err
	})
}

// ticksMs turns file ticks into milliseconds without overflow.
func (m *MKV) ticksMs(t int64) int64 {
	if t == 0 {
		return 0
	}
	f := float64(t) * float64(m.scale) / 1e6
	if f > math.MaxInt64/4 {
		return math.MaxInt64 / 4
	}
	if f < -math.MaxInt64/4 {
		return -math.MaxInt64 / 4
	}
	return int64(math.Round(f))
}

// Window is one piece of audio written out.
type Window struct {
	BaseMs int64 // source time of the first block written; output times count from it
	EndMs  int64 // source time of the last block written
	Blocks int
	Bytes  int64 // size of the .mka written
	Capped bool  // stopped early at the byte cap
}

// DurationMs of a window as written.
func (w Window) DurationMs() int64 { return w.EndMs - w.BaseMs }

// clusterStart returns where to start reading for a window at startMs.
func (m *MKV) clusterStart(startMs int64) int64 {
	i := sort.Search(len(m.cues), func(i int) bool { return m.cues[i].ms > startMs })
	if i > 0 {
		return m.cues[i-1].pos
	}
	if m.firstCluster >= 0 {
		return m.firstCluster
	}
	return m.cues[0].pos
}

// remember adds a cluster to the index built while reading, for files
// without cues, so the next window does not start from the first cluster.
func (m *MKV) remember(ms, pos int64) {
	if !m.ownIndex || len(m.cues) >= maxCuePoints {
		return
	}
	i := sort.Search(len(m.cues), func(i int) bool { return m.cues[i].pos >= pos })
	if i < len(m.cues) && m.cues[i].pos == pos {
		return
	}
	m.cues = append(m.cues, cue{})
	copy(m.cues[i+1:], m.cues[i:])
	m.cues[i] = cue{ms: ms, pos: pos}
	sort.SliceStable(m.cues, func(a, b int) bool { return m.cues[a].ms < m.cues[b].ms })
}

// Extract copies the blocks of track t with times in [startMs, startMs+durMs)
// into an audio-only Matroska file written to w, stopping before the output
// would pass maxBytes. Memory use is bounded by the largest block (16 MiB
// cap) plus one output cluster, whatever the file's size.
func (m *MKV) Extract(t Track, startMs, durMs, maxBytes int64, w io.Writer) (Window, error) {
	endMs := startMs + durMs
	out, err := newMKA(w, t, maxBytes)
	if err != nil {
		return Window{}, err
	}
	win := Window{BaseMs: -1}
	var buf []byte
	pos := m.clusterStart(startMs)
	done := false
	for pos < m.segEnd && !done {
		c, err := m.f.header(pos, m.segEnd)
		if err != nil {
			return win, err
		}
		if c.ID != idCluster {
			if c.Size == unknownSize {
				break
			}
			pos = c.End()
			continue
		}
		var clusterMs int64
		haveTs := false
		next, err := m.clusterBlocks(c, func(b elem) (bool, error) {
			if b.ID == idTimestamp {
				v, err := m.f.uint(b)
				if err != nil {
					return false, err
				}
				clusterMs, haveTs = m.ticksMs(int64(min(v, math.MaxInt64/2))), true
				m.remember(clusterMs, c.Off)
				return true, nil
			}
			if b.ID != idSimpleBlock && b.ID != idBlockGroup {
				return true, nil // CRC-32, Void, Position, PrevSize and the like
			}
			if !haveTs {
				return false, corrupt("cluster at %d has blocks before its timestamp", c.Off)
			}
			if clusterMs > endMs+60_000 { // nothing later can fall in the window
				return false, nil
			}
			blk := b
			if b.ID == idBlockGroup {
				found := false
				if err := m.f.children(b, nil, func(x elem) (int64, error) {
					if x.ID == idBlock && !found {
						blk, found = x, true
					}
					return 0, nil
				}); err != nil {
					return false, err
				}
				if !found {
					return true, nil
				}
			}
			num, rel, flags, dataOff, err := m.blockHeader(blk)
			if err != nil {
				return false, err
			}
			if num != t.Number {
				return true, nil
			}
			ms := clusterMs + m.ticksMs(int64(rel))
			if ms < startMs {
				return true, nil
			}
			if ms >= endMs {
				done = true
				return false, nil
			}
			n := blk.End() - dataOff
			if n > maxBlock {
				return false, corrupt("block at %d is %d bytes, over the %d byte cap", blk.Off, n, maxBlock)
			}
			if int64(cap(buf)) < n {
				buf = make([]byte, n)
			}
			buf = buf[:n]
			if err := m.f.readFull(dataOff, buf); err != nil {
				return false, err
			}
			if win.BaseMs < 0 {
				win.BaseMs = ms
			}
			if ms < win.BaseMs {
				return true, nil // earlier than the first block written; output times can't go below 0
			}
			ok, err := out.add(ms-win.BaseMs, flags, buf)
			if err != nil {
				return false, err
			}
			if !ok {
				win.Capped, done = true, true
				return false, nil
			}
			win.Blocks++
			win.EndMs = max(win.EndMs, ms)
			return true, nil
		})
		if err != nil {
			return win, err
		}
		newPos := c.End()
		if c.Size == unknownSize {
			newPos = next
		}
		if newPos <= c.Off { // every step must move forward, or the walk could never end
			return win, corrupt("cluster at %d does not end", c.Off)
		}
		pos = newPos
		if clusterMs > endMs+60_000 {
			break
		}
	}
	n, err := out.close()
	win.Bytes = n
	if win.Blocks == 0 {
		win.BaseMs, win.EndMs = startMs, startMs
	}
	return win, err
}

// clusterBlocks walks a cluster's children, calling fn until it returns
// false. It returns where the cluster ends (for an unknown-size cluster,
// where the next top-level element starts).
func (m *MKV) clusterBlocks(c elem, fn func(elem) (bool, error)) (int64, error) {
	end := c.End()
	stopped := false
	err := m.f.children(c, isSegmentChild, func(b elem) (int64, error) {
		if stopped {
			return 0, nil
		}
		if b.Size == unknownSize {
			return 0, corrupt("element %#x at %d inside a cluster has an unknown size", b.ID, b.Off)
		}
		more, err := fn(b)
		if err != nil {
			return 0, err
		}
		if !more {
			stopped = true
			if c.Size == unknownSize {
				return 0, errStop{at: b.End()}
			}
		}
		end = max(end, b.End())
		return 0, nil
	})
	if s, ok := err.(errStop); ok {
		return s.at, nil
	}
	return end, err
}

// errStop ends a walk early inside an unknown-size cluster.
type errStop struct{ at int64 }

func (errStop) Error() string { return "stop" }

// blockHeader reads a (Simple)Block's track number, relative time and flags.
func (m *MKV) blockHeader(b elem) (track uint64, rel int16, flags byte, dataOff int64, err error) {
	var h [maxEBMLSizeLen + 3]byte
	n := min(int64(len(h)), b.Size)
	if n < 4 {
		return 0, 0, 0, 0, corrupt("block at %d is too short", b.Off)
	}
	if err := m.f.readFull(b.DataOff, h[:n]); err != nil {
		return 0, 0, 0, 0, err
	}
	l := vintLen(h[0])
	if l == 0 || int64(l+3) > n {
		return 0, 0, 0, 0, corrupt("bad track number in block at %d", b.Off)
	}
	track, _ = vintValue(h[:l])
	rel = int16(uint16(h[l])<<8 | uint16(h[l+1]))
	return track, rel, h[l+2], b.DataOff + int64(l+3), nil
}
