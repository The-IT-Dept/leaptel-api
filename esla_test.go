package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/the-it-dept/leaptel-api/leaptel"
)

// fakeAPI serves the fake on a test server, persisting to a temp state file.
func fakeAPI(t *testing.T) (*fakeServer, *leaptel.Client, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "state.json")
	srv := newFakeServer(file)
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return srv, leaptel.NewClient(ts.URL, "u", "p"), ts.URL
}

// post sends a multipart form, returning the status and decoded body.
func post(t *testing.T, url string, fields map[string]string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetBasicAuth("u", "p")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// age moves an order's clock back by d, as if it were placed that long ago.
func (s *fakeServer) age(orderID int, d time.Duration) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	o := s.state.orders[orderID]
	o.CreatedAt = o.CreatedAt.Add(-d)
}

// liveService connects a service at an even LOC (no appointment) and walks
// its connect to completion.
func liveService(t *testing.T, srv *fakeServer, c *leaptel.Client, esla string) int {
	t.Helper()
	lo, _, err := c.CreateOrder(leaptel.CreateOrderParams{CustomerID: 1, Carrier: "nbn", LocationID: "LOC000000000002",
		PlanID: 3538, OrderType: "data", ConnectionType: "new", ESLA: esla})
	if err != nil {
		t.Fatal(err)
	}
	srv.age(lo.OrderID, 10*time.Minute)
	if o, _, err := c.GetOrder(lo.OrderID); err != nil || o.Status != "completed" {
		t.Fatalf("connect: %+v %v", o, err)
	}
	return lo.ServiceID
}

func TestFakeLeaptelESLAModifyLifecycle(t *testing.T) {
	srv, c, url := fakeAPI(t)
	id := liveService(t, srv, c, "")
	if ls, err := c.GetService(id); err != nil || ls.NbnESLA != "Standard" {
		t.Fatalf("a fresh service is on Standard: %+v %v", ls, err)
	}

	// An unknown level, or none, is a 400.
	if code, body := post(t, url+"/services/"+itoa(id)+"/modify", map[string]string{"order_type": "esla", "esla": "gold", "order_after": "2026-11-01"}); code != 400 || body["error"] != "invalid esla" {
		t.Fatalf("unknown esla: %d %v", code, body)
	}
	if code, _ := post(t, url+"/services/"+itoa(id)+"/modify", map[string]string{"order_type": "esla", "order_after": "2026-11-01"}); code != 400 {
		t.Fatalf("esla modify without esla: %d", code)
	}

	// The real client's modify: no plan, accepted.
	lo, _, err := c.ModifyESLA(id, "enhanced-4-24-7")
	if err != nil {
		t.Fatal(err)
	}
	o, _, err := c.GetOrder(lo.OrderID)
	if err != nil || o.Action != "esla" || o.State != "accepted" || o.NbnESLA != "Standard" {
		t.Fatalf("fresh esla order: %+v %v", o, err)
	}
	// It speaks of a service level change, never a speed change (Underlay keeps the reasons as the order's events).
	if r := string(o.NBNCallbacks); !bytes.Contains(o.NBNCallbacks, []byte("service level change")) || bytes.Contains(o.NBNCallbacks, []byte("speed")) {
		t.Fatalf("accepted callbacks: %s", r)
	}
	// One change at a time.
	if code, body := post(t, url+"/services/"+itoa(id)+"/modify", map[string]string{"order_type": "esla", "esla": "enhanced-12-24-7", "order_after": "2026-11-01"}); code != 422 || body["error"] != "service has an order in progress" {
		t.Fatalf("a second change while one is in flight: %d %v", code, body)
	}
	srv.age(lo.OrderID, 60*time.Second)
	if o, _, _ = c.GetOrder(lo.OrderID); o.State != "provisioning" || bytes.Contains(o.NBNCallbacks, []byte("speed")) {
		t.Fatalf("after a minute: %+v %s", o, o.NBNCallbacks)
	}
	srv.age(lo.OrderID, 60*time.Second)
	o, _, _ = c.GetOrder(lo.OrderID)
	if o.Status != "completed" || o.NbnESLA != "Enhanced - 4 (24/7)" || !bytes.Contains(o.NBNCallbacks, []byte("Service level change complete")) {
		t.Fatalf("after two minutes: %+v %s", o, o.NBNCallbacks)
	}
	ls, err := c.GetService(id)
	if err != nil || ls.NbnESLA != "Enhanced - 4 (24/7)" || ls.ESLACode() != "enhanced-4-24-7" {
		t.Fatalf("service after the esla order: %+v %v", ls, err)
	}

	// A plan modify keeps the eSLA.
	pl, _, err := c.ModifyService(id, leaptel.ModifyServiceParams{PlanID: 3541})
	if err != nil {
		t.Fatal(err)
	}
	srv.age(pl.OrderID, 10*time.Minute)
	c.GetOrder(pl.OrderID)
	if ls, _ := c.GetService(id); ls.NbnESLA != "Enhanced - 4 (24/7)" || ls.ProductID != "plan:3541" {
		t.Fatalf("after a plan modify: %+v", ls)
	}

	// The dev-only set-esla simulates a portal change (or one a modify dropped).
	if code, body := post(t, url+"/services/"+itoa(id)+"/set-esla", map[string]string{"esla": "standard"}); code != 200 || body["nbn_esla"] != "Standard" {
		t.Fatalf("set-esla: %d %v", code, body)
	}
	if code, _ := post(t, url+"/services/"+itoa(id)+"/set-esla", map[string]string{"esla": "gold"}); code != 400 {
		t.Fatalf("set-esla with an unknown level: %d", code)
	}

	// A connect carrying an eSLA completes onto it; an unknown one is refused.
	if id2 := liveService(t, srv, c, "enhanced-12-24-7"); true {
		if ls, _ := c.GetService(id2); ls.NbnESLA != "Enhanced - 12 (24/7)" {
			t.Fatalf("connect with esla: %+v", ls)
		}
	}
	if code, body := post(t, url+"/orders", map[string]string{"customer_id": "1", "location_id": "LOC000000000004", "plan_id": "3535", "esla": "platinum"}); code != 400 || body["error"] != "invalid esla" {
		t.Fatalf("connect with an unknown esla: %d %v", code, body)
	}

	// A service that isn't active can't change level.
	srv.state.mu.Lock()
	srv.state.services[id].State = "cancelled"
	srv.state.mu.Unlock()
	if code, body := post(t, url+"/services/"+itoa(id)+"/modify", map[string]string{"order_type": "esla", "esla": "enhanced-12-24-7", "order_after": "2026-11-01"}); code != 422 || body["error"] != "service is not active" {
		t.Fatalf("a change on a cancelled service: %d %v", code, body)
	}
}

