package leaptel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultBaseURL points at the local fake server (cmd/leaptel-api). This
// is intentional: in production we set LEAPTEL_BASE_URL explicitly to the
// real Leaptel URL (https://api.wholesaler.leaptel.com.au/api/v1/wholesaler).
// Defaulting to the fake means a forgotten config can't accidentally fire
// real orders — connection-refused is loud, mis-provisioning a customer is
// silent and expensive.
const defaultBaseURL = "http://localhost:9091/api/v1/wholesaler"

type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

// NewClient builds a Leaptel API client. baseURL may be empty to use the
// production Leaptel URL; pass a different value to point at a fake/dev
// server (see cmd/leaptel-api).
func NewClient(baseURL, username, password string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL:  baseURL,
		username: username,
		password: password,
		http:     &http.Client{},
	}
}

// ServiceQualification runs a service qualification for a given NBN location ID.
// Returns the raw JSON response so we can inspect the full structure.
func (c *Client) ServiceQualification(locationID string) (json.RawMessage, error) {
	return c.serviceQualification(locationID, "")
}

// ServiceQualificationWithAVC runs a service qualification with an AVC ID for validation.
func (c *Client) ServiceQualificationWithAVC(locationID, avcID string) (json.RawMessage, error) {
	return c.serviceQualification(locationID, avcID)
}

func (c *Client) serviceQualification(locationID, avcID string) (json.RawMessage, error) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer writer.Close()
		writer.WriteField("location_id", locationID)
		if avcID != "" {
			writer.WriteField("avc_id", avcID)
		}
	}()

	req, err := http.NewRequest("POST", c.baseURL+"/service-qualifications", pr)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	// The API returns non-200 when sub-carrier queries (ltn, asn, etc.) fail,
	// even when the primary nbn result is valid. Only fail on auth errors.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("api returned %d: %s", resp.StatusCode, string(body))
	}

	return json.RawMessage(body), nil
}

// Product is one wholesale NBN product as the products API lists it. The
// monthly_cost is what Leaptel charges us — our margin's floor.
type Product struct {
	PlanID      int      `json:"plan_id"`
	PlanName    string   `json:"plan_name"`
	ProductID   string   `json:"product_id"`
	Download    string   `json:"download"` // "25Mbps"
	Upload      string   `json:"upload"`
	MonthlyCost string   `json:"monthly_cost"` // dollars, e.g. "36.14"
	HandoffType string   `json:"handoff_type"`
	AccessType  []string `json:"access_type"`
}

// Products returns the typed wholesale product list for a carrier.
func (c *Client) Products(carrier string) ([]Product, error) {
	raw, err := c.ListProducts(carrier)
	if err != nil {
		return nil, err
	}
	var out []Product
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parsing products: %w", err)
	}
	return out, nil
}

// ListProducts returns all available wholesale products for a carrier (typically "nbn").
func (c *Client) ListProducts(carrier string) (json.RawMessage, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/products?carrier="+carrier, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("api returned %d: %s", resp.StatusCode, string(body))
	}

	return json.RawMessage(body), nil
}

// --- Customer / Service typed models for import ---

