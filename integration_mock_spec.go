package main

// Catalog provider simulation: the `mock` section of an integration (see
// integrations src/types.ts AppMockConfig). It is compiled once per catalog
// template and run only for fake connections that a runtime bound with
// simulate=true.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type AppMockConfig struct {
	Version     int                          `json:"version"`
	Signing     *AppMockSigning              `json:"signing,omitempty"`
	Delivery    *AppMockDelivery             `json:"delivery,omitempty"`
	Envelope    any                          `json:"envelope,omitempty"`
	Errors      map[string]AppMockError      `json:"errors,omitempty"`
	Templates   map[string]map[string]any    `json:"templates,omitempty"`
	Collections map[string]AppMockCollection `json:"collections"`
	Tools       map[string]AppMockOperation  `json:"tools"`
	Triggers    map[string]AppMockTrigger    `json:"triggers,omitempty"`
}

type AppMockSigning struct {
	Algorithm       string `json:"algorithm"`
	PublicKeyField  string `json:"public_key_field"`
	SignatureHeader string `json:"signature_header"`
	TimestampHeader string `json:"timestamp_header"`
	Message         string `json:"message"`
}

type AppMockDelivery struct {
	MaxAttempts   int   `json:"max_attempts,omitempty"`
	RetryStatuses []int `json:"retry_statuses,omitempty"`
}

type AppMockError struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

type AppMockCollection struct {
	Key      string         `json:"key,omitempty"`
	ID       any            `json:"id,omitempty"` // "numeric", "uuid" or {"token": "<prefix>"}
	Defaults map[string]any `json:"defaults,omitempty"`
	Seedable bool           `json:"seedable,omitempty"`
}

type AppMockOperation struct {
	Steps []map[string]any `json:"steps"`
}

type AppMockTrigger struct {
	Description string           `json:"description"`
	InputSchema map[string]any   `json:"input_schema"`
	Steps       []map[string]any `json:"steps"`
}

// --- compiled form ---

type compiledMock struct {
	signing     *AppMockSigning
	maxAttempts int
	retry       map[int]bool
	envelope    mockTemplate
	errors      map[string]compiledMockError
	templates   mockTemplates
	collections map[string]*compiledMockCollection
	tools       map[string]*compiledMockOperation
	triggers    map[string]*compiledMockTrigger
}

type compiledMockError struct {
	status int
	body   mockTemplate
}

type compiledMockCollection struct {
	key      string
	idKind   string
	idPrefix string
	defaults map[string]any
	seedable bool
}

type compiledMockOperation struct {
	steps []*mockStep
}

type compiledMockTrigger struct {
	compiledMockOperation
	required []string
}

type mockStep struct {
	op        string
	target    string
	when      mockExpr
	condition mockExpr // require
	letKeys   []string
	letValues []mockTemplate
	key       mockTemplate
	whereKeys []string
	whereVals []mockTemplate
	record    mockTemplate
	set       mockTemplate
	merge     mockTemplate
	omit      map[string]bool
	unset     []string
	as        string
	to        mockTemplate
	lane      mockTemplate
	payload   mockTemplate
	dedupe    mockTemplate
	respond   mockTemplate
	status    int
	errName   string
	detail    mockTemplate
}

var mockStepOperations = []string{"let", "require", "lookup", "find", "list", "create", "update", "delete", "emit", "dedupe", "respond"}

var mockStepFields = map[string]bool{
	"when": true, "key": true, "where": true, "record": true, "set": true, "merge": true, "omit": true,
	"unset": true, "as": true, "to": true, "payload": true, "lane": true, "status": true, "error": true, "detail": true,
}

const (
	mockMaxDeliveryAttempts     = 5
	mockDefaultDeliveryAttempts = 3
)

var compiledMocks sync.Map // *AppTemplate -> *compiledMock

// compiledAppMock compiles (once per catalog template) the app's mock
// section. Returns nil, nil when the app has none.
func compiledAppMock(app *AppTemplate) (*compiledMock, error) {
	if app == nil || app.Mock == nil {
		return nil, nil
	}
	if cached, ok := compiledMocks.Load(app); ok {
		return cached.(*compiledMock), nil
	}
	compiled, err := compileAppMock(app)
	if err != nil {
		return nil, fmt.Errorf("integration %s mock: %w", app.Slug, err)
	}
	actual, _ := compiledMocks.LoadOrStore(app, compiled)
	return actual.(*compiledMock), nil
}

