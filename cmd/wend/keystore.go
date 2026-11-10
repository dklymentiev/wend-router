package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// keyStore holds what the dashboard adds: provider keys, custom providers
// and on/off choices. The file is encrypted (seal/unseal, per platform);
// keys never leave the router once stored.

type storedKey struct {
	ID    string    `json:"id"`
	Key   string    `json:"key"`
	Label string    `json:"label,omitempty"`
	Added time.Time `json:"added"`
}

type storeData struct {
	Keys     map[string][]storedKey `json:"keys,omitempty"`     // provider id -> keys, in order of use
	Custom   []ProviderSpec         `json:"custom,omitempty"`   // providers added in the dashboard
	Disabled map[string]string      `json:"disabled,omitempty"` // provider id -> why it was turned off
	Enabled  map[string]bool        `json:"enabled,omitempty"`  // seed-disabled providers turned on
	Vars     map[string]string      `json:"vars,omitempty"`     // values for {NAME} in base URLs, e.g. CLOUDFLARE_ACCOUNT_ID
}

type keyStore struct {
	mu   sync.Mutex
	path string
	data storeData
}

func keyStorePath() string {
	if p := os.Getenv("ROUTER_KEYS"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "keys.dat")
	}
	return "keys.dat"
}

func openKeyStore(path string) (*keyStore, error) {
	ks := &keyStore{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ks, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := unseal(b)
	if err != nil {
		return nil, errors.New(path + ": cannot decrypt (" + err.Error() + "); it was written by another user or machine")
	}
	if err := json.Unmarshal(plain, &ks.data); err != nil {
		return nil, err
	}
	return ks, nil
}

// update changes the data under the lock and writes the file.
func (ks *keyStore) update(f func(d *storeData) error) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err := f(&ks.data); err != nil {
		return err
	}
	plain, err := json.Marshal(ks.data)
	if err != nil {
		return err
	}
	sealed, err := seal(plain)
	if err != nil {
		return err
	}
	tmp := ks.path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ks.path)
}

// view returns a copy of the data to read without the lock.
func (ks *keyStore) view() storeData {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	b, _ := json.Marshal(ks.data)
	var d storeData
	_ = json.Unmarshal(b, &d)
	return d
}

func keyID(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:4])
}

// maskKey shows enough of a key to recognise it and nothing more.
func maskKey(key string) string {
	if len(key) <= 12 {
		return "****"
	}
	return key[:6] + "…" + key[len(key)-4:]
}
