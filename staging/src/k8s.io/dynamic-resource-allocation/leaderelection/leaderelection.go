/*
Copyright 2022 The Kubernetes Authors.

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

// Package leaderelection wraps k8s.io/client-go/tools/leaderelection with a
// simpler API. It's derived from https://github.com/kubernetes-csi/csi-lib-utils/tree/v0.11.0/leaderelection
package leaderelection

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

const (
	defaultLeaseDuration      = 15 * time.Second
	defaultRenewDeadline      = 10 * time.Second
	defaultRetryPeriod        = 5 * time.Second
	defaultHealthCheckTimeout = 20 * time.Second

	// HealthCheckerAddress is the address at which the leader election health
	// checker reports status.
	// The caller sidecar should document this address in appropriate flag
	// descriptions.
	HealthCheckerAddress = "/healthz/leader-election"
)

// leaderElection is a convenience wrapper around client-go's leader election library.
type leaderElection struct {
	runFunc func(ctx context.Context)

	// the lockName identifies the leader election config and should be shared across all members
	lockName string
	// the identity is the unique identity of the currently running member
	identity string
	// the namespace to store the lock resource
	namespace string
	// resourceLock defines the type of leaderelection that should be used
	// Only resourcelock.LeasesResourceLock is valid at the moment.
	resourceLock string
	// healthCheck reports unhealthy if leader election fails to renew leadership
	// within a timeout period.
	healthCheck *leaderelection.HealthzAdaptor

	leaseDuration      time.Duration
	renewDeadline      time.Duration
	retryPeriod        time.Duration
	healthCheckTimeout time.Duration

	ctx context.Context

	clientset kubernetes.Interface
}

// Option implements functional options for New.
type Option func(l *leaderElection)

// New constructs a new leader election instance.
func New(clientset kubernetes.Interface, lockName string, runFunc func(ctx context.Context), opts ...Option) *leaderElection {
	l := &leaderElection{
		runFunc:            runFunc,
		lockName:           lockName,
		resourceLock:       resourcelock.LeasesResourceLock,
		leaseDuration:      defaultLeaseDuration,
		renewDeadline:      defaultRenewDeadline,
		retryPeriod:        defaultRetryPeriod,
		healthCheckTimeout: defaultHealthCheckTimeout,
		clientset:          clientset,
	}

	for _, opt := range opts {
		opt(l)
	}

	return l
}

func Identity(identity string) Option {
	return func(l *leaderElection) {
		l.identity = identity
	}
}

func Namespace(namespace string) Option {
	return func(l *leaderElection) {
		l.namespace = namespace
	}
}

func LeaseDuration(leaseDuration time.Duration) Option {
	return func(l *leaderElection) {
		l.leaseDuration = leaseDuration
	}
}

func RenewDeadline(renewDeadline time.Duration) Option {
	return func(l *leaderElection) {
		l.renewDeadline = renewDeadline
	}
}

func RetryPeriod(retryPeriod time.Duration) Option {
	return func(l *leaderElection) {
		l.retryPeriod = retryPeriod
	}
}

func HealthCheckTimeout(timeout time.Duration) Option {
	return func(l *leaderElection) {
		l.healthCheckTimeout = timeout
	}
}

func Context(ctx context.Context) Option {
	return func(l *leaderElection) {
		l.ctx = ctx
	}
}

// Server represents any type that could serve HTTP requests for the leader
// election health check endpoint.
type Server interface {
	Handle(pattern string, handler http.Handler)
}

// PrepareHealthCheck creates a health check for this leader election object
// with the given healthCheckTimeout and registers its HTTP handler to the given
// server at the path specified by the constant "healthCheckerAddress".
// healthCheckTimeout determines the max duration beyond lease expiration
// allowed before reporting unhealthy.
// The caller sidecar should document the handler address in appropriate flag
// descriptions.
func (l *leaderElection) PrepareHealthCheck(s Server) {
	l.healthCheck = leaderelection.NewLeaderHealthzAdaptor(l.healthCheckTimeout)
	s.Handle(HealthCheckerAddress, adaptCheckToHandler(l.healthCheck.Check))
}

func (l *leaderElection) Run() error {
	ctx := l.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if l.identity == "" {
		id, err := defaultLeaderElectionIdentity()
		if err != nil {
			return fmt.Errorf("error getting the default leader identity: %v", err)
		}

		l.identity = id
	}

	if l.namespace == "" {
		l.namespace = inClusterNamespace()
	}

	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: l.clientset.CoreV1().Events(l.namespace)})
	eventRecorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: fmt.Sprintf("%s/%s", l.lockName, l.identity)})

	rlConfig := resourcelock.ResourceLockConfig{
		Identity:      sanitizeName(l.identity),
		EventRecorder: eventRecorder,
	}

	lock, err := resourcelock.NewFromKubeclient(l.resourceLock, l.namespace, sanitizeName(l.lockName), l.clientset, rlConfig)
	if err != nil {
		return err
	}

	leaderConfig := leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: l.leaseDuration,
		RenewDeadline: l.renewDeadline,
		RetryPeriod:   l.retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.FromContext(ctx).Info("became leader, starting")
				l.runFunc(ctx)
			},
			OnStoppedLeading: func() {
				klog.FromContext(ctx).Error(nil, "stopped leading")
				klog.FlushAndExit(klog.ExitFlushTimeout, 1)
			},
			OnNewLeader: func(identity string) {
				klog.FromContext(ctx).Info("new leader detected", "idendity", identity)
			},
		},
		WatchDog: l.healthCheck,
	}

	leaderelection.RunOrDie(ctx, leaderConfig)
	return nil // should never reach here
}

func defaultLeaderElectionIdentity() (string, error) {
	return os.Hostname()
}

// sanitizeName sanitizes the provided string so it can be consumed by leader election library
func sanitizeName(name string) string {
	re := regexp.MustCompile("[^a-zA-Z0-9-]")
	name = re.ReplaceAllString(name, "-")
	if name[len(name)-1] == '-' {
		// name must not end with '-'
		name = name + "X"
	}
	return name
}

// inClusterNamespace returns the namespace in which the pod is running in by checking
// the env var POD_NAMESPACE, then the file /var/run/secrets/kubernetes.io/serviceaccount/namespace.
// if neither returns a valid namespace, the "default" namespace is returned
func inClusterNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}

	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}

	return "default"
}

// adaptCheckToHandler returns an http.HandlerFunc that serves the provided checks.
func adaptCheckToHandler(c func(r *http.Request) error) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := c(r)
		if err != nil {
			http.Error(w, fmt.Sprintf("internal server error: %v", err), http.StatusInternalServerError)
		} else {
			fmt.Fprint(w, "ok")
		}
	})
}
