// Package resmetrics reads resource utilisation (usage / request) of a
// workload from the Kubernetes resource metrics API (metrics.k8s.io, served by
// metrics-server). It never assumes the API is installed.
package resmetrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/discovery"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// ErrUnavailable means the metrics API is missing or returned nothing usable.
var ErrUnavailable = errors.New("resource metrics unavailable")

// Utilization is the validated demand signal.
type Utilization struct {
	// Value is usage/request (1.0 == 100%) for the most utilised resource.
	Value    float64
	Resource corev1.ResourceName
	// Pods is the number of ready pods that contributed.
	Pods int
	// OldestSampleAge is the age of the oldest PodMetrics sample that contributed.
	OldestSampleAge time.Duration
}

// Reader provides utilisation and API availability.
type Reader interface {
	// Utilization returns the utilisation of the ready pods. maxAge bounds the
	// age of every PodMetrics sample used.
	Utilization(ctx context.Context, namespace string, selector labels.Selector, pods []corev1.Pod,
		resources []corev1.ResourceName, now time.Time, maxAge time.Duration) (Utilization, error)
	// Available verifies that metrics.k8s.io/v1beta1 serves pod metrics.
	Available(ctx context.Context) error
}

// MinCoverage is the share of ready pods that must have fresh metrics.
const MinCoverage = 0.5

// Compute is the pure part: it validates samples and reduces them to a ratio.
func Compute(pods []corev1.Pod, samples []metricsv1beta1.PodMetrics, resources []corev1.ResourceName,
	now time.Time, maxAge time.Duration) (Utilization, error) {
	ready := map[string]*corev1.Pod{}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && podReady(p) {
			ready[p.Name] = p
		}
	}
	if len(ready) == 0 {
		return Utilization{}, fmt.Errorf("%w: no ready pods", ErrUnavailable)
	}
	best := Utilization{Value: -1}
	var firstErr error
	for _, res := range resources {
		var usage, request float64
		var oldest time.Duration
		used := 0
		for i := range samples {
			s := &samples[i]
			p, ok := ready[s.Name]
			if !ok {
				continue
			}
			if age := now.Sub(s.Timestamp.Time); age > maxAge || age < -5*time.Minute {
				continue // stale (or implausibly future) sample
			}
			req, ok := podRequest(p, res)
			if !ok {
				continue // HPA semantics: pods without a request cannot be measured
			}
			u, ok := podUsage(s, res)
			if !ok {
				continue
			}
			usage += u
			request += req
			used++
			if age := now.Sub(s.Timestamp.Time); age > oldest {
				oldest = age
			}
		}
		if float64(used) < MinCoverage*float64(len(ready)) || request <= 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("%w: %s metrics cover %d of %d ready pods (need %.0f%%), or requests are missing",
					ErrUnavailable, res, used, len(ready), MinCoverage*100)
			}
			continue
		}
		v := usage / request
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if v > best.Value {
			best = Utilization{Value: v, Resource: res, Pods: used, OldestSampleAge: oldest}
		}
	}
	if best.Value < 0 {
		if firstErr == nil {
			firstErr = ErrUnavailable
		}
		return Utilization{}, firstErr
	}
	return best, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podRequest sums the container requests; ok=false if any container lacks one.
func podRequest(p *corev1.Pod, res corev1.ResourceName) (float64, bool) {
	var total float64
	for i := range p.Spec.Containers {
		q, ok := p.Spec.Containers[i].Resources.Requests[res]
		if !ok || q.IsZero() {
			return 0, false
		}
		total += quantityValue(res, q.MilliValue(), q.Value())
	}
	return total, len(p.Spec.Containers) > 0
}

func podUsage(s *metricsv1beta1.PodMetrics, res corev1.ResourceName) (float64, bool) {
	var total float64
	for i := range s.Containers {
		q, ok := s.Containers[i].Usage[res]
		if !ok {
			return 0, false
		}
		total += quantityValue(res, q.MilliValue(), q.Value())
	}
	return total, len(s.Containers) > 0
}

func quantityValue(res corev1.ResourceName, milli, whole int64) float64 {
	if res == corev1.ResourceCPU {
		return float64(milli)
	}
	return float64(whole)
}

// Client is the metrics.k8s.io implementation.
type Client struct {
	Metrics   metricsclient.Interface
	Discovery discovery.DiscoveryInterface

	mu        sync.Mutex
	checkedAt time.Time
	checkErr  error
	// AvailabilityTTL caches the discovery result (default 30s).
	AvailabilityTTL time.Duration
	Now             func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Available reports whether pod metrics are served.
func (c *Client) Available(ctx context.Context) error {
	ttl := c.AvailabilityTTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.checkedAt.IsZero() && c.now().Sub(c.checkedAt) < ttl {
		return c.checkErr
	}
	c.checkErr = c.discover()
	c.checkedAt = c.now()
	return c.checkErr
}

func (c *Client) discover() error {
	list, err := c.Discovery.ServerResourcesForGroupVersion("metrics.k8s.io/v1beta1")
	if err != nil {
		return fmt.Errorf("%w: metrics.k8s.io/v1beta1: %v", ErrUnavailable, err)
	}
	for _, r := range list.APIResources {
		if r.Name == "pods" {
			return nil
		}
	}
	return fmt.Errorf("%w: metrics.k8s.io/v1beta1 does not serve pods", ErrUnavailable)
}

// Utilization lists PodMetrics for the selector and reduces them.
func (c *Client) Utilization(ctx context.Context, namespace string, selector labels.Selector, pods []corev1.Pod,
	resources []corev1.ResourceName, now time.Time, maxAge time.Duration) (Utilization, error) {
	if err := c.Available(ctx); err != nil {
		return Utilization{}, err
	}
	list, err := c.Metrics.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return Utilization{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return Compute(pods, list.Items, resources, now, maxAge)
}
