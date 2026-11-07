package leaptel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClock is a clock the tests move by hand; advance re-runs the pacer as
// its timer would.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func newTestPacer(t *testing.T, perMinute int) (*Pacer, func(time.Duration)) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)}
	p := newPacerClock(perMinute, clk.now)
	t.Cleanup(p.stop)
	return p, func(d time.Duration) {
		clk.mu.Lock()
		clk.t = clk.t.Add(d)
		clk.mu.Unlock()
		p.wake()
	}
}

// waitAsync starts a Wait and returns its result channel once the waiter is
// queued or done, so the order of calls is the order of queueing.
func waitAsync(t *testing.T, p *Pacer, ctx context.Context, pr Priority) <-chan error {
	t.Helper()
	before := p.queued()
	ch := make(chan error, 1)
	go func() { ch <- p.Wait(ctx, pr) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(ch) > 0 || p.queued() > before {
			return ch
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Wait neither returned nor queued")
	return nil
}

func mustReturn(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: still blocked", what)
	}
}

func mustBlock(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s: returned (%v), want it blocked", what, err)
	case <-time.After(30 * time.Millisecond):
	}
}

// mustHold checks a Wait at pr is held, then cancels it.
func mustHold(t *testing.T, p *Pacer, pr Priority, what string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := waitAsync(t, p, ctx, pr)
	mustBlock(t, ch, what)
	cancel()
	if err := <-ch; !errors.Is(err, context.Canceled) {
		t.Fatalf("%s: %v after cancel", what, err)
	}
}

// takeN waits n times at pr, each of which must return at once.
func takeN(t *testing.T, p *Pacer, n int, pr Priority) {
	t.Helper()
	for i := 0; i < n; i++ {
		mustReturn(t, waitAsync(t, p, context.Background(), pr), "slot")
	}
}

func TestPacerCapsAtFivePerMinute(t *testing.T) {
	p, advance := newTestPacer(t, DefaultPacedPerMinute)
	takeN(t, p, 5, PriorityRefresh)
	sixth := waitAsync(t, p, context.Background(), PriorityRefresh)
	mustBlock(t, sixth, "6th call")
	advance(59 * time.Second)
	mustBlock(t, sixth, "6th call at 59 s")
	advance(time.Second)
	mustReturn(t, sixth, "6th call at 60 s")
	// The first five, all at 0 s, have left the window: four more go, then
	// the 11th waits on the 6th.
	takeN(t, p, 4, PriorityRefresh)
	mustHold(t, p, PriorityRefresh, "11th call")
}

func TestPacerServesHigherPriorityFirst(t *testing.T) {
	p, advance := newTestPacer(t, DefaultPacedPerMinute)
	takeN(t, p, 1, PriorityRefresh)
	advance(time.Second)
	takeN(t, p, 4, PriorityRefresh)
	sweep := waitAsync(t, p, context.Background(), PrioritySweep)
	triggered := waitAsync(t, p, context.Background(), PriorityTriggered)
	mustBlock(t, sweep, "sweep")
	mustBlock(t, triggered, "triggered")
	advance(59 * time.Second) // only the first call leaves the window
	mustReturn(t, triggered, "triggered")
	mustBlock(t, sweep, "sweep, with no slot")
	advance(time.Second)
	mustReturn(t, sweep, "sweep, a slot later")
}

func TestPacerFIFOWithinAPriority(t *testing.T) {
	p, advance := newTestPacer(t, 1)
	takeN(t, p, 1, PriorityRefresh)
	first := waitAsync(t, p, context.Background(), PriorityIncremental)
	second := waitAsync(t, p, context.Background(), PriorityIncremental)
	advance(time.Minute)
	mustReturn(t, first, "first queued")
	mustBlock(t, second, "second queued")
	advance(time.Minute)
	mustReturn(t, second, "second queued, a minute later")
}

