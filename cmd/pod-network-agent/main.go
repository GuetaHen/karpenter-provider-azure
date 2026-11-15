/*
Portions Copyright (c) Microsoft Corporation.

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

// Command pod-network-agent programs the pod network of the node that it runs on and handles the
// node's startup taint, as docs/multi-region.md describes. The karpenter chart runs it as a
// DaemonSet and passes its settings as environment variables.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Azure/karpenter-provider-azure/pkg/podnetwork"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
)

const (
	// cniConfDir and cniBinDir are the CNI directories of the host, which the chart mounts at the same
	// paths.
	cniConfDir = "/etc/cni/net.d"
	cniBinDir  = "/opt/cni/bin"
	// probeTimeout bounds reading the header of a probe request and the shutdown of the probe server.
	probeTimeout = 5 * time.Second
)

func main() {
	log := zap.New()
	ctrllog.SetLogger(log)
	if err := run(signals.SetupSignalHandler(), log); err != nil {
		log.Error(err, "the agent stops")
		os.Exit(1)
	}
}

// run runs the agent until ctx ends or its cache or probe server fails. It serves the probes from the
// start, so that the liveness probe passes while the cache waits for the API server.
func run(ctx context.Context, log logr.Logger) error {
	settings, err := podnetwork.SettingsFromEnv(os.LookupEnv)
	if err != nil {
		return err
	}
	agent, objects, err := newAgent(&settings, log)
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	server := &http.Server{Addr: settings.ProbeAddress, Handler: agent.Probes(), ReadHeaderTimeout: probeTimeout}
	go func() {
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			stop(fmt.Errorf("serving the probes: %w", err))
		}
	}()
	defer shutdown(server)
	if err := agent.Watch(ctx, objects); err != nil {
		return err
	}
	go func() {
		if err := objects.Start(ctx); err != nil {
			stop(fmt.Errorf("running the cache: %w", err))
		}
	}()
	if objects.WaitForCacheSync(ctx) {
		agent.Run(ctx)
	}
	if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

// newAgent returns the agent of the node, with its clients of the API server, of instance metadata and
// of the node's network, and the cache that the agent watches.
func newAgent(settings *podnetwork.Settings, log logr.Logger) (*podnetwork.Agent, cache.Cache, error) {
	restConfig, err := config.GetConfig()
	if err != nil {
		return nil, nil, err
	}
	scheme, err := podnetwork.NewScheme()
	if err != nil {
		return nil, nil, err
	}
	objects, err := cache.New(restConfig, podnetwork.CacheOptions(scheme))
	if err != nil {
		return nil, nil, err
	}
	kube, err := client.New(restConfig, client.Options{Scheme: scheme, Mapper: podnetwork.NewRESTMapper()})
	if err != nil {
		return nil, nil, err
	}
	host, err := podnetwork.NewHost()
	if err != nil {
		return nil, nil, err
	}
	cluster := &podnetwork.KubeCluster{Cache: objects, Client: kube}
	cni := podnetwork.CNI{ConfDir: cniConfDir, BinDir: cniBinDir}
	return podnetwork.NewAgent(settings, cluster, podnetwork.NewMetadataClient(), host, cni, podnetwork.NewCiliumHealth(), log), objects, nil
}

// shutdown stops the probe server.
func shutdown(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	_ = server.Shutdown(ctx)
}
