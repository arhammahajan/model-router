package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const maxArchiveMemberBytes = 128 * 1024 * 1024

var allowedArchiveFiles = map[string]bool{
	"routerd":                  true,
	"config.example.toml":      true,
	"cliproxyapi.example.yaml": true,
	"README.md":                true,
	"THIRD_PARTY_NOTICES":      true,
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("release check", flag.ContinueOnError)
	version := fs.String("version", "", "expected release version label")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(*version) == "" {
		return fmt.Errorf("--version is required")
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("expected exactly one archive path argument")
	}
	archivePath := fs.Arg(0)

	home, err := os.MkdirTemp("", "router-release-smoke-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)

	if err := extractAndVerifyArchive(archivePath, home); err != nil {
		return err
	}

	binary := filepath.Join(home, "routerd")
	configPath := filepath.Join(home, "config.example.toml")
	// No inherited API keys, dotenv, provider proxies, credential paths or cache.
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"XDG_CACHE_HOME=" + home,
		"PATH=/usr/bin:/bin",
	}

	invoke := func(wantCode int, cliArgs ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, cliArgs...)
		cmd.Dir = home
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else {
				return "", fmt.Errorf("CLI %v: %w", cliArgs, err)
			}
		}
		if code != wantCode {
			return "", fmt.Errorf("CLI %v: exit %d, expected %d (stderr: %s)", cliArgs, code, wantCode, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}

	gotVersion, err := invoke(0, "--version")
	if err != nil {
		return err
	}
	if gotVersion != "routerd "+*version {
		return fmt.Errorf("archive executable has wrong version %q, want %q", gotVersion, "routerd "+*version)
	}

	validOut, err := invoke(0, "--config", configPath, "--validate-config", "--offline")
	if err != nil {
		return err
	}
	if err := assertJSONEquals(validOut, map[string]any{"status": "valid", "network_checked": false}); err != nil {
		return fmt.Errorf("offline validation contract changed: %w", err)
	}

	offlineOut, err := invoke(4, "--config", configPath, "--check", "--offline")
	if err != nil {
		return err
	}
	if err := assertJSONEquals(offlineOut, map[string]any{
		"status":          "indeterminate",
		"network_checked": false,
		"reason":          "offline_readiness_requires_discovery",
	}); err != nil {
		return fmt.Errorf("offline readiness contract changed: %w", err)
	}

	// Exercise proxy discovery against a local fixture with dummy credentials.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer proxy-smoke-key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"opencode-go/smoke-model"}]}`))
	}))
	defer proxy.Close()

	smokeConfig := filepath.Join(home, "smoke.toml")
	smokeBody := fmt.Sprintf(
		"[server]\napi_keys = [\"replace-this-local-key\"]\n[providers.cliproxyapi]\nbase_url = \"%s/v1\"\napi_key = \"proxy-smoke-key\"\n",
		proxy.URL,
	)
	if err := os.WriteFile(smokeConfig, []byte(smokeBody), 0o600); err != nil {
		return err
	}

	port, err := pickFreeLoopbackPort()
	if err != nil {
		return err
	}

	proc := exec.Command(binary, "--config", smokeConfig, "--host", "127.0.0.1", "--port", fmt.Sprintf("%d", port))
	proc.Dir = home
	proc.Env = env
	if err := proc.Start(); err != nil {
		return fmt.Errorf("start archive server: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- proc.Wait() }()

	client := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	getJSON := func(reqPath string, authenticated bool) (int, map[string]any, error) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, reqPath), nil)
		if err != nil {
			return 0, nil, err
		}
		if authenticated {
			req.Header.Set("Authorization", "Bearer replace-this-local-key")
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		if err != nil {
			return resp.StatusCode, nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			return resp.StatusCode, nil, err
		}
		return resp.StatusCode, decoded, nil
	}

	var checkErr error
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-waitCh:
			return fmt.Errorf("archive server exited before readiness: %v", err)
		default:
		}
		status, ready, err := getJSON("/readyz", false)
		if err == nil && status == http.StatusOK && ready["status"] == "ready" {
			break
		}
		if time.Now().After(deadline) {
			checkErr = fmt.Errorf("archive server did not become ready")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if checkErr == nil {
		status, index, err := getJSON("/", false)
		if err != nil || status != http.StatusOK || index["version"] != *version {
			checkErr = fmt.Errorf("HTTP index and CLI release versions disagree (status=%d, err=%v)", status, err)
		}
	}
	if checkErr == nil {
		status, _, err := getJSON("/v1/models", false)
		if err != nil || status != http.StatusUnauthorized {
			checkErr = fmt.Errorf("archive model listing bypassed authentication (status=%d, err=%v)", status, err)
		}
	}
	if checkErr == nil {
		status, models, err := getJSON("/v1/models", true)
		if err != nil || status != http.StatusOK {
			checkErr = fmt.Errorf("archive model listing failed (status=%d, err=%v)", status, err)
		} else {
			ids := extractModelIDs(models)
			if len(ids) != 1 || ids[0] != "opencode-go/smoke-model" {
				checkErr = fmt.Errorf("archive model listing lost the discovered proxy route: %v", ids)
			}
		}
	}

	_ = proc.Process.Signal(syscall.SIGTERM)
	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-time.After(5 * time.Second):
		_ = proc.Process.Kill()
		<-waitCh
		return fmt.Errorf("archive server did not stop promptly")
	}
	if checkErr != nil {
		return checkErr
	}
	if waitErr != nil {
		return fmt.Errorf("archive server did not shut down cleanly: %w", waitErr)
	}

	fmt.Printf("archive CLI, version, cached HTTP reads, auth and shutdown: OK (%s)\n", *version)
	return nil
}

