"""Browser E2E against the real HTTP server and SQLite backend (no mocked API)."""

import threading

from playwright.sync_api import sync_playwright

from kubecost_engine import server
from kubecost_engine.saas import SaaSControlPlane


def test_console_real_api_empty_registration_and_mobile(monkeypatch, tmp_path):
    monkeypatch.setenv("ENGINE_TOKEN", "local-browser-test-token")
    monkeypatch.setenv("ENGINE_TOKEN_TENANT_ID", "browser-org")
    cp = SaaSControlPlane(str(tmp_path / "browser.db"))
    monkeypatch.setattr(server, "get_control_plane", lambda: cp)
    server.get_rate_limiter().reset_local()
    httpd = server.make_server("127.0.0.1", 0)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch()
            page = browser.new_page(viewport={"width": 1280, "height": 900})
            errors = []
            page.on("pageerror", lambda exc: errors.append(str(exc)))
            page.goto(f"http://127.0.0.1:{httpd.server_port}/app.html")
            page.locator("#tenant").fill("browser-org")
            page.locator("#credential").fill("wrong-token")
            page.get_by_role("button", name="Conectar", exact=True).click()
            page.locator("#error").filter(has_text="401").wait_for()
            page.locator("#credential").fill("local-browser-test-token")
            page.get_by_role("button", name="Conectar", exact=True).click()
            page.locator("#connection").filter(has_text="API conectada").wait_for()
            assert page.locator("#cluster-count").inner_text() == "0"
            page.locator("#operation-fields [name=cluster_id]").fill("gke-lab")
            page.get_by_role("button", name="Validar y enviar").click()
            page.locator("#cluster-count").filter(has_text="1").wait_for()
            assert "gke-lab" in page.locator("#results").inner_text()
            page.locator("#filter").fill("does-not-exist")
            assert page.locator("#empty").is_visible()
            page.locator("#filter").fill("")
            page.get_by_role("button", name="Costos", exact=True).click()
            page.locator("#view-title").filter(has_text="Costos importados").wait_for()
            assert page.locator("#empty").is_visible()
            page.get_by_role("button", name="Usuarios", exact=True).click()
            page.locator("#view-title").filter(has_text="Usuarios").wait_for()
            page.get_by_role("button", name="Alertas", exact=True).click()
            page.locator("#view-title").filter(has_text="Alertas").wait_for()
            cp._store.put_object("browser-org", "prediction", "browser-prediction", {
                "id": "browser-prediction", "cluster_id": "gke-lab", "workload": "api", "status": "ready",
                "predicted_demand": 125, "observed_demand": 120, "provenance": {"demand_metric": "rps"}})
            page.get_by_role("button", name="Predicciones", exact=True).click()
            page.locator("#charts meter").first.wait_for()
            assert page.locator("#charts meter").count() == 2
            assert page.locator("#charts meter").first.get_attribute("value") == "125"
            page.locator("#admin-id").fill("browser-viewer")
            page.locator("#admin-display_name").fill("Browser Viewer")
            page.locator("#admin-form button").click()
            page.locator("#admin-status").filter(has_text="Operación completada").wait_for()
            page.get_by_role("button", name="Usuarios", exact=True).click()
            page.locator("#results").filter(has_text="browser-viewer").wait_for()
            page.locator("#admin-action").select_option("maintenance/retention")
            assert page.locator("#admin-dry_run").is_checked()
            page.locator("#admin-form button").click()
            page.locator("#admin-result").filter(has_text='"dry_run": true').wait_for()
            page.set_viewport_size({"width": 390, "height": 844})
            assert page.evaluate(
                "document.documentElement.scrollWidth <= window.innerWidth"
            )
            assert page.evaluate("localStorage.length") == 0
            page.get_by_role("button", name="Desconectar", exact=True).click()
            assert page.locator("#cluster-count").inner_text() == "—"
            assert not errors
            browser.close()
    finally:
        httpd.shutdown()
        httpd.server_close()
        thread.join(timeout=5)
        cp._store.close()
