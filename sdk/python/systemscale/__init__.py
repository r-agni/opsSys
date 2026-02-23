"""
SystemScale SDK — telemetry, alerting, and command interface.

Service-side usage::

    import systemscale

    client = systemscale.init(
        api_key  = "ssk_live_...",
        project  = "my-fleet",
        service  = "drone-001",
        location = (28.61, 77.20, 102.3),  # optional default GPS
    )

    # Continuous time-series data (queued, background delivery)
    client.log({"sensors/temp": 85.2, "nav/altitude": 102.3})

    # Discrete event
    client.alert("Low battery", level="warning", data={"pct": 12})

    # Human-in-the-loop
    response = client.request_assistance("Obstacle detected", timeout=60)

    # Receive operator instructions
    @client.on_command
    def handle(cmd):
        if cmd.type == "goto":
            navigate(cmd.data["lat"], cmd.data["lon"])
            cmd.ack()

Cloud-side (operator) usage::

    ops = systemscale.connect(api_key="ssk_live_...", project="my-fleet")
    rows = ops.query(service="drone-001", keys=["sensors/temp"], start="-1h")
    ack  = ops.send_command("drone-001", cmd_type="goto", data={"lat": 28.6, "lon": 77.2})

    for req in ops.assistance_requests():
        req.approve("proceed")

    # Real-time subscription
    for frame in ops.subscribe(service="drone-001"):
        print(frame.fields)
"""

from __future__ import annotations

from .client   import Client, Command             # noqa: F401
from .operator import OperatorClient              # noqa: F401

# ── Module-level singleton (service side) ─────────────────────────────────────

_client: "Client | None" = None


def init(
    api_key: str,
    project: str,
    service: str | None = None,
    *,
    location:   tuple[float, float, float] | None = None,
    queue_size: int = 8192,
) -> "Client":
    """
    Initialise the SDK for a service.

    Minimal usage::

        client = systemscale.init(
            api_key = "ssk_live_...",
            project = "my-fleet",
            service = "drone-001",
        )

    With a default GPS location (used for every ``log()`` call unless overridden)::

        client = systemscale.init(
            api_key  = "ssk_live_...",
            project  = "my-fleet",
            service  = "drone-001",
            location = (28.61, 77.20, 102.3),   # (lat, lon, alt_m)
        )

    The SDK automatically detects whether the SystemScale edge agent is
    installed on this machine:

    - **Agent found** — data is buffered locally and streamed to the cloud
      via the agent's QUIC relay.  No URL configuration needed.
    - **Agent not found** — the SDK runs in *agentless mode*: telemetry
      frames queue in-process and are delivered once the agent starts.
      A single warning is logged; no repeated errors.

    **Self-hosting**: override cloud URLs via environment variables::

        SYSTEMSCALE_API_BASE=http://127.0.0.1:7777
        SYSTEMSCALE_FLEET_API=http://myserver:8080
        SYSTEMSCALE_APIKEY_URL=http://myserver:8083

    :param api_key:    SystemScale API key (``ssk_live_...``).
    :param project:    Project slug, e.g. ``"my-fleet"``.
    :param service:    Service name.  Defaults to ``$SYSTEMSCALE_SERVICE``
                       env var or the system hostname.
    :param location:   Default GPS coordinates ``(lat, lon, alt_m)`` attached
                       to every ``log()`` call that does not provide its own.
    :param queue_size: In-process log queue depth (frames beyond this are
                       dropped with a warning, never blocking the caller).
    :returns:          The initialised :class:`Client` singleton.
    """
    global _client
    _client = Client(
        api_key=api_key,
        project=project,
        service=service,
        location=location,
        queue_size=queue_size,
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
    :param command_api: Command REST API URL for sending commands to services.
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


# ── Module-level convenience wrappers ─────────────────────────────────────────

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
    """Send a non-blocking alert event (module-level shortcut)."""
    _require_client().alert(message, level=level, data=data, to=to)


def on_command(fn):
    """Decorator — register *fn* as the command handler (module-level shortcut)."""
    _require_client().set_command_handler(fn)
    return fn


def next_command(timeout: float = 0.0) -> "Command | None":
    """Poll for the next pending command (module-level shortcut)."""
    return _require_client().next_command(timeout=timeout)