func extractAndVerifyArchive(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	prefix := ""
	found := map[string]bool{}
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		count++
		if hdr.Typeflag != tar.TypeReg || strings.HasPrefix(hdr.Name, "/") || hdr.Size < 0 || hdr.Size > maxArchiveMemberBytes {
			return fmt.Errorf("unexpected archive entry %q", hdr.Name)
		}
		parts := strings.Split(hdr.Name, "/")
		if len(parts) != 2 || parts[0] == "" || parts[0] == "." || parts[0] == ".." || path.Clean(hdr.Name) != hdr.Name {
			return fmt.Errorf("unexpected archive entry path %q", hdr.Name)
		}
		if prefix == "" {
			prefix = parts[0]
		} else if parts[0] != prefix {
			return fmt.Errorf("archive has multiple top-level directories (%q and %q)", prefix, parts[0])
		}
		name := parts[1]
		if !allowedArchiveFiles[name] || found[name] {
			return fmt.Errorf("unexpected or duplicate archive entry %q", hdr.Name)
		}
		found[name] = true
		data, err := io.ReadAll(io.LimitReader(tr, maxArchiveMemberBytes+1))
		if err != nil {
			return err
		}
		if int64(len(data)) > maxArchiveMemberBytes {
			return fmt.Errorf("archive entry %q exceeds size limit", hdr.Name)
		}
		mode := os.FileMode(0o600)
		if name == "routerd" {
			mode = 0o700
		}
		if err := os.WriteFile(filepath.Join(destDir, name), data, mode); err != nil {
			return err
		}
	}
	if count != len(allowedArchiveFiles) || len(found) != len(allowedArchiveFiles) {
		return fmt.Errorf("expected exactly %d files under one archive directory, got %d", len(allowedArchiveFiles), count)
	}
	return nil
}

func assertJSONEquals(raw string, want map[string]any) error {
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return err
	}
	gotBytes, err := json.Marshal(got)
	if err != nil {
		return err
	}
	wantBytes, err := json.Marshal(want)
	if err != nil {
		return err
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		return fmt.Errorf("got %s, want %s", gotBytes, wantBytes)
	}
	return nil
}

func extractModelIDs(payload map[string]any) []string {
	rows, _ := payload["data"].([]any)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if obj, ok := row.(map[string]any); ok {
			if id, ok := obj["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func pickFreeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
