#pragma once
#include "sif_event.hpp"
#include "typeNumerations.hpp"
#include "sif_trigger.hpp"

class WasmDhtTrigger : public TimerTrigger {
 public:
  explicit WasmDhtTrigger(uint64_t interval);
  ~WasmDhtTrigger() override;
  void connect() override;
  void disconnect() override;
  static void IRAM_ATTR createWasmDhtEvent(void *arg);
};
