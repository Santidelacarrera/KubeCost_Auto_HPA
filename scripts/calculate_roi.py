#!/usr/bin/env python3
"""
KubeCost Auto-HPA — Enterprise FinOps ROI & Cloud Savings Calculator

Usage:
    python scripts/calculate_roi.py --nodes 100 --node-cost-hour 0.38 --avg-cpu-util 0.20
    python scripts/calculate_roi.py --nodes 250 --output markdown > my_roi_report.md
"""

import argparse
import json
import sys


def parse_args():
    parser = argparse.ArgumentParser(
        description="Calculate enterprise cloud savings and ROI for KubeCost Auto-HPA."
    )
    parser.add_argument(
        "--nodes",
        type=int,
        default=50,
        help="Total number of Kubernetes compute worker nodes (default: 50).",
    )
    parser.add_argument(
        "--node-cost-hour",
        type=float,
        default=0.35,
        help="Average hourly cost per node in USD (default: $0.35, e.g., AWS m6i.xlarge).",
    )
    parser.add_argument(
        "--cloud",
        type=str,
        default="aws",
        choices=["aws", "gcp", "azure", "onprem"],
        help="Cloud provider (aws, gcp, azure, onprem).",
    )
    parser.add_argument(
        "--avg-cpu-util",
        type=float,
        default=0.22,
        help="Current average cluster CPU utilization between 0.05 and 0.80 (default: 0.22 = 22%%).",
    )
    parser.add_argument(
        "--spot-pct",
        type=float,
        default=0.15,
        help="Current percentage of nodes on Spot/Preemptible instances (default: 0.15 = 15%%).",
    )
    parser.add_argument(
        "--target-util",
        type=float,
        default=0.60,
        help="Target CPU utilization with KubeCost Auto-HPA dynamic sizing (default: 0.60 = 60%%).",
    )
    parser.add_argument(
        "--output",
        type=str,
        default="terminal",
        choices=["terminal", "markdown", "json"],
        help="Output format: terminal, markdown, or json.",
    )
    return parser.parse_args()


def calculate_finops_roi(nodes: int, node_cost_hour: float, avg_cpu_util: float, spot_pct: float, target_util: float):
    # Hours per year
    hours_per_year = 8760
    hours_per_month = 730

    # Total baseline compute cost
    monthly_baseline = nodes * node_cost_hour * hours_per_month
    annual_baseline = nodes * node_cost_hour * hours_per_year

    # Rightsizing efficiency factor: ratio of current util to target util, capped conservatively
    efficiency_gain = max(0.15, min(0.50, 1.0 - (avg_cpu_util / target_util)))

    # Additional spot adoption headroom enabled by safe spot drainer (+15% more spot adoption)
    spot_bonus_savings = (0.35 - spot_pct) * 0.60 if spot_pct < 0.35 else 0.05
    spot_bonus_savings = max(0.02, min(0.12, spot_bonus_savings))

    # Total estimated savings rate
    blended_savings_rate = min(0.48, efficiency_gain + spot_bonus_savings)

    annual_gross_savings = annual_baseline * blended_savings_rate
    monthly_gross_savings = monthly_baseline * blended_savings_rate

    # Pricing tiers
    standard_monthly_rate = 18.00  # $18/node/month
    mission_critical_monthly_rate = 32.00  # $32/node/month

    # Volume discount for large fleets (> 100 nodes = 15% discount, > 300 nodes = 25% discount)
    volume_discount = 0.0
    if nodes >= 300:
        volume_discount = 0.25
    elif nodes >= 100:
        volume_discount = 0.15

    discounted_std_rate = standard_monthly_rate * (1.0 - volume_discount)
    discounted_mc_rate = mission_critical_monthly_rate * (1.0 - volume_discount)

    standard_annual_license = nodes * discounted_std_rate * 12
    mc_annual_license = nodes * discounted_mc_rate * 12

    # Net ROI
    net_savings_standard = annual_gross_savings - standard_annual_license
    net_savings_mc = annual_gross_savings - mc_annual_license

    roi_multiple_standard = annual_gross_savings / max(1.0, standard_annual_license)
    roi_multiple_mc = annual_gross_savings / max(1.0, mc_annual_license)

    # Payback period (in days)
    daily_gross_savings = annual_gross_savings / 365.0
    payback_days_standard = standard_annual_license / max(1.0, daily_gross_savings)
    payback_days_mc = mc_annual_license / max(1.0, daily_gross_savings)

    return {
        "nodes": nodes,
        "hourly_node_cost": node_cost_hour,
        "monthly_baseline_spend": monthly_baseline,
        "annual_baseline_spend": annual_baseline,
        "baseline_cpu_utilization": avg_cpu_util,
        "target_cpu_utilization": target_util,
        "projected_savings_percentage": blended_savings_rate * 100.0,
        "monthly_gross_savings": monthly_gross_savings,
        "annual_gross_savings": annual_gross_savings,
        "standard_tier": {
            "rate_per_node_month": discounted_std_rate,
            "annual_license_fee": standard_annual_license,
            "net_annual_savings": net_savings_standard,
            "roi_multiple": roi_multiple_standard,
            "payback_period_days": round(payback_days_standard, 1),
        },
        "mission_critical_tier": {
            "rate_per_node_month": discounted_mc_rate,
            "annual_license_fee": mc_annual_license,
            "net_annual_savings": net_savings_mc,
            "roi_multiple": roi_multiple_mc,
            "payback_period_days": round(payback_days_mc, 1),
        },
    }


