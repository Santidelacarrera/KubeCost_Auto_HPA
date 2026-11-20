"""OpenAPI contract for the enterprise extension; legacy API remains compatible."""


def specification():
    paths = {}
    reads = (
        "billing/checkout",
        "alerts",
        "users",
        "users/me",
        "usage",
        "subscriptions",
        "clusters",
        "plans",
        "rightsizing",
        "predictions",
        "predictions/backtests",
        "finops/costs",
        "finops/reconciliations",
        "consolidation/plans",
        "licenses",
    )
    writes = (
        "billing/checkout",
        "maintenance/retention", "alerts/ack",
        "users", "users/me", "users/update", "users/credentials",
        "subscriptions/sandbox",
        "rightsizing",
        "predictions",
        "predictions/observe",
        "predictions/backtest",
        "licenses/revoke",
        "finops/import",
        "finops/reconcile",
        "licenses/activate",
        "consolidation/plans",
        "consolidation/decision",
        "consolidation/execution",
        "agents/register",
        "policies",
    )
    required = {
        "billing/checkout": ["plan", "request_id"],
        "maintenance/retention": ["days"],
        "alerts/ack": ["id"],
        "users": ["id", "role"],
        "users/me": ["display_name", "version"],
        "users/update": ["id", "role", "status", "version"],
        "users/credentials": ["id"],
        "licenses/revoke": ["id", "version", "reason"],
        "predictions/backtest": ["cluster_id", "workload", "samples"],
        "subscriptions/sandbox": ["action", "event_id"],
        "rightsizing": ["cluster_id", "workload", "samples", "current"],
        "predictions": ["cluster_id", "workload", "samples"],
        "predictions/observe": ["id", "timestamp", "observed_demand"],
        "finops/import": ["provider", "records"],
        "finops/reconcile": ["invoice"],
        "licenses/activate": ["license"],
        "consolidation/plans": ["cluster_id", "nodes"],
        "consolidation/decision": ["id", "version", "decision"],
        "consolidation/execution": ["id", "version", "node", "action"],
        "agents/register": ["cluster_id"],
        "policies": ["cluster_id"],
    }
    for method, routes in (("get", reads), ("post", writes)):
        for route in routes:
            operation = {
                "operationId": method + "_" + route.replace("/", "_"),
                "summary": route,
                "security": [{"bearerAuth": []}],
                "responses": {
                    str(code): {"description": text}
                    for code, text in (
                        (200, "Result; unknown financial values are null"),
                        (400, "Invalid input or state conflict"),
                        (401, "Authentication required"),
                        (403, "Organization or scope denied"),
                        (408, "Request body idle timeout"),
                        (413, "Payload exceeds 1 MiB"),
                        (429, "Rate limited"),
                        (503, "Required backend unavailable"),
                    )
                },
            }
            if route == "users/me":
                operation["description"] = "Read your account profile or correct your display name with a user-bound key."
                properties = {field: {"type": "string"} for field in ("id", "display_name", "role", "status")}
                properties.update(created_at={"type": "number"}, version={"type": "integer"})
                operation["responses"]["200"] = {"description": "Current account profile", "content": {
                    "application/json": {"schema": {"type": "object", "required": list(properties), "properties": properties}}
                }}
            if method == "post":
                operation["requestBody"] = {
                    "required": True,
                    "content": {
                        "application/json": {
                            "schema": {
                                "type": "object",
                                "required": required[route],
                                "properties": {
                                    key: {
                                        "type": "array"
                                        if key in ("samples", "records", "nodes")
                                        else "object"
                                        if key in ("invoice", "current")
                                        else "integer"
                                        if key in ("version", "days")
                                        else "number"
                                        if key in ("timestamp", "observed_demand")
                                        else "string"
                                    }
                                    for key in required[route]
                                },
                            }
                        }
                    },
                }
                for prop in operation["requestBody"]["content"]["application/json"][
                    "schema"
                ]["properties"].values():
                    if prop["type"] == "array":
                        prop["items"] = {"type": "object"}
                schema = operation["requestBody"]["content"]["application/json"]["schema"]
                if route == "users/me":
                    schema["additionalProperties"] = False
                    schema["properties"].update(client_id={"type": "string"}, display_name={"type": "string", "minLength": 1, "maxLength": 120})
                if route in ("agents/register", "policies"):
                    schema["properties"].update(
                        execution_mode={"type": "string", "enum": ["OBSERVE_ONLY", "ASSISTED", "AUTOMATED"]},
                    )
                if route == "agents/register":
                    schema["properties"]["cloud_provider"] = {"type": "string"}
                if route == "consolidation/execution":
                    schema["properties"]["action"]["enum"] = ["claim", "cordoned", "failed", "recovering", "recovered"]
                    operation["description"] = "Requires consolidation:execute. Records an executor claim or outcome."
                if route == "consolidation/plans":
                    schema["properties"]["execution_strategy"] = {
                        "type": "string", "enum": ["empty-only", "stateless-eviction"], "default": "empty-only"
                    }
                if route == "rightsizing":
                    keys = ("cpu_request_millicores", "cpu_limit_millicores", "memory_request_mib", "memory_limit_mib")
                    schema["properties"]["current"].update(
                        required=list(keys),
                        properties={key: {"type": "number", "minimum": 1} for key in keys},
                    )
                    operation["description"] = "Recommendations never lower existing CPU/memory limits. No Kubernetes mutation occurs."
                    schema["properties"]["samples"]["items"] = {
                        "type": "object", "required": ["timestamp", "cpu_millicores", "memory_mib"],
                        "properties": {
                            **{key: {"type": "number", "minimum": 0} for key in ("timestamp", "cpu_millicores", "memory_mib")},
                            "oom_killed": {"type": "boolean", "description": "When supplied, ambiguous strings/numbers/null are rejected"},
                        },
                    }
                    schema["properties"]["policy"] = {
                        "type": "object", "properties": {
                            "window_seconds": {"type": "number", "minimum": 60, "maximum": 2592000, "default": 86400},
                            "min_samples": {"type": "number", "minimum": 3, "maximum": 10000, "default": 20},
                            "headroom": {"type": "number", "minimum": 1.1, "maximum": 3, "default": 1.3},
                        },
                    }
                if route == "finops/import":
                    schema["properties"]["provider"]["enum"] = ["focus", "gcp", "azure"]
                    schema["properties"]["records"].update(minItems=1, maxItems=5000)
                    schema["properties"]["records"]["items"]["properties"] = {
                        key: {"type": "string", "nullable": True, "minLength": 1, "maxLength": 256}
                        for key in ("cluster_id", "namespace", "workload", "node")
                    }
                if route == "maintenance/retention":
                    schema["properties"].update(days={"type": "integer", "minimum": 1, "maximum": 3650}, dry_run={"type": "boolean", "default": True})
                if route in ("rightsizing", "predictions", "predictions/backtest"):
                    schema["required"].remove("samples")
                    schema["properties"].update(source={"type": "string", "enum": ["telemetry", "submitted"]}, namespace={"type": "string"}, pod_name={"type": "string"}, container_name={"type": "string"})
                    schema["oneOf"] = [
                        {"required": ["samples"], "properties": {"source": {"enum": ["submitted"]}}},
                        {"required": ["source", "namespace", "pod_name", "container_name"], "properties": {"source": {"enum": ["telemetry"]}}},
                    ]
            else:
                operation["parameters"] = [
                    {
                        "name": "limit",
                        "in": "query",
                        "schema": {
                            "type": "integer",
                            "minimum": 1,
                            "maximum": 200,
                            "default": 50,
                        },
                    },
                    {
                        "name": "offset",
                        "in": "query",
                        "schema": {"type": "integer", "minimum": 0, "default": 0},
                    },
                    {"name": "client_id", "in": "query", "schema": {"type": "string"}},
                ]
                if route == "consolidation/plans":
                    operation["parameters"].append(
                        {"name": "id", "in": "query", "schema": {"type": "string"}}
                    )
                if route == "billing/checkout":
                    operation["parameters"] = [
                        {"name": "request_id", "in": "query", "required": True, "schema": {"type": "string"}},
                        {"name": "client_id", "in": "query", "schema": {"type": "string"}},
                    ]
            paths.setdefault("/v1/" + route, {})[method] = operation
    legacy_get = (
        "policies commands/pending audit/decisions telemetry/events ui/cast-dashboard "
        "pricing/lookup billing/invoice auth/keys gpu/optimize greenops/report finops/guardrail "
        "gameday/simulate system/readiness cast/commitments cast/hibernation cast/network-storage "
        "cast/security cast/onboarding cast/full-suite cluster/topology openapi.json"
    ).split()
    legacy_post = (
        "evaluate cluster/binpack auth/keys auth/keys/revoke ui/recommendations/execute "
        "agents/heartbeat telemetry/ingest commands/prewarm commands/ack audit/decisions "
        "gpu/optimize finops/guardrail gameday/simulate cast/commitments cast/hibernation "
        "cast/network-storage billing/stripe-webhook"
    ).split()
    from .legacy_contract import describe

    for method, routes in (("get", legacy_get), ("post", legacy_post)):
        for route in routes:
            operation = {
                "operationId": method + "_" + route.replace("/", "_").replace(".", "_"),
                "tags": ["Legacy API"],
                "security": [{"bearerAuth": []}],
                "responses": {"200": {"description": "Result", "content": {
                    "application/json": {"schema": {"type": "object"}}}}},
            }
            if method == "post":
                operation["requestBody"] = {"required": True, "content": {
                    "application/json": {"schema": {"type": "object"}}}}
            if route == "billing/stripe-webhook":
                operation["security"] = [{"stripeSignature": []}]
            describe(method, route, operation)
            paths.setdefault("/v1/" + route, {})[method] = operation
    return {
        "openapi": "3.0.3",
        "info": {
            "title": "KubeCost Auto-HPA enterprise extension",
            "version": "0.2.0-dev",
            "description": "Enterprise and legacy routes. Financial imports implement a documented FOCUS subset.",
        },
        "paths": paths,
        "components": {
            "securitySchemes": {"bearerAuth": {"type": "http", "scheme": "bearer"},
                                "stripeSignature": {"type": "apiKey", "in": "header", "name": "Stripe-Signature"}}
        },
    }
