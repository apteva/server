package main

// eip712_typed_data — sign a JSON sub-tree as an EIP-712 typed-data
// message and inject the signature back into the request body. Used by
// any integration where the wire-level request body itself must carry
// a wallet signature: Polymarket orders, dYdX v4 actions, Hyperliquid
// actions, Permit2 approvals, GMX, etc.
//
// Catalog params:
//
//   schema           (string, required) — name in the typed-data schema registry
//                                          (see eip712_schemas.go)
//   key_field        (string)           — credential holding the private key
//                                          (hex, with or without 0x). Default: "private_key"
//   body_path        (string)           — dot-path within the JSON body to the
//                                          message-to-sign. "" / "." → whole body.
//                                          Example: "order" → sign body.order
//   signature_field  (string)           — field within body_path's parent object
//                                          where the signature should be injected.
//                                          Default: "signature"
//   field_overrides  (map[string]string)— fields to fill from credentials before
//                                          signing. Values are "{{cred_key}}" templates
//                                          resolved against the connection's creds.
//                                          Example for Polymarket:
//                                            {"maker": "{{address}}", "signer": "{{address}}"}
//                                          Without this, the adapter would need to embed
//                                          the wallet address in the request body itself —
//                                          but the address lives only in server-side creds.
//
// The signer:
//   1. JSON-decodes the request body
//   2. Walks to body_path to pull out the message map
//   3. Looks up the schema, computes the EIP-712 digest
//   4. Signs the digest with secp256k1 (recoverable, v in {27, 28})
//   5. Hex-encodes the 65-byte sig with 0x prefix
//   6. Injects under signature_field next to the message
//   7. Re-marshals the body and returns the new bytes
//
// The runner re-attaches the new bytes to the request. Header-only
// signers (HMAC) that run AFTER this one will then HMAC over the
// post-signature body.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

func init() { RegisterSigner(&eip712Signer{}) }

type eip712Signer struct{}

func (eip712Signer) Name() string { return "eip712_typed_data" }

func (e eip712Signer) Sign(_ context.Context, _ *http.Request, body []byte,
	creds map[string]string, params map[string]any) ([]byte, error) {
	schemaName := stringParam(params, "schema", "")
	if schemaName == "" {
		return nil, fmt.Errorf("eip712_typed_data: params.schema required")
	}
	keyField := stringParam(params, "key_field", "private_key")
	bodyPath := stringParam(params, "body_path", "")
	sigField := stringParam(params, "signature_field", "signature")

	pkHex := creds[keyField]
	if pkHex == "" {
		return nil, fmt.Errorf("eip712_typed_data: missing credential %q", keyField)
	}

	schema, err := getTypedDataSchema(schemaName)
	if err != nil {
		return nil, fmt.Errorf("eip712_typed_data: %w", err)
	}

	// Decode JSON body. Empty body means "create an envelope around the
	// signed message" — supported, but the signer needs a body_path
	// of "" to know where to land.
	var doc map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("eip712_typed_data: body not JSON: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	// Walk to the message map at body_path. Path "" / "." → root.
	parent, leafKey, msg, err := walkToMessage(doc, bodyPath)
	if err != nil {
		return nil, fmt.Errorf("eip712_typed_data: walk body_path %q: %w", bodyPath, err)
	}

	// Apply field_overrides — replace named fields with values templated
	// from credentials. Required for protocols where the EIP-712 message
	// includes the wallet address (maker, signer fields on Polymarket
	// orders) because the integration runner's URL/header templating
	// doesn't reach into request bodies, and the adapter sidecar can't
	// see the connection's encrypted creds.
	if overrides, ok := params["field_overrides"].(map[string]any); ok {
		for field, tmpl := range overrides {
			if s, ok := tmpl.(string); ok {
				msg[field] = resolveTemplate(s, creds)
			}
		}
	}

	// Compute digest, sign, inject signature next to the message.
	digest, err := typedDataDigest(schema, msg)
	if err != nil {
		return nil, fmt.Errorf("eip712_typed_data: digest: %w", err)
	}
	sig, err := signDigestSecp256k1(digest, pkHex)
	if err != nil {
		return nil, fmt.Errorf("eip712_typed_data: sign: %w", err)
	}

	// Inject under the same parent as the message. For root-level
	// signing (body_path == "" and the JSON IS the message), we still
	// land the signature on the root document — produces a small wart
	// (signature as a top-level field) but matches what every protocol
	// I've seen actually wants.
	if leafKey == "" {
		doc[sigField] = sig
	} else {
		parent[sigField] = sig
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("eip712_typed_data: re-marshal: %w", err)
	}
	return out, nil
}

// walkToMessage descends `root` along dot-separated `path`. Returns
// the parent map (so the caller can sibling-set a signature), the
// leaf key under which the message lives in that parent, and the
// message itself. Empty / "." path means the root IS the message;
// parent is root, leafKey is "".
func walkToMessage(root map[string]any, path string) (parent map[string]any, leafKey string, msg map[string]any, err error) {
	path = strings.Trim(path, ".")
	if path == "" {
		return root, "", root, nil
	}
	parts := strings.Split(path, ".")
	cur := root
	for i, p := range parts {
		v, ok := cur[p]
		if !ok {
			return nil, "", nil, fmt.Errorf("path component %q not found", p)
		}
		if i == len(parts)-1 {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, "", nil, fmt.Errorf("leaf %q is %T, want map", p, v)
			}
			return cur, p, m, nil
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, "", nil, fmt.Errorf("intermediate %q is %T, want map", p, v)
		}
		cur = next
	}
	return nil, "", nil, fmt.Errorf("empty path") // unreachable
}

// signDigestSecp256k1 — Ethereum-style recoverable ECDSA signature over
// a 32-byte digest. Returns 0x-prefixed hex of (r || s || v) where v
// is 27 or 28 (Ethereum's convention; raw secp256k1 recovery yields
// 0 or 1, +27).
func signDigestSecp256k1(digest []byte, hexKey string) (string, error) {
	if len(digest) != 32 {
		return "", fmt.Errorf("digest must be 32 bytes, got %d", len(digest))
	}
	hexKey = strings.TrimPrefix(strings.TrimPrefix(hexKey, "0x"), "0X")
	keyBytes, err := hex.DecodeString(hexKey)
	if err != nil {
		return "", fmt.Errorf("decode private key: %w", err)
	}
	if len(keyBytes) != 32 {
		return "", fmt.Errorf("private key must be 32 bytes, got %d", len(keyBytes))
	}
	priv := secp256k1.PrivKeyFromBytes(keyBytes)
	compact := ecdsa.SignCompact(priv, digest, false)
	// dcrec returns compact as [recoveryID+27, r(32), s(32)] — Bitcoin
	// convention. Ethereum wants [r(32), s(32), v(1)] with v in {27, 28}.
	// Reorder.
	if len(compact) != 65 {
		return "", fmt.Errorf("unexpected compact sig length: %d", len(compact))
	}
	recoveryByte := compact[0] // already 27 or 28 (dcrec uses Bitcoin's +27 convention)
	r := compact[1:33]
	s := compact[33:65]

	out := make([]byte, 65)
	copy(out[0:32], r)
	copy(out[32:64], s)
	out[64] = recoveryByte

	return "0x" + hex.EncodeToString(out), nil
}
