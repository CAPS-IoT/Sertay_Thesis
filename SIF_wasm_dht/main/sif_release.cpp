#include "sif_release.hpp"

#include <errno.h>
#include <new>
#include <pthread.h>
#include <strings.h>
#include <sys/stat.h>
#include <utility>

#include "cJSON.h"
#include "esp_log.h"
#include "esp_system.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "sif_contractPolicy.hpp"
#include "sif_httpForward.hpp"
#include "sif_led.hpp"
#include "sif_scheduler.hpp"
#include "sif_telemetry.hpp"
#include "sif_wasmPull.hpp"
#include "typeNumerations.hpp"

static const char *TAG = "SifRelease";
static const char *ACTIVE_WASM_PATH = "/spiffs/dht_reader.wasm";
static const char *STAGED_WASM_PATH = "/spiffs/dht_reader.staged.wasm";
static pthread_mutex_t s_release_mutex = PTHREAD_MUTEX_INITIALIZER;
static WasmFunction *s_local_function = nullptr;
static SubscriberFunction *s_active_function = nullptr;
static SifContractPolicy s_active_contract_policy;
static bool s_transition_in_progress = false;

struct stage_task_args {
  std::string command_id;
  std::string artifact_url;
  sif_state::ReleaseMetadata release;
};

struct activation_task_args {
  std::string command_id;
  uint64_t generation;
  sif_state::Mode mode;
};

enum class transition_action { None, Stage, Activate };

// Staging and activation never need to execute concurrently. One persistent
// worker avoids late task-stack allocation without reserving two stacks from
// the ESP32's constrained internal RAM.
static constexpr uint32_t TRANSITION_TASK_STACK_BYTES = 6144;
static constexpr uint32_t TRANSITION_TASK_STACK_WORDS =
    (TRANSITION_TASK_STACK_BYTES + sizeof(StackType_t) - 1) /
    sizeof(StackType_t);
static StaticTask_t s_transition_task_buffer;
static StackType_t s_transition_task_stack[TRANSITION_TASK_STACK_WORDS];
static TaskHandle_t s_transition_task_handle = nullptr;
static transition_action s_transition_action = transition_action::None;
static stage_task_args s_stage_args;
static activation_task_args s_activation_args;

static bool compile_contract_policy(const std::string &contract_json,
                                    SifContractPolicy &policy) {
  policy = {};
  cJSON *contract = contract_json.empty()
                        ? nullptr
                        : cJSON_Parse(contract_json.c_str());
  if (!cJSON_IsObject(contract)) {
    cJSON_Delete(contract);
    return false;
  }

  cJSON *inputs = cJSON_GetObjectItem(contract, "inputs");
  cJSON *outputs = cJSON_GetObjectItem(contract, "outputs");
  // Kubernetes JSON serialization omits empty slices. A missing inputs or
  // outputs member therefore means an empty capability set, while a present
  // non-array member is malformed.
  if ((inputs && !cJSON_IsArray(inputs)) ||
      (outputs && !cJSON_IsArray(outputs))) {
    cJSON_Delete(contract);
    return false;
  }

  cJSON *entry = nullptr;
  cJSON_ArrayForEach(entry, inputs) {
    cJSON *resource = cJSON_GetObjectItem(entry, "name");
    cJSON *keys = cJSON_GetObjectItem(entry, "keys");
    if (!cJSON_IsString(resource) || !cJSON_IsArray(keys)) continue;
    cJSON *key = nullptr;
    cJSON_ArrayForEach(key, keys) {
      cJSON *key_name = cJSON_GetObjectItem(key, "name");
      cJSON *key_type = cJSON_GetObjectItem(key, "type");
      if (!cJSON_IsString(key_name) || !cJSON_IsString(key_type)) continue;
      policy.inputs |= sif_contract_policy::input_bit(
          resource->valuestring, strlen(resource->valuestring),
          key_name->valuestring, strlen(key_name->valuestring),
          key_type->valuestring, strlen(key_type->valuestring));
    }
  }

  cJSON_ArrayForEach(entry, outputs) {
    cJSON *name = cJSON_GetObjectItem(entry, "name");
    cJSON *type = cJSON_GetObjectItem(entry, "type");
    if (!cJSON_IsString(name) || !cJSON_IsString(type)) continue;
    policy.outputs |= sif_contract_policy::output_bit(
        name->valuestring, strlen(name->valuestring), type->valuestring,
        strlen(type->valuestring));
  }
  policy.valid = true;
  cJSON_Delete(contract);
  return true;
}

