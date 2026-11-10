//go:build !windows

package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
)

// Elsewhere the key file is sealed with AES-256-GCM under a random master
// key kept in the user's config directory with 0600 permissions
// (ROUTER_MASTER_KEY_FILE overrides). This protects the file when it is
// copied or backed up on its own; it does not protect against someone who
// can read the user's files, master key included.

var aesMagic = []byte("LLMR1A")

func masterKey() ([]byte, error) {
	path := os.Getenv("ROUTER_MASTER_KEY_FILE")
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, "wend", "master.key")
	}
	if k, err := os.ReadFile(path); err == nil && len(k) == 32 {
		return k, nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return k, os.WriteFile(path, k, 0o600)
}

func gcm() (cipher.AEAD, error) {
	k, err := masterKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func seal(plain []byte) ([]byte, error) {
	g, err := gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(bytes.Clone(aesMagic), nonce...)
	return g.Seal(out, nonce, plain, nil), nil
}

func unseal(sealed []byte) ([]byte, error) {
	if !bytes.HasPrefix(sealed, aesMagic) {
		return nil, errors.New("not an AES key file")
	}
	g, err := gcm()
	if err != nil {
		return nil, err
	}
	rest := sealed[len(aesMagic):]
	if len(rest) < g.NonceSize() {
		return nil, errors.New("key file too short")
	}
	return g.Open(nil, rest[:g.NonceSize()], rest[g.NonceSize():], nil)
}
