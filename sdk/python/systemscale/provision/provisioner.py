"""
Device auto-provisioning.

Exchanges an API key for a device certificate + agent config, writes
config files to disk, and starts the edge agent process.

Re-exported from :mod:`systemscale.provisioning` for standalone use::

    from systemscale.provision import provision

    provision(
        api_key    = "ssk_live_...",
        project    = "my-fleet",
        device     = "drone-001",
        apikey_url = "https://keys.systemscale.io",
        fleet_api  = "https://api.systemscale.io",
        agent_api  = "http://127.0.0.1:7777",
    )
"""

from ..provisioning import provision  # noqa: F401

__all__ = ["provision"]
