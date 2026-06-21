#include "sif_led.hpp"
#include "sif_ledPolicy.hpp"

#include <pthread.h>

#include "driver/gpio.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "sdkconfig.h"
#include "sif_state.hpp"

#ifndef CONFIG_ACTUATOR_LED_GREEN_GPIO
#define CONFIG_ACTUATOR_LED_GREEN_GPIO 17
#endif

#ifndef CONFIG_ACTUATOR_LED_RED_GPIO
#define CONFIG_ACTUATOR_LED_RED_GPIO 16
#endif

#ifndef CONFIG_DEADLINE_LED_BLUE_GPIO
#define CONFIG_DEADLINE_LED_BLUE_GPIO 18
#endif

#ifndef CONFIG_ACTUATOR_LED_ACTIVE_LOW
#define CONFIG_ACTUATOR_LED_ACTIVE_LOW 1
#endif

#ifndef CONFIG_LED_BOOT_DIAGNOSTIC
#define CONFIG_LED_BOOT_DIAGNOSTIC 0
#endif

static const char *TAG = "SifLed";
static pthread_mutex_t s_led_mutex = PTHREAD_MUTEX_INITIALIZER;
static bool s_gpio_ready = false;
static SifLedPolicy s_policy;

struct rejection_task_args {
  std::string decision_id;
  std::string function_identity;
};

static int off_level() { return CONFIG_ACTUATOR_LED_ACTIVE_LOW ? 1 : 0; }
static int on_level() { return CONFIG_ACTUATOR_LED_ACTIVE_LOW ? 0 : 1; }

static esp_err_t configure_pin(int pin) {
  if (pin < 0) return ESP_OK;
  gpio_num_t gpio = static_cast<gpio_num_t>(pin);
  esp_err_t err = gpio_reset_pin(gpio);
  // Preload the output latch before enabling the driver. This avoids a brief
  // visible flash while the three channels are configured one by one.
  if (err == ESP_OK) err = gpio_set_level(gpio, off_level());
  if (err == ESP_OK) err = gpio_set_direction(gpio, GPIO_MODE_OUTPUT);
  if (err == ESP_OK) err = gpio_set_level(gpio, off_level());
  return err;
}

static bool mapping_is_valid() {
  const int green = CONFIG_ACTUATOR_LED_GREEN_GPIO;
  const int red = CONFIG_ACTUATOR_LED_RED_GPIO;
  const int blue = CONFIG_DEADLINE_LED_BLUE_GPIO;
  return (green < 0 || red < 0 || green != red) &&
         (green < 0 || blue < 0 || green != blue) &&
         (red < 0 || blue < 0 || red != blue);
}

static bool ensure_gpio_locked() {
  if (s_gpio_ready) return true;
  if (!mapping_is_valid()) {
    ESP_LOGE(TAG,
             "LED GPIO mapping contains duplicate pins green=%d red=%d blue=%d",
             CONFIG_ACTUATOR_LED_GREEN_GPIO, CONFIG_ACTUATOR_LED_RED_GPIO,
             CONFIG_DEADLINE_LED_BLUE_GPIO);
    return false;
  }
  esp_err_t green = configure_pin(CONFIG_ACTUATOR_LED_GREEN_GPIO);
  esp_err_t red = configure_pin(CONFIG_ACTUATOR_LED_RED_GPIO);
  esp_err_t blue = configure_pin(CONFIG_DEADLINE_LED_BLUE_GPIO);
  if (green != ESP_OK || red != ESP_OK || blue != ESP_OK) {
    ESP_LOGE(TAG, "LED init failed green=%s red=%s blue=%s",
             esp_err_to_name(green), esp_err_to_name(red), esp_err_to_name(blue));
    return false;
  }
  s_gpio_ready = true;
  ESP_LOGI(TAG, "LED mapping greenGPIO=%d redGPIO=%d blueGPIO=%d activeLow=%s",
           CONFIG_ACTUATOR_LED_GREEN_GPIO, CONFIG_ACTUATOR_LED_RED_GPIO,
           CONFIG_DEADLINE_LED_BLUE_GPIO,
           CONFIG_ACTUATOR_LED_ACTIVE_LOW ? "true" : "false");
  return true;
}

static esp_err_t write_pin_locked(int pin, bool enabled) {
  if (pin < 0) return ESP_OK;
  return gpio_set_level(static_cast<gpio_num_t>(pin),
                        enabled ? on_level() : off_level());
}

