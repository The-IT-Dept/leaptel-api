package leaptel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"
)

// outageServer answers every request with status and body, and keeps the
// queries it saw.
func outageServer(t *testing.T, status int, body string) (*httptest.Server, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var seen []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Query())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return append([]url.Values(nil), seen...)
	}
}

var outageDay = time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)

func TestListOutagesMissingKeyIsEmpty(t *testing.T) {
	srv, _ := outageServer(t, 200, `{"pagination":{"current_page":2,"total_pages":1}}`)
	c := NewClient(srv.URL, "u", "p")
	p, err := c.ListServiceOutages(context.Background(), 114412, OutageQuery{StartDate: outageDay, Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Outages) != 0 || p.Pagination.CurrentPage != 2 || p.Pagination.TotalPages != 1 {
		t.Fatalf("page: %+v", p)
	}
	p, err = c.ListOutages(context.Background(), OutageQuery{StartDate: outageDay})
	if err != nil || len(p.Outages) != 0 {
		t.Fatalf("national page: %+v, %v", p, err)
	}
}

func TestGetOutageEmptyObjectIsNil(t *testing.T) {
	for _, body := range []string{`{}`, ` {} `, `[]`} {
		srv, _ := outageServer(t, 200, body)
		o, err := NewClient(srv.URL, "u", "p").GetOutage(context.Background(), 1)
		if o != nil || err != nil {
			t.Fatalf("%s: got %+v, %v; want nil, nil", body, o, err)
		}
	}
}

func TestGetOutageDecodes(t *testing.T) {
	srv, _ := outageServer(t, 200, `{"outage_id":165277,"type_id":2,"provider":"nbn","provider_outage_id":"INC000022617583"}`)
	o, err := NewClient(srv.URL, "u", "p").GetOutage(context.Background(), 165277)
	if err != nil || o == nil || o.OutageID != 165277 || o.ProviderOutageID != "INC000022617583" {
		t.Fatalf("got %+v, %v", o, err)
	}
	srv, _ = outageServer(t, 500, `{"message":"Server Error"}`)
	if _, err := NewClient(srv.URL, "u", "p").GetOutage(context.Background(), 1); err == nil {
		t.Fatal("a 500 must be an error")
	}
}

func TestOutageDecodesPHPShapes(t *testing.T) {
	decode := func(t *testing.T, s string) Outage {
		t.Helper()
		var o Outage
		if err := json.Unmarshal([]byte(s), &o); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		return o
	}

	t.Run("empty scope array", func(t *testing.T) {
		o := decode(t, `{"outage_id":1,"type_id":2,"scope":[]}`)
		if len(o.Scope.Service)+len(o.Scope.POI)+len(o.Scope.StateTerritory)+len(o.Scope.Other) != 0 {
			t.Fatalf("scope: %+v", o.Scope)
		}
	})
	t.Run("missing and null scope", func(t *testing.T) {
		for _, s := range []string{`{"outage_id":1}`, `{"outage_id":1,"scope":null}`, `{"outage_id":1,"scope":{}}`} {
			o := decode(t, s)
			if !reflect.DeepEqual(o.Scope, OutageScope{}) {
				t.Fatalf("%s: scope %+v", s, o.Scope)
			}
		}
	})
	t.Run("scope values as numbers and strings", func(t *testing.T) {
		o := decode(t, `{"outage_id":1,"scope":{"service":[242005,"114412"],"poi":["2BLV"],"estate":["X"]}}`)
		if !reflect.DeepEqual(o.Scope.Service, []string{"242005", "114412"}) {
			t.Fatalf("service: %#v", o.Scope.Service)
		}
		if !reflect.DeepEqual(o.Scope.POI, []string{"2BLV"}) {
			t.Fatalf("poi: %#v", o.Scope.POI)
		}
		if !reflect.DeepEqual(o.Scope.Other, []string{"estate"}) {
			t.Fatalf("other: %#v", o.Scope.Other)
		}
	})
	t.Run("scope list sent as an object with index keys", func(t *testing.T) {
		// A PHP array with a gap in its keys is encoded as an object.
		o := decode(t, `{"outage_id":1,"scope":{"poi":{"0":"2ALB","2":"2BLV","10":"2PTH","3":"2GOS"},"state_territory":["NSW"]}}`)
		if !reflect.DeepEqual(o.Scope.POI, []string{"2ALB", "2BLV", "2GOS", "2PTH"}) {
			t.Fatalf("poi: %#v", o.Scope.POI)
		}
		if !reflect.DeepEqual(o.Scope.StateTerritory, []string{"NSW"}) {
			t.Fatalf("state: %#v", o.Scope.StateTerritory)
		}
	})
	t.Run("other scope keys are sorted names", func(t *testing.T) {
		o := decode(t, `{"outage_id":1,"scope":{"tower_id":["9"],"estate":["X"],"postcode":[2000]}}`)
		if !reflect.DeepEqual(o.Scope.Other, []string{"estate", "postcode", "tower_id"}) {
			t.Fatalf("other: %#v", o.Scope.Other)
		}
	})
	t.Run("type_id as a string", func(t *testing.T) {
		if o := decode(t, `{"outage_id":"7","type_id":"2"}`); o.TypeID != 2 || o.OutageID != 7 {
			t.Fatalf("ids: %d %d", o.OutageID, o.TypeID)
		}
	})
	t.Run("null and blank public_timezone", func(t *testing.T) {
		if o := decode(t, `{"outage_id":1,"public_timezone":null}`); o.PublicTimezone != "" {
			t.Fatalf("null: %q", o.PublicTimezone)
		}
		if o := decode(t, `{"outage_id":1,"public_timezone":""}`); o.PublicTimezone != "" {
			t.Fatalf("blank: %q", o.PublicTimezone)
		}
	})
	t.Run("missing, null and empty outage_updates", func(t *testing.T) {
		for _, s := range []string{`{"outage_id":1}`, `{"outage_id":1,"outage_updates":null}`, `{"outage_id":1,"outage_updates":[]}`} {
			if o := decode(t, s); len(o.Updates) != 0 {
				t.Fatalf("%s: updates %+v", s, o.Updates)
			}
		}
	})
	t.Run("raw is the input bytes", func(t *testing.T) {
		in := `{"outage_id":165277, "type_id":2,"provider":"nbn","scope":[],"outage_updates":[{"update_date":"2026-11-06 14:22:00","updated_by":"nbn_outage_notification","short_update":"x","long_update":"Current ETR is 4pm.","send_sms":"no","send_email":"no","resolved":"no"}]}`
		o := decode(t, in)
		if string(o.Raw) != in {
			t.Fatalf("raw: %s", o.Raw)
		}
		want := OutageUpdate{UpdateDate: "2026-11-06 14:22:00", UpdatedBy: "nbn_outage_notification", ShortUpdate: "x", LongUpdate: "Current ETR is 4pm.", SendSMS: "no", SendEmail: "no", Resolved: "no"}
		if len(o.Updates) != 1 || o.Updates[0] != want {
			t.Fatalf("updates: %+v", o.Updates)
		}
	})
	t.Run("every field", func(t *testing.T) {
		o := decode(t, `{"outage_id":145118,"type_id":2,"outage_type":"unplanned outage","notification_description":"d","start_time":"2026-11-06 01:00:00","finis_time":"0000-00-00 00:00:00","provider":"vocus","provider_outage_id":"CHG-1","provider_outage_information":"i","provider_location":"Belair, SA","provider_service_id":"","provider_url":"","visibility":"customer","outage_status":"published","timezone":"Australia/Melbourne","public_timezone":"Australia/Sydney","ts":"2026-11-06 12:00:00","update_time":"x","service_type":"NBN"}`)
		want := Outage{OutageID: 145118, TypeID: 2, OutageType: "unplanned outage", NotificationDescription: "d", StartTime: "2026-11-06 01:00:00", FinisTime: "0000-00-00 00:00:00", Provider: "vocus", ProviderOutageID: "CHG-1", ProviderOutageInformation: "i", ProviderLocation: "Belair, SA", Visibility: "customer", OutageStatus: "published", Timezone: "Australia/Melbourne", PublicTimezone: "Australia/Sydney", TS: "2026-11-06 12:00:00", ServiceType: "NBN"}
		o.Raw = nil
		if !reflect.DeepEqual(o, want) {
			t.Fatalf("got  %+v\nwant %+v", o, want)
		}
	})
	t.Run("a page decodes each record with its own raw bytes", func(t *testing.T) {
		in := `{"pagination":{"current_page":1,"total_pages":3,"next_page":2},"outages":[{"outage_id":2,"scope":[]},{"outage_id":1,"type_id":"16"}]}`
		var p OutagesPage
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Outages) != 2 || string(p.Outages[0].Raw) != `{"outage_id":2,"scope":[]}` || p.Outages[1].TypeID != 16 {
			t.Fatalf("page: %+v", p)
		}
		if p.Pagination.NextPage == nil || *p.Pagination.NextPage != 2 {
			t.Fatalf("pagination: %+v", p.Pagination)
		}
	})
	t.Run("a malformed record fails only itself", func(t *testing.T) {
		bad := `{"outage_id":"5","provider_location":{"suburb":"Belair"}}`
		in := `{"outages":[{"outage_id":6,"type_id":2},` + bad + `,{"outage_id":"x7"},"junk",{"outage_id":4,"provider":"nbn"}]}`
		var p OutagesPage
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatalf("the page failed: %v", err)
		}
		if len(p.Outages) != 5 {
			t.Fatalf("%d records, want 5", len(p.Outages))
		}
		good := []Outage{p.Outages[0], p.Outages[4]}
		if good[0].OutageID != 6 || good[0].TypeID != 2 || good[0].Err != nil || good[1].OutageID != 4 ||
			good[1].Provider != "nbn" || good[1].Err != nil {
			t.Fatalf("the good records: %+v", good)
		}
		if o := p.Outages[1]; o.Err == nil || o.OutageID != 5 || string(o.Raw) != bad || o.ProviderLocation != "" {
			t.Fatalf("the malformed record: %+v", o)
		}
		for _, o := range p.Outages[2:4] {
			if o.Err == nil || o.OutageID != 0 {
				t.Fatalf("a record whose id can't be read: %+v", o)
			}
		}
	})
}

