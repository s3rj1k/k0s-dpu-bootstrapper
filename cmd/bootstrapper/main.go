// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

// Command bootstrapper runs the k0s DPU bootstrapper controller.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/s3rj1k/nav/pkg/version"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"code.local/k0s-dpu-bootstrapper/internal/controller"
	"code.local/k0s-dpu-bootstrapper/internal/joinscript"
)

// Defaults for the token lifetime, which only spans minting to the agent running it.
const (
	defaultTokenTTL      = 4 * time.Hour
	defaultRefreshBefore = 1 * time.Hour
)

// Config holds the command line options of the controller.
type Config struct {
	zapOpts        zap.Options
	probeAddr      string
	namespace      string
	tokenTTL       time.Duration
	refreshBefore  time.Duration
	leaderElection bool
	showVersion    bool
}

// BindFlags registers every option on the default flag set.
func BindFlags() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.StringVar(&cfg.namespace, "namespace", "dpf-operator-system",
		"The one namespace this controller reads and writes. DPUs, DPUClusters, join script "+
			"templates and join Secrets all live here, and nothing outside it is watched.")
	flag.BoolVar(&cfg.leaderElection, "leader-elect", true, "Enable leader election, so only one replica mints tokens.")
	flag.DurationVar(&cfg.tokenTTL, "token-ttl", defaultTokenTTL,
		"Validity of a minted bootstrap token. It must cover BFB flashing and reboots, not the node's lifetime, "+
			"because once the kubelet holds a client certificate k0s rotates it without a token.")
	flag.DurationVar(&cfg.refreshBefore, "token-refresh-before", defaultRefreshBefore,
		"Mint again once a token has this much validity left.")
	flag.BoolVar(&cfg.showVersion, "version", false, "Print the version and exit.")

	// Registers the kubeconfig flag, which wins over KUBECONFIG and the in cluster config.
	clientconfig.RegisterFlags(flag.CommandLine)
	cfg.zapOpts.BindFlags(flag.CommandLine)

	return cfg
}

func Run(cfg *Config) error {
	if cfg.showVersion {
		fmt.Print(version.VersionInfo())
		return nil
	}
	if cfg.refreshBefore >= cfg.tokenTTL {
		return fmt.Errorf("token refresh window (%s) must be shorter than the token ttl (%s)", cfg.refreshBefore, cfg.tokenTTL)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&cfg.zapOpts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting k0s DPU bootstrapper", "version", version.VCS(version.AbbRevisionLen),
		"namespace", cfg.namespace, "tokenTTL", cfg.tokenTTL.String())

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	restConfig, err := clientconfig.GetConfig()
	if err != nil {
		return fmt.Errorf("loading kubernetes configuration: %w", err)
	}
	ctx := ctrl.SetupSignalHandler()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		// Disabled outright, since an empty bind address would serve metrics on 8080.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.leaderElection,
		LeaderElectionID:       "k0s-dpu-bootstrapper.k0s.mirantis.com",
		Cache: cache.Options{
			// Cross namespace is not supported, so every informer stops at this one and the
			// RBAC needed is a Role rather than a ClusterRole.
			DefaultNamespaces: map[string]cache.Config{cfg.namespace: {}},
			ByObject: map[client.Object]cache.ByObject{
				// Of the ConfigMaps in it, only join script templates are cached.
				&corev1.ConfigMap{}: {
					Label: labels.SelectorFromSet(labels.Set{
						joinscript.TemplateLabel: joinscript.TemplateLabelValue,
					}),
				},
			},
		},
		Client: client.Options{
			Cache: &client.CacheOptions{
				// DPU and DPUCluster are read through their informers.
				Unstructured: true,
				// Read live. Secrets are watched metadata only, and a structured cache
				// alongside that would be a second copy of the same objects.
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	if err := (&controller.DPUReconciler{
		Client:            mgr.GetClient(),
		Recorder:          mgr.GetEventRecorderFor("k0s-dpu-bootstrapper"),
		TemplateNamespace: cfg.namespace,
		TokenTTL:          cfg.tokenTTL,
		RefreshBefore:     cfg.refreshBefore,
	}).SetupWithManager(ctx, mgr); err != nil {
		return fmt.Errorf("setting up DPU controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding ready check: %w", err)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}

func main() {
	cfg := BindFlags()
	flag.Parse()

	if err := Run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
