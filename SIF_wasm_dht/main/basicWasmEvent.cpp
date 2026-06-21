#include "basicWasmEvent.hpp"

Event *BasicWasmEvent::clone() {
  return new BasicWasmEvent(*this);
}

SerialBin BasicWasmEvent::serialize() {
  return SerialBin(0, nullptr);
}
