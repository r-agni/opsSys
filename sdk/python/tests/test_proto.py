"""Tests for the minimal protobuf wire-format encoder."""

import struct
import unittest

from systemscale.core.proto import (
    encode_data_envelope,
    _encode_varint,
    _field_string,
    _field_varint,
    _field_double,
    _field_float,
    STREAM_TYPES,
)


class TestVarintEncoding(unittest.TestCase):
    def test_single_byte(self):
        self.assertEqual(_encode_varint(0), b"\x00")
        self.assertEqual(_encode_varint(1), b"\x01")
        self.assertEqual(_encode_varint(127), b"\x7f")

    def test_multi_byte(self):
        self.assertEqual(_encode_varint(128), b"\x80\x01")
        self.assertEqual(_encode_varint(300), b"\xac\x02")

    def test_large_value(self):
        encoded = _encode_varint(1_000_000_000)
        self.assertGreater(len(encoded), 1)
        # Decode back to verify
        result, shift = 0, 0
        for b in encoded:
            result |= (b & 0x7F) << shift
            shift += 7
        self.assertEqual(result, 1_000_000_000)


class TestFieldEncoders(unittest.TestCase):
    def test_string_field_empty(self):
        self.assertEqual(_field_string(1, ""), b"")

    def test_string_field(self):
        data = _field_string(1, "hello")
        self.assertIn(b"hello", data)

    def test_varint_field_zero(self):
        self.assertEqual(_field_varint(1, 0), b"")

    def test_double_field_zero(self):
        self.assertEqual(_field_double(5, 0.0), b"")

    def test_double_field_nonzero(self):
        data = _field_double(5, 28.6)
        self.assertGreater(len(data), 8)

    def test_float_field_nonzero(self):
        data = _field_float(7, 100.5)
        self.assertGreater(len(data), 4)


class TestEncodeDataEnvelope(unittest.TestCase):
    def test_basic_envelope(self):
        result = encode_data_envelope(
            vehicle_id="drone-001",
            stream="telemetry",
            payload=b'{"temp": 85}',
            lat=28.6,
            lon=77.2,
            alt=100.0,
            fleet_id="proj-1",
        )
        self.assertIsInstance(result, bytes)
        self.assertGreater(len(result), 20)
        # vehicle_id should appear in the bytes
        self.assertIn(b"drone-001", result)
        self.assertIn(b"proj-1", result)

    def test_empty_optional_fields_omitted(self):
        minimal = encode_data_envelope(vehicle_id="v1")
        full = encode_data_envelope(
            vehicle_id="v1",
            fleet_id="fleet",
            sender_id="s1",
            stream_name="lidar",
        )
        self.assertLess(len(minimal), len(full))

    def test_stream_type_mapping(self):
        for name, expected_val in STREAM_TYPES.items():
            result = encode_data_envelope(vehicle_id="v1", stream=name)
            self.assertIsInstance(result, bytes)
            self.assertGreater(len(result), 0)


if __name__ == "__main__":
    unittest.main()
