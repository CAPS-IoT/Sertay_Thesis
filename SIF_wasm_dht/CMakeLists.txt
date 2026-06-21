cmake_minimum_required(VERSION 3.8)
set(CMAKE_CXX_STANDARD 20)

set(SIF_HOME "${CMAKE_CURRENT_LIST_DIR}/..")

add_compile_options("-w" "-fpermissive")

set(EXTRA_COMPONENT_DIRS 
    "${SIF_HOME}/components/esp-idf-lib-0.9.2/components" 
    "${SIF_HOME}/services" 
    "${SIF_HOME}/components/" 
    "${SIF_HOME}/resources/peripherals/sif_dht"
    "${SIF_HOME}/resources/peripherals/sif_epaper"
    "${SIF_HOME}/resources/peripherals/sif_ina3221"
    "${SIF_HOME}/resources/peripherals/sif_max17048"
    "${SIF_HOME}/resources/peripherals/sif_BatteryGauge"
    "${SIF_HOME}/resources/system" 
    "${SIF_HOME}/sif_framework"
)

# Exclude components not needed / incompatible
set(EXCLUDE_COMPONENTS rest_client meshnow sif_meshnow espnow_multi bitsery)

include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project("sif_wasm_dht")
