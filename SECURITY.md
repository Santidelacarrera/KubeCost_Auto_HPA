# Security policy

Please report vulnerabilities privately through GitHub: **Security → Report a vulnerability** on this repository
(private vulnerability reporting). Do not open a public issue for security problems.

Include the affected version or commit, the impact and a minimal reproduction. Expect an acknowledgement and a
fix or mitigation plan; this is a small project, so there is no formal response-time guarantee.

The operator needs write access to `deployments/scale`, `pods/resize` and `horizontalpodautoscalers`, plus node
cordon/eviction when spot draining is enabled. Review `operator/config/rbac/role.yaml` before installing, and see
the Security section of `README.md` for the authentication and TLS options towards Prometheus, KubeCost, the
engine and Redis.
