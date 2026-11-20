package leaptel

// The call record: every request to Leaptel, with its response, goes to a
// CallSink (the store's leaptel_calls), so a strange rejection or a lost order
// can be debugged after the fact. It wraps the HTTP transport, so every call
// site is covered; For tags calls with the Underlay service/order they're for.
// Credentials never reach the sink: no headers are kept, and password fields
// are blanked in both bodies.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// maxRecordedBody caps each recorded body; the caller still gets all of it.
const maxRecordedBody = 256 << 10

// Call is one recorded request/response.
type Call struct {
	At           time.Time
	Method, Path string // Path is relative to the base URL, query included
	Write        bool
	Status       int // 0: no response (transport error)
	Duration     time.Duration
	Err          string
	RequestBody  string
	ResponseBody string
	Truncated    bool
	// ServiceID and OrderID are the Underlay service and order the call is
	// for, as ULID text ("" for none): this package imports nothing of ours,
	// so it carries our ids as their text (internal/leaptel is published on
	// its own as leaptel-api).
	ServiceID string
	OrderID   string
}

// CallSink receives every call. It must not block for long or fail the call.
type CallSink interface {
	RecordCall(ctx context.Context, c Call)
}

type scope struct{ serviceID, orderID string }

type scopeKey struct{}

// WithSink returns a client whose calls are all recorded to s.
func (c *Client) WithSink(s CallSink) *Client {
	cp := *c
	cp.transport().sink = s
	return &cp
}

// For returns a client whose calls are recorded against an Underlay service
// and (when not "") order, each its ULID text.
func (c *Client) For(serviceID, orderID string) *Client {
	cp := *c
	cp.scope = scope{serviceID, orderID}
	return &cp
}

// newRequest builds a request carrying the client's scope for the recorder,
// under the client's context (WithContext; context.Background() without one).
func (c *Client) newRequest(method, url string, body io.Reader) (*http.Request, error) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return c.newRequestCtx(ctx, method, url, body)
}

// newRequestCtx is newRequest under the caller's context (its deadline, and
// the pacer's priority).
func (c *Client) newRequestCtx(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(context.WithValue(ctx, scopeKey{}, c.scope), method, url, body)
}

// WithContext returns a client whose calls from methods that take no context
// run under ctx: its deadline, its priority (WithPriority) and NoWait reach
// the budget and the pacer.
func (c *Client) WithContext(ctx context.Context) *Client {
	cp := *c
	cp.ctx = ctx
	return &cp
}

// IsWrite reports whether a Leaptel call changes something there: anything
// but a GET, except a service qualification or a line (assurance) test —
// POSTs that only read, and must keep working while the safety stop is
// tripped.
func IsWrite(method, path string) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return false
	}
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p != "/service-qualifications" && !strings.Contains(p, "/assurance-tests")
}

var serviceOutagesPath = regexp.MustCompile(`^/services/\d+/outages(?:/|$)`)

// isOutageRead reports whether a call reads outage records: GET /outages,
// /outages/{id} or /services/{id}/outages. A page is 60-100 KB and they run
// all day, so a successful one is recorded without its body.
func isOutageRead(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p == "/outages" || strings.HasPrefix(p, "/outages/") || serviceOutagesPath.MatchString(p)
}

var (
	jsonPassword      = regexp.MustCompile(`("(?:ppoe_)?password"\s*:\s*")(?:[^"\\]|\\.)*"`)
	multipartPassword = regexp.MustCompile(`(name="(?:ppoe_)?password"\r\n\r\n)[^\r\n]*`)
)

func redact(s string) string {
	s = jsonPassword.ReplaceAllString(s, `${1}***"`)
	return multipartPassword.ReplaceAllString(s, `${1}***`)
}

// CleanText makes s storable in a Postgres TEXT column: invalid UTF-8 becomes
// U+FFFD and NUL bytes are dropped.
func CleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}

