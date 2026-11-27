package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Many uploads from both kinds of job: never two at once.
func TestASRQueueOneAtATime(t *testing.T) {
	q := newASRQueue()
	var now, peak atomic.Int32
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := q.acquire(context.Background(), asrPrio(i%2))
			if err != nil {
				t.Error(err)
				return
			}
			n := now.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			now.Add(-1)
			release()
			release() // a second release is harmless
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("%d uploads at once, want 1", peak.Load())
	}
}

// waitQueued waits until n uploads wait at priority p.
func waitQueued(t *testing.T, q *asrQueue, p asrPrio, n int) {
	t.Helper()
	waitFor(t, "queued uploads", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.waiting[p]) == n
	})
}

// The wait hook says what holds the slot and how many go first, and is not
// called when the slot is free.
func TestASRQueueNotesWait(t *testing.T) {
	q := newASRQueue()
	called := false
	free, err := q.acquireNoting(context.Background(), prioSync, func(asrPrio, int) { called = true })
	if err != nil || called {
		t.Fatalf("free slot: err %v, hook called %v", err, called)
	}
	free()
	hold, _ := q.acquire(context.Background(), prioMake)
	type note struct {
		holder asrPrio
		ahead  int
	}
	notes := make(chan note, 2)
	done := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			release, err := q.acquireNoting(context.Background(), prioSync, func(h asrPrio, a int) { notes <- note{h, a} })
			if err == nil {
				release()
			}
			done <- struct{}{}
		}()
		waitQueued(t, q, prioSync, i+1)
	}
	a, b := <-notes, <-notes
	if a.ahead > b.ahead {
		a, b = b, a // the hooks run outside the lock, in either order
	}
	if a != (note{prioMake, 1}) || b != (note{prioMake, 2}) {
		t.Errorf("waiters noted %+v and %+v, want a make holding with 1 and 2 first", a, b)
	}
	if prioMake.String() != "a subtitle being written" || prioSync.String() != "another sync" {
		t.Error("holder names")
	}
	hold()
	<-done
	<-done
}

// A waiting make goes before syncs that waited longer; each kind keeps its
// own order.
func TestASRQueueMakesFirst(t *testing.T) {
	q := newASRQueue()
	hold, err := q.acquire(context.Background(), prioSync)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	start := func(name string, p asrPrio, queued int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := q.acquire(context.Background(), p)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			release()
		}()
		waitQueued(t, q, p, queued)
	}
	start("sync1", prioSync, 1)
	start("sync2", prioSync, 2)
	start("make1", prioMake, 1)
	start("make2", prioMake, 2)
	hold()
	wg.Wait()
	want := []string{"make1", "make2", "sync1", "sync2"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("order %v, want %v", order, want)
		}
	}
	if q.busy {
		t.Error("slot still taken after every release")
	}
}

// A stop while waiting leaves the queue; the slot still passes on.
func TestASRQueueCancelWhileWaiting(t *testing.T) {
	q := newASRQueue()
	hold, _ := q.acquire(context.Background(), prioMake)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := q.acquire(ctx, prioMake)
		done <- err
	}()
	waitQueued(t, q, prioMake, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	waitQueued(t, q, prioMake, 0)
	hold()
	release, err := q.acquire(context.Background(), prioSync)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// The pause after a timeout holds back both kinds of upload.
func TestASRQueuePauseAppliesToBoth(t *testing.T) {
	q := newASRQueueWith(120*time.Millisecond, time.Second)
	q.timedOut()
	for _, p := range []asrPrio{prioMake, prioSync} {
		t0 := time.Now()
		release, err := q.acquire(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if d := time.Since(t0); p == prioMake && d < 80*time.Millisecond {
			t.Errorf("make upload after %s, inside the pause", d)
		}
	}
	q.timedOut() // 240 ms now
	t0 := time.Now()
	release, err := q.acquire(context.Background(), prioSync)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if d := time.Since(t0); d < 150*time.Millisecond {
		t.Errorf("sync upload after %s, inside the doubled pause", d)
	}
}
