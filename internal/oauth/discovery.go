package oauth

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"antigravity-proxy/internal/config"
)

const (
	officialManifestBase       = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app"
	maxManifestBytes     int64 = 64 << 10
	maxCLIBytes          int64 = 512 << 20
	maxArchiveBytes      int64 = 1 << 30
)

type credentialDiscovery struct {
	client       *http.Client
	manifestBase string
	goos, goarch string
	musl         bool
}

func newCredentialDiscovery() credentialDiscovery {
	musl := false
	for _, path := range []string{"/lib/libc.musl-x86_64.so.1", "/lib/libc.musl-aarch64.so.1"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			musl = true
		}
	}
	return credentialDiscovery{
		client: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many CLI download redirects")
			}
			return requireHTTPS(req.URL)
		}},
		manifestBase: officialManifestBase,
		goos:         runtime.GOOS, goarch: runtime.GOARCH, musl: musl,
	}
}

func requireHTTPS(u *url.URL) error {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return errors.New("official CLI download requires HTTPS without URL credentials")
	}
	return nil
}

func (d credentialDiscovery) resolve(ctx context.Context, cfg config.Config) (string, string, error) {
	id, secret, err := cfg.OAuthCredentials()
	if err != nil || id != "" {
		return id, secret, err
	}
	path, err := exec.LookPath("agy")
	if err != nil && !errors.Is(err, exec.ErrNotFound) {
		return "", "", errors.New("cannot safely resolve agy on PATH; configure a complete OAuth client pair or fix PATH")
	}
	if errors.Is(err, exec.ErrNotFound) {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", "", errors.New("cannot locate the home directory to search for agy")
		}
		path = filepath.Join(home, ".local", "bin", "agy")
		if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
			return d.download(ctx)
		} else if statErr != nil {
			return "", "", errors.New("cannot read installed agy; fix its permissions or configure a complete OAuth client pair")
		}
	}
	data, err := readCLI(path)
	if err != nil {
		return "", "", errors.New("cannot read installed agy; fix the installed file or configure a complete OAuth client pair")
	}
	id, secret, err = extractBinaryCredentials(data)
	if err != nil {
		return "", "", fmt.Errorf("cannot extract OAuth credentials from installed agy; update the installed CLI or configure a complete OAuth client pair: %w", err)
	}
	return id, secret, nil
}

func (d credentialDiscovery) platform() (string, error) {
	if d.goos != "linux" && d.goos != "darwin" && d.goos != "android" {
		return "", errors.New("automatic CLI download supports only Linux, macOS, and Android; install agy or configure a complete OAuth client pair")
	}
	if d.goarch != "amd64" && d.goarch != "arm64" {
		return "", errors.New("automatic CLI download supports only amd64 and arm64; install agy or configure a complete OAuth client pair")
	}
	platform := d.goos + "_" + d.goarch
	if d.goos == "linux" && d.musl {
		platform += "_musl"
	}
	return platform, nil
}

