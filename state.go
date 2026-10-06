package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/the-it-dept/leaptel-api/leaptel"
)

// state is the in-memory store for the fake Leaptel server. One mutex
// guards everything — simplest thing that works for a dev-only service.
//
// When stateFile is non-empty the entire store is serialised to that
// path after every mutation so fake state survives restarts (prism's DB
// remembers provider_refs across fake reboots; without persistence the
// fake 404s on orders prism still thinks exist).
type state struct {
	mu        sync.Mutex
	stateFile string

	nextCustomerID    int
	nextOrderID       int
	nextServiceID     int
	nextTestID        int
	nextAppointmentID int

	customers     map[int]leaptel.Customer
	orders        map[int]*fakeOrder
	services      map[int]*leaptel.Service
	appointments  map[string]*fakeAppt
	assuranceRuns map[int]*fakeAssuranceRun
}

func newState() *state {
	return &state{
		nextCustomerID:    1000,
		nextOrderID:       2000,
		nextServiceID:     3000,
		nextTestID:        4000,
		nextAppointmentID: 5000,
		customers:         map[int]leaptel.Customer{},
		orders:            map[int]*fakeOrder{},
		services:          map[int]*leaptel.Service{},
		appointments:      map[string]*fakeAppt{},
		assuranceRuns:     map[int]*fakeAssuranceRun{},
	}
}

// loadState returns a state primed from the given file path. If the file
// doesn't exist the state starts fresh and saves will recreate it. Pass
// an empty string to disable persistence entirely.
func loadState(path string) *state {
	s := newState()
	s.stateFile = path
	if path == "" {
		return s
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("reading state file failed, starting fresh", "path", path, "err", err)
		}
		return s
	}
	var snap stateSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		slog.Warn("parsing state file failed, starting fresh", "path", path, "err", err)
		return s
	}
	snap.applyTo(s)
	slog.Info("loaded fake state",
		"path", path,
		"customers", len(s.customers),
		"orders", len(s.orders),
		"services", len(s.services),
		"appointments", len(s.appointments),
		"assurance_runs", len(s.assuranceRuns))
	return s
}

// stateSnapshot is the on-disk representation of the in-memory store.
type stateSnapshot struct {
	NextCustomerID    int                       `json:"next_customer_id"`
	NextOrderID       int                       `json:"next_order_id"`
	NextServiceID     int                       `json:"next_service_id"`
	NextTestID        int                       `json:"next_test_id"`
	NextAppointmentID int                       `json:"next_appointment_id"`
	Customers         map[int]leaptel.Customer  `json:"customers"`
	Orders            map[int]*fakeOrder        `json:"orders"`
	Services          map[int]*leaptel.Service  `json:"services"`
	Appointments      map[string]*fakeAppt      `json:"appointments"`
	AssuranceRuns     map[int]*fakeAssuranceRun `json:"assurance_runs"`
}

// applyTo copies snapshot fields into s. Missing maps become empty.
func (snap *stateSnapshot) applyTo(s *state) {
	s.nextCustomerID = snap.NextCustomerID
	s.nextOrderID = snap.NextOrderID
	s.nextServiceID = snap.NextServiceID
	s.nextTestID = snap.NextTestID
	s.nextAppointmentID = snap.NextAppointmentID
	if snap.Customers != nil {
		s.customers = snap.Customers
	}
	if snap.Orders != nil {
		s.orders = snap.Orders
	}
	if snap.Services != nil {
		s.services = snap.Services
	}
	if snap.Appointments != nil {
		s.appointments = snap.Appointments
	}
	if snap.AssuranceRuns != nil {
		s.assuranceRuns = snap.AssuranceRuns
	}
}

