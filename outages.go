package main

// The outage routes, served from the scrubbed records in fixtures/outages
// plus any added in dev:
//
//	GET  /outages                 the national list: 50 a page, sort_by
//	                              (outage_id, start_date, finish_date),
//	                              sort_order and start_date; other filters
//	                              are ignored, as upstream
//	GET  /outages/{id}            one record; {} for an unknown id
//	GET  /services/{id}/outages   the records whose scope names the service,
//	                              plus those attached to it
//	POST /outages                 dev-only: add or replace a record (JSON)
//	POST /services/{id}/outages   dev-only: attach {"outage_id":N} to a
//	                              service, as the upstream's own match
//
// The national copies (the list and /outages/{id}) carry no scope.service;
// a per-service copy always carries a scope ([] when empty). A page past the
// last, or an empty one, has no "outages" key. The three reads share the
// upstream's limit of 10 a minute per user: 429 {"message":"Too Many
// Attempts."} past it, with no Retry-After.

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/the-it-dept/leaptel-api/leaptel"
)

// Each file is one record, {} or a page (whose records are all loaded). They
// are mirrored to the public repo, so they're scrubbed: synthetic service ids
// (9000001 up), staff_a/staff_b for upstream staff, no customer fields.
//
//go:embed fixtures/outages/*.json
var outageFixtureFS embed.FS

const (
	outagesPerPage       = 50
	outageReadsPerMinute = 10
	serviceReadsLimit    = "15" // GET /services/{id}'s own bucket, stated only
)

// fakeOutage is a record as stored, with what the routes sort and filter on.
type fakeOutage struct {
	id           int
	start, finis string
	services     []string // scope.service
	raw          json.RawMessage
}

func parseFakeOutage(raw []byte) (fakeOutage, error) {
	var o leaptel.Outage
	if err := json.Unmarshal(raw, &o); err != nil {
		return fakeOutage{}, err
	}
	if o.OutageID <= 0 {
		return fakeOutage{}, errNoOutageID
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, o.Raw); err != nil {
		return fakeOutage{}, err
	}
	return fakeOutage{id: o.OutageID, start: o.StartTime, finis: o.FinisTime, services: o.Scope.Service, raw: compact.Bytes()}, nil
}

var errNoOutageID = errors.New("a record needs an outage_id")

// outageFixtures is every fixture record by id. A record's own file wins over
// its copy in a page.
var outageFixtures = loadOutageFixtures()

func loadOutageFixtures() map[int]fakeOutage {
	out := map[int]fakeOutage{}
	entries, err := outageFixtureFS.ReadDir("fixtures/outages")
	if err != nil {
		slog.Error("loading outage fixtures", "err", err)
		return out
	}
	var singles [][]byte
	for _, e := range entries {
		raw, err := outageFixtureFS.ReadFile("fixtures/outages/" + e.Name())
		if err != nil {
			slog.Error("reading outage fixture", "name", e.Name(), "err", err)
			continue
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			slog.Error("parsing outage fixture", "name", e.Name(), "err", err)
			continue
		}
		_, isPage := keys["pagination"]
		if _, ok := keys["outages"]; ok {
			isPage = true
		}
		switch {
		case len(keys) == 0: // {}: served by an unknown id
		case isPage:
			var p leaptel.OutagesPage
			if err := json.Unmarshal(raw, &p); err != nil {
				slog.Error("parsing outage fixture page", "name", e.Name(), "err", err)
				continue
			}
			for _, o := range p.Outages {
				if fo, err := parseFakeOutage(o.Raw); err == nil {
					out[fo.id] = fo
				}
			}
		default:
			singles = append(singles, raw)
		}
	}
	for _, raw := range singles {
		fo, err := parseFakeOutage(raw)
		if err != nil {
			slog.Error("parsing outage fixture record", "err", err)
			continue
		}
		out[fo.id] = fo
	}
	return out
}

// --- state ---

