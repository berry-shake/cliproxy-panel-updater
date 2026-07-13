# Panel Updater Plugin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a standalone CLIProxyAPI C-ABI plugin that exposes a browser page and an authenticated management action for manually replacing `management.html` from the repository configured at `remote-management.panel-github-repository`.

**Architecture:** Keep all business logic in pure-Go packages under `internal/`: `internal/plugin` resolves the host config and handles plugin RPC/management routes, while `internal/updater` owns release discovery, digest verification, fallback download, concurrency control, and atomic persistence. The root `main.go` is the only cgo file; it implements ABI v1 and adapts `host.http.do` to the updater's `HTTPDoer` interface so the host supplies proxy behavior and request logging.

**Tech Stack:** Go 1.26, cgo `-buildmode=c-shared`, CLIProxyAPI plugin ABI/schema v1, `github.com/router-for-me/CLIProxyAPI/v7` v7.2.71, `gopkg.in/yaml.v3`, embedded HTML/CSS/JavaScript, GitHub Actions.

## Global Constraints

- Repository root: `/opt/data/goland_data/cliproxy-panel-updater`.
- Module path: `github.com/berry-shake/cliproxy-panel-updater`.
- Go version: `1.26`.
- CLIProxyAPI dependency: `github.com/router-for-me/CLIProxyAPI/v7 v7.2.71`.
- Plugin ABI version: `1`; RPC schema version: `1`.
- Plugin ID: `panel-updater`; host-side filename must be `panel-updater-v<version>.<so|dylib|dll>`.
- Only capability: `management_api`.
- Read `remote-management.panel-github-repository` directly from the host config selected by `--config`/`-config`; do not add a duplicate plugin setting.
- Download only through `host.http.do`; do not construct a plugin-side `http.Client`.
- Target only `management.html`; GitHub failures may use `https://cpamc.router-for.me/` only when the local file is missing. Preserve an existing panel on GitHub failure; a digest mismatch is always a hard failure and must not fall back.
- Static directory resolution: `MANAGEMENT_STATIC_PATH` → `WRITABLE_PATH`/`writable_path` + `/static` → `<host config directory>/static`.
- Writes must use a temporary file in the destination directory, mode `0644`, then `os.Rename`.
- The browser resource is unauthenticated, but status/update calls must go through host-authenticated `/v0/management/...` endpoints. Never embed or log the management key.
- Comments and user-facing documentation in the repository are English.
- Run `gofmt`; use one-line commit messages.
- Do not add scheduling, rollback, version pinning, command-line commands, or cross-process locks.

---

## File Map

- `go.mod` — standalone module declaration and pinned dependencies.
- `go.sum` — generated dependency checksums.
- `internal/plugin/hostconfig.go` — mirror the host's `--config` parsing, read the repository scalar, and resolve the target static directory.
- `internal/plugin/hostconfig_test.go` — table tests for argument parsing, YAML reading, defaults, and environment precedence.
- `internal/updater/updater.go` — release URL conversion, `host.http.do` abstraction, update/fallback flow, SHA-256 validation, in-process lock, and atomic writes.
- `internal/updater/updater_test.go` — fake-host HTTP tests for success, no-op, fallback, integrity failure, and concurrency.
- `internal/plugin/register.go` — lifecycle RPC dispatch, registration metadata, capabilities, route declarations, and envelope helpers.
- `internal/plugin/management.go` — status/update handlers and JSON response models.
- `internal/plugin/management_test.go` — RPC registration and management route behavior tests.
- `internal/plugin/page.go` — embed and return the browser page.
- `internal/plugin/page.html` — self-contained UI; stores the management key only in browser localStorage.
- `main.go` — ABI v1 cgo export and `host.http.do` adapter.
- `main_test.go` — pure decoding tests for host callback envelopes.
- `.github/workflows/build.yml` — formatting/tests plus five-platform c-shared build and release upload.
- `README.md` — installation, configuration, usage, security, and local build instructions.

---

### Task 1: Resolve the Host Configuration Without Plugin-Specific Settings

**Files:**
- Create: `go.mod`
- Create: `internal/plugin/hostconfig.go`
- Create: `internal/plugin/hostconfig_test.go`
- Generate: `go.sum`

**Interfaces:**
- Produces: `type HostConfig` with fields `ConfigFile`, `ConfigReadable`, `ConfigError`, `PanelGitHubRepository`, and `StaticDir`.
- Produces: `type HostConfigEnvironment` for deterministic tests.
- Produces: `func ResolveHostConfig(env HostConfigEnvironment) HostConfig`.
- Produces: `func ResolveCurrentHostConfig() HostConfig` for management handlers.
- Consumed later by: `internal/plugin/management.go`.

- [ ] **Step 1: Create the module manifest**

Create `go.mod`:

```go
module github.com/berry-shake/cliproxy-panel-updater

go 1.26

require (
	github.com/router-for-me/CLIProxyAPI/v7 v7.2.71
	gopkg.in/yaml.v3 v3.0.1
)
```

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
go mod download
```

Expected: exit code 0 and `go.sum` created.

- [ ] **Step 2: Write the failing host-config tests**

Create `internal/plugin/hostconfig_test.go`:

```go
package plugin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHostConfigReadsConfiguredRepository(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.pro.yaml")
	raw := []byte("remote-management:\n  panel-github-repository: https://github.com/acme/panel\n")

	got := ResolveHostConfig(HostConfigEnvironment{
		Args: []string{"server", "--config", configPath},
		Getwd: func() (string, error) { return "/unused", nil },
		Getenv: func(string) string { return "" },
		ReadFile: func(path string) ([]byte, error) {
			if path != configPath {
				t.Fatalf("ReadFile(%q), want %q", path, configPath)
			}
			return raw, nil
		},
		Stat: os.Stat,
	})

	if got.ConfigFile != configPath {
		t.Fatalf("ConfigFile = %q, want %q", got.ConfigFile, configPath)
	}
	if !got.ConfigReadable || got.ConfigError != "" {
		t.Fatalf("config status = readable:%v error:%q", got.ConfigReadable, got.ConfigError)
	}
	if got.PanelGitHubRepository != "https://github.com/acme/panel" {
		t.Fatalf("PanelGitHubRepository = %q", got.PanelGitHubRepository)
	}
	if got.StaticDir != filepath.Join(dir, "static") {
		t.Fatalf("StaticDir = %q", got.StaticDir)
	}
}

func TestResolveHostConfigMatchesHostConfigFlagForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "long separate", args: []string{"server", "--config", "config.pro.yaml"}, want: "config.pro.yaml"},
		{name: "short separate", args: []string{"server", "-config", "config.pro.yaml"}, want: "config.pro.yaml"},
		{name: "long equals", args: []string{"server", "--config=config.pro.yaml"}, want: "config.pro.yaml"},
		{name: "short equals", args: []string{"server", "-config=config.pro.yaml"}, want: "config.pro.yaml"},
		{name: "stop parsing", args: []string{"server", "--", "--config", "ignored.yaml"}, want: filepath.Join("/work", "config.yaml")},
		{name: "default", args: []string{"server"}, want: filepath.Join("/work", "config.yaml")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveHostConfig(HostConfigEnvironment{
				Args: tt.args,
				Getwd: func() (string, error) { return "/work", nil },
				Getenv: func(string) string { return "" },
				ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
				Stat: os.Stat,
			})
			if got.ConfigFile != tt.want {
				t.Fatalf("ConfigFile = %q, want %q", got.ConfigFile, tt.want)
			}
		})
	}
}

func TestResolveHostConfigStaticDirectoryPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "management file override",
			env: map[string]string{"MANAGEMENT_STATIC_PATH": "/srv/ui/management.html", "WRITABLE_PATH": "/ignored"},
			want: "/srv/ui",
		},
		{
			name: "management directory override",
			env: map[string]string{"MANAGEMENT_STATIC_PATH": "/srv/ui"},
			want: "/srv/ui",
		},
		{
			name: "uppercase writable path",
			env: map[string]string{"WRITABLE_PATH": "/data"},
			want: filepath.Join("/data", "static"),
		},
		{
			name: "lowercase writable path",
			env: map[string]string{"writable_path": "/lower"},
			want: filepath.Join("/lower", "static"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveHostConfig(HostConfigEnvironment{
				Args: []string{"server", "--config", "/etc/cliproxy/config.yaml"},
				Getwd: func() (string, error) { return "/work", nil },
				Getenv: func(key string) string { return tt.env[key] },
				ReadFile: func(string) ([]byte, error) { return []byte("{}"), nil },
				Stat: os.Stat,
			})
			if got.StaticDir != tt.want {
				t.Fatalf("StaticDir = %q, want %q", got.StaticDir, tt.want)
			}
		})
	}
}

