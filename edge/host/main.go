package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	wasmtime "github.com/bytecodealliance/wasmtime-go/v29"
)

// ProcessRequest is the JSON payload from the ESP32 edge-mode HTTP POST.
type ProcessRequest struct {
	Function          string                                   `json:"function"`
	ReleaseGeneration int64                                    `json:"releaseGeneration"`
	Source            string                                   `json:"source"`
	ResourceInputs    map[string]map[string]ResourceInputValue `json:"resourceInputs,omitempty"`
}

// ResourceInputValue accepts typed resource input shapes:
//
//	{"type":"f32","value":22.5}
//	{"type":"i32","value":4173}
//	{"type":"bool","value":true}
type ResourceInputValue struct {
	Type   string
	F32    float32
	I32    int32
	Bool   bool
	String string
}

func (v *ResourceInputValue) UnmarshalJSON(data []byte) error {
	var typed struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &typed); err != nil {
		return err
	}
	if typed.Type == "" || len(typed.Value) == 0 {
		return fmt.Errorf("resource input requires type and value")
	}
	switch typed.Type {
	case "f32":
		var value float32
		if err := json.Unmarshal(typed.Value, &value); err != nil {
			return fmt.Errorf("resource input f32 value: %w", err)
		}
		v.F32 = value
	case "i32":
		var value int32
		if err := json.Unmarshal(typed.Value, &value); err != nil {
			return fmt.Errorf("resource input i32 value: %w", err)
		}
		v.I32 = value
	case "bool":
		var value bool
		if err := json.Unmarshal(typed.Value, &value); err != nil {
			return fmt.Errorf("resource input bool value: %w", err)
		}
		v.Bool = value
	case "string":
		var value string
		if err := json.Unmarshal(typed.Value, &value); err != nil {
			return fmt.Errorf("resource input string value: %w", err)
		}
		v.String = value
	default:
		return fmt.Errorf("unsupported resource input type %q", typed.Type)
	}
	v.Type = typed.Type
	return nil
}

// ProcessResponse is returned to the caller.
type ProcessResponse struct {
	Result            int32              `json:"result"`
	Outputs           map[string]float32 `json:"outputs,omitempty"`
	Timing            *ProcessTiming     `json:"timing,omitempty"`
	Function          string             `json:"function,omitempty"`
	ReleaseGeneration int64              `json:"releaseGeneration,omitempty"`
	ArtifactDigest    string             `json:"artifactDigest,omitempty"`
	Error             string             `json:"error,omitempty"`
}

// ProcessTiming reports host-side timing for proactive placement estimates.
type ProcessTiming struct {
	EdgeExecutionMs int32 `json:"edgeExecutionMs,omitempty"`
}

type executionRelease struct {
	function   string
	generation int64
	digest     string
}

var (
	engine           *wasmtime.Engine
	module           *wasmtime.Module
	linker           *wasmtime.Linker
	wasmPath         string
	wasmDigest       string
	mu               sync.Mutex // serializes request execution and module reloads
	activeGeneration int64
	activeFunction   string
	stagedRelease    *compiledRelease
	sensorData       ProcessRequest
	outputData       map[string]float32
)

type compiledRelease struct {
	engine     *wasmtime.Engine
	module     *wasmtime.Module
	linker     *wasmtime.Linker
	digest     string
	generation int64
	function   string
	path       string
}

type releaseStatus struct {
	ActiveGeneration int64  `json:"activeGeneration"`
	ActiveDigest     string `json:"activeDigest"`
	ActiveFunction   string `json:"activeFunction"`
	StagedGeneration int64  `json:"stagedGeneration,omitempty"`
	StagedDigest     string `json:"stagedDigest,omitempty"`
	StagedFunction   string `json:"stagedFunction,omitempty"`
}

const artifactCallerHeader = "X-SIF-Artifact-Caller"
const contentTypeHeader = "Content-Type"
const wasmContentType = "application/wasm"
const jsonContentType = "application/json"
const operatorArtifactCaller = "operator-artifact-sync"
const releaseGenerationHeader = "X-SIF-Release-Generation"
const functionIdentityHeader = "X-SIF-Function-Identity"

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func digestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read wasm module %s for digest: %w", path, err)
	}
	return digestBytes(data), nil
}

