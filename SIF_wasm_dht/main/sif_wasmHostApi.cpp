#include "sif_wasmHostApi.hpp"
#include "sif_wasmFunction.hpp"
#include "sif_dht.hpp"
#include "esp_log.h"

static const char *TAG = "WasmHostApi";

// --- Host wrapper functions ---
// These are the FFI boundary between the sandboxed guest and SIF resources.
// The WasmFunction pointer is stored as user_data in the exec_env, giving the
// wrapper access to the resource map prepared by the SIF dispatcher.

static float get_temperature_wrapper(wasm_exec_env_t exec_env) {
  auto *wf = static_cast<WasmFunction *>(wasm_runtime_get_user_data(exec_env));
  auto rmap = wf->getResourceMap();
  auto it = rmap.find("DHT");
  if (it == rmap.end()) {
    // No DHT wired: return the same synthetic value used by edge-mode offload.
    ESP_LOGW(TAG, "DHT not present, returning synthetic temperature");
    return 22.0f;
  }
  auto *dht = static_cast<DHT *>(it->second);
  float humidity, temperature;
  esp_err_t err = dht->getDhtReadingFloat(&humidity, &temperature);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "DHT reading failed: %d", err);
    return -999.0f;
  }
  ESP_LOGI(TAG, "get_temperature() -> %.2f", temperature);
  return temperature;
}

static float get_humidity_wrapper(wasm_exec_env_t exec_env) {
  auto *wf = static_cast<WasmFunction *>(wasm_runtime_get_user_data(exec_env));
  auto rmap = wf->getResourceMap();
  auto it = rmap.find("DHT");
  if (it == rmap.end()) {
    ESP_LOGW(TAG, "DHT not present, returning synthetic humidity");
    return 50.0f;
  }
  auto *dht = static_cast<DHT *>(it->second);
  float humidity, temperature;
  esp_err_t err = dht->getDhtReadingFloat(&humidity, &temperature);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "DHT reading failed: %d", err);
    return -999.0f;
  }
  ESP_LOGI(TAG, "get_humidity() -> %.2f", humidity);
  return humidity;
}

static void log_message_wrapper(wasm_exec_env_t exec_env, const char *msg, uint32_t len) {
  ESP_LOGI("WasmGuest", "%.*s", (int)len, msg);
}

// --- Native symbol table ---
// The (*~) signature asks WAMR to translate and validate the guest pointer and
// length pair before entering log_message_wrapper.

static NativeSymbol native_symbols[] = {
    {"get_temperature", (void *)get_temperature_wrapper, "()f", NULL},
    {"get_humidity", (void *)get_humidity_wrapper, "()f", NULL},
    {"log_message", (void *)log_message_wrapper, "(*~)", NULL},
};

void register_wasm_native_apis() {
  int n = sizeof(native_symbols) / sizeof(NativeSymbol);
  if (!wasm_runtime_register_natives("env", native_symbols, n)) {
    ESP_LOGE(TAG, "Failed to register native APIs");
  } else {
    ESP_LOGI(TAG, "Registered %d native APIs for Wasm guests", n);
  }
}
