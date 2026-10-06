package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/the-it-dept/leaptel-api/leaptel"
)

type fakeServer struct {
	state *state
}

func newFakeServer(stateFile string) *fakeServer {
	return &fakeServer{state: loadState(stateFile)}
}

// requireAuth rejects requests without a Basic Auth header. We don't
// validate the credentials — any non-empty value is accepted, mirroring the
// fact that the fake server has no idea what real Leaptel creds look like.
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing Authorization header"})
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// --- customers -----------------------------------------------------------

// POST /customers (multipart)
func (s *fakeServer) createCustomer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart body: " + err.Error()})
		return
	}
	c := leaptel.Customer{
		FirstName:   r.FormValue("first_name"),
		LastName:    r.FormValue("last_name"),
		CompanyName: r.FormValue("company_name"),
		Email:       r.FormValue("email"),
		Mobile:      r.FormValue("mobile"),
		Phone:       r.FormValue("phone"),
		Address1:    r.FormValue("address"),
		City:        r.FormValue("city"),
		State:       r.FormValue("state"),
		Postcode:    r.FormValue("postcode"),
	}
	created := s.state.createCustomer(c)
	slog.Info("fake created customer", "customer_id", created.CustomerID, "email", created.Email)
	writeJSON(w, http.StatusOK, created)
}

// GET /customers?page=N
func (s *fakeServer) listCustomers(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	customers, pg := s.state.listCustomers(page, 50)
	writeJSON(w, http.StatusOK, leaptel.CustomersPage{Pagination: pg, Customers: customers})
}

// GET /customers/{id}/services
func (s *fakeServer) listCustomerServices(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid customer id"})
		return
	}
	writeJSON(w, http.StatusOK, s.state.servicesForCustomer(id))
}

// --- orders --------------------------------------------------------------

// POST /orders (multipart)
func (s *fakeServer) createOrder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart body: " + err.Error()})
		return
	}
	customerID, _ := strconv.Atoi(r.FormValue("customer_id"))
	planID, _ := strconv.Atoi(r.FormValue("plan_id"))
	p := leaptel.CreateOrderParams{
		CustomerID:       customerID,
		Carrier:          r.FormValue("carrier"),
		LocationID:       r.FormValue("location_id"),
		ProductID:        r.FormValue("product_id"),
		PlanID:           planID,
		OrderType:        r.FormValue("order_type"),
		ConnectionType:   r.FormValue("connection_type"),
		ContactFirstName: r.FormValue("contact_first_name"),
		ContactLastName:  r.FormValue("contact_last_name"),
		ContactAddress:   r.FormValue("contact_address"),
		ContactSuburb:    r.FormValue("contact_suburb"),
		ContactState:     r.FormValue("contact_state"),
		ContactPostcode:  r.FormValue("contact_postcode"),
		ContactEmail:     r.FormValue("contact_email"),
		ContactPhone:     r.FormValue("contact_phone"),
		NTDID:            r.FormValue("ntd_id"),
		NTDPort:          r.FormValue("ntd_port"),
		NTDType:          r.FormValue("ntd_type"),
		COAT:             r.FormValue("coat"),
		AVCID:            r.FormValue("avc_id"),
	}
	o := s.state.createOrder(p)
	slog.Info("fake created order", "order_id", o.OrderID, "service_id", o.ServiceID, "customer_id", o.CustomerID, "location_id", o.LocationID)
	writeJSON(w, http.StatusOK, leaptel.CreateOrderResponse{
		OrderID:    o.OrderID,
		ServiceID:  o.ServiceID,
		LocationID: o.LocationID,
	})
}

// GET /orders/{id}
func (s *fakeServer) getOrder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid order id"})
		return
	}
	out, ok := s.state.pollOrder(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "order not found"})
		return
	}
	slog.Debug("fake polled order", "order_id", id, "status", out.Status)
	writeJSON(w, http.StatusOK, out)
}