func currentWasmDigest() string {
	mu.Lock()
	defer mu.Unlock()
	return wasmDigest
}

func setWasmDigestHeaders(w http.ResponseWriter, digest string) {
	if digest == "" {
		return
	}
	w.Header().Set("X-Wasm-Sha256", digest)
	w.Header().Set("ETag", fmt.Sprintf(`"sha256:%s"`, digest))
}

func initWasm() error {
	wasmPath = os.Getenv("WASM_PATH")
	if wasmPath == "" {
		wasmPath = "dht_reader.wasm"
	}
	if err := reloadWasmModule(wasmPath); err != nil {
		return err
	}
	activeFunction = strings.TrimSpace(os.Getenv("FUNCTION_IDENTITY"))
	if activeFunction == "" {
		activeFunction = "dht-reader"
	}
	if value := strings.TrimSpace(os.Getenv("RELEASE_GENERATION")); value != "" {
		_, _ = fmt.Sscanf(value, "%d", &activeGeneration)
	}
	return nil
}

func compileWasmModule(path string) (*wasmtime.Engine, *wasmtime.Module, *wasmtime.Linker, error) {
	engine := wasmtime.NewEngine()
	module, err := wasmtime.NewModuleFromFile(engine, path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load wasm module %s: %w", path, err)
	}
	linker := wasmtime.NewLinker(engine)
	registerHAL(linker)
	if err := validateWasmModule(engine, module, linker); err != nil {
		return nil, nil, nil, fmt.Errorf("invalid wasm module %s: %w", path, err)
	}
	return engine, module, linker, nil
}

func validateWasmModule(engine *wasmtime.Engine, module *wasmtime.Module, linker *wasmtime.Linker) error {
	store := wasmtime.NewStore(engine)
	instance, err := linker.Instantiate(store, module)
	if err != nil {
		return fmt.Errorf("instantiate failed: %w", err)
	}
	if instance.GetFunc(store, "process_event") == nil {
		return fmt.Errorf("export 'process_event' not found in wasm module")
	}
	return nil
}

func reloadWasmModule(path string) error {
	engineNext, moduleNext, linkerNext, err := compileWasmModule(path)
	if err != nil {
		return err
	}
	digestNext, err := digestFile(path)
	if err != nil {
		return err
	}

	mu.Lock()
	defer mu.Unlock()
	engine = engineNext
	module = moduleNext
	linker = linkerNext
	wasmDigest = digestNext
	log.Printf("Wasm module loaded: %s sha256=%s", path, digestNext)
	return nil
}

func runWasm(req *ProcessRequest) (int32, map[string]float32, int32, executionRelease, error) {
	mu.Lock()
	defer mu.Unlock()
	if req.Function != activeFunction || req.ReleaseGeneration != activeGeneration {
		return 0, nil, 0, executionRelease{}, fmt.Errorf("release mismatch: active function=%s generation=%d, request function=%s generation=%d", activeFunction, activeGeneration, req.Function, req.ReleaseGeneration)
	}
	executedRelease := executionRelease{function: activeFunction, generation: activeGeneration, digest: wasmDigest}

	store := wasmtime.NewStore(engine)

	// Host imports read this request data while process_event() is running.
	sensorData = *req
	outputData = make(map[string]float32)

	instance, err := linker.Instantiate(store, module)
	if err != nil {
		return 0, nil, 0, executedRelease, fmt.Errorf("instantiate failed: %w", err)
	}

	processEvent := instance.GetFunc(store, "process_event")
	if processEvent == nil {
		return 0, nil, 0, executedRelease, fmt.Errorf("export 'process_event' not found in wasm module")
	}

	start := time.Now()
	result, err := processEvent.Call(store)
	edgeExecutionMs := durationMillisCeil(time.Since(start))
	if err != nil {
		return 0, nil, edgeExecutionMs, executedRelease, fmt.Errorf("process_event() trapped: %w", err)
	}

	return result.(int32), currentOutputs(), edgeExecutionMs, executedRelease, nil
}

func durationMillisCeil(duration time.Duration) int32 {
	if duration <= 0 {
		return 0
	}
	return int32((duration + time.Millisecond - 1) / time.Millisecond)
}

