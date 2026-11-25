package media

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
)

// Output clusters are cut at this length or size, so each fits in memory
// and every block's int16 time offset stays in range.
const (
	clusterSpanMs = 30_000
	clusterBytes  = 2 << 20
)

// mka writes one audio track as a Matroska file: EBML header, a Segment of
// unknown size (ffmpeg reads it as a stream), Info, Tracks, then clusters.
// Times are milliseconds from the first block.
type mka struct {
	w       io.Writer
	max     int64
	written int64
	cl      bytes.Buffer // the open cluster's blocks
	clMs    int64
	open    bool
}

// ebmlID appends an element id (its bytes as written, marker included).
func ebmlID(b []byte, id uint32) []byte {
	switch {
	case id >= 1<<24:
		return append(b, byte(id>>24), byte(id>>16), byte(id>>8), byte(id))
	case id >= 1<<16:
		return append(b, byte(id>>16), byte(id>>8), byte(id))
	case id >= 1<<8:
		return append(b, byte(id>>8), byte(id))
	}
	return append(b, byte(id))
}

// ebmlSize appends n as the shortest size vint (all-ones values are avoided,
// since they mean an unknown size).
func ebmlSize(b []byte, n uint64) []byte {
	for l := 1; l <= 8; l++ {
		if n < 1<<(7*l)-1 {
			v := n | 1<<(7*l)
			for i := l - 1; i >= 0; i-- {
				b = append(b, byte(v>>(8*i)))
			}
			return b
		}
	}
	panic("ebml size too large") // only sizes under 2^56 are ever written
}

func elemBytes(b []byte, id uint32, payload []byte) []byte {
	b = ebmlID(b, id)
	b = ebmlSize(b, uint64(len(payload)))
	return append(b, payload...)
}

func elemUint(b []byte, id uint32, v uint64) []byte {
	var p [8]byte
	binary.BigEndian.PutUint64(p[:], v)
	i := 0
	for i < 7 && p[i] == 0 {
		i++
	}
	return elemBytes(b, id, p[i:])
}

func elemFloat(b []byte, id uint32, v float64) []byte {
	var p [8]byte
	binary.BigEndian.PutUint64(p[:], math.Float64bits(v))
	return elemBytes(b, id, p[:])
}

func newMKA(w io.Writer, t Track, max int64) (*mka, error) {
	var h []byte
	var eh []byte
	eh = elemUint(eh, 0x4286, 1) // EBMLVersion
	eh = elemUint(eh, 0x42F7, 1) // EBMLReadVersion
	eh = elemUint(eh, 0x42F2, 4) // EBMLMaxIDLength
	eh = elemUint(eh, 0x42F3, 8) // EBMLMaxSizeLength
	eh = elemBytes(eh, idDocType, []byte("matroska"))
	eh = elemUint(eh, 0x4287, 4) // DocTypeVersion
	eh = elemUint(eh, 0x4285, 2) // DocTypeReadVersion
	h = elemBytes(h, idEBML, eh)
	h = ebmlID(h, idSegment)
	h = append(h, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF) // unknown size

	var info []byte
	info = elemUint(info, idTimescale, 1_000_000)
	info = elemBytes(info, 0x4D80, []byte("subtitle-sync")) // MuxingApp
	info = elemBytes(info, 0x5741, []byte("subtitle-sync")) // WritingApp
	h = elemBytes(h, idInfo, info)

	var au []byte
	if t.SampleRate > 0 {
		au = elemFloat(au, idSampleRate, t.SampleRate)
	}
	if t.OutRate > 0 {
		au = elemFloat(au, idOutRate, t.OutRate)
	}
	if t.Channels > 0 {
		au = elemUint(au, idChannels, t.Channels)
	}
	if t.BitDepth > 0 {
		au = elemUint(au, idBitDepth, t.BitDepth)
	}
	var te []byte
	te = elemUint(te, idTrackNumber, 1)
	te = elemUint(te, idTrackUID, 1)
	te = elemUint(te, idTrackType, 2)
	te = elemBytes(te, idCodecID, []byte(t.CodecID))
	if len(t.CodecPrivate) > 0 {
		te = elemBytes(te, idCodecPriv, t.CodecPrivate)
	}
	if t.CodecDelay > 0 {
		te = elemUint(te, idCodecDelay, t.CodecDelay)
	}
	if t.SeekPreRoll > 0 {
		te = elemUint(te, idSeekPreRoll, t.SeekPreRoll)
	}
	if t.Language != "" {
		te = elemBytes(te, idLanguage, []byte(t.Language))
	}
	if len(t.Encodings) > 0 {
		// Header-stripping compression and the like: the blocks are copied as
		// stored, so the reader needs the same instructions to undo it.
		te = elemBytes(te, idEncodings, t.Encodings)
	}
	te = elemBytes(te, idAudio, au)
	h = elemBytes(h, idTracks, elemBytes(nil, idTrackEntry, te))

	if int64(len(h)) > max {
		return nil, corrupt("track header of %d bytes is over the %d byte cap", len(h), max)
	}
	n, err := w.Write(h)
	return &mka{w: w, max: max, written: int64(n)}, err
}

// add writes one block at ms (from the first block). flags are the source
// block's flags; the lacing bits are kept and the block is marked a key
// frame, as every audio frame is. ok is false when the block would pass
// the byte cap; nothing is written then.
func (m *mka) add(ms int64, flags byte, data []byte) (ok bool, err error) {
	if ms < 0 {
		return true, nil
	}
	if m.open && (ms < m.clMs || ms-m.clMs >= clusterSpanMs || m.cl.Len() >= clusterBytes) {
		if err := m.flush(); err != nil {
			return false, err
		}
	}
	// SimpleBlock: id, size, track 1, int16 time, flags, data.
	blockLen := 4 + int64(len(data))
	need := blockLen + 1 + 8 // id + largest size vint
	if !m.open {
		need += 4 + 8 + 2 + 8 // cluster id, size, timestamp element
	}
	if m.written+int64(m.cl.Len())+need > m.max {
		return false, nil
	}
	if !m.open {
		m.open, m.clMs = true, ms
	}
	var hdr []byte
	hdr = ebmlID(hdr, idSimpleBlock)
	hdr = ebmlSize(hdr, uint64(blockLen))
	rel := ms - m.clMs
	hdr = append(hdr, 0x81, byte(rel>>8), byte(rel), 0x80|flags&0x06)
	m.cl.Write(hdr)
	m.cl.Write(data)
	return true, nil
}

func (m *mka) flush() error {
	if !m.open {
		return nil
	}
	var ts []byte
	ts = elemUint(ts, idTimestamp, uint64(m.clMs))
	var hdr []byte
	hdr = ebmlID(hdr, idCluster)
	hdr = ebmlSize(hdr, uint64(len(ts)+m.cl.Len()))
	hdr = append(hdr, ts...)
	for _, b := range [][]byte{hdr, m.cl.Bytes()} {
		n, err := m.w.Write(b)
		m.written += int64(n)
		if err != nil {
			return err
		}
	}
	m.cl.Reset()
	m.open = false
	return nil
}

// close writes the last cluster and returns the bytes written in all.
func (m *mka) close() (int64, error) {
	err := m.flush()
	return m.written, err
}
