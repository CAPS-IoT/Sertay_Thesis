#include "sif_state.hpp"

#include <string.h>
#include "esp_log.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char *TAG = "SifState";
static const char *NS = "sif_state";

namespace sif_state {

const char *mode_to_string(Mode m) {
  return (m == Mode::Edge) ? "edge" : "local";
}

Mode mode_from_string(const char *s) {
  if (s && strcmp(s, "edge") == 0) return Mode::Edge;
  return Mode::Local;
}

esp_err_t load(State &out) {
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
  if (nvs_get_u8(h, "sim_batt", &u8) == ESP_OK) out.simulate_battery = (u8 != 0);

  // pull_url
  size_t ulen = 0;
  if (nvs_get_str(h, "pull_url", nullptr, &ulen) == ESP_OK && ulen > 0) {
    out.pull_url.resize(ulen);
    nvs_get_str(h, "pull_url", out.pull_url.data(), &ulen);
    if (!out.pull_url.empty() && out.pull_url.back() == '\0') {
      out.pull_url.pop_back();
    }
  }

  nvs_close(h);
  ESP_LOGI(TAG, "loaded: mode=%s batt=%u low=%u high=%u drain=%u recover=%u url=%s",
           mode_to_string(out.mode), out.battery, out.low_threshold,
           out.high_threshold, out.local_drain, out.edge_recover,
           out.pull_url.empty() ? "(none)" : out.pull_url.c_str());
  return ESP_OK;
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

esp_err_t set_pull_url(const std::string &url) {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = nvs_set_str(h, "pull_url", url.c_str());
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

esp_err_t clear_pull_url() {
  nvs_handle_t h;
  esp_err_t err = open_rw(&h);
  if (err != ESP_OK) return err;
  err = nvs_erase_key(h, "pull_url");
  if (err == ESP_ERR_NVS_NOT_FOUND) err = ESP_OK;
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
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
