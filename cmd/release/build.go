package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var (
	supportedTargets = []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"}
	licensePrefixes  = []string{"LICENSE", "LICENCE", "COPYING", "NOTICE"}
	versionPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,95}$`)
)

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type linkedModule struct {
	Path    string
	Version string
	Dir     string
}

type archiveEntry struct {
	Data []byte
	Mode int64
}

type manifestArtifact struct {
	File   string `json:"file"`
	Target string `json:"target"`
	SHA256 string `json:"sha256"`
}

type releaseManifest struct {
	Version        string             `json:"version"`
	SourceRevision string             `json:"source_revision"`
	SourceDirty    bool               `json:"source_dirty"`
	GoVersion      string             `json:"go_version"`
	CGOEnabled     bool               `json:"cgo_enabled"`
	Dependencies   map[string]string  `json:"dependencies"`
	Artifacts      []manifestArtifact `json:"artifacts"`
}

func runBuild(args []string) error {
	fs := flag.NewFlagSet("release build", flag.ContinueOnError)
	goBin := fs.String("go", "go", "Go 1.26.10+ executable")
	version := fs.String("version", "", "short filename-safe release version label")
	outDir := fs.String("out", "", "empty output directory for release artifacts")
	noCGO := fs.Bool("no-cgo", false, "disable CGO (allows cross-compilation without a C toolchain, disables embedded CLIProxyAPI dynamic plugins)")
	var rawTargets multiFlag
	fs.Var(&rawTargets, "target", "target OS/ARCH (repeatable)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if !versionPattern.MatchString(*version) {
		return fmt.Errorf("--version must be a short filename-safe label")
	}
	if strings.TrimSpace(*outDir) == "" {
		return fmt.Errorf("--out is required")
	}
	root, err := findRepoRoot()
	if err != nil {
		return err
	}
	out, err := filepath.Abs(*outDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	cgoEnabled := !*noCGO
	baseEnv := sanitizedBuildEnv(cgoEnabled)
	hostOS, err := runCmd(root, baseEnv, *goBin, "env", "GOHOSTOS")
	if err != nil {
		return err
	}
	hostArch, err := runCmd(root, baseEnv, *goBin, "env", "GOHOSTARCH")
	if err != nil {
		return err
	}
	hostTarget := hostOS + "/" + hostArch

	targets, err := resolveTargets(rawTargets, hostTarget, cgoEnabled, os.Getenv("CC"))
	if err != nil {
		return err
	}

	expected := map[string]bool{
		"manifest.json": true,
		"SHA256SUMS":    true,
	}
	for _, target := range targets {
		expected[fmt.Sprintf("routerd-%s-%s.tar.gz", *version, strings.ReplaceAll(target, "/", "-"))] = true
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !expected[entry.Name()] {
			return fmt.Errorf("output directory %s contains another release or target set (%s); use an empty directory", out, entry.Name())
		}
	}

	goroot, err := runCmd(root, baseEnv, *goBin, "env", "GOROOT")
	if err != nil {
		return err
	}
	sourceRev, err := runCmd(root, baseEnv, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	porcelain, err := runCmd(root, baseEnv, "git", "status", "--porcelain", "--", ".")
	if err != nil {
		return err
	}
	goVersion, err := runCmd(root, baseEnv, *goBin, "version")
	if err != nil {
		return err
	}

	exampleConfig, err := os.ReadFile(filepath.Join(root, "config.example.toml"))
	if err != nil {
		return err
	}
	exampleProxyConfig, err := os.ReadFile(filepath.Join(root, "cliproxyapi.example.yaml"))
	if err != nil {
		return err
	}
	releaseReadme, err := os.ReadFile(filepath.Join(root, "RELEASE.md"))
	if err != nil {
		return err
	}

	staging, err := os.MkdirTemp(out, ".release-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	allDeps := map[string]string{}
	manifest := releaseManifest{
		Version:        *version,
		SourceRevision: sourceRev,
		SourceDirty:    porcelain != "",
		GoVersion:      goVersion,
		CGOEnabled:     cgoEnabled,
		Dependencies:   allDeps,
		Artifacts:      make([]manifestArtifact, 0, len(targets)),
	}

	for _, target := range targets {
		parts := strings.SplitN(target, "/", 2)
		goos, goarch := parts[0], parts[1]
		targetEnv := withEnv(baseEnv, "GOOS="+goos, "GOARCH="+goarch, "GOAMD64=v1")

		modules, err := collectLinkedModules(root, targetEnv, *goBin)
		if err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
		notices, err := buildNotices(goroot, modules)
		if err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
		for _, mod := range modules {
			allDeps[mod.Path] = mod.Version
		}

		binaryPath := filepath.Join(staging, "routerd")
		ldflags := fmt.Sprintf("-s -w -buildid= -X github.com/arhammahajan/router/internal/server.BuildVersion=%s", *version)
		cmd := exec.Command(*goBin, "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", binaryPath, "./cmd/routerd")
		cmd.Dir = root
		cmd.Env = targetEnv
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go build %s: %w", target, err)
		}
		binaryBytes, err := os.ReadFile(binaryPath)
		if err != nil {
			return err
		}

		prefix := fmt.Sprintf("routerd-%s-%s-%s", *version, goos, goarch)
		archiveName := prefix + ".tar.gz"
		archivePath := filepath.Join(staging, archiveName)
		err = writeDeterministicTarGz(archivePath, prefix, map[string]archiveEntry{
			"routerd":                  {Data: binaryBytes, Mode: 0o755},
			"config.example.toml":      {Data: exampleConfig, Mode: 0o600},
			"cliproxyapi.example.yaml": {Data: exampleProxyConfig, Mode: 0o600},
			"README.md":                {Data: releaseReadme, Mode: 0o644},
			"THIRD_PARTY_NOTICES":      {Data: notices, Mode: 0o644},
		})
		if err != nil {
			return err
		}
		packedBytes, err := os.ReadFile(archivePath)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(packedBytes)
		manifest.Artifacts = append(manifest.Artifacts, manifestArtifact{
			File:   archiveName,
			Target: target,
			SHA256: hex.EncodeToString(sum[:]),
		})
		fmt.Printf("built %s\n", archiveName)
	}

	var checksums strings.Builder
	for _, item := range manifest.Artifacts {
		fmt.Fprintf(&checksums, "%s  %s\n", item.SHA256, item.File)
	}
	if err := os.WriteFile(filepath.Join(staging, "SHA256SUMS"), []byte(checksums.String()), 0o644); err != nil {
		return err
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), manifestBytes, 0o644); err != nil {
		return err
	}

	for _, item := range manifest.Artifacts {
		if err := os.Rename(filepath.Join(staging, item.File), filepath.Join(out, item.File)); err != nil {
			return err
		}
	}
	if err := os.Rename(filepath.Join(staging, "SHA256SUMS"), filepath.Join(out, "SHA256SUMS")); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(staging, "manifest.json"), filepath.Join(out, "manifest.json")); err != nil {
		return err
	}
	return nil
}

func resolveTargets(rawTargets []string, hostTarget string, cgoEnabled bool, ccEnv string) ([]string, error) {
	var targets []string
	if len(rawTargets) > 0 {
		seen := map[string]bool{}
		for _, t := range rawTargets {
			if !slices.Contains(supportedTargets, t) {
				return nil, fmt.Errorf("unsupported target %q (supported: %s)", t, strings.Join(supportedTargets, ", "))
			}
			if !seen[t] {
				seen[t] = true
				targets = append(targets, t)
			}
		}
		sort.Strings(targets)
	} else if cgoEnabled {
		if !slices.Contains(supportedTargets, hostTarget) {
			return nil, fmt.Errorf("host target %q is not in supported targets (%s); pass --no-cgo --target ...", hostTarget, strings.Join(supportedTargets, ", "))
		}
		targets = []string{hostTarget}
	} else {
		targets = append([]string(nil), supportedTargets...)
	}

	if cgoEnabled && strings.TrimSpace(ccEnv) == "" {
		var foreign []string
		for _, t := range targets {
			if t != hostTarget {
				foreign = append(foreign, t)
			}
		}
		if len(foreign) > 0 {
			return nil, fmt.Errorf("cross-compiling %v from %s with CGO enabled requires CC to be set, or pass --no-cgo", foreign, hostTarget)
		}
	}
	return targets, nil
}

func sanitizedBuildEnv(cgoEnabled bool) []string {
	drop := map[string]bool{
		"CGO_ENABLED": true, "GOENV": true, "GOFLAGS": true,
		"GOTOOLCHAIN": true, "GOWORK": true, "GOPROXY": true, "GOSUMDB": true,
		"GOOS": true, "GOARCH": true, "GOARM": true, "GOAMD64": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !drop[key] {
			env = append(env, kv)
		}
	}
	cgoVal := "0"
	if cgoEnabled {
		cgoVal = "1"
	}
	return append(env,
		"CGO_ENABLED="+cgoVal,
		"GOENV=off",
		"GOFLAGS=",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOPROXY=off",
		"GOSUMDB=off",
	)
}

func withEnv(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

func runCmd(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func collectLinkedModules(root string, env []string, goBin string) ([]linkedModule, error) {
	raw, err := runCmd(root, env, goBin, "list", "-deps", "-json", "./cmd/routerd")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	byPath := map[string]linkedModule{}
	for {
		var pkg struct {
			Module *struct {
				Path    string `json:"Path"`
				Version string `json:"Version"`
				Dir     string `json:"Dir"`
				Main    bool   `json:"Main"`
			} `json:"Module"`
		}
		err := dec.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse go list -deps -json: %w", err)
		}
		if pkg.Module == nil || pkg.Module.Main {
			continue
		}
		if pkg.Module.Dir == "" {
			return nil, fmt.Errorf("linked module %s has no local Dir", pkg.Module.Path)
		}
		byPath[pkg.Module.Path] = linkedModule{
			Path:    pkg.Module.Path,
			Version: pkg.Module.Version,
			Dir:     pkg.Module.Dir,
		}
	}
	out := make([]linkedModule, 0, len(byPath))
	for _, m := range byPath {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func buildNotices(goroot string, modules []linkedModule) ([]byte, error) {
	goLicense, err := os.ReadFile(filepath.Join(goroot, "LICENSE"))
	if err != nil {
		return nil, fmt.Errorf("read Go LICENSE: %w", err)
	}
	sections := []string{"Go standard library and runtime\n\n" + strings.TrimSpace(string(goLicense))}
	for _, mod := range modules {
		entries, err := os.ReadDir(mod.Dir)
		if err != nil {
			return nil, fmt.Errorf("read module dir %s: %w", mod.Path, err)
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			upper := strings.ToUpper(e.Name())
			for _, pfx := range licensePrefixes {
				if strings.HasPrefix(upper, pfx) {
					names = append(names, e.Name())
					break
				}
			}
		}
		sort.Strings(names)
		if len(names) == 0 {
			return nil, fmt.Errorf("missing license/notice file for linked module %s %s", mod.Path, mod.Version)
		}
		texts := make([]string, 0, len(names))
		for _, name := range names {
			b, err := os.ReadFile(filepath.Join(mod.Dir, name))
			if err != nil {
				return nil, fmt.Errorf("read %s/%s: %w", mod.Path, name, err)
			}
			texts = append(texts, strings.TrimSpace(string(b)))
		}
		header := strings.TrimSpace(mod.Path + " " + mod.Version)
		sections = append(sections, header+"\n\n"+strings.Join(texts, "\n\n"))
	}
	return []byte(strings.Join(sections, "\n\n") + "\n"), nil
}

func writeDeterministicTarGz(dest, prefix string, files map[string]archiveEntry) error {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.Name = ""
	gz.ModTime = time.Time{}

	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := files[name]
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     prefix + "/" + name,
			Size:     int64(len(entry.Data)),
			Mode:     entry.Mode,
			ModTime:  time.Unix(0, 0).UTC(),
			Uid:      0,
			Gid:      0,
			Uname:    "",
			Gname:    "",
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(entry.Data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return os.WriteFile(dest, buf.Bytes(), 0o644)
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate go.mod above working directory")
		}
		dir = parent
	}
}
