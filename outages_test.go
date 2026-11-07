package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/the-it-dept/leaptel-api/leaptel"
)

// call sends method url as user (Basic Auth), with an optional JSON body.
func call(t *testing.T, method, url, user, body string) (int, http.Header, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.SetBasicAuth(user, "p")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func get(t *testing.T, url string) (int, http.Header, []byte) {
	t.Helper()
	return call(t, http.MethodGet, url, "u", "")
}

func mustPost(t *testing.T, url, body string) {
	t.Helper()
	if st, _, b := call(t, http.MethodPost, url, "u", body); st != http.StatusOK {
		t.Fatalf("POST %s: %d %s", url, st, b)
	}
}

func decodePage(t *testing.T, b []byte) (leaptel.OutagesPage, map[string]json.RawMessage) {
	t.Helper()
	var p leaptel.OutagesPage
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("page: %v: %s", err, b)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	return p, keys
}

func ids(p leaptel.OutagesPage) []int {
	var out []int
	for _, o := range p.Outages {
		out = append(out, o.OutageID)
	}
	return out
}

func TestFakeOutageFixturesLoad(t *testing.T) {
	for _, id := range []int{140236, 129126, 164915, 164914, 133939, 165277, 165406, 145118, 98112, 131557, 163595, 47099, 34707, 4551, 165408} {
		if _, ok := outageFixtures[id]; !ok {
			t.Errorf("fixture record %d not loaded", id)
		}
	}
	// The detail copy wins over the national page's copy of the same record.
	var o leaptel.Outage
	if err := json.Unmarshal(outageFixtures[165406].raw, &o); err != nil || len(o.Updates) == 0 {
		t.Fatalf("165406: %+v %v", o, err)
	}
}

func TestFakeOutagesPagesNewestFirst(t *testing.T) {
	_, _, url := fakeAPI(t)
	for i := 1; i <= 60; i++ {
		mustPost(t, url+"/outages", fmt.Sprintf(`{"outage_id":%d,"type_id":2,"provider":"nbn_direct","start_time":"2026-12-01 13:00:00","finis_time":"2026-12-01 19:00:00","outage_updates":[]}`, 9100000+i))
	}
	total := len(outageFixtures) + 60
	st, _, b := get(t, url+"/outages?sort_by=outage_id&sort_order=desc")
	if st != http.StatusOK {
		t.Fatalf("page 1: %d %s", st, b)
	}
	p1, _ := decodePage(t, b)
	got := ids(p1)
	if len(got) != 50 {
		t.Fatalf("page 1 has %d records, want 50", len(got))
	}
	if got[0] != 9100060 {
		t.Fatalf("page 1 starts at %d, want the newest (9100060)", got[0])
	}
	if p1.Pagination.NextPage == nil || *p1.Pagination.NextPage != 2 || p1.Pagination.TotalRecords != total || p1.Pagination.RecordsPerPage != 50 {
		t.Fatalf("page 1 pagination: %+v", p1.Pagination)
	}
	_, _, b = get(t, url+"/outages?sort_by=outage_id&sort_order=desc&page=2")
	p2, _ := decodePage(t, b)
	all := append(got, ids(p2)...)
	for i := 1; i < len(all); i++ {
		if all[i] >= all[i-1] {
			t.Fatalf("not strictly descending at %d: %d then %d", i, all[i-1], all[i])
		}
	}
	if len(all) != total || p2.Pagination.NextPage != nil || p2.Pagination.PreviousPage == nil || *p2.Pagination.PreviousPage != 1 {
		t.Fatalf("page 2: %d records in all (want %d), pagination %+v", len(all), total, p2.Pagination)
	}

	// Ascending by start date, from a date on.
	_, _, b = get(t, url+"/outages?sort_by=start_date&sort_order=asc&start_date=2026-11-06")
	p, _ := decodePage(t, b)
	if len(p.Outages) == 0 {
		t.Fatal("no records from 6 Nov on")
	}
	for i, o := range p.Outages {
		if o.StartTime < "2026-11-06" {
			t.Fatalf("%d starts %s, before the start_date", o.OutageID, o.StartTime)
		}
		if i > 0 && o.StartTime < p.Outages[i-1].StartTime {
			t.Fatalf("not ascending by start: %s then %s", p.Outages[i-1].StartTime, o.StartTime)
		}
	}
	// Other filters are ignored, as upstream.
	_, _, b1 := get(t, url+"/outages?sort_by=outage_id&sort_order=desc&provider=nbn&poi=2BLV")
	_, _, b2 := get(t, url+"/outages?sort_by=outage_id&sort_order=desc")
	if !bytes.Equal(b1, b2) {
		t.Fatal("a provider or poi filter changed the list")
	}
}

func TestFakeOutageUnknownIsEmptyObject(t *testing.T) {
	_, _, url := fakeAPI(t)
	for _, path := range []string{"/outages/1", "/outages/types"} {
		st, _, b := get(t, url+path)
		if st != http.StatusOK || strings.TrimSpace(string(b)) != "{}" {
			t.Fatalf("%s: %d %s, want 200 {}", path, st, b)
		}
	}
	st, _, b := get(t, url+"/outages/145118")
	var o leaptel.Outage
	if st != http.StatusOK || json.Unmarshal(b, &o) != nil || o.OutageID != 145118 || len(o.Scope.POI) == 0 {
		t.Fatalf("a known record: %d %s", st, b)
	}
}

func TestFakeServiceOutagesPastLastPageHasNoKey(t *testing.T) {
	_, _, url := fakeAPI(t)
	for _, path := range []string{"/services/9000001/outages?page=99", "/services/12345/outages"} {
		st, _, b := get(t, url+path)
		p, keys := decodePage(t, b)
		if st != http.StatusOK {
			t.Fatalf("%s: %d", path, st)
		}
		if _, ok := keys["outages"]; ok {
			t.Fatalf("%s: has an outages key: %s", path, b)
		}
		if _, ok := keys["pagination"]; !ok || p.Pagination.TotalPages != 1 || p.Pagination.NextPage != nil {
			t.Fatalf("%s: pagination %s", path, b)
		}
	}
	_, _, b := get(t, url+"/services/9000001/outages?page=99")
	if p, _ := decodePage(t, b); p.Pagination.CurrentPage != 99 || p.Pagination.TotalRecords == 0 {
		t.Fatalf("page 99 pagination: %+v", p.Pagination)
	}
}

func TestFakeServiceOutagesMatchScopeService(t *testing.T) {
	_, _, url := fakeAPI(t)
	_, _, b := get(t, url+"/services/9000001/outages")
	p, _ := decodePage(t, b)
	found := map[int]leaptel.Outage{}
	for _, o := range p.Outages {
		found[o.OutageID] = o
		if string(o.Raw) == "" || !bytes.Contains(o.Raw, []byte(`"scope"`)) {
			t.Fatalf("%d: a per-service copy always carries scope: %s", o.OutageID, o.Raw)
		}
	}
	for _, id := range []int{140236, 133939} {
		if o, ok := found[id]; !ok || len(o.Scope.Service) == 0 || o.Scope.Service[0] != "9000001" {
			t.Fatalf("%d in the per-service list: %+v (found %v)", id, o.Scope, ok)
		}
	}
	// The national copies of the same records carry no service scope.
	for _, id := range []int{140236, 133939} {
		_, _, b := get(t, url+"/outages/"+strconv.Itoa(id))
		var o leaptel.Outage
		if err := json.Unmarshal(b, &o); err != nil || o.OutageID != id {
			t.Fatalf("%d: %v %s", id, err, b)
		}
		if len(o.Scope.Service) != 0 || bytes.Contains(b, []byte(`"service"`)) {
			t.Fatalf("%d: the national copy names services: %s", id, b)
		}
	}
	_, _, b = get(t, url+"/outages?sort_by=outage_id&sort_order=desc")
	if bytes.Contains(b, []byte(`"service":`)) {
		t.Fatal("the national list names services")
	}
}

func TestFakeOutageRoutesRateLimited(t *testing.T) {
	_, _, url := fakeAPI(t)
	paths := []string{"/outages?start_date=2026-11-06", "/outages/165277", "/services/9000001/outages"}
	for i := 0; i < 10; i++ {
		st, h, b := get(t, url+paths[i%3])
		if st != http.StatusOK {
			t.Fatalf("call %d: %d %s", i+1, st, b)
		}
		if h.Get("x-ratelimit-limit") != "10" || h.Get("x-ratelimit-remaining") != strconv.Itoa(9-i) {
			t.Fatalf("call %d: limit %q remaining %q", i+1, h.Get("x-ratelimit-limit"), h.Get("x-ratelimit-remaining"))
		}
	}
	st, h, b := get(t, url+paths[0])
	if st != http.StatusTooManyRequests || string(b) != `{"message":"Too Many Attempts."}` {
		t.Fatalf("11th call: %d %s", st, b)
	}
	if h.Get("x-ratelimit-limit") != "10" || h.Get("x-ratelimit-remaining") != "0" || h.Get("Retry-After") != "" {
		t.Fatalf("11th call headers: %v", h)
	}
	// Per user: another key still has its 10.
	if st, _, _ := call(t, http.MethodGet, url+paths[1], "other", ""); st != http.StatusOK {
		t.Fatalf("another user: %d", st)
	}
	// Dev injection and other routes aren't counted; /services/{id} states its own bucket.
	mustPost(t, url+"/outages", `{"outage_id":9200001,"type_id":2,"start_time":"2026-12-01 13:00:00"}`)
	_, h, _ = get(t, url+"/services/1")
	if h.Get("x-ratelimit-limit") != "15" {
		t.Fatalf("/services/{id} limit %q, want 15", h.Get("x-ratelimit-limit"))
	}
}

func TestFakeOutageLimitWindowResets(t *testing.T) {
	now := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)
	lim := newOutageLimiter(func() time.Time { return now })
	for i := 0; i < 10; i++ {
		if ok, _ := lim.take("u"); !ok {
			t.Fatalf("call %d refused", i+1)
		}
	}
	if ok, _ := lim.take("u"); ok {
		t.Fatal("11th call allowed")
	}
	now = now.Add(59 * time.Second)
	if ok, _ := lim.take("u"); ok {
		t.Fatal("allowed within the minute")
	}
	now = now.Add(time.Second)
	if ok, remaining := lim.take("u"); !ok || remaining != 9 {
		t.Fatalf("a minute on: ok %v remaining %d", ok, remaining)
	}
}

