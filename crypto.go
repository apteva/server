package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LoadSecret returns a 32-byte encryption key.
// It checks SERVER_SECRET env var first, then falls back to a .secret file
// next to the database. If neither exists, it generates a new key and saves it.
func LoadSecret(dataDir string) ([]byte, error) {
	// Try env var first
	if env := os.Getenv("SERVER_SECRET"); env != "" {
		key, err := hex.DecodeString(env)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("SERVER_SECRET must be 64 hex chars (32 bytes)")
		}
		return key, nil
	}

	// Try .secret file
	secretPath := filepath.Join(dataDir, ".secret")
	if data, err := os.ReadFile(secretPath); err == nil {
		key, err := hex.DecodeString(string(data))
		if err == nil && len(key) == 32 {
			return key, nil
		}
	}

	// Generate new key
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("failed to generate secret: %w", err)
	}

	// Save it
	os.MkdirAll(dataDir, 0755)
	if err := os.WriteFile(secretPath, []byte(hex.EncodeToString(key)), 0600); err != nil {
		return nil, fmt.Errorf("failed to save secret: %w", err)
	}

	fmt.Fprintf(os.Stderr, "generated new encryption key at %s\n", secretPath)
	return key, nil
}

// Encrypt encrypts plaintext using AES-256-GCM with a random nonce.
// Returns hex-encoded ciphertext (nonce prepended).
func Encrypt(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(ciphertext), nil
}

// Decrypt decrypts hex-encoded ciphertext (with prepended nonce) using AES-256-GCM.
func Decrypt(key []byte, ciphertextHex string) (string, error) {
	data, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
