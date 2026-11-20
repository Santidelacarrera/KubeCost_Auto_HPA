#!/usr/bin/env python3
"""Check the expiry of the engine credential provided in KCA_CREDENTIAL."""

import argparse
import http.client
import ipaddress
import json
import os
import re
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class CheckArguments(argparse.ArgumentParser):
    def error(self, message):
        print(json.dumps({"status": "error", "error": "invalid arguments, see --help"}))
        self.exit(3)


def endpoint(base_url: str) -> str:
    if any(ord(char) <= 32 or ord(char) == 127 for char in base_url):
        raise ValueError
    parsed = urllib.parse.urlsplit(base_url)
    if (
        parsed.scheme not in {"https", "http"}
        or not parsed.hostname
        or parsed.username is not None
        or parsed.path not in {"", "/"}
        or "?" in base_url
        or "#" in base_url
    ):
        raise ValueError
    if parsed.scheme == "http" and not ipaddress.ip_address(parsed.hostname).is_loopback:
        raise ValueError
    return base_url.rstrip("/") + "/v1/auth/credential-expiry"


def check_expiry(base_url: str, credential: str, warn_days: int) -> tuple[int, dict]:
    if not re.fullmatch(r"[!-~]+", credential):
        return 3, {"status": "error", "error": "KCA_CREDENTIAL is missing or invalid"}
    try:
        url = endpoint(base_url)
    except ValueError:
        return 3, {"status": "error", "error": "provide an HTTPS engine origin or an HTTP loopback IP origin"}

    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {credential}"})
    try:
        with opener.open(request, timeout=10) as response:
            if response.status != 200:
                return 3, {"status": "error", "http_status": response.status}
            body = response.read(4097)
            if len(body) > 4096:
                return 3, {"status": "error", "error": "invalid credential expiry response"}
            payload = json.loads(body)
    except urllib.error.HTTPError as exc:
        exc.close()
        return 3, {"status": "error", "http_status": exc.code}
    except (OSError, ValueError, http.client.HTTPException, RecursionError):
        return 3, {"status": "error", "error": "could not read the credential expiry response"}

    if not isinstance(payload, dict):
        return 3, {"status": "error", "error": "invalid credential expiry response"}
    credential_type = payload.get("credential_type")
    expiry = payload.get("expires_at")
    remaining = payload.get("remaining_seconds")
    if (
        credential_type in ("engine_token", "admin_token")
        and "expires_at" in payload
        and "remaining_seconds" in payload
        and expiry is None
        and remaining is None
    ):
        return 2, {"status": "unknown", "credential_type": credential_type, "expires_at": None, "remaining_seconds": None}
    if (
        credential_type != "api_key"
        or type(expiry) is not int
        or not 0 < expiry <= 2**63 - 1
        or type(remaining) is not int
        or not 0 <= remaining <= expiry
    ):
        return 3, {"status": "error", "error": "invalid credential expiry response"}
    warning = remaining <= warn_days * 86400
    return (1 if warning else 0), {
        "status": "warning" if warning else "ok",
        "credential_type": credential_type,
        "expires_at": expiry,
        "remaining_seconds": remaining,
    }


def main() -> int:
    parser = CheckArguments(description=__doc__)
    parser.add_argument("--url", required=True, help="Engine origin")
    parser.add_argument("--warn-days", type=int, default=14, help="Warn at or below this many days (default: 14)")
    args = parser.parse_args()
    if args.warn_days < 0:
        parser.error("--warn-days must be nonnegative")
    code, result = check_expiry(args.url, os.environ.get("KCA_CREDENTIAL", ""), args.warn_days)
    print(json.dumps(result))
    return code


if __name__ == "__main__":
    raise SystemExit(main())
