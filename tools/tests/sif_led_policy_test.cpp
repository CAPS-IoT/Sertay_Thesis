#include <cassert>

#include "sif_ledPolicy.hpp"

int main() {
  SifLedPolicy policy;
  assert(policy.activateRelease("hybrid-resource-demo", true) == false);

  auto output = policy.output();
  assert(!output.green && !output.red && !output.blue);

  assert(policy.updateActuator(1));
  assert(!policy.updateActuator(1));
  assert(policy.steadyActuator() == 1);
  output = policy.output();
  assert(output.green && !output.red && !output.blue);

  assert(policy.updateActuator(2));
  output = policy.output();
  assert(!output.green && output.red && !output.blue);

  policy.updateActuator(0);
  output = policy.output();
  assert(!output.green && !output.red && !output.blue);

  policy.updateActuator(1);

  assert(policy.beginOverlay("decision-1", "hybrid-resource-demo"));
  assert(policy.overlayRunning());
  assert(!policy.beginOverlay("decision-1", "hybrid-resource-demo"));
  output = policy.output();
  assert(!output.green && !output.red && !output.blue);

  assert(policy.setOverlayBlue("decision-1", true));
  output = policy.output();
  assert(!output.green && !output.red && output.blue);

  policy.updateActuator(2);
  assert(policy.steadyActuator() == 2);
  output = policy.output();
  assert(!output.green && !output.red && output.blue);

  assert(policy.finishOverlay("decision-1"));
  assert(!policy.overlayRunning());
  assert(policy.steadyActuator() == 2);
  output = policy.output();
  assert(!output.green && output.red && !output.blue);
  assert(!policy.beginOverlay("decision-1", "hybrid-resource-demo"));

  assert(policy.beginOverlay("decision-2", "hybrid-resource-demo"));
  assert(policy.setOverlayBlue("decision-2", true));
  policy.updateActuator(1);
  assert(policy.activateRelease("dht-reader", false));
  assert(!policy.overlayRunning());
  assert(policy.steadyActuator() == 0);
  output = policy.output();
  assert(!output.green && !output.red && !output.blue);
  assert(!policy.setOverlayBlue("decision-2", true));
  assert(!policy.finishOverlay("decision-2"));

  policy.updateActuator(1);
  assert(!policy.activateRelease("dht-reader", false));
  assert(policy.steadyActuator() == 0);

  SifLedPolicy restored;
  restored.setPersistedDecision("decision-2");
  assert(!restored.beginOverlay("decision-2", "dht-reader"));
  return 0;
}
