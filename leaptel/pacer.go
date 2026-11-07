package leaptel

// The pacer. Outage reads are limited upstream to 10 a minute (429 "Too Many
// Attempts." past it, with no Retry-After), and assurance tests are throttled
// after about as many. Nobody knows yet whether those buckets are one, or
// shared with writes, and a 429 on a write trips the safety stop and holds
// every order. So outage reads and every assurance call (runs, polls, history
// and the catalogue) go through one pacer, at no more than half the limit the
// upstream states: 5 calls in any 60 s.
//
// Callers queue by priority, highest first, FIFO within one: triggered checks
// and staff refreshes, then assurance, refresh, the incremental read, location
// checks and the sweep. A 429 blocks every paced call for 60 s, then halves
// the share for 10 min. Nothing is retried here: the caller gets
// ErrRateLimited and tries again on its next tick.
//
// The pacer is per process; one instance is shared by every copy of a client.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Priority orders paced calls; the lowest value goes first.
type Priority int

const (
	PriorityTriggered   Priority = iota // a triggered check, or staff asking now
	PriorityAssurance                   // an assurance test's run, poll or catalogue
	PriorityRefresh                     // re-reading a record we hold
	PriorityIncremental                 // the national list's new records
	PriorityLocation                    // a check for services a record's location names
	PrioritySweep                       // the background per-service sweep
	numPriorities
)

// DefaultPacedPerMinute is the paced share: half the upstream's stated 10.
const DefaultPacedPerMinute = 5

const (
	pacerWindow  = time.Minute      // the sliding window the share counts over
	pacerBackoff = time.Minute      // nothing paced goes for this long after a 429
	pacerHalved  = 10 * time.Minute // then the share is halved for this long
)

// ErrRateLimited is a paced call the upstream answered 429. It was recorded,
// isn't retried, and never trips the stop.
var ErrRateLimited = errors.New("leaptel: rate limited")

// ErrPacerBusy is a paced call whose context ends before the earliest slot it
// could have: it isn't queued. It's a context.DeadlineExceeded too.
var ErrPacerBusy = fmt.Errorf("leaptel: no paced slot before the deadline: %w", context.DeadlineExceeded)

type priorityKey struct{}

// WithPriority returns ctx carrying the priority its paced calls queue at.
// Without one, a call gets its route's default: assurance for assurance
// calls, refresh for outage reads.
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityKey{}, p)
}

// priorityOf is req's priority: its context's, else its route's default.
func priorityOf(req *http.Request) Priority {
	if p, ok := req.Context().Value(priorityKey{}).(Priority); ok {
		return p
	}
	if strings.Contains(req.URL.Path, "assurance-tests") {
		return PriorityAssurance
	}
	return PriorityRefresh
}

// isPaced reports whether a call goes through the pacer: an outage read, or
// any assurance call (path relative to the base URL, query included).
func isPaced(method, path string) bool {
	if isOutageRead(method, path) {
		return true
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return strings.Contains(path, "assurance-tests")
}

// WithPacer returns a client whose outage reads and assurance calls wait
// their turn in p. Every copy made from it shares p.
func (c *Client) WithPacer(p *Pacer) *Client {
	cp := *c
	cp.transport().pacer = p
	return &cp
}

// Pacer holds paced calls to a share of the upstream's per-minute limit.
type Pacer struct {
	now       func() time.Time
	perMinute int

	mu           sync.Mutex
	perWindow    int         // the share: min(perMinute, the stated limit / 2)
	starts       []time.Time // when each call in the window was let go, oldest first
	blockedUntil time.Time
	halvedUntil  time.Time
	queues       [numPriorities][]*pacerWaiter
	timer        *time.Timer
	timerAt      time.Time
	timerGen     uint64 // which timer is current, so a stale one firing can't clear it

	rateLimited atomic.Uint64
}

type pacerWaiter struct {
	ready   chan struct{}
	granted bool
	at      time.Time // the slot it took, once granted
}

// NewPacer returns a pacer letting at most perMinute calls go in any 60 s.
func NewPacer(perMinute int) *Pacer { return newPacerClock(perMinute, time.Now) }

// newPacerClock is NewPacer on a clock tests move by hand (calling wake).
func newPacerClock(perMinute int, now func() time.Time) *Pacer {
	perMinute = max(perMinute, 1)
	return &Pacer{now: now, perMinute: perMinute, perWindow: perMinute}
}

// Wait blocks until a call at priority pr may go, or ctx is done. A call that
// gets a slot uses it: it must be sent. One whose ctx has a deadline before
// the earliest slot it could get returns ErrPacerBusy at once.
func (p *Pacer) Wait(ctx context.Context, pr Priority) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pr = min(max(pr, PriorityTriggered), PrioritySweep)
	w := &pacerWaiter{ready: make(chan struct{})}
	p.mu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		// Durations, not instants: the pacer's clock may not be the wall's.
		now := p.now()
		if time.Until(dl) < p.earliestLocked(pr, now).Sub(now) {
			p.mu.Unlock()
			return ErrPacerBusy
		}
	}
	p.queues[pr] = append(p.queues[pr], w)
	p.dispatchLocked()
	p.mu.Unlock()
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if w.granted { // granted as ctx ended: hand the slot back
		for i, at := range p.starts {
			if at.Equal(w.at) {
				p.starts = append(p.starts[:i], p.starts[i+1:]...)
				break
			}
		}
	} else {
		q := p.queues[pr]
		for i, x := range q {
			if x == w {
				p.queues[pr] = append(q[:i:i], q[i+1:]...)
				break
			}
		}
	}
	p.dispatchLocked()
	return ctx.Err()
}

