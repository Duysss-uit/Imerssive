// Package secrets keeps API keys in the OS credential store
// (Windows Credential Manager / macOS Keychain / Secret Service), never in the DB.
package secrets

import (
	"errors"

	"github.com/zalando/go-keyring"
)

const service = "immersive-app"

func Set(providerID, key string) error {
	if key == "" {
		return Delete(providerID)
	}
	return keyring.Set(service, providerID, key)
}

// Get returns "" when no key is stored.
func Get(providerID string) (string, error) {
	k, err := keyring.Get(service, providerID)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return k, err
}

func Delete(providerID string) error {
	err := keyring.Delete(service, providerID)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
