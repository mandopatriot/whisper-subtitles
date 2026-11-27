// Command whisper-subtitles is a HolyArrs plugin with two jobs, sharing the
// owner's Whisper server one upload at a time.
//
// As a subtitle site (sources.subtitles) it offers a subtitle for each
// wanted language spoken in a tagged dialogue track, and when HolyArrs picks
// one it sends that track to Whisper in chunks and answers with an SRT.
//
// As a sync tool, when HolyArrs saves or upgrades a subtitle, it reads short
// pieces of the video's sound, asks Whisper what is said and when, fits one
// offset and an optional frame-rate change, and calls HolyArrs's own Sync
// fix when the fit is sure.
//
// It reads events on standard input, keeps its state in HOLYARRS_PLUGIN_DATA
// and stops cleanly on SIGTERM or when standard input ends.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

// checkFunc checks one located subtitle against the audio and syncs it when
// sure. It is filled in by the audio steps; tests replace it.
type checkFunc func(ctx context.Context, c Candidate, loc Located) error

// app holds the running plugin.
type app struct {
	dataDir string
	set     Settings
	ha      *HolyArrs
	folders []Folder
	check   checkFunc
	out     io.Writer
	// jobTimeout is the per-job time limit (job_timeout_minutes).
	jobTimeout time.Duration

	// retryWaits is the wait before each retry of a sync stopped by the
	// time limit or a Whisper timeout.
	retryWaits []time.Duration

	mu       sync.Mutex
	st       State
	inFlight map[Job]bool
	wake     chan struct{}

	// site is the subtitle site, nil when transcribe is off or there is no
	// Whisper address.
	site *siteServer
}

func newApp(dataDir string, set Settings, ha *HolyArrs, folders []Folder, out io.Writer) (*app, error) {
	st, err := loadState(dataDir)
	if err != nil {
		return nil, err
	}
	a := &app{dataDir: dataDir, set: set, ha: ha, folders: folders, out: out,
		st: st, inFlight: map[Job]bool{}, wake: make(chan struct{}, 1), retryWaits: retryWaits,
		jobTimeout: time.Duration(set.JobTimeoutMinutes) * time.Minute}
	// One queue for every upload: sync and transcription never reach the
	// server at once.
	q := newASRQueue()
	k := &checker{ha: ha, queue: q, dataDir: dataDir, checkOnly: set.CheckOnly, logf: a.logf, open: openVideo}
	if set.ASRURL != "" {
		w := newWhisper(set.ASRURL)
		k.asr = w
		q.watchDown(w.Ready, a.logf)
		if set.Transcribe {
			s := &searcher{set: set, folders: folders, tracks: newTrackCache(openVideo), wait: searchWait, logf: a.logf}
			mk := &maker{dataDir: dataDir, folders: folders, open: openVideo, asr: w, queue: q, cache: newMadeCache(dataDir), logf: a.logf}
			a.site = newSiteServer(ha, s, mk, a.logf, a.jobTimeout)
		}
	}
	a.check = k.check // with no address no worker runs, so it is never called
	return a, nil
}

func (a *app) logf(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Fprintf(a.out, format+"\n", args...)
}

// saveLocked writes the state; a.mu must be held.
func (a *app) saveLocked() {
	if err := a.st.save(a.dataDir); err != nil {
		fmt.Fprintf(a.out, "could not save state: %v\n", err)
	}
}

func (a *app) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// queueCheckTitles queues the check_titles list once per saved value.
func (a *app) queueCheckTitles() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.set.CheckTitlesRaw == "" || a.set.CheckTitlesRaw == a.st.CheckTitlesDone {
		return
	}
	if !a.set.Sync {
		fmt.Fprintln(a.out, "check_titles: sync is off; nothing queued")
		return
	}
	for _, id := range a.set.CheckTitles {
		if d := a.st.push(Job{TitleID: id, Manual: true}); d != nil {
			fmt.Fprintf(a.out, "queue full: dropped %s\n", d)
		}
	}
	a.st.CheckTitlesDone = a.set.CheckTitlesRaw
	fmt.Fprintf(a.out, "check_titles: queued %d titles\n", len(a.set.CheckTitles))
	a.saveLocked()
}