func compileAppMock(app *AppTemplate) (*compiledMock, error) {
	cfg := app.Mock
	if cfg.Version != 1 {
		return nil, fmt.Errorf("unsupported version %d", cfg.Version)
	}
	out := &compiledMock{
		signing:     cfg.Signing,
		maxAttempts: mockDefaultDeliveryAttempts,
		retry:       map[int]bool{},
		errors:      map[string]compiledMockError{},
		templates:   mockTemplates{},
		collections: map[string]*compiledMockCollection{},
		tools:       map[string]*compiledMockOperation{},
		triggers:    map[string]*compiledMockTrigger{},
	}
	if cfg.Delivery != nil {
		if cfg.Delivery.MaxAttempts != 0 {
			if cfg.Delivery.MaxAttempts < 1 || cfg.Delivery.MaxAttempts > mockMaxDeliveryAttempts {
				return nil, fmt.Errorf("delivery.max_attempts must be 1-%d", mockMaxDeliveryAttempts)
			}
			out.maxAttempts = cfg.Delivery.MaxAttempts
		}
		for _, status := range cfg.Delivery.RetryStatuses {
			out.retry[status] = true
		}
	}
	if cfg.Signing != nil {
		s := cfg.Signing
		if s.Algorithm != "ed25519" {
			return nil, fmt.Errorf("signing.algorithm %q is not supported", s.Algorithm)
		}
		if s.PublicKeyField == "" || s.SignatureHeader == "" || s.TimestampHeader == "" || !strings.Contains(s.Message, "{body}") {
			return nil, fmt.Errorf("signing needs public_key_field, signature_header, timestamp_header and a message that signs {body}")
		}
	}
	if cfg.Envelope != nil {
		envelope, err := compileMockTemplate(cfg.Envelope)
		if err != nil {
			return nil, fmt.Errorf("envelope: %w", err)
		}
		out.envelope = envelope
	}
	for name, def := range cfg.Errors {
		body, err := compileMockTemplate(def.Body)
		if err != nil {
			return nil, fmt.Errorf("errors.%s: %w", name, err)
		}
		if def.Status < 400 || def.Status > 599 {
			return nil, fmt.Errorf("errors.%s: status must be 4xx or 5xx", name)
		}
		out.errors[name] = compiledMockError{status: def.Status, body: body}
	}
	for name, def := range cfg.Templates {
		compiled, err := compileMockTemplate(map[string]any(def))
		if err != nil {
			return nil, fmt.Errorf("templates.%s: %w", name, err)
		}
		out.templates[name] = compiled.(*mockMapTemplate)
	}
	for name, def := range cfg.Collections {
		collection := &compiledMockCollection{key: def.Key, defaults: def.Defaults, seedable: def.Seedable}
		if collection.key == "" {
			collection.key = "id"
		}
		switch id := def.ID.(type) {
		case nil:
		case string:
			if id != "numeric" && id != "uuid" {
				return nil, fmt.Errorf("collections.%s.id: unknown generator %q", name, id)
			}
			collection.idKind = id
		case map[string]any:
			prefix, ok := id["token"].(string)
			if !ok {
				return nil, fmt.Errorf("collections.%s.id: expected {token: prefix}", name)
			}
			collection.idKind, collection.idPrefix = "token", prefix
		default:
			return nil, fmt.Errorf("collections.%s.id: unsupported generator", name)
		}
		out.collections[name] = collection
	}
	toolNames := map[string]bool{}
	for _, tool := range app.Tools {
		toolNames[tool.Name] = true
	}
	events := map[string]bool{}
	if app.Webhooks != nil {
		for _, event := range app.Webhooks.Events {
			events[event.Name] = true
		}
	}
	for name, def := range cfg.Tools {
		if !toolNames[name] {
			return nil, fmt.Errorf("tools.%s: not a catalog tool", name)
		}
		op, err := out.compileOperation("tools."+name, def.Steps, events)
		if err != nil {
			return nil, err
		}
		out.tools[name] = op
	}
	for name, def := range cfg.Triggers {
		op, err := out.compileOperation("triggers."+name, def.Steps, events)
		if err != nil {
			return nil, err
		}
		trigger := &compiledMockTrigger{compiledMockOperation: *op}
		if required, ok := def.InputSchema["required"].([]any); ok {
			for _, field := range required {
				if name, ok := field.(string); ok {
					trigger.required = append(trigger.required, name)
				}
			}
		}
		out.triggers[name] = trigger
	}
	// Validate $use references now rather than at the first render.
	for name, tmpl := range out.templates {
		if tmpl.use != "" {
			if _, ok := out.templates[tmpl.use]; !ok || tmpl.use == name {
				return nil, fmt.Errorf("templates.%s: bad $use %q", name, tmpl.use)
			}
		}
	}
	if err := checkMockTemplateUses(cfg, out.templates); err != nil {
		return nil, err
	}
	return out, nil
}

