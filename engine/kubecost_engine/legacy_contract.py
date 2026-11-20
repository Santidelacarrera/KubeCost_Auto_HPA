"""Wire types used by legacy HTTP operations in the public OpenAPI document."""

from dataclasses import MISSING, fields as dataclass_fields, is_dataclass
from typing import get_args, get_origin, get_type_hints

from .binpacking import NodeState, RelocationPlan
from .gameday import GameDayScorecard
from .gpu_optimizer import GPUOptimizationPlan
from .greenops_guardrail import FinOpsBudgetGuardrailDecision, GreenOpsCarbonReport
from .pricing import CloudInstancePricing


def fields(*, string="", integer="", number="", boolean="", objects=""):
    groups = {"string": string, "integer": integer, "number": number, "boolean": boolean, "object": objects}
    return {name: {"type": kind} for kind, names in groups.items() for name in names.split()}


def array(item):
    return {"type": "array", "items": item}


def object_schema(properties, *required):
    schema = {"type": "object", "properties": properties}
    if required:
        schema["required"] = list(required)
    return schema


def model_schema(model):
    if is_dataclass(model):
        hints = get_type_hints(model)
        required = [f.name for f in dataclass_fields(model) if f.default is MISSING and f.default_factory is MISSING]
        return object_schema({name: model_schema(hint) for name, hint in hints.items()}, *required)
    args = get_args(model)
    if type(None) in args:
        return {**model_schema(next(arg for arg in args if arg is not type(None))), "nullable": True}
    if get_origin(model) is list:
        return array(model_schema(args[0]))
    if get_origin(model) is dict:
        return {"type": "object", "additionalProperties": model_schema(args[1])}
    if model is object:
        return {}
    return {"type": {int: "integer", float: "number", str: "string", bool: "boolean"}[model]}


