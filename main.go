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

	pluginruntime "github.com/berry-shake/cpa-panel-updater-plugin/internal/plugin"
	"github.com/berry-shake/cpa-panel-updater-plugin/internal/updater"
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
