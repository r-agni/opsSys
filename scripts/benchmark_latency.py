#!/usr/bin/env python3
"""
benchmark_latency.py — measures real service latencies with HTTP keep-alive.

Previous benchmark inflated numbers by 10-15x because Python's urllib creates
a new TCP connection per request (~15ms handshake overhead).  This version uses
http.client with persistent connections to measure actual service response time.
"""

import http.client
import json
import os
import statistics
import sys
import time
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk", "python"))

KEYCLOAK_URL    = "http://localhost:8180"
KEYCLOAK_REALM  = "systemscale"
KEYCLOAK_CLIENT = "systemscale-dashboard"
KC_USERNAME     = "ragni.works@gmail.com"
KC_PASSWORD     = "Test1234!"
FLEET_API       = ("localhost", 8080)
APIKEY_API      = ("localhost", 8083)
COMMAND_API     = ("localhost", 8082)
QUERY_API       = ("localhost", 8081)
QUESTDB         = ("localhost", 9000)

DRONE_ALPHA_ID  = "2fe3a291-63a5-4feb-bb67-6dd7453ffb0d"


class KeepAliveClient:
    """HTTP client that reuses a single TCP connection (HTTP/1.1 keep-alive)."""

    def __init__(self, host: str, port: int):
        self.conn = http.client.HTTPConnection(host, port, timeout=15)

    def request(self, method: str, path: str, body=None, headers=None) -> tuple:
        h = {"Connection": "keep-alive"}
        if headers:
            h.update(headers)
        if body and isinstance(body, dict):
            body = json.dumps(body).encode()
            h["Content-Type"] = "application/json"
        try:
            self.conn.request(method, path, body=body, headers=h)
        except (ConnectionError, OSError):
            self.conn.close()
            self.conn.connect()
            self.conn.request(method, path, body=body, headers=h)
        resp = self.conn.getresponse()
        data = resp.read()
        try:
            return resp.status, json.loads(data)
        except Exception:
            return resp.status, data.decode()

    def timed(self, method: str, path: str, body=None, headers=None):
        t0 = time.perf_counter()
        status, data = self.request(method, path, body, headers)
        elapsed = (time.perf_counter() - t0) * 1000
        return status, data, elapsed

    def close(self):
        self.conn.close()


def percentile(data, p):
    if not data:
        return 0
    data.sort()
    n = len(data)
    k = (n - 1) * p / 100
    f = int(k)
    c = min(f + 1, n - 1)
    return data[f] + (k - f) * (data[c] - data[f])


def report(name, times_ms, target_p95=None):
    if not times_ms:
        print(f"  {name}: NO DATA")
        return
    times_ms.sort()
    p50 = percentile(times_ms, 50)
    p95 = percentile(times_ms, 95)
    p99 = percentile(times_ms, 99)
    avg = statistics.mean(times_ms)
    mn = min(times_ms)
    mx = max(times_ms)
    status = ""
    if target_p95 is not None:
        status = "  PASS" if p95 <= target_p95 else f"  MISS (target <{target_p95}ms)"
    print(f"  {name}:")
    print(f"    n={len(times_ms)}  min={mn:.2f}ms  avg={avg:.2f}ms  p50={p50:.2f}ms  p95={p95:.2f}ms  p99={p99:.2f}ms  max={mx:.2f}ms{status}")


print("=" * 70)
print("SystemScale Latency Benchmark (keep-alive connections)")
print("=" * 70)

fleet = KeepAliveClient(*FLEET_API)
apikey = KeepAliveClient(*APIKEY_API)
commands = KeepAliveClient(*COMMAND_API)
query = KeepAliveClient(*QUERY_API)
questdb = KeepAliveClient(*QUESTDB)

# Warm up all connections
fleet.request("GET", "/healthz")
apikey.request("GET", "/healthz")
commands.request("GET", "/healthz")
query.request("GET", "/healthz")

# ── 1. Auth latency ──────────────────────────────────────────────────────────
print("\n1. AUTH LATENCY")

kc_times = []
kc = KeepAliveClient("localhost", 8180)
form = urllib.parse.urlencode({
    "grant_type": "password", "client_id": KEYCLOAK_CLIENT,
    "username": KC_USERNAME, "password": KC_PASSWORD, "scope": "openid",
}).encode()
for _ in range(10):
    t0 = time.perf_counter()
    try:
        kc.conn.request("POST", f"/realms/{KEYCLOAK_REALM}/protocol/openid-connect/token",
                        body=form, headers={"Content-Type": "application/x-www-form-urlencoded",
                                            "Connection": "keep-alive"})
        resp = kc.conn.getresponse()
        data = json.loads(resp.read())
        kc_times.append((time.perf_counter() - t0) * 1000)
    except Exception:
        kc.conn.close()
        kc.conn.connect()
admin_jwt = data["access_token"]
report("Keycloak ROPC token (bcrypt — inherently slow)", kc_times)

key_times = []
exchange_times = []
api_key = ""
for i in range(10):
    st, resp, t = apikey.timed("POST", "/v1/keys",
                                {"name": f"bench-{i}-{int(time.time())}", "scopes": ["admin"]},
                                {"Authorization": f"Bearer {admin_jwt}"})
    if st == 201:
        key_times.append(t)
        api_key = resp["api_key"]

