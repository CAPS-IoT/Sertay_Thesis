#pragma once

#include <stdint.h>

#include "esp_err.h"
#include "mqtt_client.h"

#include "sif_batteryGauge.hpp"
#include "sif_state.hpp"

const char *sif_telemetry_topic();

esp_err_t sif_telemetry_publish(esp_mqtt_client_handle_t client,
                                sif_state::Mode mode,
                                uint8_t battery_percent,
                                bool simulated,
                                int voltage_mv = 0);

esp_err_t sif_telemetry_publish_current(esp_mqtt_client_handle_t client,
                                        BatteryGauge *gauge);