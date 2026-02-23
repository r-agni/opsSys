"""
SystemScale SDK — W&B-style telemetry, alerting, and command interface.

Device-side usage::

    import systemscale

    client = systemscale.init(api_key="ssk_live_...", project="my-fleet", device="drone-001")
    client.log({"sensors/temp": 85.2, "nav/altitude": 102.3})
    client.alert("Low battery", level="warning", data={"pct": 12})
    response = client.request_assistance("Obstacle detected", timeout=60)

    @client.on_command
    def handle(cmd):
        cmd.ack()

    # Route data to a specific actor (new in v0.2)
    client.log({"nav/alt": 102.3}, to="operator:ground-control")
    client.stream.send({"lat": 28.6}, to="operator:*")

Cloud-side (operator) usage::

    ops = systemscale.connect(api_key="ssk_live_...", project="my-fleet")
    rows = ops.query(device="drone-001", keys=["sensors/temp"], start="-1h")
    ack  = ops.send_command("drone-001", cmd_type="goto", data={"lat": 28.6, "lon": 77.2})

    for req in ops.assistance_requests():
        req.approve("proceed")

    # Real-time subscription
    for frame in ops.subscribe(device="drone-001"):
        print(frame.fields)

    # Standalone module usage (new in v0.2)
    from systemscale.stream import StreamEmitter
    emitter = StreamEmitter(api_key=KEY, project="my-fleet", actor="drone-001")
    emitter.start()
    emitter.send({"lat": 28.6}, to="operator:*")
"""

from __future__ import annotations

from .client   import Client, Command             # noqa: F401
from .operator import OperatorClient              # noqa: F401

# ── Module-level singleton (device side) ──────────────────────────────────────

_client: "Client | None" = None


def init(
    api_key: str,
    project: str,
    device:  str | None = None,
    *,
    api_base:       str  = "http://127.0.0.1:7777",
    fleet_api:      str  = "https://api.systemscale.io",
    apikey_url:     str  = "https://keys.systemscale.io",
    queue_size:     int  = 8192,
    auto_provision: bool = True,
) -> "Client":
    """
    Initialise the SDK for a device.

    The SDK probes the local edge agent (``localhost:7777/healthz``).  If the
    agent is running the SDK uses it directly.  If not — and *auto_provision*
    is True — the SDK provisions the device (writes config, starts the agent).

    :param api_key:        SystemScale API key (``ssk_live_...``).
    :param project:        Project name (slug), e.g. ``"my-fleet"``.
    :param device:         Device / service name, e.g. ``"drone-001"``.
    :param api_base:       Local agent API base URL (default ``http://127.0.0.1:7777``).
    :param fleet_api:      Fleet API base URL (for provisioning).
    :param apikey_url:     API-key service URL (for token exchange).
    :param queue_size:     Background log queue depth.
    :param auto_provision: Attempt to install and start the agent if not running.
    :returns:              The initialised :class:`Client` singleton.
    """
    global _client
    _client = Client(
        api_key=api_key,
        project=project,
        device=device,
        api_base=api_base,
        fleet_api=fleet_api,
        apikey_url=apikey_url,
        queue_size=queue_size,
        auto_provision=auto_provision,
    )
    _client.start()
    return _client


def connect(
    api_key: str,
    project: str,
    *,
    fleet_api:   str = "https://api.systemscale.io",
    apikey_url:  str = "https://keys.systemscale.io",
    ws_url:      str = "wss://ws.systemscale.io",
    command_api: str = "https://cmd.systemscale.io",
) -> "OperatorClient":
    """
    Connect to the cloud platform as an operator (cloud side).

    :param api_key:     SystemScale API key (``ssk_live_...``).
    :param project:     Project name to scope queries and subscriptions.
    :param fleet_api:   Fleet API base URL.
    :param apikey_url:  API-key service URL (for token exchange).
    :param ws_url:      WebSocket gateway URL for real-time subscriptions.
    :param command_api: Command REST API URL for sending commands to devices.
    :returns:           An :class:`OperatorClient`.
    """
    return OperatorClient(
        api_key=api_key,
        project=project,
        fleet_api=fleet_api,
        apikey_url=apikey_url,
        ws_url=ws_url,
        command_api=command_api,
    )


# ── Module-level convenience wrappers (backwards-compatible) ──────────────────

def _require_client() -> "Client":
    if _client is None:
        raise RuntimeError(
            "systemscale.init() must be called before any other SDK function."
        )
    return _client


def log(
    data: dict,
    *,
    stream:      str        = "telemetry",
    stream_name: str        = "",
    lat:         float      = 0.0,
    lon:         float      = 0.0,
    alt:         float      = 0.0,
    tags:        dict | None = None,
    to:          str | None  = None,
) -> None:
    """Log a dict of key-value pairs to the platform (module-level shortcut)."""
    _require_client().log(
        data, stream=stream, stream_name=stream_name,
        lat=lat, lon=lon, alt=alt, tags=tags, to=to,
    )


def alert(
    message: str,
    *,
    level: str        = "info",
    data:  dict | None = None,
    to:    str | None  = None,
) -> None:
    """Send a non-blocking alert to the operator (module-level shortcut)."""
    _require_client().alert(message, level=level, data=data, to=to)


def on_command(fn):
    """Decorator — register *fn* as the command handler (module-level shortcut)."""
    _require_client().set_command_handler(fn)
    return fn


def next_command(timeout: float = 0.0) -> "Command | None":
    """Poll for the next pending command (module-level shortcut)."""
    return _require_client().next_command(timeout=timeout)