static bool release_equal(const sif_state::ReleaseMetadata &left,
                          const sif_state::ReleaseMetadata &right) {
  return left.generation == right.generation &&
         left.artifact_digest == right.artifact_digest &&
         left.function_identity == right.function_identity &&
         left.resource_contract_json == right.resource_contract_json;
}

static void publish_release_state() {
  if (!g_mqtt_resource) return;
  esp_mqtt_client_handle_t client = g_mqtt_resource->getMqttClient();
  if (client) sif_telemetry_publish_current(client, nullptr);
}

static esp_err_t close_admission_and_drain() {
  auto &scheduler = Scheduler::getInstance();
  scheduler.setFunctionAdmission(FunctionType::wasmProcess, false);
  constexpr unsigned DRAIN_ATTEMPTS = 100;
  for (unsigned attempt = 0; attempt < DRAIN_ATTEMPTS; ++attempt) {
    if (scheduler.getAdmittedInvocationCount(FunctionType::wasmProcess) == 0) {
      return ESP_OK;
    }
    vTaskDelay(pdMS_TO_TICKS(50));
  }
  return ESP_ERR_TIMEOUT;
}

static void restore_persisted_admission() {
  sif_state::State state;
  if (sif_state::load_summary(state) == ESP_OK && !state.admission_paused) {
    Scheduler::getInstance().setFunctionAdmission(
        FunctionType::wasmProcess, true);
  }
}

static esp_err_t promote_staged_file() {
  struct stat staged = {};
  if (stat(STAGED_WASM_PATH, &staged) != 0 || staged.st_size <= 0) {
    return ESP_ERR_NOT_FOUND;
  }
  std::string backup = std::string(ACTIVE_WASM_PATH) + ".previous";
  remove(backup.c_str());
  if (rename(ACTIVE_WASM_PATH, backup.c_str()) != 0 && errno != ENOENT) {
    return ESP_FAIL;
  }
  if (rename(STAGED_WASM_PATH, ACTIVE_WASM_PATH) != 0) {
    rename(backup.c_str(), ACTIVE_WASM_PATH);
    return ESP_FAIL;
  }
  remove(backup.c_str());
  return ESP_OK;
}

static void process_stage() {
  size_t bytes = 0;
  // The HTTP client and WAMR host-contract parsing share the small system
  // heap. Give staging exclusive ownership of invocation admission so a slow
  // or unreachable artifact server cannot trigger guest failures and a retry
  // storm while the HTTP client holds its buffers.
  esp_err_t err = close_admission_and_drain();
  if (err == ESP_OK) {
    err = sif_wasm_pull_blob(s_stage_args.artifact_url.c_str(),
                             STAGED_WASM_PATH, &bytes);
  } else {
    ESP_LOGE(TAG, "Timed out draining invocations before release staging");
  }
  char digest[SIF_WASM_SHA256_HEX_SIZE] = {};
  if (err == ESP_OK) {
    err = sif_wasm_digest_file(STAGED_WASM_PATH, digest);
  }
  if (err == ESP_OK &&
      strcasecmp(digest, s_stage_args.release.artifact_digest.c_str()) != 0) {
    ESP_LOGE(TAG, "Staged digest %s does not match release digest %s", digest,
             s_stage_args.release.artifact_digest.c_str());
    err = ESP_ERR_INVALID_CRC;
  }
  if (err == ESP_OK) {
    err = sif_state::set_staged_release(s_stage_args.release);
  }
  if (err == ESP_OK) {
    sif_state::set_last_command_id(s_stage_args.command_id);
    ESP_LOGI(TAG, "Staged release generation=%llu function=%s bytes=%u sha256=%s",
             static_cast<unsigned long long>(s_stage_args.release.generation),
             s_stage_args.release.function_identity.c_str(),
             static_cast<unsigned>(bytes), digest);
    publish_release_state();
  } else {
    remove(STAGED_WASM_PATH);
    ESP_LOGE(TAG, "Release staging failed: %s", esp_err_to_name(err));
  }
}