type Customer struct {
	CustomerID  int    `json:"customer_id"`
	CompanyName string `json:"company_name"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	Email       string `json:"email"`
	Mobile      string `json:"mobile"`
	Phone       string `json:"phone"`
	Address1    string `json:"address1"`
	Address2    string `json:"address2"`
	City        string `json:"city"`
	State       string `json:"state"`
	Postcode    string `json:"postcode"`
	Active      string `json:"active"` // "yes"/"no"
	BillingDay  string `json:"billing_day"`
}

type Pagination struct {
	TotalRecords   int  `json:"total_records"`
	CurrentPage    int  `json:"current_page"`
	RecordsPerPage int  `json:"records_per_page"`
	TotalPages     int  `json:"total_pages"`
	NextPage       *int `json:"next_page"`
	PreviousPage   *int `json:"previous_page"`
}

type CustomersPage struct {
	Pagination Pagination `json:"pagination"`
	Customers  []Customer `json:"customers"`
}

// Service is the full object returned by GET /services/{id}. It's a superset
// of the lean customer_service entry returned by GET /customers/{id}/services.
type Service struct {
	ServiceID        int    `json:"service_id"`
	CustomerID       int    `json:"customer_id"`
	State            string `json:"state"`
	Identifier       string `json:"identifier"` // LOC id
	AVCID            string `json:"avc_id"`
	NTDID            string `json:"ntd_id"`
	PortID           string `json:"port_id"`
	CopperPairID     string `json:"copperpair_id"`
	AccessTechnology string `json:"access_technology"`
	NBNPri           string `json:"nbn_pri"`
	OptPri           string `json:"opt_pri"`
	ServiceAddress   string `json:"service_address"`
	WholesalePlanID  int    `json:"wholesale_plan_id"`
	RetailPlanID     int    `json:"retail_plan_id"`
	PlanDescription  string `json:"plan_description"`
	PlanSpeed        string `json:"plan_speed"`
	PlanMonthlyCost  string `json:"plan_monthly_cost"`
	PPPoEUsername    string `json:"ppoe_username"`
	PPPoEPassword    string `json:"ppoe_password"`
	ServiceGroup     string `json:"type_service_group"`
	ProductID        string `json:"product_id"`
	POI              struct {
		POIIdentifier string `json:"poi_identifier"`
		POIName       string `json:"poi_name"`
		POIState      string `json:"poi_state"`
		CSAIdentifier string `json:"csa_identifier"`
		CSAName       string `json:"csa_name"`
		CSAPremises   int    `json:"csa_premises"`
	} `json:"poi"`
	Layer2Details struct {
		LVCID          string `json:"lvc_id"`
		LVCName        string `json:"lvc_name"`
		LVCDescription string `json:"lvc_description"`
		LVCCTag        string `json:"lvc_c_tag"` // C-VLAN tag (int as string)
	} `json:"layer_2_details"`
	StartDate  string `json:"start_date"`
	FinishDate string `json:"finish_date"`
}

// CustomerServiceRef is the lean list entry from GET /customers/{id}/services.
// Only used to enumerate service_ids; follow up with GetService for full data.
type CustomerServiceRef struct {
	ServiceID    int    `json:"service_id"`
	CustomerID   int    `json:"customer_id"`
	State        string `json:"state"`
	ServiceGroup string `json:"service_group"` // "data", "voice", etc.
}

// ListCustomers fetches a single page of customers (50 per page).
func (c *Client) ListCustomers(page int) (*CustomersPage, error) {
	var out CustomersPage
	path := fmt.Sprintf("/customers?page=%d", page)
	if err := c.doJSON("GET", path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCustomerServiceRefs returns the list of service refs for a customer.
func (c *Client) GetCustomerServiceRefs(customerID int) ([]CustomerServiceRef, error) {
	var out []CustomerServiceRef
	path := fmt.Sprintf("/customers/%d/services", customerID)
	if err := c.doJSON("GET", path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetService fetches the full service object by ID.
func (c *Client) GetService(serviceID int) (*Service, error) {
	var out Service
	path := fmt.Sprintf("/services/%d", serviceID)
	if err := c.doJSON("GET", path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) doJSON(method, path string, out any) error {
	req, err := http.NewRequest(method, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("unmarshalling %s response: %w", path, err)
	}
	return nil
}

// doJSONRaw is like doJSON but also returns the raw response body so callers
// can persist the exact bytes (useful when capturing sample payloads).
func (c *Client) doJSONRaw(method, path string, out any) (json.RawMessage, error) {
	req, err := http.NewRequest(method, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(body))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return json.RawMessage(body), fmt.Errorf("unmarshalling %s response: %w", path, err)
		}
	}
	return json.RawMessage(body), nil
}

// --- Service Assurance (diagnostic tests) ---

type AssuranceTestType struct {
	TestName         string `json:"test_name"`
	Abbreviation     string `json:"abbreviation"`
	ServiceImpacting int    `json:"service_impacting"` // 0/1
	Behaviour        string `json:"behaviour"`         // "sync" | "async"
	TestCode         string `json:"test_code"`
	Description      string `json:"description"`
}

// NumberedAssuranceTestType promotes the JSON object key (test number) to a field.
type NumberedAssuranceTestType struct {
	TestNumber int `json:"test_number"`
	AssuranceTestType
}

// AssuranceResult is the full /services/{id}/assurance-tests/{id} payload.
// The Leaptel spec lists test_result as a string (likely a JSON blob itself);
// we keep it as RawMessage so we don't lose fidelity.
type AssuranceResult struct {
	TestID         int             `json:"test_id"`
	ServiceID      int             `json:"service_id"`
	TypeID         int             `json:"type_id"`
	TestNumber     int             `json:"test_number"`
	TestName       string          `json:"test_name"`
	RequestStatus  string          `json:"request_status"`
	RequestedDt    string          `json:"requested_dt"`
	SubmittedDt    string          `json:"submitted_dt"`
	CompletedDt    string          `json:"completed_dt"`
	ProviderTestID string          `json:"provider_test_id"`
	Provider       string          `json:"provider"`
	TestResult     json.RawMessage `json:"test_result"`
	ServiceType    string          `json:"serviceType"`
	AccessType     string          `json:"accessType"`
	ServiceSpeed   string          `json:"serviceSpeed"`
	Address        string          `json:"address"`
	Locality       string          `json:"locality"`
	Postcode       string          `json:"postcode"`
	GroupID        string          `json:"group_id"`
	ServiceQual    string          `json:"service_qual"`
}

// ListAssuranceTests returns the test catalog for a (carrier, access_technology).
// The API returns a JSON object keyed by test_number (e.g. {"1":{...},"8":{...}}),
// which we flatten into a slice with TestNumber promoted.
func (c *Client) ListAssuranceTests(carrier, accessTech string) ([]NumberedAssuranceTestType, json.RawMessage, error) {
	path := fmt.Sprintf("/service-assurance-tests?carrier=%s&access_technology=%s", carrier, accessTech)
	raw, err := c.doJSONRaw("GET", path, nil)
	if err != nil {
		return nil, raw, err
	}
	var m map[string]AssuranceTestType
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, raw, fmt.Errorf("unmarshalling assurance catalog: %w", err)
	}
	out := make([]NumberedAssuranceTestType, 0, len(m))
	for k, v := range m {
		n, err := strconv.Atoi(k)
		if err != nil {
			return nil, raw, fmt.Errorf("non-integer test key %q: %w", k, err)
		}
		out = append(out, NumberedAssuranceTestType{TestNumber: n, AssuranceTestType: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TestNumber < out[j].TestNumber })
	return out, raw, nil
}

// RunAssuranceTest POSTs a new test and returns the initial response.
// Sync tests may already include completed results; async tests return
// the test_id so you can poll with GetAssuranceTest.
func (c *Client) RunAssuranceTest(serviceID, testNumber int) (*AssuranceResult, json.RawMessage, error) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		defer writer.Close()
		writer.WriteField("test_number", strconv.Itoa(testNumber))
	}()

	path := fmt.Sprintf("/services/%d/assurance-tests", serviceID)
	req, err := http.NewRequest("POST", c.baseURL+path, pr)
	if err != nil {
		return nil, nil, fmt.Errorf("building request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, json.RawMessage(body), fmt.Errorf("POST %s: %d %s", path, resp.StatusCode, string(body))
	}
	var out AssuranceResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, json.RawMessage(body), fmt.Errorf("unmarshalling run response: %w", err)
	}
	return &out, json.RawMessage(body), nil
}

// GetAssuranceTest fetches a single test result by Leaptel test_id.
func (c *Client) GetAssuranceTest(serviceID, testID int) (*AssuranceResult, json.RawMessage, error) {
	var out AssuranceResult
	path := fmt.Sprintf("/services/%d/assurance-tests/%d", serviceID, testID)
	raw, err := c.doJSONRaw("GET", path, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// ListAssuranceHistory returns one page (20 items) of historical tests.
func (c *Client) ListAssuranceHistory(serviceID, page int) ([]AssuranceResult, json.RawMessage, error) {
	var out []AssuranceResult
	path := fmt.Sprintf("/services/%d/assurance-tests-history?page=%d", serviceID, page)
	raw, err := c.doJSONRaw("GET", path, &out)
	if err != nil {
		return nil, raw, err
	}
	return out, raw, nil
}

// --- Customers ---

type CreateCustomerParams struct {
	FirstName   string
	LastName    string
	CompanyName string // business accounts
	Birthdate   string // YYYY-MM-DD
	Email       string
	Mobile      string
	Phone       string
	Address     string
	City        string
	State       string
	Postcode    string
}

// CreateCustomer creates a new customer via POST /customers. Leaptel expects
// multipart/form-data; all listed fields are required except phone per the
// OpenAPI spec.
func (c *Client) CreateCustomer(p CreateCustomerParams) (*Customer, json.RawMessage, error) {
	form := map[string]string{
		"first_name": p.FirstName,
		"last_name":  p.LastName,
		"birthdate":  p.Birthdate,
		"email":      p.Email,
		"mobile":     p.Mobile,
		"address":    p.Address,
		"city":       p.City,
		"state":      p.State,
		"postcode":   p.Postcode,
	}
	if p.Phone != "" {
		form["phone"] = p.Phone
	}
	if p.CompanyName != "" {
		form["company_name"] = p.CompanyName
	}
	var out Customer
	raw, err := c.doMultipart("POST", "/customers", form, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// FindCustomerByEmail walks the paginated /customers list looking for a
// match. Leaptel doesn't expose a server-side filter, so this is O(pages)
// — fine for our scale.
func (c *Client) FindCustomerByEmail(email string) (*Customer, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, fmt.Errorf("email is empty")
	}
	page := 1
	for {
		pageResp, err := c.ListCustomers(page)
		if err != nil {
			return nil, err
		}
		for _, cust := range pageResp.Customers {
			if strings.EqualFold(cust.Email, email) {
				return &cust, nil
			}
		}
		if pageResp.Pagination.NextPage == nil {
			return nil, nil // not found
		}
		page = *pageResp.Pagination.NextPage
	}
}

// --- Orders ---

// CreateOrderParams mirrors the POST /orders body. All non-zero fields are
// sent. Optional fields should be left at their zero value to omit.
type CreateOrderParams struct {
	CustomerID     int
	Carrier        string // "nbn" | "opticomm"
	LocationID     string
	ProductID      string // preferred over PlanID when both set
	PlanID         int
	OrderType      string // always "data" for broadband
	ConnectionType string // "new" | "transfer"

	// Contact (all required)
	ContactFirstName string
	ContactLastName  string
	ContactAddress   string
	ContactSuburb    string
	ContactState     string
	ContactPostcode  string
	ContactEmail     string
	ContactPhone     string

	// Optional / conditional
	OrderAfter       string // YYYY-MM-DD
	CADate           string // YYYY-MM-DD fixed appointment date
	NTDID            string
	NTDPort          string
	NTDType          string // "1_PORT" | "4_PORT" | "NTD_2.5" — required for new FTTP installs
	AVCID            string // transfer only
	AppointmentID    string // "APT..." — if omitted, Leaptel auto-books
	SelfInstall      string // "yes" | "no" — HFC only
	COAT             string // "yes" — Change Of Access Technology (FTTC/N → FTTP fibre connect)
	InterimServiceID string // existing copper service being COAT'd; only with COAT=yes
	CopperPair       string // "new" | "CPI..."
	Username         string
	Password         string
}

// CreateOrderResponse is the body of POST /orders on success.
type CreateOrderResponse struct {
	OrderID    int    `json:"order_id"`
	ServiceID  int    `json:"service_id"`
	LocationID string `json:"location_id"`
}

// CreateOrder submits a new order via POST /orders (multipart).
func (c *Client) CreateOrder(p CreateOrderParams) (*CreateOrderResponse, json.RawMessage, error) {
	form := map[string]string{
		"customer_id":        strconv.Itoa(p.CustomerID),
		"carrier":            p.Carrier,
		"location_id":        p.LocationID,
		"order_type":         p.OrderType,
		"connection_type":    p.ConnectionType,
		"contact_first_name": p.ContactFirstName,
		"contact_last_name":  p.ContactLastName,
		"contact_address":    p.ContactAddress,
		"contact_suburb":     p.ContactSuburb,
		"contact_state":      p.ContactState,
		"contact_postcode":   p.ContactPostcode,
	}
	if p.ContactEmail != "" {
		form["contact_email"] = p.ContactEmail
	}
	if p.ContactPhone != "" {
		form["contact_phone"] = p.ContactPhone
	}
	// Leaptel's order API requires plan_id (their product_id isn't unique —
	// HOME-FAST is both 100/20 and 500/50). Send the plan id; product_id is
	// only the fallback when we somehow don't have one.
	if p.PlanID != 0 {
		form["plan_id"] = strconv.Itoa(p.PlanID)
	} else if p.ProductID != "" {
		form["product_id"] = p.ProductID
	}
	// No realm, ever: we only buy L2 here — we are our own PPP aggregator,
	// and an account's realm is our routers' business, not Leaptel's.
	for k, v := range map[string]string{
		"order_after":        p.OrderAfter,
		"ca_date":            p.CADate,
		"ntd_id":             p.NTDID,
		"ntd_port":           p.NTDPort,
		"ntd_type":           p.NTDType,
		"avc_id":             p.AVCID,
		"appointment_id":     p.AppointmentID,
		"self_install":       p.SelfInstall,
		"coat":               p.COAT,
		"interim_service_id": p.InterimServiceID,
		"copper_pair":        p.CopperPair,
		"username":           p.Username,
		"password":           p.Password,
	} {
		if v != "" {
			form[k] = v
		}
	}
	var out CreateOrderResponse
	raw, err := c.doMultipart("POST", "/orders", form, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// ModifyServiceParams mirrors the POST /services/{id}/modify body. A bare
// ProductID is a speed change; ReplacementNTD/AppointmentID/SelfInstall drive
// an Accelerate Great NTD swap (the second leg of the interim→modify dance).
type ModifyServiceParams struct {
	PlanID         int    // Leaptel plan id of the target — preferred (product_id isn't unique)
	ProductID      string // fallback only
	ReplacementNTD string // "1_PORT" | "4_PORT" (FTTP), "NTD_2.5" (HFC)
	AppointmentID  string // "APT..." — auto-booked if empty
	SelfInstall    string // "yes" | "no" — HFC only
}

// ModifyService raises a modify order moving an active service to a new
// product (speed change), optionally swapping the NTD. The response carries
// the order to poll. order_type=plan and order_after are required by
// Leaptel; order_after defaults to today (apply now).
func (c *Client) ModifyService(serviceID int, p ModifyServiceParams) (*CreateOrderResponse, json.RawMessage, error) {
	form := map[string]string{
		"order_type":  "plan",
		"order_after": time.Now().Format("2006-01-02"),
	}
	if p.PlanID != 0 {
		form["plan_id"] = strconv.Itoa(p.PlanID)
	} else if p.ProductID != "" {
		form["product_id"] = p.ProductID
	}
	for k, v := range map[string]string{
		"replacement_ntd": p.ReplacementNTD,
		"appointment_id":  p.AppointmentID,
		"self_install":    p.SelfInstall,
	} {
		if v != "" {
			form[k] = v
		}
	}
	var out CreateOrderResponse
	raw, err := c.doMultipart("POST", fmt.Sprintf("/services/%d/modify", serviceID), form, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// CancelService raises a cancellation order for a service.
func (c *Client) CancelService(serviceID int) (json.RawMessage, error) {
	return c.doMultipart("POST", fmt.Sprintf("/services/%d/cancel", serviceID), map[string]string{}, nil)
}

// Order matches the wholesaler_order schema from GET /orders/{id}.
type Order struct {
	OrderID      int             `json:"order_id"`
	Status       string          `json:"status"`
	State        string          `json:"state"`
	ServiceID    int             `json:"service_id"`
	CustomerID   int             `json:"customer_id"`
	Action       string          `json:"action"`
	StartDate    string          `json:"start_date"`
	FinishDate   string          `json:"finish_date"`
	ProductID    string          `json:"product_id"`
	NBNCallbacks json.RawMessage `json:"nbn_callbacks"`
}

// GetOrder fetches an order's current status.
func (c *Client) GetOrder(orderID int) (*Order, json.RawMessage, error) {
	var out Order
	path := fmt.Sprintf("/orders/%d", orderID)
	raw, err := c.doJSONRaw("GET", path, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// --- Appointments ---

type TimeslotsParams struct {
	StartDate             string // YYYY-MM-DD (required)
	EndDate               string // YYYY-MM-DD (required)
	SlotType              string // "AM" | "PM" (required)
	PriorityAssist        string // "Yes" | "No" (required)
	LocationID            string
	AppointmentID         string
	ServiceID             string
	CopperPairID          string
	DemandType            string
	AlternativeTechnology string // "Fibre" for COAT upgrades
}

// Timeslot is one available appointment window, normalised from the
// carrier's availableTimeSlot[] entries.
type Timeslot struct {
	StartDateTime string `json:"startDateTime"`
	EndDateTime   string `json:"endDateTime"`
	SlotType      string `json:"appointmentSlotType"`
}

// Timeslots returns the available windows for a query, parsed from the
// envelope's availableTimeSlot[].
func (c *Client) Timeslots(p TimeslotsParams) ([]Timeslot, error) {
	raw, err := c.GetAppointmentTimeslots(p)
	if err != nil {
		return nil, err
	}
	var env struct {
		AvailableTimeSlot []Timeslot `json:"availableTimeSlot"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parsing timeslots: %w", err)
	}
	return env.AvailableTimeSlot, nil
}