func currentOutputs() map[string]float32 {
	if len(outputData) == 0 {
		return nil
	}
	outputs := make(map[string]float32, len(outputData))
	for key, value := range outputData {
		outputs[key] = value
	}
	return outputs
}

func wasmOutputLogMessage(outputs map[string]float32) string {
	if len(outputs) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"Hybrid computation outputs: temperatureF=%.2f heatIndexC=%.2f comfortScore=%d occupied=%d nextSampleSeconds=%d actuatorCommand=%d",
		outputs["temperatureF"],
		outputs["heatIndexC"],
		int32(outputs["comfortScore"]),
		int32(outputs["occupied"]),
		int32(outputs["nextSampleSeconds"]),
		int32(outputs["actuatorCommand"]),
	)
}

func requestArtifactCaller(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	if caller := strings.TrimSpace(r.Header.Get(artifactCallerHeader)); caller != "" {
		return caller
	}
	if caller := strings.TrimSpace(r.UserAgent()); caller != "" {
		return caller
	}
	return "unknown"
}

func offloadRequestLogMessage(req *ProcessRequest, bytesReceived int) string {
	message := fmt.Sprintf("Edge invocation: POST /process function=%s generation=%d source=%s bytes=%d inputs=%d",
		req.Function, req.ReleaseGeneration, req.Source, bytesReceived, resourceInputCount(req))
	if temperature, ok := resourceInputF32(req, "DHT", "temperature"); ok {
		message += fmt.Sprintf(" temp=%.2f", temperature)
	}
	if humidity, ok := resourceInputF32(req, "DHT", "humidity"); ok {
		message += fmt.Sprintf(" hum=%.2f", humidity)
	}
	return message
}

func resourceInputCount(req *ProcessRequest) int {
	if req == nil {
		return 0
	}
	count := 0
	for _, keys := range req.ResourceInputs {
		count += len(keys)
	}
	return count
}

