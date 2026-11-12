package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The response bodies. They are written here rather than taken from the
// generated models so that every documented field is always present (null,
// never omitted) and timestamps always carry milliseconds (spec docs/api.md,
// "Naming"); the contract tests validate them against the specification.

// timestamp is a time sent as RFC 3339 UTC with milliseconds.
type timestamp time.Time

const timestampLayout = "2006-01-02T15:04:05.000Z"

// MarshalJSON implements json.Marshaler.
func (t timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).UTC().Format(timestampLayout) + `"`), nil
}

// ts returns t as a nullable timestamp: nil for the zero time.
func ts(t time.Time) *timestamp {
	if t.IsZero() {
		return nil
	}
	v := timestamp(t)
	return &v
}

// formatDuration writes d in the short Go form the specification uses: whole
// seconds, without zero units at the end ("72h", "1h30m", "90s", "0").
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d == 0 {
		return "0"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

type pageJSON[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type watchJSON struct {
	Name        string            `json:"name"`
	ID          string            `json:"id"`
	ManagedBy   string            `json:"managed_by"`
	Source      string            `json:"source"`
	Coordinates map[string]string `json:"coordinates"`
	Interval    string            `json:"interval"`
	Scheme      string            `json:"scheme"`
	Labels      map[string]string `json:"labels"`
	Policy      watchPolicyJSON   `json:"policy"`
	Status      watchStatusJSON   `json:"status"`
}

type watchPolicyJSON struct {
	Prerelease string   `json:"prerelease"`
	Include    []string `json:"include"`
	Exclude    []string `json:"exclude"`
	Constraint string   `json:"constraint"`
	Bump       []string `json:"bump"`
	Cooldown   string   `json:"cooldown"`
	Baseline   bool     `json:"baseline"`
}

type watchStatusJSON struct {
	BaselineDone      bool       `json:"baseline_done"`
	EffectiveInterval string     `json:"effective_interval"`
	LastCheckedAt     *timestamp `json:"last_checked_at"`
	NextCheckAt       *timestamp `json:"next_check_at"`
	ErrorCount        int64      `json:"error_count"`
}

type versionJSON struct {
	Version           string     `json:"version"`
	NormalizedVersion string     `json:"normalized_version"`
	Prerelease        bool       `json:"prerelease"`
	Yanked            bool       `json:"yanked"`
	PublishedAt       *timestamp `json:"published_at"`
	FirstSeenAt       timestamp  `json:"first_seen_at"`
	RemovedAt         *timestamp `json:"removed_at"`
}

type eventRecordJSON struct {
	Seq        int64           `json:"seq"`
	Event      json.RawMessage `json:"event"`
	RefEventID *string         `json:"ref_event_id"`
}

type eventPageJSON struct {
	Items     []eventRecordJSON `json:"items"`
	NextAfter int64             `json:"next_after"`
	HasMore   bool              `json:"has_more"`
}

type consumerJSON struct {
	Name      string             `json:"name"`
	ID        string             `json:"id"`
	Filter    consumerFilterJSON `json:"filter"`
	AckedSeq  int64              `json:"acked_seq"`
	AckedAt   *timestamp         `json:"acked_at"`
	CreatedAt timestamp          `json:"created_at"`
	UpdatedAt timestamp          `json:"updated_at"`
}

// consumerFilterJSON is a consumer's filter, stored and sent with every key
// present.
type consumerFilterJSON struct {
	Watch  []string `json:"watch"`
	Type   []string `json:"type"`
	Source []string `json:"source"`
}

type sinkJSON struct {
	Name         string         `json:"name"`
	ID           string         `json:"id"`
	ManagedBy    string         `json:"managed_by"`
	Kind         string         `json:"kind"`
	Config       map[string]any `json:"config"`
	URLRedacted  bool           `json:"url_redacted"`
	MaskedFields []string       `json:"masked_fields"`
}

type subscriptionJSON struct {
	Name      string                 `json:"name"`
	ID        string                 `json:"id"`
	ManagedBy string                 `json:"managed_by"`
	Sink      string                 `json:"sink"`
	Filter    subscriptionFilterJSON `json:"filter"`
	Policy    subscriptionPolicyJSON `json:"policy"`
	Template  string                 `json:"template"`
}

type subscriptionFilterJSON struct {
	Source  []string          `json:"source"`
	Labels  map[string]string `json:"labels"`
	Bump    []string          `json:"bump"`
	Package string            `json:"package"`
	Type    []string          `json:"type"`
}

type subscriptionPolicyJSON struct {
	Prerelease *string `json:"prerelease"`
	Cooldown   string  `json:"cooldown"`
}

type deliveryJSON struct {
	ID               string     `json:"id"`
	EventID          string     `json:"event_id"`
	SubscriptionID   string     `json:"subscription_id"`
	SubscriptionName *string    `json:"subscription_name"`
	IdempotencyKey   string     `json:"idempotency_key"`
	Status           string     `json:"status"`
	Attempts         int64      `json:"attempts"`
	NextAttemptAt    *timestamp `json:"next_attempt_at"`
	HeldUntil        *timestamp `json:"held_until"`
	LastError        *string    `json:"last_error"`
}

type replayResultJSON struct {
	Replayed   int            `json:"replayed"`
	DryRun     bool           `json:"dry_run"`
	Deliveries []deliveryJSON `json:"deliveries"`
	Truncated  bool           `json:"truncated"`
}

type pluginJSON struct {
	Kind         string           `json:"kind"`
	Interface    string           `json:"interface"`
	Runtime      string           `json:"runtime"`
	Version      *string          `json:"version"`
	ABI          *string          `json:"abi"`
	ConfigSchema json.RawMessage  `json:"config_schema"`
	AllowedHosts []pluginHostJSON `json:"allowed_hosts"`
}

type pluginHostJSON struct {
	Host            string `json:"host"`
	Origin          string `json:"origin"`
	CredentialPorts []int  `json:"credential_ports"`
}

// response is the response object of every operation but the event stream:
// a status, a JSON body and headers. It implements each generated
// Visit<Operation>Response method, so the strict server sends it.
type response struct {
	status int
	body   any // nil: no body
	header http.Header
}

func jsonOK(body any) response { return response{status: http.StatusOK, body: body} }

func (r response) write(w http.ResponseWriter) error {
	h := w.Header()
	for k, v := range r.header {
		h[k] = v
	}
	if r.body == nil {
		w.WriteHeader(r.status)
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r.body); err != nil {
		return err
	}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(r.status)
	_, err := w.Write(b.Bytes())
	return err
}

// Visit methods of the strict server's response object interfaces.

func (r response) VisitListDeliveriesResponse(w http.ResponseWriter) error     { return r.write(w) }
func (r response) VisitGetDeliveryResponse(w http.ResponseWriter) error        { return r.write(w) }
func (r response) VisitReplayDeliveryResponse(w http.ResponseWriter) error     { return r.write(w) }
func (r response) VisitListEventsResponse(w http.ResponseWriter) error         { return r.write(w) }
func (r response) VisitGetEventResponse(w http.ResponseWriter) error           { return r.write(w) }
func (r response) VisitListPluginsResponse(w http.ResponseWriter) error        { return r.write(w) }
func (r response) VisitGetPluginResponse(w http.ResponseWriter) error          { return r.write(w) }
func (r response) VisitCreateReplayResponse(w http.ResponseWriter) error       { return r.write(w) }
func (r response) VisitListSinksResponse(w http.ResponseWriter) error          { return r.write(w) }
func (r response) VisitDeleteSinkResponse(w http.ResponseWriter) error         { return r.write(w) }
func (r response) VisitGetSinkResponse(w http.ResponseWriter) error            { return r.write(w) }
func (r response) VisitPutSinkResponse(w http.ResponseWriter) error            { return r.write(w) }
func (r response) VisitListSubscriptionsResponse(w http.ResponseWriter) error  { return r.write(w) }
func (r response) VisitDeleteSubscriptionResponse(w http.ResponseWriter) error { return r.write(w) }
func (r response) VisitGetSubscriptionResponse(w http.ResponseWriter) error    { return r.write(w) }
func (r response) VisitPutSubscriptionResponse(w http.ResponseWriter) error    { return r.write(w) }
func (r response) VisitListWatchesResponse(w http.ResponseWriter) error        { return r.write(w) }
func (r response) VisitDeleteWatchResponse(w http.ResponseWriter) error        { return r.write(w) }
func (r response) VisitGetWatchResponse(w http.ResponseWriter) error           { return r.write(w) }
func (r response) VisitPutWatchResponse(w http.ResponseWriter) error           { return r.write(w) }
func (r response) VisitListWatchVersionsResponse(w http.ResponseWriter) error  { return r.write(w) }
func (r response) VisitListConsumersResponse(w http.ResponseWriter) error      { return r.write(w) }
func (r response) VisitGetConsumerResponse(w http.ResponseWriter) error        { return r.write(w) }
func (r response) VisitPutConsumerResponse(w http.ResponseWriter) error        { return r.write(w) }
func (r response) VisitDeleteConsumerResponse(w http.ResponseWriter) error     { return r.write(w) }
func (r response) VisitAckConsumerResponse(w http.ResponseWriter) error        { return r.write(w) }

// Change is the value of the Angelia-Change header (spec docs/api.md
// decision 6).
type Change string

// The changes a PUT or DELETE reports.
const (
	ChangeCreated   Change = "created"
	ChangeUpdated   Change = "updated"
	ChangeUnchanged Change = "unchanged"
	ChangeDeleted   Change = "deleted"
)

// putResponse is 201 with Location for a created resource, 200 otherwise.
func putResponse(c Change, location string, body any) response {
	h := http.Header{}
	h.Set("Angelia-Change", string(c))
	status := http.StatusOK
	if c == ChangeCreated {
		status = http.StatusCreated
		h.Set("Location", location)
	}
	return response{status: status, body: body, header: h}
}

func deleteResponse() response {
	h := http.Header{}
	h.Set("Angelia-Change", string(ChangeDeleted))
	return response{status: http.StatusNoContent, header: h}
}
