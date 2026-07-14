package main

import (
	"path/filepath"
	"testing"
)

func TestFreshSchemaOmitsLegacyEvaluationTables(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "apteva.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, table := range []string{"agent_evals", "agent_eval_runs", "agent_directive_history"} {
		if tableExists(store.db, table) {
			t.Errorf("fresh schema still creates legacy table %q", table)
		}
	}
}
