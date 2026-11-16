package secrets

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testStoreContract(t *testing.T, store Store) {
	t.Helper()
	const name = "contract-test"
	t.Cleanup(func() { _ = store.Delete(name) })

	if _, err := store.Get(name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on missing secret: err = %v, want ErrNotFound", err)
	}
	if err := store.Set(name, "first"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(name, "s3cr3t value/with+symbols="); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(name)
	if err != nil || got != "s3cr3t value/with+symbols=" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := store.Delete(name); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(name); err != nil {
		t.Fatalf("deleting a missing secret must succeed, got %v", err)
	}
	if _, err := store.Get(name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("secret still present after delete: %v", err)
	}
}

func TestFileStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	store, err := Open(BackendFile, dir)
	if err != nil {
		t.Fatal(err)
	}
	testStoreContract(t, store)

	if err := store.Set(APIKey, "value"); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{dir, filepath.Join(dir, APIKey+".secret")} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm&0o077 != 0 {
				t.Errorf("%s is accessible to others: %o", path, perm)
			}
		}
	}
	for _, bad := range []string{"", "../escape", "a/b", "UPPER", strings.Repeat("x", 80)} {
		if err := store.Set(bad, "v"); err == nil {
			t.Errorf("Set(%q) accepted an invalid name", bad)
		}
	}
}

func TestMemoryStore(t *testing.T) {
	testStoreContract(t, NewMemory())
}

// TestPlatformStore exercises the OS credential facility (Keychain on macOS,
// DPAPI on Windows). It needs a usable login session, so it is opt-in.
func TestPlatformStore(t *testing.T) {
	if os.Getenv("TABTENDER_TEST_PLATFORM_SECRETS") == "" {
		t.Skip("set TABTENDER_TEST_PLATFORM_SECRETS=1 to test the OS secret store")
	}
	store, err := Open(BackendAuto, filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("platform backend: %s", store.Backend())
	testStoreContract(t, store)
}

func TestSealedStoreNeverWritesPlaintext(t *testing.T) {
	dir := t.TempDir()
	reverse := func(b []byte) ([]byte, error) {
		out := make([]byte, len(b))
		for i := range b {
			out[len(b)-1-i] = b[i]
		}
		return out, nil
	}
	store, err := newSealedStore(dir, sealer{backend: "test", ext: ".sealed", seal: reverse, unseal: reverse})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(APIKey, "plaintext"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, APIKey+".sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "txetnialp" {
		t.Fatalf("on-disk content = %q, want the sealed form", raw)
	}
	if got, _ := store.Get(APIKey); got != "plaintext" {
		t.Fatalf("Get = %q", got)
	}
}

func TestUnknownBackend(t *testing.T) {
	if _, err := Open("vault", t.TempDir()); err == nil {
		t.Fatal("expected an error for an unknown backend")
	}
}

func TestImportProvisioned(t *testing.T) {
	dir := t.TempDir()
	store := NewMemory()
	provision := filepath.Join(dir, APIKey+".provision")
	// A UTF-8 BOM and trailing newline are what Windows editors produce.
	if err := os.WriteFile(provision, []byte("\xef\xbb\xbfadmin-supplied-key\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ImportProvisioned(store, dir, discardLogger(), APIKey, TunnelToken); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(APIKey); got != "admin-supplied-key" {
		t.Fatalf("imported key = %q", got)
	}
	if _, err := os.Stat(provision); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the plaintext provisioning file must be deleted after import")
	}
	if _, err := store.Get(TunnelToken); !errors.Is(err, ErrNotFound) {
		t.Fatal("a secret without a provisioning file must stay untouched")
	}

	if err := os.WriteFile(provision, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ImportProvisioned(store, dir, discardLogger(), APIKey); err == nil {
		t.Fatal("an empty provisioning file must be rejected")
	}
}
