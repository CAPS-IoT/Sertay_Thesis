#pragma once

#include <stddef.h>
#include <stdint.h>
#include <string.h>

// The ESP32 implements a deliberately small, typed host API. Compile a release
// contract into these bits once, then enforce it without allocating or parsing
// JSON in the invocation hot path.
struct SifContractPolicy {
  uint32_t inputs = 0;
  uint32_t outputs = 0;
  bool valid = false;
};

namespace sif_contract_policy {

enum Input : uint32_t {
  kBatteryPercentI32 = 1U << 0,
  kBatteryVoltageMvI32 = 1U << 1,
  kDhtTemperatureF32 = 1U << 2,
  kDhtHumidityF32 = 1U << 3,
  kLightLuxF32 = 1U << 4,
  kOccupancyDistanceCmF32 = 1U << 5,
  kGpioButtonPressedBool = 1U << 6,
};

enum Output : uint32_t {
  kTemperatureFF32 = 1U << 0,
  kHeatIndexCF32 = 1U << 1,
  kComfortScoreI32 = 1U << 2,
  kOccupiedI32 = 1U << 3,
  kNextSampleSecondsI32 = 1U << 4,
  kActuatorCommandI32 = 1U << 5,
};

inline bool token_equals(const char *value, size_t value_len,
                         const char *expected) {
  return value && expected && strlen(expected) == value_len &&
         memcmp(value, expected, value_len) == 0;
}

inline uint32_t input_bit(const char *resource, size_t resource_len,
                          const char *key, size_t key_len,
                          const char *type, size_t type_len) {
  if (token_equals(resource, resource_len, "BATTERY")) {
    if (token_equals(key, key_len, "percent") &&
        token_equals(type, type_len, "i32")) {
      return kBatteryPercentI32;
    }
    if (token_equals(key, key_len, "voltageMv") &&
        token_equals(type, type_len, "i32")) {
      return kBatteryVoltageMvI32;
    }
  } else if (token_equals(resource, resource_len, "DHT")) {
    if (token_equals(key, key_len, "temperature") &&
        token_equals(type, type_len, "f32")) {
      return kDhtTemperatureF32;
    }
    if (token_equals(key, key_len, "humidity") &&
        token_equals(type, type_len, "f32")) {
      return kDhtHumidityF32;
    }
  } else if (token_equals(resource, resource_len, "LIGHT") &&
             token_equals(key, key_len, "lux") &&
             token_equals(type, type_len, "f32")) {
    return kLightLuxF32;
  } else if (token_equals(resource, resource_len, "OCCUPANCY") &&
             token_equals(key, key_len, "distanceCm") &&
             token_equals(type, type_len, "f32")) {
    return kOccupancyDistanceCmF32;
  } else if (token_equals(resource, resource_len, "GPIO") &&
             token_equals(key, key_len, "buttonPressed") &&
             token_equals(type, type_len, "bool")) {
    return kGpioButtonPressedBool;
  }
  return 0;
}

inline uint32_t output_bit(const char *name, size_t name_len,
                           const char *type, size_t type_len) {
  if (!token_equals(type, type_len, "f32")) {
    if (!token_equals(type, type_len, "i32")) return 0;
    if (token_equals(name, name_len, "comfortScore")) return kComfortScoreI32;
    if (token_equals(name, name_len, "occupied")) return kOccupiedI32;
    if (token_equals(name, name_len, "nextSampleSeconds")) {
      return kNextSampleSecondsI32;
    }
    if (token_equals(name, name_len, "actuatorCommand")) {
      return kActuatorCommandI32;
    }
    return 0;
  }
  if (token_equals(name, name_len, "temperatureF")) return kTemperatureFF32;
  if (token_equals(name, name_len, "heatIndexC")) return kHeatIndexCF32;
  return 0;
}

}  // namespace sif_contract_policy
