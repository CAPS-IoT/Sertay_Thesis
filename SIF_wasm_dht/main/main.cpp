#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_log.h"
#include "esp_system.h"
#include "wasm_export.h"
#include "bh_platform.h"
#include <string.h>
#include <sys/stat.h>

#include "sif_scheduler.hpp"
#include "sif_functionFactory.hpp"
#include "sif_init.hpp"
#include "sif_migratingWasmFunction.hpp"
#include "sif_httpForward.hpp"
#include "sif_state.hpp"
#include "sif_control.hpp"
#include "sif_wasmHostApi.hpp"
#include "sif_wasmPull.hpp"
#include "sif_mqtt.hpp"
#include "sif_wifi.hpp"
#include "sif_batteryGauge.hpp"
#include "sif_telemetry.hpp"
#include "sif_release.hpp"
#include "sif_led.hpp"
#include "typeNumerations.hpp"
#include "basicWasmTrigger.hpp"
#include "basicWasmEvent.hpp"

// Embedded Wasm fallback (only used if SPIFFS is empty).
#include "basic_edge_demo_wasm.h"

extern "C" void app_main();

char *eventTypeToString(EventType type) {
  static char *strEventType[] = {
      (char *)"wasmTimer",
      (char *)"ErrorEvent",
      (char *)"GetNextDutyFreqEvent",
      (char *)"AdjustDutyFreqEvent",
      (char *)"GetChargingModelEvent",
  };
  return strEventType[(int)type];
}

char *fncTypeToString(FunctionType type) {
  static char *strFncType[] = {
      (char *)"wasmProcess",
      (char *)"HandleErrorFunction",
      (char *)"CalcDutyFreqFunction",
      (char *)"CalcChargingModelFunction",
      (char *)"setSleepTimeFunction",
  };
  return strFncType[(int)type];
}

static const char *DATA_TOPIC     = CONFIG_DATA_TOPIC;
static const uint16_t LOCAL_THRESHOLD = 0;  // placement is now driven by operator telemetry
static const char *LOCAL_WASM_PATH = "/spiffs/dht_reader.wasm";

// Initialized once and passed into both local and edge subscribers. When
// simulation is enabled in NVS, the subscriber logic still prefers the
// deterministic drain/recovery path over the hardware gauge.
static BatteryGauge *g_gauge = nullptr;

static bool compute_active_wasm_digest(char out_digest[SIF_WASM_SHA256_HEX_SIZE]) {
  struct stat st;
  if (stat(LOCAL_WASM_PATH, &st) == 0 && st.st_size > 0) {
    return sif_wasm_digest_file(LOCAL_WASM_PATH, out_digest) == ESP_OK;
  }
  return sif_wasm_digest_blob(basic_edge_demo_wasm, basic_edge_demo_wasm_len,
                              out_digest) == ESP_OK;
}

static SubscriberFunction *createWasmProcessLocal() {
  std::string function_identity = sif_release_active_function_identity();
  struct stat st;
  if (stat(LOCAL_WASM_PATH, &st) == 0 && st.st_size > 0) {
    printf("[Factory] Local Wasm from SPIFFS (%ld bytes)\n", (long)st.st_size);
    // SIF dispatches this subscriber locally; WAMR remains the only guest
    // execution boundary and the guest can reach hardware only via env imports.
    auto *function = new MigratingWasmFunction(
      FunctionType::wasmProcess, function_identity, {}, false,
      std::string(LOCAL_WASM_PATH), DATA_TOPIC, LOCAL_THRESHOLD, g_gauge);
    sif_release_set_local_function(function);
    return function;
  }
  printf("[Factory] SPIFFS empty, embedded fallback\n");
  // Embedded module keeps the demo bootable when no blob has been pulled yet.
    auto *function = new MigratingWasmFunction(
      FunctionType::wasmProcess, function_identity, {}, false,
      basic_edge_demo_wasm, basic_edge_demo_wasm_len, DATA_TOPIC, LOCAL_THRESHOLD,
      g_gauge);
    sif_release_set_local_function(function);
    return function;
}

