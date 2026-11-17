package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
)

type appRestartAudit struct {
	Reason          string   `json:"reason"`
	UserID          int64    `json:"initiating_user_id,omitempty"`
	AppInstallID    int64    `json:"initiating_app_install_id,omitempty"`
	RequestID       string   `json:"request_id,omitempty"`
	ChangedSettings []string `json:"changed_settings,omitempty"`
}
type appRestartAuditRecord struct {
	appRestartAudit
	InstallID     int64  `json:"install_id"`
	OldPID        int64  `json:"old_pid"`
	NewPID        int64  `json:"new_pid"`
	OldGeneration int64  `json:"old_generation"`
	NewGeneration int64  `json:"new_generation"`
	Status        string `json:"status"`
	ErrorType     string `json:"error_type,omitempty"`
}

var restartCorrelationPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func restartAuditForRequest(r *http.Request, reason string, keys []string) appRestartAudit {
	a := appRestartAudit{Reason: reason, UserID: getUserID(r), ChangedSettings: append([]string(nil), keys...)}
	if principal, ok := r.Context().Value(appCallbackPrincipalKey{}).(appCallbackPrincipal); ok {
		a.AppInstallID = principal.installID
		if a.UserID == 0 {
			a.UserID = principal.userID
		}
	}
	a.RequestID = r.Header.Get("X-Request-ID")
	if !restartCorrelationPattern.MatchString(a.RequestID) {
		a.RequestID = generateID()
	}
	sort.Strings(a.ChangedSettings)
	return a
}
func changedInstallSettingNames(before, after map[string]any) []string {
	names := []string{}
	for key, value := range after {
		old, exists := before[key]
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(value)
		if !exists || string(a) != string(b) {
			names = append(names, key)
		}
	}
	sort.Strings(names)
	return names
}
func (s *Server) restartProcessIdentity(id int64) (int64, int64) {
	var pid, generation int64
	if s.store != nil {
		_ = s.store.db.QueryRow(`SELECT local_pid,runtime_generation FROM app_installs WHERE id=?`, id).Scan(&pid, &generation)
	}
	if s.localApps != nil {
		if live := s.localApps.PID(id); live > 0 {
			pid = int64(live)
		}
	}
	return pid, generation
}
func (s *Server) logAppRestart(id int64, a appRestartAudit, oldPID, oldGeneration int64, err error) {
	pid, generation := s.restartProcessIdentity(id)
	record := appRestartAuditRecord{appRestartAudit: a, InstallID: id, OldPID: oldPID, NewPID: pid, OldGeneration: oldGeneration, NewGeneration: generation, Status: "completed"}
	if err == nil && s.runtimeManaged(id) {
		record.Status = "requested"
	}
	if err != nil {
		record.Status = "failed"
		record.ErrorType = fmt.Sprintf("%T", err)
	}
	raw, _ := json.Marshal(record)
	log.Printf("[APP-RESTART] %s", raw)
}