for _ in range(10):
    st, resp, t = apikey.timed("POST", "/v1/token", {"api_key": api_key})
    if st == 200:
        exchange_times.append(t)

report("API key creation (POST /v1/keys)", key_times, target_p95=10)
report("Token exchange (POST /v1/token)", exchange_times, target_p95=10)

st, resp, _ = apikey.timed("POST", "/v1/token", {"api_key": api_key})
sdk_jwt = resp["access_token"]
auth_h = {"Authorization": f"Bearer {sdk_jwt}"}

# ── 2. Fleet API latency ─────────────────────────────────────────────────────
print("\n2. FLEET API LATENCY")

projects_t = []
vehicles_t = []
devices_t = []
for _ in range(50):
    _, _, t = fleet.timed("GET", "/v1/projects", headers={"Authorization": f"Bearer {admin_jwt}"})
    projects_t.append(t)

for _ in range(50):
    _, _, t = fleet.timed("GET", "/v1/vehicles", headers={"Authorization": f"Bearer {admin_jwt}"})
    vehicles_t.append(t)

for _ in range(50):
    _, _, t = fleet.timed("GET", "/v1/projects/drone-fleet/devices", headers=auth_h)
    devices_t.append(t)

report("List projects (GET /v1/projects)", projects_t, target_p95=5)
report("List vehicles (GET /v1/vehicles)", vehicles_t, target_p95=5)
report("List project devices", devices_t, target_p95=5)

# ── 3. Command API latency ───────────────────────────────────────────────────
print("\n3. COMMAND API LATENCY")

cmd_post_t = []
cmd_get_t = []
for _ in range(20):
    st, resp, t = commands.timed("POST", "/v1/commands",
                                  {"vehicle_id": DRONE_ALPHA_ID, "command_type": "ping",
                                   "priority": 0, "ttl_ms": 1000}, headers=auth_h)
    if st == 202:
        cmd_post_t.append(t)
        cid = resp.get("command_id", "")
        if cid:
            _, _, t2 = commands.timed("GET", f"/v1/commands/{cid}", headers=auth_h)
            cmd_get_t.append(t2)

report("Send command (POST /v1/commands)", cmd_post_t, target_p95=10)
report("Get command status (GET /v1/commands/{id})", cmd_get_t, target_p95=3)

# ── 4. Query API latency ─────────────────────────────────────────────────────
print("\n4. QUERY API LATENCY (QuestDB)")

qdb_t = []
for _ in range(20):
    _, _, t = questdb.timed("GET", "/exec?query=" + urllib.parse.quote(
        f"SELECT count(*) FROM telemetry WHERE vehicle_id = '{DRONE_ALPHA_ID}'"))
    qdb_t.append(t)

query_t = []
for _ in range(20):
    _, _, t = query.timed("GET", f"/v1/telemetry?vehicle_id={DRONE_ALPHA_ID}&limit=100",
                           headers=auth_h)
    query_t.append(t)

report("Direct QuestDB count", qdb_t, target_p95=15)
report("Query service telemetry (limit=100)", query_t, target_p95=20)

# ── 5. Health check latency ──────────────────────────────────────────────────
print("\n5. HEALTH CHECK LATENCY")

for name, client in [("fleet", fleet), ("auth", apikey), ("commands", commands), ("query", query)]:
    ht = []
    for _ in range(100):
        _, _, t = client.timed("GET", "/healthz")
        ht.append(t)
    report(f"{name} /healthz", ht, target_p95=2)

# ── 6. Throughput ─────────────────────────────────────────────────────────────
print("\n6. TELEMETRY PIPELINE THROUGHPUT")

_, r1, _ = questdb.timed("GET", "/exec?query=" + urllib.parse.quote("SELECT count(*) FROM telemetry"))
count1 = r1["dataset"][0][0]
time.sleep(5)
_, r2, _ = questdb.timed("GET", "/exec?query=" + urllib.parse.quote("SELECT count(*) FROM telemetry"))
count2 = r2["dataset"][0][0]
rate = (count2 - count1) / 5.0
print(f"  QuestDB write rate: {rate:.1f} rows/sec")
print(f"  Total rows: {count2:,}")

# ── 7. NATS stats ────────────────────────────────────────────────────────────
print("\n7. NATS MESSAGE BUS")
nats = KeepAliveClient("localhost", 8222)
_, varz, _ = nats.timed("GET", "/varz")
print(f"  in_msgs:     {varz.get('in_msgs', 0):,}")
print(f"  out_msgs:    {varz.get('out_msgs', 0):,}")
print(f"  in_bytes:    {varz.get('in_bytes', 0) / 1024 / 1024:.1f} MB")
print(f"  connections: {varz.get('connections', 0)}")

# Cleanup
for c in [fleet, apikey, commands, query, questdb, kc, nats]:
    c.close()

print("\n" + "=" * 70)
print("BENCHMARK COMPLETE")
print("=" * 70)
print("""
Performance Targets (keep-alive, same machine):
  - Health checks:        < 2ms p95
  - Fleet CRUD:           < 5ms p95
  - Auth operations:      < 10ms p95
  - Command dispatch:     < 10ms p95
  - QuestDB queries:      < 20ms p95
  - Keycloak ROPC:        ~300-400ms (bcrypt intentionally slow)
""")
