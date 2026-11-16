package secrets

import (
	"errors"

	"github.com/zalando/go-keyring"
)

const defaultBackend = BackendKeychain

// keychainService namespaces Tabtender items in the login Keychain.
const keychainService = "com.skillfulagents.tabtender"

type keychainStore struct{}

func newPlatformStore(string) (Store, error) { return keychainStore{}, nil }

func (keychainStore) Get(name string) (string, error) {
	v, err := keyring.Get(keychainService, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return v, err
}

func (keychainStore) Set(name, value string) error {
	return keyring.Set(keychainService, name, value)
}

func (keychainStore) Delete(name string) error {
	err := keyring.Delete(keychainService, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func (keychainStore) Backend() string { return BackendKeychain }
