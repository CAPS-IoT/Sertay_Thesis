#include "sif_wasmPull.hpp"

#include "esp_log.h"
#include "esp_http_client.h"
#include "esp_spiffs.h"
#include "mbedtls/sha256.h"

#include <errno.h>
#include <strings.h>
#include <sys/stat.h>
#include <stdio.h>
#include <string>
#include <string.h>

static const char *TAG = "WasmPull";
static const char *ARTIFACT_CALLER_HEADER = "X-SIF-Artifact-Caller";

struct digest_header_ctx {
  char *out_digest;
  bool found;
};

static void digest_bytes_to_hex(const uint8_t digest[32],
                                char out_digest[SIF_WASM_SHA256_HEX_SIZE]) {
  static const char *hex = "0123456789abcdef";
  for (int i = 0; i < 32; ++i) {
    out_digest[i * 2] = hex[(digest[i] >> 4) & 0x0F];
    out_digest[i * 2 + 1] = hex[digest[i] & 0x0F];
  }
  out_digest[64] = '\0';
}

static bool is_hex_digest(const char *digest) {
  if (!digest || strlen(digest) != 64) return false;
  for (size_t i = 0; i < 64; ++i) {
    char c = digest[i];
    bool digit = (c >= '0' && c <= '9');
    bool lower = (c >= 'a' && c <= 'f');
    bool upper = (c >= 'A' && c <= 'F');
    if (!digit && !lower && !upper) return false;
  }
  return true;
}

static esp_err_t capture_digest_header(esp_http_client_event_t *evt) {
  auto *ctx = static_cast<digest_header_ctx *>(evt->user_data);
  if (!ctx) return ESP_OK;
  if (evt->event_id == HTTP_EVENT_ON_HEADER && evt->header_key &&
      evt->header_value && strcasecmp(evt->header_key, "X-Wasm-Sha256") == 0) {
    size_t len = strlen(evt->header_value);
    if (len >= SIF_WASM_SHA256_HEX_SIZE) {
      return ESP_FAIL;
    }
    memcpy(ctx->out_digest, evt->header_value, len + 1);
    ctx->found = true;
  }
  return ESP_OK;
}

static esp_err_t replace_file_from_download(const std::string &tmp_path,
                                            const char *out_path) {
  struct stat st = {};
  if (stat(out_path, &st) != 0) {
    if (errno != ENOENT) {
      ESP_LOGE(TAG, "stat(%s) failed: errno=%d", out_path, errno);
      return ESP_FAIL;
    }
    if (rename(tmp_path.c_str(), out_path) != 0) {
      ESP_LOGE(TAG, "rename(%s -> %s) failed: errno=%d", tmp_path.c_str(),
               out_path, errno);
      return ESP_FAIL;
    }
    return ESP_OK;
  }

  std::string backup_path = std::string(out_path) + ".bak";
  remove(backup_path.c_str());

  if (rename(out_path, backup_path.c_str()) != 0) {
    ESP_LOGE(TAG, "rename(%s -> %s) failed: errno=%d", out_path,
             backup_path.c_str(), errno);
    return ESP_FAIL;
  }

  if (rename(tmp_path.c_str(), out_path) != 0) {
    int rename_errno = errno;
    ESP_LOGE(TAG, "rename(%s -> %s) failed: errno=%d", tmp_path.c_str(),
             out_path, rename_errno);
    if (rename(backup_path.c_str(), out_path) != 0) {
      ESP_LOGE(TAG, "rollback rename(%s -> %s) failed: errno=%d",
               backup_path.c_str(), out_path, errno);
    }
    return ESP_FAIL;
  }

  if (remove(backup_path.c_str()) != 0 && errno != ENOENT) {
    ESP_LOGW(TAG, "remove(%s) failed: errno=%d", backup_path.c_str(), errno);
  }

  return ESP_OK;
}

esp_err_t sif_spiffs_mount(void) {
  esp_vfs_spiffs_conf_t conf = {
      .base_path = "/spiffs",
      .partition_label = "storage",
      .max_files = 4,
      .format_if_mount_failed = true,
  };
  esp_err_t err = esp_vfs_spiffs_register(&conf);
  if (err == ESP_ERR_INVALID_STATE) {
    // Already mounted — treat as success.
    return ESP_OK;
  }
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "SPIFFS mount failed: %s", esp_err_to_name(err));
    return err;
  }
  size_t total = 0, used = 0;
  if (esp_spiffs_info("storage", &total, &used) == ESP_OK) {
    ESP_LOGI(TAG, "SPIFFS mounted at /spiffs (used=%u, total=%u bytes)",
             (unsigned)used, (unsigned)total);
  }
  return ESP_OK;
}

