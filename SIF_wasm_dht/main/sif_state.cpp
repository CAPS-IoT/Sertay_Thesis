#include "sif_state.hpp"

#include <new>
#include <stdio.h>
#include <string.h>
#include "esp_log.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char *TAG = "SifState";
static const char *NS = "sif_state";

namespace sif_state {

static esp_err_t load_string(nvs_handle_t handle, const char *key,
                             std::string &out) {
  size_t length = 0;
  esp_err_t err = nvs_get_str(handle, key, nullptr, &length);
  if (err == ESP_ERR_NVS_NOT_FOUND) return ESP_OK;
  if (err != ESP_OK) return err;
  if (length == 0) return ESP_OK;
  try {
    out.resize(length);
  } catch (const std::bad_alloc &) {
    out.clear();
    return ESP_ERR_NO_MEM;
  }
  err = nvs_get_str(handle, key, out.data(), &length);
  if (err == ESP_OK &&
      !out.empty() && out.back() == '\0') {
    out.pop_back();
  }
  return err;
}

static void release_key(char out[16], const char *prefix,
                        const char *suffix) {
  snprintf(out, 16, "%s_%s", prefix, suffix);
}

static esp_err_t write_release(nvs_handle_t handle, const char *prefix,
                               const ReleaseMetadata &release) {
  char key[16];
  release_key(key, prefix, "gen");
  esp_err_t err = nvs_set_u64(handle, key, release.generation);
  release_key(key, prefix, "dig");
  if (err == ESP_OK) err = nvs_set_str(handle, key, release.artifact_digest.c_str());
  release_key(key, prefix, "fn");
  if (err == ESP_OK) err = nvs_set_str(handle, key, release.function_identity.c_str());
  release_key(key, prefix, "ctr");
  if (err == ESP_OK) err = nvs_set_str(handle, key, release.resource_contract_json.c_str());
  return err;
}

static esp_err_t load_release(nvs_handle_t handle, const char *prefix,
                              ReleaseMetadata &release,
                              bool include_contract) {
  char key[16];
  release_key(key, prefix, "gen");
  esp_err_t err = nvs_get_u64(handle, key, &release.generation);
  if (err == ESP_ERR_NVS_NOT_FOUND) err = ESP_OK;
  release_key(key, prefix, "dig");
  if (err == ESP_OK) err = load_string(handle, key, release.artifact_digest);
  release_key(key, prefix, "fn");
  if (err == ESP_OK) err = load_string(handle, key, release.function_identity);
  if (include_contract) {
    release_key(key, prefix, "ctr");
    if (err == ESP_OK) err = load_string(handle, key, release.resource_contract_json);
  }
  return err;
}

static esp_err_t erase_release(nvs_handle_t handle, const char *prefix) {
  const char *suffixes[] = {"gen", "dig", "fn", "ctr"};
  char key[16];
  for (const char *suffix : suffixes) {
    release_key(key, prefix, suffix);
    esp_err_t err = nvs_erase_key(handle, key);
    if (err != ESP_OK && err != ESP_ERR_NVS_NOT_FOUND) return err;
  }
  return ESP_OK;
}

const char *mode_to_string(Mode m) {
  return (m == Mode::Edge) ? "edge" : "local";
}

Mode mode_from_string(const char *s) {
  if (s && strcmp(s, "edge") == 0) return Mode::Edge;
  return Mode::Local;
}

static esp_err_t load_impl(State &out, bool include_large_fields) {
  nvs_handle_t h;
  esp_err_t err = nvs_open(NS, NVS_READONLY, &h);
  if (err == ESP_ERR_NVS_NOT_FOUND) {
    ESP_LOGI(TAG, "namespace not found yet, using defaults");
    return ESP_OK;
  }
  if (err != ESP_OK) return err;

  // mode
  char buf[16] = {};
  size_t blen = sizeof(buf);
  if (nvs_get_str(h, "mode", buf, &blen) == ESP_OK) {
    out.mode = mode_from_string(buf);
  }

  // battery
  uint8_t u8 = 0;
  if (nvs_get_u8(h, "battery", &u8) == ESP_OK) out.battery = u8;
  if (nvs_get_u8(h, "low_th", &u8) == ESP_OK) out.low_threshold = u8;
  if (nvs_get_u8(h, "high_th", &u8) == ESP_OK) out.high_threshold = u8;
  if (nvs_get_u8(h, "drain", &u8) == ESP_OK) out.local_drain = u8;
  if (nvs_get_u8(h, "recover", &u8) == ESP_OK) out.edge_recover = u8;
  if (nvs_get_u8(h, "actuator", &u8) == ESP_OK) {
    out.actuator_command = (u8 == 1 || u8 == 2) ? u8 : 0;
  }
  if (nvs_get_u8(h, "sim_batt", &u8) == ESP_OK) out.simulate_battery = (u8 != 0);

  err = load_release(h, "act", out.active_release, include_large_fields);
  if (err == ESP_OK) {
    err = load_release(h, "stg", out.staged_release, include_large_fields);
  }
  if (nvs_get_u8(h, "paused", &u8) == ESP_OK) out.admission_paused = (u8 != 0);
  if (include_large_fields && err == ESP_OK) {
    err = load_string(h, "last_cmd", out.last_command_id);
  }
  if (include_large_fields && err == ESP_OK) {
    err = load_string(h, "last_dec", out.last_deadline_decision_id);
  }

  nvs_close(h);
  if (err != ESP_OK) return err;
  ESP_LOGD(TAG, "loaded: mode=%s batt=%u low=%u high=%u drain=%u recover=%u activeGen=%llu stagedGen=%llu paused=%s",
           mode_to_string(out.mode), out.battery, out.low_threshold,
           out.high_threshold, out.local_drain, out.edge_recover,
           static_cast<unsigned long long>(out.active_release.generation),
           static_cast<unsigned long long>(out.staged_release.generation),
           out.admission_paused ? "true" : "false");
  return ESP_OK;
}

esp_err_t load(State &out) {
  return load_impl(out, true);
}

esp_err_t load_summary(State &out) {
  return load_impl(out, false);
}

static esp_err_t open_rw(nvs_handle_t *h) {
  return nvs_open(NS, NVS_READWRITE, h);
}

esp_err_t set_mode(Mode m) {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = nvs_set_str(h, "mode", mode_to_string(m));
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t set_battery(uint8_t soc) {
  if (soc > 100) soc = 100;
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = nvs_set_u8(h, "battery", soc);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t set_actuator_command(uint8_t command) {
  if (command != 1 && command != 2) command = 0;
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = nvs_set_u8(h, "actuator", command);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t set_thresholds(uint8_t low, uint8_t high) {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  if (err == ESP_OK) err = nvs_set_u8(h, "low_th", low);
  if (err == ESP_OK) err = nvs_set_u8(h, "high_th", high);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t set_active_release(const ReleaseMetadata &release) {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = write_release(h, "act", release);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t set_staged_release(const ReleaseMetadata &release) {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = write_release(h, "stg", release);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t activate_staged_release(const ReleaseMetadata &release) {
  if (!release.valid()) return ESP_ERR_INVALID_ARG;
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  uint64_t staged_generation = 0;
  err = nvs_get_u64(h, "stg_gen", &staged_generation);
  if (err == ESP_OK && staged_generation != release.generation) {
    err = ESP_ERR_INVALID_VERSION;
  }
  if (err == ESP_OK) err = write_release(h, "act", release);
  if (err == ESP_OK) err = erase_release(h, "stg");
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t clear_staged_release() {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = erase_release(h, "stg");
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t set_admission_paused(bool paused) {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = nvs_set_u8(h, "paused", paused ? 1 : 0);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

static esp_err_t set_string_value(const char *key, const std::string &value) {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = nvs_set_str(h, key, value.c_str());
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t set_last_command_id(const std::string &command_id) {
  return set_string_value("last_cmd", command_id);
}

esp_err_t set_last_deadline_decision_id(const std::string &decision_id) {
  return set_string_value("last_dec", decision_id);
}

esp_err_t set_simulate_battery(bool enabled) {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = nvs_set_u8(h, "sim_batt", enabled ? 1 : 0);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t set_simulation_steps(uint8_t local_drain, uint8_t edge_recover) {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  if (err == ESP_OK) err = nvs_set_u8(h, "drain", local_drain);
  if (err == ESP_OK) err = nvs_set_u8(h, "recover", edge_recover);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

}  // namespace sif_state
