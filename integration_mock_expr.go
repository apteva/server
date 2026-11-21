package main

// Expressions and templates for catalog provider simulations (the `mock`
// section of an integration). The language is deliberately small: paths,
// literals, !, ==, !=, &&, || and a few functions. See integrations
// src/types.ts AppMockConfig for the authoring reference.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type mockExpr interface {
	eval(sc *mockScope) (any, error)
}

type mockLiteral struct{ value any }
type mockVar struct{ name string }
type mockIndex struct{ base, key mockExpr }
type mockNot struct{ x mockExpr }
type mockBinary struct {
	op   string
	l, r mockExpr
}
type mockCall struct {
	name string
	args []mockExpr
}

// mockScope is the evaluation environment of one simulated operation.
type mockScope struct {
	vars map[string]any
	fn   mockFunctions
}

// mockFunctions supplies the functions that need simulation state.
type mockFunctions interface {
	now() string
	nextID(kind, prefix string) string
	// fixtureURL is the public URL the server serves a fixture file at.
	fixtureURL(name string) string
}

func (e mockLiteral) eval(*mockScope) (any, error) { return e.value, nil }

func (e mockVar) eval(sc *mockScope) (any, error) { return sc.vars[e.name], nil }

func (e mockIndex) eval(sc *mockScope) (any, error) {
	base, err := e.base.eval(sc)
	if err != nil || base == nil {
		return nil, err
	}
	key, err := e.key.eval(sc)
	if err != nil {
		return nil, err
	}
	switch value := base.(type) {
	case map[string]any:
		return value[mockString(key)], nil
	case []any:
		index, ok := mockNumber(key)
		if !ok || index < 0 || int(index) >= len(value) || index != float64(int(index)) {
			return nil, nil
		}
		return value[int(index)], nil
	}
	return nil, nil
}

func (e mockNot) eval(sc *mockScope) (any, error) {
	value, err := e.x.eval(sc)
	if err != nil {
		return nil, err
	}
	return !mockTruthy(value), nil
}

func (e mockBinary) eval(sc *mockScope) (any, error) {
	left, err := e.l.eval(sc)
	if err != nil {
		return nil, err
	}
	switch e.op {
	case "||":
		if mockTruthy(left) {
			return left, nil
		}
		return e.r.eval(sc)
	case "&&":
		if !mockTruthy(left) {
			return left, nil
		}
		return e.r.eval(sc)
	}
	right, err := e.r.eval(sc)
	if err != nil {
		return nil, err
	}
	if e.op == "==" {
		return mockEqual(left, right), nil
	}
	return !mockEqual(left, right), nil
}

func (e mockCall) eval(sc *mockScope) (any, error) {
	args := make([]any, len(e.args))
	for i, arg := range e.args {
		value, err := arg.eval(sc)
		if err != nil {
			return nil, err
		}
		args[i] = value
	}
	switch e.name {
	case "len":
		switch value := args[0].(type) {
		case nil:
			return float64(0), nil
		case string:
			return float64(len(value)), nil
		case []any:
			return float64(len(value)), nil
		case map[string]any:
			return float64(len(value)), nil
		}
		return float64(0), nil
	case "lower":
		if args[0] == nil {
			return nil, nil
		}
		return strings.ToLower(mockString(args[0])), nil
	case "now":
		return sc.fn.now(), nil
	case "uuid":
		return sc.fn.nextID("uuid", ""), nil
	case "numeric_id":
		return sc.fn.nextID("numeric", ""), nil
	case "token":
		return sc.fn.nextID("token", mockString(args[0])), nil
	case "fixture_url":
		name := mockString(args[0])
		if _, ok := mockFixtureByName(name); !ok {
			return nil, fmt.Errorf("fixture_url: unknown fixture %q", name)
		}
		return sc.fn.fixtureURL(name), nil
	}
	return nil, fmt.Errorf("unknown function %s", e.name)
}

var mockFunctionArity = map[string]int{"len": 1, "lower": 1, "now": 0, "uuid": 0, "numeric_id": 0, "token": 1, "fixture_url": 1}

func mockTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	}
	if n, ok := mockNumber(value); ok {
		return n != 0
	}
	return true
}

func mockNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func mockString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case map[string]any, []any:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	if n, ok := mockNumber(value); ok {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return fmt.Sprint(value)
}

func mockEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if x, ok := mockNumber(a); ok {
		if y, ok := mockNumber(b); ok {
			return x == y
		}
	}
	if x, ok := a.(bool); ok {
		y, ok := b.(bool)
		return ok && x == y
	}
	switch a.(type) {
	case map[string]any, []any:
		return mockString(a) == mockString(b)
	}
	return mockString(a) == mockString(b)
}

// --- parser ---

