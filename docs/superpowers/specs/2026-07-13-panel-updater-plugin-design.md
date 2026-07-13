# Design: panel-updater — Manual Management Panel Update Plugin for CLIProxyAPI

Date: 2026-07-13
Status: Approved

## Goal

A CLIProxyAPI plugin that lets an operator manually update the built-in
management control panel asset (`management.html`) from a browser page,
covering the cases where the built-in auto-updater is disabled
(`disable-auto-update-panel: true`, cluster/Home mode) or when the operator
wants an immediate refresh without waiting for the 3-hour auto-update cycle.

Repository lives at `/opt/data/goland_data/cliproxy-panel-updater`
(standalone, own go.mod, built by GitHub Actions).

## Background (host facts this design relies on)

- Plugins are C ABI dynamic libraries (`.so`/`.dylib`/`.dll`) exporting
  `cliproxy_plugin_init` (ABI v1). All RPC is JSON envelopes over
  `call(method, request) -> response` (schema v1).
- The `ManagementAPI` capability lets a plugin register:
  - authenticated routes under `/v0/management/...` (management-key auth
    enforced by the host);
  - unauthenticated GET-only browser resources under
    `/v0/resource/plugins/<plugin-id>/...`.
- `management.handle` requests carry a `host_callback_id`; forwarding it in
  `host.http.do` callbacks makes downloads go through the host's
  proxy-aware HTTP client (`helps.NewProxyAwareHTTPClient`), inheriting the
  configured `proxy-url` and request logging for free.
- The built-in updater (`internal/managementasset/updater.go`):
  - release URL: `https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest`,
    overridable via config `remote-management.panel-github-repository`
    (accepts `github.com/owner/repo` or `api.github.com` URLs);
  - asset name: `management.html`; release asset `digest` field
    (`sha256:<hex>`) is verified against the downloaded bytes;
  - fallback URL when GitHub fails: `https://cpamc.router-for.me/`
    (unverified, warned);
  - static dir resolution: `MANAGEMENT_STATIC_PATH` env →
    `WRITABLE_PATH/static` → `<config dir>/static`; final file is
    `<static dir>/management.html`; writes are atomic (temp file + rename).
- Plugins receive only their own config subtree (`config_yaml`) at
  `plugin.register`/`plugin.reconfigure`. The host does not push its main
  config (e.g. the `remote-management` section) to plugins. However, the
  plugin runs in the host process, so it shares the working directory,
  environment, and command line: the host locates its config file via the
  `--config` flag, defaulting to `<cwd>/config.yaml`
  (`cmd/server/main.go`), and the plugin can apply the same rule to read
  the file directly.

## Approach (selected: Option A)

Single-capability `ManagementAPI` plugin. All downloads go through the
`host.http.do` callback (proxy + request-log inheritance). No CommandLine
capability, no plugin-side HTTP client.

Rejected alternatives:
- Option B (extra CommandLine capability for a `panel-update` subcommand):
  more surface, YAGNI — the browser page covers the need.
- Option C (plugin-side `net/http` with own proxy handling): duplicates
  host proxy logic and loses request logging.

## Plugin identity

- Plugin ID / file name: `panel-updater` → artifacts
  `panel-updater-v<version>.so|.dylib|.dll`.
- Module path: `github.com/berry-shake/cliproxy-panel-updater`.
- Go module requires `github.com/router-for-me/CLIProxyAPI/v7` at the
  latest published tag (v7.2.71 at design time) — used only for
  `sdk/pluginabi` method-name constants; all C ABI glue is local to the
  plugin (mirroring `examples/plugin/management-api/go`).

## Plugin configuration (config.yaml of the host)

```yaml
plugins:
  enabled: true
  configs:
    panel-updater:
      enabled: true
```

No plugin-specific configuration. The plugin reads the values it needs
directly from the host's own config file at update time (always fresh,
no duplicated settings):

1. **Host config file location** — same rule as the host binary: the
   `--config` value from `os.Args` if present, else `<cwd>/config.yaml`.
   The plugin runs in-process, so `os.Args` and the working directory are
   the host's own.