func (d credentialDiscovery) get(ctx context.Context, address string) (*http.Response, error) {
	u, err := url.Parse(address)
	if err != nil || requireHTTPS(u) != nil {
		return nil, errors.New("official CLI download requires a valid HTTPS URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("cannot construct official CLI download request")
	}
	// Hash the published payload bytes, not an HTTP transport's gzip decoding.
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, errors.New("cannot download official CLI; check network access and TLS configuration")
	}
	if requireHTTPS(resp.Request.URL) != nil {
		resp.Body.Close()
		return nil, errors.New("official CLI download redirected away from HTTPS")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("official CLI download returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (d credentialDiscovery) download(ctx context.Context) (string, string, error) {
	platform, err := d.platform()
	if err != nil {
		return "", "", err
	}
	resp, err := d.get(ctx, d.manifestBase+"/manifests/"+platform+".json")
	if err != nil {
		return "", "", err
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	resp.Body.Close()
	if err != nil || int64(len(manifestBytes)) > maxManifestBytes {
		return "", "", errors.New("official CLI manifest is unreadable or too large")
	}
	var manifest struct {
		Version string `json:"version"`
		URL     string `json:"url"`
		SHA512  string `json:"sha512"`
	}
	if json.Unmarshal(manifestBytes, &manifest) != nil || manifest.URL == "" {
		return "", "", errors.New("official CLI manifest is malformed")
	}
	wantHash, err := hex.DecodeString(manifest.SHA512)
	if err != nil || len(wantHash) != sha512.Size {
		return "", "", errors.New("official CLI manifest has an invalid SHA512 checksum")
	}
	payloadURL, err := url.Parse(manifest.URL)
	if err != nil || requireHTTPS(payloadURL) != nil {
		return "", "", errors.New("official CLI manifest payload must use HTTPS")
	}
	dir, err := os.MkdirTemp("", "antigravity-proxy-cli-")
	if err != nil {
		return "", "", errors.New("cannot create private CLI download directory")
	}
	defer os.RemoveAll(dir)
	payloadPath := filepath.Join(dir, "payload")
	payload, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", "", errors.New("cannot create private CLI download file")
	}
	defer payload.Close()
	resp, err = d.get(ctx, manifest.URL)
	if err != nil {
		return "", "", err
	}
	hash := sha512.New()
	n, copyErr := io.Copy(io.MultiWriter(payload, hash), io.LimitReader(resp.Body, maxCLIBytes+1))
	resp.Body.Close()
	if copyErr != nil || n > maxCLIBytes {
		return "", "", errors.New("official CLI payload is unreadable or too large")
	}
	if !bytes.Equal(hash.Sum(nil), wantHash) {
		return "", "", errors.New("official CLI payload SHA512 checksum mismatch")
	}
	binaryPath := payloadPath
	if strings.Contains(payloadURL.Path, ".tar.gz") {
		if _, err := payload.Seek(0, io.SeekStart); err != nil {
			return "", "", errors.New("cannot read verified CLI archive")
		}
		binaryPath = filepath.Join(dir, "antigravity")
		if err := extractCLIArchive(payload, binaryPath); err != nil {
			return "", "", err
		}
	}
	data, err := readCLI(binaryPath)
	if err != nil {
		return "", "", errors.New("cannot read verified CLI binary")
	}
	id, secret, err := extractBinaryCredentials(data)
	if err != nil {
		return "", "", fmt.Errorf("cannot extract OAuth credentials from official CLI; configure a complete OAuth client pair: %w", err)
	}
	return id, secret, nil
}

func readCLI(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxCLIBytes {
		return nil, errors.New("CLI file must be a bounded regular binary")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxCLIBytes {
		return nil, errors.New("CLI file must be a bounded regular binary")
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, errors.New("CLI binary changed or became unreadable while reading")
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || err != io.EOF {
		return nil, errors.New("CLI binary changed while reading")
	}
	return data, nil
}

func extractCLIArchive(payload io.Reader, destination string) error {
	gz, err := gzip.NewReader(payload)
	if err != nil {
		return errors.New("official CLI archive is not valid gzip")
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: maxArchiveBytes + 1}
	archive := tar.NewReader(limited)
	found := false
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil || limited.N <= 0 {
			return errors.New("official CLI archive is corrupt or too large")
		}
		if header.Name != "antigravity" {
			continue
		}
		if found || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || header.Size < 1 || header.Size > maxCLIBytes {
			return errors.New("official CLI archive must contain exactly one regular antigravity binary")
		}
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return errors.New("cannot create private extracted CLI file")
		}
		_, copyErr := io.Copy(file, archive)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return errors.New("cannot extract official CLI binary")
		}
		found = true
	}
	// Drain gzip to verify its checksum and enforce the expanded-size bound.
	if _, err := io.Copy(io.Discard, limited); err != nil || limited.N <= 0 {
		return errors.New("official CLI archive is corrupt or too large")
	}
	if !found {
		return errors.New("official CLI archive is missing the regular antigravity binary")
	}
	return nil
}
