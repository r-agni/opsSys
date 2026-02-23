"""
Token exchange with caching.

Exchanges a ``ssk_live_...`` API key for a short-lived JWT issued by
the apikey-service.  Tokens are cached in-process and re-exchanged 45
minutes before expiry so callers never block on a silent refresh.
"""

from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.request

_cache: dict[str, tuple[str, float]] = {}  # api_key → (jwt, expires_at)
_lock  = threading.Lock()


def exchange_token(api_key: str, apikey_url: str) -> str:
    """Return a valid JWT for *api_key*, fetching a new one if necessary."""
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
    try:
        with urllib.request.urlopen(req, timeout=10.0) as resp:
            data = json.loads(resp.read())
    except urllib.error.HTTPError as e:
        raise RuntimeError(
            f"Token exchange failed (HTTP {e.code}): {e.read().decode()}"
        ) from e
    except Exception as e:
        raise RuntimeError(f"Token exchange failed: {e}") from e

    token = data.get("access_token") or data["token"]
    ttl   = data.get("expires_in", 3600)
    # Re-exchange 45 minutes before expiry
    with _lock:
        _cache[api_key] = (token, time.time() + ttl - 2700)
    return token