2. **`remote-management.panel-github-repository`** — parsed from that
   YAML file (only this scalar is read; unknown fields ignored). Empty or
   unparsable → default official repository
   `https://github.com/router-for-me/Cli-Proxy-API-Management-Center`.
3. **`proxy-url`** — not read by the plugin; downloads go through
   `host.http.do`, which already applies the host's proxy.
4. **Static dir** — same resolution chain as the host's
   `managementasset.StaticDir`: `MANAGEMENT_STATIC_PATH` env (if it ends
   in `management.html`, use its parent dir) → `WRITABLE_PATH/static` →
   `<config file dir>/static`.

If the config file cannot be read (e.g. Postgres-backed config), the
status endpoint reports it and updates proceed with the default
repository.

## Routes

Registered at `management.register`:

- Resource (no auth, GET):
  - `GET /v0/resource/plugins/panel-updater/panel` — single self-contained HTML
    page (inline CSS/JS, no external assets). Menu label: `Panel Updater`.
    The page shows: resolved static dir, config file path, effective
    panel repository, file presence/size/mtime/hash (filled via the
    status API), a management-key input (stored in `localStorage`), a
    "Check status" button and an "Update now" button.
- Management (host-enforced auth):
  - `GET /v0/management/plugins/panel-updater/status` — returns JSON:
    `config_file` (path + readable flag), resolved `static_dir`,
    `file_path`, `exists`, `size`, `modified_at`, `local_sha256`,
    effective `panel_github_repository` and `release_url`.
  - `POST /v0/management/plugins/panel-updater/update` — performs the
    update, returns JSON: `updated` (bool), `hash`, `source`
    (`github` | `fallback` | `up-to-date`), `message`.

The browser page calls the two management endpoints with
`Authorization: Bearer <management key>` (the host accepts `Bearer` or
`X-Management-Key`). The page itself never embeds a key.

## Update flow (POST /update handler)

Mirrors `EnsureLatestManagementHTML`:

1. Locate and parse the host config file;
   read `remote-management.panel-github-repository`.
2. Resolve static dir (rules above); `mkdir -p` it.
3. Resolve release URL from the repository value (same parsing rules
   as the host's `resolveReleaseURL`: `api.github.com` URLs get
   `/releases/latest` appended if missing; `github.com/owner/repo` becomes
   `https://api.github.com/repos/owner/repo/releases/latest`; anything
   else falls back to the default).
4. `host.http.do` GET release JSON (`Accept: application/vnd.github+json`,
   `User-Agent: cliproxy-panel-updater`), forwarding the
   `host_callback_id` received in the `management.handle` request.
5. Find asset named `management.html`; read its `digest`
   (`sha256:<hex>`) if present.
6. If local file exists and its SHA-256 equals the remote digest → return
   `source: "up-to-date"` without downloading.
7. `host.http.do` GET `browser_download_url`; verify SHA-256 against the
   digest when present; on mismatch → error, do not write.
8. Atomic write: temp file in the static dir + `os.Rename` to
   `management.html`.
9. On a GitHub-path failure (steps 4–7) **and only when the local
   `management.html` is missing**: download `https://cpamc.router-for.me/`
   via `host.http.do`, write atomically, and return `source: "fallback"`
   with a warning message (no digest verification possible). If a local
   panel already exists, preserve it and report the GitHub failure.
   Digest mismatch is always a hard failure and never falls back.
10. A mutex serializes concurrent update requests (second caller gets
    `409` with a "update already in progress" message).

Concurrency with the host's own auto-updater is acceptable: both sides
write atomically via rename, so the file is never observed truncated;
last writer wins. No cross-process locking (KISS).

## Internal structure (plugin repo)

