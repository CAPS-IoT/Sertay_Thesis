#pragma once

enum class EventType {
  wasmTimer,
  ErrorEvent,
  GetNextDutyFreqEvent,
  AdjustDutyFreqEvent,
  GetChargingModelEvent
};

char *eventTypeToString(EventType type);

enum class FunctionType {
  wasmProcess,
  HandleErrorFunction,
  CalcDutyFreqFunction,
  CalcChargingModelFunction,
  setSleepTimeFunction
};

char *fncTypeToString(FunctionType type);
