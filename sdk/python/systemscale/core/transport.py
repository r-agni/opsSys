"""
HTTP transport helpers.

Uses ``http.client`` with persistent connection pooling (HTTP/1.1 keep-alive)
to avoid per-request TCP handshake overhead.  Zero external dependencies.
"""

from __future__ import annotations

import http.client
import json
import logging
import os
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

logger = logging.getLogger("systemscale")

_pool_lock = threading.Lock()
_pool: dict[tuple[str, str, int], http.client.HTTPConnection] = {}


def _get_conn(url: str) -> tuple[http.client.HTTPConnection, str]:
    """Return a keep-alive connection and the path portion of *url*."""
    parsed = urllib.parse.urlparse(url)
    scheme = parsed.scheme or "http"
    host = parsed.hostname or "localhost"
    port = parsed.port or (443 if scheme == "https" else 80)
    path = parsed.path or "/"
    if parsed.query:
        path += "?" + parsed.query

    key = (scheme, host, port)
    with _pool_lock:
        conn = _pool.get(key)
        if conn is not None:
            try:
                conn.sock  # noqa: test if socket is still alive
                return conn, path
            except Exception:
                _pool.pop(key, None)

        if scheme == "https":
            conn = http.client.HTTPSConnection(host, port, timeout=15)
        else:
            conn = http.client.HTTPConnection(host, port, timeout=15)
        _pool[key] = conn
    return conn, path


def _request(
    method: str,
    url: str,
    body: bytes | None = None,
    headers: dict | None = None,
    timeout: float = 15.0,
) -> tuple[int, bytes]:
    """Execute an HTTP request with connection reuse and one retry on broken pipe."""
    hdrs = dict(headers or {})
    hdrs.setdefault("Connection", "keep-alive")
    if body is not None:
        hdrs.setdefault("Content-Type", "application/json")

    for attempt in range(2):
        try:
            conn, path = _get_conn(url)
            conn.timeout = timeout
            conn.request(method, path, body=body, headers=hdrs)
            resp = conn.getresponse()
            data = resp.read()
            return resp.status, data
        except (ConnectionError, OSError, http.client.RemoteDisconnected):
            parsed = urllib.parse.urlparse(url)
            key = (parsed.scheme or "http", parsed.hostname, parsed.port or 80)
            with _pool_lock:
                _pool.pop(key, None)
            if attempt == 0:
                continue
            raise

    return 500, b"connection failed"


def _resolve_url(explicit: str | None, env_var: str, default: str) -> str:
    if explicit:
        return explicit.rstrip("/")
    from_env = os.environ.get(env_var, "").strip()
    if from_env:
        return from_env.rstrip("/")
    return default.rstrip("/")


def post_with_retry(
    url:         str,
    body:        bytes,
    headers:     dict | None = None,
    max_retries: int         = 3,
    retry_delay: float       = 0.05,
) -> bool:
    """POST *body* to *url* with exponential-backoff retry. Returns True on 2xx."""
    for attempt in range(max_retries):
        try:
            status, _ = _request("POST", url, body, headers, timeout=2.0)
            if 200 <= status < 300:
                return True
            logger.warning("POST %s → HTTP %d", url, status)
            return False
        except Exception as e:
            logger.debug("POST %s failed (attempt %d/%d): %s", url, attempt + 1, max_retries, e)
            if attempt < max_retries - 1:
                time.sleep(retry_delay * (2 ** attempt))
    return False


def get_json(
    url:     str,
    headers: dict | None = None,
    params:  dict | None = None,
) -> Any:
    """GET *url* and return parsed JSON. Raises RuntimeError on HTTP error."""
    if params:
        url += "?" + urllib.parse.urlencode(
            {k: v for k, v in params.items() if v is not None}
        )
    status, data = _request("GET", url, headers=headers)
    if status >= 400:
        raise RuntimeError(f"GET {url} failed (HTTP {status}): {data.decode()}")
    return json.loads(data)


def post_json(
    url:     str,
    body:    dict,
    headers: dict | None = None,
) -> Any:
    """POST JSON *body* to *url* and return parsed response. Raises RuntimeError on error."""
    status, data = _request("POST", url, json.dumps(body).encode(), headers)
    if status >= 400:
        raise RuntimeError(f"POST {url} failed (HTTP {status}): {data.decode()}")
    return json.loads(data)
