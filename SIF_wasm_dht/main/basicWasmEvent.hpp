#pragma once

#include "sif_event.hpp"
#include "typeNumerations.hpp"

class BasicWasmEvent : public Event {
 public:
  explicit BasicWasmEvent() : Event(EventType::wasmTimer) {}
  Event *clone() override;
  SerialBin serialize() override;
  void deserialize(std::string serial) override {}
};