// handleLine reads one event line and queues its job.
func (a *app) handleLine(raw []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	before := a.st.Highest
	j, err := a.st.readEvent(raw)
	if j != nil && !a.set.Sync {
		fmt.Fprintf(a.out, "sync is off; %s not checked\n", j)
		j = nil
	}
	switch {
	case j != nil:
		if d := a.st.push(*j); d != nil {
			fmt.Fprintf(a.out, "queue full: dropped %s\n", d)
		}
		fmt.Fprintf(a.out, "queued %s\n", j)
	case errors.Is(err, errSkip):
		fmt.Fprintln(a.out, err)
	case err != nil:
		fmt.Fprintf(a.out, "%v\n", err)
	}
	if j != nil || a.st.Highest != before {
		a.saveLocked()
	}
	if j != nil {
		a.poke()
	}
}

// A sync stopped by the job time limit or a Whisper timeout is tried again
// by itself, at most maxRetries times, after each wait in retryWaits.
const maxRetries = 2

var retryWaits = []time.Duration{30 * time.Minute, 2 * time.Hour}

// next takes the oldest pending job that is due and not already running. When
// there is none, wait is how long until the next one is due (0: none is).
func (a *app) next() (j Job, ok bool, wait time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().UnixMilli()
	for _, j := range a.st.Pending {
		if a.inFlight[j] {
			continue
		}
		if d := j.NotBefore - now; d > 0 {
			if wait == 0 || time.Duration(d)*time.Millisecond < wait {
				wait = time.Duration(d) * time.Millisecond
			}
			continue
		}
		a.inFlight[j] = true
		return j, true, 0
	}
	return Job{}, false, wait
}

// finish removes a job that ran to its end and queues its retries, in one
// save, so nothing is lost or run twice. A job cut off by a stop stays queued
// for the next start, and its retries are not queued.
func (a *app) finish(j Job, stopped bool, retries []Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inFlight, j)
	if stopped {
		return
	}
	a.st.done(j)
	for _, r := range retries {
		r.Tries = j.Tries + 1
		if r.Tries > maxRetries || r.Tries > len(a.retryWaits) {
			fmt.Fprintf(a.out, "%s: not tried again, after %d retries\n", r, maxRetries)
			continue
		}
		wait := a.retryWaits[r.Tries-1]
		r.NotBefore = time.Now().Add(wait).UnixMilli()
		if d := a.st.push(r); d != nil {
			fmt.Fprintf(a.out, "queue full: dropped %s\n", d)
		}
		fmt.Fprintf(a.out, "%s: tried again in %s\n", r, wait)
	}
	a.saveLocked()
}

// worker runs jobs until ctx ends.
//
// A stopped job stays queued for the next start, so the stop must be checked
// before taking a job: otherwise next() hands the same job back at once and
// the worker spins on it forever.
func (a *app) worker(ctx context.Context) {
	for ctx.Err() == nil {
		j, ok, wait := a.next()
		if !ok {
			var due <-chan time.Time
			var t *time.Timer
			if wait > 0 {
				t = time.NewTimer(wait)
				due = t.C
			}
			select {
			case <-ctx.Done():
			case <-a.wake:
			case <-due:
			}
			if t != nil {
				t.Stop()
			}
			continue
		}
		jctx, cancel := context.WithTimeout(ctx, a.jobTimeout)
		retries := a.runJob(jctx, j)
		timedOut := errors.Is(jctx.Err(), context.DeadlineExceeded)
		cancel()
		if timedOut {
			a.logf("%s: stopped at the time limit of %s", j, a.jobTimeout)
		}
		a.finish(j, ctx.Err() != nil, retries)
		a.poke() // let another worker look at the queue
	}
}

