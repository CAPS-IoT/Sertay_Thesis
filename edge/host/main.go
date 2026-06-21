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
	"strings"
	"sync"

	wasmtime "github.com/bytecodealliance/wasmtime-go/v29"
)

// ProcessRequest is the JSON payload from the ESP32 edge-mode HTTP POST.
type ProcessRequest struct {
	Function    string  `json:"function"`
	Source      string  `json:"source"`
	Temperature float32 `json:"temperature"`
	Humidity    float32 `json:"humidity"`
}

// ProcessResponse is returned to the caller.
type ProcessResponse struct {
	Result int32  `json:"result"`
	Error  string `json:"error,omitempty"`
}

var (
	engine     *wasmtime.Engine
	module     *wasmtime.Module
	linker     *wasmtime.Linker
	wasmPath   string
	wasmDigest string
	mu         sync.Mutex // serializes request execution and module reloads
	sensorData ProcessRequest
)

const artifactCallerHeader = "X-SIF-Artifact-Caller"
const contentTypeHeader = "Content-Type"
const wasmContentType = "application/wasm"
const jsonContentType = "application/json"
const operatorArtifactCaller = "operator-artifact-sync"

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
	return reloadWasmModule(wasmPath)
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

func runWasm(req *ProcessRequest) (int32, error) {
	mu.Lock()
	defer mu.Unlock()

	store := wasmtime.NewStore(engine)

	// Host imports read this request data while process_event() is running.
	sensorData = *req

	instance, err := linker.Instantiate(store, module)
	if err != nil {
		return 0, fmt.Errorf("instantiate failed: %w", err)
	}

	processEvent := instance.GetFunc(store, "process_event")
	if processEvent == nil {
		return 0, fmt.Errorf("export 'process_event' not found in wasm module")
	}

	result, err := processEvent.Call(store)
	if err != nil {
		return 0, fmt.Errorf("process_event() trapped: %w", err)
	}

	return result.(int32), nil
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
	return fmt.Sprintf("Edge invocation: POST /process source=%s bytes=%d temp=%.2f hum=%.2f",
		req.Source, bytesReceived, req.Temperature, req.Humidity)
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

func wasmArtifactUploadLogMessage(bytesWritten int64, digest string, path string, remote string, caller string) string {
	return fmt.Sprintf("Wasm artifact upload: PUT /wasm caller=%s bytes=%d sha256=%s path=%s remote=%s", caller, bytesWritten, digest, path, remote)
}

func unchangedWasmArtifactUploadLogMessage(digest string, remote string, caller string) string {
	return fmt.Sprintf("Wasm artifact upload skipped: caller=%s unchanged sha256=%s remote=%s", caller, digest, remote)
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

	result, err := runWasm(&req)
	if err != nil {
		log.Printf("Wasm execution failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, ProcessResponse{Error: err.Error()})
		return
	}

	log.Printf("process_event() returned: %d", result)
	writeJSON(w, http.StatusOK, ProcessResponse{Result: result})
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
	tmpPath := wasmPath + ".upload"
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
	currentDigest := currentWasmDigest()
	caller := requestArtifactCaller(r)
	if uploadDigest == currentDigest {
		_ = os.Remove(tmpPath)
		log.Print(unchangedWasmArtifactUploadLogMessage(uploadDigest, r.RemoteAddr, caller))
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "unchanged",
			"bytes":  bytesWritten,
			"path":   wasmPath,
			"digest": uploadDigest,
		})
		return
	}

	if _, _, _, err := compileWasmModule(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("Wasm artifact upload rejected from %s: %v", r.RemoteAddr, err)
		http.Error(w, "invalid wasm upload: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.Rename(tmpPath, wasmPath); err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("Wasm artifact upload failed: rename %s -> %s: %v", tmpPath, wasmPath, err)
		http.Error(w, "failed to install uploaded wasm", http.StatusInternalServerError)
		return
	}
	if err := reloadWasmModule(wasmPath); err != nil {
		log.Printf("Wasm artifact upload failed: reload %s: %v", wasmPath, err)
		http.Error(w, "failed to reload uploaded wasm", http.StatusInternalServerError)
		return
	}

	log.Print(wasmArtifactUploadLogMessage(bytesWritten, uploadDigest, wasmPath, r.RemoteAddr, caller))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"bytes":  bytesWritten,
		"path":   wasmPath,
		"digest": uploadDigest,
	})
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
	mux.HandleFunc("/health", handleHealth)

	log.Printf("sif-edge-host listening on :%s (wasm=%s)", port, wasmPath)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}
