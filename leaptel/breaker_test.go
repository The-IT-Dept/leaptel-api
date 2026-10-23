package leaptel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memBreaker struct {
	mu      sync.Mutex
	tripped bool
	reason  string
}

func (b *memBreaker) Tripped(context.Context) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tripped, nil
}

func (b *memBreaker) Trip(_ context.Context, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tripped, b.reason = true, reason
}

func stubServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); h(w, r) }))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestStopTripRules(t *testing.T) {
	for _, c := range []struct {
		name          string
		handler       http.HandlerFunc
		wantTrip      bool
		wantUncertain bool
	}{
		// A 5xx may have come after Leaptel saved it (a 502/504 from a proxy, a late 500): uncertain.
		{"500", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			w.Write([]byte(`{"message":"boom"}`))
		}, true, true},
		{"502", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502); w.Write([]byte(`bad gateway`)) }, true, true},
		// Auth and rate limits: nothing was done, but every write would fail the same way, so stop.
		{"401", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			w.Write([]byte(`{"message":"unauthorised"}`))
		}, true, false},
		{"403", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"blocked"}`))
		}, true, false},
		{"429", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(429)
			w.Write([]byte(`{"message":"slow down"}`))
		}, true, false},
		{"408", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(408)
			w.Write([]byte(`{"message":"timeout"}`))
		}, true, true},
		// A 4xx that isn't JSON came from something in front of Leaptel, not a rejection of this request.
		{"html 404", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			w.Write([]byte(`<html>not found</html>`))
		}, true, false},
		{"garbage 2xx", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`<html>oops`)) }, true, true},
		{"no order id", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}`)) }, true, true},
		{"timeout", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.Write([]byte(`{"order_id":1}`))
		}, true, true},
		{"422", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"not serviceable"}`))
		}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := stubServer(t, c.handler)
			b := &memBreaker{}
			cl := NewClient(srv.URL, "u", "p").WithTimeout(100 * time.Millisecond).WithBreaker(b)
			_, _, err := cl.CreateOrder(CreateOrderParams{CustomerID: 1, LocationID: "LOC1", PlanID: 1})
			if err == nil {
				t.Fatal("want an error")
			}
			if got, _ := b.Tripped(context.Background()); got != c.wantTrip || Stopped(err) != c.wantTrip {
				t.Fatalf("tripped %v Stopped(err) %v, want %v (err %v)", got, Stopped(err), c.wantTrip, err)
			}
			if Uncertain(err) != c.wantUncertain {
				t.Fatalf("Uncertain = %v, want %v (err %v)", Uncertain(err), c.wantUncertain, err)
			}
		})
	}
}

func TestStopRefusesWritesNotReads(t *testing.T) {
	srv, hits := stubServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"service_id":1,"order_id":1}`)) })
	b := &memBreaker{tripped: true}
	cl := NewClient(srv.URL, "u", "p").WithBreaker(b)
	if _, _, err := cl.CreateOrder(CreateOrderParams{CustomerID: 1, LocationID: "LOC1", PlanID: 1}); !errors.Is(err, ErrHeld) || !Stopped(err) || Uncertain(err) {
		t.Fatalf("write while stopped: %v, want ErrHeld (certain)", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("a write reached Leaptel while stopped")
	}
	if _, err := cl.GetService(1); err != nil || hits.Load() != 1 {
		t.Fatalf("read while stopped: err %v hits %d, want it sent", err, hits.Load())
	}
}
