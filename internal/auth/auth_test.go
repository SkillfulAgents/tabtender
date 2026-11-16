package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/secrets"
)

func TestGeneratesAndPersists256BitKey(t *testing.T) {
	store := secrets.NewMemory()
	first, err := NewKeyManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	// 32 random bytes, hex encoded.
	if !regexp.MustCompile(`^tt_[0-9a-f]{64}$`).MatchString(first.Key()) {
		t.Fatalf("unexpected key format: %q", first.Key())
	}
	if first.Source() != "memory" {
		t.Fatalf("source = %q", first.Source())
	}
	stored, _ := store.Get(secrets.APIKey)
	if stored != first.Key() {
		t.Fatal("generated key was not stored")
	}
	second, err := NewKeyManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Key() != first.Key() {
		t.Fatal("key changed across restarts")
	}
}

func TestGeneratedKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		key, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[key] {
			t.Fatal("duplicate key generated")
		}
		seen[key] = true
	}
}

func TestEnvironmentKeyOverridesStore(t *testing.T) {
	store := secrets.NewMemory()
	_ = store.Set(secrets.APIKey, strings.Repeat("s", 40))
	envKey := strings.Repeat("e", 40)
	m, err := NewKeyManager(store, envKey)
	if err != nil {
		t.Fatal(err)
	}
	if m.Key() != envKey || m.Source() != SourceEnvironment {
		t.Fatalf("key=%q source=%q", m.Key(), m.Source())
	}
	if !m.Verify(envKey) || m.Verify(strings.Repeat("s", 40)) {
		t.Fatal("only the environment key must be accepted")
	}
	if _, err := m.Rotate(); !errors.Is(err, ErrEnvironmentControlled) || !errors.Is(err, config.ErrEnvironmentControlled) {
		t.Fatalf("Rotate err = %v, want ErrEnvironmentControlled", err)
	}
	if stored, _ := store.Get(secrets.APIKey); stored != strings.Repeat("s", 40) {
		t.Fatal("the stored key must not be overwritten by the environment key")
	}
}

func TestWeakSuppliedKeysAreRejected(t *testing.T) {
	for _, key := range []string{"short", strings.Repeat("a", 20) + " " + strings.Repeat("b", 20)} {
		if _, err := NewKeyManager(secrets.NewMemory(), key); err == nil {
			t.Errorf("key %q should have been rejected", key)
		}
	}
}

func TestRotate(t *testing.T) {
	store := secrets.NewMemory()
	m, err := NewKeyManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	old := m.Key()
	rotated, err := m.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if rotated == old || m.Key() != rotated {
		t.Fatal("rotation did not produce a new current key")
	}
	if m.Verify(old) {
		t.Fatal("the old key must stop working immediately")
	}
	if !m.Verify(rotated) {
		t.Fatal("the new key must work")
	}
	if stored, _ := store.Get(secrets.APIKey); stored != rotated {
		t.Fatal("the rotated key was not persisted")
	}
}

func TestVerify(t *testing.T) {
	m, err := NewKeyManager(secrets.NewMemory(), "")
	if err != nil {
		t.Fatal(err)
	}
	key := m.Key()
	for _, bad := range []string{"", key[:len(key)-1], key + "0", strings.ToUpper(key), "tt_"} {
		if m.Verify(bad) {
			t.Errorf("Verify(%q) = true", bad)
		}
	}
	if !m.Verify(key) {
		t.Fatal("the real key was rejected")
	}
}

func TestMiddleware(t *testing.T) {
	m, err := NewKeyManager(secrets.NewMemory(), "")
	if err != nil {
		t.Fatal(err)
	}
	handler := Middleware(m, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		name, header string
		want         int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong key", "Bearer nope", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + m.Key(), http.StatusUnauthorized},
		{"bare key", m.Key(), http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"valid", "Bearer " + m.Key(), http.StatusNoContent},
		{"case-insensitive scheme", "bearer " + m.Key(), http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing WWW-Authenticate header")
			}
		})
	}
}

func TestInitialKeyFileIsOneTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial-api-key.txt")
	store := secrets.NewMemory()
	m, err := NewKeyManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.EnableInitialKeyFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a freshly generated key must be retrievable: %v", err)
	}
	if strings.TrimSpace(string(data)) != m.Key() {
		t.Fatal("initial key file does not contain the key")
	}

	m.Verify("wrong")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a failed authentication must not consume the file")
	}

	// A restart before first use keeps the file available.
	restarted, err := NewKeyManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.EnableInitialKeyFile(path); err != nil {
		t.Fatal(err)
	}
	if !restarted.Verify(restarted.Key()) {
		t.Fatal("key rejected")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the file must be deleted once the key has been used")
	}

	// It is never re-created for an existing key.
	again, _ := NewKeyManager(store, "")
	if err := again.EnableInitialKeyFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the file must not reappear for an existing key")
	}
}
