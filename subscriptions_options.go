package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// subscriptionDeliveryOptions narrow when a webhook, polled or app-event
// subscription wakes its agent: payload filters, an expiry time, and
// once (the subscription is removed after its first delivered event).
type subscriptionDeliveryOptions struct {
	MatchJSON     string
	ExpiresAt     time.Time
	DeleteOnMatch bool
}

// subscriptionExpiryLayout matches SQLite's datetime('now'), so expiry
// can be compared in SQL as text.
const subscriptionExpiryLayout = "2006-01-02 15:04:05"

const maxSubscriptionLifetime = 366 * 24 * time.Hour

func (o subscriptionDeliveryOptions) expiresAtValue() string {
	if o.ExpiresAt.IsZero() {
		return ""
	}
	return o.ExpiresAt.UTC().Format(subscriptionExpiryLayout)
}

func (o subscriptionDeliveryOptions) applyTo(sub *Subscription) {
	sub.MatchJSON = o.MatchJSON
	sub.ExpiresAt = o.expiresAtValue()
	sub.DeleteOnMatch = o.DeleteOnMatch
	hydrateSubscriptionFilters(sub)
}

// parseSubscriptionDeliveryOptions reads filters, expires_at,
// expires_in_seconds and once from an agent's create_subscription call.
func parseSubscriptionDeliveryOptions(args map[string]any, now time.Time) (subscriptionDeliveryOptions, error) {
	var opts subscriptionDeliveryOptions
	var filters map[string]any
	switch v := args["filters"].(type) {
	case nil:
	case map[string]any:
		filters = v
	case string:
		if strings.TrimSpace(v) != "" {
			if err := json.Unmarshal([]byte(v), &filters); err != nil {
				return opts, errors.New("filters must be a JSON object")
			}
		}
	default:
		return opts, errors.New("filters must be an object")
	}
	if _, matchJSON, err := normalizeSubscriptionFilters(filters); err != nil {
		return opts, err
	} else {
		opts.MatchJSON = matchJSON
	}

	expiresAt, _ := args["expires_at"].(string)
	expiresAt = strings.TrimSpace(expiresAt)
	_, hasExpiresIn := args["expires_in_seconds"]
	if expiresAt != "" && hasExpiresIn && args["expires_in_seconds"] != nil {
		return opts, errors.New("pass expires_at or expires_in_seconds, not both")
	}
	if expiresAt != "" {
		at, err := time.Parse(time.RFC3339, expiresAt)
		if err != nil {
			return opts, errors.New("expires_at must be an RFC3339 time, for example 2026-12-01T09:00:00Z")
		}
		opts.ExpiresAt = at.UTC()
	} else if hasExpiresIn && args["expires_in_seconds"] != nil {
		seconds, err := parseIntArg(args["expires_in_seconds"])
		if err != nil || seconds <= 0 {
			return opts, errors.New("expires_in_seconds must be a positive integer")
		}
		if time.Duration(seconds)*time.Second > maxSubscriptionLifetime {
			return opts, fmt.Errorf("expires_in_seconds must be at most %d", int64(maxSubscriptionLifetime/time.Second))
		}
		opts.ExpiresAt = now.Add(time.Duration(seconds) * time.Second).UTC()
	}
	if !opts.ExpiresAt.IsZero() {
		if !opts.ExpiresAt.After(now) {
			return opts, errors.New("expires_at must be in the future")
		}
		if opts.ExpiresAt.Sub(now) > maxSubscriptionLifetime {
			return opts, errors.New("expires_at must be within a year")
		}
	}

	switch v := args["once"].(type) {
	case nil:
	case bool:
		opts.DeleteOnMatch = v
	case string:
		opts.DeleteOnMatch = strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return opts, errors.New("once must be a boolean")
	}
	return opts, nil
}

// ClaimSubscriptionDelivery takes a once subscription for one delivery
// by disabling it, so concurrent events cannot both be delivered. It
// reports false when another delivery already holds it.
func (s *Store) ClaimSubscriptionDelivery(id string) (bool, error) {
	res, err := s.dbExec(`UPDATE subscriptions SET enabled = 0 WHERE id = ? AND enabled = 1`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseSubscriptionClaim re-enables a once subscription whose claimed
// delivery failed, so the next event can try again.
func (s *Store) ReleaseSubscriptionClaim(id string) error {
	_, err := s.dbExec(`UPDATE subscriptions SET enabled = 1 WHERE id = ?`, id)
	return err
}

type expiredSubscriptionRef struct {
	UserID int64
	ID     string
}

// ListExpiredSubscriptions returns webhook, polled and app-event
// subscriptions past their expiry. Ephemeral app waits have their own cleanup.
func (s *Store) ListExpiredSubscriptions(limit int) ([]expiredSubscriptionRef, error) {
	rows, err := s.dbQuery(
		`SELECT user_id, id FROM subscriptions
		 WHERE COALESCE(kind,'user') != 'ephemeral'
		   AND COALESCE(expires_at,'') != ''
		   AND expires_at <= datetime('now')
		 ORDER BY expires_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []expiredSubscriptionRef
	for rows.Next() {
		var ref expiredSubscriptionRef
		if err := rows.Scan(&ref.UserID, &ref.ID); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// retireSubscription removes a subscription that expired or delivered
// its one event. The row goes first, so nothing more is delivered; the
// provider's webhook is unregistered in the background, as on delete.
func (s *Server) retireSubscription(userID int64, id, reason string) {
	sub, err := s.store.GetSubscription(userID, id)
	if err != nil || sub == nil {
		return
	}
	if err := s.store.DeleteSubscription(userID, id); err != nil {
		log.Printf("[SUB-RETIRE] sub=%s reason=%s: delete: %v", id, reason, err)
		return
	}
	log.Printf("[SUB-RETIRE] sub=%s reason=%s", id, reason)
	if sub.ExternalWebhookID != "" && sub.ConnectionID > 0 && s.catalog != nil {
		if conn, _, err := s.store.GetConnection(userID, sub.ConnectionID); err == nil && conn != nil {
			go s.unregisterUpstreamWebhook(conn, s.catalog.Get(conn.AppSlug), sub.ExternalWebhookID)
		}
	}
	s.notifySubscriptionChange("Subscription removed ("+reason+")", sub)
}

func (s *Server) retireExpiredSubscriptions() {
	refs, err := s.store.ListExpiredSubscriptions(100)
	if err != nil {
		log.Printf("[SUB-RETIRE] list expired: %v", err)
		return
	}
	for _, ref := range refs {
		s.retireSubscription(ref.UserID, ref.ID, "expired")
	}
}

// subscriptionDeliveryOptionsSummary describes a subscription's options
// for agents: in list results and in subscription notices.
func subscriptionDeliveryOptionsSummary(sub *Subscription) map[string]any {
	out := map[string]any{}
	if len(sub.Filters) > 0 {
		out["filters"] = sub.Filters
	}
	if sub.ExpiresAt != "" {
		if at, err := parseTime(sub.ExpiresAt); err == nil {
			out["expires_at"] = at.UTC().Format(time.RFC3339)
		}
	}
	if sub.DeleteOnMatch {
		out["once"] = true
	}
	return out
}
