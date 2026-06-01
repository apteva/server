package main

// EIP-712 typed-data encoding (Ethereum Improvement Proposal 712).
// Hand-written to avoid pulling in go-ethereum (~30MB of deps for code
// we don't need); the algorithm is small (~300 LOC) and well-specified.
//
// What this file does:
//
//   typedDataDigest(schema, message) → 32-byte digest
//
// Where the digest is:
//
//   keccak256(0x1901 || domainSeparator || hashStruct(primaryType, message))
//
// Signing the digest with a secp256k1 key produces a 65-byte
// (r || s || v) signature that Polymarket's CLOB Exchange / dYdX v4 /
// Hyperliquid / Permit2 / etc. verify on-chain.
//
// Conformance: the canonical "Mail" example from the EIP-712 spec is
// covered by TestEIP712_SpecVector in eip712_test.go — that's the
// reference vector every typed-data implementation must match exactly.
// Subtle encoding bugs (struct dependency ordering, dynamic-bytes
// hashing, address padding) all surface immediately if the digest
// drifts from `0xbe609aee343fb3c4b28e1df9e632fca64fcfaede20f02e86244efddf30957bd2`.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/sha3"
)

// TypedDataSchema — the static part of an EIP-712 payload, shared by
// every message of the same type. Bound to a domain (chain id +
// verifying contract); domains differ per chain + per protocol. Loaded
// from typedDataSchemas() in eip712_schemas.go.
type TypedDataSchema struct {
	Domain      EIP712Domain
	Types       map[string][]EIP712Field
	PrimaryType string
}

type EIP712Domain struct {
	Name              string
	Version           string
	ChainID           int64
	VerifyingContract string // 0x-prefixed hex, 20 bytes
	Salt              string // 0x-prefixed hex, 32 bytes; usually empty
}

type EIP712Field struct {
	Name string
	Type string // "uint256", "address", "bool", "bytes32", "string", "bytes", "FooStruct", "uint256[]", ...
}

// typedDataDigest computes the EIP-712 digest:
//
//	keccak256(0x1901 || domainSeparator || hashStruct(primaryType, msg))
//
// `msg` is a generic map[string]any decoded from the request body at
// the configured body_path. Numeric values may arrive as float64 (JSON
// default), int, int64, *big.Int, or string-of-decimal; addresses /
// bytes as 0x-prefixed strings. The encoder coerces to the right
// fixed-width form per field type.
func typedDataDigest(schema TypedDataSchema, msg map[string]any) ([]byte, error) {
	// 1. domainSeparator = hashStruct("EIP712Domain", domain)
	domainMsg, domainFields := domainMessage(schema.Domain)
	// Inject the domain type so encodeData can resolve EIP712Domain like
	// any other struct. Caller's schema may or may not have included it.
	allTypes := cloneTypes(schema.Types)
	allTypes["EIP712Domain"] = domainFields

	domainSep, err := hashStruct("EIP712Domain", domainMsg, allTypes)
	if err != nil {
		return nil, fmt.Errorf("eip712 domain separator: %w", err)
	}

	// 2. messageHash = hashStruct(primaryType, msg)
	msgHash, err := hashStruct(schema.PrimaryType, msg, allTypes)
	if err != nil {
		return nil, fmt.Errorf("eip712 message hash (type=%s): %w", schema.PrimaryType, err)
	}

	// 3. digest = keccak256(0x19 || 0x01 || domainSep || msgHash)
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte{0x19, 0x01})
	h.Write(domainSep)
	h.Write(msgHash)
	return h.Sum(nil), nil
}

// hashStruct(typeName, data) = keccak256(typeHash(typeName) || encodeData(typeName, data))
func hashStruct(typeName string, data map[string]any, types map[string][]EIP712Field) ([]byte, error) {
	th := typeHash(typeName, types)
	enc, err := encodeData(typeName, data, types)
	if err != nil {
		return nil, err
	}
	h := sha3.NewLegacyKeccak256()
	h.Write(th)
	h.Write(enc)
	return h.Sum(nil), nil
}

// typeHash(typeName) = keccak256(encodeType(typeName)). Deterministic
// — same type definitions always produce the same hash.
func typeHash(typeName string, types map[string][]EIP712Field) []byte {
	encoded := encodeType(typeName, types)
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(encoded))
	return h.Sum(nil)
}

// encodeType emits the canonical type-definition string. Format:
//
//	PrimaryType(field1 type1,field2 type2,...)
//	followed by every dependent struct's definition, sorted by name.
//
// Dependencies are gathered transitively; arrays of structs (Foo[])
// count as depending on Foo.
func encodeType(typeName string, types map[string][]EIP712Field) string {
	deps := map[string]bool{}
	gatherDeps(typeName, types, deps)
	delete(deps, typeName) // primary always first
	depNames := make([]string, 0, len(deps))
	for d := range deps {
		depNames = append(depNames, d)
	}
	sort.Strings(depNames)

	var b strings.Builder
	b.WriteString(typeDef(typeName, types))
	for _, d := range depNames {
		b.WriteString(typeDef(d, types))
	}
	return b.String()
}

