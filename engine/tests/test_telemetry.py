"""Tests for Nexus Telemetry Bus Producer (kubecost_engine.telemetry).

Verifies conformance with Nexus Security & AI Lab telemetry contract:
2_Areas/Empresa/_Contrato_Telemetria.md
"""
import json
from unittest.mock import MagicMock, patch

from kubecost_engine.telemetry import (
    DEFAULT_SOURCE,
    DEFAULT_STREAM,
    DEFAULT_TENANT_ID,
    EVENT_TYPE_FINOPS,
    NexusTelemetryClient,
)


def test_telemetry_client_no_redis_fail_open():
    """Client without Redis URL fails open and returns None without raising."""
    client = NexusTelemetryClient(redis_url=None)
    result = client.publish_event(
        type_=EVENT_TYPE_FINOPS,
        severity="info",
        payload={"workload": "api", "action": "hold"},
    )
    assert result is None


def test_telemetry_client_publishes_valid_contract():
    """Client publishes event with required fields and valid json payload."""
    mock_redis = MagicMock()
    mock_redis.xadd.return_value = "1730847600000-0"

    client = NexusTelemetryClient(redis_url="redis://localhost:6379/0")
    client._client = mock_redis

    test_payload = {
        "workload": "payments-api",
        "namespace": "production",
        "action": "grow",
        "cpu_millicores": 1500,
        "unit_cost": 0.0012,
    }

    event_id = "0192e21b-87a3-7cf0-9b43-85f2619056d9"
    ts = "2026-11-05T23:00:00Z"

    result = client.publish_event(
        type_=EVENT_TYPE_FINOPS,
        severity="info",
        payload=test_payload,
        tenant_id="tenant-1234",
        event_id=event_id,
        ts=ts,
    )

    assert result == "1730847600000-0"
    mock_redis.xadd.assert_called_once()
    stream_arg, fields_arg = mock_redis.xadd.call_args[0]
    assert stream_arg == DEFAULT_STREAM
    assert fields_arg["tenant_id"] == "tenant-1234"
    assert fields_arg["event_id"] == event_id
    assert fields_arg["timestamp"] == ts
    assert fields_arg["source"] == DEFAULT_SOURCE
    assert fields_arg["type"] == EVENT_TYPE_FINOPS
    assert fields_arg["severity"] == "info"

    parsed_payload = json.loads(fields_arg["payload"])
    assert parsed_payload["workload"] == "payments-api"
    assert parsed_payload["cpu_millicores"] == 1500


def test_publish_scaling_decision_severity_mapping():
    """Severity elevates to critical/high during OOM risk, and medium on budget frozen."""
    mock_redis = MagicMock()
    mock_redis.xadd.return_value = "msg-1"

    client = NexusTelemetryClient(redis_url="redis://localhost:6379/0")
    client._client = mock_redis

    # Normal hold
    client.publish_scaling_decision(
        decision={"action": "hold", "cpu_millicores": 1000},
        request_context={"workload": "web", "namespace": "prod"},
    )
    fields = mock_redis.xadd.call_args[0][1]
    assert fields["severity"] == "info"

    # Critical OOM risk
    client.publish_scaling_decision(
        decision={
            "action": "grow",
            "cpu_millicores": 1000,
            "oom_protection": {"is_at_risk": True, "risk_level": "critical", "action": "inflate"},
        },
        request_context={"workload": "web", "namespace": "prod"},
    )
    fields = mock_redis.xadd.call_args[0][1]
    assert fields["severity"] == "critical"
    payload = json.loads(fields["payload"])
    assert payload["oom_risk"] is True
    assert payload["oom_action"] == "inflate"

    # Budget frozen
    client.publish_scaling_decision(
        decision={"action": "hold", "cpu_millicores": 1000, "budget_frozen": True},
        request_context={"workload": "web", "namespace": "prod"},
    )
    fields = mock_redis.xadd.call_args[0][1]
    assert fields["severity"] == "medium"
