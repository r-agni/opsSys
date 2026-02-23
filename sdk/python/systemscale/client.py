"""
Device-side Client — thin facade over stream, message, and provision modules.

Not part of the public API; import via ``systemscale.init()``.
"""

from __future__ import annotations

import logging
import os
import socket
import threading
import uuid
from typing import Callable

from .stream.emit      import StreamEmitter
from .message.send     import MessageSender
from .message.receive  import MessageReceiver, Command, AssistanceResponse  # noqa: F401

logger = logging.getLogger("systemscale")


class Client:
    """
    Device-side SDK client.

    Composed of three sub-modules:

    - ``stream``  (:class:`~systemscale.stream.emit.StreamEmitter`):
      non-blocking high-frequency telemetry queue.
    - ``message`` (:class:`~systemscale.message.send.MessageSender`):
      discrete alerts/events sent directly to the edge agent.
    - ``inbox``   (:class:`~systemscale.message.receive.MessageReceiver`):
      SSE command listener with handler and poll interfaces.

    All existing method signatures (``log``, ``alert``, ``on_command``,
    ``next_command``, ``request_assistance``) are preserved for
    backward compatibility.
    """

    def __init__(
        self,
        api_key:        str,
        project:        str,
        device:         str | None,
        api_base:       str,
        fleet_api:      str,
        apikey_url:     str,
        queue_size:     int,
        auto_provision: bool,
    ) -> None:
        self._api_key        = api_key
        self._project        = project
        self._device         = device or os.environ.get("SYSTEMSCALE_DEVICE") or socket.gethostname()
        self._api_base       = api_base.rstrip("/")
        self._fleet_api      = fleet_api.rstrip("/")
        self._apikey_url     = apikey_url.rstrip("/")
        self._auto_provision = auto_provision

        # Sub-modules exposed as public attributes
        self.stream  = StreamEmitter(
            api_key=api_key, project=project, actor=self._device,
            agent_api=self._api_base, queue_size=queue_size,
        )
        self.message = MessageSender(
            api_key=api_key, project=project, actor=self._device,
            agent_api=self._api_base,
        )
        self.inbox   = MessageReceiver(agent_api=self._api_base)

        self._started = False
        self._stopped = False

    # ── Lifecycle ─────────────────────────────────────────────────────────────

    def start(self) -> None:
        """Probe / provision the local agent, then start background threads."""
        if self._started:
            return
        self._started = True

        if not self._probe_agent():
            if self._auto_provision:
                self._provision()
            else:
                logger.warning(
                    "Edge agent not found at %s and auto_provision=False. "
                    "Logs will queue until the agent starts.",
                    self._api_base,
                )

        self.stream.start()
        self.inbox.start()
        logger.info(
            "SystemScale SDK started (project=%s, device=%s)",
            self._project, self._device,
        )

    def stop(self) -> None:
        """Gracefully stop background threads and flush the log queue."""
        if self._stopped:
            return
        self._stopped = True
        self.stream.stop()
        self.inbox.stop()

    # ── Backward-compatible device-side API ───────────────────────────────────

    def log(
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
        Log a dict of key-value pairs to the platform (non-blocking).

        :param to: Optional target actor ``"type:id"`` (e.g. ``"operator:*"``).
        """
        self.stream.send(
            data, stream=stream, stream_name=stream_name,
            lat=lat, lon=lon, alt=alt, tags=tags, to=to,
        )

    def alert(
        self,
        message: str,
        *,
        level: str        = "info",
        data:  dict | None = None,
        to:    str | None = None,
    ) -> None:
        """
        Send a non-blocking alert event to the platform.

        :param to: Optional target actor ``"type:id"``.
        """
        self.message.send(message, type="alert", level=level, data=data, to=to)

    def request_assistance(
        self,
        reason:  str,
        *,
        data:    dict | None  = None,
        timeout: float | None = None,
    ) -> AssistanceResponse:
        """
        Request human assistance and block until an operator responds.

        :raises TimeoutError: If *timeout* elapses with no operator response.
        """
        request_id = str(uuid.uuid4())
        event      = threading.Event()
        result:    list[AssistanceResponse] = []

        self.inbox.register_assistance_waiter(request_id, event, result)

        payload: dict = {
            "_event_type": 102,  # EVENT_TYPE_ASSISTANCE_REQ
            "_request_id": request_id,
            "reason":       reason,
        }
        if data:
            payload.update(data)
        self.stream.send(payload, stream="event")

        resolved = event.wait(timeout=timeout)
        self.inbox.unregister_assistance_waiter(request_id)

        if not resolved:
            raise TimeoutError(
                f"No operator response within {timeout}s for request {request_id}"
            )
        return result[0]

    def set_command_handler(self, fn: Callable) -> None:
        """Register *fn* as the command handler (imperative style)."""
        self.inbox.on_command(fn)

    def on_command(self, fn: Callable) -> Callable:
        """Decorator — register *fn* as the command handler."""
        self.inbox.on_command(fn)
        return fn

    def next_command(self, timeout: float = 0.0) -> Command | None:
        """Block until the next command arrives, then return it."""
        return self.inbox.next_command(timeout)

    # ── Agent probe / provisioning ────────────────────────────────────────────

    def _probe_agent(self) -> bool:
        import urllib.request
        try:
            with urllib.request.urlopen(
                f"{self._api_base}/healthz", timeout=1.0
            ) as resp:
                return resp.status == 200
        except Exception:
            return False

    def _provision(self) -> None:
        try:
            from .provisioning import provision
            provision(
                api_key=self._api_key, project=self._project, device=self._device,
                apikey_url=self._apikey_url, fleet_api=self._fleet_api,
                agent_api=self._api_base,
            )
        except Exception as e:
            logger.error("Auto-provisioning failed: %s", e)