// save writes the current state to disk via atomic rename. Caller MUST
// hold s.mu. Errors are logged but never bubbled — a save failure
// shouldn't break the API call that triggered it.
func (s *state) save() {
	if s.stateFile == "" {
		return
	}
	snap := stateSnapshot{
		NextCustomerID:    s.nextCustomerID,
		NextOrderID:       s.nextOrderID,
		NextServiceID:     s.nextServiceID,
		NextTestID:        s.nextTestID,
		NextAppointmentID: s.nextAppointmentID,
		Customers:         s.customers,
		Orders:            s.orders,
		Services:          s.services,
		Appointments:      s.appointments,
		AssuranceRuns:     s.assuranceRuns,
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		slog.Error("marshalling state", "err", err)
		return
	}
	tmp := s.stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		slog.Error("writing state tmp file", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, s.stateFile); err != nil {
		slog.Error("renaming state file", "err", err)
	}
}

// fakeOrder is the server-side representation of an in-flight Leaptel
// order. The lifecycle is driven by GET /orders/{id} polls and the
// PATCH /orders/{id}/appointment call.
type fakeOrder struct {
	OrderID    int
	ServiceID  int
	CustomerID int
	LocationID string
	ProductID  string
	// NTDType is set on Accelerate Great upgrade orders (1_PORT / 4_PORT /
	// NTD_2.5). A non-empty value forces the install-appointment flow — a
	// next-gen NTD swap or fibre connect always needs a tech onsite.
	NTDType string
	// NTDPort is the UNI-D port number the order asked for (FTTP); the
	// completed service reports it back like the real API does.
	NTDPort string
	// COAT="yes" marks a Change Of Access Technology (FTTC/N → FTTP fibre
	// connect) — a fibre build that also needs a tech onsite.
	COAT          string
	StartDate     string
	FinishDate    string
	AppointmentID string
	// Action distinguishes connects from service changes: "provision"
	// (default), "modify" (speed change), "cancel".
	Action    string
	CreatedAt time.Time
	// AppointmentAttachedAt drives the staged post-booking progression
	// (appointment_scheduled → technician_onsite → activating → completed).
	AppointmentAttachedAt time.Time

	// Polls received so far. Used to drive the pre-appointment state machine.
	Polls int
}

type fakeAppt struct {
	AppointmentID string
	LocationID    string
	StartDateTime string
	EndDateTime   string
	SlotType      string
	OrderID       int // 0 if not yet attached
}

type fakeAssuranceRun struct {
	TestID     int
	ServiceID  int
	TestNumber int
	Behaviour  string // "sync" | "async"
	Polls      int
	StartedAt  time.Time
	// Response is the full response body returned to clients. We keep it
	// as map[string]any so we can faithfully echo the per-test fields
	// captured in fixtures/ (incl. ones absent from leaptel.AssuranceResult).
	Response map[string]any
}

// --- customer helpers ----------------------------------------------------

func (s *state) createCustomer(c leaptel.Customer) leaptel.Customer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextCustomerID++
	c.CustomerID = s.nextCustomerID
	if c.Active == "" {
		c.Active = "yes"
	}
	if c.BillingDay == "" {
		c.BillingDay = "1"
	}
	s.customers[c.CustomerID] = c
	s.save()
	return c
}

func (s *state) listCustomers(page, perPage int) ([]leaptel.Customer, leaptel.Pagination) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]leaptel.Customer, 0, len(s.customers))
	for _, c := range s.customers {
		all = append(all, c)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CustomerID > all[j].CustomerID })

	total := len(all)
	if perPage <= 0 {
		perPage = 50
	}
	if page <= 0 {
		page = 1
	}
	totalPages := (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}

	start := (page - 1) * perPage
	end := start + perPage
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}

	pg := leaptel.Pagination{
		TotalRecords:   total,
		CurrentPage:    page,
		RecordsPerPage: perPage,
		TotalPages:     totalPages,
	}
	if page > 1 {
		prev := page - 1
		pg.PreviousPage = &prev
	}
	if page < totalPages {
		next := page + 1
		pg.NextPage = &next
	}
	return all[start:end], pg
}

func (s *state) servicesForCustomer(customerID int) []leaptel.CustomerServiceRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []leaptel.CustomerServiceRef{}
	for _, svc := range s.services {
		if svc.CustomerID != customerID {
			continue
		}
		out = append(out, leaptel.CustomerServiceRef{
			ServiceID:    svc.ServiceID,
			CustomerID:   svc.CustomerID,
			State:        svc.State,
			ServiceGroup: svc.ServiceGroup,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServiceID < out[j].ServiceID })
	return out
}