func typeDef(name string, types map[string][]EIP712Field) string {
	fields := types[name]
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('(')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(f.Type)
		b.WriteByte(' ')
		b.WriteString(f.Name)
	}
	b.WriteByte(')')
	return b.String()
}

// gatherDeps walks struct fields recursively, collecting every custom
// type referenced (directly or via arrays). Atomic types are skipped.
func gatherDeps(typeName string, types map[string][]EIP712Field, seen map[string]bool) {
	if seen[typeName] {
		return
	}
	seen[typeName] = true
	for _, f := range types[typeName] {
		bare := strings.TrimSuffix(strings.TrimSuffix(f.Type, "]"), "[")
		// Strip a fixed-size array suffix like "uint256[3]" too. We treat
		// anything inside brackets as opaque — only the element type
		// matters for dep gathering.
		if i := strings.IndexByte(bare, '['); i >= 0 {
			bare = bare[:i]
		}
		if _, ok := types[bare]; ok {
			gatherDeps(bare, types, seen)
		}
	}
}

// encodeData encodes each field of a struct per its declared type and
// concatenates. Each encoded field is exactly 32 bytes (the EIP-712
// encoding rule for the inner concatenation; dynamic types are hashed
// down to a 32-byte digest before insertion).
func encodeData(typeName string, data map[string]any, types map[string][]EIP712Field) ([]byte, error) {
	fields := types[typeName]
	if fields == nil {
		return nil, fmt.Errorf("unknown type %q", typeName)
	}
	out := make([]byte, 0, 32*len(fields))
	for _, f := range fields {
		val, ok := data[f.Name]
		if !ok {
			// Allow missing fields → encode as zero. EIP-712 doesn't
			// strictly require this but it's lenient with optional
			// fields like `salt` on the domain.
			val = nil
		}
		enc, err := encodeValue(f.Type, val, types)
		if err != nil {
			return nil, fmt.Errorf("field %s (%s): %w", f.Name, f.Type, err)
		}
		out = append(out, enc...)
	}
	return out, nil
}

// encodeValue produces the 32-byte encoding of a single value.
//
// Atomic types: pad-left or pad-right to 32 bytes per Solidity ABI.
// Dynamic types (bytes, string): keccak256(value bytes).
// Structs: hashStruct of the nested struct.
// Arrays: keccak256(concat(encoded elements)).
func encodeValue(typeStr string, val any, types map[string][]EIP712Field) ([]byte, error) {
	// Array? Recurse over the element type.
	if i := strings.IndexByte(typeStr, '['); i >= 0 {
		elemType := typeStr[:i]
		arr, ok := val.([]any)
		if !ok {
			// nil → empty array.
			arr = nil
		}
		buf := make([]byte, 0, 32*len(arr))
		for j, item := range arr {
			enc, err := encodeValue(elemType, item, types)
			if err != nil {
				return nil, fmt.Errorf("array element %d: %w", j, err)
			}
			buf = append(buf, enc...)
		}
		h := sha3.NewLegacyKeccak256()
		h.Write(buf)
		return h.Sum(nil), nil
	}

	// Struct? hashStruct.
	if _, isStruct := types[typeStr]; isStruct {
		nested, _ := val.(map[string]any)
		if nested == nil {
			nested = map[string]any{}
		}
		return hashStruct(typeStr, nested, types)
	}

	// Atomic types.
	switch {
	case typeStr == "string":
		s, _ := val.(string)
		h := sha3.NewLegacyKeccak256()
		h.Write([]byte(s))
		return h.Sum(nil), nil

	case typeStr == "bytes":
		b, err := bytesFromValue(val)
		if err != nil {
			return nil, err
		}
		h := sha3.NewLegacyKeccak256()
		h.Write(b)
		return h.Sum(nil), nil

	case typeStr == "address":
		return padAddress(val)

	case typeStr == "bool":
		out := make([]byte, 32)
		if b, _ := val.(bool); b {
			out[31] = 1
		}
		return out, nil

	case strings.HasPrefix(typeStr, "uint") || strings.HasPrefix(typeStr, "int"):
		return padInteger(val, typeStr)

	case strings.HasPrefix(typeStr, "bytes") && typeStr != "bytes":
		// Fixed-size bytesN (1..32) — left-padded to 32 (actually right-
		// aligned to the declared size, but EIP-712 spec mandates
		// left-aligned for bytesN). We left-align and zero-fill the
		// trailing bytes up to 32.
		n, err := strconv.Atoi(typeStr[len("bytes"):])
		if err != nil || n < 1 || n > 32 {
			return nil, fmt.Errorf("bad fixed bytes type %q", typeStr)
		}
		b, err := bytesFromValue(val)
		if err != nil {
			return nil, err
		}
		if len(b) > n {
			return nil, fmt.Errorf("bytes%d value too long: %d bytes", n, len(b))
		}
		out := make([]byte, 32)
		copy(out, b) // left-aligned
		return out, nil
	}

	return nil, fmt.Errorf("unsupported type %q", typeStr)
}