static SubscriberFunction *createHttpForward() {
  // Edge-mode subscriber does not instantiate WAMR; it owns only the HTTP
  // offload step and the post-offload battery recovery check.
  auto *function = new HttpForwardFunction(
      FunctionType::wasmProcess, sif_release_active_function_identity(),
      CONFIG_EDGE_HOST_URL, g_gauge);
  sif_release_set_active_function(function);
  return function;
}

static void run_local_mode(const sif_state::State &st) {
  printf("[Mode] LOCAL — battery=%u low=%u high=%u source=%s drain=%u recover=%u\n",
         st.battery, st.low_threshold, st.high_threshold,
         st.simulate_battery ? "simulated" : "real",
         st.local_drain, st.edge_recover);

  // Use pool allocator — the system allocator's alignment wrapper has issues
  // with ESP-IDF's TLSF when called from pthreads (SIF worker threads).
  // The pool serves WAMR internal structures only. The SIF build enables a
  // separate static 64 KiB os_mmap slot for guest linear memory so networking
  // cannot fragment the contiguous block required during instantiation.
  static char wamr_heap_buf[48 * 1024];
  RuntimeInitArgs init_args;
  memset(&init_args, 0, sizeof(init_args));
  init_args.mem_alloc_type = Alloc_With_Pool;
  init_args.mem_alloc_option.pool.heap_buf = wamr_heap_buf;
  init_args.mem_alloc_option.pool.heap_size = sizeof(wamr_heap_buf);
  if (!wasm_runtime_full_init(&init_args)) {
    printf("ERROR: Failed to initialize WAMR runtime\n");
    return;
  }
  printf("WAMR runtime initialized (pool: %uKB, free_heap=%u)\n",
         (unsigned)(sizeof(wamr_heap_buf) / 1024),
         (unsigned)esp_get_free_heap_size());
  set_wasm_battery_gauge(g_gauge);
  register_wasm_native_apis();

  auto &scheduler = Scheduler::getInstance();

  // Add WiFi + MQTT as background resources for control-plane connectivity;
  // their allocations remain in the system heap rather than the linear-memory
  // slot reserved by the project-modified WAMR ESP-IDF platform layer.
  auto *wifi = new WIFI::Wifi("WIFI", {}, CONFIG_WIFI_SSID, CONFIG_WIFI_PASS);
  Scheduler::addResource(wifi);
  auto *mqtt = new MQTTClient("MQTT", {"WIFI"}, CONFIG_MQTT_TOKEN, CONFIG_MQTT_BROKER);
  Scheduler::addResource(mqtt);
  g_mqtt_resource = mqtt;

  FunctionFactory::registerFunction(FunctionType::wasmProcess,
                                    createWasmProcessLocal);
  scheduler.subscribe(EventType::wasmTimer, FunctionType::wasmProcess);
  Scheduler::addTrigger(new BasicWasmTrigger(15 * 1000000));
  Scheduler::startScheduler();

  // Start MQTT control channel in a background task — same pattern as edge mode.
  // In LOCAL mode, WiFi isn't woken by invocations, so we must wake it
  // explicitly before MQTT can connect.
  struct ctrl_ctx { WIFI::Wifi *wifi; MQTTClient *mqtt; const char *topic; };
  static ctrl_ctx ctx = { wifi, mqtt, DATA_TOPIC };
  xTaskCreate([](void *arg) {
    auto *c = static_cast<ctrl_ctx *>(arg);
    // Reserve the control-plane chain before idle-sleep can park MQTT/WIFI.
    c->mqtt->resourceStart();
    vTaskDelay(pdMS_TO_TICKS(1000));
    // Wake WiFi explicitly — LOCAL mode invocations don't need it.
    c->wifi->resourceWakeupAction();
    // Wait for WiFi to get an IP
    for (int i = 0; i < 20; i++) {
      if (c->wifi->getState() == Resource::res_state::idle)
        break;
      vTaskDelay(pdMS_TO_TICKS(500));
    }
    if (c->wifi->getState() != Resource::res_state::idle) {
      printf("[Mode] WiFi failed to connect — MQTT control unavailable\n");
      c->mqtt->resourceStop();
      vTaskDelete(nullptr);
      return;
    }
    vTaskDelay(pdMS_TO_TICKS(500));
    if (!c->mqtt->getMqttClient()) {
      printf("[Mode] MQTT client not yet up — control handler not registered\n");
      c->mqtt->resourceStop();
      vTaskDelete(nullptr);
      return;
    }
    c->mqtt->resourceWakeupAction();
    if (c->mqtt->getState() == Resource::res_state::idle ||
        c->mqtt->getState() == Resource::res_state::active) {
      c->mqtt->resourceStart();
      sif_control_register(c->mqtt->getMqttClient(), c->topic);
      sif_telemetry_publish_current(c->mqtt->getMqttClient(), g_gauge);
      printf("[Mode] MQTT control channel active: %s\n", c->topic);
      printf("[Mode] MQTT telemetry topic: %s\n", sif_telemetry_topic());
    } else {
      printf("[Mode] MQTT control channel unavailable\n");
    }
    vTaskDelete(nullptr);
  }, "ctrl_chan", 4096, &ctx, 5, nullptr);

}

