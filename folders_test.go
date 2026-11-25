package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFolders(t *testing.T) {
	f, err := parseFolders(`{"shows":"/plugin/folders/shows","movies":"/plugin/folders/movies","empty":""}`)
	if err != nil || len(f) != 2 || f[0].Name != "movies" || f[1].Name != "shows" {
		t.Errorf("got %+v, %v; want movies then shows", f, err)
	}
	if f, err := parseFolders(""); err != nil || f != nil {
		t.Errorf("empty: %+v, %v", f, err)
	}
	if _, err := parseFolders("{"); err == nil {
		t.Error("broken JSON must be an error")
	}
}

func TestLocate(t *testing.T) {
	root := t.TempDir()
	movies, shows := filepath.Join(root, "movies"), filepath.Join(root, "shows")
	touch(t, filepath.Join(shows, "S (2023)/Season 01/S - S01E01.mkv"))
	touch(t, filepath.Join(shows, "S (2023)/Season 01/S - S01E01.en.srt"))
	touch(t, filepath.Join(movies, "Only Video (2020)/Only Video (2020).mkv"))
	os.MkdirAll(movies, 0o755)
	folders := []Folder{{"movies", movies}, {"shows", shows}}

	loc, err := locate(folders, "S (2023)/Season 01/S - S01E01.mkv", "S (2023)/Season 01/S - S01E01.en.srt")
	if err != nil || loc.Folder != "shows" || !strings.HasSuffix(loc.Video, "S - S01E01.mkv") || !strings.HasSuffix(loc.Sub, "S - S01E01.en.srt") {
		t.Errorf("second folder: %+v, %v", loc, err)
	}
	// Both files must be in the same folder.
	if _, err := locate(folders, "Only Video (2020)/Only Video (2020).mkv", "Only Video (2020)/Only Video (2020).en.srt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing subtitle: %v", err)
	}
	if _, err := locate(nil, "a.mkv", "a.srt"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "approve") {
		t.Errorf("no folders: %v", err)
	}
	// A folder, not a file.
	if _, err := locate(folders, "S (2023)/Season 01", "S (2023)/Season 01/S - S01E01.en.srt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("directory as video: %v", err)
	}
}

func TestLocateRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "lib")
	touch(t, filepath.Join(root, "secret.mkv"))
	touch(t, filepath.Join(root, "secret.srt"))
	touch(t, filepath.Join(lib, "A/A.mkv"))
	touch(t, filepath.Join(lib, "A/A.srt"))
	folders := []Folder{{"movies", lib}}
	for _, rel := range []string{"../secret.mkv", "A/../../secret.mkv", "/etc/passwd", `A\..\..\secret.mkv`, "..", ".", ""} {
		if _, err := locate(folders, rel, "A/A.srt"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v, want refused", rel, err)
		}
	}
	// A link inside the folder pointing outside it is not followed.
	if err := os.Symlink(filepath.Join(root, "secret.mkv"), filepath.Join(lib, "A/link.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, err := locate(folders, "A/link.mkv", "A/A.srt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("link out of the folder: %v, want not found", err)
	}
	// A link that stays inside is fine.
	os.Symlink(filepath.Join(lib, "A/A.mkv"), filepath.Join(lib, "A/inner.mkv"))
	if _, err := locate(folders, "A/inner.mkv", "A/A.srt"); err != nil {
		t.Errorf("link inside the folder: %v", err)
	}
	// The approved folder itself may be a link (as in a container mount).
	os.Symlink(lib, filepath.Join(root, "liblink"))
	if _, err := locate([]Folder{{"movies", filepath.Join(root, "liblink")}}, "A/A.mkv", "A/A.srt"); err != nil {
		t.Errorf("folder reached through a link: %v", err)
	}
}
