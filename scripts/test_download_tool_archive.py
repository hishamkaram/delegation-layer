"""Run the real curl adapter/API helper with finite offline transport fixtures."""

from pathlib import Path
import os
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent
URL = "https://github.com/example/tools/releases/download/v1.2.3/tool.tar.gz"
PAYLOAD = b"source-authored archive fixture"

PYTHON_TRANSPORT = r'''
import io, json, os, runpy, sys, urllib.error, urllib.request
from pathlib import Path
expected_bound = os.environ.get("TEST_EXPECTED_BOUND")
if expected_bound is not None and sys.argv[sys.argv.index("--max-bytes") + 1] != expected_bound:
    raise AssertionError("API did not receive the normalized byte bound")
class Response(io.BytesIO):
    url = "https://release-assets.githubusercontent.com/fixture"
class Opener:
    def open(self, request, timeout):
        url = request.full_url
        token = os.environ.get("DELEGATE_TOOL_DOWNLOAD_TOKEN")
        expected = "Bearer " + token if token else None
        if request.get_header("Authorization") != expected:
            raise AssertionError("API authentication does not match the explicit fixture token")
        with open(os.environ["TEST_API_CALLS"], "a") as calls:
            calls.write(url + "\n")
        if os.environ.get("TEST_API_FAIL") == "1":
            raise urllib.error.HTTPError(url, 503, "unavailable", {}, None)
        repo = os.environ.get("TEST_REPOSITORY", "example/tools")
        tag = os.environ.get("TEST_TAG", "v1.2.3")
        name = os.environ.get("TEST_ASSET", "tool.tar.gz")
        payload = Path(os.environ["TEST_PAYLOAD"]).read_bytes()
        base = "https://api.github.com/repos/" + repo + "/releases"
        if url == base + "/tags/" + tag:
            return Response(json.dumps({"tag_name": tag, "assets": [{
                "name": name, "id": 17, "size": len(payload), "state": "uploaded"}]}).encode())
        if url == base + "/assets/17" and request.get_header("Accept") == "application/octet-stream":
            return Response(payload)
        raise AssertionError("unexpected API request: " + url)
urllib.request.build_opener = lambda *handlers: Opener()
sys.argv = sys.argv[1:]
runpy.run_path(sys.argv[0], run_name="__main__")
'''


class ToolArchiveTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="tool-download-unit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.bin = self.root / "commands"
        self.bin.mkdir()
        self.payload = self.root / "payload"
        self.payload.write_bytes(PAYLOAD)
        self.calls = self.root / "api-calls"
        self.env = {**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
                    "TEST_PAYLOAD": str(self.payload), "TEST_API_CALLS": str(self.calls),
                    "TEST_CURL_EXIT": "22", "TEST_HTTP_STATUS": "504",
                    "TEST_CURL_RECORD": str(self.root / "curl-arguments")}
        self.env.pop("DELEGATE_TOOL_DOWNLOAD_TOKEN", None)
        self.program("python3", f"#!{sys.executable}\n" + PYTHON_TRANSPORT)
        self.program("curl", '''#!/bin/sh
printf '%s\\n' "$@" >"$TEST_CURL_RECORD"
target=
while [ "$#" -gt 0 ]; do
    if [ "$1" = -o ]; then target="$2"; shift 2; else shift; fi
done
if [ "$TEST_CURL_EXIT" = 0 ]; then
    cat "$TEST_PAYLOAD" >"$target"
else
    printf partial >"$target"
fi
printf '%s' "$TEST_HTTP_STATUS"
exit "$TEST_CURL_EXIT"
''')

    def program(self, name, source):
        path = self.bin / name
        path.write_text(source)
        path.chmod(0o755)

    def run_download(self, max_bytes="1024"):
        self.output = self.root / "download"
        return subprocess.run([str(ROOT / "scripts/download-tool-archive.sh"), URL,
                               str(self.output), max_bytes], env=self.env, capture_output=True,
                              text=True, check=False, timeout=10)

    def test_primary_success_never_uses_api(self):
        self.env.update(TEST_CURL_EXIT="0", TEST_HTTP_STATUS="200")
        result = self.run_download()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.output.read_bytes(), PAYLOAD)
        self.assertFalse(self.calls.exists())

    def test_successful_primary_output_obeys_the_byte_limit_even_if_curl_does_not(self):
        self.env.update(TEST_CURL_EXIT="0", TEST_HTTP_STATUS="200")
        limit = len(PAYLOAD)
        for size, expected in ((limit - 1, 0), (limit, 0), (limit + 1, 63)):
            with self.subTest(size=size):
                self.payload.write_bytes(b"x" * size)
                result = self.run_download(str(limit))
                self.assertEqual(result.returncode, expected, result.stderr)
                arguments = (self.root / "curl-arguments").read_text().splitlines()
                self.assertIn("--max-filesize", arguments)
                self.assertEqual(arguments[arguments.index("--max-filesize") + 1], str(limit))
                self.assertEqual(self.output.stat().st_size, size)
                self.assertFalse(self.calls.exists())
                if expected:
                    self.assertIn("exceeds MAX_BYTES", result.stderr)

    def test_invalid_limits_fail_before_transport_and_preserve_caller_output(self):
        self.output = self.root / "download"
        self.output.write_bytes(b"caller-owned output")
        for limit in ("", "0", "+000", "-1", "ten", "1.5", "１２", "9223372036854775808", "1" * 100):
            with self.subTest(limit=limit):
                result = self.run_download(limit)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("MAX_BYTES", result.stderr)
                self.assertFalse((self.root / "curl-arguments").exists())
                self.assertFalse(self.calls.exists())
                self.assertEqual(self.output.read_bytes(), b"caller-owned output")

    def test_limits_are_normalized_without_octal_or_signed_integer_overflow(self):
        self.env.update(TEST_CURL_EXIT="0", TEST_HTTP_STATUS="200")
        self.payload.write_bytes(b"small")
        for limit, normalized in (("+010", "10"), ("00010", "10"),
                                  ("0" * 30 + "10", "10"),
                                  ("9223372036854775807", "9223372036854775807")):
            with self.subTest(limit=limit):
                result = self.run_download(limit)
                self.assertEqual(result.returncode, 0, result.stderr)
                arguments = (self.root / "curl-arguments").read_text().splitlines()
                self.assertIn("--max-filesize", arguments)
                self.assertEqual(arguments[arguments.index("--max-filesize") + 1], normalized)
                self.assertEqual(self.output.read_bytes(), b"small")
                self.assertFalse(self.calls.exists())

    def test_curl_size_refusal_keeps_exit_and_partial_output_without_api(self):
        self.env.update(TEST_CURL_EXIT="63", TEST_HTTP_STATUS="200")
        result = self.run_download()
        self.assertEqual(result.returncode, 63, result.stderr)
        self.assertFalse(self.calls.exists())
        self.assertEqual(self.output.read_bytes(), b"partial")

    def test_fallback_receives_the_same_normalized_limit(self):
        self.payload.write_bytes(b"small")
        self.env["TEST_EXPECTED_BOUND"] = "10"
        result = self.run_download("+010")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.output.read_bytes(), b"small")
        self.assertEqual(len(self.calls.read_text().splitlines()), 2)

    def test_explicit_token_is_only_used_by_api_and_never_curl_argv_or_diagnostics(self):
        token = "source-authored-tool-download-token"
        self.env["DELEGATE_TOOL_DOWNLOAD_TOKEN"] = token
        result = self.run_download()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.output.read_bytes(), PAYLOAD)
        self.assertEqual(len(self.calls.read_text().splitlines()), 2)
        arguments = (self.root / "curl-arguments").read_text()
        self.assertNotIn(token, arguments)
        self.assertNotIn("Authorization", arguments)
        self.assertNotIn(token, result.stdout + result.stderr)

    def test_transient_failure_fallback_replaces_partial_primary_bytes(self):
        for code, http_status in (("22", "504"), ("28", "000")):
            with self.subTest(code=code):
                self.calls.unlink(missing_ok=True)
                self.env.update(TEST_CURL_EXIT=code, TEST_HTTP_STATUS=http_status)
                result = self.run_download()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("trying the exact pinned asset", result.stderr)
                self.assertEqual(self.output.read_bytes(), PAYLOAD)
                self.assertEqual(self.calls.read_text().splitlines(), [
                    "https://api.github.com/repos/example/tools/releases/tags/v1.2.3",
                    "https://api.github.com/repos/example/tools/releases/assets/17"])

    def test_permanent_certificate_and_output_failures_keep_original_exit_without_api(self):
        for code, http_status in (("22", "404"), ("22", "403"), ("23", "200"),
                                  ("60", "000"), ("99", "")):
            with self.subTest(code=code, status=http_status):
                self.env.update(TEST_CURL_EXIT=code, TEST_HTTP_STATUS=http_status)
                result = self.run_download()
                self.assertEqual(result.returncode, int(code), result.stderr)
                self.assertFalse(self.calls.exists())

    def test_api_failure_retains_both_causes_and_does_not_loop(self):
        self.env["TEST_API_FAIL"] = "1"
        result = self.run_download()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("curl 22, HTTP 504", result.stderr)
        self.assertIn("HTTP 503", result.stderr)
        self.assertEqual(len(self.calls.read_text().splitlines()), 1)

    def test_real_installer_still_rejects_unpinned_api_bytes_before_extracting(self):
        repo = self.root / "repo"
        scripts = repo / "scripts"
        scripts.mkdir(parents=True)
        for name in ("install-tools.sh", "download-tool-archive.sh", "github_release_asset.py"):
            shutil.copyfile(ROOT / "scripts" / name, scripts / name)
            (scripts / name).chmod(0o755)
        pin = (ROOT / ".go-version").read_text()
        (repo / ".go-version").write_text(pin)
        self.program("go", f"#!/bin/sh\nprintf '%s\\n' 'go version go{pin.strip()} linux/amd64'\n")
        self.program("uname", "#!/bin/sh\nif [ \"$1\" = -s ]; then echo Linux; else echo x86_64; fi\n")
        self.env.update(GOTOOLCHAIN="local", TEST_REPOSITORY="golangci/golangci-lint",
                        TEST_TAG="v2.14.0", TEST_ASSET="golangci-lint-2.14.0-linux-amd64.tar.gz",
                        DELEGATE_TOOL_DOWNLOAD_TOKEN="checksum-fixture-token")
        result = subprocess.run(["bash", str(scripts / "install-tools.sh")], env=self.env,
                                capture_output=True, text=True, check=False, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SHA256 mismatch", result.stderr)
        self.assertFalse((repo / "bin/golangci-lint").exists())
        self.assertEqual(len(self.calls.read_text().splitlines()), 2)


if __name__ == "__main__":
    unittest.main()
