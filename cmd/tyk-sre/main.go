// Command tyk-sre runs the SRE automations for Kubernetes in the cluster.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/raffaello-mdp/tyk-sre/api/v1alpha1"
	"github.com/raffaello-mdp/tyk-sre/internal/guard"
	"github.com/raffaello-mdp/tyk-sre/internal/hardening"
	"github.com/raffaello-mdp/tyk-sre/internal/httpapi"
	"github.com/raffaello-mdp/tyk-sre/internal/isolation"
)

// defaultNamespace is the namespace of the tool when POD_NAMESPACE is not set.
const defaultNamespace = "tyk-sre"

// config holds the command-line settings.
type config struct {
	apiAddr         string
	healthProbeAddr string
	metricsAddr     string
	namespace       string
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.apiAddr, "api-bind-address", "127.0.0.1:8080", "Address serving the hardening API.")
	flag.StringVar(&cfg.healthProbeAddr, "health-probe-bind-address", ":8081", "Address serving /healthz and /readyz.")
	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", `Address serving metrics, "0" disables them.`)
	flag.StringVar(&cfg.namespace, "namespace", namespaceFromEnv(), "Namespace of the tool, which it never acts on.")
	zapOptions := zap.Options{}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	if err := run(cfg); err != nil {
		ctrl.Log.Error(err, "manager stopped")
		os.Exit(1)
	}
}

// run starts the manager and blocks until it receives SIGTERM or SIGINT.
func run(cfg config) error {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("registering Kubernetes types: %w", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("registering Isolation types: %w", err)
	}

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("loading Kubernetes configuration: %w", err)
	}

	// The client must keep reading unstructured objects directly from the API
	// server, its default: a cached read of the Cilium policy would start a watch
	// on a kind that may not exist yet.
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: cfg.healthProbeAddr,
		Metrics:                metricsserver.Options{BindAddress: cfg.metricsAddr},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	namespaceGuard := guard.New(cfg.namespace)

	reconciler := &isolation.Reconciler{
		Client: mgr.GetClient(),
		Guard:  namespaceGuard,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the isolation controller: %w", err)
	}

	hardener := &hardening.Hardener{
		Reader:   mgr.GetAPIReader(),
		Writer:   mgr.GetClient(),
		Guard:    namespaceGuard,
		Defaults: hardening.DefaultResources(),
	}
	if err := mgr.Add(httpapi.NewServer(cfg.apiAddr, hardener)); err != nil {
		return fmt.Errorf("adding the hardening API: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("adding health check: %w", err)
	}
	// Ready only once the caches are synced, since decisions are based on them.
	cacheSynced := func(req *http.Request) error {
		if !mgr.GetCache().WaitForCacheSync(req.Context()) {
			return errors.New("caches are not synced")
		}
		return nil
	}
	if err := mgr.AddReadyzCheck("cache-sync", cacheSynced); err != nil {
		return fmt.Errorf("adding readiness check: %w", err)
	}

	ctrl.Log.Info("starting manager", "namespace", cfg.namespace)
	return mgr.Start(ctrl.SetupSignalHandler())
}

func namespaceFromEnv() string {
	if namespace := os.Getenv("POD_NAMESPACE"); namespace != "" {
		return namespace
	}
	return defaultNamespace
}
