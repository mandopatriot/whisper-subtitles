package main

import (
	"context"
	"sync"
	"time"
)

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
}

func newASRQueue() *asrQueue { return newASRQueueWith(backoffFirst, backoffMax) }

func newASRQueueWith(first, max time.Duration) *asrQueue {
	return &asrQueue{first: first, max: max}
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
