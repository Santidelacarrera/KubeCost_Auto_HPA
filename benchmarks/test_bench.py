"""The benchmark is only reproducible if its traces, manifests and arithmetic never drift. These tests pin them."""

import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import bench  # noqa: E402


def test_traces_are_deterministic_and_match_the_committed_files():
    a, b = bench.all_traces(), bench.all_traces()
    assert a == b
    for name, rps in a.items():
        assert len(rps) == bench.SCENARIOS[name]["minutes"] and min(rps) >= 0
        committed = (bench.HERE / "traces" / f"{name}.csv").read_text(encoding="utf-8").splitlines()
        assert committed[0] == "minute,rps"
        assert [int(line.split(",")[1]) for line in committed[1:]] == rps, f"{name}: run `python benchmarks/bench.py traces`"


def test_trace_shapes():
    t = bench.all_traces()
    step = t["step-increase"]
    assert sum(step[45:60]) / 15 > 2.5 * sum(step[:35]) / 35
    spikes = t["spikes"]
    assert max(spikes) == 40 and sorted(spikes)[len(spikes) // 2] <= 9  # 5 x 8 rps bursts on a base of 8
    growth = t["steady-growth"]
    assert sum(growth[-10:]) > 1.8 * sum(growth[:10])
    diurnal = t["diurnal"]
    assert max(diurnal) > 1.4 * 20 and min(diurnal) < 0.6 * 20


def test_render_is_the_same_workload_for_every_tool():
    y = bench.render("diurnal", "bench-a")
    assert y == bench.render("diurnal", "bench-a")
    assert y.replace("bench-a", "bench-b") == bench.render("diurnal", "bench-b")
    for needle in ["kind: Deployment", "kind: PodDisruptionBudget", "requests: {cpu: 300m, memory: 256Mi}", "trace.txt: |", "namespace: bench-a"]:
        assert needle in y
    assert y.count("\n    ") >= 180  # one trace line per minute


def test_integral_and_change_counting():
    pts = [(0, 2.0), (1800, 2.0), (3600, 4.0)]
    assert bench.integrate_hours(pts) == pytest.approx(0.0)  # 30-minute gaps are not bridged
    pts = [(t, 2.0) for t in range(0, 3601, 60)]
    assert bench.integrate_hours(pts) == pytest.approx(2.0)
    steps = [(0, 3), (60, 3), (120, 5), (180, 5), (240, 4)]
    assert bench.count_changes(steps) == 2
    assert bench.first_change_after(steps, 100) == 20
    assert bench.first_change_after(steps, 130) == 110
    assert bench.first_change_after(steps, 300) is None


def flat(v, minutes=60):
    return [(t * 60.0, float(v)) for t in range(minutes + 1)]


def test_summarise_reports_waste_and_never_invents_a_missing_value():
    m = bench.summarise({"cpu_requests": flat(1.5), "cpu_usage": flat(0.3), "replicas_desired": flat(3), "replicas_unavailable": flat(0), "nodes": flat(2)},
                        node_hourly_usd=0.1)
    assert m["cpu_wasted_core_hours"] == pytest.approx(1.2) and m["cpu_wasted_pct"] == pytest.approx(80.0)
    assert m["availability_pct"] == pytest.approx(100.0) and m["node_cost_usd_at_list_price"] == pytest.approx(0.2)
    assert m["replica_changes"] == 0
    for absent in ["mem_wasted_pct", "latency_p95_s", "restarts", "tool_cpu_cores_avg", "request_changes", "seconds_to_first_replica_change"]:
        assert m[absent] is None, absent
    assert bench.summarise({"nodes": flat(2)})["node_cost_usd_at_list_price"] is None  # no price given: no cost


def result(tool, kind="MEASURED", scenario="diurnal", cluster="c1"):
    return {"tool": tool, "kind": kind, "scenario": scenario, "run": "1", "environment": {"cluster": cluster},
            "metrics": bench.summarise({"cpu_requests": flat(1.5), "cpu_usage": flat(0.3)})}


def test_compare_refuses_apples_and_oranges_and_flags_what_is_not_comparable():
    table = bench.compare([result("none"), result("kubecost-auto-hpa")])
    assert "| CPU requested and unused (%) | 80.0 | 80.0 |" in table and "not measured" in table
    assert "not comparable" not in table
    assert "not comparable" in bench.compare([result("a"), result("b", cluster="c2")])
    assert "not a measurement" in bench.compare([result("a"), result("b", kind="SIMULATED")])
    with pytest.raises(SystemExit):
        bench.compare([result("a"), result("b", scenario="spikes")])


def test_scenarios_file_is_well_formed():
    for name, sc in json.loads((bench.HERE / "scenarios.json").read_text(encoding="utf-8")).items():
        assert set(sc) == {"description", "minutes", "workload", "load"}, name
        assert sc["minutes"] >= 60 and sc["load"]["base_rps"] <= 50, name  # the busybox generator is not meant for more
