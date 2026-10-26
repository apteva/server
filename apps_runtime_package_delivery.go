package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

func (b *remoteRuntimeBackend) packageRequest(ctx context.Context, action string, body io.Reader, length int64, contentType, key string) (int, []byte, error) {
	method := http.MethodPost
	if strings.Contains(action, "/files/") {
		method = http.MethodPut
	}
	req, err := http.NewRequestWithContext(ctx, method, runtimeEndpoint(b.base, b.registration.Endpoint)+"/artifacts"+action, body)
	if err != nil {
		return 0, nil, err
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Idempotency-Key", key)
	// Lifecycle RPC budgets remain unchanged. Files have a separate, bounded
	// streaming budget and are never read into a giant JSON/MCP envelope.
	client := *b.manager.http
	client.Timeout = 10 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("package delivery transport unavailable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, runtimeResponseLimit+1))
	if err != nil || len(raw) > runtimeResponseLimit {
		return resp.StatusCode, nil, fmt.Errorf("package delivery response unreadable or too large")
	}
	return resp.StatusCode, raw, nil
}
func (m *appRuntimeManager) packageDeliveryState(ref runtimeBackendRef, id, state string) error {
	_, err := m.s.store.db.Exec(`INSERT INTO app_runtime_package_deliveries(backend_install_id,backend_id,package_id,state) VALUES(?,?,?,?) ON CONFLICT(backend_install_id,backend_id,package_id) DO UPDATE SET state=excluded.state`, ref.InstallID, ref.ID, id, state)
	return err
}
func (m *appRuntimeManager) deliverRuntimePackage(d runtimeDeployment, op *runtimeOperation, b *remoteRuntimeBackend) error {
	p := d.Package
	root, err := m.packageCacheRoot()
	if err != nil {
		return err
	}
	packageDir := filepath.Join(root, p.Metadata.ID)
	if err = verifyRuntimePreparedPackage(packageDir, *p); err != nil {
		return err
	}
	// Different installations and Server processes can share one package and
	// backend. Serialize upload/seal without locking audio or lifecycle traffic.
	key := runtimeHash([]byte(fmt.Sprintf("%d:%s:%s", b.registration.InstallID, b.registration.ID, p.Metadata.ID)))
	unlock, err := runtimePackageLock(m.ctx, filepath.Join(root, key+".delivery.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if err = m.phase(op, "delivering_package"); err != nil {
		return err
	}
	if err = m.checkOwner(*op); err != nil {
		return err
	}
	if err = m.packageDeliveryState(d.Placement.Backend, p.Metadata.ID, "registering"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Minute)
	defer cancel()
	metadata := []byte(runtimeJSON(p.Metadata))
	status, raw, err := b.packageRequest(ctx, "", bytes.NewReader(metadata), int64(len(metadata)), "application/json", p.Metadata.ID+":register")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("backend package registration returned HTTP %d", status)
	}
	var registered runtimePackageMetadata
	if json.Unmarshal(raw, &registered) != nil || !reflect.DeepEqual(registered, p.Metadata) {
		return fmt.Errorf("backend registered different package metadata")
	}
	seal := func() (bool, error) {
		if err := m.checkOwner(*op); err != nil {
			return false, err
		}
		status, raw, err := b.packageRequest(ctx, "/"+p.Metadata.ID+"/seal", nil, 0, "application/json", p.Metadata.ID+":seal")
		if err != nil {
			return false, err
		}
		if status == http.StatusBadRequest {
			return false, nil
		} // Missing/incomplete files: retry whole files.
		if status != http.StatusOK {
			return false, fmt.Errorf("backend package seal returned HTTP %d", status)
		}
		var reply struct {
			ID     string `json:"artifact_id"`
			Sealed bool   `json:"sealed"`
		}
		if json.Unmarshal(raw, &reply) != nil || reply.ID != p.Metadata.ID || !reply.Sealed {
			return false, fmt.Errorf("backend did not confirm the expected sealed package")
		}
		return true, nil
	}
	sealed, err := seal()
	if err != nil {
		return err
	}
	if !sealed {
		if err = m.packageDeliveryState(d.Placement.Backend, p.Metadata.ID, "uploading"); err != nil {
			return err
		}
		for _, file := range p.Metadata.Files {
			if err = m.checkOwner(*op); err != nil {
				return err
			}
			filename, err := runtimePackageContained(packageDir, file.Path)
			if err != nil {
				return err
			}
			f, err := os.Open(filename)
			if err != nil {
				return err
			}
			// Escape components, retaining the protocol's slash-separated path.
			parts := strings.Split(file.Path, "/")
			for i := range parts {
				parts[i] = url.PathEscape(parts[i])
			}
			status, raw, err := b.packageRequest(ctx, "/"+p.Metadata.ID+"/files/"+strings.Join(parts, "/"), f, file.Size, "application/octet-stream", p.Metadata.ID+":file:"+file.SHA256)
			f.Close()
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return fmt.Errorf("backend package file upload returned HTTP %d", status)
			}
			var receipt struct {
				Bytes  int64  `json:"bytes"`
				SHA256 string `json:"sha256"`
			}
			if json.Unmarshal(raw, &receipt) != nil || receipt.Bytes != file.Size || receipt.SHA256 != file.SHA256 {
				return fmt.Errorf("backend package upload receipt mismatch")
			}
		}
		sealed, err = seal()
		if err != nil {
			return err
		}
		if !sealed {
			return fmt.Errorf("backend could not verify all package files")
		}
	}
	return m.packageDeliveryState(d.Placement.Backend, p.Metadata.ID, "sealed")
}
func (m *appRuntimeManager) prepareDeploymentPackage(d *runtimeDeployment, op *runtimeOperation, backend runtimeBackend) error {
	if d.Placement.local() {
		return nil
	}
	remote, ok := backend.(*remoteRuntimeBackend)
	if !ok {
		return fmt.Errorf("unsupported remote backend client")
	}
	delivery := remote.registration.PackageDelivery
	if d.Package == nil && d.PackagePreparation == nil && d.Placement.Package == nil {
		if id, ok := d.Placement.Options["artifact_id"].(string); ok && runtimeID(id) {
			return nil // Explicit legacy/manual handoffs remain supported.
		}
	}
	if delivery == nil {
		if d.Placement.Package != nil || d.Package != nil {
			return fmt.Errorf("backend has not declared a package delivery capability")
		}
		return nil // Existing delegated preparation/image backends retain their behavior.
	}
	if err := validateRuntimePackageDelivery(delivery, backend.Describe()); err != nil {
		return err
	}
	if err := m.inspectPackageTarget(d, op, remote); err != nil {
		return err
	}
	platform := ""
	if d.Package != nil {
		platform = d.Package.Metadata.Platform
	} else if d.Placement.Package != nil {
		platform = d.Placement.Package.Platform
	}
	if !runtimeContains(backend.Describe().Platforms, platform) {
		return fmt.Errorf("select an explicit supported target package platform")
	}
	if d.Package == nil {
		p, err := m.buildRuntimePackage(d, op, delivery)
		if err != nil {
			return err
		}
		d.Package = &p
		d.AppCaps = p.Metadata.AppCapabilities
		// Persist all checksums and the artifact ID before the first network side effect.
		if err = m.checkOwner(*op); err != nil {
			return err
		}
		if err = m.saveDeployment(*d); err != nil {
			return err
		}
	}
	if err := verifyRuntimeTargetRequirements(d.TargetInfo, d.Package); err != nil {
		return err
	}
	if err := m.deliverRuntimePackage(*d, op, remote); err != nil {
		return err
	}
	return m.phase(op, "preparing")
}
func (m *appRuntimeManager) verifyPackagedAppCapabilities(d runtimeDeployment, endpoint, token string) error {
	expected := d.Package.Metadata.AppCapabilities
	return m.verifyAppLifecycleCapabilities(endpoint, token, expected)
}
func (m *appRuntimeManager) verifyAppLifecycleCapabilities(endpoint, token string, expected runtimeAppCaps) error {
	managed := expected.PassiveStart || expected.Drain || expected.ReplicaSafe || expected.WorkerCoordination || expected.Routing != ""
	if !managed {
		return nil
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeEndpoint(endpoint, runtimeControlPrefix+"app/describe"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("packaged app lifecycle verification unavailable")
	}
	defer resp.Body.Close()
	var actual runtimeAppCaps
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, runtimeResponseLimit)).Decode(&actual) != nil || actual != expected {
		return fmt.Errorf("running app lifecycle capabilities differ from sealed release metadata")
	}
	return nil
}
