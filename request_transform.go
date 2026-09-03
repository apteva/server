package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

func buildRequestTransformBody(transform *RequestTransformDef, input map[string]any) (any, bool, error) {
	if transform == nil {
		return nil, false, nil
	}

	switch transform.Type {
	case "mime_email":
		mime, err := buildMIMEEmail(input)
		if err != nil {
			return nil, true, err
		}
		body := map[string]any{}
		target := transform.Target
		if target == "" {
			target = "raw"
		}
		encoding := transform.Encoding
		if encoding == "" {
			encoding = "base64url"
		}
		setBodyPath(body, target, encodeRequestTransformString(mime, encoding))
		copyRequestTransformFields(body, input, transform.IncludeFields)
		return body, true, nil
	case "base64_field":
		source, ok := input[transform.Source]
		if !ok || source == nil {
			return nil, true, fmt.Errorf("request_transform source missing: %s", transform.Source)
		}
		body := map[string]any{}
		encoding := transform.Encoding
		if encoding == "" {
			encoding = "base64"
		}
		setBodyPath(body, transform.Target, encodeRequestTransformString(fmt.Sprintf("%v", source), encoding))
		copyRequestTransformFields(body, input, transform.IncludeFields)
		return body, true, nil
	case "json_wrap":
		selected := map[string]any{}
		for field, value := range transform.Constants {
			selected[field] = value
		}
		for _, field := range transform.Fields {
			if v, ok := input[field]; ok && v != nil {
				selected[field] = v
			}
		}
		if transform.Target == "" && transform.AsArray {
			return []any{selected}, true, nil
		}
		body := map[string]any{}
		if transform.Target != "" {
			if transform.AsArray {
				setBodyPath(body, transform.Target, []any{selected})
			} else {
				setBodyPath(body, transform.Target, selected)
			}
		} else {
			for k, v := range selected {
				body[k] = v
			}
		}
		copyRequestTransformFields(body, input, transform.IncludeFields)
		return body, true, nil
	case "json_api":
		if transform.ResourceType == "" {
			return nil, true, fmt.Errorf("json_api request_transform requires resource_type")
		}
		data := map[string]any{"type": transform.ResourceType}
		if transform.IDField != "" {
			if id, ok := input[transform.IDField]; ok && id != nil && fmt.Sprint(id) != "" {
				data["id"] = id
			}
		}
		attributes := map[string]any{}
		for _, field := range transform.Attributes {
			if value, ok := input[field]; ok && value != nil {
				attributes[field] = value
			}
		}
		if len(attributes) > 0 {
			data["attributes"] = attributes
		}
		relationships := map[string]any{}
		for name, relationship := range transform.Relationships {
			value, ok := input[relationship.Source]
			if !ok || value == nil || fmt.Sprint(value) == "" {
				continue
			}
			if relationship.Many {
				ids := requestTransformSlice(value)
				linkage := make([]any, 0, len(ids))
				for _, id := range ids {
					linkage = append(linkage, map[string]any{"type": relationship.ResourceType, "id": fmt.Sprint(id)})
				}
				relationships[name] = map[string]any{"data": linkage}
			} else {
				relationships[name] = map[string]any{"data": map[string]any{"type": relationship.ResourceType, "id": fmt.Sprint(value)}}
			}
		}
		if len(relationships) > 0 {
			data["relationships"] = relationships
		}
		return map[string]any{"data": data}, true, nil
	default:
		return nil, true, fmt.Errorf("unsupported request_transform type: %s", transform.Type)
	}
}

func requestTransformSlice(value any) []any {
	switch values := value.(type) {
	case []any:
		return values
	case []string:
		out := make([]any, len(values))
		for i, item := range values {
			out[i] = item
		}
		return out
	default:
		return []any{value}
	}
}

func buildResponseTransformData(transform *ResponseTransformDef, data any, input map[string]any) (any, bool, error) {
	if transform == nil {
		return data, false, nil
	}
	switch transform.Type {
	case "email_message":
		return normalizeEmailMessageWithOptions(data, transform, input), true, nil
	case "email_thread":
		return normalizeEmailThread(data, transform), true, nil
	case "base64_field_decode":
		value := getAnyPath(data, transform.Source)
		decoded := ""
		if s, ok := value.(string); ok {
			decoded = decodeResponseTransformString(s, transform.Encoding)
		}
		out := cloneJSONLike(data)
		if m, ok := out.(map[string]any); ok {
			setBodyPath(m, transform.Target, decoded)
			return m, true, nil
		}
		return map[string]any{transform.Target: decoded}, true, nil
	case "field_map":
		out := map[string]any{}
		for target, source := range transform.Fields {
			if v := getAnyPath(data, source); v != nil {
				setBodyPath(out, target, v)
			}
		}
		return out, true, nil
	default:
		return data, true, fmt.Errorf("unsupported response_transform type: %s", transform.Type)
	}
}