func TestFakeAttachMakesServiceMatch(t *testing.T) {
	_, c, url := fakeAPI(t)
	ctx := context.Background()
	q := leaptel.OutageQuery{StartDate: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	has := func(id int) bool {
		t.Helper()
		p, err := c.ListServiceOutages(ctx, 9000001, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range p.Outages {
			if o.OutageID == id {
				if len(o.Scope.Service) == 0 || o.Scope.Service[len(o.Scope.Service)-1] != "9000001" {
					t.Fatalf("the attached copy's scope: %+v", o.Scope)
				}
				return true
			}
		}
		return false
	}
	if has(164915) {
		t.Fatal("164915 matched before it was attached")
	}
	mustPost(t, url+"/services/9000001/outages", `{"outage_id":164915}`)
	mustPost(t, url+"/services/9000001/outages", `{"outage_id":"164915"}`) // twice is once
	if !has(164915) {
		t.Fatal("164915 isn't in the service's list after attaching")
	}
	p, _ := c.ListServiceOutages(ctx, 9000001, q)
	n := 0
	for _, o := range p.Outages {
		if o.OutageID == 164915 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("164915 listed %d times", n)
	}
	if o, err := c.GetOutage(ctx, 164915); err != nil || o == nil || len(o.Scope.Service) != 0 {
		t.Fatalf("the national copy after attaching: %+v %v", o, err)
	}
	if st, _, _ := call(t, http.MethodPost, url+"/services/9000001/outages", "u", `{"outage_id":1}`); st != http.StatusNotFound {
		t.Fatalf("attaching an unknown record: %d, want 404", st)
	}
}

func TestFakeOutagesThroughClient(t *testing.T) {
	_, c, _ := fakeAPI(t)
	ctx := context.Background()
	if o, err := c.GetOutage(ctx, 1); o != nil || err != nil {
		t.Fatalf("unknown: %+v %v", o, err)
	}
	p, err := c.ListServiceOutages(ctx, 9000001, leaptel.OutageQuery{StartDate: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Page: 99})
	if err != nil || len(p.Outages) != 0 {
		t.Fatalf("past the last page: %+v %v", p, err)
	}
	p, err = c.ListOutages(ctx, leaptel.OutageQuery{StartDate: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), SortBy: "outage_id", SortOrder: "desc"})
	if err != nil || len(p.Outages) == 0 {
		t.Fatalf("national: %+v %v", p, err)
	}
	for _, id := range []int{34707, 165360} { // public_timezone null, and ""
		o, err := c.GetOutage(ctx, id)
		if err != nil || o == nil || o.OutageID != id || o.PublicTimezone != "" {
			t.Fatalf("%d: %+v %v", id, o, err)
		}
	}
}

func TestFakeDevOutagesPersist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	ts := httptest.NewServer(newFakeServer(file).routes())
	mustPost(t, ts.URL+"/outages", `{"outage_id":9300001,"type_id":2,"provider":"nbn_direct","provider_outage_id":"CRQ000000000001","start_time":"2026-12-01 13:00:00","finis_time":"2026-12-01 19:00:00","scope":{"service":[3001]}}`)
	mustPost(t, ts.URL+"/outages", `{"outage_id":164915,"type_id":2,"provider":"nbn_direct","start_time":"2026-12-02 13:00:00"}`) // replaces the fixture
	mustPost(t, ts.URL+"/services/3002/outages", `{"outage_id":9300001}`)
	if st, _, b := call(t, http.MethodPost, ts.URL+"/outages", "u", `{"type_id":2}`); st != http.StatusBadRequest {
		t.Fatalf("a record with no id: %d %s", st, b)
	}
	if st, _, b := call(t, http.MethodPost, ts.URL+"/outages", "u", `not json`); st != http.StatusBadRequest {
		t.Fatalf("not JSON: %d %s", st, b)
	}
	ts.Close()

	ts = httptest.NewServer(newFakeServer(file).routes())
	defer ts.Close()
	c := leaptel.NewClient(ts.URL, "u", "p")
	ctx := context.Background()
	q := leaptel.OutageQuery{StartDate: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	for _, svc := range []int{3001, 3002} {
		p, err := c.ListServiceOutages(ctx, svc, q)
		if err != nil || len(p.Outages) != 1 || p.Outages[0].OutageID != 9300001 {
			t.Fatalf("service %d after a restart: %+v %v", svc, p, err)
		}
	}
	if o, err := c.GetOutage(ctx, 164915); err != nil || o == nil || o.StartTime != "2026-12-02 13:00:00" {
		t.Fatalf("the replaced fixture after a restart: %+v %v", o, err)
	}
}
