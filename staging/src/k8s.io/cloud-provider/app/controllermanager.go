/*
Copyright 2016 The Kubernetes Authors.

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

package app

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/spf13/cobra"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/informers"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	cloudprovider "k8s.io/cloud-provider"
	cloudcontrollerconfig "k8s.io/cloud-provider/app/config"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/configz"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics/features"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	"k8s.io/component-base/metrics/prometheus/slis"
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	genericcontrollermanager "k8s.io/controller-manager/app"
	"k8s.io/controller-manager/controller"
	"k8s.io/controller-manager/pkg/clientbuilder"
	cmfeatures "k8s.io/controller-manager/pkg/features"
	controllerhealthz "k8s.io/controller-manager/pkg/healthz"
	"k8s.io/controller-manager/pkg/informerfactory"
	"k8s.io/controller-manager/pkg/leadermigration"
	"k8s.io/klog/v2"
)

func init() {
	utilruntime.Must(features.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

const (
	// ControllerStartJitter is the jitter value used when starting controller managers.
	ControllerStartJitter = 1.0
	// ConfigzName is the name used for register cloud-controller manager /configz, same with GroupName.
	ConfigzName = "cloudcontrollermanager.config.k8s.io"
)

// NewCloudControllerManagerCommand creates a *cobra.Command object with default parameters
// controllerInitFuncConstructors is a map of controller name(as defined by controllers flag in https://kubernetes.io/docs/reference/command-line-tools-reference/kube-controller-manager/#options) to their InitFuncConstructor.
// additionalFlags provides controller specific flags to be included in the complete set of controller manager flags
func NewCloudControllerManagerCommand(logger klog.Logger, s *options.CloudControllerManagerOptions, cloudInitializer InitCloudFunc, controllerInitFuncConstructors map[string]ControllerInitFuncConstructor, controllerAliases map[string]string, additionalFlags cliflag.NamedFlagSets, stopCh <-chan struct{}) *cobra.Command {
	logOptions := logs.NewOptions()

	cmd := &cobra.Command{
		Use: "cloud-controller-manager",
		Long: `The Cloud controller manager is a daemon that embeds
the cloud specific control loops shipped with Kubernetes.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()
			cliflag.PrintFlags(cmd.Flags())

			if err := logsapi.ValidateAndApply(logOptions, utilfeature.DefaultFeatureGate); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				return err
			}

			c, err := s.Config(ControllerNames(controllerInitFuncConstructors), ControllersDisabledByDefault.List(), controllerAliases, AllWebhooks, DisabledByDefaultWebhooks)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				return err
			}

			completedConfig := c.Complete()
			cloud := cloudInitializer(logger, completedConfig)
			controllerInitializers := ConstructControllerInitializers(logger, controllerInitFuncConstructors, completedConfig, cloud)

			if err := Run(completedConfig, cloud, controllerInitializers, make(map[string]WebhookHandler), stopCh); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				return err
			}
			return nil
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	fs := cmd.Flags()
	namedFlagSets := s.Flags(ControllerNames(controllerInitFuncConstructors), ControllersDisabledByDefault.List(), controllerAliases, AllWebhooks, DisabledByDefaultWebhooks)

	globalFlagSet := namedFlagSets.FlagSet("global")
	verflag.AddFlags(globalFlagSet)
	logsapi.AddFlags(logOptions, globalFlagSet)
	globalflag.AddGlobalFlags(globalFlagSet, cmd.Name(), logs.SkipLoggingConfigurationFlags())

	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}
	for _, f := range additionalFlags.FlagSets {
		fs.AddFlagSet(f)
	}

	usageFmt := "Usage:\n  %s\n"
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), usageFmt, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStderr(), namedFlagSets, cols)
		cliflag.PrintSections(cmd.OutOrStderr(), additionalFlags, cols)
		return nil
	})
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStdout(), namedFlagSets, cols)
		cliflag.PrintSections(cmd.OutOrStdout(), additionalFlags, cols)
	})

	return cmd
}

// Run runs the ExternalCMServer.  This should never exit.
func Run(c *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface, controllerInitializers map[string]InitFunc, webhooks map[string]WebhookHandler,
	stopCh <-chan struct{}) error {
	// To help debugging, immediately log version
	klog.Infof("Version: %+v", version.Get())

	// Start events processing pipeline.
	c.EventBroadcaster.StartStructuredLogging(0)
	c.EventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: c.Client.CoreV1().Events("")})
	defer c.EventBroadcaster.Shutdown()

	// setup /configz endpoint
	if cz, err := configz.New(ConfigzName); err == nil {
		cz.Set(c.ComponentConfig)
	} else {
		klog.Errorf("unable to register configz: %v", err)
	}

	// Setup any health checks we will want to use.
	var checks []healthz.HealthChecker
	var electionChecker *leaderelection.HealthzAdaptor
	if c.ComponentConfig.Generic.LeaderElection.LeaderElect {
		electionChecker = leaderelection.NewLeaderHealthzAdaptor(time.Second * 20)
		checks = append(checks, electionChecker)
	}

	if utilfeature.DefaultFeatureGate.Enabled(cmfeatures.CloudControllerManagerWebhook) {
		if len(webhooks) > 0 {
			klog.Info("Webhook Handlers enabled: ", webhooks)
			handler := newHandler(webhooks)
			if _, _, err := c.WebhookSecureServing.Serve(handler, 0, stopCh); err != nil {
				return err
			}
		}
	}

	healthzHandler := controllerhealthz.NewMutableHealthzHandler(checks...)
	// Start the controller manager HTTP server
	if c.SecureServing != nil {
		unsecuredMux := genericcontrollermanager.NewBaseHandler(&c.ComponentConfig.Generic.Debugging, healthzHandler)

		slis.SLIMetricsWithReset{}.Install(unsecuredMux)

		handler := genericcontrollermanager.BuildHandlerChain(unsecuredMux, &c.Authorization, &c.Authentication)
		// TODO: handle stoppedCh and listenerStoppedCh returned by c.SecureServing.Serve
		if _, _, err := c.SecureServing.Serve(handler, 0, stopCh); err != nil {
			return err
		}
	}

	run := func(ctx context.Context, controllerInitializers map[string]InitFunc) {
		clientBuilder := clientbuilder.SimpleControllerClientBuilder{
			ClientConfig: c.Kubeconfig,
		}
		controllerContext, err := CreateControllerContext(c, clientBuilder, ctx.Done())
		if err != nil {
			klog.Fatalf("error building controller context: %v", err)
		}
		if err := startControllers(ctx, cloud, controllerContext, c, ctx.Done(), controllerInitializers, healthzHandler); err != nil {
			klog.Fatalf("error running controllers: %v", err)
		}
	}

	if !c.ComponentConfig.Generic.LeaderElection.LeaderElect {
		ctx := wait.ContextForChannel(stopCh)
		run(ctx, controllerInitializers)
		<-stopCh
		return nil
	}

	// Identity used to distinguish between multiple cloud controller manager instances
	id, err := os.Hostname()
	if err != nil {
		return err
	}
	// add a uniquifier so that two processes on the same host don't accidentally both become active
	id = id + "_" + string(uuid.NewUUID())

	// leaderMigrator will be non-nil if and only if Leader Migration is enabled.
	var leaderMigrator *leadermigration.LeaderMigrator = nil

	// If leader migration is enabled, use the redirected initialization
	// Check feature gate and configuration separately so that any error in configuration checking will not
	//  affect the result if the feature is not enabled.
	if leadermigration.Enabled(&c.ComponentConfig.Generic) {
		klog.Info("starting leader migration")

		leaderMigrator = leadermigration.NewLeaderMigrator(&c.ComponentConfig.Generic.LeaderMigration,
			"cloud-controller-manager")
	}

	// Start the main lock
	go leaderElectAndRun(c, id, electionChecker,
		c.ComponentConfig.Generic.LeaderElection.ResourceLock,
		c.ComponentConfig.Generic.LeaderElection.ResourceName,
		leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				initializers := controllerInitializers
				if leaderMigrator != nil {
					// If leader migration is enabled, we should start only non-migrated controllers
					//  for the main lock.
					initializers = filterInitializers(controllerInitializers, leaderMigrator.FilterFunc, leadermigration.ControllerNonMigrated)
					klog.Info("leader migration: starting main controllers.")
					// Signal the main lock is acquired, and thus migration lock is ready to attempt.
					close(leaderMigrator.MigrationReady)
				}
				run(ctx, initializers)
			},
			OnStoppedLeading: func() {
				klog.ErrorS(nil, "leaderelection lost")
				klog.FlushAndExit(klog.ExitFlushTimeout, 1)
			},
		})

	// If Leader Migration is enabled, proceed to attempt the migration lock.
	if leaderMigrator != nil {
		// Wait for the signal of main lock being acquired.
		<-leaderMigrator.MigrationReady

		// Start the migration lock.
		go leaderElectAndRun(c, id, electionChecker,
			c.ComponentConfig.Generic.LeaderMigration.ResourceLock,
			c.ComponentConfig.Generic.LeaderMigration.LeaderName,
			leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					klog.Info("leader migration: starting migrated controllers.")
					run(ctx, filterInitializers(controllerInitializers, leaderMigrator.FilterFunc, leadermigration.ControllerMigrated))
				},
				OnStoppedLeading: func() {
					klog.ErrorS(nil, "migration leaderelection lost")
					klog.FlushAndExit(klog.ExitFlushTimeout, 1)
				},
			})
	}

	<-stopCh
	return nil
}

// startControllers starts the cloud specific controller loops.
func startControllers(ctx context.Context, cloud cloudprovider.Interface, controllerContext genericcontrollermanager.ControllerContext, c *cloudcontrollerconfig.CompletedConfig, stopCh <-chan struct{}, controllers map[string]InitFunc, healthzHandler *controllerhealthz.MutableHealthzHandler) error {
	// Initialize the cloud provider with a reference to the clientBuilder
	cloud.Initialize(c.ClientBuilder, stopCh)
	// Set the informer on the user cloud object
	if informerUserCloud, ok := cloud.(cloudprovider.InformerUser); ok {
		informerUserCloud.SetInformers(c.SharedInformers)
	}
	var controllerChecks []healthz.HealthChecker
	for controllerName, initFn := range controllers {
		if !genericcontrollermanager.IsControllerEnabled(controllerName, ControllersDisabledByDefault, c.ComponentConfig.Generic.Controllers) {
			klog.Warningf("%q is disabled", controllerName)
			continue
		}

		klog.V(1).Infof("Starting %q", controllerName)
		ctrl, started, err := initFn(ctx, controllerContext)
		if err != nil {
			klog.Errorf("Error starting %q", controllerName)
			return err
		}
		if !started {
			klog.Warningf("Skipping %q", controllerName)
			continue
		}
		check := controllerhealthz.NamedPingChecker(controllerName)
		if ctrl != nil {
			if healthCheckable, ok := ctrl.(controller.HealthCheckable); ok {
				if realCheck := healthCheckable.HealthChecker(); realCheck != nil {
					check = controllerhealthz.NamedHealthChecker(controllerName, realCheck)
				}
			}
		}
		controllerChecks = append(controllerChecks, check)
		klog.Infof("Started %q", controllerName)

		time.Sleep(wait.Jitter(c.ComponentConfig.Generic.ControllerStartInterval.Duration, ControllerStartJitter))
	}

	healthzHandler.AddHealthChecker(controllerChecks...)

	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start apiserver and controller manager at the same time.
	if err := genericcontrollermanager.WaitForAPIServer(c.VersionedClient, 10*time.Second); err != nil {
		klog.Fatalf("Failed to wait for apiserver being healthy: %v", err)
	}

	c.SharedInformers.Start(stopCh)
	controllerContext.InformerFactory.Start(controllerContext.Stop)

	<-stopCh
	return nil
}

// InitCloudFunc is used to initialize cloud
type InitCloudFunc func(logger klog.Logger, config *cloudcontrollerconfig.CompletedConfig) cloudprovider.Interface

// InitFunc is used to launch a particular controller. It returns a controller
// that can optionally implement other interfaces so that the controller manager
// can support the requested features.
// The returned controller may be nil, which will be considered an anonymous controller
// that requests no additional features from the controller manager.
// Any error returned will cause the controller process to `Fatal`
// The bool indicates whether the controller was enabled.
type InitFunc func(ctx context.Context, controllerContext genericcontrollermanager.ControllerContext) (controller controller.Interface, enabled bool, err error)

// InitFuncConstructor is used to construct InitFunc
type InitFuncConstructor func(logger klog.Logger, initcontext ControllerInitContext, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface) InitFunc

// ControllerNames indicate the default controller we are known.
func ControllerNames(controllerInitFuncConstructors map[string]ControllerInitFuncConstructor) []string {
	ret := sets.StringKeySet(controllerInitFuncConstructors)
	return ret.List()
}

var (
	// ControllersDisabledByDefault is the controller disabled default when starting cloud-controller managers.
	ControllersDisabledByDefault = sets.NewString()

	// AllWebhooks represents the list of all webhook options configured in
	// this package.  This is empty because no webhooks are currently
	// configured in this package.
	AllWebhooks = []string{}

	// DisabledByDefaultWebhooks represents the list of webhooks which must be
	// explicitly enabled. This is empty because no webhooks are currently
	// configured in this package.
	DisabledByDefaultWebhooks = []string{}
)

// ConstructControllerInitializers is a map of controller name(as defined by controllers flag in https://kubernetes.io/docs/reference/command-line-tools-reference/kube-controller-manager/#options) to their InitFuncConstructor.
// paired to their InitFunc.  This allows for structured downstream composition and subdivision.
func ConstructControllerInitializers(logger klog.Logger, controllerInitFuncConstructors map[string]ControllerInitFuncConstructor, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface) map[string]InitFunc {
	controllers := map[string]InitFunc{}
	for name, constructor := range controllerInitFuncConstructors {
		controllers[name] = constructor.Constructor(logger, constructor.InitContext, completedConfig, cloud)
	}
	return controllers
}

type ControllerInitFuncConstructor struct {
	InitContext ControllerInitContext
	Constructor InitFuncConstructor
}

type ControllerInitContext struct {
	ClientName string
}

// StartCloudNodeControllerWrapper is used to take cloud config as input and start cloud node controller
func StartCloudNodeControllerWrapper(logger klog.Logger, initContext ControllerInitContext, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface) InitFunc {
	return func(ctx context.Context, controllerContext genericcontrollermanager.ControllerContext) (controller.Interface, bool, error) {
		return startCloudNodeController(ctx, initContext, controllerContext, completedConfig, cloud)
	}
}

// StartCloudNodeLifecycleControllerWrapper is used to take cloud config as input and start cloud node lifecycle controller
func StartCloudNodeLifecycleControllerWrapper(logger klog.Logger, initContext ControllerInitContext, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface) InitFunc {
	return func(ctx context.Context, controllerContext genericcontrollermanager.ControllerContext) (controller.Interface, bool, error) {
		return startCloudNodeLifecycleController(ctx, initContext, controllerContext, completedConfig, cloud)
	}
}

// StartServiceControllerWrapper is used to take cloud config as input and start service controller
func StartServiceControllerWrapper(logger klog.Logger, initContext ControllerInitContext, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface) InitFunc {
	return func(ctx context.Context, controllerContext genericcontrollermanager.ControllerContext) (controller.Interface, bool, error) {
		return startServiceController(ctx, initContext, controllerContext, completedConfig, cloud)
	}
}

// StartRouteControllerWrapper is used to take cloud config as input and start route controller
func StartRouteControllerWrapper(logger klog.Logger, initContext ControllerInitContext, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface) InitFunc {
	return func(ctx context.Context, controllerContext genericcontrollermanager.ControllerContext) (controller.Interface, bool, error) {
		return startRouteController(ctx, initContext, controllerContext, completedConfig, cloud)
	}
}

// DefaultInitFuncConstructors is a map of default named controller groups paired with InitFuncConstructor
var DefaultInitFuncConstructors = map[string]ControllerInitFuncConstructor{
	// The cloud-node controller shares the "node-controller" identity with the cloud-node-lifecycle
	// controller for historical reasons.  See
	// https://github.com/kubernetes/kubernetes/pull/72764#issuecomment-453300990 for more context.
	names.CloudNodeController: {
		InitContext: ControllerInitContext{
			ClientName: "node-controller",
		},
		Constructor: StartCloudNodeControllerWrapper,
	},
	names.CloudNodeLifecycleController: {
		InitContext: ControllerInitContext{
			ClientName: "node-controller",
		},
		Constructor: StartCloudNodeLifecycleControllerWrapper,
	},
	names.ServiceLBController: {
		InitContext: ControllerInitContext{
			ClientName: "service-controller",
		},
		Constructor: StartServiceControllerWrapper,
	},
	names.NodeRouteController: {
		InitContext: ControllerInitContext{
			ClientName: "route-controller",
		},
		Constructor: StartRouteControllerWrapper,
	},
}

// CreateControllerContext creates a context struct containing references to resources needed by the
// controllers such as the cloud provider and clientBuilder. rootClientBuilder is only used for
// the shared-informers client and token controller.
func CreateControllerContext(s *cloudcontrollerconfig.CompletedConfig, clientBuilder clientbuilder.ControllerClientBuilder, stop <-chan struct{}) (genericcontrollermanager.ControllerContext, error) {
	versionedClient := clientBuilder.ClientOrDie("shared-informers")
	sharedInformers := informers.NewSharedInformerFactory(versionedClient, ResyncPeriod(s)())

	metadataClient := metadata.NewForConfigOrDie(clientBuilder.ConfigOrDie("metadata-informers"))
	metadataInformers := metadatainformer.NewSharedInformerFactory(metadataClient, ResyncPeriod(s)())

	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start apiserver and controller manager at the same time.
	if err := genericcontrollermanager.WaitForAPIServer(versionedClient, 10*time.Second); err != nil {
		return genericcontrollermanager.ControllerContext{}, fmt.Errorf("failed to wait for apiserver being healthy: %v", err)
	}

	// Use a discovery client capable of being refreshed.
	discoveryClient := clientBuilder.ClientOrDie("controller-discovery")
	cachedClient := cacheddiscovery.NewMemCacheClient(discoveryClient.Discovery())
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)
	go wait.Until(func() {
		restMapper.Reset()
	}, 30*time.Second, stop)

	ctx := genericcontrollermanager.ControllerContext{
		ClientBuilder:                   clientBuilder,
		InformerFactory:                 sharedInformers,
		ObjectOrMetadataInformerFactory: informerfactory.NewInformerFactory(sharedInformers, metadataInformers),
		RESTMapper:                      restMapper,
		Stop:                            stop,
		InformersStarted:                make(chan struct{}),
		ResyncPeriod:                    ResyncPeriod(s),
		ControllerManagerMetrics:        controllersmetrics.NewControllerManagerMetrics("cloud-controller-manager"),
	}
	controllersmetrics.Register()
	return ctx, nil
}

// ResyncPeriod returns a function which generates a duration each time it is
// invoked; this is so that multiple controllers don't get into lock-step and all
// hammer the apiserver with list requests simultaneously.
func ResyncPeriod(c *cloudcontrollerconfig.CompletedConfig) func() time.Duration {
	return func() time.Duration {
		factor := rand.Float64() + 1
		return time.Duration(float64(c.ComponentConfig.Generic.MinResyncPeriod.Nanoseconds()) * factor)
	}
}

// leaderElectAndRun runs the leader election, and runs the callbacks once the leader lease is acquired.
// TODO: extract this function into staging/controller-manager
func leaderElectAndRun(c *cloudcontrollerconfig.CompletedConfig, lockIdentity string, electionChecker *leaderelection.HealthzAdaptor, resourceLock string, leaseName string, callbacks leaderelection.LeaderCallbacks) {
	rl, err := resourcelock.NewFromKubeconfig(resourceLock,
		c.ComponentConfig.Generic.LeaderElection.ResourceNamespace,
		leaseName,
		resourcelock.ResourceLockConfig{
			Identity:      lockIdentity,
			EventRecorder: c.EventRecorder,
		},
		c.Kubeconfig,
		c.ComponentConfig.Generic.LeaderElection.RenewDeadline.Duration)
	if err != nil {
		klog.Fatalf("error creating lock: %v", err)
	}

	leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: c.ComponentConfig.Generic.LeaderElection.LeaseDuration.Duration,
		RenewDeadline: c.ComponentConfig.Generic.LeaderElection.RenewDeadline.Duration,
		RetryPeriod:   c.ComponentConfig.Generic.LeaderElection.RetryPeriod.Duration,
		Callbacks:     callbacks,
		WatchDog:      electionChecker,
		Name:          leaseName,
	})

	panic("unreachable")
}

// filterInitializers returns initializers that has filterFunc(name) == expected.
// filterFunc can be nil, in which case the original initializers will be returned directly.
// InitFunc is local to cloud-controller-manager, and thus filterInitializers has to be local too.
func filterInitializers(allInitializers map[string]InitFunc, filterFunc leadermigration.FilterFunc, expected leadermigration.FilterResult) map[string]InitFunc {
	if filterFunc == nil {
		return allInitializers
	}
	initializers := make(map[string]InitFunc)
	for name, initFunc := range allInitializers {
		if filterFunc(name) == expected {
			initializers[name] = initFunc
		}
	}
	return initializers
}