func checkMockTemplateUses(value any, templates mockTemplates) error {
	switch v := value.(type) {
	case *AppMockConfig:
		for _, part := range []any{v.Envelope, toAnyMap(v.Templates), toolSteps(v.Tools), triggerSteps(v.Triggers)} {
			if err := checkMockTemplateUses(part, templates); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, item := range v {
			if key == "$use" {
				if name, _ := item.(string); templates[name] == nil {
					return fmt.Errorf("$use %q: unknown template", name)
				}
				continue
			}
			if err := checkMockTemplateUses(item, templates); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := checkMockTemplateUses(item, templates); err != nil {
				return err
			}
		}
	case []map[string]any:
		for _, item := range v {
			if err := checkMockTemplateUses(item, templates); err != nil {
				return err
			}
		}
	}
	return nil
}

func toAnyMap(in map[string]map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range in {
		out[key] = value
	}
	return out
}

func toolSteps(in map[string]AppMockOperation) []any {
	var out []any
	for _, op := range in {
		for _, step := range op.Steps {
			out = append(out, step)
		}
	}
	return out
}

func triggerSteps(in map[string]AppMockTrigger) []any {
	var out []any
	for _, op := range in {
		for _, step := range op.Steps {
			out = append(out, step)
		}
	}
	return out
}

func (m *compiledMock) compileOperation(where string, steps []map[string]any, events map[string]bool) (*compiledMockOperation, error) {
	if len(steps) == 0 {
		return nil, fmt.Errorf("%s: steps required", where)
	}
	op := &compiledMockOperation{}
	for i, raw := range steps {
		at := fmt.Sprintf("%s.steps[%d]", where, i)
		step, err := m.compileStep(raw, events)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", at, err)
		}
		op.steps = append(op.steps, step)
	}
	last := op.steps[len(op.steps)-1]
	if last.op != "respond" || last.when != nil {
		return nil, fmt.Errorf("%s: last step must be an unconditional respond", where)
	}
	return op, nil
}

