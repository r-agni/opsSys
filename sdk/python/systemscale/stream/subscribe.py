"""
StreamSubscriber — real-time telemetry subscription (operator-side).

Connects to the ws-gateway with ``format=json`` and yields frames as
they arrive.  Requires ``websocket-client`` (optional dependency).

Standalone usage::

    from systemscale.stream import StreamSubscriber

    sub = StreamSubscriber(
        api_key    = "ssk_live_...",
        project    = "my-fleet",
        apikey_url = "https://keys.systemscale.io",
        ws_url     = "wss://ws.systemscale.io",
        fleet_api  = "https://api.systemscale.io",
    )
    for frame in sub.receive(device="drone-001"):
        print(frame.fields)
"""

from __future__ import annotations

import json
import logging
import queue
import threading
import time
import urllib.parse
from dataclasses import dataclass
from typing import Any, Generator

from ..core.auth import exchange_token

logger = logging.getLogger("systemscale")


@dataclass
class DataFrame:
    """A real-time data frame received from the ws-gateway."""
    device:    str
    stream:    str
    timestamp: str
    fields:    dict[str, Any]


class StreamSubscriber:
    """
    Subscribe to live telemetry/events from the ws-gateway WebSocket.

    The ``from_actor`` parameter on :meth:`receive` filters frames by their
    ``sender_type:sender_id`` (set when devices use ``to=`` routing).
    """

    def __init__(
        self,
        api_key:    str,
        project:    str,
        *,
        apikey_url: str,
        ws_url:     str,
        fleet_api:  str | None = None,
    ) -> None:
        self._api_key    = api_key
        self._project    = project
        self._apikey_url = apikey_url
        self._ws_url     = ws_url.rstrip("/")
        self._fleet_api  = (fleet_api or "").rstrip("/")

    def receive(
        self,
        *,
        device:     str | None       = None,
        streams:    list[str] | None = None,
        from_actor: str | None       = None,
    ) -> Generator[DataFrame, None, None]:
        """
        Blocking generator yielding real-time frames from the ws-gateway.

        :param device:     Device name to subscribe to (None = all in project).
        :param streams:    Stream types, e.g. ``["telemetry", "event"]``.
        :param from_actor: Only yield frames originating from this actor
                           (``"type:id"`` format, ``"device:*"`` for all devices).
        """
        try:
            import websocket  # type: ignore[import]
        except ImportError:
            raise ImportError(
                "websocket-client is required for subscribe(). "
                "Install with: pip install websocket-client"
            )

        vehicle_ids = self._resolve_vehicle_ids(device)
        if not vehicle_ids:
            logger.warning("No devices found for project=%s device=%s", self._project, device)
            return

        sub_msg = json.dumps({
            "vehicle_ids": vehicle_ids,
            "streams":     streams or ["telemetry", "event"],
            "format":      "json",
        })

        frame_q: queue.Queue = queue.Queue(maxsize=4096)

        def on_message(ws: Any, message: Any) -> None:
            if not frame_q.full():
                frame_q.put_nowait(message)

        token  = exchange_token(self._api_key, self._apikey_url)
        ws_url = f"{self._ws_url}?token={urllib.parse.quote(token)}"
        ws     = websocket.WebSocketApp(
            ws_url,
            on_message=on_message,
            on_error=lambda ws, e: logger.warning("WS error: %s", e),
        )
        ws_thread = threading.Thread(
            target=ws.run_forever, kwargs={"ping_interval": 30}, daemon=True
        )
        ws_thread.start()
        time.sleep(0.5)
        ws.send(sub_msg)

        # Parse "type:id" from_actor filter
        filter_type: str | None = None
        filter_id:   str | None = None
        if from_actor:
            at, _, aid = from_actor.partition(":")
            filter_type = at  or None
            filter_id   = aid or None

        try:
            while True:
                try:
                    raw = frame_q.get(timeout=1.0)
                except queue.Empty:
                    continue
                if not isinstance(raw, str):
                    continue
                try:
                    data = json.loads(raw)
                except json.JSONDecodeError:
                    continue

                # Apply from_actor filter
                if filter_type:
                    if data.get("sender_type", "") != filter_type:
                        continue
                if filter_id and filter_id != "*":
                    if data.get("sender_id", "") != filter_id:
                        continue

                yield DataFrame(
                    device    = data.get("vehicle_id", data.get("device", "")),
                    stream    = data.get("type", "event"),
                    timestamp = str(data.get("ts", "")),
                    fields    = data,
                )
        finally:
            ws.close()

    # ── Internal helpers ──────────────────────────────────────────────────────

    def _resolve_vehicle_ids(self, device: str | None) -> list[str]:
        if not self._fleet_api:
            return []
        try:
            from ..core.transport import get_json
            token = exchange_token(self._api_key, self._apikey_url)
            resp  = get_json(
                f"{self._fleet_api}/v1/projects/{urllib.parse.quote(self._project)}/devices",
                headers={"Authorization": f"Bearer {token}"},
            )
            devices = resp.get("devices", [])
            if device:
                return [d["id"] for d in devices if d.get("display_name") == device]
            return [d["id"] for d in devices]
        except Exception as e:
            logger.warning("Failed to resolve vehicle IDs: %s", e)
            return []