// GetAppointmentTimeslots queries available appointment slots.
func (c *Client) GetAppointmentTimeslots(p TimeslotsParams) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("start_date", p.StartDate)
	q.Set("end_date", p.EndDate)
	q.Set("slot_type", p.SlotType)
	q.Set("priority_assist", p.PriorityAssist)
	if p.LocationID != "" {
		q.Set("location_id", p.LocationID)
	}
	if p.AppointmentID != "" {
		q.Set("appointment_id", p.AppointmentID)
	}
	if p.ServiceID != "" {
		q.Set("service_id", p.ServiceID)
	}
	if p.CopperPairID != "" {
		q.Set("copper_pair_id", p.CopperPairID)
	}
	if p.DemandType != "" {
		q.Set("demand_type", p.DemandType)
	}
	if p.AlternativeTechnology != "" {
		q.Set("alternative_technology", p.AlternativeTechnology)
	}
	raw, err := c.doJSONRaw("GET", "/appointments/time-slots?"+q.Encode(), nil)
	return raw, err
}

// AppointmentContact is one entry in the end_user_contact array for POST /appointments.
type AppointmentContact struct {
	Type  string `json:"type"` // "Primary Contact" | "Secondary Contact"
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Notes string `json:"notes,omitempty"`
}

// AppointmentSlot is the time window booked.
type AppointmentSlot struct {
	StartDateTime string `json:"start_date_time"`
	EndDateTime   string `json:"end_date_time"`
	SlotType      string `json:"slot_type"`
}