// Observe takes in a paced call's outcome: its status and its
// x-ratelimit-limit header ("" when absent).
func (p *Pacer) Observe(status int, limitHeader string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, err := strconv.Atoi(strings.TrimSpace(limitHeader)); err == nil && n > 0 {
		p.perWindow = min(p.perMinute, max(n/2, 1))
	}
	if status == http.StatusTooManyRequests {
		p.rateLimited.Add(1)
		p.blockedUntil = p.now().Add(pacerBackoff)
		p.halvedUntil = p.blockedUntil.Add(pacerHalved)
	}
	p.dispatchLocked()
}

// RateLimited is how many paced calls have been answered 429.
func (p *Pacer) RateLimited() uint64 { return p.rateLimited.Load() }

// dispatchLocked lets go as many waiters as the window allows, highest
// priority first, and sets the timer for when the next can go.
func (p *Pacer) dispatchLocked() {
	now := p.now()
	for len(p.starts) > 0 && !now.Before(p.starts[0].Add(pacerWindow)) {
		p.starts = p.starts[1:]
	}
	if p.queuedLocked() == 0 {
		p.stopTimerLocked()
		return
	}
	if now.Before(p.blockedUntil) {
		p.scheduleLocked(p.blockedUntil, now)
		return
	}
	limit := p.perWindow
	if now.Before(p.halvedUntil) {
		limit = max(limit/2, 1)
	}
	for len(p.starts) < limit {
		w := p.popLocked()
		if w == nil {
			break
		}
		w.granted, w.at = true, now
		p.starts = append(p.starts, now)
		close(w.ready)
	}
	if p.queuedLocked() == 0 {
		p.stopTimerLocked()
		return
	}
	next := p.starts[0].Add(pacerWindow) // the window is full, so it isn't empty
	if now.Before(p.halvedUntil) && p.halvedUntil.Before(next) {
		next = p.halvedUntil
	}
	p.scheduleLocked(next, now)
}

// earliestLocked is the soonest a call queued now at pr could go, at the
// latest of: the end of a 429's back-off, and the window freeing a slot for
// it behind every call already waiting at pr or above. It's a lower bound
// (the share counted unhalved, nothing queued after it): the call may well
// go later, never sooner.
func (p *Pacer) earliestLocked(pr Priority, now time.Time) time.Time {
	at := now
	if now.Before(p.blockedUntil) {
		at = p.blockedUntil
	}
	ahead := 0
	for q := PriorityTriggered; q <= pr; q++ {
		ahead += len(p.queues[q])
	}
	var live []time.Time
	for _, s := range p.starts {
		if now.Before(s.Add(pacerWindow)) {
			live = append(live, s)
		}
	}
	// It takes the (ahead+1)th slot to come: free now while the window has
	// room, else when the call that many places back in the window leaves it.
	if over := len(live) + ahead - p.perWindow; over >= 0 {
		free := now
		if over < len(live) {
			free = live[over].Add(pacerWindow)
		} else if len(live) > 0 {
			free = live[len(live)-1].Add(pacerWindow)
		}
		if free.After(at) {
			at = free
		}
	}
	return at
}

func (p *Pacer) popLocked() *pacerWaiter {
	for pr := range p.queues {
		if q := p.queues[pr]; len(q) > 0 {
			p.queues[pr] = q[1:]
			return q[0]
		}
	}
	return nil
}

func (p *Pacer) queuedLocked() int {
	n := 0
	for _, q := range p.queues {
		n += len(q)
	}
	return n
}

// scheduleLocked sets the one timer to run dispatch again at at.
func (p *Pacer) scheduleLocked(at, now time.Time) {
	if p.timer != nil && p.timerAt.Equal(at) {
		return
	}
	p.stopTimerLocked()
	p.timerGen++
	gen := p.timerGen
	p.timerAt = at
	p.timer = time.AfterFunc(max(at.Sub(now), time.Millisecond), func() { p.fire(gen) })
}

// fire is the timer's callback: dispatch again.
func (p *Pacer) fire(gen uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen == p.timerGen {
		p.timer, p.timerAt = nil, time.Time{}
	}
	p.dispatchLocked()
}

func (p *Pacer) stopTimerLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer, p.timerAt = nil, time.Time{}
	}
}

// --- for tests ---

// wake re-runs dispatch, as the timer would, after a test moves its clock.
func (p *Pacer) wake() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dispatchLocked()
}

func (p *Pacer) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopTimerLocked()
}

func (p *Pacer) queued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queuedLocked()
}

func (p *Pacer) share() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.perWindow
}

func (p *Pacer) inWindow() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	n := 0
	for _, at := range p.starts {
		if now.Before(at.Add(pacerWindow)) {
			n++
		}
	}
	return n
}
