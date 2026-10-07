package oauth

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"antigravity-proxy/internal/config"
)

func isolatedDiscoveryHome(t *testing.T) string {
	t.Helper()
	clearOAuthEnvironment(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, "path"))
	return home
}

func writeTestCLI(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
}

type archiveTestMember struct {
	name string
	kind byte
	data []byte
}

func cliTestArchive(t *testing.T, members ...archiveTestMember) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	archive := tar.NewWriter(gz)
	for _, member := range members {
		header := &tar.Header{Name: member.name, Mode: 0600, Typeflag: member.kind, Size: int64(len(member.data))}
		if member.kind == tar.TypeSymlink {
			header.Linkname = "outside"
			header.Size = 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if member.kind != tar.TypeSymlink {
			if _, err := archive.Write(member.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func discoveryTestServer(t *testing.T, payload []byte, archive bool, alterManifest func(map[string]string)) (credentialDiscovery, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasPrefix(r.URL.Path, "/manifests/") {
			hash := sha512.Sum512(payload)
			path := "/payload"
			if archive {
				path += ".tar.gz"
			}
			manifest := map[string]string{"version": "synthetic-test", "url": server.URL + path, "sha512": hex.EncodeToString(hash[:])}
			if alterManifest != nil {
				alterManifest(manifest)
			}
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		if archive {
			// Storage can label an already compressed artifact with this header.
			// Its manifest checksum still describes the compressed bytes.
			w.Header().Set("Content-Encoding", "gzip")
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	d := newCredentialDiscovery()
	d.client.Transport = server.Client().Transport
	d.manifestBase, d.goos, d.goarch, d.musl = server.URL, "linux", "amd64", false
	return d, calls
}

func TestDiscoveryConfiguredPairDoesNotReadOrDownloadCLI(t *testing.T) {
	home := isolatedDiscoveryHome(t)
	writeTestCLI(t, filepath.Join(home, "path", "agy"), []byte("invalid native binary"))
	id, secret := syntheticPair(t)
	d, calls := discoveryTestServer(t, nil, false, nil)
	gotID, gotSecret, err := d.resolve(context.Background(), config.Config{OAuthClientID: id, OAuthClientSecret: secret})
	if err != nil || gotID != id || gotSecret != secret || int(calls.Load()) != 0 {
		t.Fatal("configured credentials touched local CLI or network")
	}
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", id)
	if _, _, err := d.resolve(context.Background(), config.Config{OAuthClientID: id, OAuthClientSecret: secret}); err == nil || int(calls.Load()) != 0 {
		t.Fatal("partial environment pair did not stop discovery")
	}
}

func TestDiscoveryFindsNativeCLIOnPATHBeforeHome(t *testing.T) {
	home := isolatedDiscoveryHome(t)
	data, id, secret := syntheticNativeCLI(t)
	writeTestCLI(t, filepath.Join(home, "path", "agy"), data)
	writeTestCLI(t, filepath.Join(home, ".local", "bin", "agy"), []byte("invalid native binary"))
	d, calls := discoveryTestServer(t, nil, false, nil)
	gotID, gotSecret, err := d.resolve(context.Background(), config.Config{})
	if err != nil || gotID != id || gotSecret != secret || int(calls.Load()) != 0 {
		t.Fatal("PATH executable did not supply its native credential pair before HOME")
	}
}

func TestDiscoveryFindsNativeCLIInHomeWithoutPATH(t *testing.T) {
	home := isolatedDiscoveryHome(t)
	data, id, secret := syntheticNativeCLI(t)
	writeTestCLI(t, filepath.Join(home, ".local", "bin", "agy"), data)
	d, calls := discoveryTestServer(t, nil, false, nil)
	gotID, gotSecret, err := d.resolve(context.Background(), config.Config{})
	if err != nil || gotID != id || gotSecret != secret || int(calls.Load()) != 0 {
		t.Fatal("HOME CLI did not supply its native credential pair without network")
	}
}

func TestDiscoveryInvalidLocalCLIStopsWithoutDownloadOrExecution(t *testing.T) {
	for _, location := range []string{"path", "home"} {
		t.Run(location, func(t *testing.T) {
			home := isolatedDiscoveryHome(t)
			marker := filepath.Join(home, "executed")
			path := filepath.Join(home, "path", "agy")
			if location == "home" {
				path = filepath.Join(home, ".local", "bin", "agy")
			}
			writeTestCLI(t, path, []byte("#!/bin/sh\ntouch '"+marker+"'\n"))
			d, calls := discoveryTestServer(t, nil, false, nil)
			if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || !strings.Contains(err.Error(), "installed agy") || int(calls.Load()) != 0 {
				t.Fatal("invalid installed CLI did not stop discovery before download")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("discovery executed installed CLI")
			}
		})
	}
}

func TestDiscoveryNonRegularHomeCLIStops(t *testing.T) {
	home := isolatedDiscoveryHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin", "agy"), 0700); err != nil {
		t.Fatal(err)
	}
	d, calls := discoveryTestServer(t, nil, false, nil)
	if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || int(calls.Load()) != 0 {
		t.Fatal("nonregular home CLI did not stop discovery")
	}
}

func TestDiscoveryDownloadsVerifiedNativeDirectAndArchivePayloads(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "archive"}[archive], func(t *testing.T) {
			home := isolatedDiscoveryHome(t)
			data, id, secret := syntheticNativeCLI(t)
			payload := data
			if archive {
				payload = cliTestArchive(t, archiveTestMember{name: "../outside", kind: tar.TypeReg, data: []byte("ignored")}, archiveTestMember{name: "antigravity", kind: tar.TypeReg, data: data})
			}
			d, calls := discoveryTestServer(t, payload, archive, nil)
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			gotID, gotSecret, err := d.resolve(context.Background(), config.Config{})
			if err != nil || gotID != id || gotSecret != secret || int(calls.Load()) != 2 {
				t.Fatal("verified official download did not resolve the native pair")
			}
			entries, err := os.ReadDir(temp)
			if err != nil || len(entries) != 0 {
				t.Fatal("private download files were not cleaned up")
			}
			if _, err := os.Stat(filepath.Join(home, ".local", "bin", "agy")); !os.IsNotExist(err) {
				t.Fatal("download installed a CLI in HOME")
			}
		})
	}
}

func TestDiscoveryRejectsManifestAndPayloadFailures(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		archive bool
		alter   func(map[string]string)
		want    string
		calls   int
	}{
		{name: "checksum before archive parsing", payload: []byte("not gzip"), archive: true, alter: func(m map[string]string) { m["sha512"] = strings.Repeat("0", 128) }, want: "checksum mismatch", calls: 2},
		{name: "invalid checksum", alter: func(m map[string]string) { m["sha512"] = "not hex" }, want: "invalid SHA512", calls: 1},
		{name: "missing URL", alter: func(m map[string]string) { delete(m, "url") }, want: "malformed", calls: 1},
		{name: "plaintext payload", alter: func(m map[string]string) { m["url"] = "http://localhost/payload" }, want: "HTTPS", calls: 1},
		{name: "corrupt archive", payload: []byte("not gzip"), archive: true, want: "gzip", calls: 2},
		{name: "invalid native direct payload", payload: []byte("not executable code"), want: "extract OAuth", calls: 2},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			isolatedDiscoveryHome(t)
			d, calls := discoveryTestServer(t, test.payload, test.archive, test.alter)
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || !strings.Contains(err.Error(), test.want) || int(calls.Load()) != test.calls {
				t.Fatal("download did not fail at the expected safe stage")
			}
			entries, err := os.ReadDir(temp)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed download left private temporary files")
			}
		})
	}
}

