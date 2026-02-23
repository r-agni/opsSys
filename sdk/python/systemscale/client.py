"""
Service-side Client — thin facade over stream, message, and provision modules.

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
from .core.transport   import _resolve_url

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
        api_key:    str,
        project:    str,
        service:    str | None,
        location:   tuple[float, float, float] | None,
        queue_size: int,
    ) -> None:
        self._api_key          = api_key
        self._project          = project
        self._service          = service or os.environ.get("SYSTEMSCALE_SERVICE") or socket.gethostname()
        self._default_location = location
        self._api_base         = _resolve_url(None, "SYSTEMSCALE_API_BASE",   "http://127.0.0.1:7777")
        self._fleet_api        = _resolve_url(None, "SYSTEMSCALE_FLEET_API",  "https://api.systemscale.io")
        self._apikey_url       = _resolve_url(None, "SYSTEMSCALE_APIKEY_URL", "https://keys.systemscale.io")
        self._agentless        = False

        # Sub-modules exposed as public attributes
        self.stream  = StreamEmitter(
            api_key=api_key, project=project, actor=self._service,
            agent_api=self._api_base, queue_size=queue_size,
        )
        self.message = MessageSender(
            api_key=api_key, project=project, actor=self._service,
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

        agent_running = self._probe_agent()

        if not agent_running:
            if self._detect_agent_binary():
                try:
                    self._provision()
                    agent_running = True
                except Exception as e:
                    logger.error(
                        "Auto-provisioning failed: %s. Falling back to agentless mode.", e
                    )
            else:
                logger.warning(
                    "systemscale-agent binary not found. Running in agentless mode. "
                    "Frames will queue locally until the agent becomes available. "
                    "Install the agent to enable full telemetry relay."
                )

        if not agent_running:
            self._agentless = True
            self.stream.set_agentless(True)
            self.message.set_agentless(True)
            self.inbox.set_agentless(True)

        self.stream.start()
        self.inbox.start()
        logger.info(
            "SystemScale SDK started (project=%s, service=%s, mode=%s)",
            self._project, self._service,
            "agentless" if self._agentless else "agent",
        )

        if self._agentless:
            self._start_agent_retry()

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
        if self._default_location is not None:
            if lat == 0.0:
                lat = self._default_location[0]
            if lon == 0.0:
                lon = self._default_location[1]
            if alt == 0.0:
                alt = self._default_location[2]
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

    @staticmethod
    def _detect_agent_binary() -> bool:
        """Return True if the systemscale-agent binary is present and executable."""
        import shutil
        explicit = os.environ.get("SYSTEMSCALE_AGENT_BIN", "").strip()
        if explicit:
            return os.path.isfile(explicit) and os.access(explicit, os.X_OK)
        if shutil.which("systemscale-agent") is not None:
            return True
        for path in (
            "/usr/local/bin/systemscale-agent",
            os.path.expanduser("~/.local/bin/systemscale-agent"),
            "/usr/bin/systemscale-agent",
        ):
            if os.path.isfile(path) and os.access(path, os.X_OK):
                return True
        return False

    def _provision(self) -> None:
        from .provisioning import provision
        provision(
            api_key=self._api_key, project=self._project, service=self._service,
            apikey_url=self._apikey_url, fleet_api=self._fleet_api,
            agent_api=self._api_base,
        )

    def _start_agent_retry(self) -> None:
        """Background thread: probe the agent every 30 s and switch out of agentless mode when found."""
        import time

        def _retry() -> None:
            while not self._stopped:
                time.sleep(30)
                if self._stopped:
                    break
                if self._probe_agent():
                    logger.info(
                        "Edge agent now available at %s — switching to agent mode.",
                        self._api_base,
                    )
                    self._agentless = False
                    self.stream.set_agentless(False)
                    self.message.set_agentless(False)
                    self.inbox.set_agentless(False)
                    break

        threading.Thread(target=_retry, name="ss-agent-retry", daemon=True).start()