static esp_err_t write_channels_locked(bool green, bool red, bool blue) {
  if (!ensure_gpio_locked()) return ESP_FAIL;
  esp_err_t green_err = write_pin_locked(CONFIG_ACTUATOR_LED_GREEN_GPIO, green);
  esp_err_t red_err = write_pin_locked(CONFIG_ACTUATOR_LED_RED_GPIO, red);
  esp_err_t blue_err = write_pin_locked(CONFIG_DEADLINE_LED_BLUE_GPIO, blue);
  if (green_err != ESP_OK || red_err != ESP_OK || blue_err != ESP_OK) {
    ESP_LOGE(TAG, "LED write failed green=%s red=%s blue=%s",
             esp_err_to_name(green_err), esp_err_to_name(red_err),
             esp_err_to_name(blue_err));
    return ESP_FAIL;
  }
  return ESP_OK;
}

static esp_err_t write_policy_locked() {
  const SifLedOutput output = s_policy.output();
  return write_channels_locked(output.green, output.red, output.blue);
}

static void log_policy_locked(const char *event) {
  const SifLedOutput output = s_policy.output();
  ESP_LOGI(TAG,
           "%s command=%ld overlay=%s greenGPIO=%d greenLevel=%d "
           "redGPIO=%d redLevel=%d blueGPIO=%d blueLevel=%d",
           event, static_cast<long>(s_policy.steadyActuator()),
           s_policy.overlayRunning() ? "true" : "false",
           CONFIG_ACTUATOR_LED_GREEN_GPIO,
           output.green ? on_level() : off_level(),
           CONFIG_ACTUATOR_LED_RED_GPIO,
           output.red ? on_level() : off_level(),
           CONFIG_DEADLINE_LED_BLUE_GPIO,
           output.blue ? on_level() : off_level());
}

static void diagnostic_phase_locked(const char *channel, int pin,
                                    bool green, bool red, bool blue) {
  if (pin < 0) return;
  ESP_LOGI(TAG,
           "LED boot diagnostic channel=%s gpio=%d level=%d durationMs=500",
           channel, pin, on_level());
  write_channels_locked(green, red, blue);
  vTaskDelay(pdMS_TO_TICKS(500));
  write_channels_locked(false, false, false);
  vTaskDelay(pdMS_TO_TICKS(150));
}

esp_err_t sif_led_init() {
  pthread_mutex_lock(&s_led_mutex);
  if (!ensure_gpio_locked()) {
    pthread_mutex_unlock(&s_led_mutex);
    return ESP_FAIL;
  }
  esp_err_t err = write_channels_locked(false, false, false);
#if CONFIG_LED_BOOT_DIAGNOSTIC
  if (err == ESP_OK) {
    ESP_LOGI(TAG,
             "LED boot diagnostic start; each labeled GPIO is driven alone");
    diagnostic_phase_locked("green", CONFIG_ACTUATOR_LED_GREEN_GPIO,
                            true, false, false);
    diagnostic_phase_locked("red", CONFIG_ACTUATOR_LED_RED_GPIO,
                            false, true, false);
    diagnostic_phase_locked("blue", CONFIG_DEADLINE_LED_BLUE_GPIO,
                            false, false, true);
    err = write_channels_locked(false, false, false);
    ESP_LOGI(TAG, "LED boot diagnostic complete; all channels off");
  }
#endif
  pthread_mutex_unlock(&s_led_mutex);
  return err;
}

void sif_led_apply_actuator(int32_t value) {
  pthread_mutex_lock(&s_led_mutex);
  const bool changed = s_policy.updateActuator(value);
  write_policy_locked();
  log_policy_locked("ACTUATOR");
  esp_err_t persist_err = ESP_OK;
  if (changed) {
    persist_err = sif_state::set_actuator_command(
        static_cast<uint8_t>(value == 1 || value == 2 ? value : 0));
  }
  pthread_mutex_unlock(&s_led_mutex);
  if (persist_err != ESP_OK) {
    ESP_LOGW(TAG, "Failed to persist actuator command: %s",
             esp_err_to_name(persist_err));
  }
}

