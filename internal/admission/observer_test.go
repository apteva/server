package admission

import (
	"context"
	"fmt"
	"testing"
)

func TestParallelTransportDoesNotQueueOrRejectCardinality(t *testing.T) {
	observer := NewObserver()
	permits := []*Observation{}
	// Exceeds both the old execution ceiling and diagnostic tracking capacity.
	for i := 0; i < 1400; i++ {
		p, err := observer.Acquire(context.Background(), Request{Key: "same-install", Operation: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(i, err)
		}
		permits = append(permits, p)
	}
	s := observer.Snapshot()
	if s["active"] != 1400 || s["queued"] != 0 || len(s["operations"].([]map[string]any)) > 1024 {
		t.Fatal(s)
	}
	for _, p := range permits {
		p.Finish(Result{})
		p.Finish(Result{})
	}
	if observer.Snapshot()["active"] != 0 {
		t.Fatal("accounting leaked")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := observer.Acquire(canceled, Request{Key: "same-install"}); err != context.Canceled {
		t.Fatal(err)
	}
}