type mockToken struct {
	kind string // ident, number, string, op, eof
	text string
}

func lexMockExpr(src string) ([]mockToken, error) {
	var out []mockToken
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i + 1
			for j < len(src) && (src[j] == '_' || src[j] == '$' || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= '0' && src[j] <= '9')) {
				j++
			}
			out = append(out, mockToken{"ident", src[i:j]})
			i = j
		case (c >= '0' && c <= '9') || (c == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i + 1
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				j++
			}
			out = append(out, mockToken{"number", src[i:j]})
			i = j
		case c == '\'':
			var b strings.Builder
			j := i + 1
			for ; j < len(src) && src[j] != '\''; j++ {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				b.WriteByte(src[j])
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			out = append(out, mockToken{"string", b.String()})
			i = j + 1
		default:
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "==", "!=", "&&", "||":
				out = append(out, mockToken{"op", two})
				i += 2
				continue
			}
			if strings.ContainsRune(".[](),!", rune(c)) {
				out = append(out, mockToken{"op", string(c)})
				i++
				continue
			}
			return nil, fmt.Errorf("unexpected %q", string(c))
		}
	}
	return append(out, mockToken{kind: "eof"}), nil
}

type mockParser struct {
	tokens []mockToken
	pos    int
}

func parseMockExpr(src string) (mockExpr, error) {
	tokens, err := lexMockExpr(src)
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", src, err)
	}
	p := &mockParser{tokens: tokens}
	expr, err := p.or()
	if err == nil && p.peek().kind != "eof" {
		err = fmt.Errorf("unexpected %q", p.peek().text)
	}
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", src, err)
	}
	return expr, nil
}

func (p *mockParser) peek() mockToken { return p.tokens[p.pos] }
func (p *mockParser) next() mockToken {
	t := p.tokens[p.pos]
	if t.kind != "eof" {
		p.pos++
	}
	return t
}
func (p *mockParser) accept(op string) bool {
	if t := p.peek(); t.kind == "op" && t.text == op {
		p.pos++
		return true
	}
	return false
}

func (p *mockParser) or() (mockExpr, error) {
	left, err := p.and()
	for err == nil && p.accept("||") {
		var right mockExpr
		right, err = p.and()
		left = mockBinary{op: "||", l: left, r: right}
	}
	return left, err
}

func (p *mockParser) and() (mockExpr, error) {
	left, err := p.equality()
	for err == nil && p.accept("&&") {
		var right mockExpr
		right, err = p.equality()
		left = mockBinary{op: "&&", l: left, r: right}
	}
	return left, err
}

func (p *mockParser) equality() (mockExpr, error) {
	left, err := p.unary()
	for err == nil {
		op := ""
		if p.accept("==") {
			op = "=="
		} else if p.accept("!=") {
			op = "!="
		} else {
			break
		}
		var right mockExpr
		right, err = p.unary()
		left = mockBinary{op: op, l: left, r: right}
	}
	return left, err
}

func (p *mockParser) unary() (mockExpr, error) {
	if p.accept("!") {
		x, err := p.unary()
		return mockNot{x: x}, err
	}
	return p.postfix()
}

func (p *mockParser) postfix() (mockExpr, error) {
	expr, err := p.primary()
	for err == nil {
		if p.accept(".") {
			t := p.next()
			if t.kind != "ident" {
				return nil, fmt.Errorf("expected field name after '.'")
			}
			expr = mockIndex{base: expr, key: mockLiteral{t.text}}
			continue
		}
		if p.accept("[") {
			var key mockExpr
			key, err = p.or()
			if err == nil && !p.accept("]") {
				err = fmt.Errorf("expected ']'")
			}
			expr = mockIndex{base: expr, key: key}
			continue
		}
		break
	}
	return expr, err
}

func (p *mockParser) primary() (mockExpr, error) {
	t := p.next()
	switch t.kind {
	case "number":
		n, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("bad number %q", t.text)
		}
		return mockLiteral{n}, nil
	case "string":
		return mockLiteral{t.text}, nil
	case "ident":
		switch t.text {
		case "true":
			return mockLiteral{true}, nil
		case "false":
			return mockLiteral{false}, nil
		case "null":
			return mockLiteral{nil}, nil
		}
		if p.accept("(") {
			arity, known := mockFunctionArity[t.text]
			if !known {
				return nil, fmt.Errorf("unknown function %s", t.text)
			}
			var args []mockExpr
			if !p.accept(")") {
				for {
					arg, err := p.or()
					if err != nil {
						return nil, err
					}
					args = append(args, arg)
					if p.accept(")") {
						break
					}
					if !p.accept(",") {
						return nil, fmt.Errorf("expected ',' or ')'")
					}
				}
			}
			if len(args) != arity {
				return nil, fmt.Errorf("%s takes %d argument(s)", t.text, arity)
			}
			return mockCall{name: t.text, args: args}, nil
		}
		return mockVar{t.text}, nil
	case "op":
		if t.text == "(" {
			expr, err := p.or()
			if err == nil && !p.accept(")") {
				err = fmt.Errorf("expected ')'")
			}
			return expr, err
		}
	}
	if t.kind == "eof" {
		return nil, fmt.Errorf("unexpected end")
	}
	return nil, fmt.Errorf("unexpected %q", t.text)
}

