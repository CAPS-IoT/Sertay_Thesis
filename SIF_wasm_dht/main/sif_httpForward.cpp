#include "sif_httpForward.hpp"

#include "esp_log.h"
#include "esp_system.h"
#include "esp_http_client.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "mqtt_client.h"
#include "cJSON.h"
#include "sif_state.hpp"
#include "sif_telemetry.hpp"
#include "sif_led.hpp"
#include "sif_release.hpp"
#include "esp_timer.h"
#include <stdint.h>
#include <strings.h>
#include <string>

extern char *eventTypeToString(EventType type);

static const char *TAG = "HttpForward";
static const float DHT_TEMPERATURE_FALLBACK = 22.0f;
static const float DHT_HUMIDITY_FALLBACK = 50.0f;
static const float LIGHT_LUX_EMULATED = 120.0f;
static const float OCCUPANCY_DISTANCE_CM_EMULATED = 85.0f;
static const bool GPIO_BUTTON_PRESSED_EMULATED = false;
static constexpr int HTTP_POST_TIMEOUT_MS = 15000;
static constexpr int HTTP_POST_MAX_ATTEMPTS = 3;
static constexpr int HTTP_POST_RETRY_DELAY_MS = 1000;

MQTTClient *g_mqtt_resource = nullptr;



// HTTP response buffer shared by the edge-mode offload subscriber.
static char resp_buf[1024];
static int  resp_len;

struct deadline_window_t {
  uint64_t deadline_us;
  uint64_t event_timestamp_us;
};

static esp_err_t http_event_handler(esp_http_client_event_t *evt) {
  if (evt->event_id == HTTP_EVENT_ON_DATA) {
    int space = (int)sizeof(resp_buf) - 1 - resp_len;
    if (space > 0) {
      int copy = evt->data_len < space ? evt->data_len : space;
      memcpy(resp_buf + resp_len, evt->data, copy);
      resp_len += copy;
    }
  }
  return ESP_OK;
}

