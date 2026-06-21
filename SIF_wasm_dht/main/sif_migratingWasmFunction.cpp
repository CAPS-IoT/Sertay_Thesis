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
#include "sif_release.hpp"
#include "esp_timer.h"
#include <sys/stat.h>
#include <stdio.h>
#include <stdint.h>

static const char *TAG = "MigratingWasm";
static const float LIGHT_LUX_EMULATED = 120.0f;
static const float OCCUPANCY_DISTANCE_CM_EMULATED = 85.0f;
static const bool GPIO_BUTTON_PRESSED_EMULATED = false;

struct deadline_window_t {
  uint64_t deadline_us;
  uint64_t event_timestamp_us;
};

static deadline_window_t earliest_deadline_window_us(SubscriberFunction *fn, EventMap &e_map) {
  uint64_t deadline = UINT64_MAX;
  uint64_t timestamp = UINT64_MAX;
  for (auto &[type, event] : e_map) {
    if (!event) continue;
    uint64_t event_deadline = fn->getDeadline(event);
    if (event_deadline < deadline) {
      deadline = event_deadline;
      timestamp = event->getTimeStamp();
    }
  }
  return deadline_window_t{deadline, timestamp};
}

static sif_invocation_metrics_t local_invocation_metrics(
    const char *function_name, int64_t start_us, int64_t finish_us,
    deadline_window_t deadline, float temperature, float humidity,
    float battery_percent, float battery_voltage_mv) {
  sif_invocation_metrics_t metrics = {};
  metrics.function_name = function_name;
  metrics.execution_ms = (int32_t)((finish_us - start_us) / 1000);
  if (metrics.execution_ms < 0) metrics.execution_ms = 0;
  metrics.has_timing = deadline.deadline_us != UINT64_MAX;
  if (metrics.has_timing) {
    int64_t slack_us = (int64_t)deadline.deadline_us - finish_us;
    metrics.deadline_slack_ms = (int32_t)(slack_us / 1000);
    metrics.deadline_missed = slack_us < 0;
    if (deadline.event_timestamp_us != UINT64_MAX &&
        deadline.deadline_us >= deadline.event_timestamp_us) {
      metrics.deadline_target_ms =
          (int32_t)((deadline.deadline_us - deadline.event_timestamp_us) / 1000);
      int64_t queue_delay_us = start_us - (int64_t)deadline.event_timestamp_us;
      metrics.queue_delay_ms = queue_delay_us > 0 ? (int32_t)(queue_delay_us / 1000) : 0;
    }
  }
  metrics.has_dht = true;
  metrics.dht_temperature = temperature;
  metrics.dht_humidity = humidity;
  metrics.has_battery_resource = true;
  metrics.battery_percent = battery_percent;
  metrics.battery_voltage_mv = battery_voltage_mv;
  metrics.has_light = true;
  metrics.light_lux = LIGHT_LUX_EMULATED;
  metrics.has_occupancy = true;
  metrics.occupancy_distance_cm = OCCUPANCY_DISTANCE_CM_EMULATED;
  metrics.has_gpio = true;
  metrics.gpio_button_pressed = GPIO_BUTTON_PRESSED_EMULATED;
  return metrics;
}

static void collect_dht_inputs(MigratingWasmFunction *fn, float *temperature,
                               float *humidity) {
  *temperature = 22.0f;
  *humidity = 50.0f;

  auto rmap = fn->getResourceMap();
  auto it = rmap.find("DHT");
  if (it == rmap.end()) {
    return;
  }

  auto *dht = static_cast<DHT *>(it->second);
  float measured_humidity = 0.0f;
  float measured_temperature = 0.0f;
  if (dht->getDhtReadingFloat(&measured_humidity, &measured_temperature) == ESP_OK) {
    *temperature = measured_temperature;
    *humidity = measured_humidity;
  }
}

// Local-mode subscriber: execute the Wasm guest through WAMR, update battery
// state, and persist an edge-mode transition when the low threshold is reached.
esp_err_t MigratingWasmFunction::run(EventMap e_map) {
  deadline_window_t deadline = earliest_deadline_window_us(this, e_map);
  int64_t start_us = esp_timer_get_time();
  esp_err_t err = WasmFunction::run(e_map);
  int64_t finish_us = esp_timer_get_time();

  float temperature = 22.0f;
  float humidity = 50.0f;
  collect_dht_inputs(this, &temperature, &humidity);
  sif_state::State st;
  sif_state::load_summary(st);

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
  std::string function_name = getName();
  sif_invocation_metrics_t metrics = local_invocation_metrics(
      function_name.c_str(), start_us, finish_us, deadline, temperature,
      humidity, (float)soc, (float)voltage_mv);
    metrics.has_dht =
      sif_release_input_declared("DHT", "temperature", "f32") ||
      sif_release_input_declared("DHT", "humidity", "f32");
    metrics.has_battery_resource =
      sif_release_input_declared("BATTERY", "percent", "i32") ||
      sif_release_input_declared("BATTERY", "voltageMv", "i32");
    metrics.has_light = sif_release_input_declared("LIGHT", "lux", "f32");
    metrics.has_occupancy =
      sif_release_input_declared("OCCUPANCY", "distanceCm", "f32");
    metrics.has_gpio =
      sif_release_input_declared("GPIO", "buttonPressed", "bool");

  if (g_mqtt_resource) {
    esp_mqtt_client_handle_t handle = g_mqtt_resource->getMqttClient();
    if (handle) {
      sif_telemetry_publish(handle, sif_state::Mode::Local, soc,
                            st.simulate_battery, voltage_mv, &metrics);
    }
  }

  return err;
}
