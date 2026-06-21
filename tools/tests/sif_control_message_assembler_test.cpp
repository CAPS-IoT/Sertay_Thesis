#include <cassert>
#include <cstring>

#include "sif_controlMessageAssembler.hpp"

using Status = SifControlMessageStatus;

int main() {
  SifControlMessageAssembler<64> assembler;

  const char complete[] = "{\"action\":\"set_battery\"}";
  assert(assembler.consume(true, true, complete, strlen(complete),
                           strlen(complete), 0) == Status::complete);
  assert(assembler.payloadLength() == strlen(complete));
  assert(strcmp(assembler.payload(), complete) == 0);

  assembler.reset();
  const char fragmented[] = "{\"action\":\"stage_release\",\"generation\":17}";
  constexpr size_t first_length = 13;
  constexpr size_t second_length = 17;
  const size_t total_length = strlen(fragmented);
  assert(assembler.consume(true, true, fragmented, first_length, total_length,
                           0) == Status::incomplete);
  assert(assembler.receiving());
  assert(assembler.consume(false, false, fragmented + first_length,
                           second_length, total_length,
                           first_length) == Status::incomplete);
  assert(assembler.consume(false, false,
                           fragmented + first_length + second_length,
                           total_length - first_length - second_length,
                           total_length, first_length + second_length) ==
         Status::complete);
  assert(strcmp(assembler.payload(), fragmented) == 0);

  assembler.reset();
  assert(assembler.consume(false, false, complete, strlen(complete),
                           strlen(complete), 0) == Status::ignored);

  char oversized[65] = {};
  memset(oversized, 'x', sizeof(oversized));
  assert(assembler.consume(true, true, oversized, 32, sizeof(oversized), 0) ==
         Status::rejected);
  assert(!assembler.receiving());

  assert(assembler.consume(true, true, fragmented, first_length, total_length,
                           0) == Status::incomplete);
  assert(assembler.consume(false, false, fragmented + first_length, 4,
                           total_length, first_length + 1) ==
         Status::rejected);
  assert(!assembler.receiving());

  assert(assembler.consume(true, true, fragmented, first_length, total_length,
                           0) == Status::incomplete);
  assert(assembler.consume(true, false, complete, strlen(complete),
                           strlen(complete), 0) == Status::ignored);
  assert(!assembler.receiving());

  assert(assembler.consume(true, true, complete, strlen(complete),
                           strlen(complete), 1) == Status::rejected);
  assert(assembler.consume(true, true, nullptr, 1, 1, 0) ==
         Status::rejected);

  SifControlMessageAssembler<2048> production_assembler;
  char stage_command[1122] = {};
  memset(stage_command, 's', sizeof(stage_command));
  assert(production_assembler.consume(true, true, stage_command, 900,
                                      sizeof(stage_command), 0) ==
         Status::incomplete);
  assert(production_assembler.consume(false, false, stage_command + 900,
                                      sizeof(stage_command) - 900,
                                      sizeof(stage_command), 900) ==
         Status::complete);
  assert(production_assembler.payloadLength() == sizeof(stage_command));

  return 0;
}
