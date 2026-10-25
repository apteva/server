package main

// The HTTP wire contract is deliberately server-owned and independent of any
// backend app name. Backend apps can implement it using ordinary SDK routes.
import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const runtimeBackendContract = "apteva.runtime-backend/v1"
const runtimeControlPrefix = "/_apteva/runtime/"

type runtimeBackendRef struct {
	InstallID int64  `json:"app_install_id"`
	ID        string `json:"backend_id"`
}
type runtimeResourceRef struct {
	InstallID int64  `json:"app_install_id"`
	Type      string `json:"resource_type"`
	ID        string `json:"resource_id"`
}
type runtimePlacement struct {
	Resources  *sdk.ResourcePolicy `json:"resource_policy,omitempty"`
	Ingress    *runtimeIngressSpec `json:"ingress,omitempty"`
	Type       string              `json:"type"`
	Backend    runtimeBackendRef   `json:"backend,omitempty"`
	Target     runtimeResourceRef  `json:"target,omitempty"`
	PoolID     string              `json:"pool_id,omitempty"`
	Options    map[string]any      `json:"options,omitempty"`
	GatewayURL string              `json:"gateway_url,omitempty"`
	Package    *runtimePackageSpec `json:"package,omitempty"`
}

func (p runtimePlacement) local() bool { return p.Type == "" || p.Type == "local" }
func (p runtimePlacement) validate() error {
	if p.Resources != nil {
		if e := p.Resources.Validate(); e != nil {
			return e
		}
	}
	if e := validateRuntimeIngressSpec(p.Ingress); e != nil {
		return e
	}
	if p.local() && p.Ingress != nil {
		return fmt.Errorf("managed ingress currently requires a remote backend")
	}
	if p.local() {
		if p.Backend.InstallID != 0 || p.PoolID != "" || p.Target.InstallID != 0 || len(p.Options) > 0 || p.Package != nil {
			return fmt.Errorf("local placement cannot select a backend or pool")
		}
		return nil
	}
	if p.Type != "backend" || p.Backend.InstallID <= 0 || !runtimeID(p.Backend.ID) {
		return fmt.Errorf("backend placement requires app_install_id and backend_id")
	}
	if p.PoolID == "" && (p.Target.InstallID <= 0 || p.Target.Type == "" || p.Target.ID == "") {
		return fmt.Errorf("a namespaced target or pool is required")
	}
	if p.GatewayURL == "" {
		return fmt.Errorf("remote execution requires a reachable gateway_url")
	}
	return runtimeSecureURL(p.GatewayURL)
}
func runtimeID(v string) bool {
	if len(v) == 0 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func runtimeSecureURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid runtime endpoint")
	}
	host := u.Hostname()
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return fmt.Errorf("runtime endpoints require HTTPS or a loopback tunnel")
	}
	return nil
}

type runtimeStateTransferPolicy = sdk.RuntimeStateTransferPolicy