func TestPacerBacksOffThenHalves(t *testing.T) {
	p, advance := newTestPacer(t, DefaultPacedPerMinute)
	p.Observe(http.StatusTooManyRequests, "10")
	if got := p.RateLimited(); got != 1 {
		t.Fatalf("RateLimited %d, want 1", got)
	}
	blocked := waitAsync(t, p, context.Background(), PriorityTriggered)
	mustBlock(t, blocked, "during the back-off")
	advance(59 * time.Second)
	mustBlock(t, blocked, "at 59 s")
	advance(time.Second)
	mustReturn(t, blocked, "at 60 s")
	// Halved for 10 min: 2 a window.
	for minute := 0; minute < 10; minute++ {
		if minute > 0 {
			advance(time.Minute)
			takeN(t, p, 2, PriorityRefresh)
		} else {
			takeN(t, p, 1, PriorityRefresh)
		}
		mustHold(t, p, PriorityTriggered, "a 3rd call while halved")
	}
	advance(time.Minute) // 11 min after the 429: the half share is over
	takeN(t, p, 5, PriorityRefresh)
	mustHold(t, p, PriorityRefresh, "a 6th call after recovery")
}

func TestPacerLearnsLimitFromHeader(t *testing.T) {
	p, _ := newTestPacer(t, DefaultPacedPerMinute)
	for _, c := range []struct {
		header string
		want   int
	}{
		{"6", 3},
		{"", 3},
		{"junk", 3},
		{"100", 5}, // never above perMinute
		{"1", 1},   // never below 1
		{" 10 ", 5},
	} {
		p.Observe(http.StatusOK, c.header)
		if got := p.share(); got != c.want {
			t.Fatalf("after %q: share %d, want %d", c.header, got, c.want)
		}
	}
	p.Observe(http.StatusOK, "6")
	takeN(t, p, 3, PriorityRefresh)
	mustHold(t, p, PriorityRefresh, "a 4th call at share 3")
	if p.RateLimited() != 0 {
		t.Fatal("a 200 counted as rate limited")
	}
}

func TestPacerHonoursContextCancel(t *testing.T) {
	p, advance := newTestPacer(t, 1)
	takeN(t, p, 1, PriorityRefresh)
	ctx, cancel := context.WithCancel(context.Background())
	ch := waitAsync(t, p, ctx, PrioritySweep)
	mustBlock(t, ch, "before cancel")
	cancel()
	select {
	case err := <-ch:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("still blocked after cancel")
	}
	if n := p.queued(); n != 0 {
		t.Fatalf("%d still queued after cancel", n)
	}
	// The cancelled waiter took no slot.
	advance(time.Minute)
	takeN(t, p, 1, PriorityRefresh)
	if err := p.Wait(ctx, PriorityRefresh); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait on a done context: %v", err)
	}
}

func TestPacerFailsFastPastDeadline(t *testing.T) {
	p, advance := newTestPacer(t, DefaultPacedPerMinute)
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		t.Cleanup(cancel)
		return ctx
	}
	takeN(t, p, 2, PriorityRefresh)
	advance(10 * time.Second)
	takeN(t, p, 3, PriorityRefresh)
	// The window is full until 50 s from now: a call that must go within 25 s
	// isn't queued.
	start := time.Now()
	if err := p.Wait(short(), PriorityTriggered); !errors.Is(err, ErrPacerBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a full window: %v, want ErrPacerBusy", err)
	}
	if time.Since(start) > time.Second || p.queued() != 0 {
		t.Fatalf("it waited %s, %d queued", time.Since(start), p.queued())
	}
	// 30 s on, the first two leave in 20 s: within the deadline, so it waits.
	advance(30 * time.Second)
	ch := waitAsync(t, p, short(), PriorityTriggered)
	mustBlock(t, ch, "a slot within the deadline")
	// A second behind it would get the second slot, also at 20 s; a third
	// would need the 10 s call's, at 30 s.
	mustBlock(t, waitAsync(t, p, short(), PriorityTriggered), "second in line")
	if err := p.Wait(short(), PriorityTriggered); !errors.Is(err, ErrPacerBusy) {
		t.Fatalf("third in line: %v, want ErrPacerBusy", err)
	}
	// A sweep call behind them waits on them too; one with no deadline is
	// queued however long the wait.
	if err := p.Wait(short(), PrioritySweep); !errors.Is(err, ErrPacerBusy) {
		t.Fatalf("a sweep call behind two: %v, want ErrPacerBusy", err)
	}
	advance(20 * time.Second)
	mustReturn(t, ch, "first in line")

	// A 429 holds everything for 60 s.
	p.Observe(http.StatusTooManyRequests, "")
	if err := p.Wait(short(), PriorityTriggered); !errors.Is(err, ErrPacerBusy) {
		t.Fatalf("during the back-off: %v, want ErrPacerBusy", err)
	}
}

