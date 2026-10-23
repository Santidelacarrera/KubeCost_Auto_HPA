// Package infra classifies the nodes hosting a workload as Spot/Preemptible,
// On-Demand or Unknown.
//
// Node labels are classification *inputs*, not proof of what is billed: a label
// says how the node was provisioned, not what it costs. Rules are
// configurable because label conventions differ per provider and per
// provisioning tool.
package infra

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Class is the capacity class of a node.
type Class string

const (
	Spot     Class = "spot"
	OnDemand Class = "ondemand"
	Unknown  Class = "unknown"
)

// Rule matches nodes carrying Key; when Values is non-empty the label value
// must equal one of them (case-insensitive).
type Rule struct {
	Key    string
	Values []string
}

func (r Rule) matches(labels map[string]string) bool {
	v, ok := labels[r.Key]
	if !ok {
		return false
	}
	if len(r.Values) == 0 {
		return true
	}
	for _, want := range r.Values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

// Classifier holds the label rules.
type Classifier struct {
	SpotRules     []Rule
	OnDemandRules []Rule
}

// DefaultClassifier covers the common provider conventions. They are defaults,
// not guarantees: verify them against your own node labels and override them in
// the policy when they differ.
func DefaultClassifier() *Classifier {
	return &Classifier{
		SpotRules: []Rule{
			{Key: "eks.amazonaws.com/capacityType", Values: []string{"SPOT"}},
			{Key: "karpenter.sh/capacity-type", Values: []string{"spot"}},
			{Key: "cloud.google.com/gke-spot", Values: []string{"true"}},
			{Key: "cloud.google.com/gke-preemptible", Values: []string{"true"}},
			{Key: "cloud.google.com/gke-provisioning", Values: []string{"spot", "preemptible"}},
			{Key: "kubernetes.azure.com/scalesetpriority", Values: []string{"spot"}},
		},
		OnDemandRules: []Rule{
			{Key: "eks.amazonaws.com/capacityType", Values: []string{"ON_DEMAND"}},
			{Key: "karpenter.sh/capacity-type", Values: []string{"on-demand"}},
			{Key: "cloud.google.com/gke-provisioning", Values: []string{"standard"}},
			{Key: "kubernetes.azure.com/scalesetpriority", Values: []string{"regular"}},
		},
	}
}

// ClassifyNode applies the rules. A node matching both a Spot and an On-Demand
// rule is contradictory and therefore Unknown.
func (c *Classifier) ClassifyNode(n *corev1.Node) Class {
	if n == nil {
		return Unknown
	}
	spot := anyMatch(c.SpotRules, n.Labels)
	od := anyMatch(c.OnDemandRules, n.Labels)
	switch {
	case spot && !od:
		return Spot
	case od && !spot:
		return OnDemand
	default:
		return Unknown
	}
}

func anyMatch(rules []Rule, labels map[string]string) bool {
	for _, r := range rules {
		if r.matches(labels) {
			return true
		}
	}
	return false
}

// Summary counts the workload's pods per capacity class.
type Summary struct {
	Spot, OnDemand, Unknown int32
	// Pending are pods not yet bound to a node; Unschedulable is the subset the
	// scheduler reported as unschedulable.
	Pending, Unschedulable int32
	// Terminating pods are excluded from the class counts.
	Terminating int32
}

// Placed is the number of running, bound, non-terminating pods.
func (s Summary) Placed() int32 { return s.Spot + s.OnDemand + s.Unknown }

// SpotFraction is the share of placed pods on Spot capacity (0 when none).
func (s Summary) SpotFraction() float64 {
	if p := s.Placed(); p > 0 {
		return float64(s.Spot) / float64(p)
	}
	return 0
}

// NodeLookup returns a node by name (typically a cached reader).
type NodeLookup func(name string) (*corev1.Node, bool)

// Summarize classifies the pods. Pods on nodes that cannot be found count as
// Unknown (the conservative default: priced as On-Demand, not discounted).
func (c *Classifier) Summarize(pods []corev1.Pod, nodes NodeLookup) Summary {
	var s Summary
	cache := map[string]Class{}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			s.Terminating++
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if p.Spec.NodeName == "" {
			s.Pending++
			if isUnschedulable(p) {
				s.Unschedulable++
			}
			continue
		}
		cl, ok := cache[p.Spec.NodeName]
		if !ok {
			n, found := nodes(p.Spec.NodeName)
			if !found {
				cl = Unknown
			} else {
				cl = c.ClassifyNode(n)
			}
			cache[p.Spec.NodeName] = cl
		}
		switch cl {
		case Spot:
			s.Spot++
		case OnDemand:
			s.OnDemand++
		default:
			s.Unknown++
		}
	}
	return s
}

func isUnschedulable(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return true
		}
	}
	return false
}

// CapacityFactor is the average effective capacity of a replica in (0,1]:
// Spot pods count as (1-buffer) of an On-Demand pod, because Spot capacity can
// be reclaimed and is never guaranteed. buffer is clamped to [0,0.9].
func CapacityFactor(s Summary, buffer float64) float64 {
	if buffer < 0 {
		buffer = 0
	}
	if buffer > 0.9 {
		buffer = 0.9
	}
	return 1 - s.SpotFraction()*buffer
}

// Rules returns a copy of the rules sorted by key, for stable status/logs.
func (c *Classifier) Rules() (spot, onDemand []Rule) {
	spot = append(spot, c.SpotRules...)
	onDemand = append(onDemand, c.OnDemandRules...)
	sort.Slice(spot, func(i, j int) bool { return spot[i].Key < spot[j].Key })
	sort.Slice(onDemand, func(i, j int) bool { return onDemand[i].Key < onDemand[j].Key })
	return
}
