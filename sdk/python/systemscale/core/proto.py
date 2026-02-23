"""
Minimal protobuf wire-format encoder for DataEnvelope.

Encodes DataEnvelope directly to protobuf bytes without compiled stubs
or external dependencies.  Field numbers match proto/core/envelope.proto.
"""

from __future__ import annotations

import struct
import time


def _encode_varint(value: int) -> bytes:
    out = bytearray()
    while value > 0x7F:
        out.append((value & 0x7F) | 0x80)
        value >>= 7
    out.append(value & 0x7F)
    return bytes(out)


def _field_varint(field: int, value: int) -> bytes:
    if value == 0:
        return b""
    return _encode_varint((field << 3) | 0) + _encode_varint(value)


def _field_bytes(field: int, data: bytes) -> bytes:
    if not data:
        return b""
    return _encode_varint((field << 3) | 2) + _encode_varint(len(data)) + data


def _field_string(field: int, s: str) -> bytes:
    return _field_bytes(field, s.encode("utf-8")) if s else b""


def _field_double(field: int, value: float) -> bytes:
    if value == 0.0:
        return b""
    return _encode_varint((field << 3) | 1) + struct.pack("<d", value)


def _field_float(field: int, value: float) -> bytes:
    if value == 0.0:
        return b""
    return _encode_varint((field << 3) | 5) + struct.pack("<f", value)


STREAM_TYPES = {
    "telemetry": 1,
    "event":     2,
    "sensor":    3,
    "video_meta": 4,
    "log":       5,
    "audio":     6,
}


def encode_data_envelope(
    *,
    vehicle_id:    str,
    stream:        str = "telemetry",
    payload:       bytes = b"",
    lat:           float = 0.0,
    lon:           float = 0.0,
    alt:           float = 0.0,
    seq:           int = 0,
    fleet_id:      str = "",
    org_id:        str = "",
    stream_name:   str = "",
    sender_id:     str = "",
    sender_type:   str = "",
    receiver_id:   str = "",
    receiver_type: str = "",
) -> bytes:
    """Encode a DataEnvelope to protobuf wire format."""
    ts = int(time.time() * 1_000_000_000)
    st = STREAM_TYPES.get(stream, 1)

    buf = bytearray()
    buf += _field_string(1, vehicle_id)
    buf += _field_varint(2, ts)
    buf += _field_varint(3, st)
    buf += _field_bytes(4, payload)
    buf += _field_double(5, lat)
    buf += _field_double(6, lon)
    buf += _field_float(7, alt)
    buf += _field_varint(8, seq)
    buf += _field_string(16, fleet_id)
    buf += _field_string(17, org_id)
    buf += _field_string(18, stream_name)
    buf += _field_string(19, sender_id)
    buf += _field_string(20, sender_type)
    buf += _field_string(21, receiver_id)
    buf += _field_string(22, receiver_type)
    return bytes(buf)
