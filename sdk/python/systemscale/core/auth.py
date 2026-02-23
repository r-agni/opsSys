"""
Token exchange with caching.

Exchanges a ``ssk_live_...`` API key for a short-lived JWT issued by
the apikey-service.  Tokens are cached in-process and re-exchanged 45
minutes before expiry so callers never block on a silent refresh.
"""

from __future__ import annotations

import json
import logging
import random
import threading
import time
import urllib.error
import urllib.request

logger = logging.getLogger("systemscale")

_cache: dict[str, tuple[str, float]] = {}  # api_key → (jwt, expires_at)
_lock  = threading.Lock()

_MAX_RETRIES   = 3
_BASE_DELAY    = 0.25  # seconds


def exchange_token(api_key: str, apikey_url: str) -> str:
    """Return a valid JWT for *api_key*, fetching a new one if necessary.

    Retries up to 3 times with jittered exponential backoff on transient errors.
    """
    with _lock:
        cached = _cache.get(api_key)
        if cached and time.time() < cached[1]:
            return cached[0]

    url  = f"{apikey_url.rstrip('/')}/v1/token"
    body = json.dumps({"api_key": api_key}).encode()
    req  = urllib.request.Request(
        url, data=body, method="POST",
        headers={"Content-Type": "application/json"},
    )

    last_err: Exception | None = None
    for attempt in range(_MAX_RETRIES):
        try:
            with urllib.request.urlopen(req, timeout=10.0) as resp:
                data = json.loads(resp.read())
            break
        except urllib.error.HTTPError as e:
            if e.code < 500:
                raise RuntimeError(
                    f"Token exchange failed (HTTP {e.code}): {e.read().decode()}"
                ) from e
            last_err = e
        except Exception as e:
            last_err = e

        if attempt < _MAX_RETRIES - 1:
            delay = _BASE_DELAY * (2 ** attempt)
            time.sleep(delay + random.uniform(0, delay * 0.5))
            logger.debug("Token exchange retry %d/%d", attempt + 1, _MAX_RETRIES)
    else:
        raise RuntimeError(f"Token exchange failed after {_MAX_RETRIES} attempts: {last_err}") from last_err

    token = data.get("access_token") or data["token"]
    ttl   = data.get("expires_in", 3600)
    with _lock:
        _cache[api_key] = (token, time.time() + ttl - 2700)
    return token
