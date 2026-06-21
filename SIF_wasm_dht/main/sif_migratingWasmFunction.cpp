#include "sif_migratingWasmFunction.hpp"

#include "esp_log.h"
#include "esp_wifi.h"
#include "esp_system.h"
#include "mqtt_client.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "sif_httpForward.hpp"
#include "sif_state.hpp"
#include "sif_telemetry.hpp"
#include <sys/stat.h>
#include <stdio.h>

static const char *TAG = "MigratingWasm";

// Local-mode subscriber: execute the Wasm guest through WAMR, update battery
// state, and persist an edge-mode transition when the low threshold is reached.
esp_err_t MigratingWasmFunction::run(EventMap e_map) {
  esp_err_t err = WasmFunction::run(e_map);

  sif_state::State st;
  sif_state::load(st);

  // Battery drain: real gauge > simulated > unchanged.
  uint8_t soc = st.battery;
  int voltage_mv = 0;
  if (gauge_ && !st.simulate_battery) {
    voltage_mv = gauge_->getVoltage();
    uint16_t soc_raw = gauge_->getStateOfCharge();
    soc = (soc_raw > 100) ? 100 : (uint8_t)soc_raw;
    sif_state::set_battery(soc);
    ESP_LOGI(TAG, "battery: %u%% (%.3fV) [LC709203F]", soc,
             voltage_mv / 1000.0f);
  } else if (st.simulate_battery) {
    soc = (st.battery > st.local_drain) ? (st.battery - st.local_drain) : 0;
    sif_state::set_battery(soc);
    ESP_LOGI(TAG, "battery: %u -> %u (simulated drain)", st.battery, soc);
  } else {
    ESP_LOGI(TAG, "battery: %u%% (unchanged, simulation off)", soc);
  }

  if (g_mqtt_resource) {
    esp_mqtt_client_handle_t handle = g_mqtt_resource->getMqttClient();
    if (handle) {
      sif_telemetry_publish(handle, sif_state::Mode::Local, soc,
                            st.simulate_battery, voltage_mv);
    }
  }

  return err;
}