func normalizeEmailThread(data any, transform *ResponseTransformDef) any {
	m, ok := data.(map[string]any)
	if !ok {
		return data
	}
	out := map[string]any{
		"id":        m["id"],
		"historyId": m["historyId"],
	}
	if arr, ok := m["messages"].([]any); ok {
		messages := make([]any, 0, len(arr))
		for _, item := range arr {
			messages = append(messages, normalizeEmailMessage(item))
		}
		out["messageCount"] = len(messages)
		messageIDs := make([]any, 0, len(messages))
		for _, item := range messages {
			if msg, ok := item.(map[string]any); ok {
				if id, ok := msg["id"]; ok && id != nil {
					messageIDs = append(messageIDs, id)
				}
			}
		}
		out["messageIds"] = messageIDs
		compactMessages := make([]any, 0, len(messages))
		for _, item := range messages {
			if msg, ok := item.(map[string]any); ok {
				compactMessages = append(compactMessages, compactEmailMessage(msg))
			}
		}
		out["messages"] = compactMessages
	} else {
		out["messageCount"] = 0
		out["messageIds"] = []any{}
		out["messages"] = []any{}
	}
	return out
}

func compactEmailMessage(m map[string]any) map[string]any {
	return map[string]any{
		"id":           m["id"],
		"threadId":     m["threadId"],
		"labelIds":     m["labelIds"],
		"historyId":    m["historyId"],
		"snippet":      m["snippet"],
		"sizeEstimate": m["sizeEstimate"],
		"internalDate": m["internalDate"],
		"receivedAt":   m["receivedAt"],
		"from":         m["from"],
		"to":           m["to"],
		"cc":           m["cc"],
		"bcc":          m["bcc"],
		"subject":      m["subject"],
		"date":         m["date"],
		"messageId":    m["messageId"],
		"inReplyTo":    m["inReplyTo"],
		"references":   m["references"],
	}
}

func normalizeEmailMessage(data any) any {
	return normalizeEmailMessageWithOptions(data, nil, nil)
}

func normalizeEmailMessageWithOptions(data any, transform *ResponseTransformDef, input map[string]any) any {
	m, ok := data.(map[string]any)
	if !ok {
		return data
	}
	payload, _ := m["payload"].(map[string]any)
	headers := headersObject(payload["headers"])
	bodies := collectEmailBodies(payload)
	internalDate := parseGmailInternalDate(m["internalDate"])
	normalized := map[string]any{
		"id":           m["id"],
		"threadId":     m["threadId"],
		"labelIds":     m["labelIds"],
		"historyId":    m["historyId"],
		"snippet":      m["snippet"],
		"sizeEstimate": m["sizeEstimate"],
		"internalDate": m["internalDate"],
		"receivedAt":   internalDate,
		"headers":      headers,
		"from":         pickEmailHeader(headers, "from"),
		"to":           pickEmailHeader(headers, "to"),
		"cc":           pickEmailHeader(headers, "cc"),
		"bcc":          pickEmailHeader(headers, "bcc"),
		"subject":      pickEmailHeader(headers, "subject"),
		"date":         firstNonEmptyString(pickEmailHeader(headers, "date"), internalDate),
		"messageId":    pickEmailHeader(headers, "message-id"),
		"inReplyTo":    pickEmailHeader(headers, "in-reply-to"),
		"references":   pickEmailHeader(headers, "references"),
		"attachments":  bodies.Attachments,
	}
	return selectEmailBodies(normalized, bodies, transform, input)
}

func responseTransformLocalParams(transform *ResponseTransformDef) map[string]bool {
	params := map[string]bool{}
	if transform != nil && transform.Type == "email_message" {
		if transform.BodyModeParam != "" {
			params[transform.BodyModeParam] = true
		}
		if transform.MaxCharsParam != "" {
			params[transform.MaxCharsParam] = true
		}
	}
	return params
}