func TestOutageQueryAlwaysSendsStartDate(t *testing.T) {
	srv, seen := outageServer(t, 200, `{"pagination":{},"outages":[]}`)
	c := NewClient(srv.URL, "u", "p")
	ctx := context.Background()
	// 13:30 UTC on 6 Nov is 00:30 on 7 Nov in Melbourne (AEDT, +11).
	late := time.Date(2026, 11, 6, 13, 30, 0, 0, time.UTC)
	early := time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC)
	if _, err := c.ListOutages(ctx, OutageQuery{StartDate: early, SortBy: "outage_id", SortOrder: "desc", Page: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListOutages(ctx, OutageQuery{StartDate: late}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListServiceOutages(ctx, 9000001, OutageQuery{StartDate: late}); err != nil {
		t.Fatal(err)
	}
	q := seen()
	if len(q) != 3 {
		t.Fatalf("saw %d calls", len(q))
	}
	if got := q[0].Get("start_date"); got != "2026-11-06" {
		t.Fatalf("start_date %q, want 2026-11-06", got)
	}
	if q[0].Get("sort_by") != "outage_id" || q[0].Get("sort_order") != "desc" || q[0].Get("page") != "2" {
		t.Fatalf("query: %v", q[0])
	}
	for i := 1; i < 3; i++ {
		if got, want := q[i].Get("start_date"), "2026-11-07"; got != want {
			t.Fatalf("call %d: start_date %q, want %q", i, got, want)
		}
		if _, ok := q[i]["page"]; ok {
			t.Fatalf("call %d: page sent when unset: %v", i, q[i])
		}
		if _, ok := q[i]["sort_by"]; ok {
			t.Fatalf("call %d: sort_by sent when unset: %v", i, q[i])
		}
	}
	if _, err := c.ListOutages(ctx, OutageQuery{}); err == nil {
		t.Fatal("a query with no start date must be refused")
	}
	if n := len(seen()); n != 3 {
		t.Fatalf("a query with no start date was sent (%d calls)", n)
	}
}

func TestOutageReadRecordedWithoutBody(t *testing.T) {
	page := `{"pagination":{"current_page":1},"outages":[{"outage_id":1}]}`
	for _, c := range []struct {
		name     string
		status   int
		body     string
		call     func(*Client) error
		wantPath string
		kept     bool
	}{
		{"national read", 200, page, func(c *Client) error {
			_, err := c.ListOutages(context.Background(), OutageQuery{StartDate: outageDay})
			return err
		}, "/outages?start_date=2026-11-06", false},
		{"one record", 200, `{"outage_id":1}`, func(c *Client) error {
			_, err := c.GetOutage(context.Background(), 1)
			return err
		}, "/outages/1", false},
		{"per-service read", 200, page, func(c *Client) error {
			_, err := c.ListServiceOutages(context.Background(), 5, OutageQuery{StartDate: outageDay})
			return err
		}, "/services/5/outages?start_date=2026-11-06", false},
		{"failed read keeps its body", 500, `{"message":"Server Error"}`, func(c *Client) error {
			_, err := c.ListOutages(context.Background(), OutageQuery{StartDate: outageDay})
			if err == nil {
				return fmt.Errorf("want an error for a 500")
			}
			return nil
		}, "/outages?start_date=2026-11-06", true},
		{"service read keeps its body", 200, `{"service_id":1}`, func(c *Client) error {
			_, err := c.GetService(1)
			return err
		}, "/services/1", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := outageServer(t, c.status, c.body)
			sink := &memSink{}
			if err := c.call(NewClient(srv.URL, "u", "p").WithSink(sink)); err != nil {
				t.Fatal(err)
			}
			got := sink.only(t)
			if got.Path != c.wantPath || got.Status != c.status {
				t.Fatalf("call: %+v", got)
			}
			want := fmt.Sprintf("(not kept: %d bytes)", len(c.body))
			if c.kept {
				want = c.body
			}
			if got.ResponseBody != want || got.Truncated {
				t.Fatalf("response body %q (truncated %v), want %q", got.ResponseBody, got.Truncated, want)
			}
		})
	}
}

func TestIsOutageRead(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/outages", true},
		{"GET", "/outages?start_date=2026-11-06", true},
		{"GET", "/outages/165277", true},
		{"GET", "/services/242005/outages?page=2", true},
		{"GET", "/services/242005/outages", true},
		{"POST", "/outages", false},
		{"POST", "/services/1/outages", false},
		{"GET", "/services/1", false},
		{"GET", "/services/abc/outages", false},
		{"GET", "/outagesx", false},
		{"GET", "/services/1/outages-history", false},
	} {
		if got := isOutageRead(c.method, c.path); got != c.want {
			t.Errorf("isOutageRead(%s %s) = %v", c.method, c.path, got)
		}
	}
}

func TestPHPEmpty(t *testing.T) {
	for in, want := range map[string]bool{
		``: true, `null`: true, ` null `: true, `[]`: true, `{}`: true, `[ ]`: true, "{\n}": true,
		`[1]`: false, `{"a":1}`: false, `""`: false, `0`: false, `[]x`: false, `{]`: false, `[}`: false,
	} {
		if got := phpEmpty([]byte(in)); got != want {
			t.Errorf("phpEmpty(%q) = %v", in, got)
		}
	}
}