esp_err_t sif_wasm_pull_blob(const char *url, const char *out_path, size_t *out_size) {
  if (!url || !out_path) return ESP_ERR_INVALID_ARG;

  ESP_LOGI(TAG, "GET %s", url);

  esp_http_client_config_t cfg = {};
  cfg.url = url;
  cfg.timeout_ms = 15000;
  cfg.disable_auto_redirect = false;

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (!client) {
    ESP_LOGE(TAG, "esp_http_client_init failed");
    return ESP_FAIL;
  }
  esp_http_client_set_header(client, ARTIFACT_CALLER_HEADER,
                             "device-artifact-download");

  esp_err_t err = esp_http_client_open(client, 0);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "esp_http_client_open: %s", esp_err_to_name(err));
    esp_http_client_cleanup(client);
    return err;
  }

  int64_t content_length = esp_http_client_fetch_headers(client);
  int status = esp_http_client_get_status_code(client);
  ESP_LOGI(TAG, "HTTP %d, content-length=%lld", status, content_length);
  if (status != 200) {
    ESP_LOGE(TAG, "Unexpected HTTP status %d", status);
    esp_http_client_close(client);
    esp_http_client_cleanup(client);
    return ESP_FAIL;
  }

  std::string tmp_path = std::string(out_path) + ".download";
  FILE *fp = fopen(tmp_path.c_str(), "wb");
  if (!fp) {
    ESP_LOGE(TAG, "fopen(%s) failed", tmp_path.c_str());
    esp_http_client_close(client);
    esp_http_client_cleanup(client);
    return ESP_FAIL;
  }

  static char buf[512];
  size_t total = 0;
  for (;;) {
    int n = esp_http_client_read(client, buf, sizeof(buf));
    if (n < 0) {
      ESP_LOGE(TAG, "read error");
      fclose(fp);
      remove(tmp_path.c_str());
      esp_http_client_close(client);
      esp_http_client_cleanup(client);
      return ESP_FAIL;
    }
    if (n == 0) break;
    if (fwrite(buf, 1, (size_t)n, fp) != (size_t)n) {
      ESP_LOGE(TAG, "write error to %s", tmp_path.c_str());
      fclose(fp);
      remove(tmp_path.c_str());
      esp_http_client_close(client);
      esp_http_client_cleanup(client);
      return ESP_FAIL;
    }
    total += (size_t)n;
  }

  fclose(fp);
  esp_http_client_close(client);
  esp_http_client_cleanup(client);

  if (replace_file_from_download(tmp_path, out_path) != ESP_OK) {
    remove(tmp_path.c_str());
    return ESP_FAIL;
  }

  ESP_LOGI(TAG, "Saved %u bytes to %s", (unsigned)total, out_path);
  if (out_size) *out_size = total;
  return ESP_OK;
}

esp_err_t sif_wasm_fetch_digest(const char *url,
                                char out_digest[SIF_WASM_SHA256_HEX_SIZE]) {
  if (!url || !out_digest) return ESP_ERR_INVALID_ARG;

  out_digest[0] = '\0';
  digest_header_ctx header_ctx = {out_digest, false};

  ESP_LOGI(TAG, "HEAD %s", url);

  esp_http_client_config_t cfg = {};
  cfg.url = url;
  cfg.timeout_ms = 15000;
  cfg.disable_auto_redirect = false;
  cfg.method = HTTP_METHOD_HEAD;
  cfg.event_handler = capture_digest_header;
  cfg.user_data = &header_ctx;

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (!client) {
    ESP_LOGE(TAG, "esp_http_client_init failed");
    return ESP_FAIL;
  }
  esp_http_client_set_header(client, ARTIFACT_CALLER_HEADER,
                             "device-artifact-digest");

  esp_err_t err = esp_http_client_open(client, 0);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "esp_http_client_open: %s", esp_err_to_name(err));
    esp_http_client_cleanup(client);
    return err;
  }

  int64_t content_length = esp_http_client_fetch_headers(client);
  int status = esp_http_client_get_status_code(client);
  ESP_LOGI(TAG, "HTTP %d, content-length=%lld", status, content_length);
  if (status == 404) {
    esp_http_client_close(client);
    esp_http_client_cleanup(client);
    return ESP_ERR_NOT_FOUND;
  }
  if (status != 200) {
    ESP_LOGE(TAG, "Unexpected HTTP status %d", status);
    esp_http_client_close(client);
    esp_http_client_cleanup(client);
    return ESP_FAIL;
  }
  if (!header_ctx.found || !is_hex_digest(out_digest)) {
    ESP_LOGE(TAG, "Missing or invalid X-Wasm-Sha256 header");
    esp_http_client_close(client);
    esp_http_client_cleanup(client);
    return ESP_FAIL;
  }

  esp_http_client_close(client);
  esp_http_client_cleanup(client);
  ESP_LOGI(TAG, "Remote digest %s", out_digest);
  return ESP_OK;
}