func TestResolveHostConfigFallsBackOnReadOrYAMLError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		readFile func(string) ([]byte, error)
	}{
		{name: "read failure", readFile: func(string) ([]byte, error) { return nil, errors.New("denied") }},
		{name: "yaml failure", readFile: func(string) ([]byte, error) { return []byte("remote-management: ["), nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveHostConfig(HostConfigEnvironment{
				Args: []string{"server"},
				Getwd: func() (string, error) { return "/work", nil },
				Getenv: func(string) string { return "" },
				ReadFile: tt.readFile,
				Stat: os.Stat,
			})
			if got.ConfigReadable {
				t.Fatal("ConfigReadable = true, want false")
			}
			if got.ConfigError == "" {
				t.Fatal("ConfigError is empty")
			}
			if got.PanelGitHubRepository != DefaultPanelGitHubRepository {
				t.Fatalf("PanelGitHubRepository = %q", got.PanelGitHubRepository)
			}
		})
	}
}
```

- [ ] **Step 3: Run the tests and verify the expected compile failure**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
go test ./internal/plugin -run 'TestResolveHostConfig' -v
```

Expected: FAIL with undefined identifiers such as `ResolveHostConfig`, `HostConfigEnvironment`, and `DefaultPanelGitHubRepository`.

- [ ] **Step 4: Implement host config discovery and parsing**

Create `internal/plugin/hostconfig.go`:

```go
package plugin

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"

const managementAssetName = "management.html"

type HostConfig struct {
	ConfigFile                string
	ConfigReadable            bool
	ConfigError               string
	PanelGitHubRepository     string
	StaticDir                 string
}

type HostConfigEnvironment struct {
	Args     []string
	Getwd    func() (string, error)
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	Stat     func(string) (fs.FileInfo, error)
}

func ResolveCurrentHostConfig() HostConfig {
	return ResolveHostConfig(HostConfigEnvironment{
		Args:     os.Args,
		Getwd:    os.Getwd,
		Getenv:   os.Getenv,
		ReadFile: os.ReadFile,
		Stat:     os.Stat,
	})
}

func ResolveHostConfig(env HostConfigEnvironment) HostConfig {
	configFile := resolveConfigFile(env.Args, env.Getwd)
	out := HostConfig{
		ConfigFile:            configFile,
		PanelGitHubRepository: DefaultPanelGitHubRepository,
		StaticDir:             resolveStaticDir(configFile, env.Getenv, env.Stat),
	}

	raw, errRead := env.ReadFile(configFile)
	if errRead != nil {
		out.ConfigError = fmt.Sprintf("read host config: %v", errRead)
		return out
	}

	var parsed struct {
		RemoteManagement struct {
			PanelGitHubRepository string `yaml:"panel-github-repository"`
		} `yaml:"remote-management"`
	}
	if errUnmarshal := yaml.Unmarshal(raw, &parsed); errUnmarshal != nil {
		out.ConfigError = fmt.Sprintf("parse host config: %v", errUnmarshal)
		return out
	}

	out.ConfigReadable = true
	if repository := strings.TrimSpace(parsed.RemoteManagement.PanelGitHubRepository); repository != "" {
		out.PanelGitHubRepository = repository
	}
	return out
}

func resolveConfigFile(args []string, getwd func() (string, error)) string {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			return defaultConfigFile(getwd)
		case arg == "-config" || arg == "--config":
			if index+1 < len(args) {
				return args[index+1]
			}
			return defaultConfigFile(getwd)
		case strings.HasPrefix(arg, "-config="):
			return strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "--config="):
			return strings.TrimPrefix(arg, "--config=")
		}
	}
	return defaultConfigFile(getwd)
}

func defaultConfigFile(getwd func() (string, error)) string {
	wd, errGetwd := getwd()
	if errGetwd != nil {
		return "config.yaml"
	}
	return filepath.Join(wd, "config.yaml")
}

func resolveStaticDir(configFile string, getenv func(string) string, stat func(string) (fs.FileInfo, error)) string {
	if override := strings.TrimSpace(getenv("MANAGEMENT_STATIC_PATH")); override != "" {
		cleaned := filepath.Clean(override)
		if strings.EqualFold(filepath.Base(cleaned), managementAssetName) {
			return filepath.Dir(cleaned)
		}
		return cleaned
	}

	for _, key := range []string{"WRITABLE_PATH", "writable_path"} {
		if writable := strings.TrimSpace(getenv(key)); writable != "" {
			return filepath.Join(filepath.Clean(writable), "static")
		}
	}

	base := filepath.Dir(configFile)
	if info, errStat := stat(configFile); errStat == nil && info.IsDir() {
		base = configFile
	}
	return filepath.Join(base, "static")
}
```

- [ ] **Step 5: Format and run the host-config tests**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
gofmt -w internal/plugin/hostconfig.go internal/plugin/hostconfig_test.go
go test ./internal/plugin -run 'TestResolveHostConfig' -v
```

Expected: all four test functions PASS.

- [ ] **Step 6: Commit the host-config unit**

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
git add go.mod go.sum internal/plugin/hostconfig.go internal/plugin/hostconfig_test.go
git commit -m "feat: resolve host panel configuration"
```

---

### Task 2: Implement the Verified Panel Updater

**Files:**
- Create: `internal/updater/updater.go`
- Create: `internal/updater/updater_test.go`

**Interfaces:**
- Produces: `type HTTPDoer` with `Do(context.Context, string, HTTPRequest) (HTTPResponse, error)`.
- Produces: `func New(HTTPDoer) *Updater`.
- Produces: `func (*Updater) Update(context.Context, UpdateRequest) (Result, error)`.
- Produces: `func ResolveReleaseURL(string) string` and `func FileSHA256(string) (string, error)`.
- Produces sentinels: `ErrUpdateInProgress`, `ErrDigestMismatch`.
- Consumed later by: management handlers and the root host callback adapter.

- [ ] **Step 1: Write URL-resolution and update-flow tests**

Create `internal/updater/updater_test.go`:

```go
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeDoer struct {
	mu        sync.Mutex
	responses map[string]HTTPResponse
	errors    map[string]error
	calls     []HTTPRequest
	fn        func(HTTPRequest) (HTTPResponse, error)
}

func (f *fakeDoer) Do(_ context.Context, _ string, req HTTPRequest) (HTTPResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	fn := f.fn
	resp := f.responses[req.URL]
	err := f.errors[req.URL]
	f.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return resp, err
}

func (f *fakeDoer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func testSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func releaseBody(t *testing.T, downloadURL, digest string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(map[string]any{
		"assets": []map[string]string{{
			"name":                 AssetName,
			"browser_download_url": downloadURL,
			"digest":               digest,
		}},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func TestResolveReleaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"", DefaultReleaseURL},
		{"https://github.com/acme/panel", "https://api.github.com/repos/acme/panel/releases/latest"},
		{"https://github.com/acme/panel.git/", "https://api.github.com/repos/acme/panel/releases/latest"},
		{"https://api.github.com/repos/acme/panel", "https://api.github.com/repos/acme/panel/releases/latest"},
		{"https://api.github.com/repos/acme/panel/releases/latest", "https://api.github.com/repos/acme/panel/releases/latest"},
		{"https://example.com/acme/panel", DefaultReleaseURL},
		{"not a url", DefaultReleaseURL},
	}

	for _, tt := range tests {
		if got := ResolveReleaseURL(tt.input); got != tt.want {
			t.Errorf("ResolveReleaseURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestUpdateDownloadsAndAtomicallyReplacesPanel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := []byte("old panel")
	if errWrite := os.WriteFile(filepath.Join(dir, AssetName), old, 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	fresh := []byte("new panel")
	downloadURL := "https://downloads.example/management.html"
	releaseURL := ResolveReleaseURL("https://github.com/acme/panel")
	doer := &fakeDoer{responses: map[string]HTTPResponse{
		releaseURL: {StatusCode: http.StatusOK, Body: releaseBody(t, downloadURL, "sha256:"+testSHA256(fresh))},
		downloadURL: {StatusCode: http.StatusOK, Body: fresh},
	}}

	result, errUpdate := New(doer).Update(context.Background(), UpdateRequest{
		StaticDir:            dir,
		PanelGitHubRepository: "https://github.com/acme/panel",
		HostCallbackID:       "callback-1",
	})
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if !result.Updated || result.Source != SourceGitHub || result.Hash != testSHA256(fresh) {
		t.Fatalf("result = %+v", result)
	}
	got, errRead := os.ReadFile(filepath.Join(dir, AssetName))
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(got) != string(fresh) {
		t.Fatalf("panel = %q, want %q", got, fresh)
	}
}

func TestUpdateSkipsDownloadWhenDigestMatches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	current := []byte("current panel")
	if errWrite := os.WriteFile(filepath.Join(dir, AssetName), current, 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	releaseURL := ResolveReleaseURL(DefaultPanelRepository)
	doer := &fakeDoer{responses: map[string]HTTPResponse{
		releaseURL: {
			StatusCode: http.StatusOK,
			Body:       releaseBody(t, "https://downloads.example/management.html", "sha256:"+testSHA256(current)),
		},
	}}

	result, errUpdate := New(doer).Update(context.Background(), UpdateRequest{
		StaticDir:            dir,
		PanelGitHubRepository: DefaultPanelRepository,
	})
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if result.Updated || result.Source != SourceUpToDate {
		t.Fatalf("result = %+v", result)
	}
	if doer.callCount() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", doer.callCount())
	}
}

func TestUpdateUsesFallbackWhenGitHubFailsAndLocalFileIsMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, AssetName)
	fallback := []byte("fallback panel")
	releaseURL := ResolveReleaseURL(DefaultPanelRepository)
	doer := &fakeDoer{
		responses: map[string]HTTPResponse{
			FallbackURL: {StatusCode: http.StatusOK, Body: fallback},
		},
		errors: map[string]error{releaseURL: errors.New("github unavailable")},
	}

	result, errUpdate := New(doer).Update(context.Background(), UpdateRequest{
		StaticDir:            dir,
		PanelGitHubRepository: DefaultPanelRepository,
	})
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if !result.Updated || result.Source != SourceFallback || !strings.Contains(result.Message, "unverified") {
		t.Fatalf("result = %+v", result)
	}
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(got) != string(fallback) {
		t.Fatalf("panel = %q", got)
	}
}

func TestUpdateLeavesExistingFileWhenGitHubFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, AssetName)
	old := []byte("old panel")
	if errWrite := os.WriteFile(path, old, 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	releaseURL := ResolveReleaseURL(DefaultPanelRepository)
	doer := &fakeDoer{
		responses: map[string]HTTPResponse{
			FallbackURL: {StatusCode: http.StatusOK, Body: []byte("must not be used")},
		},
		errors: map[string]error{releaseURL: errors.New("github unavailable")},
	}

	_, errUpdate := New(doer).Update(context.Background(), UpdateRequest{
		StaticDir:             dir,
		PanelGitHubRepository: DefaultPanelRepository,
	})
	if errUpdate == nil {
		t.Fatal("Update returned nil error")
	}
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(got) != string(old) {
		t.Fatalf("panel changed to %q", got)
	}
	for _, call := range doer.calls {
		if call.URL == FallbackURL {
			t.Fatal("fallback was called while a local panel existed")
		}
	}
}

func TestUpdateRejectsDigestMismatchWithoutFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := []byte("old panel")
	path := filepath.Join(dir, AssetName)
	if errWrite := os.WriteFile(path, old, 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	downloadURL := "https://downloads.example/management.html"
	releaseURL := ResolveReleaseURL(DefaultPanelRepository)
	doer := &fakeDoer{responses: map[string]HTTPResponse{
		releaseURL: {StatusCode: http.StatusOK, Body: releaseBody(t, downloadURL, "sha256:"+testSHA256([]byte("expected")))},
		downloadURL: {StatusCode: http.StatusOK, Body: []byte("tampered")},
		FallbackURL: {StatusCode: http.StatusOK, Body: []byte("must not be used")},
	}}

	_, errUpdate := New(doer).Update(context.Background(), UpdateRequest{
		StaticDir:            dir,
		PanelGitHubRepository: DefaultPanelRepository,
	})
	if !errors.Is(errUpdate, ErrDigestMismatch) {
		t.Fatalf("error = %v, want ErrDigestMismatch", errUpdate)
	}
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(got) != string(old) {
		t.Fatalf("panel changed to %q", got)
	}
	for _, call := range doer.calls {
		if call.URL == FallbackURL {
			t.Fatal("fallback was called after digest mismatch")
		}
	}
}

func TestUpdateRejectsConcurrentAttempt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	current := []byte("current")
	if errWrite := os.WriteFile(filepath.Join(dir, AssetName), current, 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	doer := &fakeDoer{fn: func(req HTTPRequest) (HTTPResponse, error) {
		once.Do(func() { close(started) })
		<-release
		return HTTPResponse{StatusCode: http.StatusOK, Body: releaseBody(t, req.URL, "sha256:"+testSHA256(current))}, nil
	}}
	instance := New(doer)
	firstDone := make(chan error, 1)
	go func() {
		_, errUpdate := instance.Update(context.Background(), UpdateRequest{StaticDir: dir})
		firstDone <- errUpdate
	}()
	<-started

	_, errSecond := instance.Update(context.Background(), UpdateRequest{StaticDir: dir})
	if !errors.Is(errSecond, ErrUpdateInProgress) {
		t.Fatalf("second error = %v, want ErrUpdateInProgress", errSecond)
	}
	close(release)
	if errFirst := <-firstDone; errFirst != nil {
		t.Fatal(errFirst)
	}
}
```

- [ ] **Step 2: Run the updater tests and verify the expected compile failure**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
go test ./internal/updater -v
```

Expected: FAIL because the updater types and functions do not exist.

- [ ] **Step 3: Implement release resolution, verified update, fallback, and atomic write**

Create `internal/updater/updater.go`:

```go
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

const (
	AssetName             = "management.html"
	DefaultPanelRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultReleaseURL      = "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest"
	FallbackURL            = "https://cpamc.router-for.me/"
	SourceGitHub            = "github"
	SourceFallback          = "fallback"
	SourceUpToDate          = "up-to-date"
	userAgent               = "cliproxy-panel-updater"
	maxReleaseResponseSize  = 2 << 20
	maxAssetDownloadSize    = 50 << 20
)

var (
	ErrUpdateInProgress = errors.New("panel update already in progress")
	ErrDigestMismatch   = errors.New("management asset digest mismatch")
)

type HTTPRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

type HTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type HTTPDoer interface {
	Do(ctx context.Context, hostCallbackID string, req HTTPRequest) (HTTPResponse, error)
}

type UpdateRequest struct {
	StaticDir             string
	PanelGitHubRepository string
	HostCallbackID        string
}

type Result struct {
	Updated bool   `json:"updated"`
	Hash    string `json:"hash"`
	Source  string `json:"source"`
	Message string `json:"message"`
}

