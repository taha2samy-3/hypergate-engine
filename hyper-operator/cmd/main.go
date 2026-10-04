package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
	"github.com/taha2samy/hypergate/hyper-operator/internal/controller"
	"github.com/taha2samy/hypergate/hyper-operator/internal/identity"
	"github.com/taha2samy/hypergate/hyper-operator/internal/leader"
	"github.com/taha2samy/hypergate/hyper-operator/internal/routes"
	"github.com/taha2samy/hypergate/hyper-operator/internal/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(hyperv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	election := leader.DefaultElectionConfig()
	var identityCfg identity.Config
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	election.BindFlags(flag.CommandLine)
	identityCfg.BindFlags(flag.CommandLine)
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := election.Validate(); err != nil {
		setupLog.Error(err, "invalid leader election flags")
		os.Exit(1)
	}
	if err := identityCfg.Validate(); err != nil {
		setupLog.Error(err, "invalid identity flags")
		os.Exit(1)
	}

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	}
	election.Apply(&mgrOpts)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	if election.Enabled {
		setupLog.Info("Leader election enabled",
			"lease", election.LeaseName,
			"leaseDuration", election.LeaseDuration,
			"renewDeadline", election.RenewDeadline,
			"retryPeriod", election.RetryPeriod,
			"releaseOnCancel", election.ReleaseOnCancel)
	}
	if err := mgr.Add(leader.NewLeadershipReporter()); err != nil {
		setupLog.Error(err, "unable to add leadership reporter")
		os.Exit(1)
	}

	identitySettings := engineIdentitySettings(&identityCfg)
	if identityCfg.Enabled {
		identityCache, err := identity.NewCacheForConfig(mgr.GetConfig(), identityCfg.Options())
		if err == nil {
			err = mgr.Add(identityCache.Runnable())
		}
		if err != nil {
			setupLog.Error(err, "unable to set up the identity cache")
			os.Exit(1)
		}
		setupLog.Info("Identity cache enabled on every replica", "trustDomain", identityCfg.TrustDomain)

		if identityCfg.ServerEnabled {
			if err := setupIdentityServer(mgr, &identityCfg, identityCache); err != nil {
				setupLog.Error(err, "unable to set up the identity server")
				os.Exit(1)
			}
		}
	}

	if err = (&controller.HyperRedisReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("hyperredis-controller"), //nolint:staticcheck
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HyperRedis")
		os.Exit(1)
	}

	if err = (&controller.HyperConfigReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Identity: identitySettings,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HyperConfig")
		os.Exit(1)
	}

	if err = (&controller.HyperChainMasterCompilerReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Identity: identitySettings,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HyperChainMasterCompiler")
		os.Exit(1)
	}

	if err = (&controller.ExtProcReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("hypergate-extproc"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ExtProc")
		os.Exit(1)
	}

	enableWebhooks := os.Getenv("ENABLE_WEBHOOKS")
	if enableWebhooks == "false" {
		setupLog.Info("Webhooks are explicitly disabled via ENABLE_WEBHOOKS env var, skipping webhook registration.")
	} else {
		if err = webhook.SetupFiltersWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhooks", "webhook", "FilterProtection")
			os.Exit(1)
		}
		if err = webhook.SetupHyperChainWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhooks", "webhook", "HyperChainValidation")
			os.Exit(1)
		}
		if err = webhook.SetupHyperRouteWebhookWithManager(mgr, routes.Options{Workloads: identitySettings.Enabled}); err != nil {
			setupLog.Error(err, "unable to create webhooks", "webhook", "HyperRouteValidation")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	// Start returns when the signal handler fires or when leadership is lost. Exit
	// right away: LeaderElectionReleaseOnCancel relies on the process ending as soon
	// as the manager has stopped.
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// setupIdentityServer adds the leader-only identity stream server and the
// publisher that points the identity Service at the leader.
func setupIdentityServer(mgr ctrl.Manager, cfg *identity.Config, cache *identity.Cache) error {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}
	reader := mgr.GetClient()
	auth := &identity.TokenReviewAuthenticator{
		Client: clientset,
		// Only engine ServiceAccounts in namespaces that run an engine may read identities.
		Allowed: func(ctx context.Context, username string) bool {
			namespaces, err := controller.EngineNamespaces(ctx, reader)
			if err != nil {
				return false
			}
			for ns := range namespaces {
				if username == identity.ServiceAccountUsername(ns, controller.EngineServiceAccountName) {
					return true
				}
			}
			return false
		},
	}
	server, err := identity.NewServer(cache.Index(), cfg.ServerOptions(auth))
	if err != nil {
		return err
	}
	port, err := cfg.ServerPort()
	if err != nil {
		return err
	}
	publisher := &identity.EndpointPublisher{
		Client:      clientset,
		Namespace:   os.Getenv("POD_NAMESPACE"),
		ServiceName: cfg.ServiceName,
		PodName:     os.Getenv("POD_NAME"),
		PodIP:       os.Getenv("POD_IP"),
		Port:        port,
	}
	if publisher.Namespace == "" || publisher.PodIP == "" {
		return fmt.Errorf("POD_NAMESPACE and POD_IP must be set from the downward API")
	}
	if err := mgr.Add(server.Runnable()); err != nil {
		return err
	}
	if err := mgr.Add(publisher.Runnable()); err != nil {
		return err
	}
	setupLog.Info("Identity server enabled on the leader", "address", cfg.ServerAddress, "service", cfg.ServiceName, "tls", !cfg.ServerInsecure)
	return nil
}

// engineIdentitySettings tells the engine controllers how engines reach the
// identity server, when this operator runs one.
func engineIdentitySettings(cfg *identity.Config) controller.IdentitySettings {
	if !cfg.Enabled || !cfg.ServerEnabled {
		return controller.IdentitySettings{}
	}
	port, _ := cfg.ServerPort() // validated in setupIdentityServer
	host := fmt.Sprintf("%s.%s.svc", cfg.ServiceName, os.Getenv("POD_NAMESPACE"))
	settings := controller.IdentitySettings{
		Enabled:    true,
		Address:    fmt.Sprintf("%s:%d", host, port),
		ServerName: host,
		Insecure:   cfg.ServerInsecure,
	}
	if cfg.ServerCertDir != "" {
		settings.CAFile = filepath.Join(cfg.ServerCertDir, "ca.crt")
	}
	return settings
}
