#include "sif_telemetry.hpp"

#include <stdio.h>
#include <string.h>
#include <sys/stat.h>

#include "cJSON.h"
#include "esp_log.h"

#include "sif_wasmPull.hpp"

extern unsigned char basic_edge_demo_wasm[];
extern unsigned int basic_edge_demo_wasm_len;

static const char *TAG = "SifTelemetry";
static char g_topic[160];
static bool g_topic_ready = false;
static const char *LOCAL_WASM_PATH = "/spiffs/dht_reader.wasm";

static void add_resource_f32(cJSON *inputs, const char *resource,
                             const char *key, float value) {
  if (!inputs || !resource || !key) return;
  cJSON *resource_obj = cJSON_GetObjectItem(inputs, resource);
  if (!resource_obj) {
    resource_obj = cJSON_AddObjectToObject(inputs, resource);
  }
  if (resource_obj) {
    cJSON *item = cJSON_AddObjectToObject(resource_obj, key);
    if (!item) return;
    cJSON_AddStringToObject(item, "type", "f32");
    cJSON_AddNumberToObject(item, "value", value);
  }
}

static void add_resource_i32(cJSON *inputs, const char *resource,
                             const char *key, int32_t value) {
  if (!inputs || !resource || !key) return;
  cJSON *resource_obj = cJSON_GetObjectItem(inputs, resource);
  if (!resource_obj) {
    resource_obj = cJSON_AddObjectToObject(inputs, resource);
  }
  if (resource_obj) {
    cJSON *item = cJSON_AddObjectToObject(resource_obj, key);
    if (!item) return;
    cJSON_AddStringToObject(item, "type", "i32");
    cJSON_AddNumberToObject(item, "value", value);
  }
}

static void add_resource_bool(cJSON *inputs, const char *resource,
                              const char *key, bool value) {
  if (!inputs || !resource || !key) return;
  cJSON *resource_obj = cJSON_GetObjectItem(inputs, resource);
  if (!resource_obj) {
    resource_obj = cJSON_AddObjectToObject(inputs, resource);
  }
  if (resource_obj) {
    cJSON *item = cJSON_AddObjectToObject(resource_obj, key);
    if (!item) return;
    cJSON_AddStringToObject(item, "type", "bool");
    cJSON_AddBoolToObject(item, "value", value);
  }
}

static bool active_artifact_digest(char out_digest[SIF_WASM_SHA256_HEX_SIZE]) {
  struct stat st;
  if (stat(LOCAL_WASM_PATH, &st) == 0 && st.st_size > 0) {
    return sif_wasm_digest_file(LOCAL_WASM_PATH, out_digest) == ESP_OK;
  }
  return sif_wasm_digest_blob(basic_edge_demo_wasm, basic_edge_demo_wasm_len,
                              out_digest) == ESP_OK;
}

const char *sif_telemetry_topic() {
  if (!g_topic_ready) {
    int written = snprintf(g_topic, sizeof(g_topic), "%s/telemetry",
                           CONFIG_DATA_TOPIC);
    if (written < 0) {
      g_topic[0] = '\0';
    } else if (written >= (int)sizeof(g_topic)) {
      g_topic[sizeof(g_topic) - 1] = '\0';
    }
    g_topic_ready = true;
  }
  return g_topic;
}

