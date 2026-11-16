// Package sshwatch is a machine-level SSH access monitor. It reads sshd log
// records (journald, else auth.log/secure), watches authorized_keys files,
// stores normalized events durably, evaluates deterministic alert rules and
// delivers alerts to a webhook independently of apteva-server.
//
// One collector per machine holds collector.lock in the collector directory;
// other processes using the same directory serve read-only from the store and
// take over when the holder exits.
package sshwatch

import "time"

// Event kinds.
const (
	KindSSHAuth              = "ssh_auth"
	KindSSHSession           = "ssh_session"
	KindAuthorizedKeysChange = "authorized_keys_change"
	KindCollectorGap         = "collector_gap"
	KindCollectorStarted     = "collector_started"
	KindCollectorStopped     = "collector_stopped"
	KindPolicyChanged        = "policy_changed"
)

// Event results.
const (
	ResultAccepted     = "accepted"
	ResultFailed       = "failed"
	ResultInvalidUser  = "invalid_user"
	ResultOpened       = "opened"
	ResultClosed       = "closed"
	ResultDisconnected = "disconnected"
	ResultChanged      = "changed"
	// ResultBaseline marks the first inventory of an authorized_keys file.
	ResultBaseline = "baseline"
)

// Alert severities.
const (
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// Alert rule names.
const (
	RuleUnapprovedSource     = "unapproved_source"
	RuleRootLogin            = "root_login"
	RuleUnknownKey           = "unknown_key"
	RulePasswordLogin        = "password_login"
	RuleAuthorizedKeysChange = "authorized_keys_change"
	RuleFailureBurst         = "failure_burst"
	RuleCollectorGap         = "collector_gap"
	RulePolicyChanged        = "policy_changed"
)

// Alert is one deterministic rule match attached to an event.
type Alert struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// AuthorizedKey identifies one authorized_keys entry without its key material.
// OptionsHash is a digest of the entry's options (e.g. from=, command=) so an
// option change is visible without storing option values.
type AuthorizedKey struct {
	Fingerprint string `json:"fingerprint"`
	Type        string `json:"type"`
	Comment     string `json:"comment,omitempty"`
	OptionsHash string `json:"options_hash,omitempty"`
}

// Event is one normalized security event. ID is stable for the same source
// record, so re-reading a log never duplicates it. Seq is assigned by the
// store and increases monotonically.
type Event struct {
	ID          string    `json:"id"`
	Seq         int64     `json:"seq"`
	MachineID   string    `json:"machine_id"`
	Hostname    string    `json:"hostname,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
	Kind        string    `json:"kind"`
	Result      string    `json:"result,omitempty"`
	User        string    `json:"user,omitempty"`
	SourceIP    string    `json:"source_ip,omitempty"`
	SourcePort  int       `json:"source_port,omitempty"`
	Method      string    `json:"method,omitempty"`
	KeyType     string    `json:"key_type,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Message     string    `json:"message,omitempty"`
	Source      string    `json:"source,omitempty"`

	// authorized_keys_change
	Path        string          `json:"path,omitempty"`
	KeysAdded   []AuthorizedKey `json:"keys_added,omitempty"`
	KeysRemoved []AuthorizedKey `json:"keys_removed,omitempty"`
	KeyCount    int             `json:"key_count,omitempty"`

	// collector_gap
	GapFrom time.Time `json:"gap_from,omitzero"`
	GapTo   time.Time `json:"gap_to,omitzero"`

	Alerts []Alert `json:"alerts,omitempty"`
}

// EventsPage is the response of GET /security/events.
type EventsPage struct {
	Events    []Event `json:"events"`
	NextAfter int64   `json:"next_after"`
}

// Collector roles reported by Health.
const (
	RoleCollector = "collector"
	RoleReader    = "reader"
	RoleStopped   = "stopped"
)

// Health is the response of GET /security/health. Fields written by the lock
// holder (heartbeat, source, cursor, delivery state) are read from the shared
// store, so a reader process reports the active collector's state.
type Health struct {
	Role                 string    `json:"role"`
	LockHeld             bool      `json:"lock_held"`
	Dir                  string    `json:"dir"`
	MachineID            string    `json:"machine_id,omitempty"`
	Hostname             string    `json:"hostname,omitempty"`
	CollectorPID         int       `json:"collector_pid,omitempty"`
	StartedAt            time.Time `json:"started_at,omitzero"`
	StoppedAt            time.Time `json:"stopped_at,omitzero"`
	Heartbeat            time.Time `json:"heartbeat,omitzero"`
	Stale                bool      `json:"stale"`
	Source               string    `json:"source,omitempty"`
	SourceError          string    `json:"source_error,omitempty"`
	Cursor               string    `json:"cursor,omitempty"`
	LastEventAt          time.Time `json:"last_event_at,omitzero"`
	LastSeq              int64     `json:"last_seq"`
	EventCount           int64     `json:"event_count"`
	LastGapAt            time.Time `json:"last_gap_at,omitzero"`
	GapCount             int64     `json:"gap_count"`
	AuthorizedKeysError  string    `json:"authorized_keys_error,omitempty"`
	FreeBytes            uint64    `json:"free_bytes"`
	LowSpace             bool      `json:"low_space"`
	AlertBacklog         int64     `json:"alert_backlog"`
	AlertLastError       string    `json:"alert_last_error,omitempty"`
	AlertLastDeliveredAt time.Time `json:"alert_last_delivered_at,omitzero"`
	WebhookConfigured    bool      `json:"webhook_configured"`
	StoreError           string    `json:"store_error,omitempty"`
}

// Policy configures the deterministic rules and alert delivery. Source lists
// accept CIDRs or bare IP addresses. Fingerprints use the OpenSSH SHA256 form
// ("SHA256:" + unpadded base64).
//
// AlertWebhookSecret is write-only: reads return it empty with
// AlertWebhookSecretSet. A PUT with an empty secret keeps the stored secret
// unless AlertWebhookURL is cleared, which also clears the secret.
type Policy struct {
	ApprovedSources           []string `json:"approved_sources"`
	ApprovedFingerprints      []string `json:"approved_fingerprints"`
	RootLoginAllowed          bool     `json:"root_login_allowed"`
	RootApprovedSources       []string `json:"root_approved_sources"`
	FailureBurstThreshold     int      `json:"failure_burst_threshold"`
	FailureBurstWindowSeconds int      `json:"failure_burst_window_seconds"`
	AlertWebhookURL           string   `json:"alert_webhook_url,omitempty"`
	AlertWebhookSecret        string   `json:"alert_webhook_secret,omitempty"`
	AlertWebhookSecretSet     bool     `json:"alert_webhook_secret_set,omitempty"`
}

// Defaults for the failure-burst rule.
const (
	DefaultFailureBurstThreshold     = 10
	DefaultFailureBurstWindowSeconds = 600
)

// Webhook delivery headers.
const (
	HeaderSignature      = "X-Apteva-Signature"
	HeaderIdempotencyKey = "X-Apteva-Idempotency-Key"
	HeaderEventID        = "X-Apteva-Event-Id"
)

// AlertDelivery is the JSON body POSTed to the alert webhook. The body is
// signed with HMAC-SHA256(AlertWebhookSecret) and sent as
// "X-Apteva-Signature: sha256=<hex>". IdempotencyKey is "<event id>:<rule>";
// receivers must dedupe on it (a crash between a 2xx and its bookkeeping can
// redeliver).
type AlertDelivery struct {
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
	MachineID      string    `json:"machine_id"`
	Hostname       string    `json:"hostname,omitempty"`
	Alert          Alert     `json:"alert"`
	Event          Event     `json:"event"`
}
