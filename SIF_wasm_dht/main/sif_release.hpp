#pragma once

#include <stddef.h>
#include <stdint.h>
#include <string>

#include "esp_err.h"
#include "sif_state.hpp"
#include "sif_wasmFunction.hpp"

void sif_release_init(sif_state::State &initial_state);
void sif_release_set_local_function(WasmFunction *function);
void sif_release_set_active_function(SubscriberFunction *function);

esp_err_t sif_release_stage_async(std::string command_id,
                                  std::string artifact_url,
                                  sif_state::ReleaseMetadata release);
esp_err_t sif_release_activate_local_async(const std::string &command_id,
                                           uint64_t generation);
esp_err_t sif_release_set_edge_async(const std::string &command_id,
                                     uint64_t generation);
esp_err_t sif_release_pause(const std::string &command_id,
                            uint64_t generation);
esp_err_t sif_release_resume(const std::string &command_id,
                             uint64_t generation);

std::string sif_release_active_function_identity();
uint64_t sif_release_active_generation();
bool sif_release_input_declared(const char *resource, const char *key,
                                const char *type);
bool sif_release_output_declared(const char *name, const char *type);
bool sif_release_input_declared_n(const char *resource, size_t resource_len,
                                  const char *key, size_t key_len,
                                  const char *type, size_t type_len);
bool sif_release_output_declared_n(const char *name, size_t name_len,
                                   const char *type, size_t type_len);
