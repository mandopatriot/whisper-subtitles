// Package media reads the audio of a video file in small pieces, without
// decoding it, and writes a piece out as an audio-only Matroska file. Every
// size read from a file is checked against its parent, the end of the file
// and fixed caps before anything is read or allocated, so a corrupt or
// hostile file can't make the plugin allocate more than a few MiB.
package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrCorrupt marks data that breaks the format or a cap.
var ErrCorrupt = errors.New("corrupt or unsupported file")

func corrupt(f string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(f, a...))
}

// Caps on what is read into memory from one file.
const (
	maxElementRead   = 16 << 20 // any element payload read whole
	maxCodecPrivate  = 1 << 20
	maxEncodings     = 64 << 10 // ContentEncodings copied as is
	maxString        = 4 << 10  // names, codec ids, languages
	maxBlock         = 16 << 20 // one block or sample
	maxCuePoints     = 1 << 20
	maxTrackEntries  = 256
	maxTagsRead      = 4 << 20
	unknownSize      = -1
	maxEBMLIDLen     = 4
	maxEBMLSizeLen   = 8
	maxChildElements = 1 << 20 // children read from one master element
)

// elem is one EBML element header.
type elem struct {
	ID      uint32
	Off     int64 // where the header starts
	DataOff int64 // where the payload starts
	Size    int64 // payload size, or unknownSize
}

// End is where the payload ends, or -1 for an unknown size.
func (e elem) End() int64 {
	if e.Size == unknownSize {
		return -1
	}
	return e.DataOff + e.Size
}

// file is the source, read only through ReadAt.
type file struct {
	r    io.ReaderAt
	size int64
}

func (f *file) readFull(off int64, b []byte) error {
	if off < 0 || off > f.size || int64(len(b)) > f.size-off {
		return corrupt("read of %d bytes at %d is past the end of the file (%d)", len(b), off, f.size)
	}
	n, err := f.r.ReadAt(b, off)
	if n == len(b) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// header reads the element header at off. limit is the end of the parent
// (or of the file); an element with a known size must end at or before it.
func (f *file) header(off, limit int64) (elem, error) {
	var b [maxEBMLIDLen + maxEBMLSizeLen]byte
	n := int64(len(b))
	if rest := min(limit, f.size) - off; rest < n {
		n = rest
	}
	if n < 2 {
		return elem{}, corrupt("element header at %d runs past its parent", off)
	}
	if err := f.readFull(off, b[:n]); err != nil {
		return elem{}, err
	}
	idLen := vintLen(b[0])
	if idLen == 0 || idLen > maxEBMLIDLen || int64(idLen) >= n {
		return elem{}, corrupt("bad element id at %d", off)
	}
	var id uint32
	for _, c := range b[:idLen] {
		id = id<<8 | uint32(c)
	}
	sizeLen := vintLen(b[idLen])
	if sizeLen == 0 || sizeLen > maxEBMLSizeLen || int64(idLen+sizeLen) > n {
		return elem{}, corrupt("bad size of element %#x at %d", id, off)
	}
	size, unknown := vintValue(b[idLen : idLen+sizeLen])
	e := elem{ID: id, Off: off, DataOff: off + int64(idLen+sizeLen), Size: int64(size)}
	if unknown {
		e.Size = unknownSize
		return e, nil
	}
	if size > uint64(math.MaxInt64) || int64(size) > min(limit, f.size)-e.DataOff {
		return elem{}, corrupt("element %#x at %d says %d bytes, more than its parent or the file holds", id, off, size)
	}
	return e, nil
}

// vintLen is the length of a variable-size integer from its first byte, 0
// when no marker bit is set.
func vintLen(first byte) int {
	for i := 0; i < 8; i++ {
		if first&(0x80>>i) != 0 {
			return i + 1
		}
	}
	return 0
}

// vintValue reads a size vint; unknown is true when every value bit is 1.
func vintValue(b []byte) (v uint64, unknown bool) {
	l := len(b)
	v = uint64(b[0]) & (0xFF >> l)
	for _, c := range b[1:] {
		v = v<<8 | uint64(c)
	}
	return v, v == 1<<(7*l)-1
}

// payload reads a whole element payload of at most max bytes.
func (f *file) payload(e elem, max int64) ([]byte, error) {
	if e.Size == unknownSize || e.Size > max {
		return nil, corrupt("element %#x at %d is %d bytes, over the %d byte cap", e.ID, e.Off, e.Size, max)
	}
	b := make([]byte, e.Size)
	return b, f.readFull(e.DataOff, b)
}

func (f *file) uint(e elem) (uint64, error) {
	if e.Size > 8 {
		return 0, corrupt("integer element %#x at %d is %d bytes", e.ID, e.Off, e.Size)
	}
	b, err := f.payload(e, 8)
	if err != nil {
		return 0, err
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

func (f *file) float(e elem) (float64, error) {
	b, err := f.payload(e, 8)
	if err != nil {
		return 0, err
	}
	switch len(b) {
	case 0:
		return 0, nil
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	}
	return 0, corrupt("float element %#x at %d is %d bytes", e.ID, e.Off, len(b))
}

func (f *file) string(e elem) (string, error) {
	b, err := f.payload(e, maxString)
	if err != nil {
		return "", err
	}
	for i, c := range b {
		if c == 0 { // strings may be zero-padded
			b = b[:i]
			break
		}
	}
	return string(b), nil
}

// children calls fn for each child of the master element parent, in order.
// A child with an unknown size is passed to fn, which must return where the
// next child starts (its own end); for known sizes fn returns 0. It stops
// at the parent's end, or for an unknown-size parent at the first id that
// isTop says belongs to a higher level.
func (f *file) children(parent elem, isTop func(uint32) bool, fn func(c elem) (next int64, err error)) error {
	limit := parent.End()
	if limit < 0 {
		limit = f.size
	}
	off := parent.DataOff
	for n := 0; off < limit; n++ {
		if n >= maxChildElements {
			return corrupt("element %#x at %d has over %d children", parent.ID, parent.Off, maxChildElements)
		}
		c, err := f.header(off, limit)
		if err != nil {
			return err
		}
		if parent.Size == unknownSize && isTop != nil && isTop(c.ID) {
			return nil
		}
		next, err := fn(c)
		if err != nil {
			return err
		}
		switch {
		case c.Size != unknownSize:
			off = c.End()
		case next > off:
			off = next
		default:
			return corrupt("element %#x at %d has an unknown size where one is needed", c.ID, c.Off)
		}
	}
	return nil
}
