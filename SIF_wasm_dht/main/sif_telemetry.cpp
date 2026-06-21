#include "sif_telemetry.hpp"

#include <stdio.h>
#include <sys/stat.h>

#include "esp_log.h"

#include "sif_wasmPull.hpp"

extern unsigned char dht_reader_wasm[];
extern unsigned int dht_reader_wasm_len;

static const char *TAG = "SifTelemetry";
static char g_topic[160];
static bool g_topic_ready = false;
static const char *LOCAL_WASM_PATH = "/spiffs/dht_reader.wasm";

static bool active_artifact_digest(char out_digest[SIF_WASM_SHA256_HEX_SIZE]) {
  struct stat st;
  if (stat(LOCAL_WASM_PATH, &st) == 0 && st.st_size > 0) {
    return sif_wasm_digest_file(LOCAL_WASM_PATH, out_digest) == ESP_OK;
  }
  return sif_wasm_digest_blob(dht_reader_wasm, dht_reader_wasm_len,
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

esp_err_t sif_telemetry_publish(esp_mqtt_client_handle_t client,
                                sif_state::Mode mode,
                                uint8_t battery_percent,
                                bool simulated,
                                int voltage_mv) {
  if (!client) return ESP_ERR_INVALID_STATE;

  char artifact_digest[SIF_WASM_SHA256_HEX_SIZE] = {};
  bool have_digest = active_artifact_digest(artifact_digest);

  char payload[320];
  int len = 0;
  if (have_digest) {
    len = snprintf(payload, sizeof(payload),
                   "{\"batteryPercent\":%u,\"mode\":\"%s\",\"source\":\"%s\",\"voltageMv\":%d,\"artifactDigest\":\"%s\"}",
                   (unsigned)battery_percent, sif_state::mode_to_string(mode),
                   simulated ? "simulated" : "real", voltage_mv,
                   artifact_digest);
  } else {
    ESP_LOGW(TAG, "active artifact digest unavailable for telemetry publish");
    len = snprintf(payload, sizeof(payload),
                   "{\"batteryPercent\":%u,\"mode\":\"%s\",\"source\":\"%s\",\"voltageMv\":%d}",
                   (unsigned)battery_percent, sif_state::mode_to_string(mode),
                   simulated ? "simulated" : "real", voltage_mv);
  }
  if (len < 0 || len >= (int)sizeof(payload)) {
    return ESP_ERR_INVALID_SIZE;
  }

  if (esp_mqtt_client_publish(client, sif_telemetry_topic(), payload, len, 1,
                              0) < 0) {
    ESP_LOGW(TAG, "publish to %s failed", sif_telemetry_topic());
    return ESP_FAIL;
  }
  return ESP_OK;
}

esp_err_t sif_telemetry_publish_current(esp_mqtt_client_handle_t client,
                                        BatteryGauge *gauge) {
  sif_state::State st;
  sif_state::load(st);

  uint8_t battery = st.battery;
  int voltage_mv = 0;
  if (gauge && !st.simulate_battery) {
    voltage_mv = gauge->getVoltage();
    uint16_t soc_raw = gauge->getStateOfCharge();
    battery = (soc_raw > 100) ? 100 : (uint8_t)soc_raw;
    sif_state::set_battery(battery);
  }

  return sif_telemetry_publish(client, st.mode, battery, st.simulate_battery,
                               voltage_mv);
}