func selectEmailBodies(normalized map[string]any, bodies emailBodies, transform *ResponseTransformDef, input map[string]any) map[string]any {
	textBody := strings.TrimSpace(strings.Join(bodies.Text, "\n\n"))
	htmlBody := strings.TrimSpace(strings.Join(bodies.HTML, "\n\n"))
	mode := "both"
	maxChars := -1
	maxCharsLimit := 0
	if transform != nil {
		if transform.DefaultBodyMode != "" {
			mode = transform.DefaultBodyMode
		}
		if transform.DefaultMaxChars > 0 {
			maxChars = transform.DefaultMaxChars
		}
		maxCharsLimit = transform.MaxCharsLimit
		if transform.BodyModeParam != "" {
			if requested := fmt.Sprint(input[transform.BodyModeParam]); validEmailBodyMode(requested) {
				mode = requested
			}
		}
		if transform.MaxCharsParam != "" {
			if requested, ok := positiveInt(input[transform.MaxCharsParam]); ok {
				maxChars = requested
			}
		}
	}
	if !validEmailBodyMode(mode) {
		mode = "both"
	}
	if maxChars > 0 && maxCharsLimit > 0 && maxChars > maxCharsLimit {
		maxChars = maxCharsLimit
	}

	type selectedBody struct {
		key   string
		value string
	}
	selected := []selectedBody{}
	switch mode {
	case "compact":
		value := textBody
		mimeType := "text/plain"
		if value == "" {
			value = htmlBody
			mimeType = "text/html"
		}
		if value == "" {
			mimeType = ""
		}
		normalized["bodyMimeType"] = mimeType
		selected = append(selected, selectedBody{key: "body", value: value})
	case "text":
		selected = append(selected, selectedBody{key: "text", value: textBody})
	case "html":
		selected = append(selected, selectedBody{key: "html", value: htmlBody})
	case "both":
		selected = append(selected, selectedBody{key: "text", value: textBody}, selectedBody{key: "html", value: htmlBody})
	}

	remaining := maxChars
	returnedChars := 0
	selectedChars := 0
	for _, item := range selected {
		chars := []rune(item.value)
		selectedChars += len(chars)
		if remaining >= 0 && len(chars) > remaining {
			chars = chars[:remaining]
		}
		normalized[item.key] = string(chars)
		returnedChars += len(chars)
		if remaining >= 0 {
			remaining -= len(chars)
		}
	}

	normalized["bodyMode"] = mode
	normalized["bodyAvailableChars"] = map[string]any{
		"text": len([]rune(textBody)),
		"html": len([]rune(htmlBody)),
	}
	normalized["bodyReturnedChars"] = returnedChars
	normalized["bodyTruncated"] = returnedChars < selectedChars
	return normalized
}

func validEmailBodyMode(mode string) bool {
	switch mode {
	case "compact", "text", "html", "both", "none":
		return true
	default:
		return false
	}
}

func positiveInt(value any) (int, bool) {
	if value == nil {
		return 0, false
	}
	var parsed int
	if _, err := fmt.Sscanf(fmt.Sprint(value), "%d", &parsed); err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func normalizeIntegrationHTTPError(status int, data any) any {
	if status != 404 {
		return data
	}
	message := "The requested resource was not found."
	switch value := data.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			message = strings.TrimSpace(value)
		}
	case map[string]any:
		if candidate, ok := value["message"].(string); ok && candidate != "" {
			message = candidate
		}
		if providerError, ok := value["error"].(map[string]any); ok {
			if candidate, ok := providerError["message"].(string); ok && candidate != "" {
				message = candidate
			}
		}
	}
	return map[string]any{
		"error":          "not_found",
		"status":         status,
		"retryable":      false,
		"message":        message,
		"instruction":    "Do not retry the same resource ID. List or search for the resource again and use a current ID.",
		"provider_error": data,
	}
}

func headersObject(value any) map[string]string {
	out := map[string]string{}
	arr, ok := value.([]any)
	if !ok {
		return out
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := strings.ToLower(fmt.Sprintf("%v", m["name"]))
		if strings.TrimSpace(name) != "" {
			out[name] = fmt.Sprintf("%v", m["value"])
		}
	}
	return out
}

func pickEmailHeader(headers map[string]string, name string) string {
	return headers[strings.ToLower(name)]
}

type emailBodies struct {
	Text        []string
	HTML        []string
	Attachments []map[string]any
}

func collectEmailBodies(part any) emailBodies {
	out := emailBodies{Text: []string{}, HTML: []string{}, Attachments: []map[string]any{}}
	collectEmailBodiesInto(part, &out)
	return out
}

