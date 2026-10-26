// Package rbaccheck holds tests that pin the operator's RBAC. It has no runtime code: the
// generated ClusterRole (config/rbac/role.yaml) and the Helm chart's ClusterRole must both
// equal an explicit allow-list, so a permission can only change through a deliberate edit of
// this package together with docs/RBAC.md.
package rbaccheck
