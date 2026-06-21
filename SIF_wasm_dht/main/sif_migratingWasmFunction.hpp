#pragma once

#include "sif_wasmFunction.hpp"
#include "sif_batteryGauge.hpp"
#include "sif_mqtt.hpp"
#include "sif_dht.hpp"
#include "cJSON.h"

/**
 * @brief A WasmFunction that executes locally and reports battery telemetry.
 *
 * Before each invocation it reads the battery state-of-charge, persists the
 * latest value, and publishes telemetry so the operator can choose whether the
 * next placement should remain local or move to the edge.
 */
class MigratingWasmFunction : public WasmFunction {
 public:
  // Embedded-buffer variant.
  MigratingWasmFunction(FunctionType ftype, std::string name,
                        std::list<std::string> neededResources,
                        bool needAllEvents,
                        const uint8_t *wasm_bytecode, uint32_t wasm_size,
                        const char *mqtt_offload_topic,
                        uint16_t battery_threshold = 20,
                        BatteryGauge *gauge = nullptr)
      : WasmFunction(ftype, name, neededResources, needAllEvents,
                      wasm_bytecode, wasm_size),
        mqtt_offload_topic(mqtt_offload_topic),
        battery_threshold(battery_threshold),
        gauge_(gauge) {};

  // SPIFFS-path variant — module is loaded from disk on first run().
  MigratingWasmFunction(FunctionType ftype, std::string name,
                        std::list<std::string> neededResources,
                        bool needAllEvents,
                        const std::string &wasm_path,
                        const char *mqtt_offload_topic,
                        uint16_t battery_threshold = 20,
                        BatteryGauge *gauge = nullptr)
      : WasmFunction(ftype, name, neededResources, needAllEvents,
                      wasm_path),
        mqtt_offload_topic(mqtt_offload_topic),
        battery_threshold(battery_threshold),
        gauge_(gauge) {};

  esp_err_t run(EventMap e_map) override;

 private:
  const char *mqtt_offload_topic;
  uint16_t battery_threshold;
      BatteryGauge *gauge_ = nullptr;

  esp_err_t offloadToEdge(EventMap &e_map);
};
