// Package auth manages the API key and authenticates HTTP requests.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/secrets"
)

// SourceEnvironment is reported as the key source when TABTENDER_API_KEY is set.
const SourceEnvironment = "environment"

// MinKeyLength is the shortest administrator-supplied key that is accepted.
const MinKeyLength = 32

// keyPrefix makes Tabtender keys recognizable to humans and secret scanners.
const keyPrefix = "tt_"

// ErrEnvironmentControlled is returned when rotating a key pinned by the environment.
var ErrEnvironmentControlled = fmt.Errorf("the API key is set by %s and cannot be rotated: %w",
	config.EnvAPIKey, config.ErrEnvironmentControlled)

// KeyManager owns the API key: it loads or generates it, verifies presented
// credentials in constant time, and rotates it on request.
type KeyManager struct {
	store secrets.Store

	mu        sync.RWMutex
	digest    [sha256.Size]byte
	key       string
	source    string
	generated bool

	initialKeyFile string
}

// NewKeyManager resolves the API key. A non-empty envKey always wins;
// otherwise the stored key is used, and a fresh 256-bit key is generated and
// stored on first run.
func NewKeyManager(store secrets.Store, envKey string) (*KeyManager, error) {
	m := &KeyManager{store: store}
	if envKey != "" {
		if err := validateSupplied(envKey); err != nil {
			return nil, fmt.Errorf("%s: %w", config.EnvAPIKey, err)
		}
		m.set(envKey, SourceEnvironment)
		return m, nil
	}
	key, err := store.Get(secrets.APIKey)
	switch {
	case err == nil:
		if err := validateSupplied(key); err != nil {
			return nil, fmt.Errorf("stored API key: %w", err)
		}
	case errors.Is(err, secrets.ErrNotFound):
		if key, err = GenerateKey(); err != nil {
			return nil, err
		}
		if err := store.Set(secrets.APIKey, key); err != nil {
			return nil, fmt.Errorf("store generated API key: %w", err)
		}
		m.generated = true
	default:
		return nil, fmt.Errorf("load API key: %w", err)
	}
	m.set(key, store.Backend())
	return m, nil
}

func validateSupplied(key string) error {
	if len(key) < MinKeyLength {
		return fmt.Errorf("key must be at least %d characters", MinKeyLength)
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return errors.New("key must not contain whitespace")
	}
	return nil
}

// GenerateKey returns a new key carrying 256 bits of entropy.
func GenerateKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate API key: %w", err)
	}
	return keyPrefix + hex.EncodeToString(buf), nil
}

func (m *KeyManager) set(key, source string) {
	m.key = key
	m.source = source
	m.digest = sha256.Sum256([]byte(key))
}

// Key returns the current API key.
func (m *KeyManager) Key() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.key
}

// Source reports where the key comes from: SourceEnvironment or the secret
// store backend name.
func (m *KeyManager) Source() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.source
}

// Rotate replaces the stored key with a freshly generated one.
func (m *KeyManager) Rotate() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.source == SourceEnvironment {
		return "", ErrEnvironmentControlled
	}
	key, err := GenerateKey()
	if err != nil {
		return "", err
	}
	if err := m.store.Set(secrets.APIKey, key); err != nil {
		return "", fmt.Errorf("store rotated API key: %w", err)
	}
	m.set(key, m.source)
	m.removeInitialKeyFileLocked()
	return key, nil
}

// Verify reports whether token matches the API key. Both values are hashed
// first so the comparison is constant-time regardless of token length.
func (m *KeyManager) Verify(token string) bool {
	presented := sha256.Sum256([]byte(token))
	m.mu.RLock()
	ok := subtle.ConstantTimeCompare(presented[:], m.digest[:]) == 1
	pending := m.initialKeyFile != ""
	m.mu.RUnlock()
	if ok && pending {
		m.mu.Lock()
		m.removeInitialKeyFileLocked()
		m.mu.Unlock()
	}
	return ok
}

// EnableInitialKeyFile supports one-time key retrieval on hosts without a
// management UI. If the key was generated during this start it is written to
// path (which must live in an access-restricted directory). The file is
// deleted as soon as the key is first used successfully, or rotated.
func (m *KeyManager) EnableInitialKeyFile(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generated {
		if err := config.WriteFileAtomic(path, []byte(m.key+"\n"), 0o600); err != nil {
			return fmt.Errorf("write initial API key file: %w", err)
		}
	}
	if _, err := os.Stat(path); err == nil {
		m.initialKeyFile = path
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (m *KeyManager) removeInitialKeyFileLocked() {
	if m.initialKeyFile == "" {
		return
	}
	_ = os.Remove(m.initialKeyFile)
	m.initialKeyFile = ""
}

// Verifier checks bearer tokens.
type Verifier interface {
	Verify(token string) bool
}

// BearerToken extracts the token from an "Authorization: Bearer <token>" header.
func BearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// Middleware rejects requests that do not present a valid bearer token.
// The WebSocket handshake is an ordinary HTTP request, so CDP connections are
// covered by the same check.
func Middleware(v Verifier, unauthorized http.HandlerFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := BearerToken(r)
			if !ok || !v.Verify(token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="tabtender"`)
				unauthorized(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
