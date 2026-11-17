package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/approval"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execute"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

var wrecGVR = schema.GroupVersionResource{Group: "autoscaling.kubecost.io", Version: "v1alpha1", Resource: "workloadrecommendations"}

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// wrecName is a stable DNS-1123 name: the same recommendation always maps to the same object, so publishing twice is idempotent.
func wrecName(rec recommend.Recommendation) string {
	n := strings.ToLower(rec.Workload + "-" + rec.ID)
	n = strings.Trim(nonDNS.ReplaceAllString(n, "-"), "-")
	if len(n) > 63 {
		n = strings.Trim(n[:63], "-")
	}
	return n
}

func summaryOf(rec recommend.Recommendation) map[string]any {
	line := func(x recommend.ResourceResult, unit func(float64) string) string {
		if x.Action != recommend.Increase && x.Action != recommend.Decrease {
			return string(x.Action)
		}
		return fmt.Sprintf("%s %s -> %s", x.Action, unit(x.Current), unit(x.Recommended))
	}
	cpu := func(v float64) string { return fmt.Sprintf("%.0fm", v*1000) }
	mem := func(v float64) string { return fmt.Sprintf("%.0fMi", v/(1<<20)) }
	more := ""
	if n := len(rec.Additional); n > 0 {
		more = fmt.Sprintf(" (+%d more container(s), see record)", n)
	}
	return map[string]any{
		"cpu": line(rec.CPU, cpu) + more, "memory": line(rec.Memory, mem) + more, "confidence": string(rec.Confidence),
		"potentialMonthlyUSD": fmt.Sprintf("%.2f (ESTIMATE)", sumUSD(rec)),
	}
}

// publishRecommendation creates the WorkloadRecommendation object. It changes nothing else in the cluster.
func publishRecommendation(ctx context.Context, dyn dynamic.Interface, rec recommend.Recommendation, sloQueryName, publisher string) (string, error) {
	if publisher == "" {
		return "", errors.New("cannot publish without knowing who you are (the publisher is recorded and, with separation of duties on, checked by the API server)")
	}
	if rec.Abstained || !(isChange(rec.CPU) || isChange(rec.Memory)) {
		return "", errors.New("the recommendation changes nothing (or abstained): there is nothing to publish")
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	spec := map[string]any{
		"recommendationID": rec.ID,
		"target":           map[string]any{"kind": rec.Kind, "name": rec.Workload, "container": rec.Container},
		"record":           string(raw),
		"summary":          summaryOf(rec),
	}
	if sloQueryName != "" {
		spec["sloQueryName"] = sloQueryName
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling.kubecost.io/v1alpha1", "kind": "WorkloadRecommendation",
		"metadata": map[string]any{"name": wrecName(rec), "namespace": rec.Namespace,
			"annotations": map[string]any{"autoscaling.kubecost.io/published-by": publisher}},
		"spec": spec,
	}}
	_, err = dyn.Resource(wrecGVR).Namespace(rec.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return wrecName(rec), nil // the same recommendation was already published
	}
	if err != nil {
		return "", fmt.Errorf("publishing the recommendation: %w", err)
	}
	return wrecName(rec), nil
}

func isChange(x recommend.ResourceResult) bool {
	return x.Action == recommend.Increase || x.Action == recommend.Decrease
}

// approveRecommendation writes status.approval through the status subresource, with the identity the API server reports.
// Needs "update workloadrecommendations/status": a permission that publishing alone does not give.
func approveRecommendation(ctx context.Context, ex *execute.Executor, rec recommend.Recommendation) (string, error) {
	changes := false
	for _, v := range rec.Views() {
		changes = changes || v.Changes()
	}
	if rec.Abstained || !changes {
		return "", errors.New("this recommendation changes nothing (or abstained): there is nothing to approve")
	}
	if err := ex.Fresh(ctx, rec); err != nil {
		return "", err
	}
	who, err := ex.Identity(ctx)
	if err != nil {
		return "", err
	}
	at := time.Now().UTC().Format(time.RFC3339)
	ap := map[string]any{"recommendationID": rec.ID, "by": who, "at": at}
	if ex.SignKey != nil {
		sig, err := approval.Sign(ex.SignKey, rec, who, at)
		if err != nil {
			return "", err
		}
		ap["signature"] = sig
	}
	name := wrecName(rec)
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cur, err := ex.Dyn.Resource(wrecGVR).Namespace(rec.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if id, _, _ := unstructured.NestedString(cur.Object, "spec", "recommendationID"); id != rec.ID {
			return fmt.Errorf("the object %s holds recommendation %q, not %q", name, id, rec.ID)
		}
		if err := unstructured.SetNestedField(cur.Object, ap, "status", "approval"); err != nil {
			return err
		}
		_, err = ex.Dyn.Resource(wrecGVR).Namespace(rec.Namespace).UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return "", fmt.Errorf("approving through the status subresource: %w", err)
	}
	return who, nil
}

func sumUSD(rec recommend.Recommendation) float64 {
	c, m := rec.PotentialMonthlyUSD()
	return c + m
}
