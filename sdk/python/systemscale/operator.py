"""
Cloud-side OperatorClient — thin facade over stream.subscribe, message.send.

Not part of the public API; import via ``systemscale.connect()``.
"""

from __future__ import annotations

import logging
import urllib.parse
from typing import Any, Generator

from .stream.subscribe import StreamSubscriber, DataFrame         # noqa: F401
from .message.send     import MessageSender, CommandResult        # noqa: F401
from .core.auth        import exchange_token
from .core.transport   import get_json, post_json

logger = logging.getLogger("systemscale")


class AssistanceRequest:
    """
    An assistance request sent by a device.

    Received via :meth:`OperatorClient.assistance_requests`.
    Call :meth:`approve` or :meth:`deny` to respond.
    """

    def __init__(
        self,
        request_id: str,
        service:    str,
        reason:     str,
        *,
        _client: "OperatorClient",
    ) -> None:
        self.request_id = request_id
        self.service    = service
        self.reason     = reason
        self._client    = _client
        self._responded = False

    def approve(self, instruction: str = "") -> None:
        """Approve the request and optionally provide instructions to the service."""
        self._respond(approved=True, instruction=instruction)

    def deny(self, reason: str = "") -> None:
        """Deny the request with an optional explanation."""
        self._respond(approved=False, instruction=reason)

    def _respond(self, *, approved: bool, instruction: str) -> None:
        if self._responded:
            logger.warning("Assistance request %s already responded to", self.request_id)
            return
        self._responded = True
        self._client._post_assistance_response(
            request_id=self.request_id,
            approved=approved,
            instruction=instruction,
        )


class DataRow:
    """A single row returned by :meth:`OperatorClient.query`."""

    def __init__(self, service: str, timestamp: str, fields: dict[str, Any]) -> None:
        self.service   = service
        self.timestamp = timestamp
        self.fields    = fields


