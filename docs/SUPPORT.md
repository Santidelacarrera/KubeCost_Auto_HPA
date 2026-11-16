# Support and maintenance policy

This is an open-source project maintained by one person. This page says what you can rely on today. It does not describe a
commercial support contract: none is offered yet ([COMMERCIAL.md](COMMERCIAL.md) describes what one would look like).

## What is supported

| Item | Policy |
|---|---|
| Product versions | The latest minor gets fixes and security fixes. The previous minor gets security fixes for 90 days ([UPGRADE.md](UPGRADE.md)) |
| Kubernetes | 1.33 to 1.36, the versions the suite runs on |
| Providers | Upstream Kubernetes, GKE Standard, AKS. Each with the limits in [GKE.md](GKE.md) and [AKS.md](AKS.md). AWS/EKS is out of scope |
| Modes | Observe-only is supported. Assisted is supported for a supervised pilot. Automated is experimental |
| Response times | Best effort. No guaranteed response or resolution time |

## Getting help and reporting

| What | Where |
|---|---|
| A bug | GitHub issue with the bug template (`.github/ISSUE_TEMPLATE/bug_report.yml`): version, Kubernetes version and provider, mode, what happened, logs |
| A feature request | GitHub issue with the feature template |
| A security vulnerability | **Privately**: Security, Report a vulnerability ([../SECURITY.md](../SECURITY.md)). Never a public issue |
| A question about your own cluster | GitHub Discussions or an issue. Remove workload names, node names and tokens from what you paste |

What to attach to a bug:

```bash
kubectl -n kubecost-auto-hpa-system logs deploy/kubecost-auto-hpa-operator --tail=500
```

```bash
kubectl get kubecostautoscaler,kubecosthpa,wrec -A -o yaml
```

and the `rightsize` JSON output or audit lines of the operation. Review them first: they contain names from your cluster.

## Severity

Used to order work, not as a promise of time.

| Severity | Meaning | Example |
|---|---|---|
| S1 | The product made a workload unavailable or kept a degraded change in place | A rollback that did not restore |
| S2 | A safety control did not hold, without damage | A change applied outside the policy window |
| S3 | A feature does not work; a safe workaround exists | A recommendation abstains wrongly |
| S4 | Cosmetic, documentation, enhancement | |

An S1 or S2 in a published version gets a patch release and an entry under "Fixed" or "Security" in the changelog, with the
versions affected.

## Incident handling

If you suspect the product caused or is causing harm:

1. **Stop it.** `killSwitch: true` in the policy ConfigMap, or `operator.observeOnly=true` ([ROLLBACK.md](ROLLBACK.md)).
2. **Restore.** `rightsize rollback` on the affected workload.
3. **Keep the evidence** before anything is cleaned up: operator logs, the custom resources as YAML, the audit file or
   collector contents, `kubectl get events -A`.
4. **Report** it as S1 or S2, privately if it has a security angle.

The maintainer's side: reproduce, fix with a regression test that fails without the fix, release a patch, and record the
defect and its evidence in [LAB.md](LAB.md) and the changelog. Defects found on real clusters so far were handled this way;
each is listed in the engineering report with its test.

## Recovery procedures

| Situation | Procedure |
|---|---|
| Operator pod crash-loops | `kubectl logs --previous`. Most common cause: CRDs older than the operator; apply `crds/` ([TROUBLESHOOTING.md](TROUBLESHOOTING.md)) |
| Operator killed during an apply | Nothing is lost: on restart it resumes from the object's status. A CLI apply that was killed: run `rightsize rollback` |
| API server unavailable | The operator holds; observation time is not counted during the outage (verified for about 40 s on a lab cluster, one operator replica) |
| Cost source down or stale | `KubeCostAutoscaler` freezes replicas (safety scale-up only if authorised); no action needed |
| Prometheus down | The recommender abstains; `KubeCostHPA` uses the last reading for up to 15 minutes, then does nothing |
| Redis lost | Caches rebuild; nothing durable is stored there |
| Cluster lost | Reinstall the chart, re-apply your custom resources and policy from Git. Shipped audit events are in your collector |
| Bad release | `helm rollback` ([UPGRADE.md](UPGRADE.md)) |

## Maintenance cadence

* Dependencies: Dependabot opens updates; CI runs `govulncheck` and an image scan on every push ([SUPPLY_CHAIN.md](SUPPLY_CHAIN.md)).
* Releases: when there is something to release, after the dry-run and the Helm rehearsal ([RELEASING.md](RELEASING.md)). No
  fixed calendar.
* Every release has a changelog entry, a signed image and a signed chart.

## What is not offered

A guaranteed response time, on-call, a private support channel, indemnification, compliance attestations (SOC 2, ISO 27001),
or a signed data-processing agreement. The documents in this repository that mention them are drafts of a future commercial
offer, marked as such.
