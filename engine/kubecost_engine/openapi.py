"""OpenAPI contract for the enterprise extension; legacy API remains compatible."""


def specification():
    paths = {}
    reads = (
        "alerts",
        "users",
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
        "maintenance/retention", "alerts/ack",
        "users", "users/update", "users/credentials",
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
    )
    required = {
        "maintenance/retention": ["days"],
        "alerts/ack": ["id"],
        "users": ["id", "role"],
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
                        (413, "Payload exceeds 1 MiB"),
                        (429, "Rate limited"),
                        (503, "Required backend unavailable"),
                    )
                },
            }
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
            paths.setdefault("/v1/" + route, {})[method] = operation
    return {
        "openapi": "3.0.3",
        "info": {
            "title": "KubeCost Auto-HPA enterprise extension",
            "version": "0.2.0-dev",
            "description": "Enterprise extension only. Approval is not Kubernetes execution. Financial imports implement a documented FOCUS subset.",
        },
        "paths": paths,
        "components": {
            "securitySchemes": {"bearerAuth": {"type": "http", "scheme": "bearer"}}
        },
    }
