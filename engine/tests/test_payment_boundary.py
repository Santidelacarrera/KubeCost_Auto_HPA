import pytest

from kubecost_engine.enterprise import PLANS
from kubecost_engine.persistence import SQLiteSaaSStore
from kubecost_engine.subscriptions import transition, LocalSandboxProvider


@pytest.mark.parametrize("change", [{"status": "declined"}, {"live_transaction": True}, {"organization": "other"}, {"amount": "999"}, {"currency": "EUR"}])
def test_provider_mismatch_or_decline_does_not_activate(change):
    class Provider(LocalSandboxProvider):
        def charge(self, *args):
            return {**super().charge(*args), **change}

    store = SQLiteSaaSStore()
    with pytest.raises(ValueError):
        transition(store, "a", {"action": "activate", "event_id": "e", "plan": "Starter", "amount": "10", "currency": "USD"}, PLANS, provider=Provider())
    assert store.list_objects("a", "subscription") == []
    store.close()


def test_timeout_retry_and_changed_idempotency_payload(tmp_path):
    class Provider(LocalSandboxProvider):
        calls = 0

        def charge(self, *args):
            self.calls += 1
            if self.calls == 1:
                raise TimeoutError("simulated transport timeout")
            return super().charge(*args)

    store = SQLiteSaaSStore(str(tmp_path / "payments.db"))
    provider = Provider()
    request = {"action": "activate", "event_id": "e", "plan": "Starter", "amount": "10", "currency": "USD"}
    with pytest.raises(TimeoutError):
        transition(store, "a", request, PLANS, provider=provider)
    assert store.list_objects("a", "subscription") == []
    active = transition(store, "a", request, PLANS, provider=provider)
    assert active["status"] == "active"
    assert transition(store, "a", request, PLANS, provider=provider)["duplicate_ignored"]
    assert provider.calls == 2
    with pytest.raises(ValueError, match="idempotency"):
        transition(store, "a", {**request, "amount": "20"}, PLANS, provider=provider)
    assert provider.calls == 2
    store.close()
