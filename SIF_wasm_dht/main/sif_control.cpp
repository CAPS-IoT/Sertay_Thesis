#include "sif_control.hpp"

#include <stddef.h>
#include <string.h>
#include <utility>
#include "esp_log.h"
#include "cJSON.h"
#include "sif_controlMessageAssembler.hpp"
#include "sif_state.hpp"
#include "sif_release.hpp"
#include "sif_led.hpp"

static const char *TAG = "SifControl";
static const char *g_topic = nullptr;
static constexpr size_t CONTROL_MESSAGE_CAPACITY = 2048;
static SifControlMessageAssembler<CONTROL_MESSAGE_CAPACITY>
    g_control_message;

static bool is_sha256_hex(const char *value) {
  if (!value || strlen(value) != 64) return false;
  for (size_t i = 0; i < 64; ++i) {
    char c = value[i];
    bool digit = (c >= '0' && c <= '9');
    bool lower = (c >= 'a' && c <= 'f');
    bool upper = (c >= 'A' && c <= 'F');
    if (!digit && !lower && !upper) return false;
  }
  return true;
}

static bool command_string(cJSON *root, const char *name, std::string &out) {
  cJSON *item = cJSON_GetObjectItem(root, name);
  if (!cJSON_IsString(item) || item->valuestring[0] == '\0') return false;
  out = item->valuestring;
  return true;
}

static bool command_generation(cJSON *root, uint64_t &generation) {
  cJSON *item = cJSON_GetObjectItem(root, "releaseGeneration");
  if (!cJSON_IsNumber(item) || item->valuedouble <= 0) return false;
  generation = static_cast<uint64_t>(item->valuedouble);
  return true;
}

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
  } else if (strcmp(a, "stage_release") == 0) {
    std::string command_id;
    std::string artifact_url;
    std::string artifact_digest;
    std::string function_identity;
    uint64_t generation = 0;
    cJSON *contract = cJSON_GetObjectItem(root, "resourceContract");
    if (!command_string(root, "commandId", command_id) ||
        !command_string(root, "artifactURL", artifact_url) ||
        !command_string(root, "artifactDigest", artifact_digest) ||
        !command_string(root, "functionIdentity", function_identity) ||
        !command_generation(root, generation) || !cJSON_IsObject(contract) ||
        !is_sha256_hex(artifact_digest.c_str())) {
      ESP_LOGW(TAG, "invalid stage_release payload");
    } else {
      char *contract_json = cJSON_PrintUnformatted(contract);
      if (!contract_json) {
        ESP_LOGE(TAG, "failed to serialize release resource contract");
      } else {
        sif_state::ReleaseMetadata release;
        release.generation = generation;
        release.artifact_digest = artifact_digest;
        release.function_identity = function_identity;

        // cJSON owns several heap allocations for this relatively large
        // command. Release that parse tree before waking the download worker;
        // otherwise esp_http_client_init() races the callback teardown and can
        // fail even though enough heap becomes available a moment later.
        cJSON_Delete(root);
        root = nullptr;
        release.resource_contract_json = contract_json;
        cJSON_free(contract_json);
        esp_err_t err = sif_release_stage_async(
            std::move(command_id), std::move(artifact_url), std::move(release));
        if (err == ESP_OK) {
          ESP_LOGD(TAG, "accepted stage_release generation=%llu function=%s",
                   static_cast<unsigned long long>(generation),
                   function_identity.c_str());
        } else {
          ESP_LOGW(TAG, "rejected stage_release generation=%llu function=%s: %s",
                   static_cast<unsigned long long>(generation),
                   function_identity.c_str(), esp_err_to_name(err));
        }
      }
    }
  } else if (strcmp(a, "activate_local") == 0) {
    std::string command_id;
    uint64_t generation = 0;
    if (command_string(root, "commandId", command_id) &&
        command_generation(root, generation)) {
      esp_err_t err = sif_release_activate_local_async(command_id, generation);
      if (err == ESP_OK) {
        ESP_LOGD(TAG, "accepted activate_local generation=%llu",
                 static_cast<unsigned long long>(generation));
      } else {
        ESP_LOGW(TAG, "rejected activate_local generation=%llu: %s",
                 static_cast<unsigned long long>(generation), esp_err_to_name(err));
      }
    } else {
      ESP_LOGW(TAG, "invalid activate_local payload");
    }
  } else if (strcmp(a, "set_runtime_mode") == 0) {
    std::string command_id;
    uint64_t generation = 0;
    cJSON *value = cJSON_GetObjectItem(root, "value");
    if (command_string(root, "commandId", command_id) &&
        command_generation(root, generation) && cJSON_IsString(value) &&
        strcmp(value->valuestring, "edge") == 0) {
      esp_err_t err = sif_release_set_edge_async(command_id, generation);
      if (err == ESP_OK) {
        ESP_LOGD(TAG, "accepted set_runtime_mode edge generation=%llu",
                 static_cast<unsigned long long>(generation));
      } else {
        ESP_LOGW(TAG, "rejected set_runtime_mode edge generation=%llu: %s",
                 static_cast<unsigned long long>(generation), esp_err_to_name(err));
      }
    } else {
      ESP_LOGW(TAG, "invalid set_runtime_mode payload");
    }
  } else if (strcmp(a, "pause_function") == 0 || strcmp(a, "resume_function") == 0) {
    std::string command_id;
    uint64_t generation = 0;
    if (command_string(root, "commandId", command_id) &&
        command_generation(root, generation)) {
      esp_err_t err = strcmp(a, "pause_function") == 0
                          ? sif_release_pause(command_id, generation)
                          : sif_release_resume(command_id, generation);
      ESP_LOGI(TAG, "%s generation=%llu -> %s", a,
               static_cast<unsigned long long>(generation), esp_err_to_name(err));
    } else {
      ESP_LOGW(TAG, "invalid %s payload", a);
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
    sif_state::load_summary(st);
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
      sif_state::load_summary(st);
      int value = v->valueint;
      if (value < 0) value = 0;
      if (value > 100) value = 100;
      sif_state::set_simulation_steps((uint8_t)value, st.edge_recover);
      ESP_LOGI(TAG, "set_drain -> %d", value);
    }
  } else if (strcmp(a, "signal_deadline_rejection") == 0) {
    std::string decision_id;
    std::string function_identity;
    if (command_string(root, "decisionId", decision_id) &&
        command_string(root, "functionIdentity", function_identity)) {
      esp_err_t err = sif_led_signal_deadline_rejection(decision_id,
                                                        function_identity);
      ESP_LOGI(TAG, "signal_deadline_rejection decisionId=%s -> %s",
               decision_id.c_str(), esp_err_to_name(err));
    } else {
      ESP_LOGW(TAG, "invalid signal_deadline_rejection payload");
    }
  } else {
    ESP_LOGW(TAG, "unknown action '%s'", a);
  }

  if (root) cJSON_Delete(root);
}