static void run_edge_mode(const sif_state::State &st) {
  printf("[Mode] EDGE — battery=%u low=%u high=%u source=%s drain=%u recover=%u\n",
         st.battery, st.low_threshold, st.high_threshold,
         st.simulate_battery ? "simulated" : "real",
         st.local_drain, st.edge_recover);
  // No WAMR init in edge mode — heap goes to WiFi instead.

  auto &scheduler = Scheduler::getInstance();
  auto *wifi = new WIFI::Wifi("WIFI", {}, CONFIG_WIFI_SSID, CONFIG_WIFI_PASS);
  Scheduler::addResource(wifi);
  auto *mqtt = new MQTTClient("MQTT", {"WIFI"}, CONFIG_MQTT_TOKEN, CONFIG_MQTT_BROKER);
  Scheduler::addResource(mqtt);
  g_mqtt_resource = mqtt;


  FunctionFactory::registerFunction(FunctionType::wasmProcess,
                                    createHttpForward);
  scheduler.subscribe(EventType::wasmTimer, FunctionType::wasmProcess);
  Scheduler::addTrigger(new BasicWasmTrigger(15 * 1000000));

  printf("[Mode] HTTP offload target: %s\n", CONFIG_EDGE_HOST_URL);
  Scheduler::startScheduler();

  // Keep MQTT alive as a persistent control-plane side channel.
  // Runs in a background task so the blocking resourceWakeupAction()
  // (portMAX_DELAY) never stalls app_main — it just retries until
  // the broker becomes reachable.
  struct ctrl_ctx { WIFI::Wifi *wifi; MQTTClient *mqtt; const char *topic; };
  static ctrl_ctx ctx = { wifi, mqtt, DATA_TOPIC };
  xTaskCreate([](void *arg) {
    auto *c = static_cast<ctrl_ctx *>(arg);
    // Reserve the control-plane chain before idle-sleep can park MQTT/WIFI.
    c->mqtt->resourceStart();
    vTaskDelay(pdMS_TO_TICKS(2500));

    // EDGE mode still needs the MQTT side channel for control commands such as
    // reload/local return, so bring WiFi fully up before starting MQTT.
    auto wifi_state = c->wifi->getState();
    if (wifi_state != Resource::res_state::idle &&
        wifi_state != Resource::res_state::active) {
      c->wifi->resourceWakeupAction();
      for (int i = 0; i < 20; i++) {
        wifi_state = c->wifi->getState();
        if (wifi_state == Resource::res_state::idle ||
            wifi_state == Resource::res_state::active)
          break;
        vTaskDelay(pdMS_TO_TICKS(500));
      }
    }
    wifi_state = c->wifi->getState();
    if (wifi_state != Resource::res_state::idle &&
        wifi_state != Resource::res_state::active) {
      printf("[Mode] WiFi failed to connect — MQTT control unavailable\n");
      c->mqtt->resourceStop();
      vTaskDelete(nullptr);
      return;
    }
    vTaskDelay(pdMS_TO_TICKS(500));

    if (!c->mqtt->getMqttClient()) {
      printf("[Mode] MQTT client not yet up — control handler not registered\n");
      c->mqtt->resourceStop();
      vTaskDelete(nullptr);
      return;
    }
    c->mqtt->resourceWakeupAction();
    if (c->mqtt->getState() == Resource::res_state::idle ||
        c->mqtt->getState() == Resource::res_state::active) {
      c->mqtt->resourceStart();
      sif_control_register(c->mqtt->getMqttClient(), c->topic);
      sif_telemetry_publish_current(c->mqtt->getMqttClient(), g_gauge);
      printf("[Mode] MQTT control channel active: %s\n", c->topic);
      printf("[Mode] MQTT telemetry topic: %s\n", sif_telemetry_topic());
    } else {
      printf("[Mode] MQTT control channel unavailable\n");
    }
    vTaskDelete(nullptr);
  }, "ctrl_chan", 4096, &ctx, 5, nullptr);
}