def describe(method, route, operation):
    identifiers = fields(string="client_id tenant_id")
    metrics = fields(number="rps successful_requests error_rate p50_latency_ms p95_latency_ms p99_latency_ms cpu_usage_cores spot_instance_risk",
                     integer="current_replicas mem_usage_bytes",
                     string="cpu_requests cpu_limits memory_requests memory_limits node_name node_instance_type namespace workload_name workload_kind pod_name container_name")
    event = fields(string="schema_version event_id client_id cluster_id agent_version cluster_version", integer="timestamp")
    command = fields(string="command_id client_id cluster_id workload reason policy_version",
                     integer="recommended_replicas execute_at expires_at", number="confidence")
    command["requested_resources"] = object_schema(fields(string="cpu memory"))
    audit = fields(string="decision_id client_id cluster_id workload decision_type execution_mode reason",
                   number="economic_delta_usd_hour", integer="timestamp", objects="before_state after_state")
    gpu = fields(string="workload", number="sm_utilization_pct vram_used_gib tokens_per_second")
    budget = fields(number="monthly_budget_usd mtd_spend_usd", integer="day_of_month")
    if method == "get":
        filters = {
            "audit/decisions": fields(string="cluster_id decision_id"),
            "telemetry/events": fields(string="cluster_id event_id"),
            "pricing/lookup": fields(string="instance_type region"),
            "billing/invoice": fields(string="cluster_id model", integer="nodes", number="savings_usd"),
            "gpu/optimize": {**gpu, **fields(string="gpu_model")},
            "greenops/report": fields(string="region"), "finops/guardrail": budget,
            "cast/commitments": fields(string="provider", integer="requested_cores"),
            "cast/hibernation": fields(string="cluster_id environment", integer="hour_utc"),
            "cast/onboarding": fields(string="provider cluster_name"),
        }
        if route in ("policies", "commands/pending", "ui/cast-dashboard", "gameday/simulate", "cast/security", "cluster/topology"):
            filters[route] = fields(string="cluster_id")
        operation["parameters"] = [
            {"name": name, "in": "query", "schema": schema}
            for name, schema in {**identifiers, **filters.get(route, {})}.items()
        ]
    else:
        bodies = {
            "telemetry/ingest": {**event, **metrics, "metrics": object_schema(metrics)},
            "commands/ack": fields(string="command_id status reason", integer="timestamp"),
            "commands/prewarm": command, "audit/decisions": audit,
            "agents/heartbeat": fields(string="cluster_id agent_version cluster_version execution_mode status"),
            "evaluate": fields(number="cpu_millicores p99_ms slo_p99_ms rps cpu_price_per_core_hour mem_price_per_gib_hour memory_gib max_cost_per_request min_millicores max_millicores cpu_usage_millicores",
                               objects="budget memory replicas spot predictive oom_protection"),
            "cluster/binpack": {**fields(string="cluster_id"), "nodes": array({"type": "object"})},
            "auth/keys": {**fields(string="prefix_tag", integer="expires_in_days"), "scopes": array({"type": "string"})},
            "auth/keys/revoke": fields(string="key_id"),
            "ui/recommendations/execute": fields(string="recommendation_id recommendation_type target workload cluster_id action_type"),
            "gpu/optimize": {**gpu, **fields(string="current_gpu_model", integer="requested_gpus", boolean="spot_tolerant", number="inference_rps latency_p99_ms latency_slo_ms")},
            "finops/guardrail": {**budget, **fields(integer="days_in_month", boolean="slo_critical_breach")},
            "gameday/simulate": fields(string="cluster_id", integer="node_count", number="baseline_rps"),
            "cast/commitments": fields(string="provider region", integer="committed_cores currently_used_cores workload_requested_cores", number="hourly_commitment_usd"),
            "cast/hibernation": fields(string="cluster_id environment", integer="active_nodes skeleton_nodes current_hour_utc"),
            "cast/network-storage": fields(number="cross_az_gb_per_day", integer="gp2_volumes_total_gib overprovisioned_pvc_gib unattached_zombie_volumes_gib"),
            "billing/stripe-webhook": fields(string="id type", objects="data"),
        }
        required = {"evaluate": ("cpu_millicores", "p99_ms", "slo_p99_ms"), "telemetry/ingest": ("cluster_id", "metrics"),
                    "commands/ack": ("command_id",), "commands/prewarm": ("cluster_id", "workload"), "auth/keys/revoke": ("key_id",)}
        props = bodies[route] if route == "billing/stripe-webhook" else {**identifiers, **bodies[route]}
        if route == "cluster/binpack":
            props["nodes"] = {**array(model_schema(NodeState)), "maxItems": 1000}
        operation["requestBody"]["content"]["application/json"]["schema"] = object_schema(props, *required.get(route, ()))
    normalized = object_schema({**event, **metrics})
    command_result = object_schema({**command, **fields(string="action status"),
                                    **{k: {**v, "nullable": True} for k, v in fields(string="ack_status ack_reason", number="ack_timestamp").items()}})
    output = {
        ("get", "telemetry/events"): {"events": array(normalized)},
        ("get", "audit/decisions"): {"decisions": array(object_schema(audit))},
        ("get", "commands/pending"): {"commands": array(command_result)},
        ("post", "commands/prewarm"): command_result["properties"],
        ("post", "audit/decisions"): audit,
        ("post", "commands/ack"): fields(string="command_id client_id cluster_id workload ack_result reason"),
        ("post", "telemetry/ingest"): {**fields(string="status event_id client_id cluster_id schema_version", boolean="deduplicated"), "normalized": normalized},
        ("post", "auth/keys/revoke"): fields(string="key_id client_id", boolean="revoked"),
        ("post", "agents/heartbeat"): fields(string="status client_id cluster_id", integer="server_timestamp pending_commands_count"),
    }.get((method, route))
    if output:
        operation["responses"]["200"]["content"]["application/json"]["schema"] = object_schema(output)
        return
    models = {"gpu/optimize": GPUOptimizationPlan, "greenops/report": GreenOpsCarbonReport, "finops/guardrail": FinOpsBudgetGuardrailDecision,
              "gameday/simulate": GameDayScorecard, "pricing/lookup": CloudInstancePricing}
    content = operation["responses"]["200"]["content"]["application/json"]
    if route in models:
        content["schema"] = model_schema(models[route])
        return
    if route == "ui/recommendations/execute":
        operation["responses"] = {"400": {"description": "Direct UI execution unavailable. Use the operator workflow."}}
        return
    key = {**fields(string="key_id client_id key_prefix", integer="created_at expires_at"), "scopes": array({"type": "string"})}
    records = array({"type": "object"})
    optional_number = {"type": "number", "nullable": True}
    remaining = {
        "auth/keys": {"keys": array(object_schema({**key, **fields(boolean="revoked expired")}))} if method == "get" else {**key, **fields(string="api_key", integer="expires_in_days")},
        "cluster/binpack": {**fields(number="cluster_efficiency_before_pct cluster_efficiency_after_pct monthly_savings_usd", integer="total_nodes", string="reason"), "nodes_can_drain": array({"type": "string"}), "relocation_plans": array(model_schema(RelocationPlan))},
        "billing/stripe-webhook": fields(string="event_id event_type tenant_id status", integer="processed_at", boolean="verified idempotent_replay"),
        "evaluate": {**fields(string="action reason", integer="cpu_millicores", boolean="budget_frozen"), "cost_per_request": optional_number, "cost_per_request_after": optional_number,
                     "spot_eligible": {"type": "boolean", "nullable": True}, **{k: {"type": "object", "nullable": True} for k in ("memory", "replicas", "predictive", "oom_protection")}},
        "policies": fields(string="policy_version client_id cluster_id execution_mode", number="slo_p99_ms max_increase_percent max_decrease_percent", integer="min_replicas max_replicas", boolean="enable_prewarm enable_spot_drain"),
        "billing/invoice": {**fields(string="savings_evidence stripe_object client_id cluster_id billing_model currency", boolean="simulation charge_created", integer="amount_due_cents", number="amount_due_usd customer_net_savings_usd customer_roi_multiple", objects="comparison"), "line_items": records},
        "ui/cast-dashboard": {**fields(string="client_id cluster_id data_source", boolean="has_data", objects="pain_number_cards"), "latest_observation": optional_number, "namespace_bar_chart": records, "actionable_recommendations": records},
        "cluster/topology": {**fields(string="cluster_id data_source message"), "nodes": records},
        "cast/commitments": fields(string="contract_id provider region commitment_type action reason", integer="committed_cores used_cores_before unused_cores_before absorbed_cores remaining_spot_cores", number="utilization_before_pct utilization_after_pct effective_incremental_hourly_usd monthly_commitment_waste_recovered_usd"),
        "cast/hibernation": fields(string="cluster_id environment hibernation_action cron_sleep_utc cron_wake_utc", integer="active_nodes target_nodes sleep_hours_per_week", number="monthly_hibernation_savings_usd hibernation_savings_pct", objects="rebalancing_plan"),
        "cast/network-storage": fields(objects="network_egress storage_ebs_pvc", number="total_monthly_net_and_storage_savings_usd"),
        "cast/security": {**fields(string="cluster_id cis_benchmark_version reason", number="security_posture_score compliant_workloads_pct", integer="critical_vulnerabilities high_findings medium_findings", objects="nexus_telemetry_event"), "findings": records},
        "cast/onboarding": {**fields(string="provider cluster_name region tenant_id api_key_preview", objects="phase_1_read_only_audit phase_2_autonomous_optimization"), "supported_platforms": array({"type": "string"})},
        "cast/full-suite": fields(objects="commitments hibernation network_and_storage security_posture onboarding"),
        "system/readiness": {**fields(string="organization pillar overall_status external_validation canonical_tenant_id", integer="certified_subsystems self_checks_passed total_subsystems"), "limitations": array({"type": "string"}), "subsystems": records},
        "openapi.json": fields(string="openapi", objects="info paths components"),
    }
    content["schema"] = object_schema(remaining[route])