func TestDiscoveryRejectsUnsafeAndMissingArchiveMembers(t *testing.T) {
	for _, members := range [][]archiveTestMember{
		{{name: "other", kind: tar.TypeReg, data: []byte("ignored")}},
		{{name: "../antigravity", kind: tar.TypeReg, data: []byte("ignored")}},
		{{name: "antigravity", kind: tar.TypeSymlink}},
		{{name: "antigravity", kind: tar.TypeReg, data: []byte("first")}, {name: "antigravity", kind: tar.TypeReg, data: []byte("second")}},
	} {
		isolatedDiscoveryHome(t)
		payload := cliTestArchive(t, members...)
		d, calls := discoveryTestServer(t, payload, true, nil)
		if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || !strings.Contains(err.Error(), "antigravity binary") || int(calls.Load()) != 2 {
			t.Fatal("unsafe or missing archive member was accepted")
		}
	}
}

func TestDiscoveryPlatformMappingAndUnsupportedPlatforms(t *testing.T) {
	for _, test := range []struct {
		goos, goarch string
		musl         bool
		want         string
	}{
		{"linux", "amd64", false, "linux_amd64"},
		{"linux", "arm64", true, "linux_arm64_musl"},
		{"darwin", "amd64", true, "darwin_amd64"},
		{"darwin", "arm64", false, "darwin_arm64"},
		{"android", "arm64", false, "android_arm64"},
		{"windows", "amd64", false, ""},
		{"linux", "386", false, ""},
	} {
		d := credentialDiscovery{goos: test.goos, goarch: test.goarch, musl: test.musl}
		got, err := d.platform()
		if got != test.want || (err != nil) != (test.want == "") {
			t.Fatal("installer platform mapping was not preserved")
		}
	}
	isolatedDiscoveryHome(t)
	d, calls := discoveryTestServer(t, nil, false, nil)
	d.goos = "unsupported"
	if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || int(calls.Load()) != 0 {
		t.Fatal("unsupported platform contacted download server")
	}
}

