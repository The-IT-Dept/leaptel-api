package main

import (
	"embed"
	"net/http"
)

// catalogFS holds the upstream's assurance test catalogue for each access
// technology the fake answers tests for (the test types it offers, no
// service or customer in them): fixtures/catalog/<carrier>-<tech>.json.
//
//go:embed fixtures/catalog/*.json
var catalogFS embed.FS

// assuranceCatalog answers GET /service-assurance-tests from the embedded
// catalogue for the carrier and access technology asked for, so the
// automatic tests run end to end locally with dev credentials. With proxy
// set (-proxy-catalog), or for a technology it has no catalogue for, it asks
// the real upstream with the caller's own Basic Auth, as before.
func assuranceCatalog(proxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		body, err := catalogFS.ReadFile("fixtures/catalog/" + q.Get("carrier") + "-" + q.Get("access_technology") + ".json")
		if proxy || err != nil {
			proxyUpstream(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}
