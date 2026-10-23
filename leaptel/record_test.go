package leaptel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

type memSink struct {
	mu    sync.Mutex
	calls []Call
}

func (m *memSink) RecordCall(_ context.Context, c Call) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, c)
}

func (m *memSink) only(t *testing.T) Call {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) != 1 {
		t.Fatalf("recorded %d calls, want 1: %+v", len(m.calls), m.calls)
	}
	return m.calls[0]
}

func TestRecorderRecordsAndRedacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"order_id":77,"ppoe_password":"hunter2"}`))
	}))
	defer srv.Close()
	sink := &memSink{}
	c := NewClient(srv.URL, "user", "s3cret").WithSink(sink).For(12, 34)
	if _, _, err := c.CreateOrder(CreateOrderParams{CustomerID: 1, LocationID: "LOC000000000001", PlanID: 3539, ContactFirstName: "Ada"}); err != nil {
		t.Fatal(err)
	}
	got := sink.only(t)
	if got.Method != "POST" || got.Path != "/orders" || !got.Write || got.Status != 200 || got.Duration <= 0 {
		t.Fatalf("call: %+v", got)
	}
	if got.ServiceID != 12 || got.OrderID != 34 {
		t.Fatalf("scope: service %d order %d, want 12/34", got.ServiceID, got.OrderID)
	}
	if !strings.Contains(got.RequestBody, "LOC000000000001") || !strings.Contains(got.ResponseBody, `"order_id":77`) {
		t.Fatalf("bodies not recorded: req %q resp %q", got.RequestBody, got.ResponseBody)
	}
	for _, leak := range []string{"hunter2", "s3cret", "Basic "} {
		if strings.Contains(got.RequestBody+got.ResponseBody+got.Err, leak) {
			t.Fatalf("recorded a credential (%q): %+v", leak, got)
		}
	}
}

func TestRecorderCapsBodies(t *testing.T) {
	big := `{"x":"` + strings.Repeat("a", 1<<20) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(big)) }))
	defer srv.Close()
	sink := &memSink{}
	c := NewClient(srv.URL, "u", "p").WithSink(sink)
	raw, err := c.ListProducts("nbn")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != len(big) {
		t.Fatalf("caller got %d bytes, want the full %d", len(raw), len(big))
	}
	got := sink.only(t)
	if len(got.ResponseBody) != 256<<10 || !got.Truncated || got.Write {
		t.Fatalf("recorded %d bytes truncated=%v write=%v", len(got.ResponseBody), got.Truncated, got.Write)
	}
}

func TestRecorderTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listening
	sink := &memSink{}
	c := NewClient(url, "u", "p").WithSink(sink)
	if _, err := c.GetService(1); err == nil {
		t.Fatal("want an error from a closed port")
	}
	if got := sink.only(t); got.Status != 0 || got.Err == "" {
		t.Fatalf("transport error call: %+v", got)
	}
}

func TestIsWrite(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/services/1", false},
		{"POST", "/service-qualifications", false},
		{"POST", "/orders", true},
		{"POST", "/services/1/modify", true},
		{"POST", "/services/1/cancel", true},
		{"POST", "/customers", true},
		{"POST", "/service-qualifications?x=1", false},
		{"POST", "/services/1/assurance-tests", false},
		{"POST", "/services/1/assurance-tests?type=line", false},
		{"GET", "/services/1/assurance-tests/9", false},
	} {
		if got := IsWrite(c.method, c.path); got != c.want {
			t.Errorf("IsWrite(%s %s) = %v", c.method, c.path, got)
		}
	}
}

// capBody's output goes into a Postgres TEXT column: it must be valid UTF-8
// with no NULs, at most maxRecordedBody bytes, cut on a rune boundary, and
// redacted over the whole body (so a password straddling the cut can't leak).
func TestCapBody(t *testing.T) {
	t.Run("NUL and invalid UTF-8", func(t *testing.T) {
		got, trunc := capBody([]byte("a\x00b\xffc\xc3("))
		if trunc || !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
			t.Fatalf("capBody = %q truncated=%v", got, trunc)
		}
		if got != "ab�c�(" {
			t.Fatalf("capBody = %q", got)
		}
	})
	t.Run("rune at the boundary", func(t *testing.T) {
		b := []byte(strings.Repeat("a", maxRecordedBody-1) + "€" + "tail") // € is 3 bytes, straddling the cut
		got, trunc := capBody(b)
		if !trunc || !utf8.ValidString(got) || len(got) > maxRecordedBody {
			t.Fatalf("len %d truncated=%v valid=%v", len(got), trunc, utf8.ValidString(got))
		}
		if got != strings.Repeat("a", maxRecordedBody-1) {
			t.Fatalf("cut mid-rune: ends %q", got[len(got)-4:])
		}
	})
	t.Run("password straddling the cut", func(t *testing.T) {
		pre := `{"x":"` + strings.Repeat("a", maxRecordedBody-30) + `","ppoe_password":"`
		b := []byte(pre + strings.Repeat("S", 100) + `","y":"` + strings.Repeat("b", 100) + `"}`)
		got, trunc := capBody(b)
		if !trunc || strings.Contains(got, "S") {
			t.Fatalf("password partly recorded: truncated=%v tail %q", trunc, got[len(got)-40:])
		}
	})
	t.Run("small body untouched", func(t *testing.T) {
		if got, trunc := capBody([]byte(`{"a":"é"}`)); trunc || got != `{"a":"é"}` {
			t.Fatalf("capBody = %q truncated=%v", got, trunc)
		}
	})
}

// fakeBase is a base RoundTripper that records the request it was handed.
type fakeBase struct{ got *http.Request }

func (f *fakeBase) RoundTrip(req *http.Request) (*http.Response, error) {
	f.got = req
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}, Request: req}, nil
}

// net/http only retries a request on a reused, closed keep-alive connection
// when it can rewind the body (GetBody); otherwise the failure surfaces as
// "no reply" and trips the stop.
func TestRecorderSetsGetBody(t *testing.T) {
	base := &fakeBase{}
	r := &recorder{base: base, baseURL: "http://leaptel"}
	req, _ := http.NewRequest("POST", "http://leaptel/orders", io.NopCloser(strings.NewReader(`{"a":1}`)))
	req.GetBody = nil // as if built from a body net/http can't rewind
	if _, err := r.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if base.got == nil || base.got.GetBody == nil {
		t.Fatal("base transport got a request without GetBody")
	}
	for i := 0; i < 2; i++ {
		rc, err := base.got.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		if string(b) != `{"a":1}` {
			t.Fatalf("GetBody #%d gave %q", i, b)
		}
	}
}
