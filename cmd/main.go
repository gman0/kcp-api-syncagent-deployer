/*
Copyright 2025 The Platform Mesh Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"os"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	deployer "github.com/gman0/kcp-api-syncagent-deployer/internal/deployer"
)

func main() {
	log.SetLogger(zap.New(zap.UseDevMode(true)))

	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		leaderElectionID     string
		leaderElectionNS     string
		endpointSlice        string
		secretLabelSelector  string
	)
	pflag.StringVar(&metricsAddr, "metrics-bind-address", ":9080", "Address the metric endpoint binds to.")
	pflag.StringVar(&probeAddr, "health-probe-bind-address", ":8080", "Address the probe endpoint binds to.")
	pflag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election to ensure only one active controller instance.")
	pflag.StringVar(&leaderElectionID, "leader-election-id", "kcp-api-syncagent-deployer", "Leader election resource lock name.")
	pflag.StringVar(&leaderElectionNS, "leader-election-namespace", "", "Namespace for the leader election lease; defaults to the pod namespace (in-cluster) or current kubeconfig namespace.")
	pflag.StringVar(&endpointSlice, "apiexport-endpoint-slice", "", "Name of the APIExportEndpointSlice passed to every api-syncagent HelmRelease (required).")
	pflag.StringVar(&secretLabelSelector, "secret-label-selector", "", "Optional label selector (e.g. 'foo=bar,env=prod'); only Secrets matching it are reconciled. Empty means all Secrets.")
	pflag.Parse()

	if endpointSlice == "" {
		log.Log.Error(nil, "--apiexport-endpoint-slice is required")
		os.Exit(2)
	}

	entryLog := log.Log.WithName("entrypoint")
	ctx := signals.SetupSignalHandler()

	cfg := ctrl.GetConfigOrDie()

	mgr, err := manager.New(cfg, manager.Options{
		HealthProbeBindAddress:  probeAddr,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: leaderElectionNS,
	})
	if err != nil {
		entryLog.Error(err, "unable to set up manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		entryLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		entryLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	var secretSelector labels.Selector
	if secretLabelSelector != "" {
		s, err := labels.Parse(secretLabelSelector)
		if err != nil {
			entryLog.Error(err, "invalid --secret-label-selector")
			os.Exit(2)
		}
		secretSelector = s
	}

	if err := (&deployer.DeployerReconciler{
		APIExportEndpointSlice: endpointSlice,
		SecretSelector:         secretSelector,
	}).SetupWithManager(mgr); err != nil {
		entryLog.Error(err, "failed to setup deployer controller")
		os.Exit(1)
	}

	entryLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		entryLog.Error(err, "unable to run manager")
		os.Exit(1)
	}
}
