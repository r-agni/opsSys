"""
MessageSender — discrete (event-driven) message and command sending.

Supports two modes:

**Device mode** (``agent_api`` set, no ``command_api``):
    Sends alerts and events through the local edge agent.
    The ``to`` parameter routes the message to a specific actor type.

**Operator mode** (``command_api`` + ``apikey_url`` set):
    Sends commands to devices via the cloud command REST API, then
    waits for an acknowledgement via SSE push (no polling).

Standalone usage::

    from systemscale.message import MessageSender

    # Device-side alert
    sender = MessageSender(api_key=KEY, project="my-fleet", actor="drone-001")
    sender.send("obstacle ahead", to="human:pilot-1", type="alert")

    # Operator-side command
    sender = MessageSender(
        api_key=KEY, project="my-fleet",
        command_api="https://cmd.systemscale.io",
        apikey_url="https://keys.systemscale.io",
        fleet_api="https://api.systemscale.io",
    )
    result = sender.send("goto", to="device:drone-001", type="command",
                         data={"lat": 28.6, "lon": 77.2})
"""

from __future__ import annotations

import http.client
import json
import logging
import urllib.parse
from dataclasses import dataclass
from typing import Any

logger = logging.getLogger("systemscale")


@dataclass
class CommandResult:
    """Outcome of a command sent via :meth:`MessageSender.send`."""
    command_id: str
    status:     str   # "completed" | "accepted" | "rejected" | "failed" | "timeout"
    message:    str


