#pragma once

#include <stddef.h>
#include <string.h>

enum class SifControlMessageStatus {
  ignored,
  incomplete,
  complete,
  rejected,
};

// ESP-MQTT emits one MQTT_EVENT_DATA per receive-buffer-sized fragment. Only
// the first event carries the topic, so control messages must be assembled
// before they are parsed as JSON. Capacity is fixed to keep memory use bounded.
template <size_t Capacity>
class SifControlMessageAssembler {
 public:
  static_assert(Capacity > 0, "control message capacity must be positive");

  SifControlMessageStatus consume(bool topic_present, bool control_topic,
                                  const char *fragment,
                                  size_t fragment_length,
                                  size_t total_length, size_t offset) {
    if (topic_present) {
      reset();
      if (!control_topic) return SifControlMessageStatus::ignored;
      if (offset != 0) return SifControlMessageStatus::rejected;
      receiving_ = true;
      expected_length_ = total_length;
    } else if (!receiving_) {
      return SifControlMessageStatus::ignored;
    }

    if (!receiving_ || !fragment || fragment_length == 0 ||
        total_length == 0 || total_length > Capacity ||
        total_length != expected_length_ || offset != received_length_ ||
        offset > total_length || fragment_length > total_length - offset) {
      reset();
      return SifControlMessageStatus::rejected;
    }

    memcpy(payload_ + offset, fragment, fragment_length);
    received_length_ += fragment_length;
    if (received_length_ != expected_length_) {
      return SifControlMessageStatus::incomplete;
    }

    payload_[received_length_] = '\0';
    receiving_ = false;
    return SifControlMessageStatus::complete;
  }

  void reset() {
    receiving_ = false;
    expected_length_ = 0;
    received_length_ = 0;
    payload_[0] = '\0';
  }

  const char *payload() const { return payload_; }
  size_t payloadLength() const { return received_length_; }
  bool receiving() const { return receiving_; }

 private:
  char payload_[Capacity + 1] = {};
  size_t expected_length_ = 0;
  size_t received_length_ = 0;
  bool receiving_ = false;
};
