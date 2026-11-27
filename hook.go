package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The subtitle site hook (sources.subtitles): the plugin long-polls for jobs
// and answers each one. A plugin that has not polled in 90 seconds is not a
// live site, so polling never waits on work.
const (
	subtitleHookPath = "/api/v1/plugin-hooks/subtitles"
	maxQueuedMakes   = 20
	answerTries      = 3
	maxHookJobBytes  = 1 << 20
	maxAnswerError   = 300
	// makeWait is how long HolyArrs waits for a make's answer.
	makeWait = time.Hour
)

// hookJob is one job from the hook.
type hookJob struct {
	ID      string          `json:"id"`
	Op      string          `json:"op"`
	Payload json.RawMessage `json:"payload"`
}

// madeAnswer is a made subtitle; Body goes out as base64.
type madeAnswer struct {
	Format string `json:"format"`
	Body   []byte `json:"body"`
}

// NextSubtitleJob waits for the next subtitle site job. HolyArrs answers
// within 25 seconds; false means none came.
func (h *HolyArrs) NextSubtitleJob(ctx context.Context) (hookJob, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Base+subtitleHookPath, nil)
	if err != nil {
		return hookJob{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return hookJob{}, false, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxHookJobBytes+1))
	if err != nil {
		return hookJob{}, false, err
	}
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return hookJob{}, false, nil
	case resp.StatusCode != http.StatusOK:
		msg := strings.TrimSpace(string(b))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return hookJob{}, false, &APIError{Status: resp.StatusCode, Msg: clip(msg, 300)}
	case len(b) > maxHookJobBytes:
		return hookJob{}, false, fmt.Errorf("a subtitle site job over %d bytes", maxHookJobBytes)
	}
	var j hookJob
	if err := json.Unmarshal(b, &j); err != nil || j.ID == "" {
		return hookJob{}, false, fmt.Errorf("a subtitle site job that can't be read: %q", clip(string(b), 200))
	}
	return j, true, nil
}

// AnswerSubtitleJob sends data, or errMsg when it is set, for job id.
func (h *HolyArrs) AnswerSubtitleJob(ctx context.Context, id string, data any, errMsg string) error {
	body := map[string]any{"job": id}
	if errMsg != "" {
		body["error"] = clip(errMsg, maxAnswerError)
	} else {
		body["data"] = data
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return h.do(ctx, http.MethodPost, subtitleHookPath, bytes.NewReader(b), nil)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// makeTask is one subtitle to write, and every job waiting for it.
type makeTask struct {
	id   string
	req  makeReq
	jobs []string
	// lastAsk is when HolyArrs last asked for it; it waits makeWait from then.
	lastAsk time.Time
}

// siteServer is the plugin as a subtitle site.
type siteServer struct {
	ha         *HolyArrs
	search     *searcher
	mk         *maker
	logf       func(format string, args ...any)
	jobTimeout time.Duration

	// Waits, shortened by tests.
	authWait     time.Duration // after 401 or 403
	firstBackoff time.Duration // after another poll error, doubling
	maxBackoff   time.Duration // up to this
	answerWait   time.Duration // between answer tries
	makeWait     time.Duration // HolyArrs's wait for a make

	mu      sync.Mutex
	pending []*makeTask
	byID    map[string]*makeTask // pending or running, by madeID
	wake    chan struct{}
	answers sync.WaitGroup
}

func newSiteServer(ha *HolyArrs, s *searcher, mk *maker, logf func(string, ...any), jobTimeout time.Duration) *siteServer {
	return &siteServer{ha: ha, search: s, mk: mk, logf: logf, jobTimeout: jobTimeout,
		authWait: time.Minute, firstBackoff: time.Second, maxBackoff: 30 * time.Second, answerWait: 2 * time.Second, makeWait: makeWait,
		byID: map[string]*makeTask{}, wake: make(chan struct{}, 1)}
}

// run polls for jobs and writes subtitles until ctx ends.
func (s *siteServer) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.worker(ctx) }()
	s.poll(ctx)
	wg.Wait()
	s.answers.Wait()
}