type Updater struct {
	http       HTTPDoer
	inProgress atomic.Bool
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

type releaseResponse struct {
	Assets []releaseAsset `json:"assets"`
}

func New(httpDoer HTTPDoer) *Updater {
	return &Updater{http: httpDoer}
}

func (u *Updater) Update(ctx context.Context, req UpdateRequest) (Result, error) {
	if u == nil || u.http == nil {
		return Result{}, errors.New("host HTTP callback is unavailable")
	}
	if !u.inProgress.CompareAndSwap(false, true) {
		return Result{}, ErrUpdateInProgress
	}
	defer u.inProgress.Store(false)

	staticDir := strings.TrimSpace(req.StaticDir)
	if staticDir == "" {
		return Result{}, errors.New("static directory is empty")
	}
	if errMkdir := os.MkdirAll(staticDir, 0o755); errMkdir != nil {
		return Result{}, fmt.Errorf("prepare static directory: %w", errMkdir)
	}

	localPath := filepath.Join(staticDir, AssetName)
	localHash, errHash := FileSHA256(localPath)
	if errHash != nil && !errors.Is(errHash, os.ErrNotExist) {
		return Result{}, fmt.Errorf("hash local management asset: %w", errHash)
	}

	result, errGitHub := u.updateFromGitHub(ctx, req, localPath, localHash)
	if errGitHub == nil {
		return result, nil
	}
	if errors.Is(errGitHub, ErrDigestMismatch) || localHash != "" {
		return Result{}, errGitHub
	}

	fallbackData, errFallback := u.get(ctx, req.HostCallbackID, FallbackURL, maxAssetDownloadSize, http.Header{
		"User-Agent": []string{userAgent},
	})
	if errFallback != nil {
		return Result{}, fmt.Errorf("github update failed: %v; fallback failed: %w", errGitHub, errFallback)
	}
	fallbackHash := hashBytes(fallbackData)
	if errWrite := atomicWriteFile(localPath, fallbackData); errWrite != nil {
		return Result{}, fmt.Errorf("persist fallback management asset: %w", errWrite)
	}
	return Result{
		Updated: true,
		Hash:    fallbackHash,
		Source:  SourceFallback,
		Message: "Management panel updated from the unverified fallback page because GitHub update failed.",
	}, nil
}

func (u *Updater) updateFromGitHub(ctx context.Context, req UpdateRequest, localPath, localHash string) (Result, error) {
	releaseURL := ResolveReleaseURL(req.PanelGitHubRepository)
	releaseData, errRelease := u.get(ctx, req.HostCallbackID, releaseURL, maxReleaseResponseSize, http.Header{
		"Accept":     []string{"application/vnd.github+json"},
		"User-Agent": []string{userAgent},
	})
	if errRelease != nil {
		return Result{}, fmt.Errorf("fetch latest release: %w", errRelease)
	}

	var release releaseResponse
	if errUnmarshal := json.Unmarshal(releaseData, &release); errUnmarshal != nil {
		return Result{}, fmt.Errorf("decode latest release: %w", errUnmarshal)
	}
	asset, remoteHash, errAsset := findManagementAsset(release.Assets)
	if errAsset != nil {
		return Result{}, errAsset
	}
	if remoteHash != "" && localHash != "" && strings.EqualFold(remoteHash, localHash) {
		return Result{
			Updated: false,
			Hash:    localHash,
			Source:  SourceUpToDate,
			Message: "Management panel is already up to date.",
		}, nil
	}

	data, errDownload := u.get(ctx, req.HostCallbackID, asset.BrowserDownloadURL, maxAssetDownloadSize, http.Header{
		"User-Agent": []string{userAgent},
	})
	if errDownload != nil {
		return Result{}, fmt.Errorf("download management asset: %w", errDownload)
	}
	downloadedHash := hashBytes(data)
	if remoteHash != "" && !strings.EqualFold(remoteHash, downloadedHash) {
		return Result{}, fmt.Errorf("%w: expected %s, got %s", ErrDigestMismatch, remoteHash, downloadedHash)
	}
	if errWrite := atomicWriteFile(localPath, data); errWrite != nil {
		return Result{}, fmt.Errorf("persist management asset: %w", errWrite)
	}
	return Result{
		Updated: true,
		Hash:    downloadedHash,
		Source:  SourceGitHub,
		Message: "Management panel updated from the configured GitHub repository.",
	}, nil
}

func (u *Updater) get(ctx context.Context, callbackID, target string, maxSize int, headers http.Header) ([]byte, error) {
	if strings.TrimSpace(target) == "" {
		return nil, errors.New("request URL is empty")
	}
	resp, errDo := u.http.Do(ctx, callbackID, HTTPRequest{
		Method:  http.MethodGet,
		URL:     target,
		Headers: headers,
	})
	if errDo != nil {
		return nil, errDo
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("GET %s returned HTTP %d", target, resp.StatusCode)
	}
	if len(resp.Body) > maxSize {
		return nil, fmt.Errorf("GET %s returned %d bytes, limit is %d", target, len(resp.Body), maxSize)
	}
	return append([]byte(nil), resp.Body...), nil
}

func ResolveReleaseURL(repository string) string {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return DefaultReleaseURL
	}
	parsed, errParse := url.Parse(repository)
	if errParse != nil || parsed.Host == "" {
		return DefaultReleaseURL
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	switch strings.ToLower(parsed.Host) {
	case "api.github.com":
		if !strings.HasSuffix(strings.ToLower(parsed.Path), "/releases/latest") {
			parsed.Path += "/releases/latest"
		}
		return parsed.String()
	case "github.com":
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			repositoryName := strings.TrimSuffix(parts[1], ".git")
			return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", parts[0], repositoryName)
		}
	}
	return DefaultReleaseURL
}

func findManagementAsset(assets []releaseAsset) (releaseAsset, string, error) {
	for _, asset := range assets {
		if strings.EqualFold(asset.Name, AssetName) {
			return asset, parseDigest(asset.Digest), nil
		}
	}
	return releaseAsset{}, "", fmt.Errorf("management asset %s not found in latest release", AssetName)
}

func parseDigest(digest string) string {
	digest = strings.TrimSpace(digest)
	if index := strings.Index(digest, ":"); index >= 0 {
		digest = digest[index+1:]
	}
	return strings.ToLower(strings.TrimSpace(digest))
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func FileSHA256(path string) (string, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return "", errOpen
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, errCopy := io.Copy(hash, file); errCopy != nil {
		return "", errCopy
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func atomicWriteFile(path string, data []byte) error {
	tmpFile, errCreate := os.CreateTemp(filepath.Dir(path), "management-*.html")
	if errCreate != nil {
		return errCreate
	}
	tmpName := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
	}()
	if _, errWrite := tmpFile.Write(data); errWrite != nil {
		return errWrite
	}
	if errChmod := tmpFile.Chmod(0o644); errChmod != nil {
		return errChmod
	}
	if errClose := tmpFile.Close(); errClose != nil {
		return errClose
	}
	return os.Rename(tmpName, path)
}
```

- [ ] **Step 4: Format and run updater tests**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
gofmt -w internal/updater/updater.go internal/updater/updater_test.go
go test ./internal/updater -v
```

Expected: all updater tests PASS.

- [ ] **Step 5: Run the race detector for the concurrent update guard**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
go test -race ./internal/updater -run TestUpdateRejectsConcurrentAttempt -v
```

Expected: PASS and no race report.

- [ ] **Step 6: Commit the updater unit**

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
git add internal/updater
git commit -m "feat: add verified management panel updater"
```

---

### Task 3: Add Plugin RPC Registration, Management Routes, and Browser Page

**Files:**
- Create: `internal/plugin/register.go`
- Create: `internal/plugin/management.go`
- Create: `internal/plugin/management_test.go`
- Create: `internal/plugin/page.go`
- Create: `internal/plugin/page.html`

**Interfaces:**
- Consumes: `HostConfig`, `ResolveCurrentHostConfig`, `updater.UpdateRequest`, `updater.Result`, and `updater.FileSHA256`.
- Produces: `type UpdateRunner`.
- Produces: `func New(version string, runner UpdateRunner, resolver ConfigResolver) *Service`.
- Produces: `func (*Service) Call(method string, request []byte) []byte`.
- Produces: `func ErrorEnvelope(code, message string) []byte` for ABI initialization failures.
- Consumed later by: root `main.go`.

- [ ] **Step 1: Write RPC and management handler tests**

Create `internal/plugin/management_test.go`:

```go
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/berry-shake/cliproxy-panel-updater/internal/updater"
)

type fakeRunner struct {
	result updater.Result
	err    error
	got    updater.UpdateRequest
}

func (f *fakeRunner) Update(_ context.Context, req updater.UpdateRequest) (updater.Result, error) {
	f.got = req
	return f.result, f.err
}

func decodeEnvelopeResult[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !envelope.OK {
		t.Fatalf("envelope error: %+v", envelope.Error)
	}
	var out T
	if errUnmarshal := json.Unmarshal(envelope.Result, &out); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return out
}

func managementRequest(t *testing.T, method, path, callbackID string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(managementRPCRequest{
		Method:         method,
		Path:           path,
		HostCallbackID: callbackID,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func TestRegisterAdvertisesOnlyManagementAPI(t *testing.T) {
	t.Parallel()

	service := New("1.2.3", &fakeRunner{}, func() HostConfig { return HostConfig{} })
	registration := decodeEnvelopeResult[rpcRegistration](t, service.Call(pluginabi.MethodPluginRegister, nil))
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("SchemaVersion = %d", registration.SchemaVersion)
	}
	if registration.Metadata.Version != "1.2.3" || registration.Metadata.Name != "Panel Updater" {
		t.Fatalf("Metadata = %+v", registration.Metadata)
	}
	if !registration.Capabilities.ManagementAPI {
		t.Fatal("management_api capability is false")
	}
}

func TestManagementRegisterDeclaresExpectedRoutes(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{}, func() HostConfig { return HostConfig{} })
	registration := decodeEnvelopeResult[managementRegistration](t, service.Call(pluginabi.MethodManagementRegister, []byte(`{}`)))
	if len(registration.Routes) != 2 {
		t.Fatalf("routes = %+v", registration.Routes)
	}
	if registration.Routes[0].Method != http.MethodGet || registration.Routes[0].Path != "/plugins/panel-updater/status" {
		t.Fatalf("status route = %+v", registration.Routes[0])
	}
	if registration.Routes[1].Method != http.MethodPost || registration.Routes[1].Path != "/plugins/panel-updater/update" {
		t.Fatalf("update route = %+v", registration.Routes[1])
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Path != "/panel" {
		t.Fatalf("resources = %+v", registration.Resources)
	}
}

func TestStatusReturnsHostConfigAndLocalPanelMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	panel := []byte("panel")
	path := filepath.Join(dir, updater.AssetName)
	if errWrite := os.WriteFile(path, panel, 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	service := New("dev", &fakeRunner{}, func() HostConfig {
		return HostConfig{
			ConfigFile:            "/etc/cliproxy/config.pro.yaml",
			ConfigReadable:        true,
			PanelGitHubRepository: "https://github.com/acme/panel",
			StaticDir:             dir,
		}
	})

	response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, statusPath, "callback-status"),
	))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, body = %s", response.StatusCode, response.Body)
	}
	var status statusResponse
	if errUnmarshal := json.Unmarshal(response.Body, &status); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !status.ConfigFile.Readable || status.ConfigFile.Path != "/etc/cliproxy/config.pro.yaml" {
		t.Fatalf("config_file = %+v", status.ConfigFile)
	}
	if !status.Exists || status.Size != int64(len(panel)) || status.LocalSHA256 == "" {
		t.Fatalf("status = %+v", status)
	}
	if status.ReleaseURL != "https://api.github.com/repos/acme/panel/releases/latest" {
		t.Fatalf("ReleaseURL = %q", status.ReleaseURL)
	}
}

func TestUpdateForwardsRepositoryDirectoryAndCallbackID(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: updater.Result{
		Updated: true,
		Hash:    "abc",
		Source:  updater.SourceGitHub,
		Message: "updated",
	}}
	service := New("dev", runner, func() HostConfig {
		return HostConfig{
			PanelGitHubRepository: "https://github.com/acme/panel",
			StaticDir:             "/var/lib/cliproxy/static",
		}
	})

	response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodPost, updatePath, "callback-update"),
	))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, body = %s", response.StatusCode, response.Body)
	}
	if runner.got.StaticDir != "/var/lib/cliproxy/static" || runner.got.PanelGitHubRepository != "https://github.com/acme/panel" || runner.got.HostCallbackID != "callback-update" {
		t.Fatalf("update request = %+v", runner.got)
	}
}

func TestUpdateMapsBusyAndOtherErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "busy", err: updater.ErrUpdateInProgress, want: http.StatusConflict},
		{name: "failure", err: errors.New("disk denied"), want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := New("dev", &fakeRunner{err: tt.err}, func() HostConfig { return HostConfig{StaticDir: "/tmp/static"} })
			response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
				pluginabi.MethodManagementHandle,
				managementRequest(t, http.MethodPost, updatePath, "callback"),
			))
			if response.StatusCode != tt.want {
				t.Fatalf("StatusCode = %d, want %d", response.StatusCode, tt.want)
			}
		})
	}
}

func TestPanelResourceContainsAuthenticatedManagementCalls(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{}, func() HostConfig { return HostConfig{} })
	response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, panelPath, ""),
	))
	body := string(response.Body)
	for _, expected := range []string{
		"/v0/management/plugins/panel-updater/status",
		"/v0/management/plugins/panel-updater/update",
		"Authorization",
		"localStorage",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("page does not contain %q", expected)
		}
	}
}

func TestUnknownRPCMethodReturnsErrorEnvelope(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{}, func() HostConfig { return HostConfig{} })
	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(service.Call("unknown.method", nil), &envelope); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "unknown_method" {
		t.Fatalf("envelope = %+v", envelope)
	}
}
```

- [ ] **Step 2: Run the management tests and verify the expected compile failure**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
go test ./internal/plugin -v
```

Expected: FAIL because `Service`, RPC wire types, paths, and handlers do not exist.

- [ ] **Step 3: Implement lifecycle dispatch and route declarations**

Create `internal/plugin/register.go`:

```go
package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/berry-shake/cliproxy-panel-updater/internal/updater"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	statusPath = "/v0/management/plugins/panel-updater/status"
	updatePath = "/v0/management/plugins/panel-updater/update"
	panelPath  = "/v0/resource/plugins/panel-updater/panel"
)

type UpdateRunner interface {
	Update(ctx context.Context, req updater.UpdateRequest) (updater.Result, error)
}

type ConfigResolver func() HostConfig

type Service struct {
	version  string
	runner   UpdateRunner
	resolver ConfigResolver
}

type rpcCapabilities struct {
	ManagementAPI bool `json:"management_api"`
}

type rpcRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  rpcCapabilities    `json:"capabilities"`
}