class OperatorClient:
    """
    Cloud-side (operator) SDK client.

    Composed of two sub-modules:

    - ``stream``  (:class:`~systemscale.stream.subscribe.StreamSubscriber`):
      real-time WebSocket telemetry subscription.
    - ``message`` (:class:`~systemscale.message.send.MessageSender`):
      discrete command sending via the cloud REST API.

    All existing method signatures (``query``, ``subscribe``,
    ``send_command``, ``assistance_requests``, ``services``) are
    preserved for backward compatibility.
    """

    def __init__(
        self,
        api_key:     str,
        project:     str,
        fleet_api:   str,
        apikey_url:  str,
        ws_url:      str,
        command_api: str = "https://cmd.systemscale.io",
    ) -> None:
        self._api_key     = api_key
        self._project     = project
        self._fleet_api   = fleet_api.rstrip("/")
        self._apikey_url  = apikey_url.rstrip("/")
        self._ws_url      = ws_url.rstrip("/")
        self._command_api = command_api.rstrip("/")

        # Sub-modules exposed as public attributes
        self.stream = StreamSubscriber(
            api_key=api_key, project=project,
            apikey_url=apikey_url, ws_url=ws_url, fleet_api=fleet_api,
        )
        self.message = MessageSender(
            api_key=api_key, project=project,
            command_api=command_api, apikey_url=apikey_url, fleet_api=fleet_api,
        )

    # ── Backward-compatible operator API ──────────────────────────────────────

    def services(self) -> list[dict]:
        """List all services (devices) in the project."""
        resp = get_json(
            f"{self._fleet_api}/v1/projects/{urllib.parse.quote(self._project)}/devices",
            headers=self._auth_headers(),
        )
        return resp.get("devices", [])

    def query(
        self,
        *,
        service:   str | None       = None,
        keys:      list[str] | None = None,
        start:     str              = "-1h",
        end:       str | None       = None,
        sample_by: str | None       = None,
        limit:     int              = 1000,
    ) -> list[DataRow]:
        """
        Query historical telemetry from QuestDB.

        :param service:   Service name to filter (None = all in project).
        :param keys:      Hierarchical keys to return, e.g. ``["sensors/temp"]``.
        :param start:     QuestDB time expression, e.g. ``"-1h"``, ``"2024-01-01"``.
        :param end:       End time (defaults to now).
        :param sample_by: QuestDB SAMPLE BY expression, e.g. ``"1m"``.
        :param limit:     Maximum rows to return.
        """
        params: dict = {
            "project": self._project,
            "start":   start,
            "limit":   str(limit),
        }
        if service:   params["device"]    = service
        if end:       params["end"]       = end
        if sample_by: params["sample_by"] = sample_by
        if keys:      params["keys"]      = ",".join(k.replace("/", "__") for k in keys)

        resp = get_json(f"{self._fleet_api}/v1/query", self._auth_headers(), params)
        rows = []
        for r in resp.get("rows", []):
            fields = {
                k.replace("__", "/"): v
                for k, v in r.items()
                if k not in ("device_id", "timestamp", "stream_type")
            }
            rows.append(DataRow(
                service   = r.get("device_id", ""),
                timestamp = r.get("timestamp", ""),
                fields    = fields,
            ))
        return rows

    def subscribe(
        self,
        *,
        service: str | None       = None,
        streams: list[str] | None = None,
    ) -> Generator[DataFrame, None, None]:
        """
        Subscribe to real-time telemetry via WebSocket.

        :param service: Service name (None = all in project).
        :param streams: Stream types, e.g. ``["telemetry", "event"]``.
        """
        return self.stream.receive(service=service, streams=streams)

    def send_command(
        self,
        service:  str,
        *,
        cmd_type: str,
        data:     dict | None = None,
        priority: str         = "normal",
        timeout:  float       = 30.0,
    ) -> CommandResult:
        """
        Send a command to a service and wait for its ACK.

        :param service:  Target service name.
        :param cmd_type: Command type string (e.g. ``"goto"``).
        :param data:     Arbitrary command payload.
        :param priority: ``"normal"`` | ``"high"`` | ``"emergency"``.
        :param timeout:  Seconds to wait for ACK.
        """
        return self.message.send(
            cmd_type,
            to       = f"device:{service}",
            type     = "command",
            data     = data,
            priority = priority,
            timeout  = timeout,
        )

    def assistance_requests(self) -> Generator[AssistanceRequest, None, None]:
        """
        Blocking generator that yields incoming assistance requests from devices.

        Polls ``GET /v1/assistance`` every second and yields new pending requests.
        """
        import time
        seen: set[str] = set()
        while True:
            try:
                resp = get_json(
                    f"{self._fleet_api}/v1/assistance",
                    self._auth_headers(),
                    {"project": self._project},
                )
                for item in resp.get("requests", []):
                    rid = item.get("request_id", "")
                    if rid and rid not in seen and item.get("status") == "pending":
                        seen.add(rid)
                        yield AssistanceRequest(
                            request_id = rid,
                            service    = item.get("device_id", ""),
                            reason     = item.get("reason", ""),
                            _client    = self,
                        )
            except Exception as e:
                logger.warning("assistance_requests poll error: %s", e)
            time.sleep(1.0)

    # ── Internal helpers ──────────────────────────────────────────────────────

    def _token(self) -> str:
        return exchange_token(self._api_key, self._apikey_url)

    def _auth_headers(self) -> dict:
        return {
            "Authorization": f"Bearer {self._token()}",
            "Content-Type":  "application/json",
        }

    def _post_assistance_response(
        self,
        *,
        request_id:  str,
        approved:    bool,
        instruction: str,
    ) -> None:
        try:
            post_json(
                f"{self._fleet_api}/v1/assistance"
                f"/{urllib.parse.quote(request_id)}/respond",
                {"approved": approved, "instruction": instruction},
                self._auth_headers(),
            )
        except Exception as e:
            logger.error("Failed to post assistance response: %s", e)