void sif_led_restore_actuator(int32_t value) {
  pthread_mutex_lock(&s_led_mutex);
  s_policy.updateActuator(value);
  write_policy_locked();
  log_policy_locked("ACTUATOR_RESTORE");
  pthread_mutex_unlock(&s_led_mutex);
}

void sif_led_on_release_activated(const std::string &function_identity,
                                  bool actuator_output_declared) {
  pthread_mutex_lock(&s_led_mutex);
  const bool overlay_cancelled =
      s_policy.activateRelease(function_identity, actuator_output_declared);
  write_policy_locked();
  ESP_LOGI(TAG,
           "Release LED ownership function=%s actuatorOutput=%s "
           "priorOverlayCancelled=%s",
           function_identity.c_str(), actuator_output_declared ? "true" : "false",
           overlay_cancelled ? "true" : "false");
  log_policy_locked("RELEASE");
  esp_err_t persist_err = ESP_OK;
  if (!actuator_output_declared) {
    persist_err = sif_state::set_actuator_command(0);
  }
  pthread_mutex_unlock(&s_led_mutex);
  if (persist_err != ESP_OK) {
    ESP_LOGW(TAG, "Failed to clear persisted actuator command: %s",
             esp_err_to_name(persist_err));
  }
}

static bool set_overlay_blue_locked(const std::string &decision_id,
                                    bool enabled) {
  if (!s_policy.setOverlayBlue(decision_id, enabled)) return false;
  write_policy_locked();
  log_policy_locked(enabled ? "DEADLINE_BLUE_ON" : "DEADLINE_BLUE_OFF");
  return true;
}

static void rejection_task(void *parameter) {
  auto *args = static_cast<rejection_task_args *>(parameter);
  bool still_owned = true;
  for (int cycle = 0; cycle < 2 && still_owned; ++cycle) {
    pthread_mutex_lock(&s_led_mutex);
    still_owned = set_overlay_blue_locked(args->decision_id, true);
    pthread_mutex_unlock(&s_led_mutex);
    if (!still_owned) break;
    vTaskDelay(pdMS_TO_TICKS(250));

    pthread_mutex_lock(&s_led_mutex);
    still_owned = set_overlay_blue_locked(args->decision_id, false);
    pthread_mutex_unlock(&s_led_mutex);
    if (!still_owned) break;
    vTaskDelay(pdMS_TO_TICKS(250));
  }

  pthread_mutex_lock(&s_led_mutex);
  if (s_policy.finishOverlay(args->decision_id)) {
    write_policy_locked();
    ESP_LOGI(TAG,
             "Deadline rejection indication complete decisionId=%s function=%s",
             args->decision_id.c_str(), args->function_identity.c_str());
    log_policy_locked("DEADLINE_RESTORE");
  } else {
    ESP_LOGI(TAG,
             "Deadline rejection indication cancelled decisionId=%s function=%s",
             args->decision_id.c_str(), args->function_identity.c_str());
  }
  pthread_mutex_unlock(&s_led_mutex);
  delete args;
  vTaskDelete(nullptr);
}

esp_err_t sif_led_signal_deadline_rejection(
    const std::string &decision_id, const std::string &function_identity) {
  if (decision_id.empty() || function_identity.empty()) {
    return ESP_ERR_INVALID_ARG;
  }
  pthread_mutex_lock(&s_led_mutex);
  if (s_policy.lastDecisionId().empty()) {
    sif_state::State state;
    if (sif_state::load(state) == ESP_OK) {
      s_policy.setPersistedDecision(state.last_deadline_decision_id);
    }
  }
  if (!s_policy.beginOverlay(decision_id, function_identity)) {
    pthread_mutex_unlock(&s_led_mutex);
    return ESP_OK;
  }
  // The overlay owns all three channels. Its initial off phase immediately
  // hides the steady actuator before the first blue pulse.
  write_policy_locked();
  sif_state::set_last_deadline_decision_id(decision_id);
  pthread_mutex_unlock(&s_led_mutex);

  auto *args = new rejection_task_args{decision_id, function_identity};
  if (xTaskCreate(rejection_task, "deadline_led", 3072, args, 4, nullptr) !=
      pdPASS) {
    delete args;
    pthread_mutex_lock(&s_led_mutex);
    s_policy.finishOverlay(decision_id);
    write_policy_locked();
    pthread_mutex_unlock(&s_led_mutex);
    return ESP_ERR_NO_MEM;
  }
  return ESP_OK;
}