func TestFakeLeaptelRejectsModifyWithoutPlanForPlanType(t *testing.T) {
	srv, c, url := fakeAPI(t)
	id := liveService(t, srv, c, "")
	path := url + "/services/" + itoa(id) + "/modify"
	for _, tc := range []struct {
		fields map[string]string
		code   int
	}{
		{map[string]string{"order_type": "plan", "order_after": "2026-11-01"}, 400},                            // no plan
		{map[string]string{"order_type": "plan", "plan_id": "3541"}, 400},                                      // no order_after
		{map[string]string{"plan_id": "3541", "order_after": "2026-11-01"}, 400},                               // no order_type
		{map[string]string{"order_type": "speed", "plan_id": "3541", "order_after": "2026-11-01"}, 400},        // unknown order_type
		{map[string]string{"order_type": "esla", "esla": "enhanced-4-24-7"}, 400},                              // esla without order_after
		{map[string]string{"order_type": "esla", "esla": "enhanced-4-24-7", "order_after": "2026-11-01"}, 200}, // no plan needed
		{map[string]string{"order_type": "plan", "plan_id": "3541", "order_after": "2026-11-01"}, 200},         // a speed change
	} {
		if code, body := post(t, path, tc.fields); code != tc.code {
			t.Errorf("%v: %d %v, want %d", tc.fields, code, body, tc.code)
		}
	}
}

func TestFakeLeaptelLoadsOldState(t *testing.T) {
	file := filepath.Join(t.TempDir(), "old.json")
	// A state file written before eSLAs: no nbn_esla on services, no ESLA on orders.
	old := `{"next_customer_id":1001,"next_order_id":2001,"next_service_id":3001,"next_test_id":4000,"next_appointment_id":5000,
		"customers":{},"services":{"3001":{"service_id":3001,"customer_id":1001,"state":"active","identifier":"LOC000000000002"}},
		"orders":{"2001":{"OrderID":2001,"ServiceID":3001,"CustomerID":1001,"LocationID":"LOC000000000002","Action":"","CreatedAt":"2026-10-01T00:00:00Z"}},
		"appointments":{},"assurance_runs":{}}`
	if err := os.WriteFile(file, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}
	srv := newFakeServer(file)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	c := leaptel.NewClient(ts.URL, "u", "p")
	ls, err := c.GetService(3001)
	if err != nil || ls.NbnESLA != "Standard" || ls.ESLACode() != "standard" {
		t.Fatalf("an old service loads on Standard: %+v %v", ls, err)
	}
	if o, _, err := c.GetOrder(2001); err != nil || o.NbnESLA != "Standard" {
		t.Fatalf("an old order reads Standard: %+v %v", o, err)
	}
	// And it saves the new field back.
	if _, _, err := c.ModifyESLA(3001, "enhanced-12-24-7"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	if !bytes.Contains(b, []byte(`"nbn_esla": "Standard"`)) || !bytes.Contains(b, []byte(`"ESLA": "enhanced-12-24-7"`)) {
		t.Fatalf("state file after a change:\n%s", b)
	}
}

func itoa(n int) string { return padInt(n, 0) }
