package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// asrProbeEvery is how often GET /asr is tried while the server can't be
// reached.
const asrProbeEvery = 30 * time.Second

// asrPrio orders uploads waiting for the Whisper server.
type asrPrio int

const (
	// prioMake is a transcription chunk: HolyArrs waits at most an hour for
	// it, and someone is usually missing that subtitle.
	prioMake asrPrio = iota
	// prioSync is a sync window: it has no deadline.
	prioSync
	nPrios
)

// asrQueue lets one upload at a time reach the Whisper server, so sync and
// transcription never share the GPU. Waiting makes go before waiting syncs,
// first come first served within each. The slot is held for one upload (a
// sync window or a make chunk), not for a whole job.
//
// It also pauses uploads after one got no answer in time: a stuck task holds
// the server's only slot, and more uploads would queue behind it. The pause
// starts at first and doubles up to max; an answer ends it.
type asrQueue struct {
	mu      sync.Mutex
	busy    bool
	holder  asrPrio // what holds the slot while busy
	waiting [nPrios][]chan struct{}

	until      time.Time
	pause      time.Duration
	first, max time.Duration

	// While the server can't be reached (errASRDown), no upload is sent:
	// one waiter tries probe every probeEvery, the others wait for it.
	// changed is closed and replaced when the server is back or the prober
	// leaves.
	probe      func(context.Context) error
	probeEvery time.Duration
	logf       func(string, ...any)
	down       bool
	downSince  time.Time
	probing    bool
	changed    chan struct{}
}

func newASRQueue() *asrQueue { return newASRQueueWith(backoffFirst, backoffMax) }

func newASRQueueWith(first, max time.Duration) *asrQueue {
	return &asrQueue{first: first, max: max, probeEvery: asrProbeEvery, changed: make(chan struct{})}
}

// watchDown makes uploads wait while the server can't be reached, trying
// probe (GET /asr) until it answers. Without it errASRDown is returned as is.
func (q *asrQueue) watchDown(probe func(context.Context) error, logf func(string, ...any)) {
	q.probe, q.logf = probe, logf
}

// send runs do, one upload, with the slot held. While the server can't be
// reached it waits until it answers and sends again; giveUp, when not nil,
// is when to stop waiting and return errASRDown.
func (q *asrQueue) send(ctx context.Context, p asrPrio, waiting func(asrPrio, int), giveUp func() time.Time, do func() error) error {
	for {
		if err := q.waitUp(ctx, giveUp); err != nil {
			return err
		}
		release, err := q.acquireNoting(ctx, p, waiting)
		if err != nil {
			return err
		}
		err = do()
		release()
		if !errors.Is(err, errASRDown) || q.probe == nil || ctx.Err() != nil {
			return err
		}
		q.wentDown(err)
	}
}

func (q *asrQueue) wentDown(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.down {
		q.down, q.downSince = true, time.Now()
		q.logf("Whisper server went away (%v); uploads wait, trying GET /asr every %s", err, q.probeEvery)
	}
}

func (q *asrQueue) cameBack() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.down {
		q.down = false
		q.logf("Whisper server is back after %s; uploads carry on", time.Since(q.downSince).Round(time.Second))
		q.signalLocked()
	}
}

func (q *asrQueue) signalLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// waitUp returns once the server is not known to be down.
func (q *asrQueue) waitUp(ctx context.Context, giveUp func() time.Time) error {
	for {
		q.mu.Lock()
		if !q.down {
			q.mu.Unlock()
			return nil
		}
		changed, since, prober := q.changed, q.downSince, !q.probing
		if prober {
			q.probing = true
		}
		q.mu.Unlock()
		err := q.waitTurn(ctx, giveUp, changed, prober, since)
		if prober {
			q.mu.Lock()
			q.probing = false
			q.signalLocked() // another waiter probes if this one left
			q.mu.Unlock()
		}
		if err != nil {
			return err
		}
	}
}

