#pragma once

enum class EventType {
  wasmDhtRead,
  ErrorEvent,
  GetNextDutyFreqEvent,
  AdjustDutyFreqEvent,
  GetChargingModelEvent
};

char *eventTypeToString(EventType type);

enum class FunctionType {
  wasmDhtProcess,
  HandleErrorFunction,
  CalcDutyFreqFunction,
  CalcChargingModelFunction,
  setSleepTimeFunction
};

char *fncTypeToString(FunctionType type);
