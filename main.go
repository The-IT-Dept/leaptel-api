// cmd/leaptel-api is a fake Leaptel API for local development.
//
// It serves the same endpoints as Leaptel's production API
// (/api/v1/wholesaler/*) so the prism backend can talk to it unchanged —
// just point LEAPTEL_BASE_URL at this server.
//
// Endpoints split into two groups:
//
//	PROXIED (forwarded upstream with the caller's own Basic Auth):
//	  - GET  /products
//	  - POST /service-qualifications
//	  - GET  /service-assurance-tests
//
//	FAKED (in-memory, no real provisioning):
//	  - POST   /customers, GET /customers, GET /customers/{id}/services
//	  - POST   /orders, GET /orders/{id}, PATCH /orders/{id}/appointment
//	  - GET    /services/{id}, POST /services/{id}/modify (order_type plan or esla)
//	  - POST   /services/{id}/cancel
//	  - POST   /services/{id}/cease, /services/{id}/set-esla (dev-only)
//	  - GET    /appointments/time-slots, POST /appointments
//	  - POST   /services/{id}/assurance-tests
//	  - GET    /services/{id}/assurance-tests/{testId}
//	  - GET    /services/{id}/assurance-tests-history
//	  - GET    /outages, /outages/{id}, /services/{id}/outages (from the
//	           scrubbed fixtures/outages, with the upstream's 10-a-minute
//	           limit; see outages.go)
//	  - POST   /outages, /services/{id}/outages (dev-only: add a record,
//	           attach one to a service)
//
// State is in memory, and saved to -state-file when one is set (records and
// attachments added in dev included).
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
)

func main() {
	addr := flag.String("addr", ":9091", "listen address (e.g. :9091 or 127.0.0.1:9091)")
	stateFile := flag.String("state-file", "./leaptel-api.state.json", "path to persist fake state to (empty to disable)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	srv := newFakeServer(*stateFile)

	r := chi.NewRouter()
	r.Use(requestLogger)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","service":"fake-leaptel"}`))
	})
	r.Mount("/api/v1/wholesaler", srv.routes())
	runServer(r, *addr)
}

// routes is the fake's API, mounted at /api/v1/wholesaler.
func (srv *fakeServer) routes() chi.Router {
	r := chi.NewRouter()
	// Proxied endpoints — forwarded to the real Leaptel.
	r.Get("/products", proxyUpstream)
	r.Post("/service-qualifications", proxyUpstream)
	r.Get("/service-assurance-tests", proxyUpstream)

	// Faked stateful endpoints — require any non-empty Authorization
	// header but don't validate it (the prism backend is the only
	// caller in dev).
	r.Post("/customers", requireAuth(srv.createCustomer))
	r.Get("/customers", requireAuth(srv.listCustomers))
	r.Get("/customers/{id}/services", requireAuth(srv.listCustomerServices))

	r.Post("/orders", requireAuth(srv.createOrder))
	r.Get("/orders/{id}", requireAuth(srv.getOrder))
	r.Patch("/orders/{id}/appointment", requireAuth(srv.attachAppointment))

	r.Get("/services/{id}", withLimitHeader(serviceReadsLimit, requireAuth(srv.getService)))
	r.Post("/services/{id}/modify", requireAuth(srv.modifyService))
	r.Post("/services/{id}/cancel", requireAuth(srv.cancelService))
	r.Post("/services/{id}/cease", requireAuth(srv.ceaseService))      // dev-only: simulate carrier churn
	r.Post("/services/{id}/set-esla", requireAuth(srv.setServiceESLA)) // dev-only: simulate a portal eSLA change
	r.Post("/services/{id}/assurance-tests", requireAuth(srv.startAssuranceTest))
	r.Get("/services/{id}/assurance-tests/{testId}", requireAuth(srv.getAssuranceTest))
	r.Get("/services/{id}/assurance-tests-history", requireAuth(srv.listAssuranceHistory))

	r.Get("/appointments/time-slots", requireAuth(srv.listTimeslots))
	r.Post("/appointments", requireAuth(srv.createAppointment))

	// Outage reads share the upstream's limit (outages.go).
	lim := newOutageLimiter(time.Now)
	r.Get("/outages", requireAuth(lim.limit(srv.listOutages)))
	r.Get("/outages/{id}", requireAuth(lim.limit(srv.getOutage)))
	r.Get("/services/{id}/outages", requireAuth(lim.limit(srv.listServiceOutages)))
	r.Post("/outages", requireAuth(srv.putOutage))                  // dev-only: add or replace a record
	r.Post("/services/{id}/outages", requireAuth(srv.attachOutage)) // dev-only: simulate the upstream's match
	return r
}

func runServer(r http.Handler, addr string) {
	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("fake leaptel api listening",
			"addr", addr,
			"base_url", "http://"+displayHost(addr)+"/api/v1/wholesaler",
			"hint", "set LEAPTEL_BASE_URL to the base_url above")
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-done
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

// requestLogger emits one slog line per request with method, path, status,
// duration. Cheap structured logging — no need for the prism middleware.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// displayHost turns a listen address into something pasteable into a browser
// or LEAPTEL_BASE_URL. A bare ":9091" needs a host prepended; "127.0.0.1:9091"
// is already usable as-is.
func displayHost(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}