// PATCH /orders/{id}/appointment (JSON {"appointment_id":"..."})
func (s *fakeServer) attachAppointment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid order id"})
		return
	}
	var body struct {
		AppointmentID string `json:"appointment_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.AppointmentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "appointment_id is required"})
		return
	}
	if _, ok := s.state.attachAppointment(id, body.AppointmentID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "order not found"})
		return
	}
	slog.Info("fake attached appointment", "order_id", id, "appointment_id", body.AppointmentID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "attached", "appointment_id": body.AppointmentID})
}

// --- services ------------------------------------------------------------

// GET /services/{id}
func (s *fakeServer) getService(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service id"})
		return
	}
	svc, ok := s.state.getService(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "service not found"})
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

// POST /services/{id}/modify (multipart, plan_id=... or product_id=...)
func (s *fakeServer) modifyService(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service id"})
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart body"})
		return
	}
	// Leaptel takes the target as plan_id (what the backend sends since
	// plans are keyed by Leaptel plan id) or, historically, product_id.
	productID := r.FormValue("product_id")
	if productID == "" && r.FormValue("plan_id") != "" {
		productID = "plan:" + r.FormValue("plan_id")
	}
	if productID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "plan_id or product_id is required"})
		return
	}
	// Mirror Leaptel's required fields so a missing one is caught here,
	// not silently in prod.
	if r.FormValue("order_type") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_type is required"})
		return
	}
	if r.FormValue("order_after") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_after is required"})
		return
	}
	o, ok := s.state.createModifyOrder(id, productID, r.FormValue("replacement_ntd"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "service not found"})
		return
	}
	slog.Info("fake created modify order", "order_id", o.OrderID, "service_id", id, "product_id", productID)
	writeJSON(w, http.StatusOK, leaptel.CreateOrderResponse{
		OrderID:    o.OrderID,
		ServiceID:  o.ServiceID,
		LocationID: o.LocationID,
	})
}

// POST /services/{id}/cancel (multipart, no fields)
func (s *fakeServer) cancelService(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service id"})
		return
	}
	if !s.state.cancelService(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "service not found"})
		return
	}
	slog.Info("fake cancelled service", "service_id", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// POST /services/{id}/cease — dev-only churn trigger (not a real leaptel
// endpoint): marks the service ceased at the carrier, so the reconcile job
// closes it out in the portal.
func (s *fakeServer) ceaseService(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service id"})
		return
	}
	if !s.state.ceaseService(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "service not found"})
		return
	}
	slog.Info("fake ceased service (simulated churn)", "service_id", id)
	writeJSON(w, http.StatusOK, map[string]string{"state": "ceased"})
}

// --- appointments --------------------------------------------------------

// GET /appointments/time-slots?start_date&end_date&slot_type
//
// Synthesises weekday slots in the requested window matching Leaptel's
// `timeslots` schema (see docs/simplicity-wholesaler-api.json): an
// envelope with metadata plus an availableTimeSlot[] that the portal
// reads via app/routes/appointment.tsx → flattenSlots().
func (s *fakeServer) listTimeslots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, err := time.Parse("2006-01-02", q.Get("start_date"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid start_date"})
		return
	}
	end, err := time.Parse("2006-01-02", q.Get("end_date"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid end_date"})
		return
	}
	slotType := q.Get("slot_type")
	if slotType == "" {
		slotType = "AM"
	}
	locationID := q.Get("location_id")

	// Australia/Sydney is where every real dev service in this project
	// lives; hardcode the offset so the client-side Date parses cleanly.
	// (In prod-facing code Leaptel aligns to the NBN location's TZ.)
	syd, _ := time.LoadLocation("Australia/Sydney")
	if syd == nil {
		syd = time.FixedZone("AEST", 10*3600)
	}

	startHour, endHour := 8, 12
	if slotType == "PM" {
		startHour, endHour = 13, 17
	}

	type slot struct {
		AppointmentSlotType string `json:"appointmentSlotType"`
		Start               string `json:"start"`
		End                 string `json:"end"`
		StartDateTime       string `json:"startDateTime"`
		EndDateTime         string `json:"endDateTime"`
		WithinTimeWindow    string `json:"withinTimeWindow"`
		AppointmentLeadTime string `json:"appointmentLeadTime"`
	}
	slots := []slot{}
	day := start
	for !day.After(end) && len(slots) < 12 {
		// Skip weekends — Leaptel slot data typically doesn't include them.
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			st := time.Date(day.Year(), day.Month(), day.Day(), startHour, 0, 0, 0, syd)
			en := time.Date(day.Year(), day.Month(), day.Day(), endHour, 0, 0, 0, syd)
			slots = append(slots, slot{
				AppointmentSlotType: slotType,
				Start:               st.Format("2006-01-02 15:04:05"),
				End:                 en.Format("2006-01-02 15:04:05"),
				StartDateTime:       st.Format("2006-01-02T15:04:05-07:00"),
				EndDateTime:         en.Format("2006-01-02T15:04:05-07:00"),
				WithinTimeWindow:    "true",
				AppointmentLeadTime: "Standard",
			})
		}
		day = day.AddDate(0, 0, 1)
	}

	envelope := map[string]any{
		"demandType":              "Standard Install",
		"priorityAssist":          "No",
		"locationId":              locationID,
		"appointmentSLA":          "Standard",
		"primaryAccessTechnology": "Fibre",
		"serviceabilityClass":     "1",
		"region":                  "Urban",
		"startDateTime":           start.Format("2006-01-02") + "T00:00:00+10:00",
		"endDateTime":             end.Format("2006-01-02") + "T00:00:00+10:00",
		"appointmentSlotType":     slotType,
		"endUserType":             "Residential",
		"appointmentType":         "Appointment",
		"availableTimeSlot":       slots,
	}
	writeJSON(w, http.StatusOK, envelope)
}

// POST /appointments (JSON CreateAppointmentParams)
//
// Returns {"id": "APT...."}. Note the response key is "id" (not
// "appointment_id") — that matches what
// internal/handler/public_appointments.go expects.
func (s *fakeServer) createAppointment(w http.ResponseWriter, r *http.Request) {
	var p leaptel.CreateAppointmentParams
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	a := s.state.createAppointment(p.LocationID, p.AppointmentSlot.StartDateTime, p.AppointmentSlot.EndDateTime, p.AppointmentSlot.SlotType)
	slog.Info("fake reserved appointment", "appointment_id", a.AppointmentID, "location_id", p.LocationID, "slot", p.AppointmentSlot.StartDateTime)
	writeJSON(w, http.StatusOK, map[string]string{
		"id":              a.AppointmentID,
		"status":          "reserved",
		"start_date_time": a.StartDateTime,
		"end_date_time":   a.EndDateTime,
		"slot_type":       a.SlotType,
	})
}

// --- service assurance ---------------------------------------------------

// POST /services/{id}/assurance-tests (multipart, test_number=N)
func (s *fakeServer) startAssuranceTest(w http.ResponseWriter, r *http.Request) {
	serviceID, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service id"})
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart body"})
		return
	}
	testNumber, _ := strconv.Atoi(r.FormValue("test_number"))
	if testNumber == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "test_number is required"})
		return
	}
	// behaviour is recorded for fidelity with Leaptel's catalog, but every
	// run completes via polling after a few seconds (see createAssuranceRun)
	// so the watcher and live-update paths get exercised in dev.
	behaviour := "async"
	if _, hasFixture := assuranceFixtures[testNumber]; hasFixture {
		behaviour = "sync"
	}
	run := s.state.createAssuranceRun(serviceID, testNumber, behaviour)
	slog.Info("fake started assurance test", "service_id", serviceID, "test_number", testNumber, "behaviour", behaviour, "test_id", run.TestID)
	writeJSON(w, http.StatusOK, run.Response)
}

// GET /services/{id}/assurance-tests/{testId}
func (s *fakeServer) getAssuranceTest(w http.ResponseWriter, r *http.Request) {
	testID, err := strconv.Atoi(chi.URLParam(r, "testId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid test id"})
		return
	}
	run, ok := s.state.getAssuranceRun(testID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "test not found"})
		return
	}
	writeJSON(w, http.StatusOK, run.Response)
}

// GET /services/{id}/assurance-tests-history?page=N
func (s *fakeServer) listAssuranceHistory(w http.ResponseWriter, r *http.Request) {
	serviceID, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service id"})
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	writeJSON(w, http.StatusOK, s.state.listAssuranceHistory(serviceID, page, 20))
}
