package leaptel

// The safety stop. When Leaptel (or our integration with it) misbehaves on a
// write, the stop trips: from then on the client refuses every write without
// sending it (ErrHeld) until staff release it, while reads carry on. A clean
// rejection of one request (a 4xx with a body) never trips it.
//
// Some failures may still have changed something at Leaptel: a timeout or a
// dropped connection after the request went out, a 5xx (it may have failed
// after saving), or a 2xx we can't read (an order with no order id). Those are Uncertain: whoever holds the request must
// check Leaptel before anything is resent, so nothing is ever ordered or
// cancelled twice.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrHeld is what a write gets while the stop is tripped. Nothing was sent.
var ErrHeld = errors.New("leaptel writes are stopped")

// StopError is a write that tripped the stop.
type StopError struct {
	Reason    string
	Uncertain bool // it may have reached Leaptel
}

func (e *StopError) Error() string { return "leaptel: " + e.Reason }

// Stopped reports whether err means the stop is in the way: the write was
// refused (ErrHeld) or it tripped the stop.
func Stopped(err error) bool {
	var se *StopError
	return errors.Is(err, ErrHeld) || errors.As(err, &se)
}

// Uncertain reports whether a failed write may still have reached Leaptel.
func Uncertain(err error) bool {
	var se *StopError
	return errors.As(err, &se) && se.Uncertain
}

// Breaker holds the stop's state (the store's leaptel_stop).
type Breaker interface {
	Tripped(ctx context.Context) (bool, error)
	Trip(ctx context.Context, reason string)
}

// WithBreaker returns a client whose writes obey b.
func (c *Client) WithBreaker(b Breaker) *Client {
	cp := *c
	rec := cp.transport()
	rec.breaker = b
	return &cp
}

// defaultTimeout bounds every Leaptel request.
const defaultTimeout = 60 * time.Second

// WithTimeout returns a client whose requests give up after d. The deadline
// lives in the transport, not http.Client.Timeout, so a timed-out write still
// surfaces as a StopError (http.Client flattens its own timeout to a string).
func (c *Client) WithTimeout(d time.Duration) *Client {
	cp := *c
	cp.transport().timeout = d
	return &cp
}

// transport makes cp's HTTP client use its own copy of the recording
// transport (wrapping whatever it had) and returns it for configuring.
func (c *Client) transport() *recorder {
	hc := *c.http
	var rec recorder
	if r, ok := hc.Transport.(*recorder); ok {
		rec = *r
	} else {
		rec = recorder{base: hc.Transport, baseURL: c.baseURL, timeout: defaultTimeout}
		if rec.base == nil {
			rec.base = http.DefaultTransport
		}
	}
	hc.Transport = &rec
	c.http = &hc
	return &rec
}

// tripReason says whether a write's outcome must trip the stop, and whether
// the write may have reached Leaptel. "" means it didn't break anything.
func tripReason(method, path string, status int, body []byte, err error) (reason string, uncertain bool) {
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return fmt.Sprintf("%s %s: can't connect: %v", method, path, err), false // never left
		}
		return fmt.Sprintf("%s %s: no reply: %v", method, path, err), true
	}
	switch {
	case status >= 500:
		// Leaptel (or a proxy in front of it) failed, possibly after saving it.
		return fmt.Sprintf("%s %s: HTTP %d", method, path, status), true
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests:
		// Nothing was done, but every write would be refused the same way.
		return fmt.Sprintf("%s %s: HTTP %d", method, path, status), false
	case status == http.StatusRequestTimeout:
		return fmt.Sprintf("%s %s: HTTP %d", method, path, status), true
	case status >= 400 && status < 500 && !json.Valid(bytes.TrimSpace(body)):
		// Not a rejection of this request: something in front of Leaptel answered.
		return fmt.Sprintf("%s %s: HTTP %d that isn't Leaptel's", method, path, status), false
	case status >= 200 && status < 300 && len(bytes.TrimSpace(body)) > 0 && !json.Valid(body):
		return fmt.Sprintf("%s %s: HTTP %d with a reply we can't read", method, path, status), true
	case status >= 200 && status < 300 && method == http.MethodPost && strings.TrimRight(path, "/") == "/orders":
		var o struct {
			OrderID int `json:"order_id"`
		}
		if json.Unmarshal(body, &o) != nil || o.OrderID <= 0 {
			return fmt.Sprintf("%s %s: accepted with no order id", method, path), true
		}
	}
	return "", false
}
