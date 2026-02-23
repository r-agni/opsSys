"""
HTTP transport helpers.

Thin wrappers around ``urllib.request`` providing retry logic and JSON
serialisation.  Zero external dependencies.
"""

from __future__ import annotations

import json
import logging
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

logger = logging.getLogger("systemscale")


def post_with_retry(
    url:         str,
    body:        bytes,
    headers:     dict | None = None,
    max_retries: int         = 3,
    retry_delay: float       = 0.05,
) -> bool:
    """POST *body* to *url* with exponential-backoff retry. Returns True on 2xx."""
    req = urllib.request.Request(
        url, data=body, method="POST",
        headers={"Content-Type": "application/json", **(headers or {})},
    )
    for attempt in range(max_retries):
        try:
            with urllib.request.urlopen(req, timeout=2.0) as resp:
                if 200 <= resp.status < 300:
                    return True
                logger.warning("POST %s → HTTP %d", url, resp.status)
                return False
        except urllib.error.URLError as e:
            logger.debug("POST %s failed (attempt %d/%d): %s", url, attempt + 1, max_retries, e)
            if attempt < max_retries - 1:
                time.sleep(retry_delay * (2 ** attempt))
        except Exception as e:
            logger.debug("POST %s error: %s", url, e)
            if attempt < max_retries - 1:
                time.sleep(retry_delay)
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
    req = urllib.request.Request(url, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=15.0) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        raise RuntimeError(
            f"GET {url} failed (HTTP {e.code}): {e.read().decode()}"
        ) from e


def post_json(
    url:     str,
    body:    dict,
    headers: dict | None = None,
) -> Any:
    """POST JSON *body* to *url* and return parsed response. Raises RuntimeError on error."""
    data = json.dumps(body).encode()
    req  = urllib.request.Request(
        url, data=data, method="POST",
        headers={"Content-Type": "application/json", **(headers or {})},
    )
    try:
        with urllib.request.urlopen(req, timeout=15.0) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        raise RuntimeError(
            f"POST {url} failed (HTTP {e.code}): {e.read().decode()}"
        ) from e
