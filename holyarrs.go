package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HolyArrs talks to the HolyArrs API with the plugin's own token.
type HolyArrs struct {
	Base  string // HOLYARRS_PLUGIN_TARGET, e.g. http://127.0.0.1:6262
	Token string // HOLYARRS_PLUGIN_TOKEN, new at each start
	HTTP  *http.Client
}

// newHolyArrs checks the target address. HolyArrs is reached directly: the
// client has no proxy, so HTTP_PROXY is never used for it.
func newHolyArrs(target, token string) (*HolyArrs, error) {
	u, err := url.Parse(strings.TrimRight(target, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("HOLYARRS_PLUGIN_TARGET %q is not an http address", target)
	}
	if token == "" {
		return nil, fmt.Errorf("HOLYARRS_PLUGIN_TOKEN is empty")
	}
	return &HolyArrs{Base: u.String(), Token: token,
		HTTP: &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{Proxy: nil}}}, nil
}

// SubRow is one row of the subtitles table as the API sends it
// (subtitles.Subtitle). Its id comes from an embedded struct without a JSON
// tag, so it arrives as "ID"; JSON field names match without case.
type SubRow struct {
	ID              int64          `json:"id"`
	FileID          int64          `json:"file_id"`
	Language        string         `json:"language"`
	Forced          bool           `json:"forced"`
	HearingImpaired bool           `json:"hearing_impaired"`
	Place           string         `json:"place"`
	RelativePath    *string        `json:"relative_path"`
	Score           *int           `json:"score"`
	Origin          string         `json:"origin"`
	State           string         `json:"state"`
	Sync            map[string]any `json:"sync"`
}

// FileView is one video file of a title with its subtitles (subtitles.FileView).
type FileView struct {
	FileID       int64    `json:"file_id"`
	RelativePath string   `json:"relative_path"` // the video, inside its Library folder
	Subtitles    []SubRow `json:"subtitles"`
}

// TitleSubtitles calls GET /api/v1/titles/{id}/subtitles (library.read).
func (h *HolyArrs) TitleSubtitles(ctx context.Context, titleID int64) ([]FileView, error) {
	var out struct {
		Files []FileView `json:"files"`
	}
	err := h.do(ctx, http.MethodGet, "/api/v1/titles/"+strconv.FormatInt(titleID, 10)+"/subtitles", nil, &out)
	return out.Files, err
}

// APIError is an answer outside 2xx.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("HolyArrs answered %d: %s", e.Status, e.Msg) }

func (h *HolyArrs) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, h.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &APIError{Status: resp.StatusCode, Msg: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("reading the answer to %s %s: %w", method, path, err)
	}
	return nil
}

// SyncFix is the body of POST /api/v1/subtitles/{id}/sync (subtext.Fix):
// an offset and/or a frame-rate change, applied as ms*from/to then +offset.
type SyncFix struct {
	OffsetMS int64   `json:"offset_ms"`
	FromFPS  float64 `json:"from_fps,omitempty"`
	ToFPS    float64 `json:"to_fps,omitempty"`
}

func (f SyncFix) String() string {
	b, _ := json.Marshal(f)
	return string(b)
}

// Sync calls POST /api/v1/subtitles/{id}/sync (files.subtitles.change).
// HolyArrs keeps the old file in its holding folder, writes History and
// answers with the new subtitle row, which has a new id.
func (h *HolyArrs) Sync(ctx context.Context, subtitleID int64, fix SyncFix) (SubRow, error) {
	b, err := json.Marshal(fix)
	if err != nil {
		return SubRow{}, err
	}
	var out SubRow
	err = h.do(ctx, http.MethodPost, "/api/v1/subtitles/"+strconv.FormatInt(subtitleID, 10)+"/sync", bytes.NewReader(b), &out)
	return out, err
}