// outage is the record with id: one added in dev, else a fixture.
func (s *state) outage(id int) (fakeOutage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if raw, ok := s.outages[id]; ok {
		if fo, err := parseFakeOutage(raw); err == nil {
			return fo, true
		}
	}
	fo, ok := outageFixtures[id]
	return fo, ok
}

// allOutages is every record, and the outage ids attached to service svc
// (0: none).
func (s *state) allOutages(svc int) ([]fakeOutage, map[int]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byID := make(map[int]fakeOutage, len(outageFixtures)+len(s.outages))
	for id, fo := range outageFixtures {
		byID[id] = fo
	}
	for _, raw := range s.outages {
		if fo, err := parseFakeOutage(raw); err == nil {
			byID[fo.id] = fo
		}
	}
	all := make([]fakeOutage, 0, len(byID))
	for _, fo := range byID {
		all = append(all, fo)
	}
	attached := map[int]bool{}
	for _, id := range s.outageLinks[svc] {
		attached[id] = true
	}
	return all, attached
}

func (s *state) putOutage(fo fakeOutage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outages[fo.id] = fo.raw
	s.save()
}

func (s *state) attachOutage(svc, id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.outageLinks[svc], id) {
		s.outageLinks[svc] = append(s.outageLinks[svc], id)
		s.save()
	}
}

// --- rendering ---

// render is the record as a route serves it. The national copy (svc 0)
// drops scope.service, and the scope with it once empty. A per-service copy
// always has a scope, naming svc when it's attached.
func (fo fakeOutage) render(svc int, attached bool) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(fo.raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return fo.raw
	}
	scope, _ := m["scope"].(map[string]any)
	if scope == nil {
		scope = map[string]any{}
	}
	if svc == 0 {
		delete(scope, "service")
		if len(scope) == 0 {
			delete(m, "scope")
		} else {
			m["scope"] = scope
		}
	} else {
		if attached && !slices.Contains(fo.services, strconv.Itoa(svc)) {
			list := make([]any, 0, len(fo.services)+1)
			for _, v := range fo.services {
				list = append(list, v)
			}
			scope["service"] = append(list, strconv.Itoa(svc))
		}
		if len(scope) == 0 {
			m["scope"] = []any{}
		} else {
			m["scope"] = scope
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return fo.raw
	}
	return bytes.TrimSpace(buf.Bytes())
}

// --- handlers ---

// GET /outages
func (s *fakeServer) listOutages(w http.ResponseWriter, r *http.Request) {
	all, _ := s.state.allOutages(0)
	s.writeOutagePage(w, r, all, 0, nil)
}

// GET /services/{id}/outages
func (s *fakeServer) listServiceOutages(w http.ResponseWriter, r *http.Request) {
	svc, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil || svc <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service id"})
		return
	}
	all, attached := s.state.allOutages(svc)
	ref := strconv.Itoa(svc)
	var mine []fakeOutage
	for _, fo := range all {
		if attached[fo.id] || slices.Contains(fo.services, ref) {
			mine = append(mine, fo)
		}
	}
	s.writeOutagePage(w, r, mine, svc, attached)
}

