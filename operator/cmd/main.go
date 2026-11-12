package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cache"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/config"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/controller"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execute"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execution"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/httpx"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/kubecost"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/metrics"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/ownership"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend/promsrc"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/resmetrics"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/stability"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/webhook"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr, probeAddr, webhookAddr, promURL, kubecostURL, engineURL, redisAddr, redisPass string
		redisDB                                                                                    int
		leaderElect, spotDrain, observeOnly, applier                                               bool
		cpuPrice, memPrice                                                                         float64
		drainTimeout, applierMaxObserve                                                            time.Duration
		applierPolicy, auditURL, auditTokenFile, auditCAFile                                       string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address of the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address of the probe endpoint.")
	flag.StringVar(&webhookAddr, "webhook-bind-address", envOr("WEBHOOK_BIND_ADDRESS", ""), "Optional bind address for the Pod MutatingAdmissionWebhook (/mutate-v1-pod).")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&promURL, "prometheus-url", envOr("PROMETHEUS_URL", "http://prometheus:9090"), "Prometheus base URL.")
	flag.StringVar(&kubecostURL, "kubecost-url", envOr("KUBECOST_URL", ""), "KubeCost API base URL (empty disables budget guardrails).")
	flag.StringVar(&engineURL, "engine-url", envOr("ENGINE_URL", "http://engine:8000"), "Unit economics engine base URL.")
	flag.StringVar(&redisAddr, "redis-addr", envOr("REDIS_ADDR", ""), "Redis address (empty uses an in-memory store).")
	flag.StringVar(&redisPass, "redis-password", os.Getenv("REDIS_PASSWORD"), "Redis password.")
	flag.IntVar(&redisDB, "redis-db", 0, "Redis database.")
	flag.Float64Var(&cpuPrice, "cpu-price-per-core-hour", 0.031, "Default CPU price (USD/core-hour).")
	flag.Float64Var(&memPrice, "mem-price-per-gib-hour", 0.004, "Default memory price (USD/GiB-hour).")
	flag.BoolVar(&observeOnly, "observe-only", false, "Compute and report every decision but write nothing to the cluster (global dry-run; also disables Spot draining).")
	flag.BoolVar(&applier, "enable-recommendation-applier", false, "Run the controller that applies APPROVED WorkloadRecommendations under a policy ConfigMap. Off by default; honours --observe-only.")
	flag.StringVar(&applierPolicy, "applier-policy-configmap", "", "namespace/name of the ConfigMap holding the applier policy (key policy.yaml). Required with the applier; without it nothing is applied.")
	flag.DurationVar(&applierMaxObserve, "applier-max-observe", 15*time.Minute, "Upper bound for the post-change observation window.")
	flag.StringVar(&auditURL, "audit-url", envOr("AUDIT_URL", ""), "Also POST every applier audit event as JSON to this append-only collector (https). Empty: Kubernetes Events only.")
	flag.StringVar(&auditTokenFile, "audit-token-file", "", "File with a bearer token for --audit-url.")
	flag.StringVar(&auditCAFile, "audit-ca-file", "", "PEM CA bundle trusted for --audit-url in addition to the system roots (a collector with a private CA).")
	flag.BoolVar(&spotDrain, "enable-spot-drain", true, "Cordon/drain Spot nodes with interruption signals.")
	flag.DurationVar(&drainTimeout, "drain-timeout", 2*time.Minute, "Maximum time to drain a node.")
	var promHTTP, kcHTTP, engHTTP, redisTLS httpx.Options
	var enableCostAutoscaler bool
	var kcAttempts int
	var redisUseTLS bool
	bindHTTP := func(prefix string, o *httpx.Options, tokenEnv string) {
		flag.StringVar(&o.CAFile, prefix+"-ca-file", "", "PEM CA bundle to verify the "+prefix+" server.")
		flag.StringVar(&o.CertFile, prefix+"-client-cert", "", "Client certificate for mTLS to "+prefix+".")
		flag.StringVar(&o.KeyFile, prefix+"-client-key", "", "Client key for mTLS to "+prefix+".")
		flag.StringVar(&o.BearerTokenFile, prefix+"-bearer-token-file", "", "File with a bearer token for "+prefix+" (re-read on every request).")
		o.BearerToken = os.Getenv(tokenEnv)
	}
	flag.BoolVar(&enableCostAutoscaler, "enable-cost-autoscaler", true, "Run the KubeCostAutoscaler controller (needs --kubecost-url).")
	flag.IntVar(&kcAttempts, "kubecost-max-attempts", 3, "Bounded attempts per KubeCost request (exponential backoff with jitter).")
	flag.BoolVar(&redisUseTLS, "redis-tls", false, "Connect to Redis over TLS.")
	flag.StringVar(&redisTLS.CAFile, "redis-ca-file", "", "PEM CA bundle to verify the Redis server.")
	flag.StringVar(&redisTLS.CertFile, "redis-client-cert", "", "Client certificate for mTLS to Redis.")
	flag.StringVar(&redisTLS.KeyFile, "redis-client-key", "", "Client key for mTLS to Redis.")
	bindHTTP("prometheus", &promHTTP, "PROMETHEUS_TOKEN")
	bindHTTP("kubecost", &kcHTTP, "KUBECOST_TOKEN")
	bindHTTP("engine", &engHTTP, "ENGINE_TOKEN")
	showVersion := flag.Bool("version", false, "Print the version and exit.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if *showVersion {
		fmt.Println(versionString())
		return
	}
	registerBuildInfo()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	if observeOnly {
		log.Info("observe-only mode: decisions are reported, nothing is written to the cluster; Spot draining is disabled")
		spotDrain = false
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "kubecost-auto-hpa.autoscaling.kubecost.io",
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		os.Exit(1)
	}
	cs, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Error(err, "unable to create clientset")
		os.Exit(1)
	}

	var store cache.Store = cache.NewMemory()
	if redisAddr != "" {
		var tlsCfg *tls.Config
		if redisUseTLS {
			var err error
			if tlsCfg, err = httpx.TLSConfig(redisTLS); err != nil {
				log.Error(err, "invalid Redis TLS configuration")
				os.Exit(1)
			}
		}
		store = cache.NewRedisTLS(redisAddr, redisPass, redisDB, tlsCfg)
		log.Info("using redis store", "addr", redisAddr, "tls", redisUseTLS)
	} else {
		log.Info("redis not configured, using in-memory store")
	}
	mustClient := func(name string, o httpx.Options, d time.Duration) *http.Client {
		c, err := httpx.NewClient(o, d)
		if err != nil {
			log.Error(err, "invalid TLS/auth configuration", "backend", name)
			os.Exit(1)
		}
		return c
	}
	prom := metrics.NewPrometheus(promURL, metrics.DefaultQueries())
	prom.HTTP = mustClient("prometheus", promHTTP, 10*time.Second)
	eng := engine.NewHTTP(engineURL)
	eng.Client = mustClient("engine", engHTTP, 10*time.Second)
	var spend metrics.SpendSource
	if kubecostURL != "" {
		kc := metrics.NewKubeCost(kubecostURL)
		kc.HTTP = mustClient("kubecost", kcHTTP, 15*time.Second)
		spend = kc
	}

	if err := (&controller.KubeCostHPAReconciler{
		Client:      mgr.GetClient(),
		ObserveOnly: observeOnly,
		Resizer:     &actuator.Resizer{Client: cs},
		Metrics:     prom,
		Spend:       spend,
		Engine:      eng,
		Store:       store,
		Recorder:    mgr.GetEventRecorderFor("kubecost-auto-hpa"),
		Defaults:    config.Defaults{CPUPricePerCoreHour: cpuPrice, MemPricePerGiBHour: memPrice},
		Stability:   stability.NewController(stability.DefaultConfig()),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create KubeCostHPA controller")
		os.Exit(1)
	}
	if spotDrain {
		if err := (&controller.NodeReconciler{
			Client:   mgr.GetClient(),
			Drainer:  &actuator.Drainer{Client: cs, Timeout: drainTimeout},
			Recorder: mgr.GetEventRecorderFor("kubecost-auto-hpa"),
		}).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to create spot node controller")
			os.Exit(1)
		}
	}
	if enableCostAutoscaler {
		if kubecostURL == "" {
			log.Info("KubeCostAutoscaler controller disabled: --kubecost-url is not set")
		} else {
			kcClient, err := kubecost.New(kubecostURL, mustClient("kubecost", kcHTTP, 15*time.Second))
			if err != nil {
				log.Error(err, "invalid KubeCost configuration")
				os.Exit(1)
			}
			kcClient.MaxAttempts = kcAttempts
			metricsCS, err := metricsclient.NewForConfig(mgr.GetConfig())
			if err != nil {
				log.Error(err, "unable to create the metrics.k8s.io client")
				os.Exit(1)
			}
			if err := (&controller.KubeCostAutoscalerReconciler{
				Client:      mgr.GetClient(),
				ObserveOnly: observeOnly,
				Scheme:      mgr.GetScheme(),
				Cost:        kcClient,
				Resources:   &resmetrics.Client{Metrics: metricsCS, Discovery: cs.Discovery()},
				Scaler:      &actuator.DeploymentScaler{Client: cs},
				Fallback:    &ownership.Fallback{Client: mgr.GetClient(), Scheme: mgr.GetScheme()},
				Recorder:    mgr.GetEventRecorderFor("kubecost-autoscaler"),
			}).SetupWithManager(mgr); err != nil {
				log.Error(err, "unable to create KubeCostAutoscaler controller")
				os.Exit(1)
			}
		}
	}
	if applier {
		ns, name, ok := strings.Cut(applierPolicy, "/")
		if !ok || ns == "" || name == "" {
			log.Error(errors.New("--applier-policy-configmap must be namespace/name"), "the recommendation applier needs a policy ConfigMap")
			os.Exit(1)
		}
		dyn, err := dynamic.NewForConfig(mgr.GetConfig())
		if err != nil {
			log.Error(err, "unable to create the dynamic client")
			os.Exit(1)
		}
		var extra execute.Sink
		if auditURL != "" {
			if !strings.HasPrefix(auditURL, "https://") {
				log.Error(errors.New("--audit-url must be https"), "refusing to ship audit events in clear text")
				os.Exit(1)
			}
			tok := ""
			if auditTokenFile != "" {
				b, err := os.ReadFile(auditTokenFile)
				if err != nil {
					log.Error(err, "cannot read --audit-token-file")
					os.Exit(1)
				}
				tok = strings.TrimSpace(string(b))
			}
			hc, err := execute.AuditHTTPClient(auditCAFile)
			if err != nil {
				log.Error(err, "cannot load --audit-ca-file")
				os.Exit(1)
			}
			hs := execute.NewHTTPSink(auditURL, tok, hc)
			// a long-running operator must not learn of lost audit events only at shutdown: log each, and export both counts
			hs.OnDrop = func(reason string) {
				log.Error(nil, "an audit event was NOT delivered to --audit-url", "reason", reason)
			}
			ctrlmetrics.Registry.MustRegister(
				prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "kubecost_audit_events_not_delivered_total",
					Help: "Audit events given up by the --audit-url sender (queue full or retry budget exhausted)."}, func() float64 { return float64(hs.Dropped()) }),
				prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "kubecost_audit_events_delivered_total",
					Help: "Audit events the --audit-url collector accepted."}, func() float64 { return float64(hs.Delivered()) }))
			extra = hs
			_ = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
				<-ctx.Done()
				if err := hs.Close(); err != nil {
					log.Error(err, "audit shipping")
				}
				return nil
			}))
		}
		if err := (&controller.RecommendationApplier{
			AuditExtra: extra,
			Client:     mgr.GetClient(), APIReader: mgr.GetAPIReader(), KC: cs, Dyn: dyn, Recorder: mgr.GetEventRecorderFor("kubecost-recommendation-applier"),
			PolicyNamespace: ns, PolicyName: name, ObserveOnly: observeOnly, MaxObserve: applierMaxObserve, MaxRollout: applierMaxObserve,
			Prom: &promsrc.Client{BaseURL: promURL, HTTP: mustClient("prometheus", promHTTP, 10*time.Second)},
		}).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to create the recommendation applier")
			os.Exit(1)
		}
		log.Info("recommendation applier enabled", "policy", applierPolicy, "observeOnly", observeOnly)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	if webhookAddr != "" {
		mux := http.NewServeMux()
		whMode := execution.ModeActive
		if observeOnly { // observe-only covers every writer, the admission webhook included
			whMode = execution.ModeObserve
		}
		mux.Handle("/mutate-v1-pod", &webhook.PodMutator{DefaultExecutionMode: whMode})
		whSrv := &http.Server{Addr: webhookAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			log.Info("starting pod mutating admission webhook server", "addr", webhookAddr)
			if werr := whSrv.ListenAndServe(); werr != nil && werr != http.ErrServerClosed {
				log.Error(werr, "webhook server exited")
			}
		}()
	}

	log.Info("starting manager", "version", version, "commit", buildCommit())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
