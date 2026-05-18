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
	service, _ := params["service"].(string)
	return nil, signAWSSigV4(
		req,
		creds["access_key_id"],
		creds["secret_access_key"],
		creds["session_token"],
		creds["region"],
		service,
		body,
	)
}
