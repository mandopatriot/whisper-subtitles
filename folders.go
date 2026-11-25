package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Folder is one Library folder the owner approved, as the plugin sees it.
type Folder struct {
	Name string
	Path string
}

// parseFolders reads HOLYARRS_PLUGIN_FOLDERS, a JSON map of name to path,
// sorted by name so lookups are always tried in the same order.
func parseFolders(raw string) ([]Folder, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("HOLYARRS_PLUGIN_FOLDERS: %w", err)
	}
	var out []Folder
	for n, p := range m {
		if p != "" {
			out = append(out, Folder{Name: n, Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ErrNotFound means no approved folder holds both files.
var ErrNotFound = errors.New("not found in any approved folder")

// Located is a candidate's two files on disk.
type Located struct {
	Folder string
	Video  string
	Sub    string
}

// locate finds the folder that holds both the video and the subtitle, given
// their paths inside a Library folder. A path that leaves the folder, by ".."
// or by a link pointing outside it, is refused.
func locate(folders []Folder, videoRel, subRel string) (Located, error) {
	for _, rel := range []string{videoRel, subRel} {
		if !safeRel(rel) {
			return Located{}, fmt.Errorf("refused path %q: it is not inside a Library folder", rel)
		}
	}
	if len(folders) == 0 {
		return Located{}, fmt.Errorf("%w: no Library folder is approved yet; approve the folders that hold your movies and shows", ErrNotFound)
	}
	for _, f := range folders {
		v, okV := inside(f.Path, videoRel)
		s, okS := inside(f.Path, subRel)
		if okV && okS {
			return Located{Folder: f.Name, Video: v, Sub: s}, nil
		}
	}
	names := make([]string, len(folders))
	for i, f := range folders {
		names[i] = f.Name
	}
	return Located{}, fmt.Errorf("%w: %s and %s are not both in %s; approve the Library folder that holds them",
		ErrNotFound, videoRel, subRel, strings.Join(names, ", "))
}

// safeRel accepts a relative slash path that stays inside its folder.
func safeRel(rel string) bool {
	if rel == "" || strings.Contains(rel, `\`) || path.IsAbs(rel) {
		return false
	}
	c := path.Clean(rel)
	return c != "." && c != ".." && !strings.HasPrefix(c, "../")
}

// inside returns the real path of rel under base when it is a regular file
// whose real location is still under base's real location.
func inside(base, rel string) (string, bool) {
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", false
	}
	full, err := filepath.EvalSymlinks(filepath.Join(base, filepath.FromSlash(path.Clean(rel))))
	if err != nil {
		return "", false
	}
	if full != realBase && !strings.HasPrefix(full, strings.TrimRight(realBase, string(filepath.Separator))+string(filepath.Separator)) {
		return "", false
	}
	fi, err := os.Stat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return full, true
}