func TestPriorityOfRequest(t *testing.T) {
	req := func(ctx context.Context, path string) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://x/api"+path, nil)
		return r
	}
	bg := context.Background()
	for _, c := range []struct {
		ctx  context.Context
		path string
		want Priority
	}{
		{bg, "/outages", PriorityRefresh},
		{bg, "/services/1/outages", PriorityRefresh},
		{bg, "/services/1/assurance-tests/9", PriorityAssurance},
		{bg, "/service-assurance-tests?carrier=nbn", PriorityAssurance},
		{WithPriority(bg, PrioritySweep), "/services/1/outages", PrioritySweep},
		{WithPriority(bg, PriorityTriggered), "/services/1/assurance-tests", PriorityTriggered},
	} {
		if got := priorityOf(req(c.ctx, c.path)); got != c.want {
			t.Errorf("%s: %d, want %d", c.path, got, c.want)
		}
	}
}

func TestIsPaced(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/outages?start_date=2026-11-06", true},
		{"GET", "/outages/1", true},
		{"GET", "/services/1/outages", true},
		{"POST", "/services/1/assurance-tests", true},
		{"GET", "/services/1/assurance-tests/9", true},
		{"GET", "/services/1/assurance-tests-history?page=1", true},
		{"GET", "/service-assurance-tests?carrier=nbn&access_technology=FTTP", true},
		{"GET", "/services/1", false},
		{"GET", "/orders/9", false},
		{"POST", "/orders", false},
		{"GET", "/products?carrier=nbn", false},
	} {
		if got := isPaced(c.method, c.path); got != c.want {
			t.Errorf("isPaced(%s %s) = %v", c.method, c.path, got)
		}
	}
}

