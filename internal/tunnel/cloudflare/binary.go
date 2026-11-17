package cloudflare

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/config"
)

// Version is the cloudflared release that Tabtender downloads when no
// existing installation is found.
const Version = "2026.11.0"

const downloadBase = "https://github.com/cloudflare/cloudflared/releases/download/"

// asset is a pinned cloudflared release artifact.
type asset struct {
	name   string
	sha256 string
}

// assets maps GOOS/GOARCH to the release artifact for Version. Checksums
// are pinned so a tampered download is never executed.
var assets = map[string]asset{
	"darwin/amd64": {"cloudflared-darwin-amd64.tgz", "8a59d51120cebbe902f0a8d6e1b7a7fd4f9a5bb4dcb3e0f05872483e75e7db6b"},
	"darwin/arm64": {"cloudflared-darwin-arm64.tgz", "8c2f90277ef06a6080bb40b964d33f09f1188102f8798f2e344174b6cf41accf"},
	"linux/amd64":  {"cloudflared-linux-amd64", "51f7c68b57ed7269164075d7e789efce5b59355982f06392deeb037d982a51fd"},
	"linux/arm64":  {"cloudflared-linux-arm64", "abe54a006acf9faf2d19ee9d953cb0d0804e92f59c5382a162e57b84d086f56f"},
	// Cloudflare publishes no native Windows ARM64 build; the x64 build
	// runs under emulation there.
	"windows/amd64": {"cloudflared-windows-amd64.exe", "d5c1d82031da6253dcdcff64c944cd0fe063016edeb2e4fd088075fc9bd52d0a"},
	"windows/arm64": {"cloudflared-windows-amd64.exe", "d5c1d82031da6253dcdcff64c944cd0fe063016edeb2e4fd088075fc9bd52d0a"},
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// resolveBinary locates cloudflared. An explicit path must exist. With
// "auto" the search order is: next to the tabtenderd executable (bundled),
// the managed copy in binDir, PATH, and finally a verified download into
// binDir.
func resolveBinary(ctx context.Context, configured, binDir string) (string, error) {
	if configured != "" && configured != config.AutoPath {
		if info, err := os.Stat(configured); err != nil || info.IsDir() {
			return "", fmt.Errorf("cloudflared executable %q not found", configured)
		}
		return configured, nil
	}
	if self, err := os.Executable(); err == nil {
		bundled := filepath.Join(filepath.Dir(self), exeName("cloudflared"))
		if isFile(bundled) {
			return bundled, nil
		}
	}
	managed := filepath.Join(binDir, exeName("cloudflared-"+Version))
	if isFile(managed) {
		return managed, nil
	}
	if path, err := exec.LookPath("cloudflared"); err == nil {
		return path, nil
	}
	a, ok := assets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("no cloudflared build is available for %s/%s; install it and set tunnel.cloudflare.executablePath",
			runtime.GOOS, runtime.GOARCH)
	}
	if err := download(ctx, downloadBase+Version+"/"+a.name, a, managed); err != nil {
		return "", fmt.Errorf("download cloudflared %s: %w", Version, err)
	}
	removeSuperseded(binDir, managed)
	return managed, nil
}

// removeSuperseded deletes what earlier downloads left in binDir: binaries
// of other versions and the remains of interrupted downloads.
func removeSuperseded(binDir, current string) {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name, path := entry.Name(), filepath.Join(binDir, entry.Name())
		stale := strings.HasPrefix(name, "cloudflared-") || strings.HasPrefix(name, downloadPrefix)
		if stale && path != current && !entry.IsDir() {
			_ = os.Remove(path)
		}
	}
}

const (
	downloadPrefix  = ".cloudflared-download-"
	maxDownloadSize = 256 << 20
)

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// download fetches a release artifact, verifies its checksum, and installs
// the cloudflared executable at dest.
func download(ctx context.Context, url string, a asset, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), downloadPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // best-effort cleanup
	hash := sha256.New()
	// The limit only guards the disk; a truncated body fails the checksum.
	if _, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, maxDownloadSize)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != a.sha256 {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", a.name, got, a.sha256)
	}

	staged := tmp.Name()
	if strings.HasSuffix(a.name, ".tgz") {
		staged = tmp.Name() + ".bin"
		defer os.Remove(staged) //nolint:errcheck // best-effort cleanup
		if err := extractCloudflared(tmp.Name(), staged); err != nil {
			return err
		}
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return err
	}
	return os.Rename(staged, dest)
}

// extractCloudflared pulls the "cloudflared" entry out of a .tgz archive.
func extractCloudflared(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only file
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("archive does not contain cloudflared")
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "cloudflared" {
			continue
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // size bounded by the pinned checksum
			_ = out.Close()
			return err
		}
		return out.Close()
	}
}
