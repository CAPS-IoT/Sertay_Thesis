#pragma once

#include "mqtt_client.h"

// Subscribe to the control topic and register a handler that updates
// sif_state and reboots when commands arrive. Should be called after
// the MQTT client is started and connected.
void sif_control_register(esp_mqtt_client_handle_t client, const char *topic);

void sif_control_handle_json(const char *json, int len);