func collectEmailBodiesInto(part any, out *emailBodies) {
	m, ok := part.(map[string]any)
	if !ok {
		return
	}
	mimeType := fmt.Sprintf("%v", valueOrDefault(m["mimeType"], ""))
	filename := fmt.Sprintf("%v", valueOrDefault(m["filename"], ""))
	body, _ := m["body"].(map[string]any)
	data := ""
	if v, ok := body["data"].(string); ok {
		data = v
	}
	attachmentID := ""
	if v, ok := body["attachmentId"].(string); ok {
		attachmentID = v
	}
	if filename != "" || attachmentID != "" {
		out.Attachments = append(out.Attachments, map[string]any{
			"filename":     filename,
			"mimeType":     mimeType,
			"attachmentId": attachmentID,
			"size":         body["size"],
			"partId":       valueOrDefault(m["partId"], ""),
		})
	} else if data != "" && strings.HasPrefix(strings.ToLower(mimeType), "text/plain") {
		out.Text = append(out.Text, decodeResponseTransformString(data, "base64url"))
	} else if data != "" && strings.HasPrefix(strings.ToLower(mimeType), "text/html") {
		out.HTML = append(out.HTML, decodeResponseTransformString(data, "base64url"))
	}

	if parts, ok := m["parts"].([]any); ok {
		for _, child := range parts {
			collectEmailBodiesInto(child, out)
		}
	}
}

func parseGmailInternalDate(value any) string {
	raw := strings.TrimSpace(fmt.Sprintf("%v", valueOrDefault(value, "")))
	if raw == "" {
		return ""
	}
	var millis int64
	if _, err := fmt.Sscanf(raw, "%d", &millis); err != nil || millis <= 0 {
		return ""
	}
	return time.UnixMilli(millis).UTC().Format(time.RFC3339Nano)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func getAnyPath(data any, path string) any {
	current := data
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			continue
		}
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}
	return current
}

func cloneJSONLike(data any) any {
	raw, err := json.Marshal(data)
	if err != nil {
		return data
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return data
	}
	return out
}

func decodeResponseTransformString(value, encoding string) string {
	if encoding == "base64url" {
		decoded, err := base64.RawURLEncoding.DecodeString(value)
		if err == nil {
			return string(decoded)
		}
		if padded, err := base64.URLEncoding.DecodeString(padBase64(value)); err == nil {
			return string(padded)
		}
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return ""
	}
	return string(decoded)
}

func padBase64(value string) string {
	if mod := len(value) % 4; mod != 0 {
		value += strings.Repeat("=", 4-mod)
	}
	return value
}

func buildMIMEEmail(input map[string]any) (string, error) {
	to := formatAddressList(input["to"])
	if to == "" {
		return "", fmt.Errorf("mime_email transform requires a to recipient")
	}
	textBody := bodyString(input["body"])
	htmlBody := bodyString(input["htmlBody"])
	if textBody == "" && htmlBody == "" {
		return "", fmt.Errorf("mime_email transform requires body or htmlBody")
	}

	headers := []string{
		"MIME-Version: 1.0",
		"To: " + to,
		"Subject: " + encodeMIMEHeader(headerString(input["subject"])),
	}
	addMIMEHeader(&headers, "From", formatAddressList(input["from"]))
	addMIMEHeader(&headers, "Cc", formatAddressList(input["cc"]))
	addMIMEHeader(&headers, "Bcc", formatAddressList(input["bcc"]))
	addMIMEHeader(&headers, "Reply-To", formatAddressList(input["replyTo"]))
	addMIMEHeader(&headers, "In-Reply-To", headerString(input["inReplyTo"]))
	addMIMEHeader(&headers, "References", headerString(input["references"]))

	contentHeaders, contentBody := buildMIMEContent(textBody, htmlBody)
	attachments := parseMIMEAttachments(input["attachments"])
	if len(attachments) == 0 {
		lines := append(headers, contentHeaders...)
		lines = append(lines, "", contentBody)
		return strings.Join(lines, "\r\n"), nil
	}

	mixedBoundary := "apteva_mixed_" + boundarySuffix()
	bodyParts := []string{
		"--" + mixedBoundary,
		strings.Join(contentHeaders, "\r\n"),
		"",
		contentBody,
	}
	for _, attachment := range attachments {
		bodyParts = append(bodyParts,
			"--"+mixedBoundary,
			fmt.Sprintf("Content-Type: %s; name=\"%s\"", sanitizeHeaderValue(attachment.MIMEType), escapeQuotedParam(attachment.Filename)),
			"Content-Transfer-Encoding: base64",
			fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"", escapeQuotedParam(attachment.Filename)),
			"",
			wrapBase64(attachment.Base64),
		)
	}
	bodyParts = append(bodyParts, "--"+mixedBoundary+"--")

	lines := append(headers, fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"", mixedBoundary), "", strings.Join(bodyParts, "\r\n"))
	return strings.Join(lines, "\r\n"), nil
}

