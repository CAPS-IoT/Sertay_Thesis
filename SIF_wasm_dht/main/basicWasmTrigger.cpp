#include "basicWasmTrigger.hpp"

#include "basicWasmEvent.hpp"
#include "sif_scheduler.hpp"

void BasicWasmTrigger::disconnect() {
  TimerTrigger::disconnect();
}

void BasicWasmTrigger::connect() {
  TimerTrigger::connect();
}

BasicWasmTrigger::~BasicWasmTrigger() {
  TimerTrigger::disconnect();
}

void BasicWasmTrigger::createEvent(void *arg) {
  auto *trigger = static_cast<BasicWasmTrigger *>(arg);
  trigger->setLastTriggered(esp_rtc_get_time_us());
  Scheduler::getInstance().enqueue(new BasicWasmEvent());
}

BasicWasmTrigger::BasicWasmTrigger(uint64_t interval)
    : TimerTrigger("BasicWasmTimer",
                   {},
                   BasicWasmTrigger::createEvent,
                   interval,
                   ESPTimer::mode_t::periodic,
                   true,
                   0,
                   8.64e+10,
                   true,
                   this) {}
