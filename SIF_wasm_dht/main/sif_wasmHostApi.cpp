#include "sif_wasmHostApi.hpp"
#include "sif_wasmFunction.hpp"
#include "sif_dht.hpp"
#include "sif_state.hpp"
#include "sif_release.hpp"
#include "sif_led.hpp"
#include "esp_err.h"
#include "esp_log.h"
#include <string.h>

static const char *TAG = "WasmHostApi";
static const float RESOURCE_ERROR_SENTINEL = -999.0f;
static const float LIGHT_LUX_EMULATED = 120.0f;
static const float OCCUPANCY_DISTANCE_CM_EMULATED = 85.0f;
static const bool GPIO_BUTTON_PRESSED_EMULATED = false;
static BatteryGauge *s_battery_gauge = nullptr;

void set_wasm_battery_gauge(BatteryGauge *gauge) {
  s_battery_gauge = gauge;
}

// --- Host wrapper functions ---
// These are the FFI boundary between the sandboxed guest and SIF resources.
// The WasmFunction pointer is stored as user_data in the exec_env, giving the
// wrapper access to the resource map prepared by the SIF dispatcher.

static bool matches_token(const char *value, uint32_t len, const char *expected) {
  return value && expected && strlen(expected) == len &&
         strncmp(value, expected, len) == 0;
}

static float read_dht_value(wasm_exec_env_t exec_env, const char *key,
                            uint32_t key_len) {
  auto *wf = static_cast<WasmFunction *>(wasm_runtime_get_user_data(exec_env));
  if (!wf) {
    ESP_LOGE(TAG, "WasmFunction user_data missing");
    return RESOURCE_ERROR_SENTINEL;
  }
  bool want_temperature = matches_token(key, key_len, "temperature");
  bool want_humidity = matches_token(key, key_len, "humidity");
  if (!want_temperature && !want_humidity) {
    ESP_LOGW(TAG, "DHT key not found: %.*s", (int)key_len, key ? key : "");
    return RESOURCE_ERROR_SENTINEL;
  }

  auto rmap = wf->getResourceMap();
  auto it = rmap.find("DHT");
  if (it == rmap.end()) {
    ESP_LOGD(TAG, "DHT not present, returning synthetic %.*s",
             (int)key_len, key ? key : "");
    return want_temperature ? 22.0f : 50.0f;
  }
  auto *dht = static_cast<DHT *>(it->second);
  float humidity, temperature;
  esp_err_t err = dht->getDhtReadingFloat(&humidity, &temperature);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "DHT reading failed: %d", err);
    return RESOURCE_ERROR_SENTINEL;
  }
  return want_temperature ? temperature : humidity;
}

static float get_resource_f32_wrapper(wasm_exec_env_t exec_env,
                                      const char *resource,
                                      uint32_t resource_len,
                                      const char *key,
                                      uint32_t key_len) {
  if (!sif_release_input_declared_n(resource, resource_len, key, key_len,
                                    "f32", 3)) {
    ESP_LOGW(TAG, "undeclared f32 input: %.*s.%.*s", (int)resource_len,
             resource ? resource : "", (int)key_len, key ? key : "");
    return RESOURCE_ERROR_SENTINEL;
  }
  if (matches_token(resource, resource_len, "DHT")) {
    float value = read_dht_value(exec_env, key, key_len);
    ESP_LOGI(TAG, "get_resource_f32(%.*s.%.*s) -> %.2f",
             (int)resource_len, resource, (int)key_len, key, value);
    return value;
  }
  if (matches_token(resource, resource_len, "BATTERY")) {
    sif_state::State st;
    sif_state::load_summary(st);
    float value = RESOURCE_ERROR_SENTINEL;
    if (matches_token(key, key_len, "percent")) {
      value = (float)st.battery;
    } else if (matches_token(key, key_len, "voltageMv")) {
      value = s_battery_gauge ? (float)s_battery_gauge->getVoltage() : 0.0f;
    }
    ESP_LOGI(TAG, "get_resource_f32(%.*s.%.*s) -> %.2f",
             (int)resource_len, resource, (int)key_len, key, value);
    return value;
  }
  if (matches_token(resource, resource_len, "LIGHT") &&
      matches_token(key, key_len, "lux")) {
    ESP_LOGI(TAG, "get_resource_f32(LIGHT.lux) -> %.2f",
             LIGHT_LUX_EMULATED);
    return LIGHT_LUX_EMULATED;
  }
  if (matches_token(resource, resource_len, "OCCUPANCY") &&
      matches_token(key, key_len, "distanceCm")) {
    ESP_LOGI(TAG, "get_resource_f32(OCCUPANCY.distanceCm) -> %.2f",
             OCCUPANCY_DISTANCE_CM_EMULATED);
    return OCCUPANCY_DISTANCE_CM_EMULATED;
  }
  if (matches_token(resource, resource_len, "GPIO") &&
      matches_token(key, key_len, "buttonPressed")) {
    ESP_LOGI(TAG, "get_resource_f32(GPIO.buttonPressed) -> %.2f",
             GPIO_BUTTON_PRESSED_EMULATED);
    return GPIO_BUTTON_PRESSED_EMULATED;
  }
  ESP_LOGW(TAG, "resource not found: %.*s", (int)resource_len,
           resource ? resource : "");
  return RESOURCE_ERROR_SENTINEL;
}

