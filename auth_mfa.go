package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	mfaPendingDuration = 5 * time.Minute
	mfaMaxAttempts     = 6
	totpPeriodSeconds  = int64(30)
	recoveryCodeCount  = 10
)

var mfaLimiter = &rateLimiter{attempts: make(map[string][]time.Time)}

func userMFAEnabled(user *User) bool {
	return user != nil && user.MFAType == "totp" && user.MFASecretEncrypted != ""
}

func generateTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

func totpCode(secret string, counter int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(
		strings.ToUpper(strings.TrimSpace(secret)),
	)
	if err != nil {
		return "", err
	}
	var message [8]byte
	value := uint64(counter)
	for i := 7; i >= 0; i-- {
		message[i] = byte(value)
		value >>= 8
	}
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(message[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	binary := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	return fmt.Sprintf("%06d", binary%1_000_000), nil
}

func validateTOTP(secret, supplied string, now time.Time, lastCounter int64) (int64, bool) {
	code := strings.TrimSpace(supplied)
	if len(code) != 6 {
		return 0, false
	}
	if _, err := strconv.Atoi(code); err != nil {
		return 0, false
	}
	current := now.UTC().Unix() / totpPeriodSeconds
	for _, delta := range []int64{0, -1, 1} {
		counter := current + delta
		if counter <= lastCounter {
			continue
		}
		expected, err := totpCode(secret, counter)
		if err == nil && subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			return counter, true
		}
	}
	return 0, false
}

func normalizeRecoveryCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}

func recoveryCodeHash(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}

func generateRecoveryCodes() ([]string, []string, error) {
	codes := make([]string, 0, recoveryCodeCount)
	hashes := make([]string, 0, recoveryCodeCount)
	for i := 0; i < recoveryCodeCount; i++ {
		raw := make([]byte, 12)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		encoded := strings.ToUpper(hex.EncodeToString(raw))
		parts := make([]string, 0, len(encoded)/4)
		for j := 0; j < len(encoded); j += 4 {
			parts = append(parts, encoded[j:j+4])
		}
		code := strings.Join(parts, "-")
		codes = append(codes, code)
		hashes = append(hashes, recoveryCodeHash(code))
	}
	return codes, hashes, nil
}

func (s *Server) verifyUserMFACode(user *User, code string) (usedRecovery bool, remaining int, err error) {
	if !userMFAEnabled(user) {
		return false, 0, fmt.Errorf("MFA is not enabled")
	}
	normalized := normalizeRecoveryCode(code)
	if len(normalized) >= 16 {
		remaining, err = s.store.ConsumeMFARecoveryHash(user.ID, recoveryCodeHash(normalized))
		if err != nil {
			return false, 0, fmt.Errorf("invalid authentication code")
		}
		return true, remaining, nil
	}
	secret, err := Decrypt(s.secret, user.MFASecretEncrypted)
	if err != nil {
		return false, 0, fmt.Errorf("unable to read MFA configuration")
	}
	if counter, ok := validateTOTP(secret, code, time.Now(), user.MFALastCounter); ok {
		if err := s.store.AdvanceMFACounter(user.ID, counter); err != nil {
			return false, 0, err
		}
		return false, recoveryHashCount(user.MFARecoveryHashes), nil
	}
	return false, 0, fmt.Errorf("invalid authentication code")
}

func verifyCurrentPassword(user *User, supplied string) bool {
	return user != nil && supplied != "" &&
		bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(supplied)) == nil
}

func (s *Server) hasActiveBrowserSession(r *http.Request, userID int64) bool {
	token := sessionToken(r)
	if token == "" {
		return false
	}
	got, err := s.store.GetSession(token)
	return err == nil && got == userID
}

func (s *Server) handleMFAStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	user, err := s.store.GetUserByID(getUserID(r))
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if !s.hasActiveBrowserSession(r, user.ID) {
		http.Error(w, "interactive session required", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{
		"enabled": userMFAEnabled(user),
		"type": func() string {
			if userMFAEnabled(user) {
				return "totp"
			}
			return ""
		}(),
		"pending":                  user.MFAType == "totp_pending",
		"recovery_codes_remaining": recoveryHashCount(user.MFARecoveryHashes),
		"enabled_at":               user.MFAEnabledAt,
	})
}

func (s *Server) handleMFAEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	user, err := s.store.GetUserByID(getUserID(r))
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if !s.hasActiveBrowserSession(r, user.ID) {
		http.Error(w, "interactive session required", http.StatusUnauthorized)
		return
	}
	if !verifyCurrentPassword(user, body.CurrentPassword) {
		http.Error(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}
	if userMFAEnabled(user) {
		http.Error(w, "MFA is already enabled", http.StatusConflict)
		return
	}
	secret, err := generateTOTPSecret()
	if err != nil {
		http.Error(w, "failed to generate MFA secret", http.StatusInternalServerError)
		return
	}
	encrypted, err := Encrypt(s.secret, secret)
	if err != nil {
		http.Error(w, "failed to protect MFA secret", http.StatusInternalServerError)
		return
	}
	if err := s.store.BeginTOTPEnrollment(user.ID, encrypted); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	params := url.Values{
		"secret":    {secret},
		"issuer":    {"Apteva"},
		"algorithm": {"SHA1"},
		"digits":    {"6"},
		"period":    {"30"},
	}
	otpauth := "otpauth://totp/" + url.PathEscape("Apteva:"+user.Email) + "?" + params.Encode()
	writeJSON(w, map[string]any{
		"type":        "totp",
		"secret":      secret,
		"otpauth_uri": otpauth,
	})
}

