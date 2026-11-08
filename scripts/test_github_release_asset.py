"""Offline API transport oracles: exact identity, bounded bytes and safe failures."""

from __future__ import annotations

from collections.abc import Buffer
from http.client import BadStatusLine, HTTPMessage, IncompleteRead
import io
import json
import ssl
import unittest
from unittest.mock import patch
import urllib.error

from scripts import github_release_asset as assets


URL = "https://github.com/example/tools/releases/download/v1.2.3/tool.tar.gz"
TAG_URL = "https://api.github.com/repos/example/tools/releases/tags/v1.2.3"
ASSET_URL = "https://api.github.com/repos/example/tools/releases/assets/17"
PAYLOAD = b"archive"
ROW = {"name": "tool.tar.gz", "id": 17, "size": len(PAYLOAD), "state": "uploaded"}


class Response(io.BytesIO):
    url = "https://release-assets.githubusercontent.com/controlled"


def metadata(row: object = ROW, *, tag: str = "v1.2.3") -> bytes:
    return json.dumps({"tag_name": tag, "assets": [row]}).encode()


class ReleaseAssetTests(unittest.TestCase):
    def test_exact_tag_and_name_resolve_asset_and_stream_actual_bytes(self) -> None:
        target = io.BytesIO()
        with patch.object(assets.urllib.request, "build_opener") as build:
            build.return_value.open.side_effect = [Response(metadata()), Response(PAYLOAD)]
            assets.download_asset(URL, target, 100)
        self.assertEqual(target.getvalue(), PAYLOAD)
        calls = build.return_value.open.call_args_list
        self.assertEqual([c.args[0].full_url for c in calls], [TAG_URL, ASSET_URL])
        self.assertEqual([c.kwargs["timeout"] for c in calls], [30, 30])
        self.assertEqual(calls[1].args[0].get_header("Accept"), "application/octet-stream")
        self.assertIsNone(calls[1].args[0].get_header("Authorization"))

    def test_wrong_release_or_malformed_asset_identity_never_downloads_payload(self) -> None:
        variants = [b"not json", b"[" * 2000 + b"0" + b"]" * 2000, b"[]", b"{}", metadata(tag="v9"), metadata(None),
                    json.dumps({"tag_name": "v1.2.3", "assets": [ROW, ROW]}).encode()]
        for key, value in (("id", True), ("id", 0), ("id", "17"), ("size", 0),
                           ("size", 101), ("size", True), ("state", "open"), ("name", "other")):
            variants.append(metadata({**ROW, key: value}))
        for raw in variants:
            with self.subTest(raw=raw), patch.object(assets.urllib.request, "build_opener") as build:
                build.return_value.open.return_value = Response(raw)
                target = io.BytesIO()
                with self.assertRaises(assets.DownloadError):
                    assets.download_asset(URL, target, 100)
                self.assertEqual(build.return_value.open.call_count, 1)
                self.assertEqual(target.getvalue(), b"")

    def test_unrelated_asset_and_additive_fields_do_not_change_exact_selection(self) -> None:
        raw = json.dumps({"tag_name": "v1.2.3", "new_field": {},
                          "assets": [{**ROW, "name": "other.tar.gz", "id": 18}, ROW]}).encode()
        with patch.object(assets.urllib.request, "build_opener") as build:
            build.return_value.open.side_effect = [Response(raw), Response(PAYLOAD)]
            target = io.BytesIO()
            assets.download_asset(URL, target, 100)
        self.assertEqual(target.getvalue(), PAYLOAD)
        self.assertEqual(build.return_value.open.call_args.args[0].full_url, ASSET_URL)

    def test_http_rate_limit_and_transport_faults_remain_explicit_without_retry(self) -> None:
        for error in (urllib.error.HTTPError(TAG_URL, 403, "limited", HTTPMessage(), None),
                      urllib.error.URLError("unreachable")):
            with self.subTest(error=error), patch.object(assets.urllib.request, "build_opener") as build:
                build.return_value.open.side_effect = error
                with self.assertRaises(assets.DownloadError) as raised:
                    assets.download_asset(URL, io.BytesIO(), 100)
                self.assertIs(raised.exception.__cause__, error)
                self.assertEqual(build.return_value.open.call_count, 1)

    def test_protocol_failures_keep_cause_and_cli_error_without_protocol_body_or_traceback(self) -> None:
        marker = "untrusted-protocol-body"
        for error in (BadStatusLine(marker), IncompleteRead(marker.encode(), 42)):
            class BrokenResponse(Response):
                def read(self, size: int | None = -1, /) -> bytes:
                    raise error

            def responses():
                return ([error] if isinstance(error, BadStatusLine)
                        else [Response(metadata()), BrokenResponse()])

            with self.subTest(error=type(error).__name__), patch.object(
                    assets.urllib.request, "build_opener") as build:
                build.return_value.open.side_effect = responses()
                target = io.BytesIO()
                with self.assertRaisesRegex(assets.DownloadError, "API transport failed") as raised:
                    assets.download_asset(URL, target, 100)
                self.assertIs(raised.exception.__cause__, error)
                self.assertEqual(target.getvalue(), b"")
                build.return_value.open.side_effect = responses()
                with patch.object(assets.sys, "argv", ["github_release_asset.py", "--url", URL,
                                                       "--max-bytes", "100"]), patch(
                        "sys.stderr", new_callable=io.StringIO) as diagnostic:
                    self.assertEqual(assets.main(), 1)
                self.assertIn("API transport failed", diagnostic.getvalue())
                self.assertNotIn(marker, diagnostic.getvalue())
                self.assertNotIn("Traceback", diagnostic.getvalue())

    def test_metadata_and_payload_have_separate_bounds_and_exact_payload_length(self) -> None:
        with patch.object(assets, "METADATA_LIMIT", 2), patch.object(
                assets.urllib.request, "build_opener") as build:
            build.return_value.open.return_value = Response(b"xxx")
            with self.assertRaisesRegex(assets.DownloadError, "metadata exceeded"):
                assets.download_asset(URL, io.BytesIO(), 100)
            self.assertEqual(build.return_value.open.call_count, 1)
        for body in (b"", PAYLOAD[:-1], PAYLOAD + b"extra" * 30):
            with self.subTest(body=body), patch.object(assets.urllib.request, "build_opener") as build:
                build.return_value.open.side_effect = [Response(metadata()), Response(body)]
                with self.assertRaises(assets.DownloadError):
                    assets.download_asset(URL, io.BytesIO(), 100)

    def test_https_required_for_initial_url_and_both_final_responses(self) -> None:
        for url in (URL.replace("https:", "http:"), URL.replace("github.com", "other.example"),
                    URL + "?download=1", URL + "#fragment", URL.replace("tool.tar.gz", "bad%2Fname"),
                    URL.replace("v1.2.3", ".."), URL.replace("tool.tar.gz", "%2e%2e")):
            with self.subTest(url=url), patch.object(assets.urllib.request, "build_opener") as build:
                with self.assertRaises(assets.DownloadError):
                    assets.download_asset(url, io.BytesIO(), 100)
                build.assert_not_called()
        for index in (0, 1):
            with self.subTest(index=index), patch.object(assets.urllib.request, "build_opener") as build:
                responses = [Response(metadata()), Response(PAYLOAD)]
                responses[index].url = "http://downgrade.example/asset"
                build.return_value.open.side_effect = responses
                with self.assertRaisesRegex(assets.DownloadError, "HTTPS"):
                    assets.download_asset(URL, io.BytesIO(), 100)
        request = assets.urllib.request.Request(TAG_URL)
        with self.assertRaisesRegex(assets.DownloadError, "HTTPS"):
            assets.HTTPSRedirects().redirect_request(request, io.BytesIO(), 302, "redirect", HTTPMessage(),
                                                    "http://example/asset")

    def test_output_failure_is_not_hidden_or_retried(self) -> None:
        class BrokenWriter(io.BytesIO):
            def write(self, buffer: Buffer, /) -> int:
                raise OSError("output disk failed")

        with patch.object(assets.urllib.request, "build_opener") as build:
            build.return_value.open.side_effect = [Response(metadata()), Response(PAYLOAD)]
            with self.assertRaisesRegex(OSError, "output disk failed"):
                assets.download_asset(URL, BrokenWriter(), 100)
            self.assertEqual(build.return_value.open.call_count, 2)

    def test_only_transient_open_failures_are_eligible(self) -> None:
        yes = [urllib.error.HTTPError(URL, 504, "timeout", HTTPMessage(), None),
               urllib.error.URLError(TimeoutError()), ConnectionResetError()]
        no = [urllib.error.HTTPError(URL, 404, "missing", HTTPMessage(), None),
              urllib.error.HTTPError(URL, 403, "denied", HTTPMessage(), None),
              urllib.error.URLError(ssl.SSLCertVerificationError()), OSError("disk"), ValueError()]
        self.assertTrue(all(assets.transient_error(x) for x in yes))
        self.assertFalse(any(assets.transient_error(x) for x in no))


if __name__ == "__main__":
    unittest.main()
