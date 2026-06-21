#pragma once

#include <stdint.h>

#include "esp_err.h"
#include "mqtt_client.h"

#include "sif_batteryGauge.hpp"
#include "sif_state.hpp"

typedef struct {
  const char *function_name;
  int32_t execution_ms;
  int32_t deadline_target_ms;
  int32_t deadline_slack_ms;
  bool deadline_missed;
  bool has_timing;
  int32_t queue_delay_ms;
  int32_t resource_wake_ms;
  int32_t resource_collection_ms;
  int32_t network_round_trip_ms;
  int32_t edge_execution_ms;
  int32_t output_application_ms;
  float dht_temperature;
  float dht_humidity;
  bool has_dht;
  float battery_percent;
  float battery_voltage_mv;
  bool has_battery_resource;
  float light_lux;
  bool has_light;
  float occupancy_distance_cm;
  bool has_occupancy;
  bool gpio_button_pressed;
  bool has_gpio;
} sif_invocation_metrics_t;

const char *sif_telemetry_topic();

esp_err_t sif_telemetry_publish(esp_mqtt_client_handle_t client,
                                sif_state::Mode mode,
                                uint8_t battery_percent,
                                bool simulated,
                                int voltage_mv = 0,
                                const sif_invocation_metrics_t *metrics = nullptr);

esp_err_t sif_telemetry_publish_current(esp_mqtt_client_handle_t client,
                                        BatteryGauge *gauge);
