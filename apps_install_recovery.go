package main

import (
	"encoding/json"
	"log"
	"time"
)

const mcpInstallQueued = "Queued MCP installation"

// The install row, receipt and placement intent are committed together.
// Recover the narrow crash window before begin() created its durable operation.
func (m *appRuntimeManager) resumeMCPInstallIntents() {
	rows, err := m.s.store.dbQueryContext(m.ctx, `SELECT i.id,i.runtime_placement_json,r.request_key FROM app_installs i JOIN app_runtime_install_receipts r ON r.install_id=i.id WHERE i.runtime_managed=1 AND i.status='pending' AND i.status_message=? AND NOT EXISTS(SELECT 1 FROM app_runtime_operations o WHERE o.install_id=i.id)`, mcpInstallQueued)
	if err != nil {
		return
	}
	type intent struct {
		id             int64
		placement, key string
	}
	intents := []intent{}
	for rows.Next() {
		var item intent
		if err = rows.Scan(&item.id, &item.placement, &item.key); err != nil {
			break
		}
		intents = append(intents, item)
	}
	if e := rows.Err(); e != nil {
		err = e
	}
	rows.Close()
	if err != nil {
		return
	}
	for _, item := range intents {
		for !runtimeConsoleInstallMu.TryLock() {
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
		var p runtimePlacement
		if json.Unmarshal([]byte(item.placement), &p) == nil {
			manifest, e := installManifest(m.s, item.id)
			if e == nil {
				_, e = m.begin(item.id, "deploy", runtimeConsoleInstallOperationKey(item.key), p, manifest)
			}
			if e != nil {
				log.Printf("[APPS-MCP] resume install=%d: %v", item.id, e)
			}
		}
		runtimeConsoleInstallMu.Unlock()
	}
}
