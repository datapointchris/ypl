// Package jarseal seals the YouTube cookie jar before the store holds it, so a
// copy of the store, a backup included, carries no usable session. The key is
// 32 bytes from the server's environment and never enters the store.
//
// A sealed jar is "v1:" and the standard base64 of a 12-byte nonce followed by
// the AES-256-GCM ciphertext.
package jarseal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const prefix = "v1:"

// ErrUnopenable is a sealed jar this key cannot open: sealed under another
// key, or not sealed at all.
var ErrUnopenable = errors.New("the stored cookie jar cannot be opened with this key")

// Sealer seals and opens jars under one key.
type Sealer struct {
	aead cipher.AEAD
}

// New is a Sealer for key, the standard base64 of 32 random bytes, as
// `openssl rand -base64 32` prints.
func New(key string) (*Sealer, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil {
		return nil, fmt.Errorf("the cookie key is not base64: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("the cookie key is %d bytes, and has to be 32", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal is jar sealed under a fresh nonce.
func (s *Sealer) Seal(jar string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("make a nonce for the cookie jar: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(jar), nil)
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open returns the jar inside sealed, or ErrUnopenable when this key did not
// seal it.
func (s *Sealer) Open(sealed string) (string, error) {
	encoded, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return "", ErrUnopenable
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", ErrUnopenable
	}
	nonce, ciphertext := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	jar, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrUnopenable
	}
	return string(jar), nil
}
