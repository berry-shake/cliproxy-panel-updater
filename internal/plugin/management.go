package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/berry-shake/cpa-panel-updater-plugin/internal/updater"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type configFileStatus struct {
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Error    string `json:"error,omitempty"`
}

type statusResponse struct {
	ConfigFile            configFileStatus `json:"config_file"`
	FilePath              string           `json:"file_path"`
	Exists                bool             `json:"exists"`
	Size                  int64            `json:"size"`
	ModifiedAt            string           `json:"modified_at,omitempty"`
	LocalSHA256           string           `json:"local_sha256,omitempty"`
	PanelGitHubRepository string           `json:"panel_github_repository"`
	ReleaseURL            string           `json:"release_url"`
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (s *Service) handleManagement(req managementRPCRequest) pluginapi.ManagementResponse {
	if req.Method != http.MethodGet {
		return jsonResponse(http.StatusMethodNotAllowed, errorBody{Error: "method_not_allowed", Message: "only GET is accepted on resource routes"})
	}
	if req.Path == logoPath {
		return logoResponse()
	}
	origins := s.AllowedOrigins()
	switch req.Path {
	case statusPath:
		if resp, ok := s.enforceOrigin(req, origins); !ok {
			return resp
		}
		return s.status()
	case updatePath:
		if resp, ok := s.enforceOrigin(req, origins); !ok {
			return resp
		}
		return s.update(req.HostCallbackID)
	case panelPath:
		return panelResponse(origins)
	default:
		return jsonResponse(http.StatusNotFound, errorBody{Error: "not_found", Message: "plugin route not found"})
	}
}

// enforceOrigin rejects cross-origin browser requests on the status/update
// endpoints. Cross-origin is always rejected unless the declared origin
// (Origin header, falling back to Referer) matches allowed_origins, so the
// empty default is the strictest setting. Same-origin browser requests and
// requests without any browser context headers (CLI tools) are permitted.
//
// Detection is layered: Sec-Fetch-Site is authoritative when present (the
// browser forbids pages from forging Sec- headers). Without it, a bare
// Origin header implies a cross-origin fetch because browsers omit Origin
// on same-origin GET requests. A Referer alone is ambiguous on legacy
// browsers, so it only causes rejection once allowed_origins is configured.
func (s *Service) enforceOrigin(req managementRPCRequest, allowed []string) (pluginapi.ManagementResponse, bool) {
	headers := req.Headers
	site := strings.ToLower(strings.TrimSpace(headers.Get("Sec-Fetch-Site")))
	if site == "same-origin" || site == "none" {
		return pluginapi.ManagementResponse{}, true
	}
	origin := requestOrigin(headers)
	if origin != "" {
		for _, candidate := range allowed {
			if strings.EqualFold(candidate, origin) {
				return pluginapi.ManagementResponse{}, true
			}
		}
	}
	crossOrigin := site == "same-site" || site == "cross-site" ||
		strings.TrimSpace(headers.Get("Origin")) != "" ||
		(origin != "" && len(allowed) > 0)
	if crossOrigin {
		return jsonResponse(http.StatusForbidden, errorBody{
			Error:   "origin_not_allowed",
			Message: "cross-origin requests are rejected; add trusted origins to allowed_origins",
		}), false
	}
	return pluginapi.ManagementResponse{}, true
}

func requestOrigin(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if raw := strings.TrimSpace(headers.Get("Origin")); raw != "" && raw != "null" {
		return strings.TrimRight(raw, "/")
	}
	referer := strings.TrimSpace(headers.Get("Referer"))
	if referer == "" {
		return ""
	}
	parsed, errParse := url.Parse(referer)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return strings.TrimRight(parsed.Scheme+"://"+parsed.Host, "/")
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
