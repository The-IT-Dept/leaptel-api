package leaptel

// Outage records: GET /outages (the national list), GET /outages/{id} and
// GET /services/{id}/outages (the per-service list, the only exact match).
//
// The API is PHP, and its JSON shows it: an empty scope is "scope": [], a list
// with a gap in its keys arrives as an object keyed "0", "2", ..., ids come as
// numbers or strings, public_timezone may be null, outage_updates may be
// missing, and a page past the last has no "outages" key at all. Each of those
// decodes to the plain Go zero value here, so none of them fails a page. A
// shape outside them is an error that quotes it, since a successful read's
// body isn't kept in the call record. On a page, that error is the one
// record's (Outage.Err): the rest of the page still decodes.
//
// The fields are kept as the upstream sends them (times as text, in the zones
// the record implies): turning them into our model is the caller's job.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // start_date is a Melbourne date, on hosts without zoneinfo too
)

// Outage is one upstream outage record.
type Outage struct {
	OutageID                  int
	TypeID                    int // the service type (2 is nbn), not the outage type
	OutageType                string
	NotificationDescription   string
	StartTime                 string
	FinisTime                 string
	Provider                  string
	ProviderOutageID          string
	ProviderOutageInformation string
	ProviderLocation          string
	Visibility                string
	OutageStatus              string
	Timezone                  string
	PublicTimezone            string // "" when sent blank or null
	TS                        string
	ServiceType               string
	Updates                   []OutageUpdate // outage_updates
	Scope                     OutageScope
	Raw                       json.RawMessage // the record's exact bytes

	// Err is set on a page's record that couldn't be decoded: then only
	// OutageID (0 when it can't be read either) and Raw are. A caller skips
	// it; the page's other records are whole.
	Err error
}

// OutageUpdate is one entry in a record's outage_updates.
type OutageUpdate struct {
	UpdateDate  string
	UpdatedBy   string
	ShortUpdate string
	LongUpdate  string
	SendSMS     string
	SendEmail   string
	Resolved    string // "yes" | "no"
}

// OutageScope is what a record says it covers. Only the per-service copy of a
// national nbn record carries one.
type OutageScope struct {
	Service        []string // upstream service ids
	POI            []string
	StateTerritory []string
	Other          []string // names of the scope keys other networks use (estate, postcode, ...), sorted
}

// OutagesPage is one page of a list. A missing "outages" key is an empty page.
type OutagesPage struct {
	Pagination Pagination
	Outages    []Outage
}

// OutageQuery is a list's filters. StartDate is required: without it the
// upstream is slow and returns records from years back. It's sent as its date
// in Melbourne.
type OutageQuery struct {
	StartDate time.Time
	SortBy    string // outage_id | start_date | finish_date
	SortOrder string // asc | desc
	Page      int    // 0: not sent (the first page)
}

// ListOutages reads a page of the national list: GET /outages.
func (c *Client) ListOutages(ctx context.Context, q OutageQuery) (*OutagesPage, error) {
	return c.listOutages(ctx, "/outages", q)
}

// ListServiceOutages reads a page of the records the upstream matches to one
// of its services: GET /services/{id}/outages.
func (c *Client) ListServiceOutages(ctx context.Context, aggServiceID int, q OutageQuery) (*OutagesPage, error) {
	return c.listOutages(ctx, fmt.Sprintf("/services/%d/outages", aggServiceID), q)
}

// GetOutage reads one record: GET /outages/{id}. An unknown id is answered
// 200 {}, which is (nil, nil) here. The national copy carries no scope.
func (c *Client) GetOutage(ctx context.Context, id int) (*Outage, error) {
	path := fmt.Sprintf("/outages/%d", id)
	body, err := c.getOutageJSON(ctx, path)
	if err != nil {
		return nil, err
	}
	if phpEmpty(body) {
		return nil, nil
	}
	var o Outage
	if err := json.Unmarshal(body, &o); err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if o.OutageID == 0 {
		return nil, fmt.Errorf("GET %s: a reply with no outage_id: %s", path, clip(body))
	}
	return &o, nil
}

