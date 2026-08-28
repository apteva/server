package main

// Outbound-request signer registry. The integration runner
// (executeIntegrationTool) used to special-case aws_sigv4 with a
// single inline branch; that doesn't scale to providers that need
// HMAC over a canonical string (Polymarket L2, Coinbase Advanced,
// KuCoin, Kraken, dozen others) or EIP-712 typed-data signing of
// payload bodies (Polymarket orders, dYdX v4, Hyperliquid, Permit2).
//
// The fix: a registry of `Signer` implementations. Each integration
// declares one or more signer NAMES in its catalog JSON (either at
// app-level via auth.signers[] or per-tool via tools[].signing.signers[]).
// At request time the runner pulls the named signers, executes them
// in declared order, and only then sends the request. Each signer is
// a one-file implementation in this package (signer_aws_sigv4.go,
// signer_hmac_sha256.go, signer_polymarket_l2.go, signer_eip712.go,
// ...) and self-registers via init().
//
// Composition rules:
//   - Body-mutating signers (EIP-712 injects a `signature` field into
//     the JSON body) run BEFORE header-only signers, so the latter
//     see the final body when they compute their canonical string.
//   - Each signer receives (req, body, creds, params) and may rewrite
//     either headers or body. If it rewrites body, it returns the new
//     bytes and the runner re-attaches them before the next signer
//     runs.
//   - Failures abort the chain (don't sign half a request and send it).

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

const signerInputPrefix = "__apteva_signer_input__."

// Signer signs one outbound request, possibly mutating headers and/or
// body. Implementations live in sibling files; one per scheme.
type Signer interface {
	// Name — the string the integration catalog references in
	// auth.signers[] / tools[].signing.signers[]. Must match the
	// registry key.
	Name() string

	// Sign mutates req (headers) and may return new body bytes (any
	// non-nil return replaces the request's body bytes for the next
	// signer in the chain and for the wire). Returning a nil byte
	// slice with err==nil means "I didn't touch the body".
	//
	// creds is the connection's decrypted credential map (the same map
	// the runner already uses to template auth.headers / auth.query).
	//
	// params is the SignerSpec.Params (free-form JSON object from the
	// catalog), per-signer-defined. Each Signer documents what it reads.
	Sign(ctx context.Context, req *http.Request, body []byte,
		creds map[string]string, params map[string]any) (newBody []byte, err error)
}

// SignerSpec — how a catalog entry references a signer. The Name
// indexes into the registry; Params get passed through unchanged.
type SignerSpec struct {
	Name   string         `json:"name"`
	Params map[string]any `json:"params,omitempty"`
}

// ─── Registry ──────────────────────────────────────────────────────

var (
	signersMu sync.RWMutex
	signers   = map[string]Signer{}
)

// RegisterSigner — called from sibling files' init() blocks. Panics on
// duplicate names (programmer error — two signers fighting for the same
// catalog reference). Safe to call concurrently before main runs.
func RegisterSigner(s Signer) {
	signersMu.Lock()
	defer signersMu.Unlock()
	if _, ok := signers[s.Name()]; ok {
		panic(fmt.Sprintf("signer %q already registered", s.Name()))
	}
	signers[s.Name()] = s
}

func lookupSigner(name string) Signer {
	signersMu.RLock()
	defer signersMu.RUnlock()
	return signers[name]
}

// RegisteredSignerNames — for diagnostics; returns a stable sorted
// list of every signer name currently registered. Used by the
// `/healthz/details` surface so operators can confirm a new build
// shipped the signers their catalog references.
func RegisteredSignerNames() []string {
	signersMu.RLock()
	defer signersMu.RUnlock()
	out := make([]string, 0, len(signers))
	for k := range signers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ─── Composition ───────────────────────────────────────────────────

// runSigners executes a chain of signers in order against (req, body).
// Returns the final body bytes (possibly rewritten by one or more
// body-mutating signers) so the caller can re-attach them to req.
// Aborts on first error — partially-signed requests must never be sent.
func runSigners(ctx context.Context, req *http.Request, body []byte,
	creds map[string]string, specs []SignerSpec) ([]byte, error) {

	for _, spec := range specs {
		s := lookupSigner(spec.Name)
		if s == nil {
			return nil, fmt.Errorf("signer %q not registered (registered: %v)",
				spec.Name, RegisteredSignerNames())
		}
		newBody, err := s.Sign(ctx, req, body, creds, spec.Params)
		if err != nil {
			return nil, fmt.Errorf("signer %q: %w", spec.Name, err)
		}
		if newBody != nil {
			body = newBody
		}
	}
	return body, nil
}

// effectiveSigners returns the signer chain to apply for a given
// (app, tool). Per-tool override wins; otherwise app-level default;
// otherwise the legacy auth.types-based fallback (aws_sigv4 only —
// the one type that ever had a special path in the integration
// runner before this refactor).
//
// Returning nil means "no signing"; the caller skips runSigners
// entirely.
func effectiveSigners(app *AppTemplate, tool *AppToolDef) []SignerSpec {
	if tool != nil && tool.Signing != nil {
		return tool.Signing.Signers
	}
	if len(app.Auth.Signers) > 0 {
		return app.Auth.Signers
	}
	if hasAuthType(app.Auth.Types, "oauth1") && app.Auth.OAuth1 != nil {
		return []SignerSpec{{Name: "oauth1"}}
	}
	// Backwards compatibility — every existing aws_sigv4 catalog entry
	// declares auth.types=["aws_sigv4"] with the Service in
	// auth.aws_sigv4. Translate that to the registry call so the new
	// path matches the old behavior with no JSON change.
	if hasAuthType(app.Auth.Types, "aws_sigv4") {
		params := map[string]any{}
		if app.Auth.AwsSigV4 != nil {
			params["service"] = app.Auth.AwsSigV4.Service
		}
		return []SignerSpec{{Name: "aws_sigv4", Params: params}}
	}
	return nil
}
