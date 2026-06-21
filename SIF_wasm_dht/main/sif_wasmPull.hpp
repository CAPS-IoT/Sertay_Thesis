#pragma once

#include "esp_err.h"
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

#define SIF_WASM_SHA256_HEX_SIZE 65

/**
 * Mount the SPIFFS partition labelled "storage" at /spiffs.
 * Safe to call multiple times. Logs free/used bytes on success.
 */
esp_err_t sif_spiffs_mount(void);

/**
 * Plain-HTTP GET of raw Wasm bytes into out_path on SPIFFS.
 *
 *   url       e.g. "http://192.168.178.86:8081/wasm"
 *   out_path  e.g. "/spiffs/dht_reader.wasm"
 *
 * Caller is responsible for bringing WiFi up before and tearing it down
 * afterwards (we keep this routine focused on HTTP+filesystem so the
 * lifecycle stays explicit).
 *
 * Returns ESP_OK on success and writes the byte count to *out_size if
 * non-null. The download is streamed chunk-by-chunk to avoid heap spikes.
 */
esp_err_t sif_wasm_pull_blob(const char *url, const char *out_path, size_t *out_size);

/**
 * Compute the lowercase SHA-256 digest for an in-memory wasm blob.
 */
esp_err_t sif_wasm_digest_blob(const void *data, size_t size,
							   char out_digest[SIF_WASM_SHA256_HEX_SIZE]);

/**
 * Compute the lowercase SHA-256 digest for a wasm file already stored locally.
 */
esp_err_t sif_wasm_digest_file(const char *path,
							   char out_digest[SIF_WASM_SHA256_HEX_SIZE]);

#ifdef __cplusplus
}
#endif