func (c *Client) listOutages(ctx context.Context, path string, q OutageQuery) (*OutagesPage, error) {
	if q.StartDate.IsZero() {
		return nil, fmt.Errorf("GET %s: an outage query needs a start date", path)
	}
	v := url.Values{"start_date": {melbourneDate(q.StartDate)}}
	if q.SortBy != "" {
		v.Set("sort_by", q.SortBy)
	}
	if q.SortOrder != "" {
		v.Set("sort_order", q.SortOrder)
	}
	if q.Page > 0 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	path += "?" + v.Encode()
	body, err := c.getOutageJSON(ctx, path)
	if err != nil {
		return nil, err
	}
	var p OutagesPage
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return &p, nil
}

// getOutageJSON GETs path and returns a 2xx body. The successful body isn't
// kept in the call record (see isOutageRead), so a decode error quotes it.
func (c *Client) getOutageJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := c.newRequestCtx(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, clip(body))
	}
	return body, nil
}

// clip is b for an error message: at most 300 bytes.
func clip(b []byte) string {
	s := CleanText(string(bytes.TrimSpace(b)))
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "") + "…"
	}
	return s
}

// --- decoding ---

func (o *Outage) UnmarshalJSON(b []byte) error {
	if phpNull(b) {
		return nil
	}
	var aux struct {
		OutageID                  flexID          `json:"outage_id"`
		TypeID                    flexID          `json:"type_id"`
		OutageType                flexString      `json:"outage_type"`
		NotificationDescription   flexString      `json:"notification_description"`
		StartTime                 flexString      `json:"start_time"`
		FinisTime                 flexString      `json:"finis_time"`
		Provider                  flexString      `json:"provider"`
		ProviderOutageID          flexString      `json:"provider_outage_id"`
		ProviderOutageInformation flexString      `json:"provider_outage_information"`
		ProviderLocation          flexString      `json:"provider_location"`
		Visibility                flexString      `json:"visibility"`
		OutageStatus              flexString      `json:"outage_status"`
		Timezone                  flexString      `json:"timezone"`
		PublicTimezone            flexString      `json:"public_timezone"`
		TS                        flexString      `json:"ts"`
		ServiceType               flexString      `json:"service_type"`
		Updates                   json.RawMessage `json:"outage_updates"`
		Scope                     OutageScope     `json:"scope"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return fmt.Errorf("outage record: %w: %s", err, clip(b))
	}
	updates, err := phpList(aux.Updates)
	if err != nil {
		return fmt.Errorf("outage %d: outage_updates: %w", aux.OutageID, err)
	}
	*o = Outage{
		OutageID:                  int(aux.OutageID),
		TypeID:                    int(aux.TypeID),
		OutageType:                string(aux.OutageType),
		NotificationDescription:   string(aux.NotificationDescription),
		StartTime:                 string(aux.StartTime),
		FinisTime:                 string(aux.FinisTime),
		Provider:                  string(aux.Provider),
		ProviderOutageID:          string(aux.ProviderOutageID),
		ProviderOutageInformation: string(aux.ProviderOutageInformation),
		ProviderLocation:          string(aux.ProviderLocation),
		Visibility:                string(aux.Visibility),
		OutageStatus:              string(aux.OutageStatus),
		Timezone:                  string(aux.Timezone),
		PublicTimezone:            string(aux.PublicTimezone),
		TS:                        string(aux.TS),
		ServiceType:               string(aux.ServiceType),
		Scope:                     aux.Scope,
		Raw:                       append(json.RawMessage(nil), b...),
	}
	for _, raw := range updates {
		var u OutageUpdate
		if err := json.Unmarshal(raw, &u); err != nil {
			return fmt.Errorf("outage %d: %w", aux.OutageID, err)
		}
		o.Updates = append(o.Updates, u)
	}
	return nil
}

func (u *OutageUpdate) UnmarshalJSON(b []byte) error {
	if phpNull(b) {
		return nil
	}
	var aux struct {
		UpdateDate  flexString `json:"update_date"`
		UpdatedBy   flexString `json:"updated_by"`
		ShortUpdate flexString `json:"short_update"`
		LongUpdate  flexString `json:"long_update"`
		SendSMS     flexString `json:"send_sms"`
		SendEmail   flexString `json:"send_email"`
		Resolved    flexString `json:"resolved"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return fmt.Errorf("outage update: %w: %s", err, clip(b))
	}
	*u = OutageUpdate{
		UpdateDate:  string(aux.UpdateDate),
		UpdatedBy:   string(aux.UpdatedBy),
		ShortUpdate: string(aux.ShortUpdate),
		LongUpdate:  string(aux.LongUpdate),
		SendSMS:     string(aux.SendSMS),
		SendEmail:   string(aux.SendEmail),
		Resolved:    string(aux.Resolved),
	}
	return nil
}

