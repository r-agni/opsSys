"""
MessageReceiver — incoming command and assistance-response listener.

Maintains a persistent SSE connection to the edge agent's
``GET /v1/commands`` endpoint.  Dispatches incoming frames either to a
registered handler function (decorator style) or to an internal queue
(polling style via :meth:`next_command` / :meth:`listen`).

Standalone usage::

    from systemscale.message import MessageReceiver

    inbox = MessageReceiver(agent_api="http://127.0.0.1:7777")
    inbox.start()

    # Blocking poll
    cmd = inbox.next_command(timeout=30.0)
    if cmd:
        cmd.ack()

    # Or decorator
    @inbox.on_command
    def handle(cmd):
        print(cmd.type, cmd.data)
        cmd.ack()
"""

from __future__ import annotations

import json
import logging
import queue
import random
import threading
import time
from dataclasses import dataclass
from typing import Any, Callable, Generator

logger = logging.getLogger("systemscale")

_STOP = object()


@dataclass
class AssistanceResponse:
    """Response received when an operator responds to a device's assistance request."""
    approved:    bool
    instruction: str
    request_id:  str


class Command:
    """
    A command received from the cloud platform.

    Call :meth:`ack`, :meth:`reject`, or :meth:`fail` exactly once to
    signal the outcome back to the sender.
    """

    def __init__(
        self,
        id:        str,
        type:      str,
        data:      dict,
        priority:  str,
        _receiver: "MessageReceiver",
    ) -> None:
        self.id       = id
        self.type     = type
        self.data     = data
        self.priority = priority
        self._recv    = _receiver
        self._acked   = False

    def ack(self, message: str = "") -> None:
        """Signal successful execution."""
        self._send("completed", message)

    def reject(self, message: str = "") -> None:
        """Reject the command (unsupported, invalid state, etc.)."""
        self._send("rejected", message)

    def fail(self, message: str = "") -> None:
        """Signal that execution failed."""
        self._send("failed", message)

    def _send(self, status: str, message: str) -> None:
        if self._acked:
            logger.warning("Command %s already acked — ignoring duplicate", self.id)
            return
        self._acked = True
        self._recv._post_ack(self.id, status, message)


