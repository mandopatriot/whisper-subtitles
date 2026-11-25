// Command subtitle-sync is a HolyArrs plugin. When HolyArrs saves or upgrades a
// subtitle, it reads short pieces of the video's sound, asks the owner's
// Whisper server what is said and when, fits one offset and an optional
// frame-rate change, and calls HolyArrs's own Sync fix when the fit is sure.
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

	mu       sync.Mutex
	st       State
	inFlight map[Job]bool
	wake     chan struct{}
}

func newApp(dataDir string, set Settings, ha *HolyArrs, folders []Folder, out io.Writer) (*app, error) {
	st, err := loadState(dataDir)
	if err != nil {
		return nil, err
	}
	a := &app{dataDir: dataDir, set: set, ha: ha, folders: folders, out: out,
		st: st, inFlight: map[Job]bool{}, wake: make(chan struct{}, 1),
		jobTimeout: time.Duration(set.JobTimeoutMinutes) * time.Minute}
	k := &checker{ha: ha, gate: newASRGate(), dataDir: dataDir, checkOnly: set.CheckOnly, logf: a.logf, open: openVideo}
	if set.ASRURL != "" {
		k.asr = newWhisper(set.ASRURL)
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

// next takes the oldest pending job not already running, or false.
func (a *app) next() (Job, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.st.Pending {
		if !a.inFlight[j] {
			a.inFlight[j] = true
			return j, true
		}
	}
	return Job{}, false
}

// finish removes a job that ran to its end. A job cut off by a stop stays
// queued for the next start.
func (a *app) finish(j Job, stopped bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inFlight, j)
	if !stopped {
		a.st.done(j)
		a.saveLocked()
	}
}

// worker runs jobs until ctx ends.
//
// A stopped job stays queued for the next start, so the stop must be checked
// before taking a job: otherwise next() hands the same job back at once and
// the worker spins on it forever.
func (a *app) worker(ctx context.Context) {
	for ctx.Err() == nil {
		j, ok := a.next()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-a.wake:
				continue
			}
		}
		jctx, cancel := context.WithTimeout(ctx, a.jobTimeout)
		a.runJob(jctx, j)
		timedOut := errors.Is(jctx.Err(), context.DeadlineExceeded)
		cancel()
		if timedOut {
			a.logf("%s: stopped at the time limit of %s", j, a.jobTimeout)
		}
		a.finish(j, ctx.Err() != nil)
		a.poke() // let another worker look at the queue
	}
}

// runJob picks, locates and checks the subtitles of one job.
func (a *app) runJob(ctx context.Context, j Job) {
	files, err := a.ha.TitleSubtitles(ctx, j.TitleID)
	if err != nil {
		a.logf("%s: %v", j, err)
		return
	}
	var cands []Candidate
	if j.Manual {
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
			return
		}
		c.TitleID = j.TitleID
		loc, err := locate(a.folders, c.VideoRel, c.SubRel)
		if err != nil {
			a.logf("%s: %s: %v", j, c, err)
			continue
		}
		if err := a.check(ctx, c, loc); err != nil {
			a.logf("%s: %s: %v", j, c, err)
		}
	}
}

// run reads events from r and runs jobs until r ends or ctx is done.
func (a *app) run(ctx context.Context, r io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.queueCheckTitles()
	var wg sync.WaitGroup
	workers := a.set.Jobs
	if a.set.ASRURL == "" {
		workers = 0 // jobs wait in the queue until the address is set
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
		fmt.Fprintln(os.Stderr, "subtitle-sync:", err)
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
	a, err := newApp(dataDir, set, ha, folders, os.Stdout)
	if err != nil {
		fail(err)
	}
	a.logf("subtitle-sync started: %d folders, %d jobs at once, check only %t, %d jobs waiting",
		len(folders), set.Jobs, set.CheckOnly, len(a.st.Pending))
	if set.ASRURL == "" {
		a.logf("set the Whisper server address before subtitles can be checked")
	} else if err := newWhisper(set.ASRURL).Ready(ctx); err != nil {
		a.logf("%v; jobs wait and try when they run (add the server as one of your own hosts)", err)
	} else {
		a.logf("Whisper server %s is ready", set.ASRURL)
	}
	if err := a.run(ctx, os.Stdin); err != nil {
		fail(fmt.Errorf("reading events: %w", err))
	}
	a.logf("subtitle-sync stopped")
}
