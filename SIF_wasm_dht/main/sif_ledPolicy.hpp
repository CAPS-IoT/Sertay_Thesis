#pragma once

#include <stdint.h>
#include <string>

struct SifLedOutput {
  bool green;
  bool red;
  bool blue;
};

class SifLedPolicy {
 public:
  void setPersistedDecision(const std::string &decision_id) {
    last_decision_id_ = decision_id;
  }

  bool updateActuator(int32_t value) {
    const int32_t next = value == 1 || value == 2 ? value : 0;
    const bool changed = steady_actuator_ != next;
    steady_actuator_ = next;
    return changed;
  }

  bool activateRelease(const std::string &function_identity,
                       bool actuator_output_declared) {
    const bool overlay_cancelled =
        overlay_running_ && overlay_function_identity_ != function_identity;
    if (overlay_cancelled) clearOverlay();
    if (!actuator_output_declared) steady_actuator_ = 0;
    return overlay_cancelled;
  }

  bool beginOverlay(const std::string &decision_id,
                    const std::string &function_identity) {
    if (decision_id.empty() || decision_id == last_decision_id_ || overlay_running_) {
      return false;
    }
    last_decision_id_ = decision_id;
    overlay_function_identity_ = function_identity;
    overlay_running_ = true;
    overlay_blue_on_ = false;
    return true;
  }

  bool setOverlayBlue(const std::string &decision_id, bool blue_on) {
    if (!overlay_running_ || last_decision_id_ != decision_id) return false;
    overlay_blue_on_ = blue_on;
    return true;
  }

  bool finishOverlay(const std::string &decision_id) {
    if (!overlay_running_ || last_decision_id_ != decision_id) return false;
    clearOverlay();
    return true;
  }

  SifLedOutput output() const {
    if (overlay_running_) {
      return {false, false, overlay_blue_on_};
    }
    return {steady_actuator_ == 1, steady_actuator_ == 2, false};
  }

  int32_t steadyActuator() const { return steady_actuator_; }
  bool overlayRunning() const { return overlay_running_; }
  const std::string &lastDecisionId() const { return last_decision_id_; }

 private:
  void clearOverlay() {
    overlay_running_ = false;
    overlay_blue_on_ = false;
    overlay_function_identity_.clear();
  }

  int32_t steady_actuator_ = 0;
  bool overlay_running_ = false;
  bool overlay_blue_on_ = false;
  std::string overlay_function_identity_;
  std::string last_decision_id_;
};
