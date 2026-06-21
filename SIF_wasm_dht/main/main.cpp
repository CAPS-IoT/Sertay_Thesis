#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_system.h"
#include "wasm_export.h"
#include "bh_platform.h"
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
#include "typeNumerations.hpp"
#include "wasmDhtT.hpp"
#include "wasmDhtReadE.hpp"

// Embedded Wasm fallback (only used if SPIFFS is empty).
#include "dht_reader_wasm.h"

extern "C" void app_main();

char *eventTypeToString(EventType type) {
  static char *strEventType[] = {
      (char *)"wasmDhtRead",
      (char *)"ErrorEvent",
      (char *)"GetNextDutyFreqEvent",
      (char *)"AdjustDutyFreqEvent",
      (char *)"GetChargingModelEvent",
  };
  return strEventType[(int)type];
}

char *fncTypeToString(FunctionType type) {
  static char *strFncType[] = {
      (char *)"wasmDhtProcess",
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
  return sif_wasm_digest_blob(dht_reader_wasm, dht_reader_wasm_len, out_digest) == ESP_OK;
}

static SubscriberFunction *createWasmDhtProcess_local() {
  struct stat st;
  if (stat(LOCAL_WASM_PATH, &st) == 0 && st.st_size > 0) {
    printf("[Factory] Local Wasm from SPIFFS (%ld bytes)\n", (long)st.st_size);
    // SIF dispatches this subscriber locally; WAMR remains the only guest
    // execution boundary and the guest can reach hardware only via env imports.
    return new MigratingWasmFunction(
        FunctionType::wasmDhtProcess, "WasmDhtProcess", {}, false,
      std::string(LOCAL_WASM_PATH), DATA_TOPIC, LOCAL_THRESHOLD, g_gauge);
  }
  printf("[Factory] SPIFFS empty, embedded fallback\n");
  // Embedded module keeps the demo bootable when no blob has been pulled yet.
  return new MigratingWasmFunction(
      FunctionType::wasmDhtProcess, "WasmDhtProcess", {}, false,
      dht_reader_wasm, dht_reader_wasm_len, DATA_TOPIC, LOCAL_THRESHOLD,
      g_gauge);
}

static SubscriberFunction *createHttpForward() {
  // Edge-mode subscriber does not instantiate WAMR; it owns only the HTTP
  // offload step and the post-offload battery recovery check.
  return new HttpForwardFunction(FunctionType::wasmDhtProcess,
                                  "WasmDhtProcess", CONFIG_EDGE_HOST_URL, g_gauge);
}

static std::string edge_wasm_upload_url() {
  std::string url = CONFIG_EDGE_HOST_URL;
  const std::string process_suffix = "/process";
  if (url.size() >= process_suffix.size() &&
      url.compare(url.size() - process_suffix.size(), process_suffix.size(),
                  process_suffix) == 0) {
    url.replace(url.size() - process_suffix.size(), process_suffix.size(), "/wasm");
    return url;
  }
  if (!url.empty() && url.back() == '/') {
    url.pop_back();
  }
  return url + "/wasm";
}

// Bring up WiFi briefly, do an HTTP pull from `url` into /spiffs/dht_reader.wasm,
// then tear WiFi down so WAMR can have the heap.
static void boot_pull_if_needed(const std::string &override_url) {
  const char *url = override_url.empty() ? CONFIG_WASM_PULL_URL
                                         : override_url.c_str();
  if (url[0] == '\0') return;

  struct stat st;
  bool already = (stat(LOCAL_WASM_PATH, &st) == 0 && st.st_size > 0);

#ifdef CONFIG_WASM_PULL_ALWAYS
  bool always = true;
#else
  bool always = false;
#endif

  // Default boot only pulls when missing or explicitly forced. Explicit reloads
  // are now digest-aware so unchanged artifacts do not trigger a re-download.
  bool need = (!already) || always || !override_url.empty();
  if (!need) {
    printf("[Boot] Wasm in SPIFFS (%ld bytes), no pull\n", (long)st.st_size);
    return;
  }

  printf("[Boot] Pulling %s\n", url);
  printf("[Boot] free_heap before WiFi=%u\n", (unsigned)esp_get_free_heap_size());

  WIFI::Wifi wifi("WIFI_BOOT", {}, CONFIG_WIFI_SSID, CONFIG_WIFI_PASS);
  wifi.resourceInitAction();
  wifi.resourceWakeupAction();

  if (!override_url.empty()) {
    char remote_digest[SIF_WASM_SHA256_HEX_SIZE] = {};
    char local_digest[SIF_WASM_SHA256_HEX_SIZE] = {};
    bool have_local_digest = compute_active_wasm_digest(local_digest);
    esp_err_t digest_err = sif_wasm_fetch_digest(url, remote_digest);
    if (digest_err == ESP_OK && have_local_digest &&
        strcmp(local_digest, remote_digest) == 0) {
      printf("[Boot] Reload skipped — local wasm already at sha256=%s\n",
             local_digest);
      sif_state::clear_pull_url();
      wifi.resourceSleepAction();
      wifi.resourceDeinitAction();
      printf("[Boot] free_heap after WiFi teardown=%u\n",
             (unsigned)esp_get_free_heap_size());
      return;
    }
    if (digest_err != ESP_OK) {
      printf("[Boot] Reload digest probe failed: %s — pulling anyway\n",
             esp_err_to_name(digest_err));
    }
  }

  size_t got = 0;
  esp_err_t err = sif_wasm_pull_blob(url, LOCAL_WASM_PATH, &got);
  if (err == ESP_OK) {
    printf("[Boot] Pulled %u bytes\n", (unsigned)got);
    if (!override_url.empty()) sif_state::clear_pull_url();
  } else {
    printf("[Boot] Pull failed: %s\n", esp_err_to_name(err));
  }

  wifi.resourceSleepAction();
  wifi.resourceDeinitAction();
  printf("[Boot] free_heap after WiFi teardown=%u\n",
         (unsigned)esp_get_free_heap_size());

  // WiFi init/deinit fragments the system heap so badly that os_mmap
  // can no longer find a contiguous 64KB block for Wasm linear memory.
  // A reboot gives WAMR a pristine heap — the binary is already in
  // SPIFFS and pull_url is cleared, so WiFi won't be touched again.
  if (err == ESP_OK) {
    printf("[Boot] Binary migrated — rebooting for clean WAMR heap\n");
    vTaskDelay(pdMS_TO_TICKS(200));
    esp_restart();
  }
}

static void run_local_mode(const sif_state::State &st) {
  printf("[Mode] LOCAL — battery=%u low=%u high=%u source=%s drain=%u recover=%u\n",
         st.battery, st.low_threshold, st.high_threshold,
         st.simulate_battery ? "simulated" : "real",
         st.local_drain, st.edge_recover);

  boot_pull_if_needed(st.pull_url);

  // Use pool allocator — the system allocator's alignment wrapper has issues
  // with ESP-IDF's TLSF when called from pthreads (SIF worker threads).
  // Pool is for WAMR internal structures only; linear memory (64KB) still
  // comes from system heap via os_mmap.
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
  register_wasm_native_apis();

  auto &scheduler = Scheduler::getInstance();

  // Add WiFi + MQTT as background resources for control-plane connectivity.
  // WAMR linear memory now uses a pre-allocated BSS buffer (espidf_memmap.c),
  // so WiFi can safely use the system heap without fragmentation issues.
  auto *wifi = new WIFI::Wifi("WIFI", {}, CONFIG_WIFI_SSID, CONFIG_WIFI_PASS);
  Scheduler::addResource(wifi);
  auto *mqtt = new MQTTClient("MQTT", {"WIFI"}, CONFIG_MQTT_TOKEN, CONFIG_MQTT_BROKER);
  Scheduler::addResource(mqtt);
  g_mqtt_resource = mqtt;

  FunctionFactory::registerFunction(FunctionType::wasmDhtProcess,
                                    createWasmDhtProcess_local);
  scheduler.subscribe(EventType::wasmDhtRead, FunctionType::wasmDhtProcess);
  Scheduler::addTrigger(new WasmDhtTrigger(15 * 1000000));
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

  // Local artifact updates are operator-authoritative: the operator sends a
  // reload command with an explicit URL, and boot-time pull logic applies it.
  // A background digest probe against CONFIG_WASM_PULL_URL races with that
  // flow and can overwrite the local artifact with the host's deferred /wasm.
}

static void run_edge_mode(const sif_state::State &st) {
  printf("[Mode] EDGE — battery=%u low=%u high=%u source=%s drain=%u recover=%u\n",
         st.battery, st.low_threshold, st.high_threshold,
         st.simulate_battery ? "simulated" : "real",
         st.local_drain, st.edge_recover);
  // No WAMR init in edge mode — heap goes to WiFi instead.

  size_t pushed = 0;
  std::string upload_url = edge_wasm_upload_url();
  WIFI::Wifi boot_wifi("WIFI_EDGE_BOOT", {}, CONFIG_WIFI_SSID, CONFIG_WIFI_PASS);
  boot_wifi.resourceInitAction();
  boot_wifi.resourceWakeupAction();
  if (boot_wifi.getState() == Resource::res_state::idle) {
    char local_digest[SIF_WASM_SHA256_HEX_SIZE] = {};
    char remote_digest[SIF_WASM_SHA256_HEX_SIZE] = {};
    bool have_local_digest = compute_active_wasm_digest(local_digest);
    esp_err_t remote_err = sif_wasm_fetch_digest(upload_url.c_str(), remote_digest);
    if (remote_err == ESP_OK) {
      if (have_local_digest && strcmp(local_digest, remote_digest) == 0) {
        printf("[EdgeBoot] Edge host already has sha256=%s — skipping sync\n",
               local_digest);
      } else {
        if (have_local_digest) {
          printf("[EdgeBoot] Host sha256=%s differs from local sha256=%s — syncing host artifact to SPIFFS\n",
                 remote_digest, local_digest);
        } else {
          printf("[EdgeBoot] Local digest unavailable; syncing host sha256=%s to SPIFFS\n",
                 remote_digest);
        }

        size_t pulled = 0;
        esp_err_t pull_err = sif_wasm_pull_blob(upload_url.c_str(), LOCAL_WASM_PATH, &pulled);
        if (pull_err == ESP_OK) {
          printf("[EdgeBoot] Synced %u bytes from edge host\n", (unsigned)pulled);
        } else {
          printf("[EdgeBoot] Host sync failed: %s\n", esp_err_to_name(pull_err));
        }
      }
    } else {
      printf("[EdgeBoot] Digest probe failed: %s — falling back to local upload\n",
             esp_err_to_name(remote_err));

      esp_err_t push_err = ESP_FAIL;
      struct stat stbuf;
      if (stat(LOCAL_WASM_PATH, &stbuf) == 0 && stbuf.st_size > 0) {
        printf("[EdgeBoot] Uploading SPIFFS wasm to %s\n", upload_url.c_str());
        push_err = sif_wasm_push_file(upload_url.c_str(), LOCAL_WASM_PATH, &pushed);
      } else {
        printf("[EdgeBoot] Uploading embedded wasm to %s\n", upload_url.c_str());
        push_err = sif_wasm_push_blob(upload_url.c_str(), dht_reader_wasm,
                                      dht_reader_wasm_len);
        if (push_err == ESP_OK) {
          pushed = (size_t)dht_reader_wasm_len;
        }
      }
      if (push_err == ESP_OK) {
        printf("[EdgeBoot] Uploaded %u bytes to edge host\n", (unsigned)pushed);
      } else {
        printf("[EdgeBoot] Upload failed: %s\n", esp_err_to_name(push_err));
      }
    }
  } else {
    printf("[EdgeBoot] WiFi failed to connect — skipping wasm upload\n");
  }
  boot_wifi.resourceSleepAction();
  boot_wifi.resourceDeinitAction();

  auto &scheduler = Scheduler::getInstance();
  auto *wifi = new WIFI::Wifi("WIFI", {}, CONFIG_WIFI_SSID, CONFIG_WIFI_PASS);
  Scheduler::addResource(wifi);
  auto *mqtt = new MQTTClient("MQTT", {"WIFI"}, CONFIG_MQTT_TOKEN, CONFIG_MQTT_BROKER);
  Scheduler::addResource(mqtt);
  g_mqtt_resource = mqtt;


  FunctionFactory::registerFunction(FunctionType::wasmDhtProcess,
                                    createHttpForward);
  scheduler.subscribe(EventType::wasmDhtRead, FunctionType::wasmDhtProcess);
  Scheduler::addTrigger(new WasmDhtTrigger(15 * 1000000));

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

  if (st.mode == sif_state::Mode::Edge) {
    run_edge_mode(st);
  } else {
    run_local_mode(st);
  }
}
