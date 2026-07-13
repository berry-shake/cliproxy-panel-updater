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

	"github.com/berry-shake/cliproxy-panel-updater/internal/updater"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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

func managementRequest(t *testing.T, method, path, callbackID string, headers http.Header) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(managementRPCRequest{
		Method:         method,
		Path:           path,
		Headers:        headers,
		HostCallbackID: callbackID,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func lifecycleRequest(t *testing.T, configYAML string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(rpcLifecycleRequest{ConfigYAML: []byte(configYAML)})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func TestRegisterAdvertisesManagementAPIAndAllowedOriginsField(t *testing.T) {
	t.Parallel()

	service := New("1.2.3", &fakeRunner{}, func() HostConfig { return HostConfig{} })
	registration := decodeEnvelopeResult[rpcRegistration](t, service.Call(pluginabi.MethodPluginRegister, nil))
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("SchemaVersion = %d", registration.SchemaVersion)
	}
	if registration.Metadata.Version != "1.2.3" || registration.Metadata.Name != "Panel Updater" {
		t.Fatalf("Metadata = %+v", registration.Metadata)
	}
	if len(registration.Metadata.ConfigFields) != 1 ||
		registration.Metadata.ConfigFields[0].Name != configAllowedOrigins ||
		registration.Metadata.ConfigFields[0].Type != pluginapi.ConfigFieldTypeString {
		t.Fatalf("ConfigFields = %+v, want single %s string field", registration.Metadata.ConfigFields, configAllowedOrigins)
	}
	if !registration.Capabilities.ManagementAPI {
		t.Fatal("management_api capability is false")
	}
}

func TestRegisterAppliesAndReconfigureClearsAllowedOrigins(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{}, func() HostConfig { return HostConfig{} })
	decodeEnvelopeResult[rpcRegistration](t, service.Call(
		pluginabi.MethodPluginRegister,
		lifecycleRequest(t, "enabled: true\nallowed_origins: \"https://a.example, https://b.example/, https://a.example\"\n"),
	))
	if got := service.AllowedOrigins(); len(got) != 2 || got[0] != "https://a.example" || got[1] != "https://b.example" {
		t.Fatalf("AllowedOrigins() = %v, want deduped and trimmed [a b]", got)
	}

	decodeEnvelopeResult[rpcRegistration](t, service.Call(
		pluginabi.MethodPluginReconfigure,
		lifecycleRequest(t, "enabled: true\n"),
	))
	if got := service.AllowedOrigins(); len(got) != 0 {
		t.Fatalf("AllowedOrigins() = %v, want empty after reconfigure", got)
	}
}

func TestManagementRegisterDeclaresExpectedResources(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{}, func() HostConfig { return HostConfig{} })
	registration := decodeEnvelopeResult[managementRegistration](t, service.Call(pluginabi.MethodManagementRegister, []byte(`{}`)))
	if len(registration.Resources) != 3 {
		t.Fatalf("resources = %+v", registration.Resources)
	}
	paths := map[string]string{}
	for _, res := range registration.Resources {
		paths[res.Path] = res.Menu
	}
	if _, ok := paths["/panel"]; !ok {
		t.Fatalf("resources missing /panel: %+v", registration.Resources)
	}
	if _, ok := paths["/status"]; !ok {
		t.Fatalf("resources missing /status: %+v", registration.Resources)
	}
	if _, ok := paths["/update"]; !ok {
		t.Fatalf("resources missing /update: %+v", registration.Resources)
	}
	if paths["/panel"] != "Panel Updater" {
		t.Fatalf("/panel menu = %q", paths["/panel"])
	}
}

func TestPanelPageCSPReflectsAllowedOrigins(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{}, func() HostConfig { return HostConfig{} })

	// No origins configured → frame-ancestors 'none'.
	response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, panelPath, "", nil),
	))
	csp := response.Headers.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("csp without origins = %q", csp)
	}

	// Configure origins → frame-ancestors 'self' <origins>.
	decodeEnvelopeResult[rpcRegistration](t, service.Call(
		pluginabi.MethodPluginReconfigure,
		lifecycleRequest(t, "allowed_origins: \"https://admin.example, https://ops.example\"\n"),
	))
	response = decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, panelPath, "", nil),
	))
	csp = response.Headers.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'self' https://admin.example https://ops.example") {
		t.Fatalf("csp with origins = %q", csp)
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
		managementRequest(t, http.MethodGet, statusPath, "callback-status", nil),
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
		managementRequest(t, http.MethodGet, updatePath, "callback-update", nil),
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
				managementRequest(t, http.MethodGet, updatePath, "callback", nil),
			))
			if response.StatusCode != tt.want {
				t.Fatalf("StatusCode = %d, want %d", response.StatusCode, tt.want)
			}
		})
	}
}

func TestNonGetMethodsRejected(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{}, func() HostConfig { return HostConfig{} })
	response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodPost, updatePath, "", nil),
	))
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("StatusCode = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestOriginEnforcementRejectsUnlistedOrigins(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{result: updater.Result{Message: "ok"}}, func() HostConfig { return HostConfig{StaticDir: "/tmp"} })
	decodeEnvelopeResult[rpcRegistration](t, service.Call(
		pluginabi.MethodPluginRegister,
		lifecycleRequest(t, "allowed_origins: \"https://admin.example\"\n"),
	))

	// Missing Origin/Referer is treated as same-origin and allowed.
	response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, updatePath, "cb", nil),
	))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("empty origin should be allowed, got %d", response.StatusCode)
	}

	// Listed Origin passes.
	allowed := http.Header{"Origin": []string{"https://admin.example"}}
	response = decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, updatePath, "cb", allowed),
	))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("listed origin should pass, got %d", response.StatusCode)
	}

	// Unlisted Origin is rejected.
	blocked := http.Header{"Origin": []string{"https://evil.example"}}
	response = decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, updatePath, "cb", blocked),
	))
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unlisted origin should be forbidden, got %d body=%s", response.StatusCode, response.Body)
	}

	// Referer with allowed origin passes when Origin absent.
	viaReferer := http.Header{"Referer": []string{"https://admin.example/some/page"}}
	response = decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, updatePath, "cb", viaReferer),
	))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("allowed referer should pass, got %d", response.StatusCode)
	}
}

func TestOriginEnforcementDisabledWhenNoOriginsConfigured(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{result: updater.Result{Message: "ok"}}, func() HostConfig { return HostConfig{StaticDir: "/tmp"} })
	// No config applied → no origin restriction.
	response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, updatePath, "cb", http.Header{"Origin": []string{"https://anywhere.example"}}),
	))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("cross-origin should pass when unconfigured, got %d body=%s", response.StatusCode, response.Body)
	}
}

func TestPanelResourceIsUnauthenticated(t *testing.T) {
	t.Parallel()

	service := New("dev", &fakeRunner{}, func() HostConfig { return HostConfig{} })
	response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, service.Call(
		pluginabi.MethodManagementHandle,
		managementRequest(t, http.MethodGet, panelPath, "", nil),
	))
	body := string(response.Body)
	for _, expected := range []string{
		"/v0/resource/plugins/panel-updater/status",
		"/v0/resource/plugins/panel-updater/update",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("page does not contain %q", expected)
		}
	}
	for _, unwanted := range []string{
		"Authorization: Bearer",
		"management-key",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page unexpectedly contains %q", unwanted)
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