void app_main(void) {
  esp_log_level_set("*", ESP_LOG_INFO);

  sif_init();
  if (sif_led_init() != ESP_OK) {
    ESP_LOGE("Main", "RGB LED initialization failed");
  }

  // Initialize the LC709203F battery gauge on the primary I2C bus.
  g_gauge = new BatteryGauge("BATTERY", {}, 0);
  g_gauge->resourceInitAction();
  g_gauge->resourceWakeupAction();
  printf("[Battery] LC709203F: %.3fV, %u%% SOC\n",
         g_gauge->getVoltage() / 1000.0f,
         (unsigned)g_gauge->getStateOfCharge());

  if (sif_spiffs_mount() != ESP_OK) {
    printf("ERROR: SPIFFS mount failed\n");
    return;
  }

  // Demo: start in local mode on first power-on and use the real battery
  // gauge as the source of truth. Software resets preserve the NVS mode.
  if (esp_reset_reason() == ESP_RST_POWERON) {
    sif_state::set_mode(sif_state::Mode::Local);
    sif_state::set_thresholds(20, 80);
    uint16_t soc_raw = g_gauge ? g_gauge->getStateOfCharge() : 100;
    uint8_t boot_soc = (soc_raw > 100) ? 100 : (uint8_t)soc_raw;
    sif_state::set_battery(boot_soc);
    sif_state::set_simulate_battery(false);
    sif_state::set_simulation_steps(0, 0);
  }

  sif_state::State st;
  sif_state::load(st);
  if (st.active_release.function_identity.empty()) {
    char digest[SIF_WASM_SHA256_HEX_SIZE] = {};
    if (compute_active_wasm_digest(digest)) {
      sif_state::ReleaseMetadata bootstrap;
      bootstrap.generation = 0;
      bootstrap.artifact_digest = digest;
      bootstrap.function_identity = "basic-edge-demo";
      bootstrap.resource_contract_json = "{\"inputs\":[],\"outputs\":[]}";
      sif_state::set_active_release(bootstrap);
      sif_state::load(st);
    }
  }
  sif_release_init(st);
  // Runtime-mode changes reboot the ESP32. Restore the active release's latest
  // actuator color so a low-battery offload does not turn a green/red status
  // light off while waiting for the first edge invocation.
  if (sif_release_output_declared("actuatorCommand", "i32")) {
    sif_led_restore_actuator(st.actuator_command);
  }

  if (st.mode == sif_state::Mode::Edge) {
    run_edge_mode(st);
  } else {
    run_local_mode(st);
  }
}
