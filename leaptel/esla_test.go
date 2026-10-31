package leaptel

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestNormaliseESLA(t *testing.T) {
	for in, want := range map[string]string{
		"Standard":                    "standard",
		"Enhanced - 12 (24/7)":        "enhanced-12-24-7",
		"Enhanced-4 (24/7)":           "enhanced-4-24-7",
		"Enhanced - 4":                "enhanced-4",
		"enhanced-4-24-7":             "enhanced-4-24-7",
		"Enhanced (90 Day)-12 (24/7)": "enhanced(90day)-12(24/7)",
		"":                            "",
		"  ":                          "",
		"standard":                    "standard",
		"Enhanced - 8 (24/7)":         "enhanced-8-24-7",
		"enhanced-12":                 "enhanced-12",
	} {
		if got := NormaliseESLA(in); got != want {
			t.Errorf("NormaliseESLA(%q) = %q, want %q", in, got, want)
		}
	}
}

// formServer records each request's multipart fields, answering with body.
type formServer struct {
	mu    sync.Mutex
	paths []string
	forms []map[string]string
}

func (f *formServer) handler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fields := map[string]string{}
		if _, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && params["boundary"] != "" {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				b, _ := io.ReadAll(p)
				fields[p.FormName()] = string(b)
			}
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.forms = append(f.forms, fields)
		f.mu.Unlock()
		w.Write([]byte(body))
	}
}

func TestModifyESLASendsOrderTypeESLAWithoutPlanID(t *testing.T) {
	fs := &formServer{}
	srv := httptest.NewServer(fs.handler(`{"order_id":"9001","service_id":"242005"}`))
	defer srv.Close()
	// 14:30 UTC on 31 October is 1 November in Melbourne (AEDT, +11).
	nowFn = func() time.Time { return time.Date(2026, 10, 31, 14, 30, 0, 0, time.UTC) }
	defer func() { nowFn = time.Now }()

	lo, _, err := NewClient(srv.URL, "u", "p").ModifyESLA(242005, ESLAEnhanced4x24)
	if err != nil {
		t.Fatal(err)
	}
	if lo.OrderID != 9001 {
		t.Fatalf("order id %d, want 9001", lo.OrderID)
	}
	if len(fs.forms) != 1 || fs.paths[0] != "POST /services/242005/modify" {
		t.Fatalf("requests: %v", fs.paths)
	}
	f := fs.forms[0]
	if f["order_type"] != "esla" || f["esla"] != "enhanced-4-24-7" || f["order_after"] != "2026-11-01" {
		t.Fatalf("form %v: want order_type esla, esla enhanced-4-24-7, order_after 2026-11-01 (Melbourne)", f)
	}
	if _, ok := f["plan_id"]; ok {
		t.Fatalf("an eSLA modify must carry no plan_id: %v", f)
	}
	if _, ok := f["product_id"]; ok {
		t.Fatalf("an eSLA modify must carry no product_id: %v", f)
	}
}

func TestCreateOrderSendsESLAOnlyWhenSet(t *testing.T) {
	fs := &formServer{}
	srv := httptest.NewServer(fs.handler(`{"order_id":77,"service_id":88}`))
	defer srv.Close()
	c := NewClient(srv.URL, "u", "p")
	for _, level := range []string{"", ESLAStandard, ESLAEnhanced12x24} {
		if _, _, err := c.CreateOrder(CreateOrderParams{CustomerID: 1, LocationID: "LOC1", PlanID: 3535, ESLA: level}); err != nil {
			t.Fatal(err)
		}
	}
	if len(fs.forms) != 3 {
		t.Fatalf("%d requests", len(fs.forms))
	}
	for i, f := range fs.forms[:2] {
		if v, ok := f["esla"]; ok {
			t.Fatalf("order %d sent esla %q; none or standard sends nothing", i, v)
		}
	}
	if fs.forms[2]["esla"] != ESLAEnhanced12x24 {
		t.Fatalf("an enhanced connect must carry esla: %v", fs.forms[2])
	}
}

func TestServiceReadsNbnESLA(t *testing.T) {
	for body, want := range map[string]string{
		`{"service_id":1,"nbn_esla":"Enhanced - 4 (24/7)"}`:               "enhanced-4-24-7",
		`{"service_id":1,"esla":"enhanced-12-24-7"}`:                      "enhanced-12-24-7",
		`{"service_id":1,"nbn_esla":"Standard","esla":"enhanced-4-24-7"}`: "standard",
		`{"service_id":1}`: "",
		`{"service_id":1,"nbn_esla":"","esla":"standard"}`:                       "standard",
		`{"service_id":1,"nbn_esla":"Enhanced (90 Day)-12 (24/7)","esla":"std"}`: "enhanced(90day)-12(24/7)",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		svc, err := NewClient(srv.URL, "u", "p").GetService(1)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := svc.ESLACode(); got != want {
			t.Errorf("%s: ESLACode %q, want %q", body, got, want)
		}
	}
	// The order read carries it too.
	var o Order
	if err := json.Unmarshal([]byte(`{"order_id":1,"nbn_esla":"Enhanced - 12 (24/7)"}`), &o); err != nil || NormaliseESLA(o.NbnESLA) != ESLAEnhanced12x24 {
		t.Fatalf("order nbn_esla: %+v %v", o, err)
	}
}
