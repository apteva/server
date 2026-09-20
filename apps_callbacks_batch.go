package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const maxAppCallBatchSize = 32
const maxAppCallConcurrency = 8
const maxAppCallResponseBytes = 16 << 20
const maxAppBatchResponseBytes = 32 << 20

func unwrapAppCallResult(body []byte) []byte {
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Error) > 0 || len(envelope.Result) == 0 {
		return body
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(envelope.Result, &result) == nil && !result.IsError && len(result.Content) == 1 && result.Content[0].Type == "text" && json.Valid([]byte(result.Content[0].Text)) {
		return []byte(result.Content[0].Text)
	}
	return body
}

type appCallTarget struct {
	callerInstallID  int64
	targetAppName    string
	targetInstallID  int64
	effectiveProject string
	callerAppName    string
	target           *InstalledApp
}

// appCallbackClient returns one shared client. The admission metadata for a
// particular child call is carried in its request context, allowing the
// underlying keep-alive transport to be reused safely.
func (s *Server) appCallbackClient() *http.Client {
	s.appCallbackClientOnce.Do(func() {
		s.appCallbackHTTPClient = &http.Client{Transport: &automaticTransport{
			server: s,
			base:   http.DefaultTransport,
		}}
	})
	return s.appCallbackHTTPClient
}

// authorizeAppCallback performs the expensive, shared portion of a sibling
// app call. Batch requests call this once and reuse the resulting target for
// every child operation.
func (s *Server) authorizeAppCallback(w http.ResponseWriter, installID int64, targetAppName, requestedProjectID string) (*appCallTarget, bool) {
	caller, err := s.appMetadata(installID)
	if err != nil || !caller.permissions[sdk.PermAppsCall] {
		http.Error(w, "missing permission: "+string(sdk.PermAppsCall), http.StatusForbidden)
		return nil, false
	}
	effectiveProjectID, ok := s.appCallProjectMetadata(w, caller, requestedProjectID)
	if !ok {
		return nil, false
	}
	targetInstallID := s.installBoundAppMetadata(caller, targetAppName)
	if targetInstallID == 0 {
		id, msg, resolved := s.resolveDynamicTarget(installID, targetAppName, effectiveProjectID)
		if !resolved {
			http.Error(w, msg, http.StatusForbidden)
			return nil, false
		}
		targetInstallID = id
	}
	target := s.installedApps.Get(targetInstallID)
	if target == nil || target.SidecarURL == "" {
		http.Error(w, "target app not reachable: "+targetAppName, http.StatusBadGateway)
		return nil, false
	}
	callerAppName := strings.TrimSpace(caller.name)
	if callerAppName == "" {
		http.Error(w, "calling app is not running", http.StatusUnauthorized)
		return nil, false
	}
	if target.ProjectID != "" {
		if effectiveProjectID == "" {
			effectiveProjectID, ok = s.appCallProjectMetadata(w, caller, target.ProjectID)
			if !ok {
				return nil, false
			}
		} else if effectiveProjectID != target.ProjectID {
			http.Error(w, "project_id does not match target app install", http.StatusForbidden)
			return nil, false
		}
	}
	return &appCallTarget{
		callerInstallID:  installID,
		targetAppName:    targetAppName,
		targetInstallID:  targetInstallID,
		effectiveProject: effectiveProjectID,
		callerAppName:    callerAppName,
		target:           target,
	}, true
}

type appCallDispatchResult struct {
	status      int
	contentType string
	wait        string
	body        []byte
	format      string
}

type appBatchCapabilityKey struct {
	installID int64
	endpoint  string
	token     string
}

type appBatchCapability struct {
	supported bool
	checkedAt time.Time
}

const appBatchNegativeCapabilityTTL = 30 * time.Second

func (s *Server) appBatchCapabilityKey(target *appCallTarget) (appBatchCapabilityKey, bool) {
	runtime := s.installedApps.Get(target.targetInstallID)
	if runtime == nil || runtime.SidecarURL == "" {
		return appBatchCapabilityKey{}, false
	}
	return appBatchCapabilityKey{installID: target.targetInstallID, endpoint: runtime.SidecarURL, token: runtime.Token}, true
}

