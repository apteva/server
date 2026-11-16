package sshwatch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SignPayload returns the X-Apteva-Signature value for body.
func SignPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks an X-Apteva-Signature header in constant time.
func VerifySignature(secret string, body []byte, header string) bool {
	return hmac.Equal([]byte(SignPayload(secret, body)), []byte(header))
}

func deliveryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 10 {
		return time.Hour
	}
	d := 5 * time.Second << (attempt - 1)
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// deliveryLoop runs only in the lock holder, so a given outbox row has one
// sender. Rows stay queued (and retried with backoff across restarts) until a
// 2xx is recorded; with no webhook configured they simply accumulate.
func (c *Collector) deliveryLoop(ctx context.Context, st *Store) {
	for {
		c.deliver(ctx, st)
		if !sleepCtx(ctx, c.cfg.DeliveryInterval) {
			return
		}
	}
}

func (c *Collector) deliver(ctx context.Context, st *Store) {
	p := c.currentPolicy()
	if p.AlertWebhookURL == "" {
		return
	}
	rows, err := st.dueAlerts(ctx, c.now(), 50)
	if err != nil {
		if ctx.Err() == nil {
			c.logf("alert outbox: %v", err)
		}
		return
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			return
		}
		sendErr := c.post(p, r)
		// Bookkeeping must survive shutdown so a delivered row is not resent.
		bk, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		now := c.now()
		if sendErr == nil {
			err = st.markDelivered(bk, r.Key, now)
		} else {
			err = st.markFailed(bk, r.Key, now.Add(deliveryBackoff(r.Attempts+1)), sendErr.Error())
		}
		cancel()
		if err != nil {
			c.logf("alert outbox update: %v", err)
			return
		}
		if sendErr != nil {
			// Keep order and avoid hammering a failing receiver.
			return
		}
	}
}

func (c *Collector) post(p Policy, r outboxRow) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.AlertWebhookURL, bytes.NewReader(r.Payload))
	if err != nil {
		return errors.New("invalid webhook URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "apteva-ssh-collector")
	req.Header.Set(HeaderIdempotencyKey, r.Key)
	if id, _, ok := strings.Cut(r.Key, ":"); ok {
		req.Header.Set(HeaderEventID, id)
	}
	if p.AlertWebhookSecret != "" {
		req.Header.Set(HeaderSignature, SignPayload(p.AlertWebhookSecret, r.Payload))
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		// Never echo the URL (it may carry a token) into health output.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("webhook delivery failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
