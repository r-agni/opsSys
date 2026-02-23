"""
SystemScale telemetry simulator — publishes DataEnvelope protobuf messages
to NATS (for ws-gateway live stream) and writes ILP directly to QuestDB
(for historical queries).

Usage:  python scripts/simulate_telemetry.py
"""

import asyncio
import json
import math
import random
import socket
import struct
import time

import nats

# ─── Device config (from provisioning above) ─────────────────────────
DEVICES = [
    {
        "id": "2fe3a291-63a5-4feb-bb67-6dd7453ffb0d",
        "name": "drone-alpha",
        "project": "drone-fleet",
        "project_id": "b906a71f-cdf9-428a-91dd-2fbe761afe9b",
        "org_id": "org-001",
        "type": "quadcopter",
        "base_lat": 37.7749,
        "base_lon": -122.4194,
    },
    {
        "id": "1e1f2f1f-d065-410c-a852-a75ec64d6a81",
        "name": "drone-bravo",
        "project": "drone-fleet",
        "project_id": "b906a71f-cdf9-428a-91dd-2fbe761afe9b",
        "org_id": "org-001",
        "type": "quadcopter",
        "base_lat": 37.7760,
        "base_lon": -122.4180,
    },
    {
        "id": "a2ec313a-7cd8-4fd4-bbe3-43b76192735a",
        "name": "rover-01",
        "project": "drone-fleet",
        "project_id": "b906a71f-cdf9-428a-91dd-2fbe761afe9b",
        "org_id": "org-001",
        "type": "ground_rover",
        "base_lat": 37.7740,
        "base_lon": -122.4200,
    },
]

NATS_URL = "nats://localhost:4222"

STREAM_TELEMETRY = 1
STREAM_EVENT = 2
STREAM_SENSOR = 3
STREAM_LOG = 5

# ─── Protobuf manual encoder ─────────────────────────────────────────
# Avoids needing compiled proto stubs.

def _encode_varint(value):
    parts = []
    while value > 0x7F:
        parts.append((value & 0x7F) | 0x80)
        value >>= 7
    parts.append(value & 0x7F)
    return bytes(parts)

def _encode_field_varint(field_num, value):
    tag = (field_num << 3) | 0  # wire type 0 = varint
    return _encode_varint(tag) + _encode_varint(value)

def _encode_field_bytes(field_num, data):
    if isinstance(data, str):
        data = data.encode("utf-8")
    tag = (field_num << 3) | 2  # wire type 2 = length-delimited
    return _encode_varint(tag) + _encode_varint(len(data)) + data

def _encode_field_double(field_num, value):
    tag = (field_num << 3) | 1  # wire type 1 = 64-bit
    return _encode_varint(tag) + struct.pack("<d", value)

def _encode_field_float(field_num, value):
    tag = (field_num << 3) | 5  # wire type 5 = 32-bit
    return _encode_varint(tag) + struct.pack("<f", value)


def encode_data_envelope(
    vehicle_id, timestamp_ns, stream_type, payload_bytes,
    lat=0.0, lon=0.0, alt=0.0, seq=0,
    fleet_id="", org_id="", stream_name=""
):
    buf = b""
    buf += _encode_field_bytes(1, vehicle_id)
    buf += _encode_field_varint(2, timestamp_ns)
    buf += _encode_field_varint(3, stream_type)
    buf += _encode_field_bytes(4, payload_bytes)
    if lat != 0.0:
        buf += _encode_field_double(5, lat)
    if lon != 0.0:
        buf += _encode_field_double(6, lon)
    if alt != 0.0:
        buf += _encode_field_float(7, alt)
    if seq:
        buf += _encode_field_varint(8, seq)
    if fleet_id:
        buf += _encode_field_bytes(16, fleet_id)
    if org_id:
        buf += _encode_field_bytes(17, org_id)
    if stream_name:
        buf += _encode_field_bytes(18, stream_name)
    return buf


# ─── Telemetry generators ────────────────────────────────────────────

