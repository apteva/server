package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// dispatchAppEventToSubscribers delivers a published AppBus event to apps that
// declare requires.apps[].events for the source app. Subscriptions are derived
// from installed manifests, so there is no subscription table to maintain.
func (s *Server) dispatchAppEventToSubscribers(ev AppEvent) {
	if s == nil || s.installedApps == nil {
		return
	}
	for _, inst := range s.installedApps.List() {
		if inst == nil || inst.SidecarURL == "" {
			continue
		}
		if inst.InstallID == ev.InstallID {
			continue
		}
		if !appInstallCanReceiveEvent(inst, ev) {
			continue
		}
		if !manifestSubscribesToEvent(inst.Manifest, ev.App, ev.Topic) {
			continue
		}
		go s.deliverAppEvent(inst, ev)
	}
}

func appInstallCanReceiveEvent(inst *InstalledApp, ev AppEvent) bool {
	if inst.ProjectID != "" {
		return ev.ProjectID != "" && inst.ProjectID == ev.ProjectID
	}
	return true
}

func manifestSubscribesToEvent(m sdk.Manifest, sourceApp, eventName string) bool {
	sourceApp = strings.TrimSpace(sourceApp)
	eventName = strings.TrimSpace(eventName)
	if sourceApp == "" || eventName == "" {
		return false
	}
	for _, dep := range m.Requires.Apps {
		if dep.Name != sourceApp {
			continue
		}
		for _, declared := range dep.Events {
			if eventPatternMatches(strings.TrimSpace(declared), eventName) {
				return true
			}
		}
	}
	return false
}

func eventPatternMatches(pattern, eventName string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" || pattern == eventName {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(eventName, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

func (s *Server) deliverAppEvent(inst *InstalledApp, ev AppEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload := map[string]any{
		"event":             ev.Topic,
		"topic":             ev.Topic,
		"source_app":        ev.App,
		"source_install_id": ev.InstallID,
		"project_id":        ev.ProjectID,
		"data":              json.RawMessage(ev.Data),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[APPBUS] marshal delivery event=%s subscriber=%s install=%d: %v", ev.Topic, inst.AppName, inst.InstallID, err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(inst.SidecarURL, "/")+"/events", bytes.NewReader(body))
	if err != nil {
		log.Printf("[APPBUS] build delivery request event=%s subscriber=%s install=%d: %v", ev.Topic, inst.AppName, inst.InstallID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if inst.Token != "" {
		req.Header.Set("Authorization", "Bearer "+inst.Token)
	}
	req.Header.Set("X-Apteva-App-Install-ID", itoa(inst.InstallID))

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		log.Printf("[APPBUS] deliver event=%s source=%s subscriber=%s install=%d: %v", ev.Topic, ev.App, inst.AppName, inst.InstallID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[APPBUS] deliver event=%s source=%s subscriber=%s install=%d status=%d", ev.Topic, ev.App, inst.AppName, inst.InstallID, resp.StatusCode)
	}
}