func TestDiscoveryNetworkAndRedirectErrors(t *testing.T) {
	for _, response := range []string{"status", "malformed", "redirect"} {
		t.Run(response, func(t *testing.T) {
			isolatedDiscoveryHome(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch response {
				case "status":
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				case "malformed":
					_, _ = io.WriteString(w, "not JSON")
				case "redirect":
					http.Redirect(w, r, "http://localhost/forbidden", http.StatusFound)
				}
			}))
			defer server.Close()
			d := newCredentialDiscovery()
			d.manifestBase, d.goos, d.goarch = server.URL, "linux", "amd64"
			d.client.Transport = server.Client().Transport
			if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil {
				t.Fatal("manifest failure or insecure redirect was accepted")
			}
		})
	}
	isolatedDiscoveryHome(t)
	d := newCredentialDiscovery()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.resolve(ctx, config.Config{}); err == nil {
		t.Fatal("cancelled download was accepted")
	}
}

func TestDiscoveryLocalNativeWithoutCredentialsStops(t *testing.T) {
	home := isolatedDiscoveryHome(t)
	data, id, _ := syntheticNativeCLI(t)
	data = bytes.ReplaceAll(data, []byte(id), bytes.Repeat([]byte{'x'}, len(id)))
	writeTestCLI(t, filepath.Join(home, "path", "agy"), data)
	d, calls := discoveryTestServer(t, nil, false, nil)
	if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || int(calls.Load()) != 0 {
		t.Fatal("native CLI missing credential references did not stop discovery")
	}
}

func TestDiscoveryBrokenHomeSymlinkStops(t *testing.T) {
	home := isolatedDiscoveryHome(t)
	path := filepath.Join(home, ".local", "bin", "agy")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "missing-cli"), path); err != nil {
		t.Fatal(err)
	}
	d, calls := discoveryTestServer(t, nil, false, nil)
	if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || int(calls.Load()) != 0 {
		t.Fatal("broken installed CLI symlink triggered a download")
	}
}

func TestDiscoveryRejectsCorruptGzipTrailer(t *testing.T) {
	isolatedDiscoveryHome(t)
	payload := cliTestArchive(t, archiveTestMember{name: "antigravity", kind: tar.TypeReg, data: []byte("synthetic invalid native")})
	payload[len(payload)-1] ^= 1
	d, calls := discoveryTestServer(t, payload, true, nil)
	if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || !strings.Contains(err.Error(), "archive is corrupt") || int(calls.Load()) != 2 {
		t.Fatal("verified payload with corrupt gzip trailer was not rejected")
	}
}

func TestDiscoveryOversizedLocalCLIStopsBeforeRead(t *testing.T) {
	home := isolatedDiscoveryHome(t)
	path := filepath.Join(home, ".local", "bin", "agy")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxCLIBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	d, calls := discoveryTestServer(t, nil, false, nil)
	if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || int(calls.Load()) != 0 {
		t.Fatal("oversized installed CLI triggered extraction or download")
	}
}

func TestDiscoveryRejectsOversizedManifest(t *testing.T) {
	isolatedDiscoveryHome(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", int(maxManifestBytes)+1))
	}))
	defer server.Close()
	d := newCredentialDiscovery()
	d.manifestBase, d.goos, d.goarch = server.URL, "linux", "amd64"
	d.client.Transport = server.Client().Transport
	if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatal("unbounded manifest was accepted")
	}
}

func TestDiscoveryAmbiguousLocalNativeCLIStopsWithoutDownload(t *testing.T) {
	home := isolatedDiscoveryHome(t)
	data := syntheticAmbiguousNativeCLI(t)
	writeTestCLI(t, filepath.Join(home, "path", "agy"), data)
	d, calls := discoveryTestServer(t, nil, false, nil)
	if _, _, err := d.resolve(context.Background(), config.Config{}); err == nil || int(calls.Load()) != 0 {
		t.Fatal("ambiguous native credential pairs triggered fallback or download")
	}
}