func (s *Server) cachedInternalAppBatchCapability(target *appCallTarget) (bool, bool) {
	key, ok := s.appBatchCapabilityKey(target)
	if !ok {
		return false, true
	}
	value, ok := s.appBatchCapabilities.Load(key)
	if !ok {
		return false, false
	}
	capability := value.(appBatchCapability)
	if !capability.supported && time.Since(capability.checkedAt) >= appBatchNegativeCapabilityTTL {
		return false, false
	}
	return capability.supported, true
}

func (s *Server) rememberInternalAppBatch(target *appCallTarget, supported bool) {
	if key, ok := s.appBatchCapabilityKey(target); ok {
		s.appBatchCapabilities.Store(key, appBatchCapability{supported: supported, checkedAt: time.Now()})
	}
}

// supportsInternalAppBatch performs a side-effect-free HEAD negotiation before
// the first optimized POST. This matters for old apps that mount a catch-all
// HTTP route: probing them with the batch payload could return 200 after doing
// arbitrary work, at which point a legacy retry would be unsafe.
func (s *Server) supportsInternalAppBatch(ctx context.Context, target *appCallTarget) (bool, error) {
	if supported, cached := s.cachedInternalAppBatchCapability(target); cached {
		return supported, nil
	}
	runtime := s.installedApps.Get(target.targetInstallID)
	if runtime == nil || runtime.SidecarURL == "" {
		return false, errors.New("target app not reachable")
	}
	requestContext := withAutomaticAdmissionMetadata(ctx, automaticAdmissionMetadata{
		target:     fmt.Sprintf("app:%d", target.targetInstallID),
		operation:  "batch-capability",
		caller:     fmt.Sprintf("app:%d", target.callerInstallID),
		source:     fmt.Sprintf("app:%d", target.callerInstallID),
		background: target.callerAppName == "jobs",
	})
	req, err := http.NewRequestWithContext(requestContext, http.MethodHead, strings.TrimRight(runtime.SidecarURL, "/")+sdk.InternalAppBatchPath, nil)
	if err != nil {
		return false, err
	}
	if runtime.Token != "" {
		req.Header.Set("Authorization", "Bearer "+runtime.Token)
	}
	req.Header.Set(sdk.HeaderBoundCallerInstallID, strconv.FormatInt(target.callerInstallID, 10))
	req.Header.Set(sdk.HeaderBoundCallerAppName, target.callerAppName)
	if deadline, ok := requestContext.Deadline(); ok {
		req.Header.Set(sdk.HeaderAppCallDeadline, strconv.FormatInt(deadline.UnixMilli(), 10))
	}
	resp, err := s.appCallbackClient().Do(req)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	supported := resp.Header.Get(sdk.HeaderInternalAppBatchVersion) == sdk.InternalAppBatchVersion
	s.rememberInternalAppBatch(target, supported)
	return supported, nil
}

func appCallProjectArgRaw(input json.RawMessage) (string, error) {
	fields, err := appCallInputFields(input)
	if err != nil {
		return "", err
	}
	raw, present := fields["_project_id"]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var projectID string
	if err := json.Unmarshal(raw, &projectID); err != nil {
		return "", errors.New("_project_id must be a string")
	}
	return strings.TrimSpace(projectID), nil
}

func appCallInputFields(input json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil || fields == nil {
		return nil, errors.New("input must be a JSON object")
	}
	return fields, nil
}

func pinAppCallProjectRaw(input json.RawMessage, projectID string) (json.RawMessage, error) {
	fields, err := appCallInputFields(input)
	if err != nil {
		return nil, err
	}
	delete(fields, "_project_id")
	if projectID != "" {
		encoded, _ := json.Marshal(projectID)
		fields["_project_id"] = encoded
	}
	return json.Marshal(fields)
}

func decodeAppCallInputRaw(input json.RawMessage) (map[string]any, error) {
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil || decoded == nil {
		return nil, errors.New("input must be a JSON object")
	}
	return decoded, nil
}