// CreateAppointmentParams matches POST /appointments JSON body.
type CreateAppointmentParams struct {
	LocationID      string               `json:"location_id"`
	EndUserType     string               `json:"end_user_type"`   // "Residential" | "Business"
	DemandType      string               `json:"demand_type"`     // e.g. "Standard Install"
	PriorityAssist  string               `json:"priority_assist"` // "Yes" | "No"
	AppointmentSlot AppointmentSlot      `json:"appointment_slot"`
	EndUserContact  []AppointmentContact `json:"end_user_contact"`
	RelatedEntity   map[string]string    `json:"related_entity,omitempty"` // {id, type}
	SiteAccess      map[string]any       `json:"site_access,omitempty"`
}

// CreateAppointment reserves a slot. The returned appointment id is what
// you pass to PATCH /orders/{id}/appointment to attach it to an order.
func (c *Client) CreateAppointment(p CreateAppointmentParams) (json.RawMessage, error) {
	return c.doJSONBody("POST", "/appointments", p, nil)
}

// CreateAppointmentID reserves a slot and returns just the appointment id.
func (c *Client) CreateAppointmentID(p CreateAppointmentParams) (string, error) {
	raw, err := c.CreateAppointment(p)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("appointment response missing id")
	}
	return out.ID, nil
}

