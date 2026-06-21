package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOffloadRequestLogMessage(t *testing.T) {
	req := &ProcessRequest{
		Function: "dht-reader", ReleaseGeneration: 7, Source: "esp32",
		ResourceInputs: map[string]map[string]ResourceInputValue{
			"DHT": {
				"temperature": {Type: "f32", F32: 22.5},
				"humidity":    {Type: "f32", F32: 51.25},
			},
		},
	}
	got := offloadRequestLogMessage(req, 57)
	want := "Edge invocation: POST /process function=dht-reader generation=7 source=esp32 bytes=57 inputs=2 temp=22.50 hum=51.25"
	if got != want {
		t.Fatalf("offloadRequestLogMessage = %q, want %q", got, want)
	}
}

func TestOffloadRequestLogMessageShowsEmptyInputContract(t *testing.T) {
	req := &ProcessRequest{
		Function: "basic-edge-demo", ReleaseGeneration: 32, Source: "esp32-edgemode",
		ResourceInputs: map[string]map[string]ResourceInputValue{},
	}
	want := "Edge invocation: POST /process function=basic-edge-demo generation=32 source=esp32-edgemode bytes=128 inputs=0"
	if got := offloadRequestLogMessage(req, 128); got != want {
		t.Fatalf("offloadRequestLogMessage = %q, want %q", got, want)
	}
}

func TestResourceInputValueAcceptsTypedF32(t *testing.T) {
	var req ProcessRequest
	if err := json.Unmarshal([]byte(`{"resourceInputs":{"DHT":{"temperature":{"type":"f32","value":24.5},"humidity":{"type":"f32","value":53.25}}}}`), &req); err != nil {
		t.Fatalf("unmarshal typed resource input: %v", err)
	}
	temp, ok := resourceInputF32(&req, "DHT", "temperature")
	if !ok || temp != 24.5 {
		t.Fatalf("DHT.temperature = %.2f/%v, want 24.50/true", temp, ok)
	}
	humidity, ok := resourceInputF32(&req, "DHT", "humidity")
	if !ok || humidity != 53.25 {
		t.Fatalf("DHT.humidity = %.2f/%v, want 53.25/true", humidity, ok)
	}
}

func TestResourceInputValueRejectsUnsupportedType(t *testing.T) {
	var req ProcessRequest
	if err := json.Unmarshal([]byte(`{"resourceInputs":{"DHT":{"temperature":{"type":"bytes","value":"AAAA"}}}}`), &req); err == nil {
		t.Fatalf("expected unsupported resource input type to fail")
	}
}

func TestResourceInputValueAcceptsTypedI32AndBool(t *testing.T) {
	var req ProcessRequest
	if err := json.Unmarshal([]byte(`{"resourceInputs":{"BATTERY":{"percent":{"type":"i32","value":94},"voltageMv":{"type":"i32","value":4173}},"GPIO":{"buttonPressed":{"type":"bool","value":true}}}}`), &req); err != nil {
		t.Fatalf("unmarshal typed resource inputs: %v", err)
	}

	percent, ok := resourceInputI32(&req, "BATTERY", "percent")
	if !ok || percent != 94 {
		t.Fatalf("BATTERY.percent = %d/%v, want 94/true", percent, ok)
	}
	pressed, ok := resourceInputBool(&req, "GPIO", "buttonPressed")
	if !ok || !pressed {
		t.Fatalf("GPIO.buttonPressed = %t/%v, want true/true", pressed, ok)
	}
}

func TestResourceF32ReturnsMissingForUnknownInput(t *testing.T) {
	sensorData = ProcessRequest{}

	if _, ok := resourceF32("DHT", "pressure"); ok {
		t.Fatalf("expected missing DHT.pressure")
	}
}

func TestResourceF32ReadsHybridDemoInputs(t *testing.T) {
	sensorData = ProcessRequest{
		ResourceInputs: map[string]map[string]ResourceInputValue{
			"BATTERY": {
				"percent":   {Type: "i32", I32: 94},
				"voltageMv": {Type: "i32", I32: 4173},
			},
			"LIGHT": {
				"lux": {Type: "f32", F32: 120},
			},
			"OCCUPANCY": {
				"distanceCm": {Type: "f32", F32: 85},
			},
			"GPIO": {
				"buttonPressed": {Type: "bool", Bool: false},
			},
		},
	}

	tests := []struct {
		resource string
		key      string
		want     float32
	}{
		{"BATTERY", "percent", 94},
		{"BATTERY", "voltageMv", 4173},
		{"LIGHT", "lux", 120},
		{"OCCUPANCY", "distanceCm", 85},
		{"GPIO", "buttonPressed", 0},
	}
	for _, tt := range tests {
		got, ok := resourceF32(tt.resource, tt.key)
		if !ok || got != tt.want {
			t.Fatalf("%s.%s = %.2f/%v, want %.2f/true", tt.resource, tt.key, got, ok, tt.want)
		}
	}

	percent, ok := resourceI32("BATTERY", "percent")
	if !ok || percent != 94 {
		t.Fatalf("BATTERY.percent = %d/%v, want 94/true", percent, ok)
	}
	pressed, ok := resourceBool("GPIO", "buttonPressed")
	if !ok || pressed {
		t.Fatalf("GPIO.buttonPressed = %t/%v, want false/true", pressed, ok)
	}
}

