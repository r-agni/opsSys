"""
Auto-provisioning: exchange an API key for a device certificate + agent config,
write config to disk, and start the edge agent process.

Not part of the public API; called internally by Client.start() when no local
agent is detected.
"""

from __future__ import annotations

import json
import logging
import os
import subprocess
import time
import urllib.error
import urllib.request

logger = logging.getLogger("systemscale.provisioning")

# Default paths — can be overridden by environment variables
_CFG_DIR   = os.environ.get("SYSTEMSCALE_CONFIG_DIR", "/etc/systemscale")
_CERT_DIR  = os.environ.get("SYSTEMSCALE_CERT_DIR",   "/etc/systemscale/certs")
_AGENT_BIN = os.environ.get("SYSTEMSCALE_AGENT_BIN",  "systemscale-agent")


def provision(
    *,
    api_key:   str,
    project:   str,
    device:    str,
    apikey_url: str,
    fleet_api:  str,
    agent_api:  str,
) -> None:
    """
    Provision a new device and start the edge agent.

    Steps:
    1. Exchange API key → JWT via apikey-service
    2. Call fleet-api POST /v1/projects/{project}/devices → provisioning bundle
    3. Write agent.yaml + TLS certs to /etc/systemscale/ (or $SYSTEMSCALE_CONFIG_DIR)
    4. Spawn the edge agent process
    5. Poll agent /healthz until ready (30s timeout)

    :raises RuntimeError: If any step fails.
    """
    logger.info("Provisioning device '%s' in project '%s'", device, project)

    # ── Step 1: Exchange API key for JWT ──────────────────────────────────────
    jwt = _exchange_token(api_key, apikey_url)
    logger.info("Token obtained from apikey-service")

    # ── Step 2: Call fleet-api to provision device ────────────────────────────
    bundle = _provision_device(jwt, project, device, fleet_api)
    logger.info("Device provisioned: id=%s", bundle.get("device_id"))

    # ── Step 3: Write config and certs ────────────────────────────────────────
    _write_config(bundle)
    logger.info("Agent config written to %s", _CFG_DIR)

    # ── Step 4: Start the agent ───────────────────────────────────────────────
    _start_agent()
    logger.info("Edge agent process started")

    # ── Step 5: Poll /healthz until ready ────────────────────────────────────
    _wait_for_agent(agent_api, timeout=30.0)
    logger.info("Edge agent is healthy and ready")


def _exchange_token(api_key: str, apikey_url: str) -> str:
    url  = f"{apikey_url.rstrip('/')}/v1/token"
    body = json.dumps({"api_key": api_key}).encode()
    req  = urllib.request.Request(
        url, data=body, method="POST",
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=10.0) as resp:
            data = json.loads(resp.read())
    except urllib.error.HTTPError as e:
        raise RuntimeError(
            f"API key exchange failed (HTTP {e.code}): {e.read().decode()}"
        ) from e
    return data["token"]


def _provision_device(jwt: str, project: str, device: str, fleet_api: str) -> dict:
    import urllib.parse
    url  = f"{fleet_api.rstrip('/')}/v1/projects/{urllib.parse.quote(project)}/devices"
    body = json.dumps({"display_name": device}).encode()
    req  = urllib.request.Request(
        url, data=body, method="POST",
        headers={
            "Content-Type":  "application/json",
            "Authorization": f"Bearer {jwt}",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=15.0) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        raise RuntimeError(
            f"Device provisioning failed (HTTP {e.code}): {e.read().decode()}"
        ) from e


def _write_config(bundle: dict) -> None:
    """
    Write the provisioning bundle (agent YAML + certs) to disk.

    Bundle keys (from fleet-api provisionDevice):
    - agent_config_yaml: the full agent.yaml content
    - cert_pem:          device TLS certificate (PEM)
    - key_pem:           device private key (PEM)
    - ca_pem:            CA certificate (PEM)
    """
    os.makedirs(_CFG_DIR, mode=0o755, exist_ok=True)
    os.makedirs(_CERT_DIR, mode=0o700, exist_ok=True)

    # Write agent config
    cfg_path = os.path.join(_CFG_DIR, "agent.yaml")
    with open(cfg_path, "w") as f:
        f.write(bundle.get("agent_config_yaml", ""))

    # Write TLS certs (sensitive — owner-only read)
    for filename, key in [
        ("device.crt", "cert_pem"),
        ("device.key", "key_pem"),
        ("ca.crt",     "ca_pem"),
    ]:
        pem = bundle.get(key, "")
        if pem:
            path = os.path.join(_CERT_DIR, filename)
            with open(path, "w") as f:
                f.write(pem)
            os.chmod(path, 0o600)


def _start_agent() -> None:
    """Launch the edge agent as a background process."""
    cfg_path = os.path.join(_CFG_DIR, "agent.yaml")
    env = {**os.environ, "SYSTEMSCALE_AGENT_CONFIG": cfg_path}
    try:
        subprocess.Popen(
            [_AGENT_BIN],
            env=env,
            start_new_session=True,  # detach from parent process group
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except FileNotFoundError:
        raise RuntimeError(
            f"Edge agent binary '{_AGENT_BIN}' not found. "
            "Install the SystemScale agent package first: "
            "pip install systemscale-agent  OR  apt install systemscale-agent"
        )


def _wait_for_agent(agent_api: str, timeout: float = 30.0) -> None:
    """Poll the agent's /healthz endpoint until it responds 200."""
    url      = f"{agent_api.rstrip('/')}/healthz"
    deadline = time.monotonic() + timeout
    delay    = 0.25

    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=1.0) as resp:
                if resp.status == 200:
                    return
        except Exception:
            pass
        time.sleep(delay)
        delay = min(delay * 1.5, 2.0)

    raise RuntimeError(
        f"Edge agent did not become healthy within {timeout}s. "
        f"Check logs at /var/log/systemscale-agent.log"
    )