func (m *compiledMock) compileStep(raw map[string]any, events map[string]bool) (*mockStep, error) {
	step := &mockStep{}
	for _, name := range mockStepOperations {
		if _, ok := raw[name]; ok {
			if step.op != "" {
				return nil, fmt.Errorf("both %s and %s set", step.op, name)
			}
			step.op = name
		}
	}
	if step.op == "" {
		return nil, fmt.Errorf("no operation")
	}
	for key := range raw {
		if key != step.op && !mockStepFields[key] {
			return nil, fmt.Errorf("unknown field %q", key)
		}
	}
	expr := func(field string) (mockExpr, error) {
		text, ok := raw[field].(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an expression string", field)
		}
		return parseMockExpr(text)
	}
	tmpl := func(field string) (mockTemplate, error) {
		value, ok := raw[field]
		if !ok {
			return nil, nil
		}
		t, err := compileMockTemplate(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		return t, nil
	}
	mapTmpl := func(field string) (mockTemplate, error) {
		value, ok := raw[field]
		if !ok {
			return nil, nil
		}
		if _, isMap := value.(map[string]any); !isMap {
			return nil, fmt.Errorf("%s must be an object", field)
		}
		return tmpl(field)
	}
	var err error
	if _, ok := raw["when"]; ok {
		if step.when, err = expr("when"); err != nil {
			return nil, err
		}
	}
	if name, ok := raw["error"]; ok {
		step.errName, _ = name.(string)
		if _, known := m.errors[step.errName]; !known {
			return nil, fmt.Errorf("unknown error %q", step.errName)
		}
	}
	if step.detail, err = tmpl("detail"); err != nil {
		return nil, err
	}
	if as, ok := raw["as"]; ok {
		step.as, _ = as.(string)
		if step.as == "" || step.as == "input" {
			return nil, fmt.Errorf("as must name a variable other than input")
		}
	}
	if status, ok := raw["status"]; ok {
		n, isNumber := mockNumber(status)
		if !isNumber || n < 100 || n > 599 {
			return nil, fmt.Errorf("status must be an HTTP status")
		}
		step.status = int(n)
	}
	stringList := func(field string) ([]string, error) {
		value, ok := raw[field]
		if !ok {
			return nil, nil
		}
		items, isList := value.([]any)
		if !isList {
			return nil, fmt.Errorf("%s must be a list of field names", field)
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			name, ok := item.(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("%s must be a list of field names", field)
			}
			out = append(out, name)
		}
		return out, nil
	}
	collection := func() error {
		name, _ := raw[step.op].(string)
		if m.collections[name] == nil {
			return fmt.Errorf("unknown collection %q", name)
		}
		step.target = name
		return nil
	}

	switch step.op {
	case "let":
		values, ok := raw["let"].(map[string]any)
		if !ok || len(values) == 0 {
			return nil, fmt.Errorf("let must be an object")
		}
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key == "input" {
				return nil, fmt.Errorf("let cannot replace input")
			}
			t, err := compileMockTemplate(values[key])
			if err != nil {
				return nil, fmt.Errorf("let.%s: %w", key, err)
			}
			step.letKeys = append(step.letKeys, key)
			step.letValues = append(step.letValues, t)
		}
	case "require":
		if step.condition, err = expr("require"); err != nil {
			return nil, err
		}
		if step.errName == "" {
			return nil, fmt.Errorf("require needs error")
		}
	case "lookup", "update", "delete":
		if err := collection(); err != nil {
			return nil, err
		}
		if step.key, err = tmpl("key"); err != nil || step.key == nil {
			return nil, fmt.Errorf("%s needs key", step.op)
		}
	case "find", "list":
		if err := collection(); err != nil {
			return nil, err
		}
		if step.as == "" {
			return nil, fmt.Errorf("%s needs as", step.op)
		}
		if where, ok := raw["where"].(map[string]any); ok {
			keys := make([]string, 0, len(where))
			for key := range where {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				t, err := compileMockTemplate(where[key])
				if err != nil {
					return nil, fmt.Errorf("where.%s: %w", key, err)
				}
				step.whereKeys = append(step.whereKeys, key)
				step.whereVals = append(step.whereVals, t)
			}
		}
	case "create":
		if err := collection(); err != nil {
			return nil, err
		}
	case "emit":
		step.target, _ = raw["emit"].(string)
		if !events[step.target] {
			return nil, fmt.Errorf("event %q is not declared in webhooks.events", step.target)
		}
		if m.signing == nil || m.envelope == nil {
			return nil, fmt.Errorf("emit needs signing and envelope")
		}
		if step.to, err = tmpl("to"); err != nil || step.to == nil {
			return nil, fmt.Errorf("emit needs to")
		}
		if step.lane, err = tmpl("lane"); err != nil {
			return nil, err
		}
		if step.payload, err = mapTmpl("payload"); err != nil {
			return nil, err
		}
	case "dedupe":
		if step.dedupe, err = tmpl("dedupe"); err != nil {
			return nil, err
		}
	case "respond":
		if step.respond, err = tmpl("respond"); err != nil {
			return nil, err
		}
	}
	if step.op == "lookup" && step.as == "" {
		return nil, fmt.Errorf("lookup needs as")
	}
	if step.record, err = mapTmpl("record"); err != nil {
		return nil, err
	}
	if step.set, err = mapTmpl("set"); err != nil {
		return nil, err
	}
	if step.merge, err = tmpl("merge"); err != nil {
		return nil, err
	}
	omit, err := stringList("omit")
	if err != nil {
		return nil, err
	}
	if len(omit) > 0 {
		step.omit = map[string]bool{}
		for _, field := range omit {
			step.omit[field] = true
		}
	}
	if step.unset, err = stringList("unset"); err != nil {
		return nil, err
	}
	return step, nil
}