func resourceInputF32(req *ProcessRequest, resource string, key string) (float32, bool) {
	if req == nil {
		return 0, false
	}
	keys, ok := req.ResourceInputs[resource]
	if !ok {
		return 0, false
	}
	value, ok := keys[key]
	if !ok {
		return 0, false
	}
	switch value.Type {
	case "f32", "":
		return value.F32, true
	case "i32":
		return float32(value.I32), true
	case "bool":
		if value.Bool {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func resourceInputI32(req *ProcessRequest, resource string, key string) (int32, bool) {
	if req == nil {
		return 0, false
	}
	keys, ok := req.ResourceInputs[resource]
	if !ok {
		return 0, false
	}
	value, ok := keys[key]
	if !ok {
		return 0, false
	}
	switch value.Type {
	case "i32":
		return value.I32, true
	case "f32", "":
		return int32(value.F32), true
	case "bool":
		if value.Bool {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func resourceInputBool(req *ProcessRequest, resource string, key string) (bool, bool) {
	if req == nil {
		return false, false
	}
	keys, ok := req.ResourceInputs[resource]
	if !ok {
		return false, false
	}
	value, ok := keys[key]
	if !ok {
		return false, false
	}
	switch value.Type {
	case "bool":
		return value.Bool, true
	case "i32":
		return value.I32 != 0, true
	case "f32", "":
		return value.F32 != 0, true
	default:
		return false, false
	}
}

func wasmArtifactAccessLogMessage(method string, path string, digest string, remote string, caller string) string {
	switch method {
	case http.MethodHead:
		return fmt.Sprintf("Wasm artifact digest probe: HEAD /wasm caller=%s sha256=%s path=%s remote=%s", caller, digest, path, remote)
	case http.MethodGet:
		return fmt.Sprintf("Wasm artifact download: GET /wasm caller=%s sha256=%s path=%s remote=%s", caller, digest, path, remote)
	default:
		return fmt.Sprintf("Wasm artifact access: %s /wasm caller=%s sha256=%s path=%s remote=%s", method, caller, digest, path, remote)
	}
}

func shouldLogWasmArtifactAccess(method string, caller string) bool {
	return !(method == http.MethodHead && caller == operatorArtifactCaller)
}

func handleProcess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ProcessResponse{Error: "read body: " + err.Error()})
		return
	}
	defer r.Body.Close()

	var req ProcessRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ProcessResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	log.Print(offloadRequestLogMessage(&req, len(body)))

	result, outputs, edgeExecutionMs, executedRelease, err := runWasm(&req)
	if err != nil {
		log.Printf("Wasm execution failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, ProcessResponse{Error: err.Error()})
		return
	}

	if msg := wasmOutputLogMessage(outputs); msg != "" {
		log.Print(msg)
	}
	log.Printf("process_event() returned: %d edgeExecutionMs=%d", result, edgeExecutionMs)
	writeJSON(w, http.StatusOK, ProcessResponse{
		Result: result, Outputs: outputs,
		Timing:   &ProcessTiming{EdgeExecutionMs: edgeExecutionMs},
		Function: executedRelease.function, ReleaseGeneration: executedRelease.generation,
		ArtifactDigest: executedRelease.digest,
	})
}

func handleWasm(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		fallthrough
	case http.MethodHead:
		digest := currentWasmDigest()
		caller := requestArtifactCaller(r)
		if shouldLogWasmArtifactAccess(r.Method, caller) {
			log.Print(wasmArtifactAccessLogMessage(r.Method, wasmPath, digest, r.RemoteAddr, caller))
		}
		w.Header().Set(contentTypeHeader, wasmContentType)
		setWasmDigestHeaders(w, digest)
		http.ServeFile(w, r, wasmPath)
	case http.MethodPut:
		handleWasmUpload(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleWasmUpload(w http.ResponseWriter, r *http.Request) {
	generation, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get(releaseGenerationHeader)), 10, 64)
	if err != nil || generation <= 0 {
		http.Error(w, "missing or invalid release generation", http.StatusBadRequest)
		return
	}
	function := strings.TrimSpace(r.Header.Get(functionIdentityHeader))
	if function == "" {
		http.Error(w, "missing function identity", http.StatusBadRequest)
		return
	}
	declaredDigest := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Wasm-Sha256")))
	if len(declaredDigest) != 64 {
		http.Error(w, "missing or invalid release digest", http.StatusBadRequest)
		return
	}

	tmpPath := wasmPath + ".staged.upload"
	stagedPath := wasmPath + ".staged"
	if err := os.MkdirAll(filepath.Dir(wasmPath), 0o755); err != nil {
		log.Printf("Wasm artifact upload failed: mkdir %s: %v", filepath.Dir(wasmPath), err)
		http.Error(w, "failed to prepare wasm directory", http.StatusInternalServerError)
		return
	}

	file, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("Wasm artifact upload failed: create %s: %v", tmpPath, err)
		http.Error(w, "failed to create temp wasm file", http.StatusInternalServerError)
		return
	}

	hasher := sha256.New()
	bytesWritten, copyErr := io.Copy(io.MultiWriter(file, hasher), r.Body)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		log.Printf("Wasm artifact upload failed: copy body: %v", copyErr)
		http.Error(w, "failed to write wasm upload", http.StatusBadRequest)
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		log.Printf("Wasm artifact upload failed: close %s: %v", tmpPath, closeErr)
		http.Error(w, "failed to finalize wasm upload", http.StatusInternalServerError)
		return
	}
	if bytesWritten == 0 {
		_ = os.Remove(tmpPath)
		http.Error(w, "empty wasm upload", http.StatusBadRequest)
		return
	}
	uploadDigest := hex.EncodeToString(hasher.Sum(nil))
	caller := requestArtifactCaller(r)
	if uploadDigest != declaredDigest {
		_ = os.Remove(tmpPath)
		http.Error(w, "uploaded bytes do not match declared release digest", http.StatusBadRequest)
		return
	}

	engineNext, moduleNext, linkerNext, err := compileWasmModule(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("Wasm artifact upload rejected from %s: %v", r.RemoteAddr, err)
		http.Error(w, "invalid wasm upload: "+err.Error(), http.StatusBadRequest)
		return
	}

	mu.Lock()
	if generation < activeGeneration || (stagedRelease != nil && generation < stagedRelease.generation) {
		mu.Unlock()
		_ = os.Remove(tmpPath)
		http.Error(w, "stale release generation", http.StatusConflict)
		return
	}
	if generation == activeGeneration {
		matches := uploadDigest == wasmDigest && function == activeFunction
		mu.Unlock()
		_ = os.Remove(tmpPath)
		if !matches {
			http.Error(w, "release generation conflicts with active metadata", http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "active", "generation": generation,
			"function": function, "digest": uploadDigest,
		})
		return
	}
	if stagedRelease != nil && generation == stagedRelease.generation {
		matches := uploadDigest == stagedRelease.digest && function == stagedRelease.function
		mu.Unlock()
		_ = os.Remove(tmpPath)
		if !matches {
			http.Error(w, "release generation conflicts with staged metadata", http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "staged", "generation": generation,
			"function": function, "digest": uploadDigest,
		})
		return
	}
	mu.Unlock()

	_ = os.Remove(stagedPath)
	if err := os.Rename(tmpPath, stagedPath); err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("Wasm artifact staging failed: rename %s -> %s: %v", tmpPath, stagedPath, err)
		http.Error(w, "failed to stage uploaded wasm", http.StatusInternalServerError)
		return
	}

	mu.Lock()
	stagedRelease = &compiledRelease{
		engine: engineNext, module: moduleNext, linker: linkerNext,
		digest: uploadDigest, generation: generation, function: function,
		path: stagedPath,
	}
	mu.Unlock()

	log.Printf("Wasm release staged: caller=%s generation=%d function=%s bytes=%d sha256=%s remote=%s",
		caller, generation, function, bytesWritten, uploadDigest, r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "staged", "bytes": bytesWritten, "path": stagedPath,
		"digest": uploadDigest, "generation": generation, "function": function,
	})
}

