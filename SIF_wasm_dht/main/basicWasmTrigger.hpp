#pragma once

#include "sif_event.hpp"
#include "sif_trigger.hpp"
#include "typeNumerations.hpp"

class BasicWasmTrigger : public TimerTrigger {
 public:
  explicit BasicWasmTrigger(uint64_t interval);
  ~BasicWasmTrigger() override;
  void connect() override;
  void disconnect() override;
  static void IRAM_ATTR createEvent(void *arg);
};
