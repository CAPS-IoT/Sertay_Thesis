#include "sif_httpForward.hpp"

#include "esp_log.h"
#include "esp_system.h"
#include "esp_http_client.h"
#include "esp_wifi.h"
#include "mqtt_client.h"
#include "cJSON.h"
#include "sif_state.hpp"
#include "sif_telemetry.hpp"
#include <string>

extern char *eventTypeToString(EventType type);

static const char *TAG = "HttpForward";

MQTTClient *g_mqtt_resource = nullptr;



// HTTP response buffer shared by the edge-mode offload subscriber.
static char resp_buf[512];
static int  resp_len;

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
  // 1. Build the offload payload expected by the edge Go/wasmtime host.
  cJSON *root = cJSON_CreateObject();
  cJSON_AddStringToObject(root, "function", getName().c_str());
  cJSON_AddStringToObject(root, "source", "esp32-edgemode");
  cJSON_AddNumberToObject(root, "temperature", 22.0);
  cJSON_AddNumberToObject(root, "humidity", 50.0);
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

  // 2. HTTP POST to the edge host. SIF has already woken the WiFi resource.
  resp_len = 0;
  esp_http_client_config_t config = {};
  config.url = edge_url_;
  config.method = HTTP_METHOD_POST;
  config.event_handler = http_event_handler;
  config.timeout_ms = 5000;

  esp_http_client_handle_t client = esp_http_client_init(&config);
  esp_http_client_set_header(client, "Content-Type", "application/json");
  esp_http_client_set_post_field(client, payload, strlen(payload));

  esp_err_t err = esp_http_client_perform(client);
  int status = esp_http_client_get_status_code(client);
  esp_http_client_cleanup(client);

  resp_buf[resp_len] = '\0';

  if (err != ESP_OK) {
    ESP_LOGE(TAG, "HTTP POST to %s failed: %s", edge_url_, esp_err_to_name(err));
    cJSON_free(payload);
    return ESP_FAIL;
  }

  // 3. Parse the host response and record the Wasm result for the monitor log.
  if (status == 200) {
    cJSON *resp = cJSON_Parse(resp_buf);
    int result = -1;
    if (resp) {
      cJSON *r = cJSON_GetObjectItem(resp, "result");
      if (r) result = r->valueint;
      cJSON_Delete(resp);
    }
    ESP_LOGI(TAG, "Edge offload OK (status=%d, result=%d): %s", status, result, payload);
  } else {
    ESP_LOGW(TAG, "Edge offload returned status=%d body=%s", status, resp_buf);
  }
  cJSON_free(payload);

  // Battery recovery: simulation mode or real gauge reading.
  sif_state::State st;
  sif_state::load(st);

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
    uint16_t next = (uint16_t)st.battery + st.edge_recover;
    if (next > 100) next = 100;
    soc = (uint8_t)next;
    sif_state::set_battery(soc);
    ESP_LOGI(TAG, "battery: %u -> %u (simulated recovery)", st.battery, soc);
  } else {
    ESP_LOGI(TAG, "battery: %u%% (unchanged, simulation off)", soc);
  }

  if (g_mqtt_resource) {
    esp_mqtt_client_handle_t handle = g_mqtt_resource->getMqttClient();
    if (handle) {
      sif_telemetry_publish(handle, sif_state::Mode::Edge, soc,
                            st.simulate_battery, voltage_mv);
    }
  }

  return ESP_OK;
}