class MessageSender:
    """
    Discrete message / command sender.

    Construct with ``agent_api`` for device-side alerts, or with
    ``command_api`` + ``apikey_url`` + ``fleet_api`` for operator-side
    commands.  Both modes share the same :meth:`send` interface.
    """

    def __init__(
        self,
        api_key:     str,
        project:     str,
        actor:       str | None = None,
        *,
        agent_api:   str        = "http://127.0.0.1:7777",
        command_api: str | None = None,
        apikey_url:  str | None = None,
        fleet_api:   str | None = None,
    ) -> None:
        self._api_key     = api_key
        self._project     = project
        self._actor       = actor or ""
        self._agent_api   = agent_api.rstrip("/")
        self._command_api = (command_api or "").rstrip("/")
        self._apikey_url  = (apikey_url  or "").rstrip("/")
        self._fleet_api   = (fleet_api   or "").rstrip("/")

    def send(
        self,
        content:  str,
        *,
        to:       str | None = None,
        type:     str        = "alert",
        data:     dict | None = None,
        level:    str        = "info",
        priority: str        = "normal",
        timeout:  float      = 30.0,
    ) -> CommandResult | None:
        """
        Send a discrete message or command.

        :param content:  Alert message text (device mode) or command type
                         string (operator mode, e.g. ``"goto"``).
        :param to:       Target actor ``"type:id"`` (e.g. ``"operator:*"``,
                         ``"device:drone-002"``, ``"human:pilot-1"``).
                         ``None`` = broadcast to all actors in project.
        :param type:     ``"alert"`` | ``"event"`` | ``"command"``.
        :param data:     Extra payload included with the message.
        :param level:    Alert severity: ``"info"`` | ``"warning"`` | ``"error"``.
        :param priority: Command priority: ``"normal"`` | ``"high"`` | ``"emergency"``.
        :param timeout:  Seconds to wait for a command ACK (operator mode only).
        :returns:        :class:`CommandResult` for commands; ``None`` for alerts/events.
        """
        if self._command_api and type == "command":
            return self._send_command(content, to=to, data=data,
                                      priority=priority, timeout=timeout)
        self._send_event(content, to=to, type=type, level=level, data=data)
        return None

    # ── Device-side: alert/event via local agent ──────────────────────────────

    def _send_event(
        self,
        message: str,
        *,
        to:    str | None,
        type:  str,
        level: str,
        data:  dict | None,
    ) -> None:
        from ..core.transport import post_with_retry
        payload: dict[str, Any] = {
            "_event_type": 101,   # EVENT_TYPE_ALERT
            "level":       level,
            "message":     message,
        }
        if data:
            payload.update(data)
        body_dict: dict = {
            "data":       payload,
            "stream":     "event",
            "project_id": self._project,
            "lat":        0.0,
            "lon":        0.0,
            "alt":        0.0,
        }
        if to:
            body_dict["to"] = to
        if not post_with_retry(
            f"{self._agent_api}/v1/log",
            json.dumps(body_dict).encode(),
            max_retries=2,
        ):
            logger.warning("Failed to deliver %s event to local agent", type)

    # ── Operator-side: command via REST API ───────────────────────────────────

    def _send_command(
        self,
        command_type: str,
        *,
        to:       str | None,
        data:     dict | None,
        priority: str,
        timeout:  float,
    ) -> CommandResult:
        from ..core.auth      import exchange_token
        from ..core.transport import post_json

        token   = exchange_token(self._api_key, self._apikey_url)
        headers = {"Authorization": f"Bearer {token}"}

        # Resolve vehicle_id from "device:name" target
        vehicle_id = ""
        if to and to.startswith("device:"):
            device_name = to[len("device:"):]
            vehicle_id  = self._resolve_vehicle_id(device_name, token)

        resp = post_json(
            f"{self._command_api}/v1/commands",
            {
                "vehicle_id":   vehicle_id,
                "command_type": command_type,
                "data":         data or {},
                "priority":     priority,
                "ttl_ms":       int(timeout * 1000),
            },
            headers=headers,
        )
        command_id = resp.get("command_id", "")
        if not command_id:
            return CommandResult(command_id="", status="failed",
                                 message="server did not return a command_id")

        # Wait for ACK via SSE push — latency = actual command RTT, no polling.
        return self._wait_for_ack_sse(token, command_id, timeout)

    def _wait_for_ack_sse(self, token: str, command_id: str, timeout: float) -> CommandResult:
        """
        Open a persistent SSE connection to /v1/commands/{id}/stream and block
        until command-api pushes the ACK event.  No polling loop, no sleep().
        """
        parsed = urllib.parse.urlparse(self._command_api)
        path   = f"/v1/commands/{urllib.parse.quote(command_id)}/stream"
        hdrs   = {
            "Authorization": f"Bearer {token}",
            "Accept":        "text/event-stream",
            "Cache-Control": "no-cache",
        }

        # Deadline is command TTL + 5 s so the server's own timeout fires first.
        sock_timeout = timeout + 5.0
        if parsed.scheme == "https":
            conn: http.client.HTTPConnection | http.client.HTTPSConnection = (
                http.client.HTTPSConnection(parsed.netloc, timeout=sock_timeout))
        else:
            conn = http.client.HTTPConnection(parsed.netloc, timeout=sock_timeout)

        try:
            conn.request("GET", path, headers=hdrs)
            resp = conn.getresponse()

            while True:
                line_bytes = resp.readline(4096)
                if not line_bytes:
                    break
                line = line_bytes.decode("utf-8", errors="replace").rstrip("\r\n")
                if not line.startswith("data:"):
                    continue
                data = line[len("data:"):].strip()
                try:
                    ack    = json.loads(data)
                    status = ack.get("status", "")
                    if status:
                        return CommandResult(
                            command_id=command_id,
                            status=status,
                            message=ack.get("message", ""),
                        )
                except json.JSONDecodeError:
                    pass
        except Exception as exc:
            logger.warning("ACK SSE error for %s: %s", command_id, exc)
        finally:
            conn.close()  # always release the socket

        return CommandResult(command_id=command_id, status="timeout", message="")

    def _resolve_vehicle_id(self, device_name: str, token: str) -> str:
        if not self._fleet_api:
            return device_name
        from ..core.transport import get_json
        try:
            resp = get_json(
                f"{self._fleet_api}/v1/projects/{urllib.parse.quote(self._project)}/devices",
                headers={"Authorization": f"Bearer {token}"},
            )
            for d in resp.get("devices", []):
                if d.get("display_name") == device_name:
                    return d["id"]
        except Exception as e:
            logger.warning("Failed to resolve vehicle ID for '%s': %s", device_name, e)
        return device_name
