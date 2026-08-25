package main

import (
	"log"
	"time"
)

const (
	delegatedAPIKeyRetention = 24 * time.Hour
	delegatedAPIKeySweep     = time.Hour
)

// CleanInactiveDelegatedAPIKeys physically removes delegated credentials only
// after they have been unusable for the retention period. Authentication and
// CORS reject them immediately at expiry/revocation; the delay keeps a small
// audit window without allowing this high-churn credential class to grow
// without bound.
func (s *Store) CleanInactiveDelegatedAPIKeys(now time.Time, retention time.Duration) (int64, error) {
	cutoff := now.UTC().Add(-retention).Format("2006-01-02 15:04:05")
	result, err := s.db.Exec(`
		DELETE FROM api_keys
		 WHERE kind='delegated_user'
		   AND ((revoked_at IS NOT NULL AND datetime(revoked_at) <= datetime(?))
		     OR (expires_at IS NOT NULL AND datetime(expires_at) <= datetime(?)))`, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Server) startDelegatedAPIKeyRetention() {
	cleanup := func() {
		deleted, err := s.store.CleanInactiveDelegatedAPIKeys(time.Now(), delegatedAPIKeyRetention)
		if err != nil {
			log.Printf("[AUTH] delegated key cleanup failed: %v", err)
			return
		}
		if deleted > 0 {
			log.Printf("[AUTH] delegated key cleanup removed %d credential(s)", deleted)
		}
	}
	cleanup()
	ticker := time.NewTicker(delegatedAPIKeySweep)
	defer ticker.Stop()
	for range ticker.C {
		cleanup()
	}
}