// capBody is b as recorded: redacted over the whole body (so a password
// straddling the cut is still blanked), cleaned for TEXT, then cut to at most
// maxRecordedBody bytes on a rune boundary.
func capBody(b []byte) (string, bool) {
	s := CleanText(redact(string(b)))
	if len(s) <= maxRecordedBody {
		return s, false
	}
	cut := maxRecordedBody
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// recorder is the client's transport: it records every call (sink),
// enforces the safety stop on writes (breaker), takes every call's slot from
// the shared budget (limiter) and, without one, paces outage reads and
// assurance calls (pacer). Any of them may be nil.
type recorder struct {
	base    http.RoundTripper
	sink    CallSink
	breaker Breaker
	pacer   *Pacer       // shared by every copy; nil: nothing is paced
	limiter *limiterSlot // shared by every copy, set or not (UseLimiter)
	baseURL string
	timeout time.Duration
}

// --- the shared budget ---

// Limiter is the budget every call takes a slot from before it's sent: one
// shared by every process that calls the upstream (the store's DB bucket),
// so two pods together stay inside the upstream's per-route limits. route is
// the upstream's rate bucket the call counts against (RouteOf), write whether
// it changes anything there. release, which must be called once, takes the
// answer's status (0: none came). Take waits for a slot until ctx ends, or
// no longer than MaxWait allows: an error means the call wasn't sent.
type Limiter interface {
	Take(ctx context.Context, route string, write bool) (release func(status int), err error)
}

// The upstream's rate buckets, as RouteOf names them.
const (
	RouteOutages  = "outages"  // GET /outages, /outages/{id}, /services/{id}/outages: 10 a minute
	RouteServices = "services" // GET /services/{id}: 15 a minute
	RouteGeneral  = "general"  // every other read, service qualifications and assurance tests: about 10 a minute
	RouteWrites   = "writes"   // orders, modifies, cancels, customers, appointments (general's bucket, unless proven otherwise)
)

var serviceRead = regexp.MustCompile(`^/services/\d+$`)

// RouteOf is the rate bucket a call (path relative to the base URL, query
// included) counts against upstream.
func RouteOf(method, path string) string {
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	switch {
	case p == "/outages" || strings.HasPrefix(p, "/outages/") || serviceOutagesPath.MatchString(p):
		return RouteOutages
	case method == http.MethodGet && serviceRead.MatchString(p):
		return RouteServices
	case !IsWrite(method, p):
		return RouteGeneral
	}
	return RouteWrites
}

// ErrBudgetBusy is a call the shared budget had no slot for: before its
// context's deadline, or within MaxWait (at once under NoWait). It wasn't sent. It's an
// ErrPacerBusy (and so a context.DeadlineExceeded) too: callers that map a
// busy pacer map this the same way.
var ErrBudgetBusy = fmt.Errorf("leaptel: no slot in the shared budget: %w", ErrPacerBusy)

type maxWaitKey struct{}

// MaxWait returns ctx whose calls wait at most d for a budget slot, whatever
// their own deadline (time in the queue isn't the call's): past it they fail
// with ErrBudgetBusy, unsent. A negative d lifts an outer bound (wait as long
// as the context lasts). A service qualification waits 30 s for its turn,
// then asks the customer to try again.
func MaxWait(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, maxWaitKey{}, d)
}

// NoWait is MaxWait(ctx, 0): a slot only if one is free now. Loops that
// spread their work over ticks use it, so a busy budget ends the tick rather
// than holding it.
func NoWait(ctx context.Context) context.Context { return MaxWait(ctx, 0) }

// MaxWaitFrom is ctx's bound on waiting for a slot, and false when it has
// none (or MaxWait lifted it).
func MaxWaitFrom(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Value(maxWaitKey{}).(time.Duration)
	if !ok || d < 0 {
		return 0, false
	}
	return d, true
}

// PriorityFrom is the priority WithPriority put on ctx, and false when none
// was: a Limiter reads it to hold capacity back for the calls a person waits
// on.
func PriorityFrom(ctx context.Context) (Priority, bool) {
	p, ok := ctx.Value(priorityKey{}).(Priority)
	return p, ok
}

// limiterSlot is the Limiter every copy of a client shares: setting it once
// reaches copies made before (the outage ingest's) as well as after.
type limiterSlot struct{ l atomic.Pointer[Limiter] }

func (s *limiterSlot) get() Limiter {
	if s == nil {
		return nil
	}
	if p := s.l.Load(); p != nil {
		return *p
	}
	return nil
}