func (s *Server) dispatchInternalAppBatch(ctx context.Context, target *appCallTarget, body appCallBatchRequest) (appCallBatchResponse, string, bool, error) {
	runtime := s.installedApps.Get(target.targetInstallID)
	if runtime == nil || runtime.SidecarURL == "" {
		return appCallBatchResponse{}, "", false, errors.New("target app not reachable")
	}
	payload, err := json.Marshal(sdk.InternalAppBatchRequest{Calls: body.Calls, AppBatchOptions: body.AppBatchOptions})
	if err != nil {
		return appCallBatchResponse{}, "", false, err
	}
	requestContext := withAutomaticAdmissionMetadata(ctx, automaticAdmissionMetadata{
		target:     fmt.Sprintf("app:%d", target.targetInstallID),
		operation:  fmt.Sprintf("batch:%d", len(body.Calls)),
		caller:     fmt.Sprintf("app:%d", target.callerInstallID),
		source:     fmt.Sprintf("app:%d", target.callerInstallID),
		background: target.callerAppName == "jobs",
	})
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, strings.TrimRight(runtime.SidecarURL, "/")+sdk.InternalAppBatchPath, bytes.NewReader(payload))
	if err != nil {
		return appCallBatchResponse{}, "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if runtime.Token != "" {
		req.Header.Set("Authorization", "Bearer "+runtime.Token)
	}
	req.Header.Set(sdk.HeaderBoundCallerInstallID, strconv.FormatInt(target.callerInstallID, 10))
	req.Header.Set(sdk.HeaderBoundCallerAppName, target.callerAppName)
	setAppCallbackPrincipalHeaders(s, req, requestContext, target.effectiveProject)
	if deadline, ok := requestContext.Deadline(); ok {
		req.Header.Set(sdk.HeaderAppCallDeadline, strconv.FormatInt(deadline.UnixMilli(), 10))
	}
	resp, err := s.appCallbackClient().Do(req)
	if err != nil {
		return appCallBatchResponse{}, "", false, err
	}
	defer resp.Body.Close()
	reader := io.LimitReader(resp.Body, maxAppBatchResponseBytes+1)
	responseBody, err := io.ReadAll(reader)
	if err != nil {
		return appCallBatchResponse{}, "", false, err
	}
	if len(responseBody) > maxAppBatchResponseBytes {
		return appCallBatchResponse{}, "", false, errors.New("batch response too large")
	}
	version := resp.Header.Get(sdk.HeaderInternalAppBatchVersion)
	if version == "" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented) {
		s.rememberInternalAppBatch(target, false)
		return appCallBatchResponse{}, "", true, nil
	}
	if version != sdk.InternalAppBatchVersion {
		return appCallBatchResponse{}, "", false, errors.New("target returned an unknown internal batch protocol")
	}
	s.rememberInternalAppBatch(target, true)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return appCallBatchResponse{}, "", false, fmt.Errorf("target returned HTTP %d: %.200s", resp.StatusCode, responseBody)
	}
	var internal sdk.InternalAppBatchResponse
	if err := json.Unmarshal(responseBody, &internal); err != nil {
		return appCallBatchResponse{}, "", false, fmt.Errorf("decode target batch: %w", err)
	}
	if len(internal.Results) != len(body.Calls) {
		return appCallBatchResponse{}, "", false, errors.New("target batch returned the wrong result count")
	}
	for i := range internal.Results {
		result := &internal.Results[i]
		if result.ID != body.Calls[i].ID {
			return appCallBatchResponse{}, "", false, errors.New("target batch reordered or replaced result IDs")
		}
		if result.Error != nil {
			result.Result = nil
			result.Format = ""
			continue
		}
		if result.Format != "json" || !json.Valid(result.Result) {
			return appCallBatchResponse{}, "", false, errors.New("target batch returned an invalid structured result")
		}
		if body.ResultMode != "json" {
			wrapped, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]any{"content": []map[string]any{{"type": "text", "text": string(result.Result)}}},
			})
			if err != nil {
				return appCallBatchResponse{}, "", false, err
			}
			result.Result = wrapped
			result.Format = ""
		}
	}
	timings := make([]string, 0, 3)
	if wait, err := strconv.ParseFloat(resp.Header.Get("X-Apteva-Admission-Wait-Ms"), 64); err == nil && wait >= 0 && wait < 3.6e6 {
		timings = append(timings, fmt.Sprintf("apteva_app_queue;dur=%.1f", wait))
	}
	if targetTiming := safeTargetServerTiming(resp.Header.Get("Server-Timing")); targetTiming != "" {
		timings = append(timings, targetTiming)
	}
	return appCallBatchResponse{Results: internal.Results}, strings.Join(timings, ", "), false, nil
}

func safeTargetServerTiming(raw string) string {
	allowed := map[string]bool{"apteva_target_handler": true, "apteva_target_encode": true}
	out := make([]string, 0, 2)
	for _, item := range strings.Split(raw, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		if len(parts) != 2 || !allowed[parts[0]] || !strings.HasPrefix(parts[1], "dur=") {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimPrefix(parts[1], "dur="), 64)
		if err == nil && value >= 0 && value < 3.6e6 {
			out = append(out, fmt.Sprintf("%s;dur=%.1f", parts[0], value))
		}
	}
	return strings.Join(out, ", ")
}

