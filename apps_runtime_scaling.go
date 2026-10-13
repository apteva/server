package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

type runtimePool struct {
	ID        string               `json:"id"`
	ProjectID string               `json:"project_id"`
	Backend   runtimeBackendRef    `json:"backend"`
	Targets   []runtimeResourceRef `json:"targets"`
}
type runtimeScalingPolicy struct {
	InstallID       int64   `json:"install_id"`
	Min             int     `json:"min_replicas"`
	Max             int     `json:"max_replicas"`
	TargetLoad      float64 `json:"target_load"`
	CooldownSeconds int     `json:"cooldown_seconds"`
	LastScaled      int64   `json:"last_scaled"`
	Enabled         bool    `json:"enabled"`
}

func (m *appRuntimeManager) resolvePool(id int64, p *runtimePlacement) error {
	var raw, project string
	if err := m.s.store.db.QueryRow(`SELECT record_json FROM app_runtime_pools WHERE id=?`, p.PoolID).Scan(&raw); err != nil {
		return fmt.Errorf("execution pool not found")
	}
	var pool runtimePool
	if err := json.Unmarshal([]byte(raw), &pool); err != nil {
		return err
	}
	m.s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, id).Scan(&project)
	if pool.ProjectID != project || pool.Backend != p.Backend || len(pool.Targets) == 0 {
		return fmt.Errorf("pool is incompatible with this installation")
	}
	// Spread by persisted deployment count, without measuring host CPU in Server.
	best := int64(1 << 62)
	for _, target := range pool.Targets {
		var count int64
		m.s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_deployments WHERE status IN ('preparing','prepared','starting','ready','active') AND json_extract(record_json,'$.placement.target.app_install_id')=? AND json_extract(record_json,'$.placement.target.resource_type')=? AND json_extract(record_json,'$.placement.target.resource_id')=?`, target.InstallID, target.Type, target.ID).Scan(&count)
		if count < best {
			best = count
			p.Target = target
		}
	}
	return nil
}
func (m *appRuntimeManager) replicaEligible(d runtimeDeployment) error {
	if !d.AppCaps.ReplicaSafe || !d.AppCaps.WorkerCoordination || (d.AppCaps.Routing != "stateless" && d.AppCaps.Routing != "key") {
		return fmt.Errorf("app must declare replica safety, worker coordination and routing through its runtime lifecycle contract")
	}
	if d.Manifest.Runtime.ActivationStrategy == "exclusive" || d.Manifest.Runtime.DatabaseUpgrade == "requires_restore" {
		return fmt.Errorf("exclusive or restore-required apps cannot be replicated")
	}
	if d.Placement.local() {
		return fmt.Errorf("replicas require a backend with independently owned executions")
	}
	b, err := m.backend(d)
	if err != nil {
		return err
	}
	if !b.Describe().Replicas {
		return fmt.Errorf("backend does not support replicas")
	}
	return nil
}
func (m *appRuntimeManager) scale(id int64, want int) error {
	if want < 1 || want > 32 {
		return fmt.Errorf("replicas must be between 1 and 32")
	}
	active, err := m.active(id)
	if err != nil {
		return err
	}
	if err = m.replicaEligible(active); err != nil {
		return err
	}
	_, err = m.begin(id, "scale", fmt.Sprintf("scale-%d-%s", want, generateToken(12)), active.Placement, nil, want)
	return err
}
func (m *appRuntimeManager) scaleOperation(op *runtimeOperation) error {
	var want int
	if err := m.s.store.db.QueryRow(`SELECT desired_replicas FROM app_runtime_operations WHERE id=?`, op.ID).Scan(&want); err != nil {
		return err
	}
	if want < 1 || want > 32 {
		return fmt.Errorf("invalid replica request")
	}
	active, err := m.active(op.InstallID)
	if err != nil {
		return err
	}
	if err = m.replicaEligible(active); err != nil {
		return err
	}
	all, err := m.deployments(op.InstallID)
	if err != nil {
		return err
	}
	var live []runtimeDeployment
	for _, d := range all {
		if d.Generation == active.Generation && (d.Status == "active") {
			live = append(live, d)
		}
	}
	for len(live) > want {
		d := live[len(live)-1]
		if d.ID == active.ID {
			return fmt.Errorf("primary replica cannot be removed")
		}
		if err = m.drain(d, *op); err != nil {
			return err
		}
		if err = m.stopDeployment(&d, *op); err != nil {
			return err
		}
		m.s.store.db.Exec(`DELETE FROM app_runtime_ports WHERE deployment_id=?`, d.ID)
		live = live[:len(live)-1]
	}
	for len(live) < want {
		d, err := m.makeDeployment(op.InstallID, active.Placement, &active.Manifest, len(live))
		if err == nil {
			for _, existing := range all {
				if existing.Generation == active.Generation && existing.Replica == len(live) {
					d = existing
					break
				}
			}
		}
		if err != nil {
			return err
		}
		d.Generation = active.Generation
		raw, e := Decrypt(m.s.secret, d.EnvironmentEncrypted)
		if e != nil {
			return e
		}
		var env map[string]string
		if e = json.Unmarshal([]byte(raw), &env); e != nil {
			return e
		}
		env["APTEVA_DEPLOYMENT_GENERATION"] = fmt.Sprint(d.Generation)
		if d.Status == "stopped" || d.Status == "deleted" {
			token := rand.Text()
			d.CredentialEncrypted, e = Encrypt(m.s.secret, token)
			if e != nil {
				return e
			}
			env["APTEVA_APP_TOKEN"] = token
			env["APTEVA_OUTBOUND_TOKEN"] = token
		}
		d.EnvironmentEncrypted, e = Encrypt(m.s.secret, runtimeJSON(env))
		if e != nil {
			return e
		}
		d.AppCaps = active.AppCaps
		if d.Placement.PoolID != "" {
			if err = m.resolvePool(op.InstallID, &d.Placement); err != nil {
				return err
			}
		}
		// Persistent replicated state is provided by the app/backend; never clone a
		// SQLite file into independently writable replicas.
		if active.Manifest.DB != nil && (active.Manifest.DB.Driver == "sqlite" || active.Manifest.DB.Driver == "") {
			return fmt.Errorf("replicated SQLite is unsupported; app must externalize shared state")
		}
		if err = m.saveDeployment(d); err != nil {
			return err
		}
		op.DeploymentID = d.ID
		if err = m.saveOperation(*op); err != nil {
			return err
		}
		if d.Status != "prepared" && d.Status != "starting" && d.Status != "ready" {
			if err = m.prepare(op); err != nil {
				return err
			}
		}
		d, err = m.loadDeployment(d.ID)
		if err != nil {
			return err
		}
		if d.Status == "ready" {
			err = m.waitReady(&d, *op)
		} else {
			err = m.startDeployment(&d, *op)
		}
		if err != nil {
			_ = m.stopDeployment(&d, *op)
			return err
		}
		if err = m.appLifecycle(d, "activate"); err != nil {
			return err
		}
		d.Status = "active"
		if err = m.saveDeployment(d); err != nil {
			return err
		}
		live = append(live, d)
	}
	if m.s.installedApps != nil {
		m.s.LoadInstalledApps()
	}
	return nil
}
func (m *appRuntimeManager) autoscale() {
	rows, err := m.s.store.db.Query(`SELECT record_json FROM app_runtime_scaling`)
	if err != nil {
		return
	}
	var policies []runtimeScalingPolicy
	for rows.Next() {
		var raw string
		var p runtimeScalingPolicy
		if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &p) == nil {
			policies = append(policies, p)
		}
	}
	rows.Close()
	for _, p := range policies {
		if !p.Enabled || time.Now().Unix()-p.LastScaled < int64(p.CooldownSeconds) {
			continue
		}
		active, err := m.active(p.InstallID)
		if err != nil || m.replicaEligible(active) != nil {
			continue
		}
		ds, _ := m.deployments(p.InstallID)
		var total float64
		count := 0
		valid := true
		for _, d := range ds {
			if d.Status != "active" || d.Generation != active.Generation {
				continue
			}
			b, e := m.backend(d)
			if e != nil {
				valid = false
				break
			}
			req, _ := m.request(d, runtimeOperation{ID: "autoscale"}, "inspect")
			state, e := b.Inspect(req)
			if e != nil || state.Load == nil || *state.Load < 0 {
				valid = false
				break
			}
			count++
			total += *state.Load
		}
		if !valid || count == 0 {
			continue
		}
		wanted := count
		mean := total / float64(count)
		if mean > p.TargetLoad*1.2 && count < p.Max {
			wanted++
		} else if mean < p.TargetLoad*0.5 && count > p.Min {
			wanted--
		}
		if wanted == count {
			continue
		}
		if err = m.scale(p.InstallID, wanted); err == nil {
			p.LastScaled = time.Now().Unix()
			m.s.store.db.Exec(`UPDATE app_runtime_scaling SET record_json=? WHERE install_id=?`, runtimeJSON(p), p.InstallID)
		}
	}
}
