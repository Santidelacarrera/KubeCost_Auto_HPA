//go:build envtest && integration

package controller

// Full-process test: the real manager binary (cmd/) talks to a real
// kube-apiserver, the real Python engine and a fake Prometheus. Only the
// kubelet is absent, so the test marks pods Running.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
)

func TestManagerBinaryAgainstRealAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}
	engURL := startEngine(t)
	prom := &promState{}
	prom.set("200", "50", "100") // 2 pods: p99 50ms vs SLO 200ms, 100m used per pod
	ps := httptest.NewServer(http.HandlerFunc(prom.handler))
	defer ps.Close()

	crd, _ := filepath.Abs("../../config/crd/bases")
	env := &envtest.Environment{CRDDirectoryPaths: []string{crd}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Stop()
	user, err := restrictedOperator(env, cfg)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	kcPath := filepath.Join(dir, "kubeconfig")
	os.WriteFile(kcPath, kc, 0o600)

	bin := filepath.Join(dir, "manager")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd").CombinedOutput(); err != nil {
		t.Fatalf("build manager: %v\n%s", err, out)
	}
	logs := &syncBuffer{}
	mgr := exec.Command(bin, "--prometheus-url="+ps.URL, "--engine-url="+engURL,
		"--metrics-bind-address=0", "--health-probe-bind-address=0", "--enable-spot-drain=false")
	mgr.Env = append(os.Environ(), "KUBECONFIG="+kcPath)
	mgr.Stdout, mgr.Stderr = logs, logs
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Process.Kill(); _ = mgr.Wait() }()

	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	c, _ := client.New(cfg, client.Options{Scheme: s})
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%v\nmanager logs:\n%s", err, logs.String())
		}
	}
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}))
	dep := newDeployment()
	dep.Spec.Template.Spec.Containers[0].Image = "nginx"
	must(c.Create(ctx, dep))
	mkPod(t, c, "api-1", "node-a")
	mkPod(t, c, "api-2", "node-a")
	must(c.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling.kubecost.io/v1alpha1", "kind": "KubeCostHPA",
		"metadata": map[string]any{"name": "api", "namespace": "shop"},
		"spec": map[string]any{"targetRef": map[string]any{"name": "api"}, "slo": map[string]any{"p99LatencyMs": int64(200)},
			"cooldown": "1s", "interval": "1s"},
	}}))

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var p corev1.Pod
		if err := c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api-2"}, &p); err == nil &&
			p.Spec.Containers[0].Resources.Requests.Cpu().MilliValue() < 1000 {
			var h v1alpha1.KubeCostHPA
			must(c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api"}, &h))
			if h.Status.DesiredCPUMillicores == 0 || h.Status.LastAction == "" {
				t.Fatalf("status not populated: %+v", h.Status)
			}
			if !strings.Contains(logs.String(), "starting manager") {
				t.Fatal("manager did not log startup")
			}
			// the usage floor (100m / 0.7 = 143m) bounds how far it may shrink
			if got := p.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(); got < 143 {
				t.Fatalf("shrunk below the usage floor: %dm", got)
			}
			deleteAndExpectRestore(t, c, logs)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("pods were never resized\nmanager logs:\n%s", logs.String())
}

// Deleting the KubeCostHPA must hand the pods back at their template resources
// (the finalizer) and then let the resource go.
func deleteAndExpectRestore(t *testing.T, c client.Client, logs *syncBuffer) {
	t.Helper()
	ctx := context.Background()
	var h v1alpha1.KubeCostHPA
	if err := c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api"}, &h); err != nil {
		t.Fatal(err)
	}
	if len(h.Finalizers) == 0 {
		t.Fatal("finalizer was not added by the manager")
	}
	if err := c.Delete(ctx, &h); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var p corev1.Pod
		cpuOK := c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api-2"}, &p) == nil &&
			p.Spec.Containers[0].Resources.Requests.Cpu().MilliValue() == 1000
		gone := apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api"}, &v1alpha1.KubeCostHPA{}))
		if cpuOK && gone {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("pods were not restored / resource not released after delete\nmanager logs:\n%s", logs.String())
}

// The KubeCostAutoscaler controller wired by main(): flags, KubeCost client,
// field index, watches. envtest has no metrics-server, so the controller must
// notice that, report it and hold (not guess).
func TestManagerRunsKubeCostAutoscalerAgainstRealAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}
	var hits int32
	kc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(401)
			return
		}
		end := time.Now().UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"code":200,"status":"success","data":[{"production/payments-api":{"name":"payments-api",
			"properties":{"cluster":"c1","namespace":"production","controller":"payments-api","controllerKind":"deployment"},
			"window":{"start":"%s","end":"%s"},"start":"%s","end":"%s","minutes":15,"totalCost":0.75}}]}`, end, end, end, end)
	}))
	defer kc.Close()

	crd, _ := filepath.Abs("../../config/crd/bases")
	env := &envtest.Environment{CRDDirectoryPaths: []string{crd}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Stop()
	user, err := restrictedOperator(env, cfg)
	if err != nil {
		t.Fatal(err)
	}
	kcfg, _ := user.KubeConfig()
	dir := t.TempDir()
	kcPath := filepath.Join(dir, "kubeconfig")
	os.WriteFile(kcPath, kcfg, 0o600)
	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte("test-token\n"), 0o600)

	bin := filepath.Join(dir, "manager")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd").CombinedOutput(); err != nil {
		t.Fatalf("build manager: %v\n%s", err, out)
	}
	logs := &syncBuffer{}
	mgr := exec.Command(bin, "--kubecost-url="+kc.URL, "--kubecost-bearer-token-file="+tokenFile,
		"--metrics-bind-address=0", "--health-probe-bind-address=0", "--enable-spot-drain=false")
	mgr.Env = append(os.Environ(), "KUBECONFIG="+kcPath)
	mgr.Stdout, mgr.Stderr = logs, logs
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Process.Kill(); _ = mgr.Wait() }()

	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	c, _ := client.New(cfg, client.Options{Scheme: s})
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%v\nmanager logs:\n%s", err, logs.String())
		}
	}
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "production"}}))
	lbl := map[string]string{"app": "payments-api"}
	replicas := int32(3)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "payments-api", Namespace: "production"},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}}}}}
	must(c.Create(ctx, dep))
	must(c.Create(ctx, kcaObject("payments-autoscaler", map[string]any{
		"costPolicy": map[string]any{"maxHourlyCostUSD": "5.00"}, "evaluationInterval": "5s"})))

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var a v1alpha1.KubeCostAutoscaler
		if err := c.Get(ctx, client.ObjectKey{Namespace: "production", Name: "payments-autoscaler"}, &a); err == nil {
			mc := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionMetricsAvailable)
			if mc != nil && mc.Status == metav1.ConditionFalse && mc.Reason == "DemandUnavailable" {
				var d appsv1.Deployment
				must(c.Get(ctx, client.ObjectKey{Namespace: "production", Name: "payments-api"}, &d))
				if *d.Spec.Replicas != 3 {
					t.Fatalf("replicas changed to %d without any demand signal", *d.Spec.Replicas)
				}
				if a.Status.LastDecision == nil || a.Status.LastDecision.Type != "InvalidMetrics" || a.Status.CurrentReplicas != 3 {
					t.Fatalf("%+v", a.Status)
				}
				if atomic.LoadInt32(&hits) == 0 {
					t.Fatal("the manager never queried KubeCost with the bearer token")
				}
				if strings.Contains(logs.String(), "test-token") {
					t.Fatalf("the bearer token leaked into the logs:\n%s", logs.String())
				}
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the KubeCostAutoscaler was never reconciled\nmanager logs:\n%s", logs.String())
}

// syncBuffer collects the manager's output. exec copies stdout and stderr on their
// own goroutines, so the test reads the logs while they are still being written.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
