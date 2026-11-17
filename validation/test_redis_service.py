"""Run explicitly against a disposable Redis; never flush shared databases."""

import os
import uuid

from kubecost_engine.security import DistributedRateLimiter


def test_real_redis_atomic_limit_across_instances(monkeypatch):
    url = os.environ["TEST_REDIS_URL"]
    tenant = "test-" + uuid.uuid4().hex
    monkeypatch.setenv("RATE_LIMIT_COMPUTE_PER_MIN", "3")
    first, second = (
        DistributedRateLimiter(redis_url=url),
        DistributedRateLimiter(redis_url=url),
    )
    assert first._get_redis_client() is not None
    assert first.check_request(
        "/v1/evaluate", "POST", "127.0.0.1", tenant, "test"
    ).allowed
    assert second.check_request(
        "/v1/evaluate", "POST", "127.0.0.1", tenant, "test"
    ).allowed
    assert first.check_request(
        "/v1/evaluate", "POST", "127.0.0.1", tenant, "test"
    ).allowed
    assert not second.check_request(
        "/v1/evaluate", "POST", "127.0.0.1", tenant, "test"
    ).allowed
