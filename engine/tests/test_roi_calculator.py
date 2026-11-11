"""Unit tests for the enterprise FinOps ROI calculator."""

import importlib.util
import pathlib
import sys

# Load scripts/calculate_roi.py dynamically
SCRIPT_PATH = pathlib.Path(__file__).resolve().parent.parent.parent / "scripts" / "calculate_roi.py"
spec = importlib.util.spec_from_file_location("calculate_roi", str(SCRIPT_PATH))
roi_module = importlib.util.module_from_spec(spec)
sys.modules["calculate_roi"] = roi_module
spec.loader.exec_module(roi_module)


def test_calculate_finops_roi_basic():
    data = roi_module.calculate_finops_roi(
        nodes=50,
        node_cost_hour=0.35,
        avg_cpu_util=0.22,
        spot_pct=0.15,
        target_util=0.60,
    )
    assert data["nodes"] == 50
    assert data["annual_baseline_spend"] == 50 * 0.35 * 8760
    assert data["monthly_baseline_spend"] == 50 * 0.35 * 730
    assert 20.0 <= data["projected_savings_percentage"] <= 50.0
    assert data["annual_gross_savings"] > 0
    assert data["standard_tier"]["annual_license_fee"] == 50 * 18.0 * 12
    assert data["standard_tier"]["roi_multiple"] > 1.0
    assert data["standard_tier"]["payback_period_days"] > 0


def test_volume_discounts():
    # 100 nodes receives 15% discount
    data_100 = roi_module.calculate_finops_roi(100, 0.35, 0.20, 0.10, 0.60)
    assert data_100["standard_tier"]["rate_per_node_month"] == 18.0 * 0.85

    # 350 nodes receives 25% discount
    data_350 = roi_module.calculate_finops_roi(350, 0.35, 0.20, 0.10, 0.60)
    assert data_350["standard_tier"]["rate_per_node_month"] == 18.0 * 0.75


def test_formatters():
    data = roi_module.calculate_finops_roi(80, 0.40, 0.25, 0.15, 0.65)

    terminal_out = roi_module.format_terminal(data)
    assert "KUBECOST AUTO-HPA" in terminal_out
    assert "80" in terminal_out

    markdown_out = roi_module.format_markdown(data)
    assert "# FinOps Business Case & ROI Assessment" in markdown_out
    assert "Standard Tier" in markdown_out