func TestCurrentOutputsCopiesWasmOutputs(t *testing.T) {
	outputData = map[string]float32{
		"comfortScore":      87,
		"heatIndexC":        35.2,
		"nextSampleSeconds": 5,
		"actuatorCommand":   1,
	}

	outputs := currentOutputs()
	if outputs["comfortScore"] != 87 || outputs["actuatorCommand"] != 1 {
		t.Fatalf("outputs = %#v, want comfortScore and actuatorCommand", outputs)
	}
	outputData["comfortScore"] = 1
	if outputs["comfortScore"] != 87 {
		t.Fatalf("outputs alias outputData; got comfortScore %.2f", outputs["comfortScore"])
	}
}

func TestWasmOutputLogMessage(t *testing.T) {
	outputs := map[string]float32{
		"temperatureF":      85.1,
		"heatIndexC":        35.2,
		"comfortScore":      87,
		"occupied":          1,
		"nextSampleSeconds": 5,
		"actuatorCommand":   1,
	}
	got := wasmOutputLogMessage(outputs)
	want := "Hybrid computation outputs: temperatureF=85.10 heatIndexC=35.20 comfortScore=87 occupied=1 nextSampleSeconds=5 actuatorCommand=1"
	if got != want {
		t.Fatalf("wasmOutputLogMessage = %q, want %q", got, want)
	}
}

