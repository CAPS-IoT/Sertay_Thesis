#include <cassert>
#include <cstring>

#include "sif_contractPolicy.hpp"

using namespace sif_contract_policy;

int main() {
  assert(input_bit("BATTERY", 7, "percent", 7, "i32", 3) ==
         kBatteryPercentI32);
  assert(input_bit("BATTERY", 7, "voltageMv", 9, "i32", 3) ==
         kBatteryVoltageMvI32);
  assert(input_bit("DHT", 3, "temperature", 11, "f32", 3) ==
         kDhtTemperatureF32);
  assert(input_bit("DHT", 3, "humidity", 8, "f32", 3) == kDhtHumidityF32);
  assert(input_bit("LIGHT", 5, "lux", 3, "f32", 3) == kLightLuxF32);
  assert(input_bit("OCCUPANCY", 9, "distanceCm", 10, "f32", 3) ==
         kOccupancyDistanceCmF32);
  assert(input_bit("GPIO", 4, "buttonPressed", 13, "bool", 4) ==
         kGpioButtonPressedBool);

  assert(output_bit("temperatureF", 12, "f32", 3) == kTemperatureFF32);
  assert(output_bit("heatIndexC", 10, "f32", 3) == kHeatIndexCF32);
  assert(output_bit("comfortScore", 12, "i32", 3) == kComfortScoreI32);
  assert(output_bit("occupied", 8, "i32", 3) == kOccupiedI32);
  assert(output_bit("nextSampleSeconds", 17, "i32", 3) ==
         kNextSampleSecondsI32);
  assert(output_bit("actuatorCommand", 15, "i32", 3) ==
         kActuatorCommandI32);

  // Contract enforcement is exact and typed; prefixes, case changes and
  // unsupported host fields must never be admitted accidentally.
  assert(input_bit("DHTx", 4, "temperature", 11, "f32", 3) == 0);
  assert(input_bit("DHT", 3, "temperature", 11, "i32", 3) == 0);
  assert(output_bit("actuatorCommand", 15, "f32", 3) == 0);
  assert(output_bit("unknown", 7, "i32", 3) == 0);
  return 0;
}
