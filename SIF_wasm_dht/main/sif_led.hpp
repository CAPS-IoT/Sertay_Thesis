#pragma once

#include <stdint.h>
#include <string>

#include "esp_err.h"

esp_err_t sif_led_init();
void sif_led_apply_actuator(int32_t value);
void sif_led_restore_actuator(int32_t value);
void sif_led_on_release_activated(const std::string &function_identity,
                                  bool actuator_output_declared);
esp_err_t sif_led_signal_deadline_rejection(const std::string &decision_id,
                                            const std::string &function_identity);
