package leaptel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Leaptel's POST /orders answers with its ids quoted (GET /orders/{id} doesn't).
func TestCreateOrderReadsQuotedIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"location_id":"LOC000069012224","order_id":"598728","service_id":"241989"}`))
	}))
	defer srv.Close()
	b := &memBreaker{}
	lo, _, err := NewClient(srv.URL, "u", "p").WithBreaker(b).CreateOrder(CreateOrderParams{CustomerID: 1, LocationID: "LOC000069012224", PlanID: 3532})
	if err != nil {
		t.Fatal(err)
	}
	if tripped, _ := b.Tripped(context.Background()); tripped {
		t.Fatal("a quoted order id tripped the stop")
	}
	if lo.OrderID != 598728 || lo.ServiceID != 241989 || lo.LocationID != "LOC000069012224" {
		t.Fatalf("read %+v, want order 598728 service 241989", lo)
	}
}

func TestCreateOrderReadsNumericIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"order_id":77,"service_id":88}`))
	}))
	defer srv.Close()
	lo, _, err := NewClient(srv.URL, "u", "p").CreateOrder(CreateOrderParams{CustomerID: 1, LocationID: "LOC1", PlanID: 1})
	if err != nil || lo.OrderID != 77 || lo.ServiceID != 88 {
		t.Fatalf("read %+v err %v, want 77/88", lo, err)
	}
}