// stoppedByLimit says whether err, or the job's ctx, means the job was cut
// off by the time limit or a Whisper timeout, rather than checked: such a
// sync is tried again. A subtitle a rule left alone never is.
func stoppedByLimit(ctx context.Context, err error) bool {
	if errors.Is(err, errRule) {
		return false
	}
	return errors.Is(err, errASRTimeout) || errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// retryOf is the job that tries c again by itself.
func retryOf(j Job, c Candidate) Job {
	return Job{EventID: j.EventID, TitleID: j.TitleID, Kind: j.Kind, FileID: c.FileID, Path: c.SubRel, Manual: j.Manual, Tries: j.Tries}
}

// runJob picks, locates and checks the subtitles of one job. It returns the
// subtitles to try again, cut off by the time limit or a Whisper timeout.
func (a *app) runJob(ctx context.Context, j Job) (retry []Job) {
	files, err := a.ha.TitleSubtitles(ctx, j.TitleID)
	if err != nil {
		a.logf("%s: %v", j, err)
		if stoppedByLimit(ctx, err) {
			retry = append(retry, j) // nothing was checked: all of it again
		}
		return retry
	}
	var cands []Candidate
	if j.wholeTitle() {
		var skipped []string
		cands, skipped = pickForTitle(files, a.set)
		for _, s := range skipped {
			a.logf("%s: %s", j, s)
		}
	} else {
		c, err := pickForEvent(files, j, a.set)
		if err != nil {
			a.logf("%s: %v", j, err)
			return
		}
		cands = []Candidate{c}
	}
	for _, c := range cands {
		if ctx.Err() != nil {
			if stoppedByLimit(ctx, nil) {
				retry = append(retry, retryOf(j, c)) // never reached
			}
			continue
		}
		c.TitleID = j.TitleID
		loc, err := locate(a.folders, c.VideoRel, c.SubRel)
		if err != nil {
			a.logf("%s: %s: %v", j, c, err)
			continue
		}
		if err := a.check(ctx, c, loc); err != nil {
			a.logf("%s: %s: %v", j, c, err)
			if stoppedByLimit(ctx, err) {
				retry = append(retry, retryOf(j, c))
			}
		}
	}
	return retry
}

// run reads events from r and runs jobs until r ends or ctx is done.
func (a *app) run(ctx context.Context, r io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.queueCheckTitles()
	var wg sync.WaitGroup
	workers := a.set.Jobs
	if a.set.ASRURL == "" || !a.set.Sync {
		workers = 0 // jobs wait in the queue until the address is set
	}
	if a.site != nil {
		wg.Add(1)
		go func() { defer wg.Done(); a.site.run(ctx) }()
	}
	for range workers {
		wg.Add(1)
		go func() { defer wg.Done(); a.worker(ctx) }()
	}
	a.poke()

	type read struct {
		line []byte
		err  error
	}
	lines := make(chan read)
	go func() {
		br := bufio.NewReader(r)
		for {
			b, err := br.ReadBytes('\n')
			select {
			case lines <- read{b, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var readErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case rd := <-lines:
			if len(rd.line) > 0 {
				a.handleLine(rd.line)
			}
			if errors.Is(rd.err, io.EOF) {
				break loop // HolyArrs is stopping the plugin
			}
			if rd.err != nil {
				readErr = rd.err
				break loop
			}
		}
	}
	cancel()
	wg.Wait()
	a.mu.Lock()
	a.saveLocked()
	a.mu.Unlock()
	return readErr
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "whisper-subtitles:", err)
		os.Exit(1)
	}
	set, warns := readSettings(os.Getenv)
	for _, w := range warns {
		fmt.Println(w)
	}
	ha, err := newHolyArrs(os.Getenv("HOLYARRS_PLUGIN_TARGET"), os.Getenv("HOLYARRS_PLUGIN_TOKEN"))
	if err != nil {
		fail(err)
	}
	folders, err := parseFolders(os.Getenv("HOLYARRS_PLUGIN_FOLDERS"))
	if err != nil {
		fail(err)
	}
	dataDir := os.Getenv("HOLYARRS_PLUGIN_DATA")
	if dataDir == "" {
		fail(errors.New("HOLYARRS_PLUGIN_DATA is empty"))
	}
	// The sandbox allows 512 MiB; reading is bounded well below this, which
	// only makes the collector work harder if something grows.
	debug.SetMemoryLimit(256 << 20)
	cleanWindowFiles(dataDir) // windows a stop left behind
	if err := newMadeCache(dataDir).prune(); err != nil {
		fmt.Println("cleaning the data folder:", err)
	}
	a, err := newApp(dataDir, set, ha, folders, os.Stdout)
	if err != nil {
		fail(err)
	}
	a.logf("whisper-subtitles started: %d folders, sync %t (%d jobs at once, check only %t, %d jobs waiting), subtitle site %t",
		len(folders), set.Sync, set.Jobs, set.CheckOnly, len(a.st.Pending), a.site != nil)
	if set.ASRURL == "" {
		a.logf("set the Whisper server address before subtitles can be checked or written")
	} else if err := newWhisper(set.ASRURL).Ready(ctx); err != nil {
		a.logf("%v; jobs wait and try when they run (add the server as one of your own hosts)", err)
	} else {
		a.logf("Whisper server %s is ready", set.ASRURL)
	}
	if err := a.run(ctx, os.Stdin); err != nil {
		fail(fmt.Errorf("reading events: %w", err))
	}
	a.logf("whisper-subtitles stopped")
}
