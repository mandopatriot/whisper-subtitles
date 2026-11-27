package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Limits on what a make keeps in the data folder.
const (
	madeKeep     = 7 * 24 * time.Hour // a finished subtitle, for a second ask
	madeMax      = 200                // finished subtitles kept at most
	workKeep     = 2 * 24 * time.Hour // the chunks of an unfinished make
	madeDirName  = "made"
	workDirName  = "work"
	chunkPattern = "chunk-%03d.json"
)

// writeFileAtomic writes dir/name through a temporary file and a rename, so
// a stop halfway leaves the old file whole.
func writeFileAtomic(dir, name string, b []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone after the rename; cleans up on failure
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// madeID names a make in the cache: the video's id and the offer's key,
// which holds the track, the language and the video's size and time.
func madeID(fileID int64, key string) string {
	h := sha256.Sum256([]byte(strconv.FormatInt(fileID, 10) + "\x00" + key))
	return hex.EncodeToString(h[:16])
}

// madeMeta is the sidecar of a finished subtitle.
type madeMeta struct {
	FileID int64     `json:"file_id"`
	Key    string    `json:"key"`
	MadeAt time.Time `json:"made_at"`
}

// madeCache keeps finished subtitles and the chunks of unfinished makes.
type madeCache struct {
	dir string // HOLYARRS_PLUGIN_DATA
	now func() time.Time
}

func newMadeCache(dataDir string) *madeCache {
	return &madeCache{dir: dataDir, now: time.Now}
}

func (c *madeCache) madeDir() string { return filepath.Join(c.dir, madeDirName) }

// get returns the finished subtitle for id, when it is kept and fresh.
func (c *madeCache) get(id string, fileID int64, key string) ([]byte, bool) {
	mb, err := os.ReadFile(filepath.Join(c.madeDir(), id+".json"))
	if err != nil {
		return nil, false
	}
	var m madeMeta
	if json.Unmarshal(mb, &m) != nil || m.FileID != fileID || m.Key != key || c.now().Sub(m.MadeAt) > madeKeep {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(c.madeDir(), id+".srt"))
	if err != nil || len(b) == 0 || len(b) > maxMadeBytes {
		return nil, false
	}
	return b, true
}

// put keeps a finished subtitle and drops the chunks it was made from.
func (c *madeCache) put(id string, fileID int64, key string, body []byte) error {
	if err := os.MkdirAll(c.madeDir(), 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(c.madeDir(), id+".srt", body); err != nil {
		return err
	}
	mb, err := json.Marshal(madeMeta{FileID: fileID, Key: key, MadeAt: c.now().UTC()})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(c.madeDir(), id+".json", mb); err != nil {
		return err
	}
	return os.RemoveAll(c.workDir(id))
}

func (c *madeCache) workDir(id string) string { return filepath.Join(c.dir, workDirName, id) }

// workMeta names what a work folder is for, so chunks are never read for
// another make.
type workMeta struct {
	FileID int64  `json:"file_id"`
	Key    string `json:"key"`
}

// chunkFile is one finished chunk: the speech it owns, in ms of the video,
// and where the next chunk starts.
type chunkFile struct {
	Start int64    `json:"start"`
	Next  int64    `json:"next"`
	Segs  []tSeg   `json:"segs"`
	Notes []string `json:"notes,omitempty"`
}

// openWork makes or checks the work folder of id. A folder left by another
// make (it can't happen short of a hash clash) is cleared.
func (c *madeCache) openWork(id string, fileID int64, key string) error {
	dir := c.workDir(id)
	want := workMeta{FileID: fileID, Key: key}
	if b, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		var m workMeta
		if json.Unmarshal(b, &m) == nil && m == want {
			now := c.now()
			_ = os.Chtimes(dir, now, now) // pruned by age since last use
			return nil
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(want)
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, "meta.json", b)
}

// chunk reads finished chunk i of id, or false.
func (c *madeCache) chunk(id string, i int) (chunkFile, bool) {
	b, err := os.ReadFile(filepath.Join(c.workDir(id), fmt.Sprintf(chunkPattern, i)))
	if err != nil {
		return chunkFile{}, false
	}
	var ch chunkFile
	if json.Unmarshal(b, &ch) != nil || ch.Next <= ch.Start {
		return chunkFile{}, false
	}
	return ch, true
}

func (c *madeCache) putChunk(id string, i int, ch chunkFile) error {
	b, err := json.Marshal(ch)
	if err != nil {
		return err
	}
	return writeFileAtomic(c.workDir(id), fmt.Sprintf(chunkPattern, i), b)
}

// prune drops finished subtitles older than madeKeep or past the newest
// madeMax, and work folders unused for workKeep.
func (c *madeCache) prune() error {
	var errs []error
	ents, err := os.ReadDir(c.madeDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	type made struct {
		id string
		at time.Time
	}
	var all []made
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".tmp-") {
			errs = append(errs, os.Remove(filepath.Join(c.madeDir(), n)))
			continue
		}
		if !strings.HasSuffix(n, ".json") {
			continue
		}
		id := strings.TrimSuffix(n, ".json")
		var m madeMeta
		b, err := os.ReadFile(filepath.Join(c.madeDir(), n))
		if err != nil || json.Unmarshal(b, &m) != nil {
			m.MadeAt = time.Time{} // unreadable: dropped as oldest
		}
		all = append(all, made{id, m.MadeAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	keep := map[string]bool{}
	for i, m := range all {
		if i < madeMax && c.now().Sub(m.at) <= madeKeep {
			keep[m.id] = true
			continue
		}
		errs = append(errs, rmQuiet(filepath.Join(c.madeDir(), m.id+".json")), rmQuiet(filepath.Join(c.madeDir(), m.id+".srt")))
	}
	// A .srt without its sidecar is never read; drop it.
	for _, e := range ents {
		if id, ok := strings.CutSuffix(e.Name(), ".srt"); ok && !keep[id] {
			errs = append(errs, rmQuiet(filepath.Join(c.madeDir(), e.Name())))
		}
	}
	works, err := os.ReadDir(filepath.Join(c.dir, workDirName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, w := range works {
		fi, err := w.Info()
		if err != nil || c.now().Sub(fi.ModTime()) > workKeep {
			errs = append(errs, os.RemoveAll(filepath.Join(c.dir, workDirName, w.Name())))
		}
	}
	return errors.Join(errs...)
}

func rmQuiet(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