class MessageReceiver:
    """
    Listens for incoming commands and assistance responses via SSE.

    Exactly one background thread maintains a persistent SSE connection
    to the edge agent.  Frames are dispatched either to a registered
    handler (fire-and-forget thread per command) or to an internal
    queue for manual polling.
    """

    def __init__(self, agent_api: str = "http://127.0.0.1:7777") -> None:
        self._agent_api    = agent_api.rstrip("/")
        self._cmd_queue:   queue.Queue = queue.Queue(maxsize=256)
        self._handler:     Callable | None = None
        self._handler_lock = threading.Lock()
        self._thread:      threading.Thread | None = None
        self._stopped      = False
        self._agentless:   bool = False

        # Pending assistance requests: request_id → (Event, result_list)
        self._pending:      dict[str, tuple[threading.Event, list]] = {}
        self._pending_lock  = threading.Lock()

    def set_agentless(self, value: bool) -> None:
        """Switch agentless mode on/off. Called by Client when agent availability changes."""
        self._agentless = value

    # ── Lifecycle ─────────────────────────────────────────────────────────────

    def start(self) -> None:
        """Start the background SSE listener thread."""
        if self._thread is not None:
            return
        self._stopped = False
        self._thread  = threading.Thread(
            target=self._run_sse, name="ss-inbox", daemon=True
        )
        self._thread.start()

    def stop(self) -> None:
        """Stop the background thread (waits up to 5 s)."""
        if self._stopped:
            return
        self._stopped = True
        try:
            self._cmd_queue.put_nowait(_STOP)
        except queue.Full:
            pass
        if self._thread:
            self._thread.join(timeout=5.0)

    # ── Public API ────────────────────────────────────────────────────────────

    def on_command(self, fn: Callable) -> Callable:
        """Register *fn* as the command handler (can also be used as a decorator)."""
        with self._handler_lock:
            self._handler = fn
        return fn

    def next_command(self, timeout: float = 0.0) -> Command | None:
        """
        Block until the next command arrives, then return it.

        :param timeout: Seconds to wait; ``0`` blocks forever; negative → no wait.
        :returns: :class:`Command` or ``None`` if the receiver was stopped.
        """
        try:
            item = self._cmd_queue.get(
                timeout=timeout if timeout > 0 else (None if timeout == 0 else 0.001)
            )
            if item is _STOP:
                return None
            return item
        except queue.Empty:
            return None

    def listen(self) -> Generator[Command, None, None]:
        """Generator that yields commands as they arrive."""
        while not self._stopped:
            cmd = self.next_command(timeout=1.0)
            if cmd is not None:
                yield cmd

    # ── Assistance-request coordination ───────────────────────────────────────

    def register_assistance_waiter(
        self, request_id: str, event: threading.Event, result: list
    ) -> None:
        with self._pending_lock:
            self._pending[request_id] = (event, result)

    def unregister_assistance_waiter(self, request_id: str) -> None:
        with self._pending_lock:
            self._pending.pop(request_id, None)

    # ── Internal ACK posting ──────────────────────────────────────────────────

    def _post_ack(self, command_id: str, status: str, message: str) -> None:
        import urllib.parse
        from ..core.transport import post_with_retry
        url  = f"{self._agent_api}/v1/commands/{urllib.parse.quote(command_id)}/ack"
        body = json.dumps({"status": status, "message": message}).encode()
        if not post_with_retry(url, body):
            logger.warning("Failed to deliver ACK for command %s", command_id)

    # ── SSE listener ──────────────────────────────────────────────────────────

    def _run_sse(self) -> None:
        import urllib.request
        url     = f"{self._agent_api}/v1/commands"
        backoff = 0.25
        while not self._stopped:
            if self._agentless:
                time.sleep(5.0)
                continue
            try:
                req = urllib.request.Request(
                    url,
                    headers={
                        "Accept":        "text/event-stream",
                        "Cache-Control": "no-cache",
                    },
                )
                with urllib.request.urlopen(req, timeout=None) as resp:
                    logger.info("SSE command stream connected")
                    backoff = 0.25
                    self._consume_sse(resp)
            except Exception as e:
                if self._stopped:
                    break
                jittered = backoff + random.uniform(0, backoff * 0.5)
                logger.warning("SSE disconnected (%s) — reconnecting in %.1fs", e, jittered)
                time.sleep(jittered)
                backoff = min(backoff * 2, 10.0)

    def _consume_sse(self, resp: Any) -> None:
        event_data: list[str] = []
        for raw_line in resp:
            if self._stopped:
                return
            line = raw_line.decode("utf-8", errors="replace").rstrip("\r\n")
            if line.startswith("data:"):
                event_data.append(line[5:].lstrip())
            elif line == "" and event_data:
                full_data  = "\n".join(event_data)
                event_data = []
                try:
                    self._dispatch(json.loads(full_data))
                except (json.JSONDecodeError, KeyError) as e:
                    logger.warning("Malformed SSE event: %s", e)

    def _dispatch(self, payload: dict) -> None:
        cmd_type = payload.get("command_type", payload.get("type", "unknown"))

        # ── Assistance response (intercepted, not forwarded to user handlers) ──
        if cmd_type == "_assistance_response":
            try:
                data = payload.get("data") or {}
                if isinstance(data, str):
                    data = json.loads(data)
                request_id  = data.get("_request_id", "")
                approved    = bool(data.get("approved", False))
                instruction = str(data.get("instruction", ""))
                response    = AssistanceResponse(
                    approved=approved, instruction=instruction, request_id=request_id
                )
                with self._pending_lock:
                    pending = self._pending.get(request_id)
                if pending:
                    event, result = pending
                    result.append(response)
                    event.set()
            except Exception as e:
                logger.warning("Failed to parse assistance response: %s", e)
            return

        # ── Regular command ────────────────────────────────────────────────────
        try:
            data = payload.get("data") or {}
            if isinstance(data, str):
                try:
                    data = json.loads(data)
                except json.JSONDecodeError:
                    data = {}
            cmd = Command(
                id       = payload["id"],
                type     = cmd_type,
                data     = data,
                priority = payload.get("priority", "normal"),
                _receiver = self,
            )
        except KeyError as e:
            logger.warning("Command event missing field %s — skipping", e)
            return

        with self._handler_lock:
            handler = self._handler

        if handler is not None:
            t = threading.Thread(
                target=self._call_handler, args=(handler, cmd), daemon=True
            )
            t.start()
        else:
            try:
                self._cmd_queue.put_nowait(cmd)
            except queue.Full:
                logger.warning("Command queue full — dropping command %s", cmd.id)
                cmd.fail("command queue full on device")

    def _call_handler(self, handler: Callable, cmd: Command) -> None:
        try:
            handler(cmd)
        except Exception as e:
            logger.exception("Command handler raised: %s", e)
        finally:
            if not cmd._acked:
                logger.warning(
                    "Handler for %s returned without ack/reject/fail — sending 'failed'",
                    cmd.id,
                )
                cmd.fail("handler returned without ACK")