// writeOutagePage filters by start_date, sorts and pages recs.
func (s *fakeServer) writeOutagePage(w http.ResponseWriter, r *http.Request, recs []fakeOutage, svc int, attached map[int]bool) {
	q := r.URL.Query()
	if d, err := time.Parse(time.DateOnly, q.Get("start_date")); err == nil {
		from := d.Format(time.DateOnly)
		recs = slices.DeleteFunc(recs, func(fo fakeOutage) bool { return fo.start < from })
	}
	key := func(fo fakeOutage) string { return "" }
	switch q.Get("sort_by") {
	case "start_date":
		key = func(fo fakeOutage) string { return fo.start }
	case "finish_date":
		key = func(fo fakeOutage) string { return fo.finis }
	}
	desc := strings.EqualFold(q.Get("sort_order"), "desc")
	sort.Slice(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if ka, kb := key(a), key(b); ka != kb {
			return (ka < kb) != desc
		}
		return (a.id < b.id) != desc
	})

	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	total := len(recs)
	pages := max((total+outagesPerPage-1)/outagesPerPage, 1)
	pg := leaptel.Pagination{TotalRecords: total, CurrentPage: page, RecordsPerPage: outagesPerPage, TotalPages: pages}
	if page < pages {
		next := page + 1
		pg.NextPage = &next
	}
	if page > 1 {
		prev := page - 1
		pg.PreviousPage = &prev
	}
	body := map[string]any{"pagination": pg}
	if lo := (page - 1) * outagesPerPage; lo < total {
		hi := min(lo+outagesPerPage, total)
		out := make([]json.RawMessage, 0, hi-lo)
		for _, fo := range recs[lo:hi] {
			out = append(out, fo.render(svc, attached[fo.id]))
		}
		body["outages"] = out
	}
	writeJSON(w, http.StatusOK, body)
}

// GET /outages/{id}
func (s *fakeServer) getOutage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	fo, ok := s.state.outage(id)
	if err != nil || !ok {
		writeJSON(w, http.StatusOK, struct{}{}) // as upstream: /outages/types is {} too
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(fo.render(0, false))
}

// POST /outages (dev-only): add or replace a record.
func (s *fakeServer) putOutage(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	fo, err := parseFakeOutage(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid outage record: " + err.Error()})
		return
	}
	s.state.putOutage(fo)
	slog.Info("fake stored outage", "outage_id", fo.id)
	w.Header().Set("Content-Type", "application/json")
	w.Write(fo.raw)
}

// POST /services/{id}/outages (dev-only): {"outage_id":N} joins the
// service's list, as if the upstream had matched it.
func (s *fakeServer) attachOutage(w http.ResponseWriter, r *http.Request) {
	svc, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil || svc <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service id"})
		return
	}
	var body struct {
		OutageID json.Number `json:"outage_id"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "want {\"outage_id\":N}"})
		return
	}
	id, err := strconv.Atoi(strings.Trim(body.OutageID.String(), `"`))
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "want {\"outage_id\":N}"})
		return
	}
	if _, ok := s.state.outage(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "outage not found"})
		return
	}
	s.state.attachOutage(svc, id)
	slog.Info("fake attached outage", "service_id", svc, "outage_id", id)
	writeJSON(w, http.StatusOK, map[string]int{"service_id": svc, "outage_id": id})
}

// --- the upstream's rate limit ---

// outageLimiter is the upstream's limit on the outage reads: a fixed window
// of a minute from a user's first call in it.
type outageLimiter struct {
	now func() time.Time
	mu  sync.Mutex
	win map[string]*limitWindow
}

type limitWindow struct {
	start time.Time
	n     int
}

func newOutageLimiter(now func() time.Time) *outageLimiter {
	return &outageLimiter{now: now, win: map[string]*limitWindow{}}
}

// take counts a call by user, reporting whether it may go and how many are
// left in the window.
func (l *outageLimiter) take(user string) (ok bool, remaining int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.win[user]
	if w == nil || !now.Before(w.start.Add(time.Minute)) {
		w = &limitWindow{start: now}
		l.win[user] = w
	}
	if w.n >= outageReadsPerMinute {
		return false, 0
	}
	w.n++
	return true, outageReadsPerMinute - w.n
}

// limit wraps an outage read in the limit, with the upstream's headers.
func (l *outageLimiter) limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		ok, remaining := l.take(user)
		w.Header().Set("x-ratelimit-limit", strconv.Itoa(outageReadsPerMinute))
		w.Header().Set("x-ratelimit-remaining", strconv.Itoa(remaining))
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"message":"Too Many Attempts."}`))
			return
		}
		next(w, r)
	}
}

// withLimitHeader states a route's limit without enforcing it.
func withLimitHeader(limit string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-limit", limit)
		next(w, r)
	}
}