type runtimeBackendCaps struct {
	ResourceCapabilities *sdk.ResourceCapabilities   `json:"resource_capabilities,omitempty"`
	StateTransfer        *runtimeStateTransferPolicy `json:"state_transfer,omitempty"`
	TargetDiscovery      *runtimeTargetDiscovery     `json:"target_discovery,omitempty"`
	TargetListing        bool                        `json:"target_listing,omitempty"`
	ManagedIngress       bool                        `json:"managed_ingress,omitempty"`
	TargetInspection     bool                        `json:"target_inspection,omitempty"`
	PackageDelivery      *runtimePackageDeliveryCaps `json:"package_delivery,omitempty"`
	FencingScope         string                      `json:"fencing_scope,omitempty"`
	Contract             string                      `json:"contract"`
	ID                   string                      `json:"id"`
	RuntimeKinds         []string                    `json:"runtime_kinds"`
	Platforms            []string                    `json:"platforms"`
	Formats              []string                    `json:"formats"`
	Protocols            []string                    `json:"protocols"`
	Snapshots            bool                        `json:"snapshots"`
	ConfirmedStop        bool                        `json:"confirmed_stop"`
	Fencing              bool                        `json:"fencing"`
	Replicas             bool                        `json:"replicas"`
	ResourceControls     bool                        `json:"resource_controls"`
	TargetSchema         json.RawMessage             `json:"target_schema,omitempty"`
	OptionsSchema        json.RawMessage             `json:"options_schema,omitempty"`
}
type runtimeBackendRegistration struct {
	DisplayName     string                  `json:"display_name,omitempty"`
	TargetDiscovery *runtimeTargetDiscovery `json:"target_discovery,omitempty"`
	InstallID       int64                   `json:"app_install_id"`
	ID              string                  `json:"id"`
	Endpoint        string                  `json:"endpoint"`
	Caps            runtimeBackendCaps      `json:"capabilities"`
	// An administrator can attest a legacy backend's optional delivery contract.
	PackageDelivery *runtimePackageDeliveryCaps `json:"package_delivery,omitempty"`
}
type runtimeAppCaps struct {
	PassiveStart       bool   `json:"passive_start"`
	Drain              bool   `json:"drain"`
	ReplicaSafe        bool   `json:"replica_safe"`
	WorkerCoordination bool   `json:"worker_coordination"`
	Routing            string `json:"routing"` // stateless or key; existing streams stay on their selected replica.
}
type runtimePortBinding struct {
	Protocol  string `json:"protocol"`
	Start     int    `json:"start"`
	End       int    `json:"end"`
	Exclusive bool   `json:"exclusive"`
}
type runtimeExecution struct {
	ProcessResources    map[string]string    `json:"process_resources,omitempty"`
	StateEntries        *int64               `json:"state_entries,omitempty"`
	StateAvailableBytes *int64               `json:"state_available_bytes,omitempty"`
	StateBytes          *int64               `json:"state_bytes,omitempty"` // Current uncompressed persistent bytes, reported by the owning backend.
	Endpoints           map[string]string    `json:"endpoints,omitempty"`
	AppCapabilities     runtimeAppCaps       `json:"app_capabilities"`
	ReleaseDigest       string               `json:"release_digest"`
	Platform            string               `json:"platform"`
	Handle              string               `json:"handle"`
	State               string               `json:"state"`
	Ready               bool                 `json:"ready"`
	StoppedConfirmed    bool                 `json:"stopped_confirmed"`
	HostKey             string               `json:"host_key"`
	Endpoint            string               `json:"control_endpoint,omitempty"`
	BrowserEndpoint     string               `json:"browser_endpoint,omitempty"`
	StateRef            string               `json:"state_ref,omitempty"`
	Ports               []runtimePortBinding `json:"ports,omitempty"`
	SnapshotSHA256      string               `json:"snapshot_sha256,omitempty"`
	Load                *float64             `json:"load,omitempty"` // backend-reported work load; never server CPU sampling.
}
type runtimeDeployment struct {
	Resources            sdk.ResourcePolicy         `json:"resource_policy,omitempty"`
	ResourceRevision     int64                      `json:"resource_revision,omitempty"`
	PersistentData       bool                       `json:"persistent_data,omitempty"`
	TargetInfo           *sdk.RuntimeTargetInfo     `json:"target_info,omitempty"`
	Package              *runtimePreparedPackage    `json:"package,omitempty"`
	PackagePreparation   *runtimePackagePreparation `json:"package_preparation,omitempty"`
	LocalPID             int                        `json:"-"`
	ID                   string                     `json:"id"`
	InstallID            int64                      `json:"install_id"`
	Generation           int64                      `json:"generation"`
	Replica              int                        `json:"replica"`
	Placement            runtimePlacement           `json:"placement"`
	Manifest             sdk.Manifest               `json:"manifest"`
	AppCaps              runtimeAppCaps             `json:"app_capabilities"`
	Status               string                     `json:"status"`
	Execution            runtimeExecution           `json:"execution"`
	CreatedAt            string                     `json:"created_at"`
	CredentialEncrypted  string                     `json:"-"`
	EnvironmentEncrypted string                     `json:"-"`
	LocalBin             string                     `json:"-"`
	LocalPort            int                        `json:"-"`
}
type runtimeOperation struct {
	StateTransfer    *runtimeStateTransferPolicy `json:"state_transfer,omitempty"`
	SnapshotSourceID string                      `json:"snapshot_source_id,omitempty"`
	CandidateStarted bool                        `json:"candidate_started,omitempty"`
	PreviousStopped  bool                        `json:"previous_stopped,omitempty"`
	RollbackID       string                      `json:"rollback_deployment_id,omitempty"`
	FailedID         string                      `json:"failed_deployment_id,omitempty"`
	ID               string                      `json:"id"`
	InstallID        int64                       `json:"install_id"`
	Kind             string                      `json:"kind"`
	Phase            string                      `json:"phase"`
	Status           string                      `json:"status"`
	DeploymentID     string                      `json:"deployment_id,omitempty"`
	PreviousID       string                      `json:"previous_deployment_id,omitempty"`
	Error            string                      `json:"error,omitempty"`
	IdempotencyKey   string                      `json:"idempotency_key"`
	Fingerprint      string                      `json:"-"`
	CreatedAt        string                      `json:"created_at"`
	UpdatedAt        string                      `json:"updated_at"`
	Owner            string                      `json:"-"`
	LeaseUntil       int64                       `json:"-"`
	SnapshotPath     string                      `json:"-"`
	SnapshotSHA256   string                      `json:"snapshot_sha256,omitempty"`
}
type runtimeBackendRequest struct {
	Resources        *sdk.ResourcePolicy         `json:"resource_policy,omitempty"`
	StateTransfer    *runtimeStateTransferPolicy `json:"state_transfer,omitempty"`
	TargetInfo       *sdk.RuntimeTargetInfo      `json:"target_info,omitempty"`
	Package          *runtimePackageReference    `json:"package,omitempty"`
	GatewayURL       string                      `json:"gateway_url,omitempty"`
	Contract         string                      `json:"contract"`
	OperationID      string                      `json:"operation_id"`
	IdempotencyKey   string                      `json:"idempotency_key"`
	DeploymentID     string                      `json:"deployment_id"`
	InstallID        int64                       `json:"install_id"`
	Generation       int64                       `json:"generation"`
	ProjectID        string                      `json:"project_id,omitempty"`
	Target           runtimeResourceRef          `json:"target"`
	Options          map[string]any              `json:"options,omitempty"`
	Manifest         sdk.Manifest                `json:"manifest"`
	Environment      map[string]string           `json:"environment,omitempty"`
	Execution        runtimeExecution            `json:"execution,omitempty"`
	ArtifactPlatform string                      `json:"artifact_platform,omitempty"`
	StateRef         string                      `json:"state_ref,omitempty"`
}
type runtimeBackend interface {
	Describe() runtimeBackendCaps
	Prepare(runtimeBackendRequest) (runtimeExecution, error)
	Start(runtimeBackendRequest) (runtimeExecution, error)
	Inspect(runtimeBackendRequest) (runtimeExecution, error)
	Stop(runtimeBackendRequest) (runtimeExecution, error)
	Fence(runtimeBackendRequest) (runtimeExecution, error)
	Delete(runtimeBackendRequest) error
}

func runtimeContains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}
func runtimeTimestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func runtimeHostKey(r runtimeResourceRef) string {
	return fmt.Sprintf("%d:%s:%s", r.InstallID, r.Type, r.ID)
}
func runtimeEndpoint(base, path string) string { return strings.TrimRight(base, "/") + path }

func runtimePublicEndpoint(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid public endpoint")
	}
	if u.Scheme == "wss" {
		return nil
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
		return runtimeSecureURL(u.String())
	}
	return runtimeSecureURL(raw)
}