// --- order helpers -------------------------------------------------------

func (s *state) createOrder(p leaptel.CreateOrderParams) *fakeOrder {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextOrderID++
	s.nextServiceID++

	o := &fakeOrder{
		OrderID:    s.nextOrderID,
		ServiceID:  s.nextServiceID,
		CustomerID: p.CustomerID,
		LocationID: p.LocationID,
		ProductID:  p.ProductID,
		NTDType:    p.NTDType,
		NTDPort:    p.NTDPort,
		COAT:       p.COAT,
		StartDate:  time.Now().UTC().Format("2006-01-02 15:04:05"),
		CreatedAt:  time.Now().UTC(),
	}
	s.orders[o.OrderID] = o

	// Provision a backing service row in "provisioning" state. Filled out
	// fully when the order completes. The realm is cosmetic fake data —
	// the real API derives it from the RSP account, not the order.
	username := strings.ToLower(strings.ReplaceAll(p.ContactFirstName+p.ContactLastName, " ", "")) +
		"@theitdept.au"
	s.services[o.ServiceID] = &leaptel.Service{
		ServiceID:        o.ServiceID,
		CustomerID:       o.CustomerID,
		State:            "provisioning",
		Identifier:       p.LocationID,
		ServiceAddress:   strings.TrimSpace(p.ContactAddress + ", " + p.ContactSuburb + " " + p.ContactState + " " + p.ContactPostcode),
		AccessTechnology: "FTTP",
		ProductID:        p.ProductID,
		ServiceGroup:     "data",
		PPPoEUsername:    username,
		StartDate:        o.StartDate,
	}
	s.save()
	return o
}

// createModifyOrder raises a speed-change order against an existing service.
// A non-empty replacementNTD makes it an NTD swap, which needs a tech onsite
// (appointment flow) — a plain speed change doesn't. Returns false when the
// service doesn't exist.
func (s *state) createModifyOrder(serviceID int, productID, replacementNTD string) (*fakeOrder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[serviceID]
	if !ok {
		return nil, false
	}
	s.nextOrderID++
	o := &fakeOrder{
		OrderID:    s.nextOrderID,
		ServiceID:  serviceID,
		CustomerID: svc.CustomerID,
		LocationID: svc.Identifier,
		ProductID:  productID,
		NTDType:    replacementNTD,
		Action:     "modify",
		StartDate:  time.Now().UTC().Format("2006-01-02 15:04:05"),
		CreatedAt:  time.Now().UTC(),
	}
	s.orders[o.OrderID] = o
	s.save()
	return o, true
}

// cancelService marks the backing service cancelled. The real API raises
// a disconnect order; the fake just flips state — completion semantics
// the portal doesn't poll yet.
func (s *state) cancelService(serviceID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[serviceID]
	if !ok {
		return false
	}
	svc.State = "cancelled"
	s.save()
	return true
}

// ceaseService simulates carrier-side churn: the line is closed out at the
// carrier without us asking (customer moved away, premises disconnected).
// Dev-only — lets us exercise the reconcile job, which should then close the
// service out in the portal too.
func (s *state) ceaseService(serviceID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[serviceID]
	if !ok {
		return false
	}
	svc.State = "ceased"
	svc.FinishDate = time.Now().UTC().Format("2006-01-02 15:04:05")
	s.save()
	return true
}

func (s *state) getOrder(orderID int) (*fakeOrder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[orderID]
	return o, ok
}