func (s *OutageScope) UnmarshalJSON(b []byte) error {
	*s = OutageScope{}
	if phpEmpty(b) {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("outage scope: %w: %s", err, clip(b))
	}
	for key, raw := range m {
		var dst *[]string
		switch key {
		case "service":
			dst = &s.Service
		case "poi":
			dst = &s.POI
		case "state_territory":
			dst = &s.StateTerritory
		default:
			s.Other = append(s.Other, key)
			continue
		}
		items, err := phpList(raw)
		if err != nil {
			return fmt.Errorf("outage scope %s: %w", key, err)
		}
		for _, item := range items {
			var v flexString
			if err := json.Unmarshal(item, &v); err != nil {
				return fmt.Errorf("outage scope %s: %w", key, err)
			}
			if t := strings.TrimSpace(string(v)); t != "" {
				*dst = append(*dst, t)
			}
		}
	}
	sort.Strings(s.Other)
	return nil
}

func (p *OutagesPage) UnmarshalJSON(b []byte) error {
	*p = OutagesPage{}
	if phpEmpty(b) {
		return nil
	}
	var aux struct {
		Pagination Pagination      `json:"pagination"`
		Outages    json.RawMessage `json:"outages"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return fmt.Errorf("outages page: %w: %s", err, clip(b))
	}
	items, err := phpList(aux.Outages)
	if err != nil {
		return fmt.Errorf("outages page: %w", err)
	}
	p.Pagination = aux.Pagination
	for _, raw := range items {
		var o Outage
		if err := json.Unmarshal(raw, &o); err != nil {
			o = Outage{OutageID: lenientOutageID(raw), Raw: append(json.RawMessage(nil), raw...), Err: err}
		}
		p.Outages = append(p.Outages, o)
	}
	return nil
}

// lenientOutageID is a record's outage_id, read on its own from bytes that
// don't decode as a whole: 0 when it can't be.
func lenientOutageID(raw []byte) int {
	var aux struct {
		OutageID flexID `json:"outage_id"`
	}
	if err := json.Unmarshal(raw, &aux); err != nil {
		return 0
	}
	return int(aux.OutageID)
}

// flexString is a PHP scalar as text: a string as is, a number or boolean as
// written, null (or an empty [] or {}) as "".
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case phpEmpty(b):
		*f = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
	case b[0] == '[' || b[0] == '{':
		return fmt.Errorf("want text, got %s", clip(b))
	default:
		*f = flexString(b)
	}
	return nil
}

// phpNull reports whether b is absent or null.
func phpNull(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) == 0 || string(b) == "null"
}

// phpEmpty reports whether b is absent, null, [] or {}: PHP's empty array
// is either of the last two. It reads only the brackets, so a page isn't
// decoded twice.
func phpEmpty(b []byte) bool {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return true
	}
	if len(b) < 2 || len(bytes.TrimSpace(b[1:len(b)-1])) != 0 {
		return false
	}
	return b[0] == '[' && b[len(b)-1] == ']' || b[0] == '{' && b[len(b)-1] == '}'
}

// phpList is a PHP list's items in order. It arrives as a JSON array, or as an
// object keyed by index when the keys have a gap; absent, null, [] and {} are
// no items.
func phpList(b []byte) ([]json.RawMessage, error) {
	b = bytes.TrimSpace(b)
	if phpNull(b) {
		return nil, nil
	}
	switch b[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(b, &items); err != nil {
			return nil, err
		}
		return items, nil
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, errA := strconv.Atoi(keys[i])
			c, errC := strconv.Atoi(keys[j])
			switch {
			case errA == nil && errC == nil:
				return a < c
			case errA == nil:
				return true
			case errC == nil:
				return false
			}
			return keys[i] < keys[j]
		})
		items := make([]json.RawMessage, len(keys))
		for i, k := range keys {
			items[i] = m[k]
		}
		return items, nil
	}
	return nil, errors.New("want a list, got " + clip(b))
}