type routeDeclaration struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

type resourceDeclaration struct {
	Path        string
	Menu        string
	Description string
}

type managementRegistration struct {
	Routes    []routeDeclaration    `json:"routes"`
	Resources []resourceDeclaration `json:"resources"`
}

type managementRPCRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          map[string][]string
	Body           []byte
	HostCallbackID string `json:"host_callback_id"`
}

func New(version string, runner UpdateRunner, resolver ConfigResolver) *Service {
	if strings.TrimSpace(version) == "" {
		version = "0.0.0-dev"
	}
	if resolver == nil {
		resolver = ResolveCurrentHostConfig
	}
	return &Service{version: version, runner: runner, resolver: resolver}
}

func (s *Service) Call(method string, request []byte) []byte {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(rpcRegistration{
			SchemaVersion: pluginabi.SchemaVersion,
			Metadata: pluginapi.Metadata{
				Name:             "Panel Updater",
				Version:          s.version,
				Author:           "berry-shake",
				GitHubRepository: "https://github.com/berry-shake/cliproxy-panel-updater",
				ConfigFields:     []pluginapi.ConfigField{},
			},
			Capabilities: rpcCapabilities{ManagementAPI: true},
		})
	case pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Routes: []routeDeclaration{
				{Method: http.MethodGet, Path: "/plugins/panel-updater/status", Description: "Show the configured panel repository and local management.html state."},
				{Method: http.MethodPost, Path: "/plugins/panel-updater/update", Description: "Download and atomically replace management.html."},
			},
			Resources: []resourceDeclaration{
				{Path: "/panel", Menu: "Panel Updater", Description: "Manually update the CLIProxyAPI management panel."},
			},
		})
	case pluginabi.MethodManagementHandle:
		var req managementRPCRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return ErrorEnvelope("invalid_request", "decode management request: "+errUnmarshal.Error())
		}
		return okEnvelope(s.handleManagement(req))
	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method)
	}
}

func okEnvelope(result any) []byte {
	rawResult, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return ErrorEnvelope("marshal_failed", errMarshal.Error())
	}
	raw, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: rawResult})
	return raw
}

func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:    code,
			Message: message,
		},
	})
	return raw
}
```

- [ ] **Step 4: Implement status and update management handlers**

Create `internal/plugin/management.go`:

```go
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/berry-shake/cliproxy-panel-updater/internal/updater"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type configFileStatus struct {
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Error    string `json:"error,omitempty"`
}