static void process_activation() {
  esp_err_t drain_err = close_admission_and_drain();
  if (drain_err != ESP_OK) {
    ESP_LOGE(TAG, "Timed out draining invocations before release activation");
    return;
  }

  sif_state::State state;
  esp_err_t err = sif_state::load(state);
  bool was_paused = state.admission_paused;
  bool staged_matches = err == ESP_OK && state.staged_release.valid() &&
                        state.staged_release.generation == s_activation_args.generation;
  bool active_matches = err == ESP_OK && state.active_release.valid() &&
                        state.active_release.generation == s_activation_args.generation;
  bool activate_staged = staged_matches && !active_matches;
  SifContractPolicy staged_policy;
  if (err == ESP_OK && !staged_matches && !active_matches) {
    err = ESP_ERR_INVALID_STATE;
  }
  if (err == ESP_OK && activate_staged &&
      !compile_contract_policy(state.staged_release.resource_contract_json,
                               staged_policy)) {
    ESP_LOGE(TAG, "Invalid or unsupported staged resource contract");
    err = ESP_ERR_INVALID_ARG;
  }
  if (err == ESP_OK && activate_staged &&
      s_activation_args.mode == sif_state::Mode::Local && s_local_function) {
    err = s_local_function->reloadFromPath(STAGED_WASM_PATH);
  }
  if (err == ESP_OK && activate_staged) err = promote_staged_file();
  if (err == ESP_OK && activate_staged) {
    err = sif_state::activate_staged_release(state.staged_release);
  }
  if (err == ESP_OK && activate_staged && s_active_function &&
      !state.staged_release.function_identity.empty()) {
    // The subscriber object survives a release change when the runtime mode
    // stays the same. Keep the scheduler/dispatcher identity aligned with the
    // generation-bound identity used by telemetry and HTTP forwarding.
    s_active_function->setName(state.staged_release.function_identity);
  }
  if (err == ESP_OK && activate_staged) {
    pthread_mutex_lock(&s_release_mutex);
    s_active_contract_policy = staged_policy;
    pthread_mutex_unlock(&s_release_mutex);
    sif_led_on_release_activated(state.staged_release.function_identity,
        sif_release_output_declared("actuatorCommand", "i32"));
  }
  if (err == ESP_OK) err = sif_state::set_mode(s_activation_args.mode);
  if (err == ESP_OK) err = sif_state::set_last_command_id(s_activation_args.command_id);

  if (err == ESP_OK) {
    ESP_LOGI(TAG, "Activated release generation=%llu runtimeMode=%s",
             static_cast<unsigned long long>(s_activation_args.generation),
             sif_state::mode_to_string(s_activation_args.mode));
    publish_release_state();
    if (state.mode != s_activation_args.mode) {
      if (!was_paused) sif_state::set_admission_paused(false);
      vTaskDelay(pdMS_TO_TICKS(200));
      esp_restart();
    }
    if (!was_paused) {
      sif_state::set_admission_paused(false);
    }
  } else {
    ESP_LOGE(TAG, "Release activation failed: %s", esp_err_to_name(err));
  }
}

static void transition_task(void *parameter) {
  (void)parameter;
  for (;;) {
    ulTaskNotifyTake(pdTRUE, portMAX_DELAY);
    pthread_mutex_lock(&s_release_mutex);
    transition_action action = s_transition_action;
    pthread_mutex_unlock(&s_release_mutex);
    if (action == transition_action::Stage) {
      process_stage();
    } else if (action == transition_action::Activate) {
      process_activation();
    }
    pthread_mutex_lock(&s_release_mutex);
    if (action == transition_action::Stage) {
      stage_task_args empty;
      s_stage_args = std::move(empty);
    } else if (action == transition_action::Activate) {
      activation_task_args empty;
      s_activation_args = std::move(empty);
    }
    s_transition_action = transition_action::None;
    s_transition_in_progress = false;
    pthread_mutex_unlock(&s_release_mutex);
    // Keep admission closed until all command strings and the new contract
    // have been released. Otherwise an invocation can overlap this final
    // cleanup window and contend with it for the ESP32's fragmented heap.
    if (action == transition_action::Stage ||
        action == transition_action::Activate) {
      restore_persisted_admission();
    }
  }
}

