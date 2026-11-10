package api

import (
	"context"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// streamHub is the one poller of a Server's event streams. A stream that has
// read the log up to its head arms a waiter with its cursor and sleeps; while
// any waiter is armed the hub asks the store once per StreamPoll whether the
// log goes on after the lowest armed cursor, and wakes every armed waiter
// when it does. Each woken stream then reads its own pages, so the idle cost
// is one store call per interval however many streams are open, and nothing
// when none is.
type streamHub struct {
	store store.APIReader
	poll  time.Duration
	log   func(ctx context.Context, err error)
	// ctx ends the poller goroutine; the Server cancels it on Drain.
	ctx context.Context

	mu      sync.Mutex
	armed   map[*streamWaiter]struct{}
	running bool
}

// streamWaiter is one stream's place in the hub.
type streamWaiter struct {
	hub    *streamHub
	cursor int64         // guarded by hub.mu
	wake   chan struct{} // capacity 1: a wake-up that finds the stream busy is kept
	set    bool          // in hub.armed; guarded by hub.mu
}

func newStreamHub(ctx context.Context, st store.APIReader, poll time.Duration, log func(context.Context, error)) *streamHub {
	return &streamHub{ctx: ctx, store: st, poll: poll, log: log, armed: map[*streamWaiter]struct{}{}}
}

func (h *streamHub) newWaiter() *streamWaiter {
	return &streamWaiter{hub: h, wake: make(chan struct{}, 1)}
}

// arm records that the stream has read the log up to cursor and waits for
// news after it. It is idempotent while the waiter stays armed.
func (w *streamWaiter) arm(cursor int64) {
	h := w.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	w.cursor = cursor
	if !w.set {
		w.set = true
		h.armed[w] = struct{}{}
	}
	if !h.running {
		h.running = true
		go h.run()
	}
}

// release takes the waiter out of the hub for good.
func (w *streamWaiter) release() {
	h := w.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if w.set {
		w.set = false
		delete(h.armed, w)
	}
}

// lowest returns the lowest armed cursor, or false when nothing is armed. It
// ends the poller (running = false) in that case, under the same lock that
// arm takes, so a stream arming at that moment starts a new poller.
func (h *streamHub) lowest() (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.armed) == 0 || h.ctx.Err() != nil {
		h.running = false
		return 0, false
	}
	var low int64
	first := true
	for w := range h.armed {
		if first || w.cursor < low {
			low, first = w.cursor, false
		}
	}
	return low, true
}

func (h *streamHub) wakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for w := range h.armed {
		w.set = false
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
	clear(h.armed)
}

func (h *streamHub) run() {
	t := time.NewTicker(h.poll)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			h.mu.Lock()
			h.running = false
			h.mu.Unlock()
			return
		case <-t.C:
		}
		low, ok := h.lowest()
		if !ok {
			return
		}
		more, err := h.store.HasEventsAfter(h.ctx, low)
		if err != nil {
			if h.ctx.Err() == nil {
				h.log(h.ctx, err)
			}
			more = true // let each stream hit the error on its own read and end
		}
		if more {
			h.wakeAll()
		}
	}
}
