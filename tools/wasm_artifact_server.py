#!/usr/bin/env python3

"""Serve a single Wasm artifact with digest metadata for ESP32 reloads."""

from __future__ import annotations

import argparse
import hashlib
import os
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from shutil import copyfileobj
from typing import Optional, Tuple
from urllib.parse import urlsplit


class ArtifactState:
    def __init__(self, artifact_path: str) -> None:
        self.artifact_path = os.path.abspath(artifact_path)
        self._cache_key: Optional[Tuple[int, int]] = None
        self._cached_digest: Optional[str] = None

    def digest_and_size(self) -> Tuple[str, int]:
        stat_result = os.stat(self.artifact_path)
        cache_key = (stat_result.st_mtime_ns, stat_result.st_size)
        if cache_key != self._cache_key:
            digest = hashlib.sha256()
            with open(self.artifact_path, "rb") as artifact:
                for chunk in iter(lambda: artifact.read(64 * 1024), b""):
                    digest.update(chunk)
            self._cache_key = cache_key
            self._cached_digest = digest.hexdigest()
        return self._cached_digest or "", stat_result.st_size


def build_handler(state: ArtifactState, route: str):
    class Handler(BaseHTTPRequestHandler):
        server_version = "SIFWasmArtifactServer/1.0"

        def do_GET(self) -> None:
            self._handle_request(send_body=True)

        def do_HEAD(self) -> None:
            self._handle_request(send_body=False)

        def _handle_request(self, send_body: bool) -> None:
            path = urlsplit(self.path).path
            if path == "/health":
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Type", "text/plain; charset=utf-8")
                self.send_header("Content-Length", "2")
                self.end_headers()
                if send_body:
                    self.wfile.write(b"ok")
                return

            if path != route:
                self.send_error(HTTPStatus.NOT_FOUND, "Not Found")
                return

            try:
                digest, size = state.digest_and_size()
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Type", "application/wasm")
                self.send_header("Content-Length", str(size))
                self.send_header("Cache-Control", "no-store")
                self.send_header("ETag", '"%s"' % digest)
                self.send_header("X-Wasm-Sha256", digest)
                self.end_headers()
                if send_body:
                    with open(state.artifact_path, "rb") as artifact:
                        copyfileobj(artifact, self.wfile)
            except FileNotFoundError:
                self.send_error(HTTPStatus.NOT_FOUND, "Artifact not found")
            except OSError as err:
                self.send_error(HTTPStatus.INTERNAL_SERVER_ERROR, str(err))

        def log_message(self, fmt: str, *args: object) -> None:
            super().log_message(fmt, *args)

    return Handler


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Serve a Wasm artifact with HEAD and X-Wasm-Sha256 support.",
    )
    parser.add_argument("artifact", help="Path to the .wasm artifact to serve")
    parser.add_argument(
        "--bind",
        default="0.0.0.0",
        help="Address to bind to (default: 0.0.0.0)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=8081,
        help="Port to listen on (default: 8081)",
    )
    parser.add_argument(
        "--route",
        default="/wasm",
        help="HTTP route for the artifact (default: /wasm)",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    route = args.route if args.route.startswith("/") else "/" + args.route
    state = ArtifactState(args.artifact)
    digest, size = state.digest_and_size()
    server = ThreadingHTTPServer((args.bind, args.port), build_handler(state, route))
    print(
        "Serving %s at http://%s:%d%s sha256=%s bytes=%d"
        % (state.artifact_path, args.bind, args.port, route, digest, size),
        flush=True,
    )
    exit_code = 0
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        exit_code = 130
    finally:
        server.server_close()
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())