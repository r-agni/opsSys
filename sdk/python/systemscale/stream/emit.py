"""
StreamEmitter — continuous (high-frequency) data emission.

Queues frames in-process and drains them to the local edge agent in a
background thread, providing non-blocking ``send()`` at up to 50 Hz.

The ``to`` parameter routes a frame to a specific actor instead of
broadcasting it within the project:

    emitter.send({"lat": 28.6}, to="operator:*")         # all operators
    emitter.send(data, to="device:drone-002")             # peer device
    emitter.send(data, to="human:pilot-1")                # specific human

Standalone usage::

    from systemscale.stream import StreamEmitter

    emitter = StreamEmitter(api_key="ssk_live_...", project="my-fleet",
                            actor="drone-001")
    emitter.start()
    emitter.send({"sensors/temp": 85.2, "nav/alt": 102.3})
    emitter.stop()
"""

from __future__ import annotations

import json
import logging
import os
import queue
import socket
import threading
from dataclasses import dataclass, field
from typing import Any

from ..core.transport import post_with_retry

logger = logging.getLogger("systemscale")


def _flatten_keys(data: dict, prefix: str = "") -> dict:
    """Recursively flatten nested dicts using slash-separated keys."""
    out: dict = {}
    for k, v in data.items():
        full_key = f"{prefix}/{k}" if prefix else k
        if isinstance(v, dict):
            out.update(_flatten_keys(v, full_key))
        else:
            out[full_key] = v
    return out


@dataclass
class _Frame:
    data:        dict
    stream:      str
    stream_name: str
    lat:         float
    lon:         float
    alt:         float
    project_id:  str
    tags:        dict | None
    to:          str | None  # "type:id" routing, e.g. "operator:*"


_STOP = object()


class StreamEmitter:
    """
    High-frequency telemetry sender.

    Frames are placed on an in-process queue (O(1)) and sent by a
    background thread, so ``send()`` never blocks the caller.
    """

    def __init__(
        self,
        api_key:    str,
        project:    str,
        actor:      str | None = None,
        *,
        agent_api:  str = "http://127.0.0.1:7777",
        queue_size: int = 8192,
    ) -> None:
        self._api_key   = api_key
        self._project   = project
        self._actor     = actor or os.environ.get("SYSTEMSCALE_DEVICE") or socket.gethostname()
        self._agent_api = agent_api.rstrip("/")
        self._queue:  queue.Queue = queue.Queue(maxsize=queue_size)
        self._thread: threading.Thread | None = None
        self._stopped = False

    # ── Lifecycle ─────────────────────────────────────────────────────────────

    def start(self) -> None:
        """Start the background sender thread."""
        if self._thread is not None:
            return
        self._stopped = False
        self._thread  = threading.Thread(target=self._run, name="ss-emit", daemon=True)
        self._thread.start()

    def stop(self) -> None:
        """Drain remaining frames and stop the background thread (waits up to 5 s)."""
        if self._stopped:
            return
        self._stopped = True
        try:
            self._queue.put_nowait(_STOP)
        except queue.Full:
            pass
        if self._thread:
            self._thread.join(timeout=5.0)

    # ── Public API ────────────────────────────────────────────────────────────

    def send(
        self,
        data:        dict,
        *,
        stream:      str        = "telemetry",
        stream_name: str        = "",
        lat:         float      = 0.0,
        lon:         float      = 0.0,
        alt:         float      = 0.0,
        tags:        dict | None = None,
        to:          str | None = None,
    ) -> None:
        """
        Queue a data frame for delivery to the edge agent.

        :param data:        Key-value pairs; nested dicts are flattened
                            (``{"sensors": {"temp": 85}}`` → ``{"sensors/temp": 85}``).
        :param stream:      Stream type: ``"telemetry"`` | ``"sensor"`` | ``"event"``.
        :param stream_name: Optional sub-stream label within the stream type.
        :param lat:         GPS latitude (decimal degrees).
        :param lon:         GPS longitude (decimal degrees).
        :param alt:         Altitude in metres.
        :param tags:        Low-cardinality string tags stored as indexed columns.
        :param to:          Target actor ``"type:id"`` (e.g. ``"operator:ground-control"``
                            or ``"operator:*"`` for all operators).  ``None`` = broadcast.
        """
        try:
            self._queue.put_nowait(_Frame(
                data=_flatten_keys(data), stream=stream, stream_name=stream_name,
                lat=lat, lon=lon, alt=alt, project_id=self._project,
                tags=tags, to=to,
            ))
        except queue.Full:
            logger.warning("systemscale emit queue full — dropping frame")

    # ── Background sender ─────────────────────────────────────────────────────

    def _run(self) -> None:
        url = f"{self._agent_api}/v1/log"
        while not self._stopped:
            try:
                item = self._queue.get(timeout=0.1)
            except queue.Empty:
                continue
            if item is _STOP:
                break
            assert isinstance(item, _Frame)
            body_dict: dict[str, Any] = {
                "data":       item.data,
                "stream":     item.stream,
                "lat":        item.lat,
                "lon":        item.lon,
                "alt":        item.alt,
                "project_id": item.project_id,
            }
            if item.stream_name:
                body_dict["stream_name"] = item.stream_name
            if item.tags:
                body_dict["tags"] = item.tags
            if item.to:
                body_dict["to"] = item.to
            post_with_retry(url, json.dumps(body_dict).encode())
