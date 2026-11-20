"""Tenant users with API credentials and enforced roles; no password/SSO login."""
import os
import time

from .security import AuthorizationError, AuthenticationError, validate_identifier

ROLES = {
    "viewer": ["enterprise:read", "telemetry:read", "policies:read", "audit:read", "billing:read"],
    "operator": ["enterprise:read", "enterprise:write", "telemetry:read", "policies:read", "policies:write", "audit:read", "billing:read", "compute:evaluate"],
    "approver": ["enterprise:read", "consolidation:approve", "audit:read"],
}
ROLES["owner"] = sorted(set(scope for scopes in ROLES.values() for scope in scopes) | {"maintenance:write", "alerts:write", "users:manage", "keys:manage", "licenses:manage", "agents:write", "consolidation:execute", "billing:write"})


def credential_user(store, tenant, key_id):
    binding = store.get_object(tenant, "user_credential", key_id)
    if binding is None:
        return None
    user = store.get_object(tenant, "user", binding["user_id"])
    if not user or user["status"] != "active":
        raise AuthenticationError("user is disabled or missing")
    return user


def own_profile(store, tenant, context, changes=None):
    prefix = f"user:{tenant}:"
    if (context.credential_type != "api_key" or context.authenticated_tenant != tenant
            or not context.principal.startswith(prefix)):
        raise AuthorizationError("a user-bound credential is required")
    user_id = context.principal[len(prefix):]
    user = store.get_object(tenant, "user", user_id)
    if not user or user["status"] != "active":
        raise AuthorizationError("active account required")
    if changes is not None:
        if set(changes) - {"display_name", "version", "client_id"}:
            raise ValueError("unexpected profile field")
        name = changes["display_name"]
        version = changes["version"]
        if not isinstance(name, str) or not 1 <= len(name.strip()) <= 120:
            raise ValueError("invalid display name")
        if type(version) is not int or version != user["version"]:
            raise ValueError("stale profile version")
        user = store.put_object(tenant, "user", user_id, {**user, "display_name": name.strip()}, version)
    return {key: user[key] for key in ("id", "display_name", "role", "status", "created_at", "version")}


def mutate(cp, auth, route, tenant, payload, context):
    from .enterprise import EnterpriseService

    service = EnterpriseService(cp)
    user_id = validate_identifier(payload["id"], "user_id")
    if context.credential_type == "api_key" and not context.has_scope("users:manage"):
        raise AuthorizationError("missing users:manage scope")
    with cp._lock:
        users = cp._store.list_objects(tenant, "user")
        matches = [user for user in users if user["id"] == user_id]
        if route == "users/credentials":
            if not matches or matches[0]["status"] != "active":
                raise ValueError("active user not found")
            scopes = ROLES[matches[0]["role"]]
            if context.credential_type == "api_key" and any(not context.has_scope(scope) for scope in scopes):
                raise AuthorizationError("cannot delegate scopes absent from caller")
            key = auth.issue_api_key(tenant, scopes=scopes, expires_in_days=payload.get("expires_in_days", 30))
            try:
                cp._store.put_object(tenant, "user_credential", key["key_id"], {"id": key["key_id"], "user_id": user_id, "created_at": time.time(), "issued_by": context.principal})
            except Exception:
                auth.revoke_api_key(tenant, key["key_id"])
                raise
            return {**key, "user_id": user_id}
        role = payload.get("role", "viewer")
        status = payload.get("status", "active")
        display_name = payload.get("display_name", user_id)
        if role not in ROLES or status not in ("active", "disabled"):
            raise ValueError("invalid user role or status")
        if not isinstance(display_name, str) or not 1 <= len(display_name.strip()) <= 120:
            raise ValueError("display_name must contain 1..120 characters")
        if context.credential_type == "api_key" and any(not context.has_scope(scope) for scope in ROLES[role]):
            raise AuthorizationError("cannot assign a role exceeding caller scopes")
        previous = matches[0] if matches else None
        if route == "users/update" and previous is None:
            raise ValueError("user not found")
        if route == "users" and previous:
            raise ValueError("user already exists")
        if status == "active" and (previous is None or previous["status"] != "active") and os.environ.get("REQUIRE_LICENSE") == "true":
            if sum(u["status"] == "active" for u in users) >= service.active_plan(tenant)["users"]:
                raise AuthorizationError("licensed user limit reached")
        history = list(previous.get("history", [])) if previous else []
        history.append({"timestamp": time.time(), "principal": context.principal, "role": role, "status": status})
        user = {"id": user_id, "display_name": display_name, "role": role, "status": status, "history": history,
                "created_at": previous["created_at"] if previous else time.time()}
        return cp._store.put_object(tenant, "user", user_id, user, int(payload["version"]) if previous else 0)
