package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/berry-shake/cpa-panel-updater-plugin/internal/updater"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	statusPath = "/v0/resource/plugins/panel-updater/status"
	updatePath = "/v0/resource/plugins/panel-updater/update"
	panelPath  = "/v0/resource/plugins/panel-updater/panel"

	configAllowedOrigins = "allowed_origins"
)

// UpdateRunner abstracts the updater so management handlers can be tested with a fake.
type UpdateRunner interface {
	Update(ctx context.Context, req updater.UpdateRequest) (updater.Result, error)
}

// ConfigResolver returns the host configuration at call time so updates always see fresh values.
type ConfigResolver func() HostConfig

// Service dispatches plugin lifecycle and management RPC methods.
type Service struct {
	version  string
	runner   UpdateRunner
	resolver ConfigResolver

	mu             sync.RWMutex
	allowedOrigins []string
}

type rpcCapabilities struct {
	ManagementAPI bool `json:"management_api"`
}

type rpcRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  rpcCapabilities    `json:"capabilities"`
}

type resourceDeclaration struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistration struct {
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

type rpcLifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type pluginConfig struct {
	AllowedOrigins string `yaml:"allowed_origins"`
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

// Call handles one plugin RPC method and always returns a JSON envelope.
func (s *Service) Call(method string, request []byte) []byte {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		s.applyConfig(request)
		return okEnvelope(rpcRegistration{
			SchemaVersion: pluginabi.SchemaVersion,
			Metadata: pluginapi.Metadata{
				Name:             "Panel Updater",
				Version:          s.version,
				Author:           "berry-shake",
				GitHubRepository: "https://github.com/berry-shake/cpa-panel-updater-plugin",
				Logo:             logoPath,
				ConfigFields: []pluginapi.ConfigField{
					{
						Name:        configAllowedOrigins,
						Type:        pluginapi.ConfigFieldTypeString,
						Description: "受信任的来源列表（逗号分隔），允许以 iframe 嵌入面板并跨源调用资源接口。留空为最严格默认：禁止任何 iframe 嵌入（frame-ancestors 'none'），并拒绝跨源浏览器请求。",
					},
				},
			},
			Capabilities: rpcCapabilities{ManagementAPI: true},
		})
	case pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Resources: []resourceDeclaration{
				{Path: "/panel", Menu: "Panel Updater", Description: "Manually update the CLIProxyAPI management panel."},
				{Path: "/status", Description: "Show the configured panel repository and local management.html state."},
				{Path: "/update", Description: "Download and atomically replace management.html."},
				{Path: "/logo.png", Description: "Plugin logo served to the management panel sidebar."},
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

// applyConfig reads plugins.configs.panel-updater from the host RPC payload and
// updates the cached allowed_origins list. Missing payload or missing field
// resets the list to empty.
func (s *Service) applyConfig(request []byte) {
	var origins []string
	if len(request) > 0 {
		var lifecycle rpcLifecycleRequest
		if errUnmarshal := json.Unmarshal(request, &lifecycle); errUnmarshal == nil && len(lifecycle.ConfigYAML) > 0 {
			var cfg pluginConfig
			if errYAML := yaml.Unmarshal(lifecycle.ConfigYAML, &cfg); errYAML == nil {
				origins = parseOrigins(cfg.AllowedOrigins)
			}
		}
	}
	s.mu.Lock()
	s.allowedOrigins = origins
	s.mu.Unlock()
}

// AllowedOrigins returns a snapshot of the configured origin list.
func (s *Service) AllowedOrigins() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.allowedOrigins) == 0 {
		return nil
	}
	out := make([]string, len(s.allowedOrigins))
	copy(out, s.allowedOrigins)
	return out
}

func parseOrigins(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(part)
		if origin == "" {
			continue
		}
		origin = strings.TrimRight(origin, "/")
		if _, dup := seen[origin]; dup {
			continue
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func okEnvelope(result any) []byte {
	rawResult, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return ErrorEnvelope("marshal_failed", errMarshal.Error())
	}
	raw, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: rawResult})
	return raw
}

// ErrorEnvelope builds a failure envelope for RPC and ABI initialization errors.
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