static esp_err_t publish_with_state(esp_mqtt_client_handle_t client,
                                    sif_state::Mode mode,
                                    uint8_t battery_percent,
                                    bool simulated,
                                    int voltage_mv,
                                    const sif_invocation_metrics_t *metrics,
                                    const sif_state::State &state) {
  if (!client) return ESP_ERR_INVALID_STATE;

  char artifact_digest[SIF_WASM_SHA256_HEX_SIZE] = {};
  bool have_digest = active_artifact_digest(artifact_digest);

  cJSON *root = cJSON_CreateObject();
  if (!root) return ESP_ERR_NO_MEM;

  cJSON_AddNumberToObject(root, "batteryPercent", (unsigned)battery_percent);
  cJSON_AddStringToObject(root, "mode", sif_state::mode_to_string(mode));
  cJSON_AddStringToObject(root, "source", simulated ? "simulated" : "real");
  cJSON_AddNumberToObject(root, "voltageMv", voltage_mv);
  cJSON_AddBoolToObject(root, "admissionPaused", state.admission_paused);
  if (have_digest) {
    cJSON_AddStringToObject(root, "artifactDigest", artifact_digest);
  } else {
    ESP_LOGW(TAG, "active artifact digest unavailable for telemetry publish");
  }
  cJSON_AddNumberToObject(root, "releaseGeneration",
                          static_cast<double>(state.active_release.generation));
  cJSON_AddNumberToObject(root, "stagedReleaseGeneration",
                          static_cast<double>(state.staged_release.generation));
  cJSON_AddStringToObject(root, "stagedArtifactDigest",
                          state.staged_release.artifact_digest.c_str());

  // The function identity belongs to the active release, not to the long-lived
  // subscriber object. A hot release can change this identity without rebooting
  // the device, so invocation metrics are only a legacy fallback.
  if (!state.active_release.function_identity.empty()) {
    cJSON_AddStringToObject(root, "function",
                            state.active_release.function_identity.c_str());
  } else if (metrics && metrics->function_name) {
    cJSON_AddStringToObject(root, "function", metrics->function_name);
  }
  if (metrics && metrics->has_timing) {
    cJSON_AddNumberToObject(root, "executionMs", metrics->execution_ms);
    cJSON_AddNumberToObject(root, "deadlineTargetMs", metrics->deadline_target_ms);
    cJSON_AddNumberToObject(root, "deadlineSlackMs", metrics->deadline_slack_ms);
    cJSON_AddBoolToObject(root, "deadlineMissed", metrics->deadline_missed);
    cJSON_AddNumberToObject(root, "queueDelayMs", metrics->queue_delay_ms);
    cJSON_AddNumberToObject(root, "resourceWakeMs", metrics->resource_wake_ms);
    cJSON_AddNumberToObject(root, "resourceCollectionMs", metrics->resource_collection_ms);
    cJSON_AddNumberToObject(root, "networkRoundTripMs", metrics->network_round_trip_ms);
    cJSON_AddNumberToObject(root, "edgeExecutionMs", metrics->edge_execution_ms);
    cJSON_AddNumberToObject(root, "outputApplicationMs", metrics->output_application_ms);
  }
  if (metrics && (metrics->has_dht || metrics->has_battery_resource ||
                  metrics->has_light || metrics->has_occupancy ||
                  metrics->has_gpio)) {
    cJSON *inputs = cJSON_AddObjectToObject(root, "resourceInputs");
    if (metrics->has_dht) {
      add_resource_f32(inputs, "DHT", "temperature", metrics->dht_temperature);
      add_resource_f32(inputs, "DHT", "humidity", metrics->dht_humidity);
    }
    if (metrics->has_battery_resource) {
      add_resource_i32(inputs, "BATTERY", "percent",
                       (int32_t)metrics->battery_percent);
      add_resource_i32(inputs, "BATTERY", "voltageMv",
                       (int32_t)metrics->battery_voltage_mv);
    }
    if (metrics->has_light) {
      add_resource_f32(inputs, "LIGHT", "lux", metrics->light_lux);
    }
    if (metrics->has_occupancy) {
      add_resource_f32(inputs, "OCCUPANCY", "distanceCm",
                       metrics->occupancy_distance_cm);
    }
    if (metrics->has_gpio) {
      add_resource_bool(inputs, "GPIO", "buttonPressed",
                        metrics->gpio_button_pressed);
    }
  }

  char *payload = cJSON_PrintUnformatted(root);
  cJSON_Delete(root);
  if (!payload) {
    return ESP_ERR_NO_MEM;
  }
  size_t len = strlen(payload);
  if (len > 1024) {
    cJSON_free(payload);
    return ESP_ERR_INVALID_SIZE;
  }

  if (esp_mqtt_client_publish(client, sif_telemetry_topic(), payload, (int)len, 1,
                              0) < 0) {
    ESP_LOGW(TAG, "publish to %s failed", sif_telemetry_topic());
    cJSON_free(payload);
    return ESP_FAIL;
  }
  cJSON_free(payload);
  return ESP_OK;
}

esp_err_t sif_telemetry_publish(esp_mqtt_client_handle_t client,
                                sif_state::Mode mode,
                                uint8_t battery_percent,
                                bool simulated,
                                int voltage_mv,
                                const sif_invocation_metrics_t *metrics) {
  sif_state::State state;
  esp_err_t err = sif_state::load_summary(state);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "telemetry state unavailable: %s", esp_err_to_name(err));
    return err;
  }
  return publish_with_state(client, mode, battery_percent, simulated,
                            voltage_mv, metrics, state);
}

esp_err_t sif_telemetry_publish_current(esp_mqtt_client_handle_t client,
                                        BatteryGauge *gauge) {
  sif_state::State st;
  esp_err_t err = sif_state::load_summary(st);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "current telemetry state unavailable: %s",
             esp_err_to_name(err));
    return err;
  }

  uint8_t battery = st.battery;
  int voltage_mv = 0;
  if (gauge && !st.simulate_battery) {
    voltage_mv = gauge->getVoltage();
    uint16_t soc_raw = gauge->getStateOfCharge();
    battery = (soc_raw > 100) ? 100 : (uint8_t)soc_raw;
    sif_state::set_battery(battery);
  }

  return publish_with_state(client, st.mode, battery, st.simulate_battery,
                            voltage_mv, nullptr, st);
}
