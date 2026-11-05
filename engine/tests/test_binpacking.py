import pytest
from kubecost_engine.binpacking import NodeBinPacker, NodeState, PodPlacement


def test_binpacking_finds_drainable_nodes():
    # Node 1: Heavy loaded (75% CPU)
    node1 = NodeState(
        name="node-1",
        instance_type="m6i.xlarge",
        zone="us-east-1a",
        monthly_cost_usd=140.0,
        total_cpu_millicores=4000,
        total_memory_mib=16384,
        allocated_cpu_millicores=3000,
        allocated_memory_mib=10000,
        pods=[PodPlacement("pod-a", "default", 3000, 10000)],
    )

    # Node 2: Low loaded (10% CPU), easily consolidatable to node 1
    node2 = NodeState(
        name="node-2",
        instance_type="m6i.xlarge",
        zone="us-east-1a",
        monthly_cost_usd=140.0,
        total_cpu_millicores=4000,
        total_memory_mib=16384,
        allocated_cpu_millicores=400,
        allocated_memory_mib=2000,
        pods=[PodPlacement("pod-b", "default", 400, 2000)],
    )

    packer = NodeBinPacker()
    res = packer.optimize_cluster([node1, node2])

    assert "node-2" in res.nodes_can_drain
    assert res.monthly_savings_usd == 140.0
    assert len(res.relocation_plans) == 1
    assert res.relocation_plans[0].pod_name == "pod-b"
    assert res.relocation_plans[0].to_node == "node-1"
    assert res.cluster_efficiency_after_pct > res.cluster_efficiency_before_pct


def test_binpacking_empty_cluster():
    packer = NodeBinPacker()
    res = packer.optimize_cluster([])
    assert res.total_nodes == 0
    assert len(res.nodes_can_drain) == 0