func (s *siteServer) poll(ctx context.Context) {
	backoff := s.firstBackoff
	refused := false
	for ctx.Err() == nil {
		j, ok, err := s.ha.NextSubtitleJob(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var ae *APIError
			if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
				if !refused {
					s.logf("HolyArrs refused the subtitle site hook (%v); approve Write subtitles from the audio for this plugin. Trying again every %s", err, s.authWait)
					refused = true
				}
				sleepCtx(ctx, s.authWait)
				continue
			}
			s.logf("asking HolyArrs for subtitle site jobs: %v; trying again in %s", err, backoff)
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, s.maxBackoff)
			continue
		}
		if refused {
			s.logf("HolyArrs accepts the subtitle site hook again")
		}
		backoff, refused = s.firstBackoff, false
		if ok {
			s.dispatch(ctx, j)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// dispatch hands a job on and returns at once, so polling goes on.
func (s *siteServer) dispatch(ctx context.Context, j hookJob) {
	switch j.Op {
	case "search":
		var ask searchAsk
		if err := json.Unmarshal(j.Payload, &ask); err != nil {
			s.answer(ctx, j.ID, nil, "the search could not be read")
			return
		}
		s.answers.Add(1)
		go func() {
			defer s.answers.Done()
			offers := s.search.search(ctx, ask)
			s.sendNow(ctx, j.ID, offers, "")
		}()
	case "make":
		var r makeReq
		if err := json.Unmarshal(j.Payload, &r); err != nil || r.FileID <= 0 || r.Key == "" {
			s.answer(ctx, j.ID, nil, "the make could not be read")
			return
		}
		s.enqueue(ctx, j.ID, r)
	default:
		s.answer(ctx, j.ID, nil, fmt.Sprintf("this plugin does not know the %q job", j.Op))
	}
}

// enqueue answers a make from the cache, joins it to the same make already
// waiting or running, or queues it.
func (s *siteServer) enqueue(ctx context.Context, jobID string, r makeReq) {
	if b, ok := s.mk.cached(r); ok {
		s.logf("file %d, %s: answered from a subtitle written before", r.FileID, r.Language)
		s.answer(ctx, jobID, madeAnswer{Format: "srt", Body: b}, "")
		return
	}
	id := madeID(r.FileID, r.Key)
	s.mu.Lock()
	if t := s.byID[id]; t != nil {
		t.jobs = append(t.jobs, jobID)
		t.lastAsk = time.Now()
		s.mu.Unlock()
		s.logf("file %d, %s: already being written; this ask waits for it", r.FileID, r.Language)
		return
	}
	if n := len(s.byID); n >= maxQueuedMakes {
		s.mu.Unlock()
		s.answer(ctx, jobID, nil, fmt.Sprintf("busy, %d subtitles already queued", n))
		return
	}
	s.byID[id] = &makeTask{id: id, req: r, jobs: []string{jobID}, lastAsk: time.Now()}
	s.pending = append(s.pending, s.byID[id])
	n := len(s.pending)
	s.mu.Unlock()
	s.logf("file %d, %s: queued to write from the audio (%d waiting)", r.FileID, r.Language, n)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *siteServer) next() *makeTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	t := s.pending[0]
	s.pending = s.pending[1:]
	return t
}

// worker writes queued subtitles one at a time.
func (s *siteServer) worker(ctx context.Context) {
	for ctx.Err() == nil {
		t := s.next()
		if t == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		mctx, cancel := context.WithTimeout(ctx, s.jobTimeout)
		mctx = withGiveUp(mctx, func() time.Time {
			s.mu.Lock()
			defer s.mu.Unlock()
			return t.lastAsk.Add(s.makeWait)
		})
		body, err := s.mk.make(mctx, t.req)
		timedOut := errors.Is(mctx.Err(), context.DeadlineExceeded)
		cancel()
		s.mu.Lock()
		jobs := t.jobs
		delete(s.byID, t.id)
		s.mu.Unlock()
		if ctx.Err() != nil {
			// Stopping: the finished chunks stay, and HolyArrs asks again
			// once the plugin is back.
			return
		}
		what := fmt.Sprintf("file %d, %s", t.req.FileID, t.req.Language)
		if err != nil {
			msg := err.Error()
			if timedOut {
				msg = fmt.Sprintf("stopped at the time limit of %s", s.jobTimeout)
			}
			s.logf("%s: no subtitle written: %s", what, msg)
			for _, j := range jobs {
				s.answer(ctx, j, nil, msg)
			}
		} else {
			for _, j := range jobs {
				s.answer(ctx, j, madeAnswer{Format: "srt", Body: body}, "")
			}
		}
		if err := s.mk.cache.prune(); err != nil {
			s.logf("cleaning the data folder: %v", err)
		}
	}
}

// answer sends an answer without holding up the caller.
func (s *siteServer) answer(ctx context.Context, jobID string, data any, errMsg string) {
	s.answers.Add(1)
	go func() {
		defer s.answers.Done()
		s.sendNow(ctx, jobID, data, errMsg)
	}()
}

// sendNow sends an answer, trying again on a network or server error. A job
// HolyArrs no longer waits for (404) is only logged: a finished subtitle
// stays cached for the next ask.
func (s *siteServer) sendNow(ctx context.Context, jobID string, data any, errMsg string) {
	for try := 1; ; try++ {
		err := s.ha.AnswerSubtitleJob(ctx, jobID, data, errMsg)
		if err == nil || ctx.Err() != nil {
			return
		}
		var ae *APIError
		if errors.As(err, &ae) {
			if ae.Status == http.StatusNotFound {
				s.logf("job %s: HolyArrs no longer waits for this answer (%s)", jobID, ae.Msg)
				return
			}
			if ae.Status < 500 {
				s.logf("job %s: HolyArrs refused the answer: %v", jobID, err)
				return
			}
		}
		if try == answerTries {
			s.logf("job %s: could not send the answer after %d tries: %v", jobID, try, err)
			return
		}
		sleepCtx(ctx, s.answerWait)
	}
}
