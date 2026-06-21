#pragma once

#include <stdint.h>
#include <string>
#include "esp_err.h"

// Persistent runtime state for the migration demo.
// All values are stored in the "sif_state" NVS namespace and survive reboots.
namespace sif_state {

enum class Mode { Local, Edge };

struct ReleaseMetadata {
  uint64_t generation = 0;
  std::string artifact_digest;
  std::string function_identity;
  std::string resource_contract_json;

  bool valid() const {
    return generation > 0 && artifact_digest.size() == 64 &&
           !function_identity.empty() && !resource_contract_json.empty();
  }
};

struct State {
  Mode mode = Mode::Local;
  uint8_t battery = 100;        // simulated or observed SoC, 0..100
  uint8_t low_threshold = 20;   // local -> edge when battery <= this
  uint8_t high_threshold = 80;  // edge -> local when battery >= this
  uint8_t local_drain = 25;     // battery delta per local invocation
  uint8_t edge_recover = 25;    // battery delta per edge invocation
  uint8_t actuator_command = 0; // last accepted actuator output, 0..2
  bool simulate_battery = false; // when true, ignore the real gauge
  ReleaseMetadata active_release;
  ReleaseMetadata staged_release;
  bool admission_paused = false;
  std::string last_command_id;
  std::string last_deadline_decision_id;
};

// Load state from NVS, applying defaults for missing keys.
esp_err_t load(State &out);

// Load the fields needed by telemetry and placement without allocating the
// potentially large resource contracts or diagnostic command identifiers.
esp_err_t load_summary(State &out);

// Persist individual fields (each opens/commits its own handle for safety).
esp_err_t set_mode(Mode m);
esp_err_t set_battery(uint8_t soc);
esp_err_t set_actuator_command(uint8_t command);
esp_err_t set_thresholds(uint8_t low, uint8_t high);
esp_err_t set_simulate_battery(bool enabled);
esp_err_t set_simulation_steps(uint8_t local_drain, uint8_t edge_recover);
esp_err_t set_active_release(const ReleaseMetadata &release);
esp_err_t set_staged_release(const ReleaseMetadata &release);
esp_err_t activate_staged_release(const ReleaseMetadata &release);
esp_err_t clear_staged_release();
esp_err_t set_admission_paused(bool paused);
esp_err_t set_last_command_id(const std::string &command_id);
esp_err_t set_last_deadline_decision_id(const std::string &decision_id);

// Helpers
const char *mode_to_string(Mode m);
Mode mode_from_string(const char *s);

}  // namespace sif_state
