#pragma once

#include "sif_function.hpp"
#include "sif_batteryGauge.hpp"
#include "sif_mqtt.hpp"
#include "esp_err.h"

// Set by main.cpp after MQTT resource creation.  The raw
// esp_mqtt_client_handle_t is only read at shutdown time via
// getMqttClient(), which is safe because MQTT has already connected
// by then.  Stays nullptr in local mode.
extern MQTTClient *g_mqtt_resource;

// Edge-mode SubscriberFunction that offloads via HTTP POST to the
// Go sif-edge-host running on K3s, instead of MQTT.
// Reads real battery SOC from gauge; when battery climbs back above
// the high-water threshold, persists a mode switch and reboots into
// local mode.
class HttpForwardFunction : public SubscriberFunction {
 public:
  HttpForwardFunction(FunctionType ftype, std::string name,
                      const char *edge_url, BatteryGauge *gauge = nullptr);

  esp_err_t run(EventMap e_map) override;
  uint64_t getDeadline(Event *event) override;

 private:
  const char *edge_url_;
  BatteryGauge *gauge_ = nullptr;
};