// pollOrder advances the state machine and returns the Order JSON the
// client should see.
//
// The flow branches on the LOC ID's last digit:
//   - ODD  → appointment-required: first poll emits APPOINTMENT_REQUIRED,
//     prism SMSes a magic-link; after the customer attaches an
//     appointment we walk a 5-min staged install progression.
//   - EVEN → auto-provision: skip the appointment step entirely and walk
//     an accepted→provisioning→activating→completed progression
//     driven by time since order creation.
//
// Unknown (lazy-created) orders default to appointment-required.
//
// Stages are ~5 minutes wall-clock end-to-end so a dev can click "retry"
// on poll_order_status and watch real state transitions.
func (s *state) pollOrder(orderID int) (*leaptel.Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, ok := s.orders[orderID]
	if !ok {
		// Lazy-create: the prism DB may carry provider_refs from an
		// earlier fake run whose state we lost. Better to fabricate a
		// stub than to dead-end the workflow with 404s.
		slog.Warn("lazy-creating unknown order on poll", "order_id", orderID)
		now := time.Now().UTC().Format("2006-01-02 15:04:05")
		o = &fakeOrder{
			OrderID:   orderID,
			StartDate: now,
			CreatedAt: time.Now().UTC(),
		}
		s.orders[orderID] = o
		if orderID >= s.nextOrderID {
			s.nextOrderID = orderID
		}
	}
	o.Polls++

	action := o.Action
	if action == "" {
		action = "provision"
	}
	out := &leaptel.Order{
		OrderID:    o.OrderID,
		ServiceID:  o.ServiceID,
		CustomerID: o.CustomerID,
		ProductID:  o.ProductID,
		StartDate:  o.StartDate,
		Action:     action,
	}

	// Plain speed changes never need appointments — quick staged
	// progression, then the backing service flips to the new product. An NTD
	// swap (replacement_ntd set → o.NTDType) falls through to the
	// appointment-capable path below, like a connect.
	if action == "modify" && o.NTDType == "" {
		elapsed := time.Since(o.CreatedAt)
		switch {
		case elapsed < 30*time.Second:
			out.Status = "in_progress"
			out.State = "accepted"
			out.NBNCallbacks = json.RawMessage(`[{"event_note":"MODIFY_ACCEPTED","status":"accepted","reason":"NBN has accepted the speed change"}]`)
		case elapsed < 90*time.Second:
			out.Status = "in_progress"
			out.State = "provisioning"
			out.NBNCallbacks = json.RawMessage(`[{"event_note":"MODIFY_IN_PROGRESS","status":"in_progress","reason":"NBN is moving the AVC to the new speed tier"}]`)
		default:
			o.FinishDate = time.Now().UTC().Format("2006-01-02 15:04:05")
			out.Status = "completed"
			out.State = "completed"
			out.FinishDate = o.FinishDate
			out.NBNCallbacks = json.RawMessage(`[{"event_note":"COMPLETED","status":"completed","reason":"Speed change complete; service is on the new tier"}]`)
			if svc, ok := s.services[o.ServiceID]; ok {
				svc.ProductID = o.ProductID
			}
		}
		s.save()
		return out, true
	}

	// An upgrade order (next-gen NTD swap / fibre connect / COAT) always needs
	// a tech onsite, whatever the LOC parity says.
	needsAppt := needsAppointment(o.LocationID) || o.NTDType != "" || o.COAT == "yes"

	// Pre-appointment: only when this LOC requires one AND it hasn't
	// been attached yet. Emit APPOINTMENT_REQUIRED so prism hands the
	// SMS magic-link to the customer.
	if needsAppt && o.AppointmentID == "" {
		out.Status = "in_progress"
		out.State = "appointment_required"
		out.NBNCallbacks = json.RawMessage(`[{"event_note":"APPOINTMENT_REQUIRED","status":"on_hold","reason":"NBN requires a technician appointment"}]`)
		s.save()
		return out, true
	}

	// Stage clock: time since the last state-advancing event.
	//   - appointment orders: time since the customer attached.
	//   - auto-provision orders: time since creation.
	var stageStart time.Time
	if o.AppointmentID != "" {
		stageStart = o.AppointmentAttachedAt
	} else {
		stageStart = o.CreatedAt
	}
	elapsed := time.Since(stageStart)

	switch {
	case elapsed < 60*time.Second:
		out.Status = "in_progress"
		if needsAppt {
			out.State = "appointment_scheduled"
			out.NBNCallbacks = json.RawMessage(`[{"event_note":"APPOINTMENT_BOOKED","status":"scheduled","reason":"NBN appointment reserved; technician will attend on scheduled date"}]`)
		} else {
			out.State = "accepted"
			out.NBNCallbacks = json.RawMessage(`[{"event_note":"ORDER_ACCEPTED","status":"accepted","reason":"NBN has accepted the order and queued it for provisioning"}]`)
		}

	case elapsed < 3*time.Minute:
		out.Status = "in_progress"
		if needsAppt {
			out.State = "technician_dispatched"
			out.NBNCallbacks = json.RawMessage(`[{"event_note":"SITE_VISIT_STARTED","status":"in_progress","reason":"NBN technician has arrived onsite and is starting the install"}]`)
		} else {
			out.State = "provisioning"
			out.NBNCallbacks = json.RawMessage(`[{"event_note":"PROVISIONING","status":"in_progress","reason":"NBN is provisioning the service on the access network"}]`)
		}

	case elapsed < 5*time.Minute:
		out.Status = "in_progress"
		out.State = "activating"
		out.NBNCallbacks = json.RawMessage(`[{"event_note":"ACTIVATION_IN_PROGRESS","status":"in_progress","reason":"Service is being activated on the NBN access network"}]`)

	default:
		o.FinishDate = time.Now().UTC().Format("2006-01-02 15:04:05")
		out.Status = "completed"
		out.State = "completed"
		out.FinishDate = o.FinishDate
		out.NBNCallbacks = json.RawMessage(`[{"event_note":"COMPLETED","status":"completed","reason":"Order fulfilled; service is live"}]`)
		if svc, ok := s.services[o.ServiceID]; ok {
			if action == "modify" {
				// NTD swap: service already has its identity — just move it to
				// the target product on the new NTD.
				svc.State = "active"
				svc.ProductID = o.ProductID
				svc.NTDID = "NTD" + padInt(o.ServiceID, 9) + "-" + o.NTDType
				svc.FinishDate = o.FinishDate
			} else {
				completeService(svc, o)
			}
		}
	}

	s.save()
	return out, true
}

