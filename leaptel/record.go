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
	"io"
	"net/http"
	"regexp"
	"strings"
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
	ServiceID    int64 // 0 when the call isn't for one service
	OrderID      int64
}

// CallSink receives every call. It must not block for long or fail the call.
type CallSink interface {
	RecordCall(ctx context.Context, c Call)
}

type scope struct{ serviceID, orderID int64 }

type scopeKey struct{}

// WithSink returns a client whose calls are all recorded to s.
func (c *Client) WithSink(s CallSink) *Client {
	cp := *c
	cp.transport().sink = s
	return &cp
}

// For returns a client whose calls are recorded against an Underlay service
// and (when non-zero) order.
func (c *Client) For(serviceID, orderID int64) *Client {
	cp := *c
	cp.scope = scope{serviceID, orderID}
	return &cp
}

// newRequest builds a request carrying the client's scope for the recorder.
func (c *Client) newRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	return req.WithContext(context.WithValue(req.Context(), scopeKey{}, c.scope)), nil
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

// recorder is the client's transport: it records every call (sink) and
// enforces the safety stop on writes (breaker). Either may be nil.
type recorder struct {
	base    http.RoundTripper
	sink    CallSink
	breaker Breaker
	baseURL string
	timeout time.Duration
}

func (r *recorder) record(ctx context.Context, c Call) {
	if r.sink != nil {
		r.sink.RecordCall(ctx, c)
	}
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	if r.timeout > 0 {
		ctx, cancel := context.WithTimeout(req.Context(), r.timeout)
		defer cancel() // the body is read in full below, so the deadline can end with this call
		req = req.WithContext(ctx)
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
	path := strings.TrimPrefix(req.URL.String(), r.baseURL)
	call := Call{At: start, Method: req.Method, Path: path, Write: IsWrite(req.Method, path)}
	if call.Write && r.breaker != nil {
		if tripped, err := r.breaker.Tripped(req.Context()); err != nil || tripped {
			return nil, ErrHeld // not sent; staff release the stop
		}
	}
	if sc, ok := req.Context().Value(scopeKey{}).(scope); ok {
		call.ServiceID, call.OrderID = sc.serviceID, sc.orderID
	}
	var truncReq bool
	call.RequestBody, truncReq = capBody(reqBody)

	resp, err := r.base.RoundTrip(req)
	call.Duration = time.Since(start)
	if err != nil {
		call.Err = err.Error()
		r.record(req.Context(), call)
		return nil, r.stopIfBroken(req, call, nil, err)
	}
	respBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	call.Status = resp.StatusCode
	var truncResp bool
	call.ResponseBody, truncResp = capBody(respBody)
	call.Truncated = truncReq || truncResp
	if readErr != nil {
		call.Err = "reading response: " + readErr.Error()
	}
	r.record(req.Context(), call)
	if readErr != nil {
		return nil, r.stopIfBroken(req, call, nil, readErr)
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
