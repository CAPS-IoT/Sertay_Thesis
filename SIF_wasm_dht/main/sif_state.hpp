#pragma once

#include <stdint.h>
#include <string>
#include "esp_err.h"

// Persistent runtime state for the migration demo.
// All values are stored in the "sif_state" NVS namespace and survive reboots.
namespace sif_state {

enum class Mode { Local, Edge };

struct State {
  Mode mode = Mode::Local;
  uint8_t battery = 100;        // simulated or observed SoC, 0..100
  uint8_t low_threshold = 20;   // local -> edge when battery <= this
  uint8_t high_threshold = 80;  // edge -> local when battery >= this
  uint8_t local_drain = 25;     // battery delta per local invocation
  uint8_t edge_recover = 25;    // battery delta per edge invocation
  bool simulate_battery = false; // when true, ignore the real gauge
  std::string pull_url;         // optional override of CONFIG_WASM_PULL_URL
};

// Load state from NVS, applying defaults for missing keys.
esp_err_t load(State &out);

// Persist individual fields (each opens/commits its own handle for safety).
esp_err_t set_mode(Mode m);
esp_err_t set_battery(uint8_t soc);
esp_err_t set_thresholds(uint8_t low, uint8_t high);
esp_err_t set_simulate_battery(bool enabled);
esp_err_t set_simulation_steps(uint8_t local_drain, uint8_t edge_recover);
esp_err_t set_pull_url(const std::string &url);
esp_err_t clear_pull_url();

// Helpers
const char *mode_to_string(Mode m);
Mode mode_from_string(const char *s);

}  // namespace sif_state
