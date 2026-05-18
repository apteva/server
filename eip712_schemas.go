package main

// Typed-data schema registry. Each schema is bound to a (chain id,
// verifying contract) tuple — those define the domain separator,
// which is itself part of the digest. Schemas live here as Go literals
// rather than catalog JSON because the protocol-specific field shapes
// rarely change (when they do, it's tied to a contract upgrade that
// also needs Go-level review).
//
// Catalog declaration references a schema by name:
//
//   {
//     "name": "eip712_typed_data",
//     "params": {
//       "schema":          "polymarket_order_v1",
//       "body_path":       "order",
//       "key_field":       "private_key",
//       "signature_field": "signature"
//     }
//   }
//
// To add a new protocol: append an entry below. The signer auto-picks
// it up via getTypedDataSchema(name).

import (
	"fmt"
	"sync"
)

var (
	schemasMu sync.RWMutex
	schemas   = map[string]TypedDataSchema{
		"polymarket_order_v1":          polymarketOrderSchemaV1(),
		"polymarket_order_negrisk_v1":  polymarketNegRiskOrderSchemaV1(),
	}
)

func getTypedDataSchema(name string) (TypedDataSchema, error) {
	schemasMu.RLock()
	defer schemasMu.RUnlock()
	s, ok := schemas[name]
	if !ok {
		out := []string{}
		for k := range schemas {
			out = append(out, k)
		}
		return TypedDataSchema{}, fmt.Errorf("typed-data schema %q not registered (known: %v)", name, out)
	}
	return s, nil
}

// polymarketOrderSchemaV1 — Polymarket's standard CTF Exchange Order
// type. Used for "single-outcome" markets (the standard YES/NO binary
// case). Multi-outcome negative-risk markets use a different verifying
// contract and a slightly different domain name — see
// polymarketNegRiskOrderSchemaV1.
//
// Contract addresses come from Polymarket's published constants:
//   https://docs.polymarket.com/developers/CLOB/orders/orders
// Domain values must match the contract's EIP712Domain exactly; any
// drift produces a different domain separator and rejected signatures.
func polymarketOrderSchemaV1() TypedDataSchema {
	return TypedDataSchema{
		Domain: EIP712Domain{
			Name:              "Polymarket CTF Exchange",
			Version:           "1",
			ChainID:           137, // Polygon mainnet
			VerifyingContract: "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E",
		},
		PrimaryType: "Order",
		Types: map[string][]EIP712Field{
			"Order": {
				{Name: "salt", Type: "uint256"},
				{Name: "maker", Type: "address"},
				{Name: "signer", Type: "address"},
				{Name: "taker", Type: "address"},
				{Name: "tokenId", Type: "uint256"},
				{Name: "makerAmount", Type: "uint256"},
				{Name: "takerAmount", Type: "uint256"},
				{Name: "expiration", Type: "uint256"},
				{Name: "nonce", Type: "uint256"},
				{Name: "feeRateBps", Type: "uint256"},
				{Name: "side", Type: "uint8"},
				{Name: "signatureType", Type: "uint8"},
			},
		},
	}
}

// polymarketNegRiskOrderSchemaV1 — same Order struct shape, different
// verifying contract + domain name. The negative-risk exchange handles
// markets where multiple outcomes are exclusive (e.g. presidential
// election with five candidates); each candidate is a separate
// position but resolution is mutually exclusive on-chain.
func polymarketNegRiskOrderSchemaV1() TypedDataSchema {
	s := polymarketOrderSchemaV1()
	s.Domain.Name = "Polymarket CTF Exchange"
	s.Domain.VerifyingContract = "0xC5d563A36AE78145C45a50134d48A1215220f80a"
	return s
}
