#pragma once
#include "sif_event.hpp"
#include "typeNumerations.hpp"

class WasmDhtReadEvent : public Event {
 public:
  explicit WasmDhtReadEvent() : Event(EventType::wasmDhtRead) {}
  Event *clone() override;
  SerialBin serialize() override;
  void deserialize(std::string serial) override {}
};
