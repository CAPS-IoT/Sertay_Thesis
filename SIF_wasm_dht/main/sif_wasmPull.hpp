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
 * Plain-HTTP GET of an OCI blob URL into out_path on SPIFFS.
 *
 *   url       e.g. "http://192.168.178.86:5050/v2/dht_reader/blobs/sha256:abc..."
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
 * Fetch the current remote wasm digest from an upload/download endpoint.
 *
 * Expects the server to return X-Wasm-Sha256 on HTTP HEAD /wasm.
 */
esp_err_t sif_wasm_fetch_digest(const char *url,
								char out_digest[SIF_WASM_SHA256_HEX_SIZE]);

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

/**
 * Plain-HTTP PUT of a wasm blob to an edge host upload URL.
 *
 *   url   e.g. "http://192.168.178.113:8080/wasm"
 *   data  raw wasm bytes already present in memory
 *   size  number of bytes in data
 *
 * Caller is responsible for WiFi lifecycle. The upload is intended for
 * local-to-edge binary migration before the device starts request offload.
 */
esp_err_t sif_wasm_push_blob(const char *url, const void *data, size_t size);

/**
 * Plain-HTTP PUT of a wasm file already stored on SPIFFS.
 *
 * Returns ESP_OK on success and writes the uploaded byte count to *out_size if
 * non-null.
 */
esp_err_t sif_wasm_push_file(const char *url, const char *path, size_t *out_size);

#ifdef __cplusplus
}
#endif
