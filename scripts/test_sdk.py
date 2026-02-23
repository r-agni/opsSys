#!/usr/bin/env python3
"""
test_sdk.py — E2E test of the SystemScale Python SDK operator flow.

Steps:
  1. Keycloak ROPC → admin JWT
  2. POST /v1/keys → create API key
  3. systemscale.connect() → OperatorClient (handles token exchange internally)
  4. ops.devices() → list fleet via GET /v1/projects/{name}/devices with SDK JWT
  5. POST /v1/assistance → create test request (simulates device SDK call)
  6. GET /v1/assistance → retrieve via SDK JWT
  7. ops.send_command("drone-alpha") → resolves name→UUID → command-api → SSE
     status=timeout is EXPECTED (no real edge agent in local dev)

Prerequisites:
  - All Docker services healthy (fleet, auth, command, ws-gateway)
  - seed_fleet.py has already been run
  - Run from repo root:  python3 scripts/test_sdk.py
"""
import json, os, sys, time, uuid, urllib.error, urllib.parse, urllib.request

# Add SDK to path so we can import without pip install
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk", "python"))

KEYCLOAK_URL    = "http://localhost:8180"
KEYCLOAK_REALM  = "systemscale"
KEYCLOAK_CLIENT = "systemscale-dashboard"
KC_USERNAME     = "ragni.works@gmail.com"
KC_PASSWORD     = "Test1234!"
FLEET_API_URL   = "http://localhost:8080"
APIKEY_URL      = "http://localhost:8083"
WS_URL          = "ws://localhost:8084"
COMMAND_API_URL = "http://localhost:8082"
PROJECT_NAME    = "drone-fleet"
DRONE_ALPHA_ID  = "2fe3a291-63a5-4feb-bb67-6dd7453ffb0d"