// pacedServer answers every route the client tests call.
func pacedServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var outageHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/outages/"):
			outageHits.Add(1)
			w.Write([]byte(`{"outage_id":1}`))
		case strings.HasSuffix(r.URL.Path, "/outages"):
			outageHits.Add(1)
			w.Write([]byte(`{"pagination":{"current_page":1},"outages":[]}`))
		case strings.HasPrefix(r.URL.Path, "/service-assurance-tests"):
			w.Write([]byte(`{"8":{"test_name":"Service Health","behaviour":"async"}}`))
		case strings.Contains(r.URL.Path, "/assurance-tests"):
			w.Write([]byte(`{"test_id":5,"request_status":"pending"}`))
		case strings.HasPrefix(r.URL.Path, "/orders/"):
			w.Write([]byte(`{"order_id":9}`))
		default:
			w.Write([]byte(`{"service_id":1}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &outageHits
}

func TestClientPacesOutageAndAssuranceOnly(t *testing.T) {
	srv, outageHits := pacedServer(t)
	q := OutageQuery{StartDate: outageDay}

	t.Run("a full window blocks outage reads, not other reads", func(t *testing.T) {
		c := NewClient(srv.URL, "u", "p").WithPacer(NewPacer(1))
		if _, err := c.ListOutages(context.Background(), q); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if _, err := c.ListOutages(ctx, q); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second outage read: %v, want it held until its deadline", err)
		}
		if n := outageHits.Load(); n != 1 {
			t.Fatalf("upstream saw %d outage reads, want 1", n)
		}
		start := time.Now()
		if _, err := c.GetService(1); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.GetOrder(9); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("unpaced reads took %v", d)
		}
	})

	t.Run("each assurance call takes a slot", func(t *testing.T) {
		p, _ := newTestPacer(t, 10)
		c := NewClient(srv.URL, "u", "p").WithPacer(p).WithSink(&memSink{}).WithBreaker(&memBreaker{})
		if _, err := c.GetService(1); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.GetOrder(9); err != nil {
			t.Fatal(err)
		}
		if n := p.inWindow(); n != 0 {
			t.Fatalf("unpaced reads took %d slots", n)
		}
		steps := []func() error{
			func() error { _, err := c.ListOutages(context.Background(), q); return err },
			func() error { _, err := c.GetOutage(context.Background(), 1); return err },
			func() error { _, err := c.ListServiceOutages(context.Background(), 1, q); return err },
			func() error { _, _, err := c.ListAssuranceTests("nbn", "FTTP"); return err },
			func() error { _, _, err := c.RunAssuranceTest(1, 8); return err },
			func() error { _, _, err := c.GetAssuranceTest(1, 5); return err },
		}
		for i, step := range steps {
			if err := step(); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
			if n := p.inWindow(); n != i+1 {
				t.Fatalf("after step %d: %d slots taken", i, n)
			}
		}
	})

	t.Run("copies share the pacer", func(t *testing.T) {
		p, _ := newTestPacer(t, 10)
		c := NewClient(srv.URL, "u", "p").WithPacer(p)
		if _, err := c.WithSink(&memSink{}).For(1, 0).WithTimeout(time.Second).ListOutages(context.Background(), q); err != nil {
			t.Fatal(err)
		}
		if n := p.inWindow(); n != 1 {
			t.Fatalf("%d slots taken through a copy", n)
		}
	})
}

func TestRateLimitedReadDoesNotTripStop(t *testing.T) {
	srv, _ := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-limit", "10")
		w.Header().Set("x-ratelimit-remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"Too Many Attempts."}`))
	})
	b := &memBreaker{}
	sink := &memSink{}
	p, advance := newTestPacer(t, DefaultPacedPerMinute)
	c := NewClient(srv.URL, "u", "p").WithBreaker(b).WithSink(sink).WithPacer(p)
	_, err := c.ListOutages(context.Background(), OutageQuery{StartDate: outageDay})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err %v, want ErrRateLimited", err)
	}
	if tripped, _ := b.Tripped(context.Background()); tripped {
		t.Fatalf("a rate-limited read tripped the stop: %s", b.reason)
	}
	if p.RateLimited() != 1 {
		t.Fatalf("RateLimited %d, want 1", p.RateLimited())
	}
	if got := sink.only(t); got.Status != 429 || !strings.Contains(got.ResponseBody, "Too Many Attempts.") {
		t.Fatalf("recorded: %+v", got)
	}
	// An assurance run (a POST that isn't a write) answered 429 is the same,
	// once the back-off is over.
	advance(time.Minute)
	if _, _, err := c.RunAssuranceTest(1, 8); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("assurance run: %v, want ErrRateLimited", err)
	}
	if tripped, _ := b.Tripped(context.Background()); tripped {
		t.Fatalf("a rate-limited assurance run tripped the stop: %s", b.reason)
	}
	if p.RateLimited() != 2 {
		t.Fatalf("RateLimited %d, want 2", p.RateLimited())
	}
}

func TestUnpacedClientIgnoresRateLimitHeader(t *testing.T) {
	srv, _ := pacedServer(t)
	c := NewClient(srv.URL, "u", "p")
	for i := 0; i < 8; i++ {
		if _, err := c.ListOutages(context.Background(), OutageQuery{StartDate: outageDay}); err != nil {
			t.Fatal(err)
		}
	}
}