// needsAppointment inspects the LOC ID's last digit. Odd → appointment
// required; even → auto-provision. Locations with no trailing digit (or
// an empty string — e.g. lazy-created orders) default to requiring an
// appointment so the customer magic-link flow still exercises end-to-end.
func needsAppointment(locationID string) bool {
	for i := len(locationID) - 1; i >= 0; i-- {
		ch := locationID[i]
		if ch >= '0' && ch <= '9' {
			return (ch-'0')%2 == 1
		}
	}
	return true
}

func (s *state) attachAppointment(orderID int, appointmentID string) (*fakeOrder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[orderID]
	if !ok {
		slog.Warn("lazy-creating unknown order on attach", "order_id", orderID)
		now := time.Now().UTC().Format("2006-01-02 15:04:05")
		o = &fakeOrder{
			OrderID:   orderID,
			StartDate: now,
			CreatedAt: time.Now().UTC(),
		}
		s.orders[orderID] = o
		if orderID >= s.nextOrderID {
			s.nextOrderID = orderID
		}
	}
	o.AppointmentID = appointmentID
	o.AppointmentAttachedAt = time.Now()
	if appt, ok := s.appointments[appointmentID]; ok {
		appt.OrderID = orderID
	}
	s.save()
	return o, true
}

// completeService fills out a service row with realistic details once the
// underlying order completes. The values are all synthetic but match the
// shape of real Leaptel responses so the prism import code paths exercise
// the same fields.
func completeService(svc *leaptel.Service, o *fakeOrder) {
	svc.State = "active"
	svc.AVCID = "AVC000000" + padInt(o.ServiceID, 6)
	svc.NTDID = "NTD" + padInt(o.ServiceID, 9)
	svc.PortID = "1"
	if o.NTDPort != "" {
		svc.PortID = o.NTDPort
	}
	// nbn_pri / opt_pri are the product instance IDs (Leaptel's
	// persistent handle for the provisioned service on the access
	// network), not QoS labels. Synthesise in the same PRI000000XXXXXX
	// shape as the real values so prism's product_instance_id column
	// ends up with something that looks realistic.
	svc.NBNPri = "PRI000000" + padInt(o.ServiceID, 6)
	svc.OptPri = ""
	svc.WholesalePlanID = 100
	svc.RetailPlanID = 200
	svc.PlanDescription = "FTTP 100/40"
	svc.PlanSpeed = "100/40"
	svc.PlanMonthlyCost = "79.00"
	svc.PPPoEPassword = "fake-dev-password"
	svc.POI.POIIdentifier = "POI001"
	svc.POI.POIName = "Sydney CBD"
	svc.POI.POIState = "NSW"
	svc.POI.CSAIdentifier = "CSA001"
	svc.POI.CSAName = "Sydney CBD"
	svc.POI.CSAPremises = 5000
	svc.Layer2Details.LVCID = "LVC0001"
	svc.Layer2Details.LVCName = "warp-lvc-syd-01"
	svc.Layer2Details.LVCDescription = "warp Sydney LVC 01"
	svc.Layer2Details.LVCCTag = "100"
	svc.FinishDate = o.FinishDate
}