func setAppCallbackPrincipalHeaders(s *Server, req *http.Request, ctx context.Context, projectID string) {
	principal, ok := ctx.Value(appCallbackPrincipalKey{}).(appCallbackPrincipal)
	if !ok || !principal.userSession || principal.userID <= 0 {
		return
	}
	trusted := &sdk.TrustedPrincipal{
		Version:     sdk.TrustedPrincipalVersion,
		UserID:      principal.userID,
		SubjectType: "user",
		SubjectID:   strconv.FormatInt(principal.userID, 10),
		ProjectID:   projectID,
		ExpiresAt:   time.Now().Add(time.Minute).Unix(),
	}
	if user, err := s.store.GetUserByID(principal.userID); err == nil && user != nil {
		trusted.Email = user.Email
		trusted.SubjectEmail = user.Email
	}
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	setTrustedAppPrincipalHeaders(req, token, trusted)
}

func (s *Server) dispatchAppCallback(ctx context.Context, target *appCallTarget, tool string, input map[string]any, direct bool, budget *atomic.Int64) (appCallDispatchResult, error) {
	if err := ctx.Err(); err != nil {
		return appCallDispatchResult{}, err
	}
	// Endpoint and token are runtime state, never authorization-cache entries.
	runtime := s.installedApps.Get(target.targetInstallID)
	if runtime == nil || runtime.SidecarURL == "" {
		return appCallDispatchResult{}, errors.New("target app not reachable")
	}
	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      tool,
			"arguments": input,
		},
	}
	rpcBody, err := json.Marshal(rpc)
	if err != nil {
		return appCallDispatchResult{}, err
	}
	requestContext := withAutomaticAdmissionMetadata(ctx, automaticAdmissionMetadata{
		target:     fmt.Sprintf("app:%d", target.targetInstallID),
		operation:  admissionCallbackIdentity(tool, input),
		caller:     fmt.Sprintf("app:%d", target.callerInstallID),
		source:     fmt.Sprintf("app:%d", target.callerInstallID),
		background: target.callerAppName == "jobs",
	})
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, runtime.SidecarURL+"/mcp", strings.NewReader(string(rpcBody)))
	if err != nil {
		return appCallDispatchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if runtime.Token != "" {
		req.Header.Set("Authorization", "Bearer "+runtime.Token)
	}
	if direct {
		req.Header.Set(sdk.HeaderAppResultFormat, sdk.AppResultJSON)
	}
	req.Header.Set(sdk.HeaderBoundCallerInstallID, strconv.FormatInt(target.callerInstallID, 10))
	req.Header.Set(sdk.HeaderBoundCallerAppName, target.callerAppName)
	setAppCallbackPrincipalHeaders(s, req, requestContext, target.effectiveProject)
	if deadline, ok := requestContext.Deadline(); ok {
		req.Header.Set(sdk.HeaderAppCallDeadline, strconv.FormatInt(deadline.UnixMilli(), 10))
	}
	resp, err := s.appCallbackClient().Do(req)
	if err != nil {
		return appCallDispatchResult{}, err
	}
	defer resp.Body.Close()
	reader := io.Reader(resp.Body)
	if budget != nil {
		reader = &appBatchBudgetReader{reader: reader, used: budget}
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxAppCallResponseBytes+1))
	if err != nil {
		return appCallDispatchResult{}, err
	}
	if len(body) > maxAppCallResponseBytes {
		return appCallDispatchResult{}, errors.New("app response too large")
	}
	return appCallDispatchResult{
		status:      resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		wait:        resp.Header.Get("X-Apteva-Admission-Wait-Ms"),
		body:        body,
		format:      resp.Header.Get(sdk.HeaderAppResultFormat),
	}, nil
}

type appCallBatchRequest struct {
	Calls []sdk.InternalAppCall `json:"calls"`
	sdk.AppBatchOptions
}

var errAppBatchTooLarge = errors.New("batch response too large")

type appBatchBudgetReader struct {
	reader io.Reader
	used   *atomic.Int64
}

