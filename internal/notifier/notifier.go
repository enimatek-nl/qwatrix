// Package notifier is the in-process wake-up bus for long-polling readers.
//
// It exists so that /sync can wait for something to happen instead of asking whether
// something has happened: a sync that polls the database every 200 ms makes every other
// request more expensive and adds latency to the thing it is waiting for.
//
// The design is a monotonic generation counter plus a broadcast channel. Readers remember
// the generation *before* they read the database and then wait for it to advance, which is
// race-free by construction: a publication that happens while the read is in flight has
// already incremented the counter the waiter compares against, so no wake-up can be lost.
// A spurious wake-up costs one extra database read, and a lost one is impossible — the
// right trade for a server whose sync correctness users notice immediately.
package notifier

import (
	"context"
	"sync"
	"time"
)

// Notifier publishes "something changed" and hands out waits.
type Notifier struct {
	mu     sync.Mutex
	gen    uint64
	bound  chan struct{} // closed and replaced on every publication
	closed bool
}

// New returns a notifier ready to use.
func New() *Notifier {
	return &Notifier{bound: make(chan struct{})}
}

// Publish records a change and wakes every waiter.
//
// It takes no arguments on purpose: the waiters do not consume events, they re-read state,
// so a single "something happened" is both sufficient and much harder to get wrong than a
// taxonomy of what kind of thing happened.
func (n *Notifier) Publish() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	n.gen++
	close(n.bound)
	n.bound = make(chan struct{})
}

// Generation returns the current counter, for a later [Notifier.WaitForChange].
func (n *Notifier) Generation() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.gen
}

// WaitForChange blocks until the generation advances past `after`, the context is
// cancelled, or `timeout` elapses. It reports whether the generation advanced.
func (n *Notifier) WaitForChange(ctx context.Context, after uint64, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		// The channel is read under the lock and the generation compared afterwards: a
		// publication between the two is caught by the comparison, and one afterwards
		// closes the channel this call is holding.
		n.mu.Lock()
		if n.gen > after || n.closed {
			advanced := n.gen > after
			n.mu.Unlock()
			return advanced
		}
		bound := n.bound
		n.mu.Unlock()

		select {
		case <-bound:
			// Loop: re-check the generation, because a broadcast is not a promise that
			// this waiter's own data changed.
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// Close wakes every waiter so a shutdown does not leave requests parked.
func (n *Notifier) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	n.closed = true
	close(n.bound)
}
