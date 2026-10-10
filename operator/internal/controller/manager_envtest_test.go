//go:build envtest && integration

package controller

// Full-process test: the real manager binary (cmd/) talks to a real
// kube-apiserver, the real Python engine and a fake Prometheus. Only the
// kubelet is absent, so the test marks pods Running.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	user, err := env.ControlPlane.AddUser(envtest.User{Name: "operator", Groups: []string{"system:masters"}}, nil)
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
	var logs bytes.Buffer
	mgr := exec.Command(bin, "--prometheus-url="+ps.URL, "--engine-url="+engURL,
		"--metrics-bind-address=0", "--health-probe-bind-address=0", "--enable-spot-drain=false")
	mgr.Env = append(os.Environ(), "KUBECONFIG="+kcPath)
	mgr.Stdout, mgr.Stderr = &logs, &logs
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
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("pods were never resized\nmanager logs:\n%s", logs.String())
}
