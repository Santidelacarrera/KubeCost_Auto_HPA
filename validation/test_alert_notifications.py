"""Send the chart's synthetic alert through a disposable Alertmanager to a test receiver."""

from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid

import pytest


@pytest.mark.skipif(os.getenv("KCA_ALERTMANAGER_SMOKE") != "1", reason="requires Helm and Docker")
def test_chart_webhook_sends_firing_and_resolved(tmp_path):
    import yaml

    chart = Path(__file__).resolve().parents[1] / "deploy" / "helm" / "kubecost-auto-hpa"
    rendered = subprocess.check_output([
        "helm", "template", "smoke", str(chart), "--namespace", "lab",
        "--set", "prometheusRule.enabled=true", "--set", "alertNotifications.enabled=true",
        "--set", "alertNotifications.webhookSecretName=test-secret",
        "--set", "alertNotifications.testAlert=true",
    ], text=True, encoding="utf-8", timeout=20)
    objects = {obj["kind"]: obj for obj in yaml.safe_load_all(rendered) if obj}
    routing = objects["AlertmanagerConfig"]["spec"]
    webhook = routing["receivers"][0]["webhookConfigs"][0]
    assert webhook["urlSecret"] == {"name": "test-secret", "key": "url"}
    rule = next(rule for group in objects["PrometheusRule"]["spec"]["groups"] for rule in group["rules"]
                if rule["alert"] == "KubeCostNotificationTest")
    received = queue.Queue()

    class Receiver(BaseHTTPRequestHandler):
        def do_POST(self):
            received.put(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
            self.send_response(204)
            self.end_headers()

    server = ThreadingHTTPServer(("0.0.0.0", 0), Receiver)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    container = "kca-alert-" + uuid.uuid4().hex[:8]
    config = tmp_path / "alertmanager.yml"
    route = routing["route"]
    # Simulate the Operator's namespace matcher and Secret resolution for the
    # native Alertmanager configuration. Shorter timers keep this smoke fast.
    config.write_text(yaml.safe_dump({
        "route": {"receiver": "discard", "routes": [{
            "receiver": route["receiver"], "group_by": route["groupBy"],
            "group_wait": "1s", "group_interval": "2s", "repeat_interval": route["repeatInterval"],
            "matchers": ["namespace=\"lab\""] + [
                f"{m['name']}{m['matchType']}{json.dumps(m['value'])}" for m in route["matchers"]
            ],
        }]},
        "receivers": [{"name": "discard"}, {"name": route["receiver"], "webhook_configs": [{
            "url": f"http://host.docker.internal:{server.server_port}/",
            "send_resolved": webhook["sendResolved"],
        }]}],
    }), encoding="utf-8")
    try:
        host = [] if os.name == "nt" else ["--add-host", "host.docker.internal:host-gateway"]
        subprocess.run([
            "docker", "run", "-d", "--name", container, *host,
            "-p", "127.0.0.1::9093", "-v", f"{config}:/etc/alertmanager/alertmanager.yml:ro",
            "quay.io/prometheus/alertmanager:v0.28.1",
        ], check=True, timeout=180)
        port = subprocess.check_output(["docker", "port", container, "9093/tcp"], text=True, timeout=15).strip().rsplit(":", 1)[1]
        base = f"http://127.0.0.1:{port}"
        for _ in range(30):
            try:
                with urllib.request.urlopen(base + "/-/ready", timeout=2):
                    break
            except (OSError, urllib.error.URLError):
                time.sleep(1)
        else:
            pytest.fail("Alertmanager did not become ready")
        now = datetime.now(timezone.utc)
        labels = {"alertname": rule["alert"], **rule["labels"]}

        def send(end, alert_labels):
            payload = [{"labels": alert_labels, "startsAt": now.isoformat(), "endsAt": end.isoformat()}]
            req = urllib.request.Request(base + "/api/v2/alerts", data=json.dumps(payload).encode(),
                                         headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=5):
                pass

        send(now + timedelta(minutes=2), {**labels, "kubecost_release": "other"})
        send(now + timedelta(minutes=2), labels)
        assert received.get(timeout=15)["status"] == "firing"
        send(datetime.now(timezone.utc), labels)
        resolved = received.get(timeout=15)
        assert resolved["status"] == "resolved"
        assert all(alert["labels"]["kubecost_release"] == "smoke" for alert in resolved["alerts"])
        assert received.empty()
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
        subprocess.run(["docker", "rm", "-f", container], timeout=30, capture_output=True)