esp_err_t sif_wasm_digest_blob(const void *data, size_t size,
                               char out_digest[SIF_WASM_SHA256_HEX_SIZE]) {
  if (!data || size == 0 || !out_digest) return ESP_ERR_INVALID_ARG;

  mbedtls_sha256_context ctx;
  uint8_t digest[32] = {};
  mbedtls_sha256_init(&ctx);
  mbedtls_sha256_starts(&ctx, 0);
  mbedtls_sha256_update(&ctx, static_cast<const uint8_t *>(data), size);
  mbedtls_sha256_finish(&ctx, digest);
  mbedtls_sha256_free(&ctx);

  digest_bytes_to_hex(digest, out_digest);
  return ESP_OK;
}

esp_err_t sif_wasm_digest_file(const char *path,
                               char out_digest[SIF_WASM_SHA256_HEX_SIZE]) {
  if (!path || !out_digest) return ESP_ERR_INVALID_ARG;

  FILE *fp = fopen(path, "rb");
  if (!fp) {
    ESP_LOGE(TAG, "fopen(%s) failed", path);
    return ESP_FAIL;
  }

  mbedtls_sha256_context ctx;
  uint8_t digest[32] = {};
  uint8_t buf[512];
  mbedtls_sha256_init(&ctx);
  mbedtls_sha256_starts(&ctx, 0);
  bool ok = true;
  while (ok) {
    size_t n = fread(buf, 1, sizeof(buf), fp);
    if (n > 0) {
      mbedtls_sha256_update(&ctx, buf, n);
    }
    if (n < sizeof(buf)) {
      if (feof(fp)) break;
      ok = false;
      ESP_LOGE(TAG, "fread(%s) failed", path);
      break;
    }
  }
  if (ok) mbedtls_sha256_finish(&ctx, digest);
  mbedtls_sha256_free(&ctx);
  fclose(fp);
  if (!ok) {
    return ESP_FAIL;
  }

  digest_bytes_to_hex(digest, out_digest);
  return ESP_OK;
}

esp_err_t sif_wasm_push_blob(const char *url, const void *data, size_t size) {
  if (!url || !data || size == 0) return ESP_ERR_INVALID_ARG;

  ESP_LOGI(TAG, "PUT %s (%u bytes)", url, (unsigned)size);

  esp_http_client_config_t cfg = {};
  cfg.url = url;
  cfg.timeout_ms = 15000;
  cfg.disable_auto_redirect = false;
  cfg.method = HTTP_METHOD_PUT;

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (!client) {
    ESP_LOGE(TAG, "esp_http_client_init failed");
    return ESP_FAIL;
  }

  esp_http_client_set_header(client, ARTIFACT_CALLER_HEADER,
                             "device-artifact-upload");
  esp_http_client_set_header(client, "Content-Type", "application/wasm");

  esp_err_t err = esp_http_client_open(client, (int)size);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "esp_http_client_open: %s", esp_err_to_name(err));
    esp_http_client_cleanup(client);
    return err;
  }

  int written = esp_http_client_write(client, static_cast<const char *>(data), (int)size);
  if (written < 0 || (size_t)written != size) {
    ESP_LOGE(TAG, "write error: wrote %d/%u bytes", written, (unsigned)size);
    esp_http_client_close(client);
    esp_http_client_cleanup(client);
    return ESP_FAIL;
  }

  int64_t content_length = esp_http_client_fetch_headers(client);
  int status = esp_http_client_get_status_code(client);
  ESP_LOGI(TAG, "HTTP %d, content-length=%lld", status, content_length);
  if (status != 200) {
    ESP_LOGE(TAG, "Unexpected HTTP status %d", status);
    esp_http_client_close(client);
    esp_http_client_cleanup(client);
    return ESP_FAIL;
  }

  esp_http_client_close(client);
  esp_http_client_cleanup(client);
  ESP_LOGI(TAG, "Uploaded %u bytes to %s", (unsigned)size, url);
  return ESP_OK;
}

esp_err_t sif_wasm_push_file(const char *url, const char *path, size_t *out_size) {
  if (!url || !path) return ESP_ERR_INVALID_ARG;

  struct stat st;
  if (stat(path, &st) != 0 || st.st_size <= 0) {
    ESP_LOGE(TAG, "stat(%s) failed or empty", path);
    return ESP_FAIL;
  }

  FILE *fp = fopen(path, "rb");
  if (!fp) {
    ESP_LOGE(TAG, "fopen(%s) failed", path);
    return ESP_FAIL;
  }

  size_t total = (size_t)st.st_size;
  uint8_t *buf = static_cast<uint8_t *>(malloc(total));
  if (!buf) {
    ESP_LOGE(TAG, "malloc(%u) failed", (unsigned)total);
    fclose(fp);
    return ESP_ERR_NO_MEM;
  }

  size_t read_total = fread(buf, 1, total, fp);
  fclose(fp);
  if (read_total != total) {
    ESP_LOGE(TAG, "fread(%s) read %u/%u bytes", path, (unsigned)read_total, (unsigned)total);
    free(buf);
    return ESP_FAIL;
  }

  esp_err_t err = sif_wasm_push_blob(url, buf, total);
  free(buf);
  if (err == ESP_OK && out_size) *out_size = total;
  return err;
}