func (s *Server) handleMFAConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	user, err := s.store.GetUserByID(getUserID(r))
	if err != nil || user.MFAType != "totp_pending" {
		http.Error(w, "TOTP enrollment is not pending", http.StatusConflict)
		return
	}
	if !s.hasActiveBrowserSession(r, user.ID) {
		http.Error(w, "interactive session required", http.StatusUnauthorized)
		return
	}
	secret, err := Decrypt(s.secret, user.MFASecretEncrypted)
	if err != nil {
		http.Error(w, "unable to read MFA configuration", http.StatusInternalServerError)
		return
	}
	counter, ok := validateTOTP(secret, body.Code, time.Now(), -1)
	if !ok {
		http.Error(w, "invalid authentication code", http.StatusUnauthorized)
		return
	}
	codes, hashes, err := generateRecoveryCodes()
	if err != nil {
		http.Error(w, "failed to generate recovery codes", http.StatusInternalServerError)
		return
	}
	if err := s.store.ConfirmTOTPEnrollment(user.ID, counter, hashes); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	currentToken := sessionToken(r)
	_ = s.store.DeleteSessionsForUserExcept(user.ID, currentToken)
	writeJSON(w, map[string]any{"enabled": true, "recovery_codes": codes})
}

func (s *Server) handleMFAVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	token := sessionToken(r)
	userID, attempts, err := s.store.GetPendingMFASession(token)
	if err != nil || attempts >= mfaMaxAttempts {
		clearSessionCookie(w, r)
		http.Error(w, "MFA challenge expired", http.StatusUnauthorized)
		return
	}
	limitKey := fmt.Sprintf("%d:%s", userID, clientIP(r))
	if !mfaLimiter.allow(limitKey, mfaMaxAttempts, 5*time.Minute) {
		http.Error(w, "too many MFA attempts", http.StatusTooManyRequests)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	user, err := s.store.GetUserByID(userID)
	if err != nil {
		http.Error(w, "invalid authentication code", http.StatusUnauthorized)
		return
	}
	usedRecovery, remaining, err := s.verifyUserMFACode(user, body.Code)
	if err != nil {
		_ = s.store.RecordPendingMFAFailure(token, mfaMaxAttempts)
		http.Error(w, "invalid authentication code", http.StatusUnauthorized)
		return
	}
	if err := s.store.ActivateMFASession(token, time.Now().Add(sessionDuration)); err != nil {
		http.Error(w, "MFA challenge expired", http.StatusUnauthorized)
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, map[string]any{
		"user_id":                  user.ID,
		"email":                    user.Email,
		"used_recovery_code":       usedRecovery,
		"recovery_codes_remaining": remaining,
	})
}

func (s *Server) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		Code            string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	user, err := s.store.GetUserByID(getUserID(r))
	if err != nil || !userMFAEnabled(user) {
		http.Error(w, "MFA is not enabled", http.StatusConflict)
		return
	}
	if !s.hasActiveBrowserSession(r, user.ID) {
		http.Error(w, "interactive session required", http.StatusUnauthorized)
		return
	}
	if !verifyCurrentPassword(user, body.CurrentPassword) {
		http.Error(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}
	if _, _, err := s.verifyUserMFACode(user, body.Code); err != nil {
		http.Error(w, "invalid authentication code", http.StatusUnauthorized)
		return
	}
	if err := s.store.DisableMFA(user.ID); err != nil {
		http.Error(w, "failed to disable MFA", http.StatusInternalServerError)
		return
	}
	currentToken := sessionToken(r)
	_ = s.store.DeleteSessionsForUserExcept(user.ID, currentToken)
	writeJSON(w, map[string]any{"enabled": false})
}

func (s *Server) handleMFARecoveryCodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		Code            string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	user, err := s.store.GetUserByID(getUserID(r))
	if err != nil || !userMFAEnabled(user) {
		http.Error(w, "MFA is not enabled", http.StatusConflict)
		return
	}
	if !s.hasActiveBrowserSession(r, user.ID) {
		http.Error(w, "interactive session required", http.StatusUnauthorized)
		return
	}
	if !verifyCurrentPassword(user, body.CurrentPassword) {
		http.Error(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}
	if _, _, err := s.verifyUserMFACode(user, body.Code); err != nil {
		http.Error(w, "invalid authentication code", http.StatusUnauthorized)
		return
	}
	codes, hashes, err := generateRecoveryCodes()
	if err != nil {
		http.Error(w, "failed to regenerate recovery codes", http.StatusInternalServerError)
		return
	}
	if err := s.store.ReplaceMFARecoveryHashes(user.ID, hashes); err != nil {
		http.Error(w, "failed to regenerate recovery codes", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"recovery_codes": codes})
}

func sessionToken(r *http.Request) string {
	if cookie, err := r.Cookie(cookieName); err == nil {
		return cookie.Value
	}
	return ""
}