// --- templates ---

type mockTemplate interface {
	render(sc *mockScope, t *mockTemplates) (any, error)
}

// mockTemplates holds the named `$use` templates of one simulation.
type mockTemplates map[string]*mockMapTemplate

type mockLiteralTemplate struct{ value any }
type mockExprTemplate struct{ expr mockExpr }
type mockInterpTemplate struct {
	text  []string
	exprs []mockExpr // len(text) == len(exprs)+1
}
type mockMapTemplate struct {
	use    string
	keys   []string
	values []mockTemplate
}
type mockListTemplate struct{ items []mockTemplate }

var mockWholeTemplate = regexp.MustCompile(`^\s*\{\{([^{}]*)\}\}\s*$`)

func compileMockTemplate(value any) (mockTemplate, error) {
	switch v := value.(type) {
	case string:
		if m := mockWholeTemplate.FindStringSubmatch(v); m != nil {
			expr, err := parseMockExpr(m[1])
			if err != nil {
				return nil, err
			}
			return mockExprTemplate{expr}, nil
		}
		if !strings.Contains(v, "{{") {
			return mockLiteralTemplate{v}, nil
		}
		out := mockInterpTemplate{}
		rest := v
		for {
			start := strings.Index(rest, "{{")
			if start < 0 {
				out.text = append(out.text, rest)
				break
			}
			end := strings.Index(rest[start:], "}}")
			if end < 0 {
				return nil, fmt.Errorf("unterminated template in %q", v)
			}
			expr, err := parseMockExpr(rest[start+2 : start+end])
			if err != nil {
				return nil, err
			}
			out.text = append(out.text, rest[:start])
			out.exprs = append(out.exprs, expr)
			rest = rest[start+end+2:]
		}
		return out, nil
	case map[string]any:
		out := &mockMapTemplate{}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key == "$use" {
				name, ok := v[key].(string)
				if !ok || name == "" {
					return nil, fmt.Errorf("$use must name a template")
				}
				out.use = name
				continue
			}
			item, err := compileMockTemplate(v[key])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			out.keys = append(out.keys, key)
			out.values = append(out.values, item)
		}
		return out, nil
	case []any:
		out := mockListTemplate{}
		for i, item := range v {
			compiled, err := compileMockTemplate(item)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out.items = append(out.items, compiled)
		}
		return out, nil
	}
	return mockLiteralTemplate{value}, nil
}

func (t mockLiteralTemplate) render(*mockScope, *mockTemplates) (any, error) {
	return cloneMockValue(t.value), nil
}

func (t mockExprTemplate) render(sc *mockScope, _ *mockTemplates) (any, error) {
	value, err := t.expr.eval(sc)
	return cloneMockValue(value), err
}

func (t mockInterpTemplate) render(sc *mockScope, _ *mockTemplates) (any, error) {
	var b strings.Builder
	for i, expr := range t.exprs {
		b.WriteString(t.text[i])
		value, err := expr.eval(sc)
		if err != nil {
			return nil, err
		}
		b.WriteString(mockString(value))
	}
	b.WriteString(t.text[len(t.text)-1])
	return b.String(), nil
}

func (t *mockMapTemplate) render(sc *mockScope, named *mockTemplates) (any, error) {
	out := map[string]any{}
	if t.use != "" {
		base, ok := (*named)[t.use]
		if !ok {
			return nil, fmt.Errorf("unknown template %q", t.use)
		}
		rendered, err := base.render(sc, named)
		if err != nil {
			return nil, err
		}
		out = rendered.(map[string]any)
	}
	for i, key := range t.keys {
		value, err := t.values[i].render(sc, named)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		// A whole-expression value that is null means "not provided".
		if _, isExpr := t.values[i].(mockExprTemplate); isExpr && value == nil {
			continue
		}
		out[key] = value
	}
	return out, nil
}

func (t mockListTemplate) render(sc *mockScope, named *mockTemplates) (any, error) {
	out := make([]any, 0, len(t.items))
	for _, item := range t.items {
		value, err := item.render(sc, named)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// cloneMockValue deep-copies JSON-shaped values so rendered output never
// aliases simulation state.
func cloneMockValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cloneMockValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneMockValue(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneMockValue(item)
		}
		return out
	}
	return value
}