type statusResponse struct {
	ConfigFile                 configFileStatus `json:"config_file"`
	StaticDir                  string           `json:"static_dir"`
	FilePath                   string           `json:"file_path"`
	Exists                     bool             `json:"exists"`
	Size                       int64            `json:"size"`
	ModifiedAt                 string           `json:"modified_at,omitempty"`
	LocalSHA256                string           `json:"local_sha256,omitempty"`
	PanelGitHubRepository      string           `json:"panel_github_repository"`
	ReleaseURL                 string           `json:"release_url"`
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (s *Service) handleManagement(req managementRPCRequest) pluginapi.ManagementResponse {
	switch {
	case req.Method == http.MethodGet && req.Path == statusPath:
		return s.status()
	case req.Method == http.MethodPost && req.Path == updatePath:
		return s.update(req.HostCallbackID)
	case req.Method == http.MethodGet && req.Path == panelPath:
		return panelResponse()
	default:
		return jsonResponse(http.StatusNotFound, errorBody{Error: "not_found", Message: "plugin route not found"})
	}
}

func (s *Service) status() pluginapi.ManagementResponse {
	config := s.resolver()
	filePath := filepath.Join(config.StaticDir, updater.AssetName)
	status := statusResponse{
		ConfigFile: configFileStatus{
			Path:     config.ConfigFile,
			Readable: config.ConfigReadable,
			Error:    config.ConfigError,
		},
		StaticDir:             config.StaticDir,
		FilePath:              filePath,
		PanelGitHubRepository: config.PanelGitHubRepository,
		ReleaseURL:            updater.ResolveReleaseURL(config.PanelGitHubRepository),
	}

	info, errStat := os.Stat(filePath)
	if errors.Is(errStat, os.ErrNotExist) {
		return jsonResponse(http.StatusOK, status)
	}
	if errStat != nil {
		return jsonResponse(http.StatusInternalServerError, errorBody{Error: "stat_failed", Message: errStat.Error()})
	}
	localHash, errHash := updater.FileSHA256(filePath)
	if errHash != nil {
		return jsonResponse(http.StatusInternalServerError, errorBody{Error: "hash_failed", Message: errHash.Error()})
	}
	status.Exists = true
	status.Size = info.Size()
	status.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
	status.LocalSHA256 = localHash
	return jsonResponse(http.StatusOK, status)
}

func (s *Service) update(hostCallbackID string) pluginapi.ManagementResponse {
	if s.runner == nil {
		return jsonResponse(http.StatusInternalServerError, errorBody{Error: "updater_unavailable", Message: "panel updater is unavailable"})
	}
	config := s.resolver()
	result, errUpdate := s.runner.Update(context.Background(), updater.UpdateRequest{
		StaticDir:             config.StaticDir,
		PanelGitHubRepository: config.PanelGitHubRepository,
		HostCallbackID:        hostCallbackID,
	})
	if errors.Is(errUpdate, updater.ErrUpdateInProgress) {
		return jsonResponse(http.StatusConflict, errorBody{Error: "update_in_progress", Message: errUpdate.Error()})
	}
	if errUpdate != nil {
		return jsonResponse(http.StatusInternalServerError, errorBody{Error: "update_failed", Message: errUpdate.Error()})
	}
	return jsonResponse(http.StatusOK, result)
}

func jsonResponse(statusCode int, value any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		statusCode = http.StatusInternalServerError
		raw = []byte(`{"error":"marshal_failed","message":"failed to encode response"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: raw,
	}
}
```

- [ ] **Step 5: Add the embedded browser page handler**

Create `internal/plugin/page.go`:

```go
package plugin

import (
	_ "embed"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed page.html
var panelPage []byte

func panelResponse() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":              []string{"text/html; charset=utf-8"},
			"Cache-Control":             []string{"no-store"},
			"X-Content-Type-Options":    []string{"nosniff"},
			"Content-Security-Policy":   []string{"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'"},
		},
		Body: append([]byte(nil), panelPage...),
	}
}
```

Create `internal/plugin/page.html`:

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>CLIProxyAPI Panel Updater</title>
  <style>
    :root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, sans-serif; }
    body { margin: 0; background: #111827; color: #e5e7eb; }
    main { max-width: 820px; margin: 48px auto; padding: 0 20px; }
    section { background: #1f2937; border: 1px solid #374151; border-radius: 14px; padding: 22px; margin-bottom: 18px; }
    h1 { margin: 0 0 8px; font-size: 28px; }
    p { color: #9ca3af; line-height: 1.5; }
    label { display: block; font-weight: 600; margin-bottom: 8px; }
    input { width: 100%; box-sizing: border-box; padding: 11px 12px; border-radius: 8px; border: 1px solid #4b5563; background: #111827; color: inherit; }
    .actions { display: flex; gap: 10px; flex-wrap: wrap; margin-top: 16px; }
    button { border: 0; border-radius: 8px; padding: 10px 16px; font-weight: 700; cursor: pointer; }
    button.primary { background: #2563eb; color: white; }
    button.secondary { background: #374151; color: white; }
    button:disabled { cursor: wait; opacity: .6; }
    dl { display: grid; grid-template-columns: 190px 1fr; gap: 9px 14px; margin: 0; }
    dt { color: #9ca3af; }
    dd { margin: 0; overflow-wrap: anywhere; }
    pre { white-space: pre-wrap; overflow-wrap: anywhere; background: #111827; border-radius: 8px; padding: 14px; min-height: 22px; }
    .ok { color: #86efac; }
    .error { color: #fca5a5; }
    @media (max-width: 640px) { dl { grid-template-columns: 1fr; } dt { margin-top: 8px; } }
  </style>
</head>
<body>
<main>
  <section>
    <h1>Management Panel Updater</h1>
    <p>Reads <code>remote-management.panel-github-repository</code> from the active host config and atomically updates <code>management.html</code>.</p>
  </section>
  <section>
    <label for="management-key">Management key</label>
    <input id="management-key" type="password" autocomplete="current-password" placeholder="Enter the remote management secret key">
    <div class="actions">
      <button id="status-button" class="secondary" type="button">Check status</button>
      <button id="update-button" class="primary" type="button">Update now</button>
    </div>
  </section>
  <section>
    <dl>
      <dt>Config file</dt><dd id="config-file">—</dd>
      <dt>Config readable</dt><dd id="config-readable">—</dd>
      <dt>Panel repository</dt><dd id="repository">—</dd>
      <dt>Release API</dt><dd id="release-url">—</dd>
      <dt>Static directory</dt><dd id="static-dir">—</dd>
      <dt>Panel file</dt><dd id="file-path">—</dd>
      <dt>File state</dt><dd id="file-state">—</dd>
      <dt>SHA-256</dt><dd id="sha256">—</dd>
    </dl>
  </section>
  <section>
    <strong>Result</strong>
    <pre id="result">Ready.</pre>
  </section>
</main>
<script>
(() => {
  const keyName = 'cliproxy-panel-updater-management-key';
  const keyInput = document.getElementById('management-key');
  const statusButton = document.getElementById('status-button');
  const updateButton = document.getElementById('update-button');
  const result = document.getElementById('result');
  keyInput.value = localStorage.getItem(keyName) || '';

  function setResult(message, isError) {
    result.textContent = message;
    result.className = isError ? 'error' : 'ok';
  }

  async function api(path, method) {
    const key = keyInput.value.trim();
    if (!key) throw new Error('Management key is required.');
    localStorage.setItem(keyName, key);
    const response = await fetch(path, {
      method,
      headers: { Authorization: `Bearer ${key}` },
      cache: 'no-store'
    });
    const text = await response.text();
    let data;
    try { data = JSON.parse(text); } catch (_) { data = { message: text || `HTTP ${response.status}` }; }
    if (!response.ok) throw new Error(data.message || data.error || `HTTP ${response.status}`);
    return data;
  }

  function renderStatus(data) {
    document.getElementById('config-file').textContent = data.config_file.path || '—';
    document.getElementById('config-readable').textContent = data.config_file.readable ? 'yes' : `no${data.config_file.error ? ` — ${data.config_file.error}` : ''}`;
    document.getElementById('repository').textContent = data.panel_github_repository || '—';
    document.getElementById('release-url').textContent = data.release_url || '—';
    document.getElementById('static-dir').textContent = data.static_dir || '—';
    document.getElementById('file-path').textContent = data.file_path || '—';
    document.getElementById('file-state').textContent = data.exists ? `${data.size} bytes, modified ${data.modified_at}` : 'missing';
    document.getElementById('sha256').textContent = data.local_sha256 || '—';
  }

  async function checkStatus() {
    statusButton.disabled = true;
    try {
      const data = await api('/v0/management/plugins/panel-updater/status', 'GET');
      renderStatus(data);
      setResult('Status loaded.', false);
    } catch (error) {
      setResult(error.message, true);
    } finally {
      statusButton.disabled = false;
    }
  }

  async function updatePanel() {
    updateButton.disabled = true;
    try {
      const data = await api('/v0/management/plugins/panel-updater/update', 'POST');
      setResult(`${data.message}\nsource=${data.source}\nhash=${data.hash}`, false);
      await checkStatus();
    } catch (error) {
      setResult(error.message, true);
    } finally {
      updateButton.disabled = false;
    }
  }

  statusButton.addEventListener('click', checkStatus);
  updateButton.addEventListener('click', updatePanel);
})();
</script>
</body>
</html>
```

- [ ] **Step 6: Format and run plugin package tests**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
gofmt -w internal/plugin/register.go internal/plugin/management.go internal/plugin/management_test.go internal/plugin/page.go
go test ./internal/plugin -v
```

Expected: all host-config, registration, status, update mapping, page, and unknown-method tests PASS.

- [ ] **Step 7: Run all pure-Go tests**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
go test ./internal/...
```

Expected: PASS for both `internal/plugin` and `internal/updater`.

- [ ] **Step 8: Commit the RPC and UI unit**

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
git add internal/plugin
git commit -m "feat: add panel updater management UI"
```

---

### Task 4: Add the C ABI Entry Point and Host HTTP Callback Adapter

**Files:**
- Create: `main.go`
- Create: `main_test.go`

**Interfaces:**
- Consumes: `plugin.New`, `plugin.ErrorEnvelope`, `updater.HTTPDoer`, and `pluginabi.MethodHostHTTPDo`.
- Produces native exports: `cliproxy_plugin_init`, `cliproxyPluginCall`, `cliproxyPluginFree`, `cliproxyPluginShutdown`.
- Produces: root `hostHTTPDoer`, which forwards `host_callback_id` and decodes the host envelope.

- [ ] **Step 1: Write failing tests for host callback response decoding**

Create `main_test.go`:

```go
package main

import (
	"encoding/json"
	"testing"

	"github.com/berry-shake/cliproxy-panel-updater/internal/updater"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestDecodeHostHTTPResponse(t *testing.T) {
	t.Parallel()

	result, errMarshal := json.Marshal(updater.HTTPResponse{StatusCode: 200, Body: []byte("panel")})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	raw, errMarshal := json.Marshal(pluginabi.Envelope{OK: true, Result: result})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	resp, errDecode := decodeHostHTTPResponse(raw)
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	if resp.StatusCode != 200 || string(resp.Body) != "panel" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestDecodeHostHTTPResponseReturnsHostError(t *testing.T) {
	t.Parallel()

	raw, errMarshal := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:    "host_call_failed",
			Message: "proxy unavailable",
		},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	_, errDecode := decodeHostHTTPResponse(raw)
	if errDecode == nil || errDecode.Error() != "host_call_failed: proxy unavailable" {
		t.Fatalf("error = %v", errDecode)
	}
}
```

- [ ] **Step 2: Run root tests and verify the expected compile failure**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
go test . -v
```

Expected: FAIL because `decodeHostHTTPResponse` does not exist.

- [ ] **Step 3: Implement the ABI bridge and host callback adapter**

Create `main.go`:

```go
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static void clear_host_api(void) {
	stored_host = NULL;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"unsafe"

	pluginruntime "github.com/berry-shake/cliproxy-panel-updater/internal/plugin"
	"github.com/berry-shake/cliproxy-panel-updater/internal/updater"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

var (
	pluginVersion = "0.0.0-dev"
	serviceMu     sync.RWMutex
	service       *pluginruntime.Service
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || uint32(host.abi_version) != pluginabi.ABIVersion {
		return 1
	}
	C.store_host_api(host)
	serviceMu.Lock()
	service = pluginruntime.New(pluginVersion, updater.New(hostHTTPDoer{}), pluginruntime.ResolveCurrentHostConfig)
	serviceMu.Unlock()
	api.abi_version = C.uint32_t(pluginabi.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writePluginResponse(response, pluginruntime.ErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	serviceMu.RLock()
	current := service
	serviceMu.RUnlock()
	if current == nil {
		writePluginResponse(response, pluginruntime.ErrorEnvelope("not_initialized", "plugin is not initialized"))
		return 1
	}
	writePluginResponse(response, current.Call(C.GoString(method), requestBytes))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	serviceMu.Lock()
	service = nil
	serviceMu.Unlock()
	C.clear_host_api()
}

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type hostHTTPDoer struct{}

func (hostHTTPDoer) Do(ctx context.Context, callbackID string, req updater.HTTPRequest) (updater.HTTPResponse, error) {
	select {
	case <-ctx.Done():
		return updater.HTTPResponse{}, ctx.Err()
	default:
	}
	payload, errMarshal := json.Marshal(hostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
	})
	if errMarshal != nil {
		return updater.HTTPResponse{}, fmt.Errorf("marshal host HTTP request: %w", errMarshal)
	}
	raw, errCall := callHost(pluginabi.MethodHostHTTPDo, payload)
	if errCall != nil {
		return updater.HTTPResponse{}, errCall
	}
	return decodeHostHTTPResponse(raw)
}

func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cPayload unsafe.Pointer
	if len(payload) > 0 {
		cPayload = C.CBytes(payload)
		defer C.free(cPayload)
	}
	var response C.cliproxy_buffer
	if rc := C.call_host_api(cMethod, (*C.uint8_t)(cPayload), C.size_t(len(payload)), &response); rc != 0 {
		return nil, fmt.Errorf("host callback %s returned %d", method, int(rc))
	}
	if response.ptr == nil || response.len == 0 {
		return nil, errors.New("host callback returned an empty response")
	}
	defer C.free_host_buffer(response.ptr, response.len)
	return C.GoBytes(response.ptr, C.int(response.len)), nil
}

func decodeHostHTTPResponse(raw []byte) (updater.HTTPResponse, error) {
	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return updater.HTTPResponse{}, fmt.Errorf("decode host envelope: %w", errUnmarshal)
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return updater.HTTPResponse{}, errors.New("host callback failed")
		}
		return updater.HTTPResponse{}, fmt.Errorf("%s: %s", envelope.Error.Code, envelope.Error.Message)
	}
	var response updater.HTTPResponse
	if errUnmarshal := json.Unmarshal(envelope.Result, &response); errUnmarshal != nil {
		return updater.HTTPResponse{}, fmt.Errorf("decode host HTTP response: %w", errUnmarshal)
	}
	return response, nil
}

func writePluginResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
```

- [ ] **Step 4: Format and run all tests**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
gofmt -w main.go main_test.go
go test ./...
```

Expected: PASS for the root package and both internal packages.

- [ ] **Step 5: Build the native library and verify the required export**

Run on the current Darwin arm64 machine:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
go build -buildmode=c-shared -ldflags "-X main.pluginVersion=0.1.0-dev" -o /tmp/panel-updater-v0.1.0-dev.dylib .
nm -gU /tmp/panel-updater-v0.1.0-dev.dylib | grep 'cliproxy_plugin_init$'
rm -f /tmp/panel-updater-v0.1.0-dev.dylib /tmp/panel-updater-v0.1.0-dev.h
```

Expected: build succeeds and `nm` prints one exported `cliproxy_plugin_init` symbol.

- [ ] **Step 6: Commit the ABI unit**

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
git add main.go main_test.go
git commit -m "feat: add CLIProxyAPI plugin ABI bridge"
```

---

### Task 5: Add Cross-Platform GitHub Actions Builds and Operator Documentation

**Files:**
- Create: `.github/workflows/build.yml`
- Create: `README.md`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: root c-shared build and `main.pluginVersion` linker variable.
- Produces: five platform-tagged release assets; documents the required on-disk rename.

- [ ] **Step 1: Extend generated-build-output ignores**

Ensure `.gitignore` contains exactly:

```gitignore
.idea/
.vscode/
*.so
*.dylib
*.dll
*.h
```

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
git diff --check
```

Expected: exit code 0.

- [ ] **Step 2: Create the build and release workflow**

Create `.github/workflows/build.yml`:

```yaml
name: build

on:
  push:
    tags:
      - "v*"
  workflow_dispatch:

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: Check formatting
        shell: bash
        run: test -z "$(gofmt -l .)"
      - name: Vet
        run: go vet ./...
      - name: Test
        run: go test ./...

  build:
    needs: test
    strategy:
      fail-fast: false
      matrix:
        include:
          - runner: ubuntu-latest
            goos: linux
            goarch: amd64
            extension: so
          - runner: ubuntu-24.04-arm
            goos: linux
            goarch: arm64
            extension: so
          - runner: macos-15-intel
            goos: darwin
            goarch: amd64
            extension: dylib
          - runner: macos-latest
            goos: darwin
            goarch: arm64
            extension: dylib
          - runner: windows-latest
            goos: windows
            goarch: amd64
            extension: dll
    runs-on: ${{ matrix.runner }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: Install MinGW
        if: matrix.goos == 'windows'
        uses: msys2/setup-msys2@v2
        with:
          msystem: MINGW64
          update: true
          install: mingw-w64-x86_64-gcc
      - name: Add MinGW to PATH
        if: matrix.goos == 'windows'
        shell: pwsh
        run: Add-Content $env:GITHUB_PATH 'C:\msys64\mingw64\bin'
      - name: Build shared library
        shell: bash
        env:
          TARGET_GOOS: ${{ matrix.goos }}
          TARGET_GOARCH: ${{ matrix.goarch }}
          EXTENSION: ${{ matrix.extension }}
        run: |
          if [[ "${GITHUB_REF_TYPE}" == "tag" ]]; then
            VERSION="${GITHUB_REF_NAME#v}"
          else
            VERSION="0.0.0-dev"
          fi
          OUTPUT="panel-updater-v${VERSION}-${TARGET_GOOS}-${TARGET_GOARCH}.${EXTENSION}"
          CGO_ENABLED=1 GOOS="${TARGET_GOOS}" GOARCH="${TARGET_GOARCH}" \
            go build -buildmode=c-shared -trimpath \
            -ldflags "-s -w -X main.pluginVersion=${VERSION}" \
            -o "${OUTPUT}" .
          rm -f "${OUTPUT%.*}.h"
          mkdir -p dist
          mv "${OUTPUT}" dist/
      - uses: actions/upload-artifact@v4
        with:
          name: panel-updater-${{ matrix.goos }}-${{ matrix.goarch }}
          path: dist/*
          if-no-files-found: error

  release:
    if: startsWith(github.ref, 'refs/tags/v')
    needs: build
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: actions/download-artifact@v4
        with:
          pattern: panel-updater-*
          path: dist
          merge-multiple: true
      - name: Publish GitHub release
        env:
          GH_TOKEN: ${{ github.token }}
        run: gh release create "${GITHUB_REF_NAME}" dist/* --generate-notes --verify-tag
```

- [ ] **Step 3: Validate workflow syntax with actionlint**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 \
  -ignore 'label "macos-15-intel" is unknown' \
  .github/workflows/build.yml
```

Expected: no output and exit code 0. The narrow ignore is required because
actionlint v1.7.7 predates the official `macos-15-intel` label; do not ignore
any other diagnostic.

- [ ] **Step 4: Write installation and usage documentation**

Create `README.md`:

```markdown
# CLIProxyAPI Panel Updater Plugin

A CLIProxyAPI ManagementAPI plugin for manually updating the built-in
`management.html` control panel.

The plugin reads `remote-management.panel-github-repository` directly from
the same host configuration file selected by `--config`. Downloads use the
host's `host.http.do` callback, so the host's `proxy-url` and request logging
behavior apply automatically.

## Requirements

- CLIProxyAPI v7.2.71 or newer with dynamic plugin support
- Linux amd64/arm64, macOS amd64/arm64, or Windows amd64
- cgo-enabled CLIProxyAPI build on Unix platforms

## Install

1. Download the release asset matching the host platform.
2. Remove the platform suffix from the filename. For example:

   ```text
   panel-updater-v0.1.0-linux-amd64.so
   → panel-updater-v0.1.0.so
   ```

3. Put it in the preferred platform directory:

   ```text
   plugins/linux/amd64/panel-updater-v0.1.0.so
   plugins/linux/arm64/panel-updater-v0.1.0.so
   plugins/darwin/amd64/panel-updater-v0.1.0.dylib
   plugins/darwin/arm64/panel-updater-v0.1.0.dylib
   plugins/windows/amd64/panel-updater-v0.1.0.dll
   ```

4. Enable the plugin in the CLIProxyAPI host configuration:

   ```yaml
   remote-management:
     panel-github-repository: https://github.com/router-for-me/Cli-Proxy-API-Management-Center

   plugins:
     enabled: true
     configs:
       panel-updater:
         enabled: true
   ```

No plugin-specific repository setting is required or supported.

## Use

Start CLIProxyAPI with its normal config argument:

```bash
./cli-proxy-api --config config.yaml
```

Open:

```text
http://127.0.0.1:<port>/v0/resource/plugins/panel-updater/panel
```

Enter the remote management secret key, then select **Check status** or
**Update now**. The key is sent only in the `Authorization` header and stored
in the browser's localStorage for that origin; it is not written by the
plugin.

Authenticated API endpoints:

```text
GET  /v0/management/plugins/panel-updater/status
POST /v0/management/plugins/panel-updater/update
```

Example:

```bash
curl -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8317/v0/management/plugins/panel-updater/status

curl -X POST -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8317/v0/management/plugins/panel-updater/update
```

## Update behavior

1. Read `remote-management.panel-github-repository` from the active host
   config (`--config`, `-config`, or the default `./config.yaml`).
2. Resolve the same static directory used by CLIProxyAPI:
   `MANAGEMENT_STATIC_PATH`, then `WRITABLE_PATH/static`, then
   `<config-directory>/static`.
3. Fetch the latest GitHub release and locate the `management.html` asset.
4. Skip the download when the local SHA-256 already matches the release
   digest.
5. Verify the downloaded digest and atomically replace `management.html`.
6. If GitHub metadata or asset download fails while the local panel is
   missing, use `https://cpamc.router-for.me/` as an unverified fallback.
   Preserve an existing panel on GitHub failure. Digest mismatch never falls
   back and never replaces the current file.

Only one update can run inside the plugin at a time. A concurrent request
returns HTTP 409.

## Build locally

```bash
go test ./...
go build -buildmode=c-shared \
  -ldflags '-X main.pluginVersion=0.1.0-dev' \
  -o panel-updater-v0.1.0-dev.dylib .
```

Use `.so` on Linux and `.dll` on Windows. The c-shared build also produces a
C header; the host does not need it.

## Security notes

- The browser page is public, like all CLIProxyAPI plugin resources, but it
  cannot read status or run updates without the management key.
- The plugin never logs the management key or embeds it in HTML.
- GitHub release digests are verified before replacement.
- The fallback page has no digest metadata; update responses clearly report
  `source: "fallback"` when it is used.
```

- [ ] **Step 5: Run repository checks**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
gofmt -w .
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 6: Commit CI and documentation**

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
git add .gitignore .github/workflows/build.yml README.md
git commit -m "ci: build and publish plugin artifacts"
```

---

### Task 6: Final Compatibility Verification

**Files:**
- Verify only; modify files solely if a check exposes a concrete defect.

**Interfaces:**
- Verifies all interfaces produced in Tasks 1–5 as a complete plugin.

- [ ] **Step 1: Run formatting, vet, tests, and race tests**

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
gofmt -w .
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
go test -race ./internal/...
```

Expected: all commands exit 0 with no race report.

- [ ] **Step 2: Build the required local shared library**

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
VERSION=0.1.0-dev
go build -buildmode=c-shared -trimpath \
  -ldflags "-s -w -X main.pluginVersion=${VERSION}" \
  -o "/tmp/panel-updater-v${VERSION}.dylib" .
nm -gU "/tmp/panel-updater-v${VERSION}.dylib" | grep 'cliproxy_plugin_init$'
```

Expected: build succeeds and the exported initializer is present.

- [ ] **Step 3: Validate the plugin file naming against CLIProxyAPI discovery**

Run:

```bash
cd /opt/data/goland_data/CLIProxyAPI
go test ./internal/pluginhost -run 'Test.*Plugin.*Path|Test.*Plugin.*File|Test.*Parse' -count=1
```

Expected: matching pluginhost discovery tests PASS. The deployable local filename remains `panel-updater-v0.1.0-dev.dylib`.

- [ ] **Step 4: Run a minimal in-process host smoke test**

Use a temporary host configuration and plugin directory without touching the repository config:

```bash
set -euo pipefail
PLUGIN_REPO=/opt/data/goland_data/cliproxy-panel-updater
HOST_REPO=/opt/data/goland_data/CLIProxyAPI
TMP_DIR="$(mktemp -d)"
PORT=18317
cleanup() {
  if [[ -n "${SERVER_PID:-}" ]]; then kill "${SERVER_PID}" 2>/dev/null || true; fi
  rm -rf "${TMP_DIR}"
  rm -f /tmp/panel-updater-v0.1.0-dev.dylib /tmp/panel-updater-v0.1.0-dev.h
}
trap cleanup EXIT
mkdir -p "${TMP_DIR}/plugins/darwin/arm64"
cp /tmp/panel-updater-v0.1.0-dev.dylib "${TMP_DIR}/plugins/darwin/arm64/"
cat > "${TMP_DIR}/config.yaml" <<EOF
host: 127.0.0.1
port: ${PORT}
remote-management:
  allow-remote: false
  secret-key: smoke-secret
  disable-auto-update-panel: true
  panel-github-repository: https://github.com/router-for-me/Cli-Proxy-API-Management-Center
plugins:
  enabled: true
  dir: ${TMP_DIR}/plugins
  configs:
    panel-updater:
      enabled: true
EOF
cd "${HOST_REPO}"
go run ./cmd/server --config "${TMP_DIR}/config.yaml" >"${TMP_DIR}/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:${PORT}/v0/resource/plugins/panel-updater/panel" >"${TMP_DIR}/panel.html"; then break; fi
  sleep 1
done
grep -q 'Management Panel Updater' "${TMP_DIR}/panel.html"
curl -fsS -H 'Authorization: Bearer smoke-secret' \
  "http://127.0.0.1:${PORT}/v0/management/plugins/panel-updater/status" \
  >"${TMP_DIR}/status.json"
grep -q 'panel_github_repository' "${TMP_DIR}/status.json"
grep -q 'Cli-Proxy-API-Management-Center' "${TMP_DIR}/status.json"
```

Expected: the browser resource loads, the authenticated status endpoint returns JSON, and the configured `panel-github-repository` appears in that response. Do not invoke the update endpoint in this smoke test; updater behavior is covered by deterministic fake-host tests and calling it would depend on external network availability.

- [ ] **Step 5: Confirm a clean repository and preserve evidence**

Run:

```bash
cd /opt/data/goland_data/cliproxy-panel-updater
git status --short
git log --oneline --max-count=8
```

Expected: `git status --short` is empty. The log includes the implementation commits from Tasks 1–5 plus the approved design and implementation-plan commits.