func (q *asrQueue) waitTurn(ctx context.Context, giveUp func() time.Time, changed <-chan struct{}, prober bool, since time.Time) error {
	gaveUp := func() error {
		return fmt.Errorf("%w since %s, and HolyArrs no longer waits", errASRDown, since.Format("15:04:05"))
	}
	var deadline <-chan time.Time
	if giveUp != nil {
		d := time.Until(giveUp())
		if d <= 0 {
			return gaveUp()
		}
		t := time.NewTimer(d)
		defer t.Stop()
		deadline = t.C
	}
	var tick <-chan time.Time
	if prober {
		t := time.NewTimer(q.probeEvery)
		defer t.Stop()
		tick = t.C
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-deadline:
		return gaveUp()
	case <-changed:
		return nil
	case <-tick:
		if q.probe(ctx) == nil {
			q.cameBack()
		}
		return nil
	}
}

// acquire waits for the upload slot and any pause after a timeout. The
// caller must call release once its upload is done.
func (q *asrQueue) acquire(ctx context.Context, p asrPrio) (release func(), err error) {
	return q.acquireNoting(ctx, p, nil)
}

// acquireNoting is acquire that calls waiting, before it blocks, with what
// holds the slot and how many uploads go first, counting that one. waiting
// is not called when the slot is free.
func (q *asrQueue) acquireNoting(ctx context.Context, p asrPrio, waiting func(holder asrPrio, ahead int)) (release func(), err error) {
	if err := q.take(ctx, p, waiting); err != nil {
		return nil, err
	}
	// The slot is held while a pause runs out, so the order is kept.
	if err := q.waitPause(ctx); err != nil {
		q.release()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(q.release) }, nil
}

func (q *asrQueue) take(ctx context.Context, p asrPrio, waiting func(holder asrPrio, ahead int)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	if !q.busy && q.aheadLocked(p) == 0 {
		q.busy, q.holder = true, p
		q.mu.Unlock()
		return nil
	}
	holder, ahead := q.holder, q.aheadLocked(p)
	if q.busy {
		ahead++
	}
	ch := make(chan struct{})
	q.waiting[p] = append(q.waiting[p], ch)
	q.mu.Unlock()
	if waiting != nil {
		waiting(holder, ahead)
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		q.mu.Lock()
		for i, c := range q.waiting[p] {
			if c == ch {
				q.waiting[p] = append(q.waiting[p][:i:i], q.waiting[p][i+1:]...)
				q.mu.Unlock()
				return ctx.Err()
			}
		}
		q.mu.Unlock()
		// The slot was handed over just as ctx ended: pass it on.
		q.release()
		return ctx.Err()
	}
}

// aheadLocked counts the uploads waiting that go before priority p.
func (q *asrQueue) aheadLocked(p asrPrio) int {
	n := 0
	for i := asrPrio(0); i <= p; i++ {
		n += len(q.waiting[i])
	}
	return n
}

// release hands the slot to the next waiting upload, makes first.
func (q *asrQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for p := range q.waiting {
		if len(q.waiting[p]) > 0 {
			ch := q.waiting[p][0]
			q.waiting[p] = q.waiting[p][1:]
			q.holder = asrPrio(p)
			close(ch) // busy stays true: the slot passes straight on
			return
		}
	}
	q.busy = false
}

func (q *asrQueue) waitPause(ctx context.Context) error {
	for {
		q.mu.Lock()
		d := time.Until(q.until)
		q.mu.Unlock()
		if d <= 0 {
			return nil
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (p asrPrio) String() string {
	if p == prioMake {
		return "a subtitle being written"
	}
	return "another sync"
}

// timedOut starts or doubles the pause and returns it.
func (q *asrQueue) timedOut() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pause == 0 {
		q.pause = q.first
	} else {
		q.pause = min(q.pause*2, q.max)
	}
	q.until = time.Now().Add(q.pause)
	return q.pause
}

// answered ends the pause after an upload got an answer.
func (q *asrQueue) answered() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pause = 0
}
