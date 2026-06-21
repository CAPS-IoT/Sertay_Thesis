#include "sif_control.hpp"

#include <string.h>
#include "esp_log.h"
#include "esp_system.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "cJSON.h"
#include "sif_state.hpp"
#include "sif_httpForward.hpp"

static const char *TAG = "SifControl";
static const char *g_topic = nullptr;

void sif_control_handle_json(const char *json, int len) {
  cJSON *root = cJSON_ParseWithLength(json, len);
  if (!root) {
    ESP_LOGW(TAG, "invalid JSON command");
    return;
  }
  cJSON *action = cJSON_GetObjectItem(root, "action");
  if (!cJSON_IsString(action)) {
    ESP_LOGW(TAG, "missing 'action'");
    cJSON_Delete(root);
    return;
  }

  bool restart = false;
  const char *a = action->valuestring;
  if (strcmp(a, "set_battery") == 0) {
    cJSON *v = cJSON_GetObjectItem(root, "value");
    if (cJSON_IsNumber(v)) {
      int b = v->valueint;
      if (b < 0) b = 0;
      if (b > 100) b = 100;
      sif_state::set_battery((uint8_t)b);
      ESP_LOGI(TAG, "set_battery -> %d", b);
    }
  } else if (strcmp(a, "set_mode") == 0) {
    cJSON *v = cJSON_GetObjectItem(root, "value");
    if (cJSON_IsString(v)) {
      sif_state::Mode m = sif_state::mode_from_string(v->valuestring);
      sif_state::State st;
      sif_state::load(st);
      if (st.mode == m) {
        ESP_LOGI(TAG, "set_mode -> %s (already active)", sif_state::mode_to_string(m));
      } else {
        sif_state::set_mode(m);
        ESP_LOGI(TAG, "set_mode -> %s (rebooting)", sif_state::mode_to_string(m));
        restart = true;
      }
    }
  } else if (strcmp(a, "set_thresholds") == 0) {
    cJSON *lo = cJSON_GetObjectItem(root, "low");
    cJSON *hi = cJSON_GetObjectItem(root, "high");
    if (cJSON_IsNumber(lo) && cJSON_IsNumber(hi)) {
      sif_state::set_thresholds((uint8_t)lo->valueint, (uint8_t)hi->valueint);
      ESP_LOGI(TAG, "set_thresholds low=%d high=%d", lo->valueint, hi->valueint);
    }
  } else if (strcmp(a, "set_battery_source") == 0) {
    cJSON *v = cJSON_GetObjectItem(root, "value");
    if (cJSON_IsString(v)) {
      bool simulated = strcmp(v->valuestring, "simulated") == 0 ||
                       strcmp(v->valuestring, "simulation") == 0 ||
                       strcmp(v->valuestring, "sim") == 0;
      sif_state::set_simulate_battery(simulated);
      ESP_LOGI(TAG, "set_battery_source -> %s", simulated ? "simulated" : "real");
    }
  } else if (strcmp(a, "set_simulation") == 0) {
    sif_state::State st;
    sif_state::load(st);
    cJSON *enabled = cJSON_GetObjectItem(root, "enabled");
    cJSON *drain = cJSON_GetObjectItem(root, "drain");
    cJSON *recover = cJSON_GetObjectItem(root, "recover");
    if (cJSON_IsBool(enabled)) {
      sif_state::set_simulate_battery(cJSON_IsTrue(enabled));
      ESP_LOGI(TAG, "set_simulation enabled=%s", cJSON_IsTrue(enabled) ? "true" : "false");
    }
    uint8_t next_drain = st.local_drain;
    uint8_t next_recover = st.edge_recover;
    if (cJSON_IsNumber(drain)) {
      int value = drain->valueint;
      if (value < 0) value = 0;
      if (value > 100) value = 100;
      next_drain = (uint8_t)value;
    }
    if (cJSON_IsNumber(recover)) {
      int value = recover->valueint;
      if (value < 0) value = 0;
      if (value > 100) value = 100;
      next_recover = (uint8_t)value;
    }
    if (cJSON_IsNumber(drain) || cJSON_IsNumber(recover)) {
      sif_state::set_simulation_steps(next_drain, next_recover);
      ESP_LOGI(TAG, "set_simulation drain=%u recover=%u", next_drain, next_recover);
    }
  } else if (strcmp(a, "set_drain") == 0) {
    cJSON *v = cJSON_GetObjectItem(root, "value");
    if (cJSON_IsNumber(v)) {
      sif_state::State st;
      sif_state::load(st);
      int value = v->valueint;
      if (value < 0) value = 0;
      if (value > 100) value = 100;
      sif_state::set_simulation_steps((uint8_t)value, st.edge_recover);
      ESP_LOGI(TAG, "set_drain -> %d", value);
    }
  } else if (strcmp(a, "reload") == 0) {
    cJSON *u = cJSON_GetObjectItem(root, "url");
    if (cJSON_IsString(u)) {
      sif_state::set_pull_url(u->valuestring);
      sif_state::set_mode(sif_state::Mode::Local);  // reload needs local WAMR path
      ESP_LOGI(TAG, "reload from %s — switching to LOCAL and rebooting",
               u->valuestring);
      restart = true;
    }
  } else {
    ESP_LOGW(TAG, "unknown action '%s'", a);
  }

  cJSON_Delete(root);

  if (restart) {
    // Graceful shutdown: stop MQTT first so the broker sees a clean
    // DISCONNECT, then tear down WiFi.
    // Suppress internal MQTT/transport error logs during socket teardown.
    esp_log_level_set("mqtt_client", ESP_LOG_NONE);
    esp_log_level_set("transport_base", ESP_LOG_NONE);
    if (g_mqtt_resource) {
      esp_mqtt_client_handle_t h = g_mqtt_resource->getMqttClient();
      if (h) esp_mqtt_client_stop(h);
    }
    esp_wifi_disconnect();
    esp_wifi_stop();
    vTaskDelay(pdMS_TO_TICKS(300));
    esp_restart();
  }
}

static void event_handler(void *handler_args, esp_event_base_t base,
                          int32_t event_id, void *event_data) {
  auto *data = (esp_mqtt_event_t *)event_data;
  if (event_id == MQTT_EVENT_CONNECTED && g_topic) {
    int id = esp_mqtt_client_subscribe(data->client, g_topic, 1);
    ESP_LOGI(TAG, "subscribed to %s (msg_id=%d)", g_topic, id);
  } else if (event_id == MQTT_EVENT_DATA && g_topic) {
    if (data->topic_len == (int)strlen(g_topic) &&
        strncmp(data->topic, g_topic, data->topic_len) == 0) {
      ESP_LOGI(TAG, "control msg: %.*s", data->data_len, data->data);
      sif_control_handle_json(data->data, data->data_len);
    }
  }
}

void sif_control_register(esp_mqtt_client_handle_t client, const char *topic) {
  g_topic = topic;
  esp_mqtt_client_register_event(client, MQTT_EVENT_ANY, event_handler, nullptr);
  // Subscribe immediately as well as on CONNECT; duplicate subscriptions are
  // harmless and cover the case where MQTT was already connected.
  int id = esp_mqtt_client_subscribe(client, topic, 1);
  ESP_LOGI(TAG, "control register: %s (subscribe msg_id=%d)", topic, id);
}