```
cliproxy-panel-updater/
├── go.mod / go.sum
├── main.go                    # package main: cgo preamble, cliproxy_plugin_init,
│                              # dispatch table, host.http.do bridge (implements updater.HTTPDoer)
├── internal/plugin/
│   ├── register.go            # register/reconfigure payloads
│   ├── hostconfig.go          # locate host config file (--config / cwd), read panel-github-repository, resolve static dir
│   ├── management.go          # management.register + management.handle routing
│   ├── management_test.go     # route dispatch / envelope tests
│   ├── page.go                # embedded HTML page (go:embed page.html)
│   └── page.html
├── internal/updater/
│   ├── updater.go             # release URL resolution, digest check, atomic write
│   └── updater_test.go        # unit tests against a fake HTTPDoer
├── .github/workflows/build.yml
├── README.md
└── docs/superpowers/specs/2026-07-13-panel-updater-plugin-design.md
```

Design-for-isolation notes:
- `internal/updater` and `internal/plugin` are pure Go (no cgo); the
  update logic takes an injected `HTTPDoer` interface, so
  `go test ./internal/...` needs neither cgo nor a live host.
- cgo appears only in `main.go`, which wires the C ABI to
  `internal/plugin` and implements `HTTPDoer` on top of `host.http.do`.

## Error handling

- Every RPC reply is a proper envelope; handler errors map to
  `{ok:false, error:{code, message}}` (the host translates to HTTP 500)
  or, for expected conditions, an `ok` envelope whose ManagementResponse
  carries a 4xx/5xx `StatusCode` and JSON error body — the plugin prefers
  the latter so the browser page can render messages.
- Digest mismatch: hard error, no write, message tells the user to retry.
- Missing static dir permissions: 500 with the underlying error string.
- Unknown method / unknown route path: `unknown_method` /
  404-style envelope.
- No panics; panic in a handler would fuse the whole plugin (host
  behavior), so handlers wrap work in plain error returns.

## GitHub Actions build

Workflow `build.yml`, triggered on tag push (`v*`) and manual dispatch:

| Artifact | Runner | Toolchain |
|---|---|---|
| `panel-updater-v<ver>-linux-amd64.so` | ubuntu-latest | native cgo |
| `panel-updater-v<ver>-linux-arm64.so` | ubuntu-24.04-arm | native cgo |
| `panel-updater-v<ver>-darwin-amd64.dylib` | macos-15-intel | native cgo |
| `panel-updater-v<ver>-darwin-arm64.dylib` | macos-latest | native cgo |
| `panel-updater-v<ver>-windows-amd64.dll` | windows-latest | native cgo (MinGW preinstalled) |

- Build command: `go build -buildmode=c-shared -o <artifact> .`
  (c-shared implies CGO_ENABLED=1; the version is injected with
  `-ldflags "-X main.pluginVersion=<ver>"`).
- Version comes from the git tag (strip leading `v`); manual dispatch
  builds `0.0.0-dev`.
- Each job uploads its artifact; a final job on tag pushes attaches all
  artifacts to a GitHub Release.
Note on file naming: release assets carry a platform suffix only so they
can coexist in one GitHub Release. The host requires the on-disk name
`panel-updater-v<version>.<ext>` (no platform suffix), in either the flat
`plugins/` dir or the preferred `plugins/<goos>/<goarch>/` subdirectory.
The README instructs: download the asset for your platform, rename it to
`panel-updater-v<ver>.<ext>`, and place it at
`plugins/<goos>/<goarch>/panel-updater-v<ver>.<ext>`.

## Testing

- `updater_test.go`: unit tests for `resolveReleaseURL` parity cases,
  digest verification (match/mismatch/absent), up-to-date short-circuit,
  fallback path, atomic write behavior — all against a fake `HTTPDoer`.
- `hostconfig` tests: `--config` flag extraction from an args slice,
  `panel-github-repository` YAML extraction, static dir resolution chain.
- `management_test.go`: route dispatch and JSON envelope round-trips.
- CI runs `gofmt` check + `go vet` + `go test ./...` before the matrix
  build.
- Manual smoke test: build locally, drop into `plugins/`, enable in
  config, open the resource page, run status + update.

## Out of scope (YAGNI)

- Updating assets other than `management.html`.
- Scheduled/automatic updates (host already has them).
- Version pinning / rollback of the panel.
- Cross-process file locking against the host auto-updater.
