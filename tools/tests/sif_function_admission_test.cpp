#include <cassert>

#include "sif_functionAdmission.hpp"

int main() {
  FunctionAdmissionGate gate;
  FunctionType function = FunctionType::wasmProcess;

  assert(gate.isEnabled(function));
  assert(gate.admit(function));
  assert(gate.admittedCount(function) == 1);

  gate.setEnabled(function, false);
  assert(!gate.isEnabled(function));
  assert(!gate.admit(function));
  assert(gate.admittedCount(function) == 1);

  // A failed execution retry is the same admitted Invocation, so admission
  // accounting remains unchanged until terminal completion.
  assert(gate.admittedCount(function) == 1);
  gate.complete(function);
  assert(gate.admittedCount(function) == 0);

  gate.setEnabled(function, true);
  assert(gate.admit(function));
  assert(gate.admittedCount(function) == 1);
  gate.complete(function);
  assert(gate.admittedCount(function) == 0);
  return 0;
}