// AttachAppointmentToOrder links an existing appointment (by id) to an
// in-flight order. Used when the NBN returns a "appointment required"
// signal after order creation.
func (c *Client) AttachAppointmentToOrder(orderID int, appointmentID string) (json.RawMessage, error) {
	body := map[string]string{"appointment_id": appointmentID}
	path := fmt.Sprintf("/orders/%d/appointment", orderID)
	return c.doJSONBody("PATCH", path, body, nil)
}

// --- helpers ---

// doMultipart submits a multipart/form-data body. out may be nil.
func (c *Client) doMultipart(method, path string, fields map[string]string, out any) (json.RawMessage, error) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer writer.Close()
		for k, v := range fields {
			writer.WriteField(k, v)
		}
	}()

	req, err := http.NewRequest(method, c.baseURL+path, pr)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return json.RawMessage(body), fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(body))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return json.RawMessage(body), fmt.Errorf("unmarshalling %s response: %w", path, err)
		}
	}
	return json.RawMessage(body), nil
}

// doJSONBody submits a JSON body. out may be nil.
func (c *Client) doJSONBody(method, path string, body, out any) (json.RawMessage, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshalling body: %w", err)
	}
	req, err := http.NewRequest(method, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return json.RawMessage(respBody), fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return json.RawMessage(respBody), fmt.Errorf("unmarshalling %s response: %w", path, err)
		}
	}
	return json.RawMessage(respBody), nil
}