func TestProcessResponseIncludesTiming(t *testing.T) {
	payload, err := json.Marshal(ProcessResponse{
		Result: 0,
		Timing: &ProcessTiming{EdgeExecutionMs: 42},
	})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	var decoded struct {
		Timing struct {
			EdgeExecutionMs int32 `json:"edgeExecutionMs"`
		} `json:"timing"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if decoded.Timing.EdgeExecutionMs != 42 {
		t.Fatalf("edgeExecutionMs = %d, want 42", decoded.Timing.EdgeExecutionMs)
	}
}

func TestDurationMillisCeil(t *testing.T) {
	if got := durationMillisCeil(1500 * time.Microsecond); got != 2 {
		t.Fatalf("durationMillisCeil(1.5ms) = %d, want 2", got)
	}
	if got := durationMillisCeil(0); got != 0 {
		t.Fatalf("durationMillisCeil(0) = %d, want 0", got)
	}
}

func TestRequestArtifactCaller(t *testing.T) {
	t.Run("custom header wins", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/wasm", nil)
		req.Header.Set(artifactCallerHeader, "device-artifact-digest")
		req.Header.Set("User-Agent", "ignored")
		if got := requestArtifactCaller(req); got != "device-artifact-digest" {
			t.Fatalf("requestArtifactCaller = %q, want %q", got, "device-artifact-digest")
		}
	})

	t.Run("user agent fallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/wasm", nil)
		req.Header.Set("User-Agent", "Go-http-client/1.1")
		if got := requestArtifactCaller(req); got != "Go-http-client/1.1" {
			t.Fatalf("requestArtifactCaller = %q, want %q", got, "Go-http-client/1.1")
		}
	})

	t.Run("unknown fallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/wasm", nil)
		if got := requestArtifactCaller(req); got != "unknown" {
			t.Fatalf("requestArtifactCaller = %q, want %q", got, "unknown")
		}
	})
}

func TestWasmArtifactAccessLogMessage(t *testing.T) {
	tests := []struct {
		name   string
		method string
		want   string
	}{
		{
			name:   "head probe",
			method: http.MethodHead,
			want:   "Wasm artifact digest probe: HEAD /wasm caller=device-artifact-digest sha256=abc path=/app/dht_reader.wasm remote=10.0.0.1:1234",
		},
		{
			name:   "get download",
			method: http.MethodGet,
			want:   "Wasm artifact download: GET /wasm caller=device-artifact-download sha256=abc path=/app/dht_reader.wasm remote=10.0.0.1:1234",
		},
		{
			name:   "fallback access",
			method: http.MethodOptions,
			want:   "Wasm artifact access: OPTIONS /wasm caller=unknown sha256=abc path=/app/dht_reader.wasm remote=10.0.0.1:1234",
		},
	}

	callers := map[string]string{
		http.MethodHead:    "device-artifact-digest",
		http.MethodGet:     "device-artifact-download",
		http.MethodOptions: "unknown",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wasmArtifactAccessLogMessage(tc.method, "/app/dht_reader.wasm", "abc", "10.0.0.1:1234", callers[tc.method])
			if got != tc.want {
				t.Fatalf("wasmArtifactAccessLogMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShouldLogWasmArtifactAccess(t *testing.T) {
	if shouldLogWasmArtifactAccess(http.MethodHead, operatorArtifactCaller) {
		t.Fatal("operator HEAD /wasm probes should be suppressed in host logs")
	}
	if !shouldLogWasmArtifactAccess(http.MethodHead, "device-artifact-digest") {
		t.Fatal("device HEAD /wasm probe should still log")
	}
	if !shouldLogWasmArtifactAccess(http.MethodGet, operatorArtifactCaller) {
		t.Fatal("operator GET /wasm downloads should still log")
	}
}

func stageRequest(body []byte, generation int64, function string, digest string) *http.Request {
	request := httptest.NewRequest(http.MethodPut, "/wasm", bytes.NewReader(body))
	request.Header.Set(releaseGenerationHeader, fmt.Sprintf("%d", generation))
	request.Header.Set(functionIdentityHeader, function)
	request.Header.Set("X-Wasm-Sha256", digest)
	return request
}

func initializeReleaseFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	fixture := mustReadFixture(t)
	digest := mustDigest(t, fixture)
	tmpPath := filepath.Join(t.TempDir(), "dht_reader.wasm")
	if err := os.WriteFile(tmpPath, fixture, 0o644); err != nil {
		t.Fatalf("write initial wasm: %v", err)
	}
	wasmPath = tmpPath
	if err := reloadWasmModule(wasmPath); err != nil {
		t.Fatalf("reload initial wasm: %v", err)
	}
	mu.Lock()
	activeGeneration = 0
	activeFunction = "dht-reader"
	stagedRelease = nil
	mu.Unlock()
	return fixture, digest
}

func TestHandleWasmUploadStagesWithoutActivation(t *testing.T) {
	fixture, digest := initializeReleaseFixture(t)

	putReq := stageRequest(fixture, 1, "hybrid-resource-demo", digest)
	putRec := httptest.NewRecorder()
	handleWasm(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT /wasm status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	var putBody map[string]any
	if err := json.Unmarshal(putRec.Body.Bytes(), &putBody); err != nil {
		t.Fatalf("decode PUT /wasm response: %v", err)
	}
	if putBody["status"] != "staged" || putBody["digest"] != digest {
		t.Fatalf("PUT /wasm digest=%v want %s", putBody["digest"], digest)
	}
	if activeGeneration != 0 || activeFunction != "dht-reader" {
		t.Fatalf("staging changed active release to %s/%d", activeFunction, activeGeneration)
	}

	statusRec := httptest.NewRecorder()
	handleRelease(statusRec, httptest.NewRequest(http.MethodGet, "/release", nil))
	var status releaseStatus
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode release status: %v", err)
	}
	if status.StagedGeneration != 1 || status.StagedFunction != "hybrid-resource-demo" {
		t.Fatalf("staged status = %#v", status)
	}

	activateReq := httptest.NewRequest(http.MethodPost, "/release", bytes.NewBufferString(`{"releaseGeneration":1}`))
	activateRec := httptest.NewRecorder()
	handleRelease(activateRec, activateReq)
	if activateRec.Code != http.StatusOK {
		t.Fatalf("activate status=%d body=%s", activateRec.Code, activateRec.Body.String())
	}
	if activeGeneration != 1 || activeFunction != "hybrid-resource-demo" || wasmDigest != digest {
		t.Fatalf("active release = %s/%d/%s", activeFunction, activeGeneration, wasmDigest)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/wasm", nil)
	getRec := httptest.NewRecorder()
	handleWasm(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /wasm status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	if gotDigest := getRec.Header().Get("X-Wasm-Sha256"); gotDigest != digest {
		t.Fatalf("GET /wasm X-Wasm-Sha256=%q want %q", gotDigest, digest)
	}
	if !bytes.Equal(getRec.Body.Bytes(), fixture) {
		t.Fatalf("served wasm bytes differ from uploaded fixture")
	}
}

func TestHandleWasmHeadReturnsDigest(t *testing.T) {
	fixture := mustReadFixture(t)
	digest := mustDigest(t, fixture)
	tmpPath := filepath.Join(t.TempDir(), "dht_reader.wasm")
	if err := os.WriteFile(tmpPath, fixture, 0o644); err != nil {
		t.Fatalf("write initial wasm: %v", err)
	}
	wasmPath = tmpPath
	if err := reloadWasmModule(wasmPath); err != nil {
		t.Fatalf("reload initial wasm: %v", err)
	}

	headReq := httptest.NewRequest(http.MethodHead, "/wasm", nil)
	headRec := httptest.NewRecorder()
	handleWasm(headRec, headReq)
	if headRec.Code != http.StatusOK {
		t.Fatalf("HEAD /wasm status=%d body=%s", headRec.Code, headRec.Body.String())
	}
	if gotDigest := headRec.Header().Get("X-Wasm-Sha256"); gotDigest != digest {
		t.Fatalf("HEAD /wasm X-Wasm-Sha256=%q want %q", gotDigest, digest)
	}
	if headRec.Body.Len() != 0 {
		t.Fatalf("HEAD /wasm returned %d body bytes", headRec.Body.Len())
	}
}

func TestHandleWasmUploadIsIdempotentAndRejectsConflicts(t *testing.T) {
	fixture, digest := initializeReleaseFixture(t)
	putReq := stageRequest(fixture, 2, "dht-reader", digest)
	putRec := httptest.NewRecorder()
	handleWasm(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT /wasm status=%d body=%s", putRec.Code, putRec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(putRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode unchanged PUT /wasm response: %v", err)
	}
	if body["status"] != "staged" {
		t.Fatalf("PUT /wasm status=%v want staged", body["status"])
	}
	if body["digest"] != digest {
		t.Fatalf("PUT /wasm digest=%v want %s", body["digest"], digest)
	}

	duplicateRec := httptest.NewRecorder()
	handleWasm(duplicateRec, stageRequest(fixture, 2, "dht-reader", digest))
	if duplicateRec.Code != http.StatusOK {
		t.Fatalf("duplicate status=%d body=%s", duplicateRec.Code, duplicateRec.Body.String())
	}
	conflictRec := httptest.NewRecorder()
	handleWasm(conflictRec, stageRequest(fixture, 2, "hybrid-resource-demo", digest))
	if conflictRec.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflictRec.Code, conflictRec.Body.String())
	}
	staleRec := httptest.NewRecorder()
	handleWasm(staleRec, stageRequest(fixture, 1, "dht-reader", digest))
	if staleRec.Code != http.StatusConflict {
		t.Fatalf("stale status=%d body=%s", staleRec.Code, staleRec.Body.String())
	}
}

func TestHandleWasmUploadRejectsInvalidModule(t *testing.T) {
	fixture, _ := initializeReleaseFixture(t)

	invalid := []byte("not wasm")
	putReq := stageRequest(invalid, 1, "dht-reader", mustDigest(t, invalid))
	putRec := httptest.NewRecorder()
	handleWasm(putRec, putReq)
	if putRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT /wasm invalid status=%d body=%s", putRec.Code, putRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/wasm", nil)
	getRec := httptest.NewRecorder()
	handleWasm(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /wasm status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	if !bytes.Equal(getRec.Body.Bytes(), fixture) {
		t.Fatalf("existing wasm changed after invalid upload")
	}
}

func TestRunWasmRejectsReleaseIdentityMismatch(t *testing.T) {
	initializeReleaseFixture(t)
	if _, _, _, _, err := runWasm(&ProcessRequest{Function: "hybrid-resource-demo", ReleaseGeneration: 0}); err == nil {
		t.Fatal("expected function identity mismatch")
	}
	if _, _, _, _, err := runWasm(&ProcessRequest{Function: "dht-reader", ReleaseGeneration: 99}); err == nil {
		t.Fatal("expected generation mismatch")
	}
}

func TestActivationWaitsForInvocationMutex(t *testing.T) {
	fixture, digest := initializeReleaseFixture(t)
	recorder := httptest.NewRecorder()
	handleWasm(recorder, stageRequest(fixture, 3, "dht-reader", digest))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stage status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	mu.Lock()
	done := make(chan error, 1)
	go func() { done <- activateStagedRelease(3) }()
	select {
	case err := <-done:
		mu.Unlock()
		t.Fatalf("activation completed before invocation boundary: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("activation after boundary: %v", err)
	}
}

func mustReadFixture(t *testing.T) []byte {
	t.Helper()
	file, err := os.Open("dht_reader.wasm")
	if err != nil {
		t.Fatalf("open fixture wasm: %v", err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read fixture wasm: %v", err)
	}
	return data
}

func mustDigest(t *testing.T, data []byte) string {
	t.Helper()
	return digestBytes(data)
}
