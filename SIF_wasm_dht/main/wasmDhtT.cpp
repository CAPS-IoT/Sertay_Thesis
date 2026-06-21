#include "wasmDhtT.hpp"
#include "wasmDhtReadE.hpp"
#include "sif_scheduler.hpp"

void WasmDhtTrigger::disconnect() {
  TimerTrigger::disconnect();
}

void WasmDhtTrigger::connect() {
  TimerTrigger::connect();
}

WasmDhtTrigger::~WasmDhtTrigger() {
  TimerTrigger::disconnect();
}

void WasmDhtTrigger::createWasmDhtEvent(void *arg) {
  auto *trigger = (WasmDhtTrigger *)arg;
  trigger->setLastTriggered(esp_rtc_get_time_us());
  Scheduler::getInstance().enqueue(new WasmDhtReadEvent());
}

WasmDhtTrigger::WasmDhtTrigger(uint64_t interval)
    : TimerTrigger("WasmDHT",
                   {},
                   WasmDhtTrigger::createWasmDhtEvent,
                   interval,
                   ESPTimer::mode_t::periodic,
                   true,
                   0,
                   8.64e+10,
                   true,
                   this) {}