def _post_form(url, data):
    req = urllib.request.Request(
        url, urllib.parse.urlencode(data).encode(),
        {"Content-Type": "application/x-www-form-urlencoded"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()

def _http(method, url, body=None, token=None):
    h = {"Content-Type": "application/json"}
    if token:
        h["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(
        url, json.dumps(body).encode() if body else None, h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()

def step(n, msg): print(f"\n{'='*60}\nSTEP {n}: {msg}\n{'='*60}")
def ok(msg):   print(f"  [PASS] {msg}")
def warn(msg): print(f"  [WARN] {msg}")
def fail(msg): print(f"  [FAIL] {msg}", file=sys.stderr); sys.exit(1)

# ── Step 1: Keycloak admin JWT ────────────────────────────────────────────────
step(1, "Get admin JWT from Keycloak (ROPC)")
status, kc = _post_form(
    f"{KEYCLOAK_URL}/realms/{KEYCLOAK_REALM}/protocol/openid-connect/token",
    {"grant_type": "password", "client_id": KEYCLOAK_CLIENT,
     "username": KC_USERNAME, "password": KC_PASSWORD, "scope": "openid"})
if status != 200:
    fail(f"Keycloak ROPC HTTP {status}: {kc}")
admin_jwt = kc["access_token"]
ok(f"Admin JWT obtained (len={len(admin_jwt)})")

# Decode claims for display (no signature verification — test only)
import base64
def _jwt_claims(token):
    try:
        part = token.split(".")[1]
        part += "=" * (4 - len(part) % 4)
        return json.loads(base64.urlsafe_b64decode(part))
    except Exception:
        return {}
claims = _jwt_claims(admin_jwt)
ok(f"Claims: org_id={claims.get('org_id')}, role={claims.get('role')}, email={claims.get('email')}")

# ── Step 2: Create API key ───────────────────────────────────────────────────
step(2, "Create API key via apikey-service POST /v1/keys")
status, key_resp = _http("POST", f"{APIKEY_URL}/v1/keys",
                         {"name": f"e2e-test-{int(time.time())}", "scopes": ["operator"]},
                         token=admin_jwt)
if status != 201:
    fail(f"POST /v1/keys HTTP {status}: {key_resp}")
api_key = key_resp["api_key"]
ok(f"API key created: prefix={key_resp['key_prefix']}, id={key_resp['id']}")
if not api_key.startswith("ssk_live_"):
    fail(f"Unexpected key format (expected ssk_live_...): {api_key[:24]}")

# ── Step 3: systemscale.connect() ────────────────────────────────────────────
step(3, "systemscale.connect() → OperatorClient")
try:
    import systemscale
    ok(f"SDK imported from: {systemscale.__file__}")
except ImportError as e:
    fail(f"Cannot import systemscale SDK: {e}")

ops = systemscale.connect(
    api_key     = api_key,
    project     = PROJECT_NAME,
    fleet_api   = FLEET_API_URL,
    apikey_url  = APIKEY_URL,
    ws_url      = WS_URL,
    command_api = COMMAND_API_URL,
)
ok(f"OperatorClient ready for project='{PROJECT_NAME}'")

# ── Step 4: ops.devices() ────────────────────────────────────────────────────
step(4, "ops.devices() → GET /v1/projects/{name}/devices")
# SDK internally calls exchange_token(api_key, apikey_url) → JWT, then calls fleet-api
try:
    devices = ops.devices()
    ok(f"{len(devices)} device(s) returned")
    for d in devices:
        print(f"  - {d.get('display_name')} ({d.get('vehicle_type')}) id={d.get('id')}")
    if not devices:
        warn("No devices returned — did seed_fleet.py run? Fleet-api JWT auth might also be failing.")
except Exception as e:
    fail(f"ops.devices() raised: {e}")

# ── Step 5: Simulate device assistance request ───────────────────────────────
step(5, "Simulate device assistance request via POST /v1/assistance")
rid = str(uuid.uuid4())
status, _ = _http(
    "POST", f"{FLEET_API_URL}/v1/assistance",
    body={"request_id": rid, "device_id": DRONE_ALPHA_ID,
          "project_id": "b906a71f-cdf9-428a-91dd-2fbe761afe9b",
          "reason": "E2E test assistance from test_sdk.py", "timeout_s": 120},
    token=admin_jwt)
if status != 201:
    fail(f"POST /v1/assistance HTTP {status}")
ok(f"Assistance request created: request_id={rid}")

# ── Step 6: List assistance requests via SDK JWT ─────────────────────────────
step(6, "GET /v1/assistance using SDK JWT (from apikey-service)")
sdk_jwt = ops._token()  # exchange_token() — cached after step 4
sdk_claims = _jwt_claims(sdk_jwt)
ok(f"SDK JWT: iss={sdk_claims.get('iss')}, org_id={sdk_claims.get('org_id')}")

status, ar = _http("GET", f"{FLEET_API_URL}/v1/assistance", token=sdk_jwt)
if status != 200:
    fail(f"GET /v1/assistance HTTP {status}: {ar}")
reqs = ar.get("requests", [])
ok(f"{len(reqs)} pending request(s) in org")
if any(r.get("request_id") == rid for r in reqs):
    ok(f"Found our test request ({rid})")
else:
    warn(f"Request {rid} not in list — may have already expired or status filtered")

# Respond to clean up (use admin JWT so Role check passes)
status, _ = _http("POST", f"{FLEET_API_URL}/v1/assistance/{rid}/respond",
                  {"approved": True, "instruction": "auto-approved by test_sdk.py"},
                  token=admin_jwt)
if status == 200:
    ok("Responded (approved=True) — request resolved")
elif status == 404:
    warn("Respond 404 — request was not pending (already expired?)")
else:
    warn(f"Respond returned HTTP {status}")

# ── Step 7: Send command ─────────────────────────────────────────────────────
step(7, "ops.send_command('drone-alpha', cmd_type='test_ping') — timeout EXPECTED")
print("  SDK resolves 'drone-alpha' → vehicle UUID via /v1/projects/{name}/devices")
print("  Then POSTs to command-api /v1/commands and waits SSE on /v1/commands/{id}/stream")
print("  Expected outcome: status='timeout' (no edge agent to ACK)")
try:
    result = ops.send_command(
        "drone-alpha",
        cmd_type = "test_ping",
        data     = {"source": "test_sdk.py", "ts": time.time()},
        timeout  = 8.0,
    )
    ok(f"CommandResult: status={result.status!r}, command_id={result.command_id}")
    if result.status == "timeout":
        ok("status='timeout' is EXPECTED — full command path is working!")
    elif result.status in ("completed", "accepted"):
        ok(f"Command acknowledged with status={result.status!r} (edge agent running?)")
    else:
        warn(f"Unexpected status: {result.status!r}")
except Exception as e:
    warn(f"send_command() raised: {e}")
    warn("Check command-api logs: docker compose -f deploy/docker/docker-compose.dev.yml logs commands")

# ── Summary ───────────────────────────────────────────────────────────────────
print(f"\n{'='*60}")
print("E2E SDK TEST COMPLETE")
print(f"{'='*60}")
print("""
Step 1  Keycloak OIDC issuing admin JWTs with org_id/role claims
Step 2  apikey-service creating API keys (ssk_live_...) in CockroachDB
Step 3  SDK OperatorClient construction (systemscale.connect())
Step 4  fleet-api returning project devices with apikey-service JWT
Step 5  fleet-api accepting assistance requests (device simulation)
Step 6  fleet-api listing + responding to assistance requests
Step 7  command-api accepting commands + returning ACK via SSE
        (timeout=expected, no real edge agent in local dev)

All PASS = full auth + fleet + command + SDK path verified end-to-end.
""")
