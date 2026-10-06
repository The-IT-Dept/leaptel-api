package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// upstreamBaseURL is the real Leaptel API. Proxied endpoints
// hit this — the caller's Authorization header is forwarded as-is.
const upstreamBaseURL = "https://api.wholesaler.leaptel.com.au/api/v1/wholesaler"

// Leaptel allowlists our IPv4 address, so egress must not happen over IPv6 —
// this host prefers v6 and the upstream WAF answers those with a 403
// "request has been blocked" that looks exactly like a credential failure.
// Pinning the dialer to tcp4 is the whole fix.
var upstreamClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", addr)
		},
	},
}

// proxyUpstream forwards the request to the real Leaptel API at the same
// path (relative to /api/v1/wholesaler). Used for read-only catalog and
// service-qualification endpoints. The client's own Basic Auth header is
// reused — the fake server holds no credentials of its own.
func proxyUpstream(w http.ResponseWriter, r *http.Request) {
	path := relativePath(r.URL.Path)
	target := upstreamBaseURL + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		slog.Error("proxy: build request failed", "err", err, "target", target)
		http.Error(w, "proxy build failed", http.StatusBadGateway)
		return
	}

	// Forward auth + content negotiation headers. Don't forward Host.
	for _, h := range []string{"Authorization", "Content-Type", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	slog.Debug("proxy upstream", "method", r.Method, "target", target)

	resp, err := upstreamClient.Do(req)
	if err != nil {
		slog.Error("proxy: upstream request failed", "err", err, "target", target)
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Warn("proxy: copy response body failed", "err", err)
	}
}

// relativePath strips the /api/v1/wholesaler prefix that our routes are
// mounted under, leaving the bare endpoint path the upstream API expects.
func relativePath(p string) string {
	const prefix = "/api/v1/wholesaler"
	if len(p) >= len(prefix) && p[:len(prefix)] == prefix {
		return p[len(prefix):]
	}
	return p
}
