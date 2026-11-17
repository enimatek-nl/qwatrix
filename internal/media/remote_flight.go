package media

import (
	"sync"
	"time"
)

// remoteFetchPause is how long a refusal from an origin is remembered for one file.
//
// The number is short on purpose. A media ID is fetched because a client is rendering it, so a wrong
// answer has to be retryable quickly — an origin that was restarting, or that had not yet finished
// replicating the upload, becomes fetchable a moment later. Thirty seconds is long enough to cover the
// burst a room list produces, which is the thing this exists to stop.
const remoteFetchPause = 30 * time.Second

// remoteFetchFlights keeps a room full of viewers of one remote file down to a single request to its
// origin, and remembers briefly when that origin said no.
//
// Both halves are about the same log line. A /sync that names three members with the same avatar makes
// three downloads, and a client that retries a broken image makes more; before this, each of them walked
// to the origin — three signed federation requests inside sixty milliseconds, three TLS failures when
// the origin's certificate is wrong, and a room list that asks again on every scroll for a file that is
// never going to arrive. One caller asks, the others wait for the answer, and a refusal is believed for
// [remoteFetchPause] instead of being rediscovered each time.
type remoteFetchFlights struct {
	mu      sync.Mutex
	running map[string]*remoteFetchCall
	failed  map[string]remoteFetchFailure
	pause   time.Duration
	now     func() time.Time
}

// remoteFetchCall is one in-flight fetch that other callers may wait on.
type remoteFetchCall struct {
	done chan struct{}
	err  error
}

// remoteFetchFailure is a refusal, with the moment it stops being worth repeating.
type remoteFetchFailure struct {
	err   error
	until time.Time
}

func newRemoteFetchFlights(pause time.Duration) *remoteFetchFlights {
	return &remoteFetchFlights{
		running: map[string]*remoteFetchCall{},
		failed:  map[string]remoteFetchFailure{},
		pause:   pause,
		now:     time.Now,
	}
}

// acquire decides what a caller asking for key has to do: fetch it (leader), wait for whoever else is
// fetching it, or accept that the origin refused it a moment ago (cachedErr).
func (f *remoteFetchFlights) acquire(key string) (call *remoteFetchCall, leader bool, cachedErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if failure, refused := f.failed[key]; refused {
		if f.now().Before(failure.until) {
			return nil, false, failure.err
		}
		delete(f.failed, key)
	}
	if running, ongoing := f.running[key]; ongoing {
		return running, false, nil
	}
	call = &remoteFetchCall{done: make(chan struct{})}
	f.running[key] = call
	return call, true, nil
}

// complete releases the waiters and records whether the answer was a file or a refusal.
//
// The error a leader returns is the error the waiters and the later callers in the pause window get, so
// it is already the specification's answer for a file this server could not obtain — the flight layer
// does not invent a second one.
//
// A success leaves nothing behind: the cached copy this server just wrote is the thing every later
// request finds, and a second cache entry for the same fact would be a second thing to get wrong.
func (f *remoteFetchFlights) complete(key string, call *remoteFetchCall, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if current, ongoing := f.running[key]; ongoing && current == call {
		delete(f.running, key)
	}
	call.err = err
	if err != nil {
		f.failed[key] = remoteFetchFailure{err: err, until: f.now().Add(f.pause)}
	}
	close(call.done)
}
