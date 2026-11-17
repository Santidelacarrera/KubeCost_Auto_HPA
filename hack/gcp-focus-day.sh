#!/usr/bin/env bash
# Writes one day of Google's detailed billing export for one GKE cluster as a FOCUS-shaped CSV, in the BILLING currency, for
# `rightsize reconcile`. Read-only: one BigQuery query.
#   PROJECT=<gcp project> TABLE=<dataset.gcp_billing_export_resource_v1_...> CLUSTER=<gke cluster name> DAY=2026-11-12 bash hack/gcp-focus-day.sh > day.csv
# Column mapping (Google's own definitions): ListCost = cost_at_list; BilledCost and EffectiveCost = cost + credits (what is
# invoiced after promotions, discounts and free tier); tax is not in this export. Rows are the cluster's nodes, disks and
# cluster fee, selected by the goog-k8s-cluster-name label or the gke-<cluster>- resource name prefix.
set -eu
: "${PROJECT:?}"; : "${TABLE:?}"; : "${CLUSTER:?}"; : "${DAY:?}"
bq --project_id "$PROJECT" query --use_legacy_sql=false --format=csv --max_rows=1000000 "
  SELECT 'Usage' AS ChargeCategory,
         IFNULL(resource.global_name, IFNULL(resource.name, service.description)) AS ResourceId,
         IFNULL(resource.name, '') AS ResourceName,
         ROUND(cost + IFNULL((SELECT SUM(c.amount) FROM UNNEST(credits) c), 0), 6) AS BilledCost,
         ROUND(cost + IFNULL((SELECT SUM(c.amount) FROM UNNEST(credits) c), 0), 6) AS EffectiveCost,
         ROUND(IFNULL(cost_at_list, cost), 6) AS ListCost,
         currency AS BillingCurrency,
         FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', usage_start_time) AS ChargePeriodStart,
         FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', usage_end_time) AS ChargePeriodEnd
  FROM \`$PROJECT.$TABLE\`
  WHERE usage_start_time >= TIMESTAMP('$DAY') AND usage_end_time <= TIMESTAMP_ADD(TIMESTAMP('$DAY'), INTERVAL 1 DAY)
    AND usage_end_time > usage_start_time
    AND (resource.name LIKE 'gke-$CLUSTER-%' OR resource.name = '$CLUSTER'
         OR EXISTS (SELECT 1 FROM UNNEST(labels) l WHERE l.key = 'goog-k8s-cluster-name' AND l.value = '$CLUSTER'))
  ORDER BY ChargePeriodStart, ResourceId"
