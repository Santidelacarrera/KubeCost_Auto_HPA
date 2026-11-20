import json
import os
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts/check_credential_expiry.py"
TEST_CREDENTIAL = "local-test-credential"


def run_check(url, *args):
    response = subprocess.run(
        [sys.executable, str(SCRIPT), "--url", url, *args],
        env={**os.environ, "KCA_CREDENTIAL": TEST_CREDENTIAL},
        capture_output=True,
        text=True,
        timeout=15,
        check=False,
    )
    assert TEST_CREDENTIAL not in response.stdout
    assert response.stderr == ""
    return response.returncode, json.loads(response.stdout)


@pytest.fixture
def endpoint():
    calls = []
    response = {}

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            calls.append((self.path, self.headers.get("Authorization")))
            self.send_response(response["status"])
            if "location" in response:
                self.send_header("Location", response["location"])
            self.end_headers()
            self.wfile.write(json.dumps(response["body"]).encode())

        def log_message(self, *_args):
            pass

    with HTTPServer(("127.0.0.1", 0), Handler) as server:
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
        thread.start()
        try:
            yield f"http://127.0.0.1:{server.server_port}", response, calls
        finally:
            server.shutdown()
            thread.join(timeout=5)


def test_check_outputs_and_warning_boundary(endpoint):
    url, response, calls = endpoint
    response.update(status=200, body={"credential_type": "api_key", "expires_at": 1800000000,
                                      "remaining_seconds": 86400, "unexpected": TEST_CREDENTIAL})
    assert run_check(url, "--warn-days", "0")[0] == 0
    assert run_check(url, "--warn-days", "1") == (
        1, {"status": "warning", "credential_type": "api_key", "expires_at": 1800000000, "remaining_seconds": 86400}
    )
    response["body"] = {"credential_type": "engine_token", "expires_at": None, "remaining_seconds": None}
    assert run_check(url)[0] == 2
    response.update(status=401, body={"error": TEST_CREDENTIAL})
    assert run_check(url) == (3, {"status": "error", "http_status": 401})
    assert calls == [("/v1/auth/credential-expiry", f"Bearer {TEST_CREDENTIAL}")] * 4


def test_check_does_not_follow_redirects_or_send_credentials_over_nonlocal_http(endpoint):
    url, response, calls = endpoint
    response.update(status=302, location=url + "/capture", body={})
    assert run_check(url) == (3, {"status": "error", "http_status": 302})
    assert run_check("http://example.com")[0] == 3
    assert run_check(url, "--warn-days", TEST_CREDENTIAL)[0] == 3
    assert calls == [("/v1/auth/credential-expiry", f"Bearer {TEST_CREDENTIAL}")]
