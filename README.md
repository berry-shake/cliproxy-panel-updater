# CLIProxyAPI Panel Updater Plugin

A CLIProxyAPI plugin for manually updating the built-in `management.html`
control panel from a browser page — no management key required.

The plugin reads `remote-management.panel-github-repository` directly from
the same host configuration file selected by `--config`. Downloads use the
host's `host.http.do` callback, so the host's `proxy-url` and request logging
behavior apply automatically.

## Requirements

- CLIProxyAPI v7.2.71 or newer with dynamic plugin support
- Linux amd64/arm64, macOS amd64/arm64, or Windows amd64
- cgo-enabled CLIProxyAPI build on Unix platforms

## Install

### Automatic (plugin store)

Release assets follow the CLIProxyAPI plugin store layout
(`panel-updater_<version>_<goos>_<goarch>.zip` plus `checksums.txt`), so the
host can install the plugin from GitHub releases when a `store` block is
configured:

```yaml
remote-management:
  panel-github-repository: https://github.com/router-for-me/Cli-Proxy-API-Management-Center

plugins:
  enabled: true
  configs:
    panel-updater:
      enabled: true
      store:
        id: panel-updater
        name: Panel Updater
        description: Manually update the management center panel (management.html).
        author: berry-shake
        version: 0.1.9
        release-tag: v0.1.9
        repository: https://github.com/berry-shake/cliproxy-panel-updater
        install:
          type: github-release
```

### Manual

1. Download the zip matching the host platform and extract
   `panel-updater.<so|dylib|dll>`.
2. Put it in the preferred platform directory:

   ```text
   plugins/linux/amd64/panel-updater.so
   plugins/linux/arm64/panel-updater.so
   plugins/darwin/amd64/panel-updater.dylib
   plugins/darwin/arm64/panel-updater.dylib
   plugins/windows/amd64/panel-updater.dll
   ```

3. Enable the plugin in the CLIProxyAPI host configuration:

   ```yaml
   plugins:
     enabled: true
     configs:
       panel-updater:
         enabled: true
   ```

## Configuration

| Key | Type | Description |
| --- | --- | --- |
| `allowed_origins` | string | Optional. Comma-separated origin list (e.g. `https://admin.example, https://ops.example`). Sets the CSP `frame-ancestors` list so those origins may embed the panel in an iframe, and restricts the `/status` and `/update` resource endpoints to requests whose `Origin` (fallback `Referer`) matches one of the entries. Empty disables both. |

Example:

```yaml
plugins:
  enabled: true
  configs:
    panel-updater:
      enabled: true
      allowed_origins: "https://admin.example.com, https://ops.example.com"
```

Entries are trimmed of whitespace and trailing slashes and deduplicated. When
`allowed_origins` is empty, the CSP `frame-ancestors` stays at `'none'` and
the resource endpoints accept requests regardless of origin (same behavior as
v0.1.2).

## Use

Start CLIProxyAPI with its normal config argument:

```bash
./cli-proxy-api --config config.yaml
```

Open:

```text
http://127.0.0.1:<port>/v0/resource/plugins/panel-updater/panel
```

Click **Check status** to inspect the current `management.html`, then click
**Update now** to pull the latest release and atomically replace the file.
No key entry is needed.

The page is localized in English and Simplified Chinese. It picks the
default language from `navigator.language` (any `zh-*` tag selects Chinese;
everything else falls back to English), and the choice can be overridden
with the EN/中文 toggle in the header. The selection is remembered in
`localStorage` under `cliproxy-panel-updater-lang`.

Public resource endpoints (GET only):

```text
GET /v0/resource/plugins/panel-updater/panel
GET /v0/resource/plugins/panel-updater/status
GET /v0/resource/plugins/panel-updater/update
```

Example:

```bash
curl http://127.0.0.1:8317/v0/resource/plugins/panel-updater/status
curl http://127.0.0.1:8317/v0/resource/plugins/panel-updater/update
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
  -ldflags '-X main.pluginVersion=0.1.9-dev' \
  -o panel-updater-v0.1.9-dev.dylib .
```

Use `.so` on Linux and `.dll` on Windows. The c-shared build also produces a
C header; the host does not need it.

## Security notes

- The panel page and its status/update endpoints are exposed as unauthenticated
  `resource` routes. Anyone able to reach the CLIProxyAPI HTTP port can open
  the panel; without `allowed_origins` they can also trigger an update.
- Configure `allowed_origins` to restrict the browser origins that can embed
  the panel and call the endpoints. The check uses the `Origin` header
  (falling back to `Referer`); requests without either header are treated as
  same-origin and permitted so CLI callers still work.
- Update content is constrained by design: the plugin only replaces
  `management.html` with the digest-verified asset from the repository
  configured in `remote-management.panel-github-repository`. It never accepts
  arbitrary URLs, file paths, or content from the request.
- GitHub release digests are verified before replacement. The fallback page
  has no digest metadata; update responses clearly report `source: "fallback"`
  when it is used.
- Do not expose the CLIProxyAPI HTTP port to untrusted networks without
  additional protection (reverse proxy auth, IP allow-list, VPN, etc.).
