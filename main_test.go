package main

import (
	"encoding/json"
	"testing"

	"github.com/berry-shake/cpa-panel-updater-plugin/internal/updater"
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
