package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestOffloadRequestLogMessage(t *testing.T) {
	req := &ProcessRequest{Source: "esp32", Temperature: 22.5, Humidity: 51.25}
	got := offloadRequestLogMessage(req, 57)
	want := "Edge invocation: POST /process source=esp32 bytes=57 temp=22.50 hum=51.25"
	if got != want {
		t.Fatalf("offloadRequestLogMessage = %q, want %q", got, want)
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

func TestWasmArtifactUploadLogMessages(t *testing.T) {
	gotUpload := wasmArtifactUploadLogMessage(339, "abc", "/app/dht_reader.wasm", "10.0.0.1:1234", "operator-artifact-sync")
	wantUpload := "Wasm artifact upload: PUT /wasm caller=operator-artifact-sync bytes=339 sha256=abc path=/app/dht_reader.wasm remote=10.0.0.1:1234"
	if gotUpload != wantUpload {
		t.Fatalf("wasmArtifactUploadLogMessage = %q, want %q", gotUpload, wantUpload)
	}

	gotUnchanged := unchangedWasmArtifactUploadLogMessage("abc", "10.0.0.1:1234", "operator-artifact-sync")
	wantUnchanged := "Wasm artifact upload skipped: caller=operator-artifact-sync unchanged sha256=abc remote=10.0.0.1:1234"
	if gotUnchanged != wantUnchanged {
		t.Fatalf("unchangedWasmArtifactUploadLogMessage = %q, want %q", gotUnchanged, wantUnchanged)
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

func TestHandleWasmUploadReplacesServedModule(t *testing.T) {
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

	putReq := httptest.NewRequest(http.MethodPut, "/wasm", bytes.NewReader(fixture))
	putRec := httptest.NewRecorder()
	handleWasm(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT /wasm status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	var putBody map[string]any
	if err := json.Unmarshal(putRec.Body.Bytes(), &putBody); err != nil {
		t.Fatalf("decode PUT /wasm response: %v", err)
	}
	if putBody["digest"] != digest {
		t.Fatalf("PUT /wasm digest=%v want %s", putBody["digest"], digest)
	}

	got, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read uploaded wasm: %v", err)
	}
	if !bytes.Equal(got, fixture) {
		t.Fatalf("uploaded wasm bytes differ from fixture")
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

func TestHandleWasmUploadSkipsUnchangedModule(t *testing.T) {
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

	putReq := httptest.NewRequest(http.MethodPut, "/wasm", bytes.NewReader(fixture))
	putRec := httptest.NewRecorder()
	handleWasm(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT /wasm status=%d body=%s", putRec.Code, putRec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(putRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode unchanged PUT /wasm response: %v", err)
	}
	if body["status"] != "unchanged" {
		t.Fatalf("PUT /wasm status=%v want unchanged", body["status"])
	}
	if body["digest"] != digest {
		t.Fatalf("PUT /wasm digest=%v want %s", body["digest"], digest)
	}
}

func TestHandleWasmUploadRejectsInvalidModule(t *testing.T) {
	fixture := mustReadFixture(t)
	tmpPath := filepath.Join(t.TempDir(), "dht_reader.wasm")
	if err := os.WriteFile(tmpPath, fixture, 0o644); err != nil {
		t.Fatalf("write initial wasm: %v", err)
	}
	wasmPath = tmpPath
	if err := reloadWasmModule(wasmPath); err != nil {
		t.Fatalf("reload initial wasm: %v", err)
	}

	putReq := httptest.NewRequest(http.MethodPut, "/wasm", bytes.NewReader([]byte("not wasm")))
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