class DeviceSimulator:
    def __init__(self, device):
        self.dev = device
        self.seq = 0
        self.t0 = time.time()
        self.lat = device["base_lat"]
        self.lon = device["base_lon"]
        self.alt = 50.0 if "drone" in device["type"] else 0.5
        self.battery_pct = 100
        self.battery_mv = 16800
        self.heading = random.uniform(0, 360)
        self.speed = 0.0
        self.armed = True

    def _tick(self):
        self.seq += 1
        dt = time.time() - self.t0
        r = 0.0003
        self.lat = self.dev["base_lat"] + r * math.sin(dt * 0.1 + hash(self.dev["id"]))
        self.lon = self.dev["base_lon"] + r * math.cos(dt * 0.07 + hash(self.dev["id"]))
        if "drone" in self.dev["type"]:
            self.alt = 50 + 10 * math.sin(dt * 0.05)
        else:
            self.alt = 0.5
        self.heading = (self.heading + random.uniform(-2, 2)) % 360
        self.speed = 5.0 + 2.0 * math.sin(dt * 0.2)
        self.battery_pct = max(5, int(100 - dt * 0.05))
        self.battery_mv = max(12000, int(16800 - dt * 7))

    def telemetry_envelope(self):
        self._tick()
        payload = json.dumps({
            "roll": round(random.gauss(0, 3), 2),
            "pitch": round(random.gauss(0, 2), 2),
            "yaw": round(self.heading, 2),
            "vx": round(self.speed * math.cos(math.radians(self.heading)), 2),
            "vy": round(self.speed * math.sin(math.radians(self.heading)), 2),
            "vz": round(random.gauss(0, 0.5), 2),
            "battery_mv": self.battery_mv,
            "battery_pct": self.battery_pct,
            "armed": self.armed,
            "gps_fix": 3,
            "gps_sats": random.randint(10, 18),
            "hdop": round(random.uniform(0.5, 2.0), 2),
            "heading": round(self.heading, 2),
            "speed_ms": round(self.speed, 2),
            "rssi": random.randint(180, 255),
            "link_quality": random.randint(85, 100),
        }).encode()
        return encode_data_envelope(
            vehicle_id=self.dev["id"],
            timestamp_ns=time.time_ns(),
            stream_type=STREAM_TELEMETRY,
            payload_bytes=payload,
            lat=self.lat, lon=self.lon, alt=self.alt,
            seq=self.seq,
            fleet_id=self.dev["project_id"],
            org_id=self.dev["org_id"],
            stream_name="flight_state",
        )

    def sensor_envelope(self):
        self._tick()
        payload = json.dumps({
            "sensors": {
                "temp_cpu": round(random.uniform(40, 75), 1),
                "temp_motor_1": round(random.uniform(35, 65), 1),
                "temp_motor_2": round(random.uniform(35, 65), 1),
                "temp_esc": round(random.uniform(30, 55), 1),
                "humidity": round(random.uniform(30, 70), 1),
                "pressure_hpa": round(random.uniform(1010, 1020), 1),
                "vibration_x": round(random.gauss(0, 0.05), 4),
                "vibration_y": round(random.gauss(0, 0.05), 4),
                "vibration_z": round(random.gauss(0, 0.08), 4),
            }
        }).encode()
        return encode_data_envelope(
            vehicle_id=self.dev["id"],
            timestamp_ns=time.time_ns(),
            stream_type=STREAM_SENSOR,
            payload_bytes=payload,
            lat=self.lat, lon=self.lon, alt=self.alt,
            seq=self.seq,
            fleet_id=self.dev["project_id"],
            org_id=self.dev["org_id"],
            stream_name="onboard_sensors",
        )

    def log_envelope(self):
        messages = [
            "Navigation: waypoint 3/12 reached",
            "GPS: RTK fix acquired, accuracy 0.02m",
            "Battery: cell balance OK, delta 12mV",
            "Comms: link quality stable at 95%",
            "Autopilot: mode GUIDED, alt hold stable",
            "Motor: ESC calibration verified",
            "Camera: streaming 1080p @ 30fps",
            "Storage: 42% capacity remaining",
            "Wind: estimated 3.2 m/s from NNW",
            "Geofence: within boundaries, margin 85m",
        ]
        payload = json.dumps({
            "level": random.choice(["info", "info", "info", "debug"]),
            "message": random.choice(messages),
            "component": random.choice(["nav", "gps", "power", "comms", "autopilot", "camera"]),
        }).encode()
        return encode_data_envelope(
            vehicle_id=self.dev["id"],
            timestamp_ns=time.time_ns(),
            stream_type=STREAM_LOG,
            payload_bytes=payload,
            seq=self.seq,
            fleet_id=self.dev["project_id"],
            org_id=self.dev["org_id"],
            stream_name="system_log",
        )

    def event_envelope(self, event_type_val, description):
        payload = json.dumps({
            "event_type": event_type_val,
            "description": description,
        }).encode()
        return encode_data_envelope(
            vehicle_id=self.dev["id"],
            timestamp_ns=time.time_ns(),
            stream_type=STREAM_EVENT,
            payload_bytes=payload,
            lat=self.lat, lon=self.lon, alt=self.alt,
            seq=self.seq,
            fleet_id=self.dev["project_id"],
            org_id=self.dev["org_id"],
            stream_name="events",
        )

    def alert_envelope(self, level, message):
        payload = json.dumps({
            "event_type": 101,
            "description": message,
            "_tags": {"level": level},
            "level": level,
            "message": message,
        }).encode()
        return encode_data_envelope(
            vehicle_id=self.dev["id"],
            timestamp_ns=time.time_ns(),
            stream_type=STREAM_EVENT,
            payload_bytes=payload,
            lat=self.lat, lon=self.lon, alt=self.alt,
            seq=self.seq,
            fleet_id=self.dev["project_id"],
            org_id=self.dev["org_id"],
            stream_name="alerts",
        )

