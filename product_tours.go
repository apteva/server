package main

import (
	"encoding/json"
	"fmt"
	"regexp"
)

type productTourUpdate struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

var productTourID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)

func (s *Store) GetUserProductTours(userID int64) map[string]string {
	var raw string
	tours := map[string]string{}
	if err := s.db.QueryRow("SELECT product_tours FROM user_preferences WHERE user_id=?", userID).Scan(&raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &tours)
	}
	if tours == nil {
		return map[string]string{}
	}
	return tours
}

func (s *Store) SetUserProductTour(userID int64, update productTourUpdate) error {
	if !productTourID.MatchString(update.ID) || (update.Status != "skipped" && update.Status != "completed") {
		return fmt.Errorf("invalid product tour preference")
	}
	// Patch one tour atomically, preserving other preferences and tour histories.
	_, err := s.db.Exec(`INSERT INTO user_preferences (user_id, product_tours, updated_at)
 VALUES (?, json_object(?, ?), CURRENT_TIMESTAMP)
 ON CONFLICT(user_id) DO UPDATE SET product_tours=json_set(user_preferences.product_tours, ?, ?), updated_at=CURRENT_TIMESTAMP`,
		userID, update.ID, update.Status, `$."`+update.ID+`"`, update.Status)
	return err
}