func handleRelease(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		mu.Lock()
		status := releaseStatus{
			ActiveGeneration: activeGeneration, ActiveDigest: wasmDigest,
			ActiveFunction: activeFunction,
		}
		if stagedRelease != nil {
			status.StagedGeneration = stagedRelease.generation
			status.StagedDigest = stagedRelease.digest
			status.StagedFunction = stagedRelease.function
		}
		mu.Unlock()
		writeJSON(w, http.StatusOK, status)
	case http.MethodPost:
		var request struct {
			Generation int64 `json:"releaseGeneration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Generation <= 0 {
			http.Error(w, "invalid activation request", http.StatusBadRequest)
			return
		}
		if err := activateStagedRelease(request.Generation); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "active", "releaseGeneration": request.Generation,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func activateStagedRelease(generation int64) error {
	mu.Lock()
	defer mu.Unlock()
	if generation == activeGeneration {
		return nil
	}
	if stagedRelease == nil || stagedRelease.generation != generation {
		return fmt.Errorf("release generation %d is not staged", generation)
	}
	backupPath := wasmPath + ".previous"
	_ = os.Remove(backupPath)
	if err := os.Rename(wasmPath, backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup active wasm: %w", err)
	}
	if err := os.Rename(stagedRelease.path, wasmPath); err != nil {
		_ = os.Rename(backupPath, wasmPath)
		return fmt.Errorf("activate staged wasm: %w", err)
	}
	_ = os.Remove(backupPath)
	engine = stagedRelease.engine
	module = stagedRelease.module
	linker = stagedRelease.linker
	wasmDigest = stagedRelease.digest
	activeGeneration = stagedRelease.generation
	activeFunction = stagedRelease.function
	stagedRelease = nil
	log.Printf("Wasm release activated: generation=%d function=%s sha256=%s", activeGeneration, activeFunction, wasmDigest)
	return nil
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(contentTypeHeader, jsonContentType)
	fmt.Fprintf(w, `{"status":"ok","runtime":"wasmtime-go","arch":"%s"}`, runtime.GOARCH)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set(contentTypeHeader, jsonContentType)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	if err := initWasm(); err != nil {
		log.Fatalf("Failed to initialize Wasm runtime: %v", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/process", handleProcess)
	mux.HandleFunc("/wasm", handleWasm)
	mux.HandleFunc("/release", handleRelease)
	mux.HandleFunc("/health", handleHealth)

	log.Printf("sif-edge-host listening on :%s (wasm=%s)", port, wasmPath)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}