func (s *state) getService(serviceID int) (*leaptel.Service, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[serviceID]
	return svc, ok
}

// --- appointment helpers -------------------------------------------------

func (s *state) createAppointment(locationID, start, end, slot string) *fakeAppt {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextAppointmentID++
	id := "APT" + padInt(s.nextAppointmentID, 8)
	a := &fakeAppt{
		AppointmentID: id,
		LocationID:    locationID,
		StartDateTime: start,
		EndDateTime:   end,
		SlotType:      slot,
	}
	s.appointments[id] = a
	s.save()
	return a
}

// --- assurance helpers ---------------------------------------------------

// createAssuranceRun mints a new test run for the given service. Tests
// with a baked-in fixture (see fixtures/) reproduce the exact shape real
// Leaptel returns for that test_number — including the test_result blob
// the portal UI parses. Unknown test numbers fall back to a synthetic
// Service Health envelope.
//
// Every run starts request_status=pending and completes once it's been
// polled at least twice over ~6 seconds — the captured fixtures show real
// runs taking 10–20s, so instant completion would leave the prism poller
// and the live-update UI untested in dev.
func (s *state) createAssuranceRun(serviceID, testNumber int, behaviour string) *fakeAssuranceRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextTestID++
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	resp := fixtureFor(testNumber)
	if resp == nil {
		resp = syntheticAssuranceResponse(testNumber)
	}

	// Patch identifiers + envelope dates so the response looks fresh.
	resp["test_id"] = s.nextTestID
	resp["service_id"] = serviceID
	resp["test_number"] = testNumber
	resp["requested_dt"] = now
	resp["submitted_dt"] = now
	resp["completed_dt"] = ""
	resp["request_status"] = "pending"

	run := &fakeAssuranceRun{
		TestID:     s.nextTestID,
		ServiceID:  serviceID,
		TestNumber: testNumber,
		Behaviour:  behaviour,
		StartedAt:  time.Now(),
		Response:   resp,
	}
	s.assuranceRuns[run.TestID] = run
	s.save()
	return run
}

func (s *state) getAssuranceRun(testID int) (*fakeAssuranceRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.assuranceRuns[testID]
	if !ok {
		return nil, false
	}
	run.Polls++
	// Tests complete once polled twice over ~6 seconds — long enough that
	// watchers see a run in flight, short enough not to drag in dev.
	if status, _ := run.Response["request_status"].(string); status == "pending" &&
		run.Polls >= 2 && time.Since(run.StartedAt) >= 6*time.Second {
		now := time.Now().UTC().Format("2006-01-02 15:04:05")
		run.Response["request_status"] = "completed"
		run.Response["completed_dt"] = now
	}
	s.save()
	return run, true
}

func (s *state) listAssuranceHistory(serviceID, page, perPage int) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := []map[string]any{}
	for _, r := range s.assuranceRuns {
		if r.ServiceID == serviceID {
			all = append(all, r.Response)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		ai, _ := all[i]["test_id"].(int)
		aj, _ := all[j]["test_id"].(int)
		return ai > aj
	})
	if perPage <= 0 {
		perPage = 20
	}
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	return all[start:end]
}

// --- utility -------------------------------------------------------------

func padInt(n, width int) string {
	return fmt.Sprintf("%0*d", width, n)
}