static void event_handler(void *handler_args, esp_event_base_t base,
                          int32_t event_id, void *event_data) {
  auto *data = (esp_mqtt_event_t *)event_data;
  if (event_id == MQTT_EVENT_CONNECTED && g_topic) {
    g_control_message.reset();
    int id = esp_mqtt_client_subscribe(data->client, g_topic, 1);
    ESP_LOGI(TAG, "subscribed to %s (msg_id=%d)", g_topic, id);
  } else if (event_id == MQTT_EVENT_DATA && g_topic) {
    const bool topic_present = data->topic && data->topic_len > 0;
    const bool control_topic =
        topic_present && data->topic_len == (int)strlen(g_topic) &&
        strncmp(data->topic, g_topic, data->topic_len) == 0;

    if (data->data_len < 0 || data->total_data_len < 0 ||
        data->current_data_offset < 0) {
      g_control_message.reset();
      ESP_LOGW(TAG, "discarding control message with invalid fragment lengths");
      return;
    }

    SifControlMessageStatus status = g_control_message.consume(
        topic_present, control_topic, data->data,
        static_cast<size_t>(data->data_len),
        static_cast<size_t>(data->total_data_len),
        static_cast<size_t>(data->current_data_offset));
    if (status == SifControlMessageStatus::rejected) {
      ESP_LOGW(TAG,
               "discarding malformed or oversized control message "
               "fragment (total=%d offset=%d fragment=%d capacity=%u)",
               data->total_data_len, data->current_data_offset, data->data_len,
               static_cast<unsigned>(CONTROL_MESSAGE_CAPACITY));
    } else if (status == SifControlMessageStatus::complete) {
      if (data->total_data_len > data->data_len) {
        ESP_LOGI(TAG, "assembled fragmented control message (%d bytes)",
                 data->total_data_len);
      }
      ESP_LOGD(TAG, "complete control msg: %.*s",
               static_cast<int>(g_control_message.payloadLength()),
               g_control_message.payload());
      sif_control_handle_json(g_control_message.payload(),
                              static_cast<int>(
                                  g_control_message.payloadLength()));
      g_control_message.reset();
    }
  } else if (event_id == MQTT_EVENT_DISCONNECTED) {
    g_control_message.reset();
  }
}

void sif_control_register(esp_mqtt_client_handle_t client, const char *topic) {
  g_topic = topic;
  g_control_message.reset();
  esp_mqtt_client_register_event(client, MQTT_EVENT_ANY, event_handler, nullptr);
  // Subscribe immediately as well as on CONNECT; duplicate subscriptions are
  // harmless and cover the case where MQTT was already connected.
  int id = esp_mqtt_client_subscribe(client, topic, 1);
  ESP_LOGI(TAG, "control register: %s (subscribe msg_id=%d)", topic, id);
}