STREAM_TYPE_NAMES = {
    STREAM_TELEMETRY: "telemetry",
    STREAM_EVENT: "event",
    STREAM_SENSOR: "sensor",
    STREAM_LOG: "log",
}

QUESTDB_ILP_HOST = "localhost"
QUESTDB_ILP_PORT = 9009


def _escape_ilp(s):
    return s.replace(" ", "\\ ").replace(",", "\\,").replace("=", "\\=")


def _escape_ilp_str(s):
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n") + '"'


class ILPWriter:
    """Batches ILP lines and flushes to QuestDB over TCP."""

    def __init__(self, host=QUESTDB_ILP_HOST, port=QUESTDB_ILP_PORT):
        self.host = host
        self.port = port
        self._buf: list[str] = []
        self._sock: socket.socket | None = None

    def _connect(self):
        if self._sock is None:
            self._sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            self._sock.connect((self.host, self.port))

    def add(self, vehicle_id, stream_type, stream_name, project_id, org_id,
            lat, lon, alt, seq, payload_json, ts_ns, extra_fields=None):
        tags = (
            f"telemetry,vehicle_id={_escape_ilp(vehicle_id)}"
            f",stream_type={_escape_ilp(stream_type)}"
            f",project_id={_escape_ilp(project_id)}"
            f",org_id={_escape_ilp(org_id)}"
        )
        if stream_name:
            tags += f",stream_name={_escape_ilp(stream_name)}"

        fields = f"lat={lat},lon={lon},alt_m={alt},seq={seq}i,payload_size={len(payload_json)}i"
        if extra_fields:
            for k, v in extra_fields.items():
                fields += f",{_escape_ilp(k)}={v}"
        fields += f",payload_json={_escape_ilp_str(payload_json)}"

        self._buf.append(f"{tags} {fields} {ts_ns}")

    def flush(self):
        if not self._buf:
            return
        try:
            self._connect()
            data = "\n".join(self._buf) + "\n"
            self._sock.sendall(data.encode())
            self._buf.clear()
        except Exception as e:
            print(f"  ILP write error: {e}")
            self._sock = None
            self._buf.clear()


