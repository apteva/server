package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	plaintext := `{"FIREWORKS_API_KEY":"sk-test123","model":"llama3"}`

	encrypted, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if encrypted == plaintext {
		t.Fatal("encrypted should differ from plaintext")
	}

	decrypted, err := Decrypt(key, encrypted)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if decrypted != plaintext {
		t.Fatalf("expected %q, got %q", plaintext, decrypted)
	}
}

func TestEncryptDecrypt_WrongKey(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	key2[0] = 1

	encrypted, _ := Encrypt(key1, "secret")
	_, err := Decrypt(key2, encrypted)
	if err == nil {
		t.Fatal("expected error decrypting with wrong key")
	}
}

func TestEncryptDecrypt_DifferentNonce(t *testing.T) {
	key := make([]byte, 32)
	plaintext := "same text"

	e1, _ := Encrypt(key, plaintext)
	e2, _ := Encrypt(key, plaintext)

	if e1 == e2 {
		t.Fatal("two encryptions of same text should produce different ciphertext (random nonce)")
	}

	d1, _ := Decrypt(key, e1)
	d2, _ := Decrypt(key, e2)
	if d1 != d2 || d1 != plaintext {
		t.Fatal("both should decrypt to same plaintext")
	}
}

func TestLoadSecret_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()

	key1, err := LoadSecret(dir)
	if err != nil {
		t.Fatalf("LoadSecret: %v", err)
	}
	if len(key1) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(key1))
	}

	// File should exist
	if _, err := os.Stat(filepath.Join(dir, ".secret")); err != nil {
		t.Fatal("expected .secret file to exist")
	}

	// Second call should return same key
	key2, err := LoadSecret(dir)
	if err != nil {
		t.Fatalf("LoadSecret second call: %v", err)
	}
	if string(key1) != string(key2) {
		t.Fatal("expected same key on second load")
	}
}
