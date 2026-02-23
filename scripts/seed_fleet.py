#!/usr/bin/env python3
"""
seed_fleet.py — Seeds fleet-api CockroachDB with fixed-UUID vehicles
matching scripts/simulate_telemetry.py hardcoded device IDs.

Strategy: direct psycopg2 INSERT into CockroachDB (port 26257, root/insecure).
fleet-api creates tables via applySchema() on startup; run AFTER fleet is healthy.

Usage:
    pip install psycopg2-binary
    python3 scripts/seed_fleet.py
"""
import json, sys, urllib.request, urllib.parse, urllib.error
from datetime import datetime, timezone

KEYCLOAK_URL    = "http://localhost:8180"
KEYCLOAK_REALM  = "systemscale"
KEYCLOAK_CLIENT = "systemscale-dashboard"
KC_USERNAME     = "ragni.works@gmail.com"
KC_PASSWORD     = "Test1234!"
FLEET_API_URL   = "http://localhost:8080"
CRDB_HOST, CRDB_PORT, CRDB_USER, CRDB_DB = "localhost", 26257, "root", "systemscale"

PROJECT_ID   = "b906a71f-cdf9-428a-91dd-2fbe761afe9b"
PROJECT_NAME = "drone-fleet"
ORG_ID       = "org-001"

VEHICLES = [
    {"id": "2fe3a291-63a5-4feb-bb67-6dd7453ffb0d", "display_name": "drone-alpha",
     "vehicle_type": "quadcopter",   "adapter_type": "custom_unix", "region": "local-dev"},
    {"id": "1e1f2f1f-d065-410c-a852-a75ec64d6a81", "display_name": "drone-bravo",
     "vehicle_type": "quadcopter",   "adapter_type": "custom_unix", "region": "local-dev"},
    {"id": "a2ec313a-7cd8-4fd4-bbe3-43b76192735a", "display_name": "rover-01",
     "vehicle_type": "ground_rover", "adapter_type": "custom_unix", "region": "local-dev"},
]

def _post_form(url, data):
    req = urllib.request.Request(url, urllib.parse.urlencode(data).encode(),
                                 {"Content-Type": "application/x-www-form-urlencoded"})
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.loads(r.read())

def _http(method, url, body=None, token=None):
    h = {"Content-Type": "application/json"}
    if token:
        h["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(
        url, json.dumps(body).encode() if body else None, h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()

def step(msg): print(f"\n[STEP] {msg}")
def ok(msg):   print(f"  OK   {msg}")
def fail(msg): print(f"  FAIL {msg}", file=sys.stderr); sys.exit(1)

# ── Step 1: Keycloak JWT ─────────────────────────────────────────────────────
step("1. Get Keycloak JWT via ROPC")
try:
    jwt = _post_form(
        f"{KEYCLOAK_URL}/realms/{KEYCLOAK_REALM}/protocol/openid-connect/token",
        {"grant_type": "password", "client_id": KEYCLOAK_CLIENT,
         "username": KC_USERNAME, "password": KC_PASSWORD, "scope": "openid"},
    )["access_token"]
    ok(f"JWT obtained (len={len(jwt)})")
except Exception as e:
    fail(f"Keycloak ROPC failed: {e}")

# ── Step 2: Verify fleet-api health ─────────────────────────────────────────
step("2. Verify fleet-api /healthz")
status, _ = _http("GET", f"{FLEET_API_URL}/healthz")
if status != 200:
    fail(f"fleet-api health HTTP {status} — is the fleet service running?")
ok("fleet-api healthy")

# ── Step 3: Direct CockroachDB insert ───────────────────────────────────────
step("3. Insert project and vehicles directly into CockroachDB")
try:
    import psycopg2
except ImportError:
    fail("psycopg2 not installed. Run: pip install psycopg2-binary")

try:
    conn = psycopg2.connect(host=CRDB_HOST, port=CRDB_PORT, user=CRDB_USER,
                            dbname=CRDB_DB, sslmode="disable")
    conn.autocommit = True
    cur = conn.cursor()
except Exception as e:
    fail(f"CockroachDB connect failed ({CRDB_HOST}:{CRDB_PORT}): {e}")

now = datetime.now(timezone.utc).isoformat()

# Upsert project with fixed ID (ON CONFLICT on unique index org_id+name)
cur.execute(
    """INSERT INTO projects (id, org_id, name, display_name, created_at, active)
       VALUES (%s, %s, %s, %s, %s, true)
       ON CONFLICT (org_id, name) DO UPDATE SET id = EXCLUDED.id, active = true""",
    (PROJECT_ID, ORG_ID, PROJECT_NAME, PROJECT_NAME, now),
)
ok(f"Upserted project '{PROJECT_NAME}' id={PROJECT_ID}")

# Upsert vehicles with fixed IDs (ON CONFLICT on primary key)
for v in VEHICLES:
    cur.execute(
        """INSERT INTO vehicles
             (id, org_id, project_id, display_name, vehicle_type, adapter_type,
              region, created_at, updated_at, active)
           VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, true)
           ON CONFLICT (id) DO UPDATE
             SET org_id        = EXCLUDED.org_id,
                 project_id    = EXCLUDED.project_id,
                 display_name  = EXCLUDED.display_name,
                 vehicle_type  = EXCLUDED.vehicle_type,
                 updated_at    = EXCLUDED.updated_at,
                 active        = true""",
        (v["id"], ORG_ID, PROJECT_ID, v["display_name"], v["vehicle_type"],
         v["adapter_type"], v["region"], now, now),
    )
    ok(f"Upserted vehicle '{v['display_name']}' id={v['id']}")

cur.close()
conn.close()

# ── Step 4: Verify via fleet-api ─────────────────────────────────────────────
step("4. Verify via fleet-api GET /v1/vehicles")
status, body = _http("GET", f"{FLEET_API_URL}/v1/vehicles", token=jwt)
if status != 200:
    fail(f"GET /v1/vehicles HTTP {status}: {body}")

seeded = {v["id"] for v in VEHICLES}
found  = {v["id"] for v in body.get("vehicles", [])}
for v in body.get("vehicles", []):
    marker = "*" if v["id"] in seeded else " "
    print(f"  [{marker}] {v['display_name']} ({v.get('vehicle_type','')}) id={v['id']}")

if seeded <= found:
    ok(f"All {len(VEHICLES)} seeded vehicles confirmed via fleet-api")
else:
    fail(f"Missing in fleet-api response: {seeded - found}")

print("\nSeed complete. Ready to run scripts/simulate_telemetry.py")