// ─── Value coercion ─────────────────────────────────────────────────

// padInteger encodes uint*/int* as 32-byte big-endian. Accepts:
//
//	string  — decimal or 0x-hex
//	float64 — must be exactly integral (JSON number path)
//	int / int64 / uint64 — direct
//	*big.Int — direct
//	nil → 0
func padInteger(v any, typeStr string) ([]byte, error) {
	signed := strings.HasPrefix(typeStr, "int")
	n := new(big.Int)
	switch x := v.(type) {
	case nil:
		// zero
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			// zero
		} else if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			if _, ok := n.SetString(s[2:], 16); !ok {
				return nil, fmt.Errorf("bad hex int %q", x)
			}
		} else {
			if _, ok := n.SetString(s, 10); !ok {
				return nil, fmt.Errorf("bad decimal int %q", x)
			}
		}
	case float64:
		if x != float64(int64(x)) {
			return nil, fmt.Errorf("non-integer float %v for %s", x, typeStr)
		}
		n.SetInt64(int64(x))
	case int:
		n.SetInt64(int64(x))
	case int64:
		n.SetInt64(x)
	case uint64:
		n.SetUint64(x)
	case *big.Int:
		if x != nil {
			n.Set(x)
		}
	default:
		return nil, fmt.Errorf("can't encode %T as %s", v, typeStr)
	}

	if !signed && n.Sign() < 0 {
		return nil, fmt.Errorf("%s must be non-negative, got %s", typeStr, n.String())
	}

	// Two's complement for signed negatives.
	if signed && n.Sign() < 0 {
		// 256-bit two's complement: 2^256 + n
		twoPow256 := new(big.Int).Lsh(big.NewInt(1), 256)
		n.Add(twoPow256, n)
	}
	out := make([]byte, 32)
	b := n.Bytes()
	if len(b) > 32 {
		return nil, fmt.Errorf("integer overflow: %s requires %d bytes", n.String(), len(b))
	}
	copy(out[32-len(b):], b)
	return out, nil
}

// padAddress encodes a 20-byte Ethereum address as 32 bytes (12 zero
// bytes of left padding, then the 20 address bytes). Accepts 0x-prefixed
// hex string or nil (→ zero address).
func padAddress(v any) ([]byte, error) {
	out := make([]byte, 32)
	s, _ := v.(string)
	if s == "" {
		return out, nil
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) != 40 {
		return nil, fmt.Errorf("address must be 20 hex bytes, got %d chars: %q", len(s), s)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("address hex decode: %w", err)
	}
	copy(out[12:], b) // left-pad with 12 zero bytes
	return out, nil
}

// bytesFromValue accepts 0x-hex strings, base64 strings (with explicit
// "base64:" prefix to disambiguate), and []byte.
func bytesFromValue(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return x, nil
	case string:
		switch {
		case strings.HasPrefix(x, "0x") || strings.HasPrefix(x, "0X"):
			return hex.DecodeString(x[2:])
		case x == "":
			return nil, nil
		default:
			// Treat plain string as utf-8 bytes — matches how Polymarket /
			// most EIP-712 payloads pass nonces or arbitrary strings.
			return []byte(x), nil
		}
	}
	return nil, fmt.Errorf("can't read bytes from %T", v)
}

// domainMessage emits a (data, fields) pair representing the domain
// struct. Only the populated fields are emitted as Fields — EIP-712's
// EIP712Domain definition itself varies by which fields are present
// (this is part of the spec: a domain without `salt` has a different
// type hash than one with `salt`, even if salt would be zero).
func domainMessage(d EIP712Domain) (map[string]any, []EIP712Field) {
	data := map[string]any{}
	var fields []EIP712Field
	if d.Name != "" {
		data["name"] = d.Name
		fields = append(fields, EIP712Field{Name: "name", Type: "string"})
	}
	if d.Version != "" {
		data["version"] = d.Version
		fields = append(fields, EIP712Field{Name: "version", Type: "string"})
	}
	if d.ChainID != 0 {
		data["chainId"] = d.ChainID
		fields = append(fields, EIP712Field{Name: "chainId", Type: "uint256"})
	}
	if d.VerifyingContract != "" {
		data["verifyingContract"] = d.VerifyingContract
		fields = append(fields, EIP712Field{Name: "verifyingContract", Type: "address"})
	}
	if d.Salt != "" {
		data["salt"] = d.Salt
		fields = append(fields, EIP712Field{Name: "salt", Type: "bytes32"})
	}
	return data, fields
}

func cloneTypes(in map[string][]EIP712Field) map[string][]EIP712Field {
	out := make(map[string][]EIP712Field, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// padTo32 — used by tests; left-pads a byte slice to 32 bytes.
func padTo32(b []byte) []byte {
	if len(b) >= 32 {
		return b[:32]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// errUnused keeps `errors` imported for future helper functions.
var errUnused = errors.New("unused")
var _ = errUnused