// UseLimiter makes every call of this client, and of every copy of it made
// before or after, take its slot from l first (nil: none). With a Limiter
// the pacer no longer holds calls back (the budget does, for every process
// at once); it still counts its 429s.
func (c *Client) UseLimiter(l Limiter) {
	r, ok := c.http.Transport.(*recorder)
	if !ok {
		r = c.transport()
	}
	if r.limiter == nil {
		r.limiter = &limiterSlot{}
	}
	if l == nil {
		r.limiter.l.Store(nil)
		return
	}
	r.limiter.l.Store(&l)
}

func (r *recorder) record(ctx context.Context, c Call) {
	if r.sink != nil {
		r.sink.RecordCall(ctx, c)
	}
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(req.URL.String(), r.baseURL)
	limiter := r.limiter.get()
	paced := r.pacer != nil && isPaced(req.Method, path)
	if paced && limiter == nil {
		// Before the timeout starts: time in the queue isn't the call's.
		if err := r.pacer.Wait(req.Context(), priorityOf(req)); err != nil {
			if req.Body != nil {
				req.Body.Close() // not sent
			}
			return nil, err
		}
	}
	var reqBody []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		reqBody = b
		req.Body = io.NopCloser(bytes.NewReader(b))
		// net/http rewinds through GetBody to retry on a reused keep-alive
		// connection that turned out closed; without it that surfaces as
		// "no reply", which is uncertain and trips the stop.
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
		req.ContentLength = int64(len(b))
	}
	call := Call{Method: req.Method, Path: path, Write: IsWrite(req.Method, path)}
	if call.Write && r.breaker != nil {
		if tripped, err := r.breaker.Tripped(req.Context()); err != nil || tripped {
			return nil, ErrHeld // not sent; staff release the stop
		}
	}
	// The budget's slot, before the timeout starts (time waiting for it isn't
	// the call's). Taken, it's handed back with the answer's status, so a
	// 429 holds the route back for every process.
	release := func(int) {}
	if limiter != nil {
		rel, err := limiter.Take(req.Context(), RouteOf(req.Method, path), call.Write)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", req.Method, path, err) // not sent
		}
		release = rel
	}
	start := time.Now()
	call.At = start
	if r.timeout > 0 {
		ctx, cancel := context.WithTimeout(req.Context(), r.timeout)
		defer cancel() // the body is read in full below, so the deadline can end with this call
		req = req.WithContext(ctx)
	}
	if sc, ok := req.Context().Value(scopeKey{}).(scope); ok {
		call.ServiceID, call.OrderID = sc.serviceID, sc.orderID
	}
	var truncReq bool
	call.RequestBody, truncReq = capBody(reqBody)

	resp, err := r.base.RoundTrip(req)
	call.Duration = time.Since(start)
	if err != nil {
		release(0)
		call.Err = err.Error()
		r.record(req.Context(), call)
		return nil, r.stopIfBroken(req, call, nil, err)
	}
	release(resp.StatusCode)
	respBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	call.Status = resp.StatusCode
	if paced {
		r.pacer.Observe(resp.StatusCode, resp.Header.Get("x-ratelimit-limit"))
	}
	var truncResp bool
	call.ResponseBody, truncResp = capBody(respBody)
	if readErr == nil && resp.StatusCode/100 == 2 && isOutageRead(req.Method, path) {
		call.ResponseBody, truncResp = fmt.Sprintf("(not kept: %d bytes)", len(respBody)), false
	}
	call.Truncated = truncReq || truncResp
	if readErr != nil {
		call.Err = "reading response: " + readErr.Error()
	}
	r.record(req.Context(), call)
	if readErr != nil {
		return nil, r.stopIfBroken(req, call, nil, readErr)
	}
	if !call.Write && resp.StatusCode == http.StatusTooManyRequests {
		// A read answered 429 never trips the stop: it's ErrRateLimited, for
		// the caller to try again later (and the budget holds the route back).
		return nil, fmt.Errorf("%s %s: %w", req.Method, path, ErrRateLimited)
	}
	if err := r.stopIfBroken(req, call, respBody, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// stopIfBroken trips the stop when a write broke, returning the StopError the
// caller sees instead of the response; otherwise it passes err through.
func (r *recorder) stopIfBroken(req *http.Request, call Call, body []byte, err error) error {
	if !call.Write || r.breaker == nil {
		return err
	}
	reason, uncertain := tripReason(call.Method, call.Path, call.Status, body, err)
	if reason == "" {
		return err
	}
	r.breaker.Trip(req.Context(), reason)
	return &StopError{Reason: reason, Uncertain: uncertain}
}
