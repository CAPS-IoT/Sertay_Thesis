#pragma once

#include "sif_batteryGauge.hpp"
#include "wasm_export.h"

/**
 * @brief Register native host API functions that Wasm guest modules can call.
 *        Must be called after wasm_runtime_init() and before loading any module.
 */
void register_wasm_native_apis();

/**
 * @brief Provide optional real hardware resources to WAMR host imports.
 */
void set_wasm_battery_gauge(BatteryGauge *gauge);
