package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

type DatabaseIdentity struct {
	Version    string `json:"version"`
	SchemaHash string `json:"schema_hash"`
	SecretHash string `json:"secret_hash"`
}

func (h *Host) diskIdentity(ctx context.Context, c Candidate) (DatabaseIdentity, error) {
	var identity DatabaseIdentity
	cmd := exec.CommandContext(ctx, c.Binary, "--host-check-schema")
	cmd.Env = overrides(os.Environ(), map[string]string{"APTEVA_HOME": h.cfg.Home, "DATA_DIR": h.cfg.DataDir, "DB_PATH": h.cfg.DBPath, "APTEVA_HOST_SLOT_SOCKET": "", "APTEVA_HOST_SOCKET": ""})
	output, err := cmd.Output()
	if err != nil {
		return identity, fmt.Errorf("cannot validate the current database and key: %w", err)
	}
	if err = json.Unmarshal(output, &identity); err != nil {
		return identity, err
	}
	if identity.Version != c.Version || identity.SchemaHash == "" || identity.SecretHash == "" {
		return identity, fmt.Errorf("incomplete database identity")
	}
	return identity, nil
}
func (h *Host) prepare(ctx context.Context, s *slot) error {
	if !s.candidate.ExistingSchema {
		return nil
	}
	client := UnixClient(s.socket)
	defer client.CloseIdleConnections()
	return Call(ctx, client, "POST", "/prepare", DatabaseIdentity{SchemaHash: s.candidate.SchemaHash, SecretHash: s.candidate.SecretHash}, nil)
}
