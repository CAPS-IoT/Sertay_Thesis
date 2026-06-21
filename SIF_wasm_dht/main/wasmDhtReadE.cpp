#include "wasmDhtReadE.hpp"

Event *WasmDhtReadEvent::clone() {
  return new WasmDhtReadEvent(*this);
}

SerialBin WasmDhtReadEvent::serialize() {
  return SerialBin(0, nullptr);
}
