"""Tests for the HTTP transport layer."""

import unittest
from unittest.mock import patch, MagicMock
import http.client

from systemscale.core.transport import (
    _get_conn,
    _POOL_MAX_SIZE,
    _POOL_IDLE_TTL,
    _pool,
    _pool_lock,
)


class TestConnectionPool(unittest.TestCase):
    def setUp(self):
        with _pool_lock:
            _pool.clear()

    def tearDown(self):
        with _pool_lock:
            _pool.clear()

    def test_new_connection_created(self):
        conn, path = _get_conn("http://localhost:8080/healthz")
        self.assertIsInstance(conn, http.client.HTTPConnection)
        self.assertEqual(path, "/healthz")

    def test_pool_caches_connection(self):
        conn1, _ = _get_conn("http://localhost:8080/a")
        conn2, _ = _get_conn("http://localhost:8080/b")
        self.assertIs(conn1, conn2)

    def test_different_hosts_different_connections(self):
        conn1, _ = _get_conn("http://localhost:8080/a")
        conn2, _ = _get_conn("http://localhost:8081/a")
        self.assertIsNot(conn1, conn2)

    def test_pool_max_size(self):
        for port in range(_POOL_MAX_SIZE + 5):
            _get_conn(f"http://localhost:{9000 + port}/test")
        with _pool_lock:
            self.assertLessEqual(len(_pool), _POOL_MAX_SIZE)


class TestRetryWithJitter(unittest.TestCase):
    def test_post_with_retry_returns_true_on_success(self):
        from systemscale.core.transport import post_with_retry
        with patch("systemscale.core.transport._request") as mock_req:
            mock_req.return_value = (200, b"ok")
            result = post_with_retry("http://localhost:8080/test", b"body")
            self.assertTrue(result)

    def test_post_with_retry_returns_false_on_4xx(self):
        from systemscale.core.transport import post_with_retry
        with patch("systemscale.core.transport._request") as mock_req:
            mock_req.return_value = (400, b"bad request")
            result = post_with_retry("http://localhost:8080/test", b"body")
            self.assertFalse(result)


if __name__ == "__main__":
    unittest.main()
