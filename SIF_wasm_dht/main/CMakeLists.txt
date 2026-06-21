idf_component_register(
        SRCS main.cpp basicWasmTrigger.cpp basicWasmEvent.cpp sif_wasmHostApi.cpp sif_migratingWasmFunction.cpp sif_wasmPull.cpp sif_state.cpp sif_httpForward.cpp sif_control.cpp sif_telemetry.cpp sif_release.cpp sif_led.cpp
        INCLUDE_DIRS "." "${CMAKE_SOURCE_DIR}/../wasm_guests/basic_edge_demo"
        REQUIRES sif_framework sif_dht sif_BatteryGauge sif_mqtt sif_wifi wamr esp_http_client spiffs nvs_flash mqtt mbedtls
)
