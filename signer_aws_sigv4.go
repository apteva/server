package main

// aws_sigv4 — registry-facing wrapper around the existing signAWSSigV4
// function in sigv4.go. Behavior is identical to the pre-registry
// inline branch in connections.go; this file exists only so the runner
// can dispatch via the Signer registry like every other auth scheme.
//
// Catalog declaration (unchanged from before — the legacy
// auth.types=["aws_sigv4"] form is translated by effectiveSigners):
//
//   {
//     "auth": {
//       "signers": [
//         {"name": "aws_sigv4", "params": {"service": "ses"}}
//       ]
//     }
//   }
//
// Params:
//   service (string, required) — the AWS service name (ses, s3, sns, …).
//   access_key_field, secret_key_field, session_token_field, region_field —
//   optional connection credential field overrides.
//   access_key_input, secret_key_input, region_input — optional tool input
//   field names. Input values are exposed to signers with the internal
//   signerInputPrefix by executeIntegrationTool.
//
// Credentials read from the connection's decrypted blob:
//   access_key_id, secret_access_key, region (required)
//   session_token (optional; for STS-issued temporary creds)

import (
	"context"
	"net/http"
)

func init() { RegisterSigner(&awsSigV4Signer{}) }

type awsSigV4Signer struct{}

func (awsSigV4Signer) Name() string { return "aws_sigv4" }

func (awsSigV4Signer) Sign(_ context.Context, req *http.Request, body []byte,
	creds map[string]string, params map[string]any) ([]byte, error) {
	service := signerStringParam(params, "service", "")
	accessKeyField := signerStringParam(params, "access_key_field", "access_key_id")
	secretKeyField := signerStringParam(params, "secret_key_field", "secret_access_key")
	sessionTokenField := signerStringParam(params, "session_token_field", "session_token")
	regionField := signerStringParam(params, "region_field", "region")

	accessKey := creds[accessKeyField]
	if input := signerStringParam(params, "access_key_input", ""); input != "" {
		accessKey = creds[signerInputPrefix+input]
	}
	secretKey := creds[secretKeyField]
	if input := signerStringParam(params, "secret_key_input", ""); input != "" {
		secretKey = creds[signerInputPrefix+input]
	}
	region := creds[regionField]
	if input := signerStringParam(params, "region_input", ""); input != "" {
		region = creds[signerInputPrefix+input]
	}
	return nil, signAWSSigV4(
		req,
		accessKey,
		secretKey,
		creds[sessionTokenField],
		region,
		service,
		body,
	)
}

func signerStringParam(params map[string]any, name, fallback string) string {
	if value, ok := params[name].(string); ok && value != "" {
		return value
	}
	return fallback
}