static deadline_window_t earliest_forward_deadline_window_us(SubscriberFunction *fn,
                                                             EventMap &e_map) {
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

static sif_invocation_metrics_t offload_invocation_metrics(
    const char *function_name, int64_t start_us, int64_t finish_us,
    deadline_window_t deadline, int64_t collection_finish_us,
    int64_t http_start_us, int64_t http_finish_us, int32_t edge_execution_ms,
    float temperature, float humidity, float battery_percent,
    float battery_voltage_mv) {
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
    int64_t collection_us = collection_finish_us - start_us;
    metrics.resource_collection_ms = collection_us > 0 ? (int32_t)(collection_us / 1000) : 0;
    int64_t http_us = http_finish_us - http_start_us;
    int32_t http_ms = http_us > 0 ? (int32_t)(http_us / 1000) : 0;
    metrics.edge_execution_ms = edge_execution_ms > 0 ? edge_execution_ms : 0;
    metrics.network_round_trip_ms = http_ms - metrics.edge_execution_ms;
    if (metrics.network_round_trip_ms < 0) metrics.network_round_trip_ms = 0;
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

static void add_typed_f32(cJSON *parent, const char *key, float value) {
  if (!parent || !key) return;
  cJSON *item = cJSON_AddObjectToObject(parent, key);
  if (!item) return;
  cJSON_AddStringToObject(item, "type", "f32");
  cJSON_AddNumberToObject(item, "value", value);
}

static void add_typed_i32(cJSON *parent, const char *key, int32_t value) {
  if (!parent || !key) return;
  cJSON *item = cJSON_AddObjectToObject(parent, key);
  if (!item) return;
  cJSON_AddStringToObject(item, "type", "i32");
  cJSON_AddNumberToObject(item, "value", value);
}

static void add_typed_bool(cJSON *parent, const char *key, bool value) {
  if (!parent || !key) return;
  cJSON *item = cJSON_AddObjectToObject(parent, key);
  if (!item) return;
  cJSON_AddStringToObject(item, "type", "bool");
  cJSON_AddBoolToObject(item, "value", value);
}

HttpForwardFunction::HttpForwardFunction(FunctionType ftype, std::string name,
                                         const char *edge_url,
                                         BatteryGauge *gauge)
    : SubscriberFunction(ftype, std::move(name),
                         std::list<std::string>{"WIFI"},
                         false),
      edge_url_(edge_url),
      gauge_(gauge) {}

uint64_t HttpForwardFunction::getDeadline(Event *event) {
  return event->getTimeStamp() + 10000000;  // 10s
}

esp_err_t HttpForwardFunction::run(EventMap e_map) {
  // The subscriber object survives same-mode release activation, so resolve
  // the generation-bound identity from persisted release state per invocation.
  std::string function_identity = sif_release_active_function_identity();
  deadline_window_t deadline = earliest_forward_deadline_window_us(this, e_map);
  int64_t start_us = esp_timer_get_time();
  float temperature = DHT_TEMPERATURE_FALLBACK;
  float humidity = DHT_HUMIDITY_FALLBACK;
  sif_state::State st;
  sif_state::load_summary(st);
  uint8_t soc = st.battery;
  int voltage_mv = 0;
  if (gauge_ && !st.simulate_battery) {
    voltage_mv = gauge_->getVoltage();
    uint16_t soc_raw = gauge_->getStateOfCharge();
    soc = (soc_raw > 100) ? 100 : (uint8_t)soc_raw;
  }

  // 1. Build the offload payload expected by the edge Go/wasmtime host.
  cJSON *root = cJSON_CreateObject();
  cJSON_AddStringToObject(root, "function", function_identity.c_str());
  cJSON_AddNumberToObject(root, "releaseGeneration",
                          static_cast<double>(sif_release_active_generation()));
  cJSON_AddStringToObject(root, "source", "esp32-edgemode");
  cJSON *inputs = cJSON_AddObjectToObject(root, "resourceInputs");
  bool dht_temperature_declared = sif_release_input_declared("DHT", "temperature", "f32");
  bool dht_humidity_declared = sif_release_input_declared("DHT", "humidity", "f32");
  if (inputs && (dht_temperature_declared || dht_humidity_declared)) {
    cJSON *dht = cJSON_AddObjectToObject(inputs, "DHT");
    if (dht_temperature_declared) add_typed_f32(dht, "temperature", temperature);
    if (dht_humidity_declared) add_typed_f32(dht, "humidity", humidity);
  }
  bool battery_percent_declared = sif_release_input_declared("BATTERY", "percent", "i32");
  bool battery_voltage_declared = sif_release_input_declared("BATTERY", "voltageMv", "i32");
  if (inputs && (battery_percent_declared || battery_voltage_declared)) {
    cJSON *battery = cJSON_AddObjectToObject(inputs, "BATTERY");
    if (battery_percent_declared) add_typed_i32(battery, "percent", (int32_t)soc);
    if (battery_voltage_declared) add_typed_i32(battery, "voltageMv", (int32_t)voltage_mv);
    ESP_LOGI(TAG, "resourceSource=real BATTERY.percent=%u BATTERY.voltageMv=%d",
             (unsigned)soc, voltage_mv);
  }
  bool light_declared = sif_release_input_declared("LIGHT", "lux", "f32");
  if (inputs && light_declared) {
    cJSON *light = cJSON_AddObjectToObject(inputs, "LIGHT");
    add_typed_f32(light, "lux", LIGHT_LUX_EMULATED);
    ESP_LOGI(TAG, "resourceSource=emulated LIGHT.lux=%.2f", LIGHT_LUX_EMULATED);
  }
  bool occupancy_declared = sif_release_input_declared("OCCUPANCY", "distanceCm", "f32");
  if (inputs && occupancy_declared) {
    cJSON *occupancy = cJSON_AddObjectToObject(inputs, "OCCUPANCY");
    add_typed_f32(occupancy, "distanceCm", OCCUPANCY_DISTANCE_CM_EMULATED);
    ESP_LOGI(TAG, "resourceSource=emulated OCCUPANCY.distanceCm=%.2f",
             OCCUPANCY_DISTANCE_CM_EMULATED);
  }
  bool gpio_declared = sif_release_input_declared("GPIO", "buttonPressed", "bool");
  if (inputs && gpio_declared) {
    cJSON *gpio = cJSON_AddObjectToObject(inputs, "GPIO");
    add_typed_bool(gpio, "buttonPressed", GPIO_BUTTON_PRESSED_EMULATED);
    ESP_LOGI(TAG, "resourceSource=emulated GPIO.buttonPressed=%s",
             GPIO_BUTTON_PRESSED_EMULATED ? "true" : "false");
  }
  cJSON *events = cJSON_AddArrayToObject(root, "events");
  for (auto &[type, event] : e_map) {
    cJSON *ev = cJSON_CreateObject();
    cJSON_AddStringToObject(ev, "type", eventTypeToString(type));
    cJSON_AddNumberToObject(ev, "timestamp", (double)event->getTimeStamp());
    cJSON_AddItemToArray(events, ev);
  }

  char *payload = cJSON_PrintUnformatted(root);
  cJSON_Delete(root);
  if (!payload) return ESP_FAIL;
  int64_t collection_finish_us = esp_timer_get_time();

  // 2. HTTP POST to the edge host. SIF has already woken the WiFi resource.
  esp_err_t err = ESP_FAIL;
  int status = 0;
  int64_t http_start_us = esp_timer_get_time();
  for (int attempt = 1; attempt <= HTTP_POST_MAX_ATTEMPTS; ++attempt) {
    resp_len = 0;
    esp_http_client_config_t config = {};
    config.url = edge_url_;
    config.method = HTTP_METHOD_POST;
    config.event_handler = http_event_handler;
    config.timeout_ms = HTTP_POST_TIMEOUT_MS;

    ESP_LOGI(TAG, "HTTP POST attempt %d/%d to %s", attempt,
             HTTP_POST_MAX_ATTEMPTS, edge_url_);
    esp_http_client_handle_t client = esp_http_client_init(&config);
    if (!client) {
      err = ESP_ERR_NO_MEM;
    } else {
      esp_http_client_set_header(client, "Content-Type", "application/json");
      esp_http_client_set_post_field(client, payload, strlen(payload));

      err = esp_http_client_perform(client);
      status = esp_http_client_get_status_code(client);
      esp_http_client_cleanup(client);
    }

    resp_buf[resp_len] = '\0';
    if (err == ESP_OK) {
      break;
    }

    ESP_LOGW(TAG, "HTTP POST attempt %d/%d failed: %s", attempt,
             HTTP_POST_MAX_ATTEMPTS, esp_err_to_name(err));
    if (attempt < HTTP_POST_MAX_ATTEMPTS) {
      vTaskDelay(pdMS_TO_TICKS(HTTP_POST_RETRY_DELAY_MS));
    }
  }
  int64_t http_finish_us = esp_timer_get_time();

  if (err != ESP_OK) {
    ESP_LOGE(TAG, "HTTP POST to %s failed: %s", edge_url_, esp_err_to_name(err));
    cJSON_free(payload);
    return ESP_FAIL;
  }

  // 3. Parse the host response and record the Wasm result for the monitor log.
  int32_t edge_execution_ms = 0;
  int32_t output_application_ms = 0;
  bool response_release_matches = false;
  if (status == 200) {
    cJSON *resp = cJSON_Parse(resp_buf);
    int result = -1;
    if (resp) {
      sif_state::State active_state;
      sif_state::load_summary(active_state);
      cJSON *response_function = cJSON_GetObjectItem(resp, "function");
      cJSON *response_generation = cJSON_GetObjectItem(resp, "releaseGeneration");
      cJSON *response_digest = cJSON_GetObjectItem(resp, "artifactDigest");
      response_release_matches = cJSON_IsString(response_function) &&
        cJSON_IsNumber(response_generation) && cJSON_IsString(response_digest) &&
        active_state.active_release.function_identity == response_function->valuestring &&
        active_state.active_release.generation ==
          static_cast<uint64_t>(response_generation->valuedouble) &&
        strcasecmp(active_state.active_release.artifact_digest.c_str(),
             response_digest->valuestring) == 0;
      cJSON *r = cJSON_GetObjectItem(resp, "result");
      if (r) result = r->valueint;
      cJSON *timing = cJSON_GetObjectItem(resp, "timing");
      cJSON *edge_ms = timing ? cJSON_GetObjectItem(timing, "edgeExecutionMs") : nullptr;
      if (edge_ms && cJSON_IsNumber(edge_ms) && edge_ms->valuedouble > 0) {
        edge_execution_ms = (int32_t)edge_ms->valuedouble;
      }
      cJSON *outputs = cJSON_GetObjectItem(resp, "outputs");
      cJSON *actuator = outputs ? cJSON_GetObjectItem(outputs, "actuatorCommand") : nullptr;
        if (response_release_matches && actuator && cJSON_IsNumber(actuator) &&
          sif_release_output_declared("actuatorCommand", "i32")) {
        int64_t output_start_us = esp_timer_get_time();
        sif_led_apply_actuator((int32_t)actuator->valuedouble);
        output_application_ms = static_cast<int32_t>(
            (esp_timer_get_time() - output_start_us) / 1000);
      }
      cJSON_Delete(resp);
    }
    if (!response_release_matches) {
      ESP_LOGE(TAG, "Edge response release metadata did not match active release");
      cJSON_free(payload);
      return ESP_ERR_INVALID_STATE;
    }
    ESP_LOGI(TAG, "Edge offload OK (status=%d, result=%d): %s", status, result, payload);
  } else {
    ESP_LOGW(TAG, "Edge offload returned status=%d body=%s", status, resp_buf);
  }
  cJSON_free(payload);
  int64_t finish_us = esp_timer_get_time();

  // Battery recovery: simulation mode or real gauge reading.
  sif_state::load_summary(st);

  if (gauge_ && !st.simulate_battery) {
    voltage_mv = gauge_->getVoltage();
    uint16_t soc_raw = gauge_->getStateOfCharge();
    soc = (soc_raw > 100) ? 100 : (uint8_t)soc_raw;
    sif_state::set_battery(soc);
    ESP_LOGI(TAG, "battery: %u%% (%.3fV) [LC709203F]", soc,
             voltage_mv / 1000.0f);
  } else if (st.simulate_battery) {
    uint16_t next = (uint16_t)st.battery + st.edge_recover;
    if (next > 100) next = 100;
    soc = (uint8_t)next;
    sif_state::set_battery(soc);
    ESP_LOGI(TAG, "battery: %u -> %u (simulated recovery)", st.battery, soc);
  } else {
    ESP_LOGI(TAG, "battery: %u%% (unchanged, simulation off)", soc);
  }
  sif_invocation_metrics_t metrics = offload_invocation_metrics(
      function_identity.c_str(), start_us, finish_us, deadline, collection_finish_us,
      http_start_us, http_finish_us, edge_execution_ms, temperature, humidity,
      (float)soc, (float)voltage_mv);
  metrics.output_application_ms = output_application_ms;
  metrics.has_dht = dht_temperature_declared || dht_humidity_declared;
  metrics.has_battery_resource = battery_percent_declared || battery_voltage_declared;
  metrics.has_light = light_declared;
  metrics.has_occupancy = occupancy_declared;
  metrics.has_gpio = gpio_declared;

  if (g_mqtt_resource) {
    esp_mqtt_client_handle_t handle = g_mqtt_resource->getMqttClient();
    if (handle) {
      sif_telemetry_publish(handle, sif_state::Mode::Edge, soc,
                            st.simulate_battery, voltage_mv, &metrics);
    }
  }

  return ESP_OK;
}