void sif_release_init(sif_state::State &initial_state) {
  if (!s_transition_task_handle) {
    s_transition_task_handle = xTaskCreateStatic(
        transition_task, "release_worker", TRANSITION_TASK_STACK_BYTES,
        nullptr, 6, s_transition_task_stack, &s_transition_task_buffer);
  }
  if (!s_transition_task_handle) {
    ESP_LOGE(TAG, "Failed to initialize static release worker");
  }

  Scheduler::getInstance().setFunctionAdmission(
      FunctionType::wasmProcess, !initial_state.admission_paused);
  SifContractPolicy initial_policy;
  if (!compile_contract_policy(
          initial_state.active_release.resource_contract_json,
          initial_policy)) {
    ESP_LOGE(TAG, "Unable to compile active resource contract");
  }
  pthread_mutex_lock(&s_release_mutex);
  s_active_contract_policy = initial_policy;
  pthread_mutex_unlock(&s_release_mutex);
  sif_led_on_release_activated(initial_state.active_release.function_identity,
      sif_release_output_declared("actuatorCommand", "i32"));
}

void sif_release_set_local_function(WasmFunction *function) {
  pthread_mutex_lock(&s_release_mutex);
  s_local_function = function;
  s_active_function = function;
  pthread_mutex_unlock(&s_release_mutex);
}

void sif_release_set_active_function(SubscriberFunction *function) {
  pthread_mutex_lock(&s_release_mutex);
  s_active_function = function;
  pthread_mutex_unlock(&s_release_mutex);
}

esp_err_t sif_release_stage_async(std::string command_id,
                                  std::string artifact_url,
                                  sif_state::ReleaseMetadata release) {
  if (command_id.empty() || artifact_url.empty() || !release.valid()) {
    return ESP_ERR_INVALID_ARG;
  }
  sif_state::State state;
  esp_err_t err = sif_state::load_summary(state);
  if (err != ESP_OK) return err;
  if (release.generation < state.active_release.generation ||
      release.generation < state.staged_release.generation) {
    return ESP_ERR_INVALID_VERSION;
  }

  const bool active_generation_matches =
      state.active_release.generation == release.generation;
  const bool staged_generation_matches =
      state.staged_release.generation == release.generation;
  if (active_generation_matches || staged_generation_matches) {
    const auto summary_conflicts = [&release](
        const sif_state::ReleaseMetadata &existing) {
      return existing.artifact_digest != release.artifact_digest ||
             existing.function_identity != release.function_identity;
    };
    if ((active_generation_matches && summary_conflicts(state.active_release)) ||
        (staged_generation_matches && summary_conflicts(state.staged_release))) {
      return ESP_ERR_INVALID_STATE;
    }

    // Contracts are the largest persisted release field. Load them only for a
    // same-generation idempotency/conflict check, never for a new release.
    sif_state::State detailed_state;
    err = sif_state::load(detailed_state);
    if (err != ESP_OK) return err;
    if ((active_generation_matches &&
         !release_equal(detailed_state.active_release, release)) ||
        (staged_generation_matches &&
         !release_equal(detailed_state.staged_release, release))) {
      return ESP_ERR_INVALID_STATE;
    }
    publish_release_state();
    return ESP_OK;
  }

  pthread_mutex_lock(&s_release_mutex);
  if (!s_transition_task_handle) {
    pthread_mutex_unlock(&s_release_mutex);
    return ESP_ERR_NO_MEM;
  }
  if (s_transition_in_progress) {
    bool duplicate = s_transition_action == transition_action::Stage &&
                     s_stage_args.command_id == command_id &&
                     release_equal(s_stage_args.release, release);
    pthread_mutex_unlock(&s_release_mutex);
    return duplicate ? ESP_OK : ESP_ERR_INVALID_STATE;
  }
  try {
    s_stage_args.command_id = std::move(command_id);
    s_stage_args.artifact_url = std::move(artifact_url);
    s_stage_args.release = std::move(release);
  } catch (const std::bad_alloc &) {
    s_stage_args = {};
    pthread_mutex_unlock(&s_release_mutex);
    return ESP_ERR_NO_MEM;
  }
  s_transition_action = transition_action::Stage;
  s_transition_in_progress = true;
  pthread_mutex_unlock(&s_release_mutex);

  xTaskNotifyGive(s_transition_task_handle);
  return ESP_OK;
}

