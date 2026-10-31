package leaptel

// nbn enhanced service levels (eSLA): a faster fault-restoration target on an
// nbn service, bought per service from Leaptel. Leaptel names them by id
// (standard, enhanced-12-24-7, …) on products, orders and modifies, but its
// live service and order reads carry a display name in nbn_esla ("Standard",
// "Enhanced - 12 (24/7)"). NormaliseESLA turns either form into the id.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The eSLA ids Underlay sells (and standard, which is none).
const (
	ESLAStandard      = "standard"
	ESLAEnhanced12x24 = "enhanced-12-24-7"
	ESLAEnhanced4x24  = "enhanced-4-24-7"
)

// ProductESLA is one eSLA level a product offers, at Leaptel's monthly cost.
type ProductESLA struct {
	ID          string `json:"id"`           // standard | enhanced-12 | enhanced-12-24-7 | …
	Name        string `json:"name"`         // "Enhanced - 12 (24/7)"
	MonthlyCost string `json:"monthly_cost"` // dollars ex GST, e.g. "11.00"
}

var (
	eslaID      = regexp.MustCompile(`^enhanced-\d+(-24-7)?$`)
	eslaDisplay = regexp.MustCompile(`^enhanced-(\d+)(\(24/7\))?$`)
)

// NormaliseESLA turns what Leaptel reports for a service's eSLA into its id:
// an id stays as it is, a display name ("Enhanced - 4 (24/7)") becomes its id
// ("enhanced-4-24-7"), and "" stays "" (unknown: it must never overwrite what
// we know). Anything else comes back lower-cased with its spaces removed, which
// never equals an id we order, so it reads as a mismatch.
func NormaliseESLA(s string) string {
	s = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "")
	switch {
	case s == "", s == ESLAStandard, eslaID.MatchString(s):
		return s
	}
	if m := eslaDisplay.FindStringSubmatch(s); m != nil {
		if m[2] != "" {
			return "enhanced-" + m[1] + "-24-7"
		}
		return "enhanced-" + m[1]
	}
	return s
}

// ESLACode is the service's eSLA as an id: the live nbn_esla (a display name),
// else the spec's esla (an id); "" when Leaptel said neither.
func (s *Service) ESLACode() string {
	if v := NormaliseESLA(s.NbnESLA); v != "" {
		return v
	}
	return NormaliseESLA(s.ESLA)
}

// melbourne is the zone Leaptel reads order dates in.
var melbourne = func() *time.Location {
	if loc, err := time.LoadLocation("Australia/Melbourne"); err == nil {
		return loc
	}
	return time.FixedZone("AEST", 10*3600)
}()

// melbourneDate is now's date in Melbourne, YYYY-MM-DD.
func melbourneDate(now time.Time) string { return now.In(melbourne).Format(time.DateOnly) }

// nowFn is the clock ModifyESLA dates its order by; tests pin it.
var nowFn = time.Now

// ModifyESLA raises a modify order that sets the service's eSLA to level
// (standard removes it), effective today in Melbourne. It carries no plan:
// the speed stays as it is.
func (c *Client) ModifyESLA(serviceID int, level string) (*CreateOrderResponse, json.RawMessage, error) {
	form := map[string]string{
		"order_type":  "esla",
		"esla":        level,
		"order_after": melbourneDate(nowFn()),
	}
	var out CreateOrderResponse
	raw, err := c.doMultipart("POST", fmt.Sprintf("/services/%d/modify", serviceID), form, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}