static int32_t get_resource_i32_wrapper(wasm_exec_env_t exec_env,
                                        const char *resource,
                                        uint32_t resource_len,
                                        const char *key,
                                        uint32_t key_len) {
  if (!sif_release_input_declared_n(resource, resource_len, key, key_len,
                                    "i32", 3)) {
    ESP_LOGW(TAG, "undeclared i32 input: %.*s.%.*s", (int)resource_len,
             resource ? resource : "", (int)key_len, key ? key : "");
    return -999;
  }
  if (matches_token(resource, resource_len, "BATTERY")) {
    sif_state::State st;
    sif_state::load_summary(st);
    int32_t value = -999;
    if (matches_token(key, key_len, "percent")) {
      value = (int32_t)st.battery;
    } else if (matches_token(key, key_len, "voltageMv")) {
      value = s_battery_gauge ? (int32_t)s_battery_gauge->getVoltage() : 0;
    }
    ESP_LOGI(TAG, "get_resource_i32(%.*s.%.*s) -> %ld",
             (int)resource_len, resource, (int)key_len, key, (long)value);
    return value;
  }
  float value = get_resource_f32_wrapper(exec_env, resource, resource_len, key,
                                         key_len);
  return (int32_t)value;
}

static int32_t get_resource_bool_wrapper(wasm_exec_env_t exec_env,
                                         const char *resource,
                                         uint32_t resource_len,
                                         const char *key,
                                         uint32_t key_len) {
  (void)exec_env;
  if (!sif_release_input_declared_n(resource, resource_len, key, key_len,
                                    "bool", 4)) {
    ESP_LOGW(TAG, "undeclared bool input: %.*s.%.*s", (int)resource_len,
             resource ? resource : "", (int)key_len, key ? key : "");
    return 0;
  }
  bool value = false;
  bool found = false;
  if (matches_token(resource, resource_len, "GPIO") &&
      matches_token(key, key_len, "buttonPressed")) {
    value = GPIO_BUTTON_PRESSED_EMULATED;
    found = true;
  }
  if (!found) {
    ESP_LOGW(TAG, "bool resource not found: %.*s.%.*s",
             (int)resource_len, resource ? resource : "",
             (int)key_len, key ? key : "");
  } else {
    ESP_LOGI(TAG, "get_resource_bool(%.*s.%.*s) -> %s",
             (int)resource_len, resource, (int)key_len, key,
             value ? "true" : "false");
  }
  return value ? 1 : 0;
}

static void set_output_i32_wrapper(wasm_exec_env_t exec_env,
                                   const char *key,
                                   uint32_t key_len,
                                   int32_t value) {
  (void)exec_env;
  ESP_LOGI(TAG, "set_output_i32(%.*s) -> %ld",
           (int)key_len, key ? key : "", (long)value);
  if (!sif_release_output_declared_n(key, key_len, "i32", 3)) {
    ESP_LOGW(TAG, "ignoring undeclared i32 output: %.*s", (int)key_len,
             key ? key : "");
    return;
  }
  if (matches_token(key, key_len, "actuatorCommand")) {
    sif_led_apply_actuator(value);
  }
}

static void set_output_f32_wrapper(wasm_exec_env_t exec_env,
                                   const char *key,
                                   uint32_t key_len,
                                   float value) {
  (void)exec_env;
  ESP_LOGI(TAG, "set_output_f32(%.*s) -> %.2f",
           (int)key_len, key ? key : "", value);
  if (!sif_release_output_declared_n(key, key_len, "f32", 3)) {
    ESP_LOGW(TAG, "ignoring undeclared f32 output: %.*s", (int)key_len,
             key ? key : "");
  }
}

static void log_message_wrapper(wasm_exec_env_t exec_env, const char *msg, uint32_t len) {
  ESP_LOGI("WasmGuest", "%.*s", (int)len, msg);
}

// --- Native symbol table ---
// The (*~) signature asks WAMR to translate and validate the guest pointer and
// length pair before entering log_message_wrapper.

static NativeSymbol native_symbols[] = {
    {"get_resource_f32", (void *)get_resource_f32_wrapper, "(*~*~)f", NULL},
    {"get_resource_i32", (void *)get_resource_i32_wrapper, "(*~*~)i", NULL},
    {"get_resource_bool", (void *)get_resource_bool_wrapper, "(*~*~)i", NULL},
    {"set_output_i32", (void *)set_output_i32_wrapper, "(*~i)", NULL},
    {"set_output_f32", (void *)set_output_f32_wrapper, "(*~f)", NULL},
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