func (r *appBatchBudgetReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if r.used.Add(int64(n)) > maxAppBatchResponseBytes {
		return 0, errAppBatchTooLarge
	}
	return n, err
}

type appCallBatchResponse struct {
	Results []sdk.AppCallResult `json:"results"`
}

func (s *Server) handleCallbackAppBatch(w http.ResponseWriter, r *http.Request, targetAppName string) {
	started := time.Now()
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var body appCallBatchRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "batch must contain exactly one JSON object within 8 MiB", http.StatusBadRequest)
		return
	}
	workers := 1
	switch body.Execution {
	case "", "sequential":
		if body.Concurrency != 0 && body.Concurrency != 1 {
			http.Error(w, "concurrency requires parallel_independent", 400)
			return
		}
	case sdk.ParallelIndependent:
		workers = body.Concurrency
		if workers == 0 {
			workers = 4
		}
		if workers < 1 || workers > maxAppCallConcurrency {
			http.Error(w, "concurrency must be between 1 and 8", 400)
			return
		}
	default:
		http.Error(w, "unknown batch execution mode", 400)
		return
	}
	if body.ResultMode != "" && body.ResultMode != "json" {
		http.Error(w, "unknown batch result mode", 400)
		return
	}
	if len(body.Calls) == 0 || len(body.Calls) > maxAppCallBatchSize {
		http.Error(w, fmt.Sprintf("calls must contain 1-%d items", maxAppCallBatchSize), http.StatusBadRequest)
		return
	}
	for i := range body.Calls {
		if strings.TrimSpace(body.Calls[i].Tool) == "" {
			http.Error(w, fmt.Sprintf("calls[%d].tool required", i), http.StatusBadRequest)
			return
		}
		if body.Calls[i].ID == "" {
			body.Calls[i].ID = strconv.Itoa(i + 1)
		}
		if len(body.Calls[i].Input) == 0 {
			body.Calls[i].Input = json.RawMessage(`{}`)
		}
		if !json.Valid(body.Calls[i].Input) {
			http.Error(w, fmt.Sprintf("calls[%d].input must be valid JSON", i), http.StatusBadRequest)
			return
		}
	}
	requestedProjectID, err := appCallProjectArgRaw(body.Calls[0].Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, ok := s.authorizeAppCallback(w, installID, targetAppName, requestedProjectID)
	if !ok {
		return
	}
	authDone := time.Now()
	for i := range body.Calls {
		projectID, err := appCallProjectArgRaw(body.Calls[i].Input)
		if err != nil || projectID != requestedProjectID {
			http.Error(w, "all batch calls must use the same project_id", http.StatusBadRequest)
			return
		}
	}
	if isPlatformBackupApp(target.target) {
		http.Error(w, "backup management is not available through batch calls", http.StatusForbidden)
		return
	}
	for i := range body.Calls {
		input, err := pinAppCallProjectRaw(body.Calls[i].Input, target.effectiveProject)
		if err != nil {
			http.Error(w, fmt.Sprintf("calls[%d].input: %v", i, err), http.StatusBadRequest)
			return
		}
		body.Calls[i].Input = input
	}

	// New SDK targets execute the entire array through one authenticated
	// request. Only an explicit unsupported response falls through to the
	// legacy per-child /mcp path; ambiguous failures are never retried because
	// a tool may already have produced side effects.
	fastSupported, capabilityErr := s.supportsInternalAppBatch(r.Context(), target)
	if capabilityErr != nil {
		if isAdmissionFailure(capabilityErr) {
			writeAdmissionError(w, capabilityErr)
			return
		}
		if errors.Is(capabilityErr, context.Canceled) {
			return
		}
		if errors.Is(capabilityErr, context.DeadlineExceeded) {
			http.Error(w, "caller deadline exceeded", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "target batch capability failed: "+capabilityErr.Error(), http.StatusBadGateway)
		return
	}
	if fastSupported {
		fastResponse, targetTiming, unavailable, fastErr := s.dispatchInternalAppBatch(r.Context(), target, body)
		if !unavailable {
			if fastErr != nil {
				if isAdmissionFailure(fastErr) {
					writeAdmissionError(w, fastErr)
					return
				}
				if errors.Is(fastErr, context.Canceled) {
					return
				}
				if errors.Is(fastErr, context.DeadlineExceeded) {
					http.Error(w, "caller deadline exceeded", http.StatusGatewayTimeout)
					return
				}
				http.Error(w, "target batch failed: "+fastErr.Error(), http.StatusBadGateway)
				return
			}
			encoded, err := json.Marshal(fastResponse)
			if err != nil || len(encoded) > maxAppBatchResponseBytes {
				http.Error(w, "batch response too large or invalid", http.StatusBadGateway)
				return
			}
			timing := fmt.Sprintf("apteva_app_auth;dur=%.1f, apteva_app_batch_dispatch;dur=%.1f", authDone.Sub(started).Seconds()*1000, time.Since(authDone).Seconds()*1000)
			if targetTiming != "" {
				timing += ", " + targetTiming
			}
			w.Header().Set("Server-Timing", timing)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(encoded)
			return
		}
	}

	response := appCallBatchResponse{Results: make([]sdk.AppCallResult, len(body.Calls))}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var totalBytes atomic.Int64
	var tooLarge atomic.Bool
	var next atomic.Int64
	var wg sync.WaitGroup
	for i := range response.Results {
		response.Results[i] = sdk.AppCallResult{ID: body.Calls[i].ID, Status: 499, Error: &sdk.AppCallError{Message: "batch canceled before dispatch"}}
	}
	run := func() {
		defer wg.Done()
		for {
			if ctx.Err() != nil {
				return
			}
			index := int(next.Add(1) - 1)
			if index >= len(body.Calls) || ctx.Err() != nil {
				return
			}
			call := body.Calls[index]
			input, decodeErr := decodeAppCallInputRaw(call.Input)
			result := sdk.AppCallResult{ID: call.ID}
			if decodeErr != nil {
				result.Status = http.StatusBadRequest
				result.Error = &sdk.AppCallError{Code: -32602, Message: decodeErr.Error()}
				response.Results[index] = result
				continue
			}
			dispatched, dispatchErr := s.dispatchAppCallback(ctx, target, call.Tool, input, body.ResultMode == "json", &totalBytes)
			if dispatchErr != nil {
				if errors.Is(dispatchErr, errAppBatchTooLarge) {
					tooLarge.Store(true)
					cancel()
				}
				result.Status = http.StatusBadGateway
				if isAdmissionFailure(dispatchErr) {
					result.Status = http.StatusTooManyRequests
				}
				if errors.Is(dispatchErr, context.Canceled) {
					result.Status = 499
				}
				if errors.Is(dispatchErr, context.DeadlineExceeded) {
					result.Status = http.StatusGatewayTimeout
				}
				result.Error = &sdk.AppCallError{Message: dispatchErr.Error()}
			} else {
				result.Status = dispatched.status
				if dispatched.status >= 200 && dispatched.status < 300 {
					if !json.Valid(dispatched.body) {
						result.Error = &sdk.AppCallError{Message: "target returned invalid JSON"}
					} else {
						result.Result = json.RawMessage(dispatched.body)
						if dispatched.format == sdk.AppResultJSON {
							result.Format = "json"
						} else {
							result.Error = appCallProtocolError(dispatched.body)
						}
					}
				} else {
					result.Error = &sdk.AppCallError{Message: fmt.Sprintf("target returned HTTP %d", dispatched.status)}
				}
			}
			response.Results[index] = result
		}
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go run()
	}
	wg.Wait()
	if tooLarge.Load() {
		http.Error(w, errAppBatchTooLarge.Error(), 502)
		return
	}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > maxAppBatchResponseBytes {
		http.Error(w, "batch response too large or invalid", 502)
		return
	}
	w.Header().Set("Server-Timing", fmt.Sprintf("apteva_app_auth;dur=%.1f, apteva_app_batch_dispatch;dur=%.1f", authDone.Sub(started).Seconds()*1000, time.Since(authDone).Seconds()*1000))
	debugLogf("[APPS-CALL-BATCH] caller_install=%d target=%s calls=%d auth_ms=%.1f dispatch_ms=%.1f total_ms=%.1f", installID, targetAppName, len(body.Calls), authDone.Sub(started).Seconds()*1000, time.Since(authDone).Seconds()*1000, time.Since(started).Seconds()*1000)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

func appCallProtocolError(body []byte) *sdk.AppCallError {
	var envelope struct {
		Error  *sdk.AppCallError `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return nil
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if envelope.Result.IsError {
		message := "tool returned error"
		if len(envelope.Result.Content) > 0 {
			message = envelope.Result.Content[0].Text
		}
		return &sdk.AppCallError{Message: message}
	}
	return nil
}