def format_terminal(data):
    s = []
    s.append("=" * 72)
    s.append("   KUBECOST AUTO-HPA --- ENTERPRISE FINOPS ROI & SAVINGS ASSESSMENT")
    s.append("=" * 72)
    s.append(f"  Cluster Worker Nodes:         {data['nodes']}")
    s.append(f"  Average Node Cost:            ${data['hourly_node_cost']:.2f} / hr")
    s.append(f"  Current Average CPU Util:     {data['baseline_cpu_utilization'] * 100:.1f}%")
    s.append(f"  Target Post-Optimization:     {data['target_cpu_utilization'] * 100:.1f}%")
    s.append("-" * 72)
    s.append(f"  Current Monthly Spend:        ${data['monthly_baseline_spend']:,.2f}")
    s.append(f"  Current Annual Spend:         ${data['annual_baseline_spend']:,.2f}")
    s.append(f"  Projected Cloud Reduction:    {data['projected_savings_percentage']:.1f}%")
    s.append(f"  Monthly Gross Savings:        ${data['monthly_gross_savings']:,.2f}")
    s.append(f"  ANNUAL GROSS SAVINGS:         ${data['annual_gross_savings']:,.2f}")
    s.append("=" * 72)
    s.append("  COMMERCIAL TIER ANALYSIS:")
    s.append("-" * 72)
    std = data["standard_tier"]
    s.append(f"  [Standard Tier: ${std['rate_per_node_month']:.2f}/node/mo]")
    s.append(f"    - Annual License:           ${std['annual_license_fee']:,.2f}")
    s.append(f"    - Net Annual Savings:       ${std['net_annual_savings']:,.2f}")
    s.append(f"    - Return on Investment:     {std['roi_multiple']:.1f}x ROI")
    s.append(f"    - Payback Period:           {std['payback_period_days']} Days")
    s.append("")
    mc = data["mission_critical_tier"]
    s.append(f"  [Mission-Critical Tier: ${mc['rate_per_node_month']:.2f}/node/mo (24/7 SLA)]")
    s.append(f"    - Annual License:           ${mc['annual_license_fee']:,.2f}")
    s.append(f"    - Net Annual Savings:       ${mc['net_annual_savings']:,.2f}")
    s.append(f"    - Return on Investment:     {mc['roi_multiple']:.1f}x ROI")
    s.append(f"    - Payback Period:           {mc['payback_period_days']} Days")
    s.append("=" * 72)
    s.append("  RECOMMENDATION: Schedule a 14-day zero-risk dry-run pilot.")
    s.append("  Contact: santiago@delacarrera.com | https://github.com/Santidelacarrera/KubeCost_Auto_HPA")
    s.append("=" * 72)
    return "\n".join(s)


def format_markdown(data):
    s = []
    s.append("# FinOps Business Case & ROI Assessment: KubeCost Auto-HPA")
    s.append("")
    s.append("## Executive Summary")
    s.append(f"- **Fleet Profile:** {data['nodes']} nodes @ ${data['hourly_node_cost']:.2f}/hour average cost.")
    s.append(f"- **Baseline Annual Spend:** **${data['annual_baseline_spend']:,.2f} USD**")
    s.append(f"- **Expected Cloud Cost Reduction:** **{data['projected_savings_percentage']:.1f}%**")
    s.append(f"- **Projected Gross Annual Savings:** **${data['annual_gross_savings']:,.2f} USD**")
    s.append("")
    s.append("## Commercial Tier Comparison Table")
    s.append("")
    s.append("| Metric | Baseline (Status Quo) | Enterprise Standard Tier | Enterprise Mission-Critical Tier |")
    s.append("|---|:---:|:---:|:---:|")
    s.append(f"| **Effective Rate / Node** | N/A | ${data['standard_tier']['rate_per_node_month']:.2f} / mo | ${data['mission_critical_tier']['rate_per_node_month']:.2f} / mo |")
    s.append(f"| **Annual Software Investment** | $0 | ${data['standard_tier']['annual_license_fee']:,.2f} | ${data['mission_critical_tier']['annual_license_fee']:,.2f} |")
    s.append(f"| **Gross Cloud Savings** | $0 | ${data['annual_gross_savings']:,.2f} | ${data['annual_gross_savings']:,.2f} |")
    s.append(f"| **Net Annualized Savings** | $0 | **${data['standard_tier']['net_annual_savings']:,.2f}** | **${data['mission_critical_tier']['net_annual_savings']:,.2f}** |")
    s.append(f"| **Net ROI Multiplier** | 1.0x | **{data['standard_tier']['roi_multiple']:.1f}x** | **{data['mission_critical_tier']['roi_multiple']:.1f}x** |")
    s.append(f"| **Payback Period** | Immediate Loss | **{data['standard_tier']['payback_period_days']} Days** | **{data['mission_critical_tier']['payback_period_days']} Days** |")
    s.append(f"| **Support SLA** | Internal On-Call | Business Hours (4h SLA) | 24/7/365 (<1h SLA) |")
    s.append("")
    s.append("## Proposed Action Plan")
    s.append("1. **Day 1-7:** Deploy KubeCost Auto-HPA in `spec.dryRun: true` mode on staging/production.")
    s.append("2. **Day 8-12:** Verify live calculated savings in Grafana dashboard without pod disruption.")
    s.append("3. **Day 14:** Review realized FinOps metrics with executive committee and activate enterprise subscription.")
    return "\n".join(s)


def main():
    args = parse_args()
    data = calculate_finops_roi(
        nodes=args.nodes,
        node_cost_hour=args.node_cost_hour,
        avg_cpu_util=args.avg_cpu_util,
        spot_pct=args.spot_pct,
        target_util=args.target_util,
    )

    if args.output == "json":
        print(json.dumps(data, indent=2))
    elif args.output == "markdown":
        print(format_markdown(data))
    else:
        print(format_terminal(data))


if __name__ == "__main__":
    main()