static esp_err_t activate_async(const std::string &command_id,
                                uint64_t generation, sif_state::Mode mode) {
  if (command_id.empty() || generation == 0) return ESP_ERR_INVALID_ARG;
  sif_state::State state;
  esp_err_t err = sif_state::load(state);
  if (err != ESP_OK) return err;
  bool active_matches = state.active_release.valid() &&
                        state.active_release.generation == generation;
  bool staged_matches = state.staged_release.valid() &&
                        state.staged_release.generation == generation;
  if (active_matches && state.mode == mode) {
    publish_release_state();
    return ESP_OK;
  }
  if (!active_matches && !staged_matches) {
    return ESP_ERR_INVALID_STATE;
  }

  pthread_mutex_lock(&s_release_mutex);
  if (!s_transition_task_handle) {
    pthread_mutex_unlock(&s_release_mutex);
    return ESP_ERR_NO_MEM;
  }
  if (s_transition_in_progress) {
    bool duplicate = s_transition_action == transition_action::Activate &&
                     s_activation_args.command_id == command_id &&
                     s_activation_args.generation == generation &&
                     s_activation_args.mode == mode;
    pthread_mutex_unlock(&s_release_mutex);
    return duplicate ? ESP_OK : ESP_ERR_INVALID_STATE;
  }
  s_activation_args.command_id = command_id;
  s_activation_args.generation = generation;
  s_activation_args.mode = mode;
  s_transition_action = transition_action::Activate;
  s_transition_in_progress = true;
  pthread_mutex_unlock(&s_release_mutex);

  xTaskNotifyGive(s_transition_task_handle);
  return ESP_OK;
}

esp_err_t sif_release_activate_local_async(const std::string &command_id,
                                           uint64_t generation) {
  return activate_async(command_id, generation, sif_state::Mode::Local);
}

esp_err_t sif_release_set_edge_async(const std::string &command_id,
                                     uint64_t generation) {
  return activate_async(command_id, generation, sif_state::Mode::Edge);
}

esp_err_t sif_release_pause(const std::string &command_id,
                            uint64_t generation) {
  sif_state::State state;
  esp_err_t err = sif_state::load(state);
  if (err != ESP_OK) return err;
  if (generation < state.active_release.generation) return ESP_ERR_INVALID_VERSION;
  Scheduler::getInstance().setFunctionAdmission(FunctionType::wasmProcess, false);
  err = sif_state::set_admission_paused(true);
  if (err == ESP_OK) err = sif_state::set_last_command_id(command_id);
  if (err == ESP_OK) publish_release_state();
  return err;
}

esp_err_t sif_release_resume(const std::string &command_id,
                             uint64_t generation) {
  sif_state::State state;
  esp_err_t err = sif_state::load(state);
  if (err != ESP_OK) return err;
  if (generation != state.active_release.generation) return ESP_ERR_INVALID_VERSION;
  err = sif_state::set_admission_paused(false);
  if (err == ESP_OK) err = sif_state::set_last_command_id(command_id);
  if (err == ESP_OK) {
    Scheduler::getInstance().setFunctionAdmission(FunctionType::wasmProcess, true);
    publish_release_state();
  }
  return err;
}

std::string sif_release_active_function_identity() {
  sif_state::State state;
  if (sif_state::load_summary(state) == ESP_OK &&
      !state.active_release.function_identity.empty()) {
    return state.active_release.function_identity;
  }
  return "basic-edge-demo";
}

uint64_t sif_release_active_generation() {
  sif_state::State state;
  return sif_state::load_summary(state) == ESP_OK
             ? state.active_release.generation
             : 0;
}

bool sif_release_input_declared(const char *resource, const char *key,
                                const char *type) {
  return resource && key && type &&
         sif_release_input_declared_n(resource, strlen(resource), key,
                                      strlen(key), type, strlen(type));
}

bool sif_release_input_declared_n(const char *resource, size_t resource_len,
                                  const char *key, size_t key_len,
                                  const char *type, size_t type_len) {
  uint32_t bit = sif_contract_policy::input_bit(
      resource, resource_len, key, key_len, type, type_len);
  if (bit == 0) return false;
  pthread_mutex_lock(&s_release_mutex);
  bool declared = s_active_contract_policy.valid &&
                  (s_active_contract_policy.inputs & bit) != 0;
  pthread_mutex_unlock(&s_release_mutex);
  return declared;
}

bool sif_release_output_declared(const char *name, const char *type) {
  return name && type && sif_release_output_declared_n(
                            name, strlen(name), type, strlen(type));
}

bool sif_release_output_declared_n(const char *name, size_t name_len,
                                   const char *type, size_t type_len) {
  uint32_t bit =
      sif_contract_policy::output_bit(name, name_len, type, type_len);
  if (bit == 0) return false;
  pthread_mutex_lock(&s_release_mutex);
  bool declared = s_active_contract_policy.valid &&
                  (s_active_contract_policy.outputs & bit) != 0;
  pthread_mutex_unlock(&s_release_mutex);
  return declared;
}