async def main():
    nc = await nats.connect(NATS_URL)
    print(f"Connected to NATS at {NATS_URL}")

    ilp = ILPWriter()
    print(f"ILP writer targeting QuestDB at {QUESTDB_ILP_HOST}:{QUESTDB_ILP_PORT}")

    sims = [DeviceSimulator(d) for d in DEVICES]
    msg_count = 0
    start = time.time()

    print(f"Streaming telemetry for {len(DEVICES)} devices...")
    print(f"  Devices: {', '.join(d['name'] for d in DEVICES)}")
    print(f"  Types: telemetry (2Hz), sensor (1Hz), log (0.5Hz), events/alerts (periodic)")
    print()

    try:
        cycle = 0
        while True:
            cycle += 1
            ts_ns = time.time_ns()

            for sim in sims:
                # Telemetry at ~2Hz
                data = sim.telemetry_envelope()
                subj = f"telemetry.{sim.dev['id']}.telemetry"
                await nc.publish(subj, data)

                payload = json.loads(data[data.find(b'{'):data.rfind(b'}')+1])
                ilp.add(
                    vehicle_id=sim.dev["id"], stream_type="telemetry",
                    stream_name="flight_state", project_id=sim.dev["project_id"],
                    org_id=sim.dev["org_id"], lat=sim.lat, lon=sim.lon,
                    alt=sim.alt, seq=sim.seq, payload_json=json.dumps(payload),
                    ts_ns=ts_ns,
                    extra_fields={
                        "battery_pct": f"{sim.battery_pct}i",
                        "speed_ms": f"{sim.speed:.2f}",
                        "heading": f"{sim.heading:.2f}",
                    }
                )
                msg_count += 1

                # Sensor data at ~1Hz
                if cycle % 2 == 0:
                    data = sim.sensor_envelope()
                    subj = f"telemetry.{sim.dev['id']}.sensor"
                    await nc.publish(subj, data)

                    payload = json.loads(data[data.find(b'{'):data.rfind(b'}')+1])
                    ilp.add(
                        vehicle_id=sim.dev["id"], stream_type="sensor",
                        stream_name="onboard_sensors", project_id=sim.dev["project_id"],
                        org_id=sim.dev["org_id"], lat=sim.lat, lon=sim.lon,
                        alt=sim.alt, seq=sim.seq, payload_json=json.dumps(payload),
                        ts_ns=ts_ns + 1,
                    )
                    msg_count += 1

                # Log at ~0.5Hz
                if cycle % 4 == 0:
                    data = sim.log_envelope()
                    subj = f"telemetry.{sim.dev['id']}.log"
                    await nc.publish(subj, data)

                    payload = json.loads(data[data.find(b'{'):data.rfind(b'}')+1])
                    ilp.add(
                        vehicle_id=sim.dev["id"], stream_type="log",
                        stream_name="system_log", project_id=sim.dev["project_id"],
                        org_id=sim.dev["org_id"], lat=0.0, lon=0.0,
                        alt=0.0, seq=sim.seq, payload_json=json.dumps(payload),
                        ts_ns=ts_ns + 2,
                    )
                    msg_count += 1

                # Events
                if cycle % 20 == 0 and random.random() < 0.4:
                    events = [
                        (3, "Mode changed to GUIDED"),
                        (6, f"Waypoint {random.randint(1,12)} reached"),
                        (4, "Takeoff complete, altitude 50m"),
                    ]
                    evt_type, desc = random.choice(events)
                    data = sim.event_envelope(evt_type, desc)
                    subj = f"telemetry.{sim.dev['id']}.event"
                    await nc.publish(subj, data)

                    ilp.add(
                        vehicle_id=sim.dev["id"], stream_type="event",
                        stream_name="events", project_id=sim.dev["project_id"],
                        org_id=sim.dev["org_id"], lat=sim.lat, lon=sim.lon,
                        alt=sim.alt, seq=sim.seq, payload_json=json.dumps({"event_type": evt_type, "description": desc}),
                        ts_ns=ts_ns + 3,
                    )
                    msg_count += 1

                # Alerts
                if cycle % 30 == 0 and random.random() < 0.5:
                    alerts = [
                        ("warning", f"Battery at {sim.battery_pct}% — consider RTL"),
                        ("info", "Entering restricted airspace zone B"),
                        ("warning", f"Wind gust detected: {round(random.uniform(5,12),1)} m/s"),
                        ("error", "GPS HDOP degraded to 3.5 — accuracy reduced"),
                        ("critical", "Motor 3 current spike — 42A peak"),
                        ("info", f"Signal strength: {random.randint(85,100)}%"),
                        ("warning", "Temperature warning: CPU at 72C"),
                    ]
                    level, msg = random.choice(alerts)
                    data = sim.alert_envelope(level, msg)
                    subj = f"telemetry.{sim.dev['id']}.event"
                    await nc.publish(subj, data)

                    ilp.add(
                        vehicle_id=sim.dev["id"], stream_type="event",
                        stream_name="alerts", project_id=sim.dev["project_id"],
                        org_id=sim.dev["org_id"], lat=sim.lat, lon=sim.lon,
                        alt=sim.alt, seq=sim.seq,
                        payload_json=json.dumps({"event_type": 101, "level": level, "message": msg}),
                        ts_ns=ts_ns + 4,
                    )
                    msg_count += 1

            await nc.flush()
            ilp.flush()

            elapsed = time.time() - start
            if cycle % 10 == 0:
                rate = msg_count / elapsed if elapsed > 0 else 0
                print(f"  [{elapsed:.0f}s] {msg_count} messages sent ({rate:.1f} msg/s)")

            await asyncio.sleep(0.5)

    except KeyboardInterrupt:
        print(f"\nStopped. Total: {msg_count} messages in {time.time()-start:.0f}s")
    finally:
        await nc.close()


if __name__ == "__main__":
    asyncio.run(main())
