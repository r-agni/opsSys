"""Tests for token exchange with retry logic."""

import json
import unittest
from unittest.mock import patch, MagicMock
import urllib.error

from systemscale.core.auth import exchange_token, _cache, _lock


class TestTokenExchange(unittest.TestCase):
    def setUp(self):
        with _lock:
            _cache.clear()

    def tearDown(self):
        with _lock:
            _cache.clear()

    @patch("systemscale.core.auth.urllib.request.urlopen")
    def test_successful_exchange(self, mock_urlopen):
        resp_data = json.dumps({
            "access_token": "jwt-token-123",
            "expires_in": 3600,
        }).encode()
        mock_resp = MagicMock()
        mock_resp.read.return_value = resp_data
        mock_resp.__enter__ = lambda s: s
        mock_resp.__exit__ = MagicMock(return_value=False)
        mock_urlopen.return_value = mock_resp

        token = exchange_token("ssk_live_test", "http://localhost:8083")
        self.assertEqual(token, "jwt-token-123")
        self.assertEqual(mock_urlopen.call_count, 1)

    @patch("systemscale.core.auth.urllib.request.urlopen")
    def test_cached_token_reused(self, mock_urlopen):
        resp_data = json.dumps({
            "access_token": "jwt-cached",
            "expires_in": 3600,
        }).encode()
        mock_resp = MagicMock()
        mock_resp.read.return_value = resp_data
        mock_resp.__enter__ = lambda s: s
        mock_resp.__exit__ = MagicMock(return_value=False)
        mock_urlopen.return_value = mock_resp

        token1 = exchange_token("ssk_live_cache", "http://localhost:8083")
        token2 = exchange_token("ssk_live_cache", "http://localhost:8083")
        self.assertEqual(token1, token2)
        self.assertEqual(mock_urlopen.call_count, 1)

    @patch("systemscale.core.auth.urllib.request.urlopen")
    def test_retries_on_500(self, mock_urlopen):
        error_500 = urllib.error.HTTPError(
            "http://localhost:8083/v1/token", 500, "Server Error",
            {}, MagicMock(read=MagicMock(return_value=b"error"))
        )
        resp_data = json.dumps({
            "access_token": "jwt-retry",
            "expires_in": 3600,
        }).encode()
        mock_resp = MagicMock()
        mock_resp.read.return_value = resp_data
        mock_resp.__enter__ = lambda s: s
        mock_resp.__exit__ = MagicMock(return_value=False)

        mock_urlopen.side_effect = [error_500, mock_resp]

        token = exchange_token("ssk_live_retry", "http://localhost:8083")
        self.assertEqual(token, "jwt-retry")
        self.assertEqual(mock_urlopen.call_count, 2)

    @patch("systemscale.core.auth.urllib.request.urlopen")
    def test_no_retry_on_4xx(self, mock_urlopen):
        error_401 = urllib.error.HTTPError(
            "http://localhost:8083/v1/token", 401, "Unauthorized",
            {}, MagicMock(read=MagicMock(return_value=b"bad key"))
        )
        mock_urlopen.side_effect = error_401

        with self.assertRaises(RuntimeError) as ctx:
            exchange_token("ssk_live_bad", "http://localhost:8083")
        self.assertIn("401", str(ctx.exception))
        self.assertEqual(mock_urlopen.call_count, 1)


if __name__ == "__main__":
    unittest.main()