func buildMIMEContent(textBody, htmlBody string) ([]string, string) {
	if textBody != "" && htmlBody != "" {
		boundary := "apteva_alt_" + boundarySuffix()
		body := strings.Join([]string{
			"--" + boundary,
			strings.Join(mimeTextPartHeaders("text/plain"), "\r\n"),
			"",
			encodeRequestTransformString(textBody, "base64"),
			"--" + boundary,
			strings.Join(mimeTextPartHeaders("text/html"), "\r\n"),
			"",
			encodeRequestTransformString(htmlBody, "base64"),
			"--" + boundary + "--",
		}, "\r\n")
		return []string{fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"", boundary)}, body
	}

	contentType := "text/plain"
	body := textBody
	if htmlBody != "" {
		contentType = "text/html"
		body = htmlBody
	}
	return mimeTextPartHeaders(contentType), encodeRequestTransformString(body, "base64")
}

func mimeTextPartHeaders(contentType string) []string {
	return []string{
		"Content-Type: " + contentType + "; charset=UTF-8",
		"Content-Transfer-Encoding: base64",
	}
}

func addMIMEHeader(headers *[]string, name, value string) {
	if value != "" {
		*headers = append(*headers, name+": "+sanitizeHeaderValue(value))
	}
}

func encodeMIMEHeader(value string) string {
	value = sanitizeHeaderValue(value)
	if value == "" || utf8.RuneCountInString(value) == len(value) {
		return value
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

func formatAddressList(value any) string {
	parts := []string{}
	for _, item := range arrayFromInput(value) {
		cleaned := sanitizeHeaderValue(fmt.Sprintf("%v", item))
		if cleaned != "" {
			parts = append(parts, cleaned)
		}
	}
	return strings.Join(parts, ", ")
}

func arrayFromInput(value any) []any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case []any:
		return v
	case []string:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, item)
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		parts := strings.Split(v, ",")
		out := make([]any, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	default:
		return []any{v}
	}
}

func headerString(value any) string {
	if value == nil {
		return ""
	}
	return sanitizeHeaderValue(fmt.Sprintf("%v", value))
}

func bodyString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%v", value)
}

type mimeAttachment struct {
	Filename string
	MIMEType string
	Base64   string
}

func parseMIMEAttachments(value any) []mimeAttachment {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := []mimeAttachment{}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		filename := headerString(valueOrDefault(m["filename"], "attachment"))
		mimeType := headerString(valueOrDefault(valueOrDefault(m["mimeType"], m["contentType"]), "application/octet-stream"))
		rawBase64 := strings.TrimSpace(fmt.Sprintf("%v", valueOrDefault(m["base64"], "")))
		content := bodyString(m["content"])
		if rawBase64 == "" && content != "" {
			rawBase64 = base64.StdEncoding.EncodeToString([]byte(content))
		}
		if rawBase64 != "" {
			out = append(out, mimeAttachment{Filename: filename, MIMEType: mimeType, Base64: rawBase64})
		}
	}
	return out
}

func valueOrDefault(value any, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}

func sanitizeHeaderValue(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
}

func escapeQuotedParam(value string) string {
	value = sanitizeHeaderValue(value)
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

func encodeRequestTransformString(value, encoding string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	if encoding == "base64url" {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	return wrapBase64(encoded)
}

func wrapBase64(value string) string {
	compact := strings.Join(strings.Fields(value), "")
	if compact == "" {
		return ""
	}
	parts := []string{}
	for len(compact) > 76 {
		parts = append(parts, compact[:76])
		compact = compact[76:]
	}
	parts = append(parts, compact)
	return strings.Join(parts, "\r\n")
}

func copyRequestTransformFields(body map[string]any, input map[string]any, includeFields map[string]string) {
	for source, target := range includeFields {
		if v, ok := input[source]; ok && v != nil {
			if s, isString := v.(string); isString && s == "" {
				continue
			}
			setBodyPath(body, target, v)
		}
	}
}

func setBodyPath(body map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	if len(parts) == 0 || strings.TrimSpace(path) == "" {
		return
	}
	current := body
	for _, part := range parts[:len(parts)-1] {
		if strings.TrimSpace(part) == "" {
			continue
		}
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	last := parts[len(parts)-1]
	if strings.TrimSpace(last) != "" {
		current[last] = value
	}
}

func boundarySuffix() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
