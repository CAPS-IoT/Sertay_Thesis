cmake_minimum_required(VERSION 3.8)
set(CMAKE_CXX_STANDARD 20)

set(SIF_BASE_DIR "" CACHE PATH "Path to the external SIF base repository")
if(NOT SIF_BASE_DIR)
    if(DEFINED ENV{SIF_BASE_DIR} AND NOT "$ENV{SIF_BASE_DIR}" STREQUAL "")
        set(SIF_BASE_DIR "$ENV{SIF_BASE_DIR}")
    else()
        message(FATAL_ERROR
            "Set SIF_BASE_DIR to a SIF checkout containing the companion Wasm integration")
    endif()
endif()

if(NOT EXISTS "${SIF_BASE_DIR}/sif_framework/sif_function/sif_wasmFunction.cpp")
    message(FATAL_ERROR
        "SIF_BASE_DIR does not contain the required SIF Wasm integration: ${SIF_BASE_DIR}")
endif()

add_compile_options("-w" "-fpermissive")
# The firmware owns one reusable WAMR linear-memory slot, so local guest
# invocations are serialized on one SIF worker rather than reserving a second
# 32 KiB worker stack on the other core.
add_compile_options("-DSIF_THREADPOOL_WORKER_CORE_COUNT=1")
add_compile_options("-DWAMR_ESP_IDF_STATIC_LINEAR_MEMORY_SIZE=65536")

set(EXTRA_COMPONENT_DIRS
    "${SIF_BASE_DIR}/components/esp-idf-lib-0.9.2/components"
    "${SIF_BASE_DIR}/services"
    "${SIF_BASE_DIR}/components/"
    "${SIF_BASE_DIR}/resources/peripherals/sif_dht"
    "${SIF_BASE_DIR}/resources/peripherals/sif_epaper"
    "${SIF_BASE_DIR}/resources/peripherals/sif_ina3221"
    "${SIF_BASE_DIR}/resources/peripherals/sif_max17048"
    "${SIF_BASE_DIR}/resources/peripherals/sif_BatteryGauge"
    "${SIF_BASE_DIR}/resources/system"
    "${SIF_BASE_DIR}/sif_framework"
)

# Exclude components not needed / incompatible
set(EXCLUDE_COMPONENTS rest_client meshnow sif_meshnow espnow_multi bitsery)

include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project("sif_wasm_dht")
