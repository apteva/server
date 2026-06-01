package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// TestEIP712_SpecVector — the canonical "Mail" example from the
// EIP-712 spec (https://eips.ethereum.org/EIPS/eip-712). If this test
// passes, the encoder agrees with every other compliant implementation
// on: struct dependency ordering, address padding, integer encoding,
// dynamic-bytes hashing, and domain separator computation.
//
// Reference digest is the hashStruct(mail) ++ domainSeparator pipeline
// from the spec's "Example" section.
//
//	domainSeparator: 0xf2cee375fa42b42143804025fc449deafd50cc031ca257e0b194a650a912090f
//	hashStruct(message): 0xc52c0ee5d84264471806290a3f2c4cecfc5490626bf912d01f240d7a274b371e
//	digest: 0xbe609aee343fb3c4b28e1df9e632fca64fcfaede20f02e86244efddf30957bd2
func TestEIP712_SpecVector(t *testing.T) {
	schema := TypedDataSchema{
		Domain: EIP712Domain{
			Name:              "Ether Mail",
			Version:           "1",
			ChainID:           1,
			VerifyingContract: "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
		},
		PrimaryType: "Mail",
		Types: map[string][]EIP712Field{
			"Person": {
				{Name: "name", Type: "string"},
				{Name: "wallet", Type: "address"},
			},
			"Mail": {
				{Name: "from", Type: "Person"},
				{Name: "to", Type: "Person"},
				{Name: "contents", Type: "string"},
			},
		},
	}
	message := map[string]any{
		"from": map[string]any{
			"name":   "Cow",
			"wallet": "0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826",
		},
		"to": map[string]any{
			"name":   "Bob",
			"wallet": "0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB",
		},
		"contents": "Hello, Bob!",
	}

	digest, err := typedDataDigest(schema, message)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	const want = "be609aee343fb3c4b28e1df9e632fca64fcfaede20f02e86244efddf30957bd2"
	got := hex.EncodeToString(digest)
	if got != want {
		t.Errorf("EIP-712 digest mismatch\n got:  0x%s\n want: 0x%s", got, want)
	}
}

// TestEIP712_TypeHash_Mail proves encodeType produces the exact
// canonical string the spec specifies. Dependent types come sorted
// alphabetically after the primary; the spec gives this concrete
// example so we can pin it as a regression test.
func TestEIP712_TypeHash_Mail(t *testing.T) {
	types := map[string][]EIP712Field{
		"Person": {
			{Name: "name", Type: "string"},
			{Name: "wallet", Type: "address"},
		},
		"Mail": {
			{Name: "from", Type: "Person"},
			{Name: "to", Type: "Person"},
			{Name: "contents", Type: "string"},
		},
	}
	got := encodeType("Mail", types)
	const want = "Mail(Person from,Person to,string contents)Person(string name,address wallet)"
	if got != want {
		t.Errorf("encodeType wrong\n got:  %q\n want: %q", got, want)
	}
}

// TestEIP712_PolymarketOrderRoundtrip — sign a Polymarket-shaped order
// with a fixed private key + message, then assert the signer:
//  1. Computes the same digest twice (deterministic)
//  2. Returns a 65-byte 0x-hex signature
//  3. v byte is 27 or 28 (Ethereum recovery convention, not 0/1)
//  4. Injects the signature next to the order in the body
//
// We don't pin the signature value because ECDSA k is deterministic
// per the dcrec implementation but the convention isn't universal —
// pinning would couple this test to a specific library version. The
// structural checks catch the failure modes we actually care about.
func TestEIP712_PolymarketOrderRoundtrip(t *testing.T) {
	// Fixed test key — DO NOT USE FOR REAL FUNDS. 32 random bytes
	// chosen for readability.
	const testPK = "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"

	body := mustJSON(map[string]any{
		"order": map[string]any{
			"salt":          "12345",
			"maker":         "0x0000000000000000000000000000000000000001",
			"signer":        "0x0000000000000000000000000000000000000001",
			"taker":         "0x0000000000000000000000000000000000000000",
			"tokenId":       "100000000000000000000000000000000000000000000000000000000000001",
			"makerAmount":   "10000000",
			"takerAmount":   "20000000",
			"expiration":    "0",
			"nonce":         "1",
			"feeRateBps":    "0",
			"side":          "0", // BUY
			"signatureType": "0", // EOA
		},
	})

	creds := map[string]string{"private_key": testPK}
	params := map[string]any{
		"schema":          "polymarket_order_v1",
		"body_path":       "order",
		"key_field":       "private_key",
		"signature_field": "signature",
	}

	newBody, err := (eip712Signer{}).Sign(context.Background(), nil, body, creds, params)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(newBody, &decoded); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	order, ok := decoded["order"].(map[string]any)
	if !ok {
		t.Fatal("rewritten body missing order")
	}
	sig, ok := decoded["signature"].(string)
	if !ok {
		t.Fatal("rewritten body missing signature field at body root")
	}
	if !strings.HasPrefix(sig, "0x") {
		t.Errorf("signature missing 0x prefix: %q", sig)
	}
	sigBytes, err := hex.DecodeString(strings.TrimPrefix(sig, "0x"))
	if err != nil {
		t.Fatalf("signature hex: %v", err)
	}
	if len(sigBytes) != 65 {
		t.Errorf("signature wrong length: got %d, want 65", len(sigBytes))
	}
	v := sigBytes[64]
	if v != 27 && v != 28 {
		t.Errorf("v byte wrong: got %d, want 27 or 28", v)
	}

	// The order itself should be untouched — only the signature is new.
	if order["salt"] != "12345" || order["side"] != "0" {
		t.Errorf("eip712 mutated order body unexpectedly: %+v", order)
	}

	// Determinism — signing twice produces the same signature for the
	// same digest + key. (dcrec uses RFC 6979 deterministic k.)
	newBody2, err := (eip712Signer{}).Sign(context.Background(), nil, body, creds, params)
	if err != nil {
		t.Fatalf("sign again: %v", err)
	}
	var decoded2 map[string]any
	json.Unmarshal(newBody2, &decoded2)
	if decoded2["signature"] != sig {
		t.Errorf("nondeterministic signature\n first:  %s\n second: %s", sig, decoded2["signature"])
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
