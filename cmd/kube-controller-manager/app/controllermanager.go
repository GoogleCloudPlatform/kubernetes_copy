/*
Copyright 2014 The Kubernetes Authors.

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

// Package app implements a server that runs a set of active
// components.  This includes replication controllers, service endpoints and
// nodes.
package app

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/blang/semver/v4"
	"github.com/spf13/cobra"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/mux"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/informers"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/configz"
	"k8s.io/component-base/featuregate"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	metricsfeatures "k8s.io/component-base/metrics/features"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	"k8s.io/component-base/metrics/prometheus/slis"
	"k8s.io/component-base/term"
	utilversion "k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	zpagesfeatures "k8s.io/component-base/zpages/features"
	"k8s.io/component-base/zpages/statusz"
	genericcontrollermanager "k8s.io/controller-manager/app"
	"k8s.io/controller-manager/controller"
	"k8s.io/controller-manager/pkg/clientbuilder"
	controllerhealthz "k8s.io/controller-manager/pkg/healthz"
	"k8s.io/controller-manager/pkg/informerfactory"
	"k8s.io/controller-manager/pkg/leadermigration"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kube-controller-manager/app/config"
	"k8s.io/kubernetes/cmd/kube-controller-manager/app/options"
	"k8s.io/kubernetes/cmd/kube-controller-manager/names"
	kubectrlmgrconfig "k8s.io/kubernetes/pkg/controller/apis/config"
	garbagecollector "k8s.io/kubernetes/pkg/controller/garbagecollector"
	serviceaccountcontroller "k8s.io/kubernetes/pkg/controller/serviceaccount"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
	utilruntime.Must(metricsfeatures.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

const (
	// ControllerStartJitter is the Jitter used when starting controller managers
	ControllerStartJitter = 1.0
	// ConfigzName is the name used for register kube-controller manager /configz, same with GroupName.
	ConfigzName = "kubecontrollermanager.config.k8s.io"
	// kubeControllerManager defines variable used internally when referring to cloud-controller-manager component
	kubeControllerManager = "kube-controller-manager"
)

// NewControllerManagerCommand creates a *cobra.Command object with default parameters
func NewControllerManagerCommand() *cobra.Command {
	_, _ = featuregate.DefaultComponentGlobalsRegistry.ComponentGlobalsOrRegister(
		featuregate.DefaultKubeComponent, utilversion.DefaultBuildEffectiveVersion(), utilfeature.DefaultMutableFeatureGate)

	s, err := options.NewKubeControllerManagerOptions()
	if err != nil {
		klog.Background().Error(err, "Unable to initialize command options")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	cmd := &cobra.Command{
		Use: kubeControllerManager,
		Long: `The Kubernetes controller manager is a daemon that embeds
the core control loops shipped with Kubernetes. In applications of robotics and
automation, a control loop is a non-terminating loop that regulates the state of
the system. In Kubernetes, a controller is a control loop that watches the shared
state of the cluster through the apiserver and makes changes attempting to move the
current state towards the desired state. Examples of controllers that ship with
Kubernetes today are the replication controller, endpoints controller, namespace
controller, and serviceaccounts controller.`,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			// silence client-go warnings.
			// kube-controller-manager generically watches APIs (including deprecated ones),
			// and CI ensures it works properly against matching kube-apiserver versions.
			restclient.SetDefaultWarningHandler(restclient.NoWarnings{})
			// makes sure feature gates are set before RunE.
			return s.ComponentGlobalsRegistry.Set()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()

			// Activate logging as soon as possible, after that
			// show flags with the final logging configuration.
			if err := logsapi.ValidateAndApply(s.Logs, utilfeature.DefaultFeatureGate); err != nil {
				return err
			}
			cliflag.PrintFlags(cmd.Flags())

			c, err := s.Config(KnownControllers(), ControllersDisabledByDefault(), ControllerAliases())
			if err != nil {
				return err
			}

			// add feature enablement metrics
			fg := s.ComponentGlobalsRegistry.FeatureGateFor(featuregate.DefaultKubeComponent)
			fg.(featuregate.MutableFeatureGate).AddMetrics()
			return Run(context.Background(), c.Complete())
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
	namedFlagSets := s.Flags(KnownControllers(), ControllersDisabledByDefault(), ControllerAliases())
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name(), logs.SkipLoggingConfigurationFlags())
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, namedFlagSets, cols)

	return cmd
}

// ResyncPeriod returns a function which generates a duration each time it is
// invoked; this is so that multiple controllers don't get into lock-step and all
// hammer the apiserver with list requests simultaneously.
func ResyncPeriod(c *config.CompletedConfig) func() time.Duration {
	return func() time.Duration {
		factor := rand.Float64() + 1
		return time.Duration(float64(c.ComponentConfig.Generic.MinResyncPeriod.Nanoseconds()) * factor)
	}
}

// Run runs the KubeControllerManagerOptions.
func Run(ctx context.Context, c *config.CompletedConfig) error {
	logger := klog.FromContext(ctx)
	stopCh := ctx.Done()

	// To help debugging, immediately log version
	logger.Info("Starting", "version", utilversion.Get())

	logger.Info("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS", os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	// Start events processing pipeline.
	c.EventBroadcaster.StartStructuredLogging(0)
	c.EventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: c.Client.CoreV1().Events("")})
	defer c.EventBroadcaster.Shutdown()

	if cfgz, err := configz.New(ConfigzName); err == nil {
		cfgz.Set(c.ComponentConfig)
	} else {
		logger.Error(err, "Unable to register configz")
	}

	// Setup any healthz checks we will want to use.
	var checks []healthz.HealthChecker
	var electionChecker *leaderelection.HealthzAdaptor
	if c.ComponentConfig.Generic.LeaderElection.LeaderElect {
		electionChecker = leaderelection.NewLeaderHealthzAdaptor(time.Second * 20)
		checks = append(checks, electionChecker)
	}
	healthzHandler := controllerhealthz.NewMutableHealthzHandler(checks...)

	// Start the controller manager HTTP server
	// unsecuredMux is the handler for these controller *after* authn/authz filters have been applied
	var unsecuredMux *mux.PathRecorderMux
	if c.SecureServing != nil {
		unsecuredMux = genericcontrollermanager.NewBaseHandler(&c.ComponentConfig.Generic.Debugging, healthzHandler)
		slis.SLIMetricsWithReset{}.Install(unsecuredMux)

		if utilfeature.DefaultFeatureGate.Enabled(zpagesfeatures.ComponentStatusz) {
			statusz.Install(unsecuredMux, kubeControllerManager, statusz.NewRegistry())
		}

		handler := genericcontrollermanager.BuildHandlerChain(unsecuredMux, &c.Authorization, &c.Authentication)
		// TODO: handle stoppedCh and listenerStoppedCh returned by c.SecureServing.Serve
		if _, _, err := c.SecureServing.Serve(handler, 0, stopCh); err != nil {
			return err
		}
	}

	clientBuilder, rootClientBuilder := createClientBuilders(c)

	saTokenControllerDescriptor := newServiceAccountTokenControllerDescriptor(rootClientBuilder)

	run := func(ctx context.Context, controllerDescriptors map[string]*ControllerDescriptor) {
		controllerContext, err := CreateControllerContext(ctx, c, rootClientBuilder, clientBuilder)
		if err != nil {
			logger.Error(err, "Error building controller context")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}

		if err := StartControllers(ctx, controllerContext, controllerDescriptors, unsecuredMux, healthzHandler); err != nil {
			logger.Error(err, "Error starting controllers")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}

		controllerContext.InformerFactory.Start(stopCh)
		controllerContext.ObjectOrMetadataInformerFactory.Start(stopCh)
		close(controllerContext.InformersStarted)

		<-ctx.Done()
	}

	// No leader election, run directly
	if !c.ComponentConfig.Generic.LeaderElection.LeaderElect {
		controllerDescriptors := NewControllerDescriptors()
		controllerDescriptors[names.ServiceAccountTokenController] = saTokenControllerDescriptor
		run(ctx, controllerDescriptors)
		return nil
	}

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// add a uniquifier so that two processes on the same host don't accidentally both become active
	id = id + "_" + string(uuid.NewUUID())

	// leaderMigrator will be non-nil if and only if Leader Migration is enabled.
	var leaderMigrator *leadermigration.LeaderMigrator = nil

	// If leader migration is enabled, create the LeaderMigrator and prepare for migration
	if leadermigration.Enabled(&c.ComponentConfig.Generic) {
		logger.Info("starting leader migration")

		leaderMigrator = leadermigration.NewLeaderMigrator(&c.ComponentConfig.Generic.LeaderMigration,
			kubeControllerManager)

		// startSATokenControllerInit is the original InitFunc.
		startSATokenControllerInit := saTokenControllerDescriptor.GetInitFunc()

		// Wrap saTokenControllerDescriptor to signal readiness for migration after starting
		//  the controller.
		saTokenControllerDescriptor.initFunc = func(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
			defer close(leaderMigrator.MigrationReady)
			return startSATokenControllerInit(ctx, controllerContext, controllerName)
		}
	}

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CoordinatedLeaderElection) {
		binaryVersion, err := semver.ParseTolerant(featuregate.DefaultComponentGlobalsRegistry.EffectiveVersionFor(featuregate.DefaultKubeComponent).BinaryVersion().String())
		if err != nil {
			return err
		}
		emulationVersion, err := semver.ParseTolerant(featuregate.DefaultComponentGlobalsRegistry.EffectiveVersionFor(featuregate.DefaultKubeComponent).EmulationVersion().String())
		if err != nil {
			return err
		}

		// Start lease candidate controller for coordinated leader election
		leaseCandidate, waitForSync, err := leaderelection.NewCandidate(
			c.Client,
			"kube-system",
			id,
			kubeControllerManager,
			binaryVersion.FinalizeVersion(),
			emulationVersion.FinalizeVersion(),
			coordinationv1.OldestEmulationVersion,
		)
		if err != nil {
			return err
		}
		healthzHandler.AddHealthChecker(healthz.NewInformerSyncHealthz(waitForSync))

		go leaseCandidate.Run(ctx)
	}

	// Start the main lock
	go leaderElectAndRun(ctx, c, id, electionChecker,
		c.ComponentConfig.Generic.LeaderElection.ResourceLock,
		c.ComponentConfig.Generic.LeaderElection.ResourceName,
		leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				controllerDescriptors := NewControllerDescriptors()
				if leaderMigrator != nil {
					// If leader migration is enabled, we should start only non-migrated controllers
					//  for the main lock.
					controllerDescriptors = filteredControllerDescriptors(controllerDescriptors, leaderMigrator.FilterFunc, leadermigration.ControllerNonMigrated)
					logger.Info("leader migration: starting main controllers.")
				}
				controllerDescriptors[names.ServiceAccountTokenController] = saTokenControllerDescriptor
				run(ctx, controllerDescriptors)
			},
			OnStoppedLeading: func() {
				logger.Error(nil, "leaderelection lost")
				klog.FlushAndExit(klog.ExitFlushTimeout, 1)
			},
		})

	// If Leader Migration is enabled, proceed to attempt the migration lock.
	if leaderMigrator != nil {
		// Wait for Service Account Token Controller to start before acquiring the migration lock.
		// At this point, the main lock must have already been acquired, or the KCM process already exited.
		// We wait for the main lock before acquiring the migration lock to prevent the situation
		//  where KCM instance A holds the main lock while KCM instance B holds the migration lock.
		<-leaderMigrator.MigrationReady

		// Start the migration lock.
		go leaderElectAndRun(ctx, c, id, electionChecker,
			c.ComponentConfig.Generic.LeaderMigration.ResourceLock,
			c.ComponentConfig.Generic.LeaderMigration.LeaderName,
			leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					logger.Info("leader migration: starting migrated controllers.")
					controllerDescriptors := NewControllerDescriptors()
					controllerDescriptors = filteredControllerDescriptors(controllerDescriptors, leaderMigrator.FilterFunc, leadermigration.ControllerMigrated)
					// DO NOT start saTokenController under migration lock
					delete(controllerDescriptors, names.ServiceAccountTokenController)
					run(ctx, controllerDescriptors)
				},
				OnStoppedLeading: func() {
					logger.Error(nil, "migration leaderelection lost")
					klog.FlushAndExit(klog.ExitFlushTimeout, 1)
				},
			})
	}

	<-stopCh
	return nil
}

// ControllerContext defines the context object for controller
type ControllerContext struct {
	// ClientBuilder will provide a client for this controller to use
	ClientBuilder clientbuilder.ControllerClientBuilder

	// InformerFactory gives access to informers for the controller.
	InformerFactory informers.SharedInformerFactory

	// ObjectOrMetadataInformerFactory gives access to informers for typed resources
	// and dynamic resources by their metadata. All generic controllers currently use
	// object metadata - if a future controller needs access to the full object this
	// would become GenericInformerFactory and take a dynamic client.
	ObjectOrMetadataInformerFactory informerfactory.InformerFactory

	// ComponentConfig provides access to init options for a given controller
	ComponentConfig kubectrlmgrconfig.KubeControllerManagerConfiguration

	// DeferredDiscoveryRESTMapper is a RESTMapper that will defer
	// initialization of the RESTMapper until the first mapping is
	// requested.
	RESTMapper *restmapper.DeferredDiscoveryRESTMapper

	// InformersStarted is closed after all of the controllers have been initialized and are running.  After this point it is safe,
	// for an individual controller to start the shared informers. Before it is closed, they should not.
	InformersStarted chan struct{}

	// ResyncPeriod generates a duration each time it is invoked; this is so that
	// multiple controllers don't get into lock-step and all hammer the apiserver
	// with list requests simultaneously.
	ResyncPeriod func() time.Duration

	// ControllerManagerMetrics provides a proxy to set controller manager specific metrics.
	ControllerManagerMetrics *controllersmetrics.ControllerManagerMetrics

	// GraphBuilder gives an access to dependencyGraphBuilder which keeps tracks of resources in the cluster
	GraphBuilder *garbagecollector.GraphBuilder
}

// IsControllerEnabled checks if the context's controllers enabled or not
func (c ControllerContext) IsControllerEnabled(controllerDescriptor *ControllerDescriptor) bool {
	controllersDisabledByDefault := sets.NewString()
	if controllerDescriptor.IsDisabledByDefault() {
		controllersDisabledByDefault.Insert(controllerDescriptor.Name())
	}
	return genericcontrollermanager.IsControllerEnabled(controllerDescriptor.Name(), controllersDisabledByDefault, c.ComponentConfig.Generic.Controllers)
}

// InitFunc is used to launch a particular controller. It returns a controller
// that can optionally implement other interfaces so that the controller manager
// can support the requested features.
// The returned controller may be nil, which will be considered an anonymous controller
// that requests no additional features from the controller manager.
// Any error returned will cause the controller process to `Fatal`
// The bool indicates whether the controller was enabled.
type InitFunc func(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller controller.Interface, enabled bool, err error)

type ControllerDescriptor struct {
	name                      string
	initFunc                  InitFunc
	requiredFeatureGates      []featuregate.Feature
	aliases                   []string
	isDisabledByDefault       bool
	isCloudProviderController bool
	requiresSpecialHandling   bool
}

func (r *ControllerDescriptor) Name() string {
	return r.name
}

func (r *ControllerDescriptor) GetInitFunc() InitFunc {
	return r.initFunc
}

func (r *ControllerDescriptor) GetRequiredFeatureGates() []featuregate.Feature {
	return append([]featuregate.Feature(nil), r.requiredFeatureGates...)
}

// GetAliases returns aliases to ensure backwards compatibility and should never be removed!
// Only addition of new aliases is allowed, and only when a canonical name is changed (please see CHANGE POLICY of controller names)
func (r *ControllerDescriptor) GetAliases() []string {
	return append([]string(nil), r.aliases...)
}

func (r *ControllerDescriptor) IsDisabledByDefault() bool {
	return r.isDisabledByDefault
}

func (r *ControllerDescriptor) IsCloudProviderController() bool {
	return r.isCloudProviderController
}

// RequiresSpecialHandling should return true only in a special non-generic controllers like ServiceAccountTokenController
func (r *ControllerDescriptor) RequiresSpecialHandling() bool {
	return r.requiresSpecialHandling
}

// KnownControllers returns all known controllers's name
func KnownControllers() []string {
	return sets.StringKeySet(NewControllerDescriptors()).List()
}

// ControllerAliases returns a mapping of aliases to canonical controller names
func ControllerAliases() map[string]string {
	aliases := map[string]string{}
	for name, c := range NewControllerDescriptors() {
		for _, alias := range c.GetAliases() {
			aliases[alias] = name
		}
	}
	return aliases
}

func ControllersDisabledByDefault() []string {
	var controllersDisabledByDefault []string

	for name, c := range NewControllerDescriptors() {
		if c.IsDisabledByDefault() {
			controllersDisabledByDefault = append(controllersDisabledByDefault, name)
		}
	}

	sort.Strings(controllersDisabledByDefault)

	return controllersDisabledByDefault
}

// NewControllerDescriptors is a public map of named controller groups (you can start more than one in an init func)
// paired to their ControllerDescriptor wrapper object that includes InitFunc.
// This allows for structured downstream composition and subdivision.
func NewControllerDescriptors() map[string]*ControllerDescriptor {
	controllers := map[string]*ControllerDescriptor{}
	aliases := sets.NewString()

	// All the controllers must fulfil common constraints, or else we will explode.
	register := func(controllerDesc *ControllerDescriptor) {
		if controllerDesc == nil {
			panic("received nil controller for a registration")
		}
		name := controllerDesc.Name()
		if len(name) == 0 {
			panic("received controller without a name for a registration")
		}
		if _, found := controllers[name]; found {
			panic(fmt.Sprintf("controller name %q was registered twice", name))
		}
		if controllerDesc.GetInitFunc() == nil {
			panic(fmt.Sprintf("controller %q does not have an init function", name))
		}

		for _, alias := range controllerDesc.GetAliases() {
			if aliases.Has(alias) {
				panic(fmt.Sprintf("controller %q has a duplicate alias %q", name, alias))
			}
			aliases.Insert(alias)
		}

		controllers[name] = controllerDesc
	}

	// First add "special" controllers that aren't initialized normally. These controllers cannot be initialized
	// in the main controller loop initialization, so we add them here only for the metadata and duplication detection.
	// app.ControllerDescriptor#RequiresSpecialHandling should return true for such controllers
	// The only known special case is the ServiceAccountTokenController which *must* be started
	// first to ensure that the SA tokens for future controllers will exist. Think very carefully before adding new
	// special controllers.
	register(newServiceAccountTokenControllerDescriptor(nil))

	register(newEndpointsControllerDescriptor())
	register(newEndpointSliceControllerDescriptor())
	register(newEndpointSliceMirroringControllerDescriptor())
	register(newReplicationControllerDescriptor())
	register(newPodGarbageCollectorControllerDescriptor())
	register(newResourceQuotaControllerDescriptor())
	register(newNamespaceControllerDescriptor())
	register(newServiceAccountControllerDescriptor())
	register(newGarbageCollectorControllerDescriptor())
	register(newDaemonSetControllerDescriptor())
	register(newJobControllerDescriptor())
	register(newDeploymentControllerDescriptor())
	register(newReplicaSetControllerDescriptor())
	register(newHorizontalPodAutoscalerControllerDescriptor())
	register(newDisruptionControllerDescriptor())
	register(newStatefulSetControllerDescriptor())
	register(newCronJobControllerDescriptor())
	register(newCertificateSigningRequestSigningControllerDescriptor())
	register(newCertificateSigningRequestApprovingControllerDescriptor())
	register(newCertificateSigningRequestCleanerControllerDescriptor())
	register(newTTLControllerDescriptor())
	register(newBootstrapSignerControllerDescriptor())
	register(newTokenCleanerControllerDescriptor())
	register(newNodeIpamControllerDescriptor())
	register(newNodeLifecycleControllerDescriptor())

	register(newServiceLBControllerDescriptor())          // cloud provider controller
	register(newNodeRouteControllerDescriptor())          // cloud provider controller
	register(newCloudNodeLifecycleControllerDescriptor()) // cloud provider controller
	// TODO: persistent volume controllers into the IncludeCloudLoops only set as a cloud provider controller.

	register(newPersistentVolumeBinderControllerDescriptor())
	register(newPersistentVolumeAttachDetachControllerDescriptor())
	register(newPersistentVolumeExpanderControllerDescriptor())
	register(newClusterRoleAggregrationControllerDescriptor())
	register(newPersistentVolumeClaimProtectionControllerDescriptor())
	register(newPersistentVolumeProtectionControllerDescriptor())
	register(newVolumeAttributesClassProtectionControllerDescriptor())
	register(newTTLAfterFinishedControllerDescriptor())
	register(newRootCACertificatePublisherControllerDescriptor())
	register(newKubeAPIServerSignerClusterTrustBundledPublisherDescriptor())
	register(newEphemeralVolumeControllerDescriptor())

	// feature gated
	register(newStorageVersionGarbageCollectorControllerDescriptor())
	register(newResourceClaimControllerDescriptor())
	register(newLegacyServiceAccountTokenCleanerControllerDescriptor())
	register(newValidatingAdmissionPolicyStatusControllerDescriptor())
	register(newTaintEvictionControllerDescriptor())
	register(newServiceCIDRsControllerDescriptor())
	register(newStorageVersionMigratorControllerDescriptor())
	register(newSELinuxWarningControllerDescriptor())

	for _, alias := range aliases.UnsortedList() {
		if _, ok := controllers[alias]; ok {
			panic(fmt.Sprintf("alias %q conflicts with a controller name", alias))
		}
	}

	return controllers
}

// CreateControllerContext creates a context struct containing references to resources needed by the
// controllers such as the cloud provider and clientBuilder. rootClientBuilder is only used for
// the shared-informers client and token controller.
func CreateControllerContext(ctx context.Context, s *config.CompletedConfig, rootClientBuilder, clientBuilder clientbuilder.ControllerClientBuilder) (ControllerContext, error) {
	// Informer transform to trim ManagedFields for memory efficiency.
	trim := func(obj interface{}) (interface{}, error) {
		if accessor, err := meta.Accessor(obj); err == nil {
			if accessor.GetManagedFields() != nil {
				accessor.SetManagedFields(nil)
			}
		}
		return obj, nil
	}

	versionedClient := rootClientBuilder.ClientOrDie("shared-informers")
	sharedInformers := informers.NewSharedInformerFactoryWithOptions(versionedClient, ResyncPeriod(s)(), informers.WithTransform(trim))

	metadataClient := metadata.NewForConfigOrDie(rootClientBuilder.ConfigOrDie("metadata-informers"))
	metadataInformers := metadatainformer.NewSharedInformerFactoryWithOptions(metadataClient, ResyncPeriod(s)(), metadatainformer.WithTransform(trim))

	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start apiserver and controller manager at the same time.
	if err := genericcontrollermanager.WaitForAPIServer(versionedClient, 10*time.Second); err != nil {
		return ControllerContext{}, fmt.Errorf("failed to wait for apiserver being healthy: %v", err)
	}

	// Use a discovery client capable of being refreshed.
	discoveryClient := rootClientBuilder.DiscoveryClientOrDie("controller-discovery")
	cachedClient := cacheddiscovery.NewMemCacheClient(discoveryClient)
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)
	go wait.Until(func() {
		restMapper.Reset()
	}, 30*time.Second, ctx.Done())

	controllerContext := ControllerContext{
		ClientBuilder:                   clientBuilder,
		InformerFactory:                 sharedInformers,
		ObjectOrMetadataInformerFactory: informerfactory.NewInformerFactory(sharedInformers, metadataInformers),
		ComponentConfig:                 s.ComponentConfig,
		RESTMapper:                      restMapper,
		InformersStarted:                make(chan struct{}),
		ResyncPeriod:                    ResyncPeriod(s),
		ControllerManagerMetrics:        controllersmetrics.NewControllerManagerMetrics(kubeControllerManager),
	}

	if controllerContext.ComponentConfig.GarbageCollectorController.EnableGarbageCollector &&
		controllerContext.IsControllerEnabled(NewControllerDescriptors()[names.GarbageCollectorController]) {
		ignoredResources := make(map[schema.GroupResource]struct{})
		for _, r := range controllerContext.ComponentConfig.GarbageCollectorController.GCIgnoredResources {
			ignoredResources[schema.GroupResource{Group: r.Group, Resource: r.Resource}] = struct{}{}
		}

		controllerContext.GraphBuilder = garbagecollector.NewDependencyGraphBuilder(
			ctx,
			metadataClient,
			controllerContext.RESTMapper,
			ignoredResources,
			controllerContext.ObjectOrMetadataInformerFactory,
			controllerContext.InformersStarted,
		)
	}

	controllersmetrics.Register()
	return controllerContext, nil
}

// StartControllers starts a set of controllers with a specified ControllerContext
func StartControllers(ctx context.Context, controllerCtx ControllerContext, controllerDescriptors map[string]*ControllerDescriptor,
	unsecuredMux *mux.PathRecorderMux, healthzHandler *controllerhealthz.MutableHealthzHandler) error {
	var controllerChecks []healthz.HealthChecker

	// Always start the SA token controller first using a full-power client, since it needs to mint tokens for the rest
	// If this fails, just return here and fail since other controllers won't be able to get credentials.
	if serviceAccountTokenControllerDescriptor, ok := controllerDescriptors[names.ServiceAccountTokenController]; ok {
		check, err := StartController(ctx, controllerCtx, serviceAccountTokenControllerDescriptor, unsecuredMux)
		if err != nil {
			return err
		}
		if check != nil {
			// HealthChecker should be present when controller has started
			controllerChecks = append(controllerChecks, check)
		}
	}

	// Each controller is passed a context where the logger has the name of
	// the controller set through WithName. That name then becomes the prefix of
	// of all log messages emitted by that controller.
	//
	// In StartController, an explicit "controller" key is used instead, for two reasons:
	// - while contextual logging is alpha, klog.LoggerWithName is still a no-op,
	//   so we cannot rely on it yet to add the name
	// - it allows distinguishing between log entries emitted by the controller
	//   and those emitted for it - this is a bit debatable and could be revised.
	for _, controllerDesc := range controllerDescriptors {
		if controllerDesc.RequiresSpecialHandling() {
			continue
		}

		check, err := StartController(ctx, controllerCtx, controllerDesc, unsecuredMux)
		if err != nil {
			return err
		}
		if check != nil {
			// HealthChecker should be present when controller has started
			controllerChecks = append(controllerChecks, check)
		}
	}

	healthzHandler.AddHealthChecker(controllerChecks...)

	return nil
}

// StartController starts a controller with a specified ControllerContext
// and performs required pre- and post- checks/actions
func StartController(ctx context.Context, controllerCtx ControllerContext, controllerDescriptor *ControllerDescriptor,
	unsecuredMux *mux.PathRecorderMux) (healthz.HealthChecker, error) {
	logger := klog.FromContext(ctx)
	controllerName := controllerDescriptor.Name()

	for _, featureGate := range controllerDescriptor.GetRequiredFeatureGates() {
		if !utilfeature.DefaultFeatureGate.Enabled(featureGate) {
			logger.Info("Controller is disabled by a feature gate", "controller", controllerName, "requiredFeatureGates", controllerDescriptor.GetRequiredFeatureGates())
			return nil, nil
		}
	}

	if controllerDescriptor.IsCloudProviderController() {
		logger.Info("Skipping a cloud provider controller", "controller", controllerName)
		return nil, nil
	}

	if !controllerCtx.IsControllerEnabled(controllerDescriptor) {
		logger.Info("Warning: controller is disabled", "controller", controllerName)
		return nil, nil
	}

	time.Sleep(wait.Jitter(controllerCtx.ComponentConfig.Generic.ControllerStartInterval.Duration, ControllerStartJitter))

	logger.V(1).Info("Starting controller", "controller", controllerName)

	initFunc := controllerDescriptor.GetInitFunc()
	ctrl, started, err := initFunc(klog.NewContext(ctx, klog.LoggerWithName(logger, controllerName)), controllerCtx, controllerName)
	if err != nil {
		logger.Error(err, "Error starting controller", "controller", controllerName)
		return nil, err
	}
	if !started {
		logger.Info("Warning: skipping controller", "controller", controllerName)
		return nil, nil
	}

	check := controllerhealthz.NamedPingChecker(controllerName)
	if ctrl != nil {
		// check if the controller supports and requests a debugHandler
		// and it needs the unsecuredMux to mount the handler onto.
		if debuggable, ok := ctrl.(controller.Debuggable); ok && unsecuredMux != nil {
			if debugHandler := debuggable.DebuggingHandler(); debugHandler != nil {
				basePath := "/debug/controllers/" + controllerName
				unsecuredMux.UnlistedHandle(basePath, http.StripPrefix(basePath, debugHandler))
				unsecuredMux.UnlistedHandlePrefix(basePath+"/", http.StripPrefix(basePath, debugHandler))
			}
		}
		if healthCheckable, ok := ctrl.(controller.HealthCheckable); ok {
			if realCheck := healthCheckable.HealthChecker(); realCheck != nil {
				check = controllerhealthz.NamedHealthChecker(controllerName, realCheck)
			}
		}
	}

	logger.Info("Started controller", "controller", controllerName)
	return check, nil
}

// serviceAccountTokenControllerStarter is special because it must run first to set up permissions for other controllers.
// It cannot use the "normal" client builder, so it tracks its own.
func newServiceAccountTokenControllerDescriptor(rootClientBuilder clientbuilder.ControllerClientBuilder) *ControllerDescriptor {
	return &ControllerDescriptor{
		name:    names.ServiceAccountTokenController,
		aliases: []string{"serviceaccount-token"},
		initFunc: func(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
			return startServiceAccountTokenController(ctx, controllerContext, controllerName, rootClientBuilder)
		},
		// will make sure it runs first before other controllers
		requiresSpecialHandling: true,
	}
}

func startServiceAccountTokenController(ctx context.Context, controllerContext ControllerContext, controllerName string, rootClientBuilder clientbuilder.ControllerClientBuilder) (controller.Interface, bool, error) {
	logger := klog.FromContext(ctx)
	if len(controllerContext.ComponentConfig.SAController.ServiceAccountKeyFile) == 0 {
		logger.Info("Controller is disabled because there is no private key", "controller", controllerName)
		return nil, false, nil
	}
	privateKey, err := keyutil.PrivateKeyFromFile(controllerContext.ComponentConfig.SAController.ServiceAccountKeyFile)
	if err != nil {
		return nil, true, fmt.Errorf("error reading key for service account token controller: %v", err)
	}

	var rootCA []byte
	if controllerContext.ComponentConfig.SAController.RootCAFile != "" {
		if rootCA, err = readCA(controllerContext.ComponentConfig.SAController.RootCAFile); err != nil {
			return nil, true, fmt.Errorf("error parsing root-ca-file at %s: %v", controllerContext.ComponentConfig.SAController.RootCAFile, err)
		}
	} else {
		rootCA = rootClientBuilder.ConfigOrDie("tokens-controller").CAData
	}

	tokenGenerator, err := serviceaccount.JWTTokenGenerator(serviceaccount.LegacyIssuer, privateKey)
	if err != nil {
		return nil, false, fmt.Errorf("failed to build token generator: %v", err)
	}
	tokenController, err := serviceaccountcontroller.NewTokensController(
		logger,
		controllerContext.InformerFactory.Core().V1().ServiceAccounts(),
		controllerContext.InformerFactory.Core().V1().Secrets(),
		rootClientBuilder.ClientOrDie("tokens-controller"),
		serviceaccountcontroller.TokensControllerOptions{
			TokenGenerator: tokenGenerator,
			RootCA:         rootCA,
		},
	)
	if err != nil {
		return nil, true, fmt.Errorf("error creating Tokens controller: %v", err)
	}
	go tokenController.Run(ctx, int(controllerContext.ComponentConfig.SAController.ConcurrentSATokenSyncs))

	// start the first set of informers now so that other controllers can start
	controllerContext.InformerFactory.Start(ctx.Done())

	return nil, true, nil
}

func readCA(file string) ([]byte, error) {
	rootCA, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if _, err := certutil.ParseCertsPEM(rootCA); err != nil {
		return nil, err
	}

	return rootCA, err
}

// createClientBuilders creates clientBuilder and rootClientBuilder from the given configuration
func createClientBuilders(c *config.CompletedConfig) (clientBuilder clientbuilder.ControllerClientBuilder, rootClientBuilder clientbuilder.ControllerClientBuilder) {

	rootClientBuilder = clientbuilder.SimpleControllerClientBuilder{
		ClientConfig: c.Kubeconfig,
	}
	if c.ComponentConfig.KubeCloudShared.UseServiceAccountCredentials {

		clientBuilder = clientbuilder.NewDynamicClientBuilder(
			restclient.AnonymousClientConfig(c.Kubeconfig),
			c.Client.CoreV1(),
			metav1.NamespaceSystem)
	} else {
		clientBuilder = rootClientBuilder
	}
	return
}

// leaderElectAndRun runs the leader election, and runs the callbacks once the leader lease is acquired.
// TODO: extract this function into staging/controller-manager
func leaderElectAndRun(ctx context.Context, c *config.CompletedConfig, lockIdentity string, electionChecker *leaderelection.HealthzAdaptor, resourceLock string, leaseName string, callbacks leaderelection.LeaderCallbacks) {
	logger := klog.FromContext(ctx)
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
		logger.Error(err, "Error creating lock")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: c.ComponentConfig.Generic.LeaderElection.LeaseDuration.Duration,
		RenewDeadline: c.ComponentConfig.Generic.LeaderElection.RenewDeadline.Duration,
		RetryPeriod:   c.ComponentConfig.Generic.LeaderElection.RetryPeriod.Duration,
		Callbacks:     callbacks,
		WatchDog:      electionChecker,
		Name:          leaseName,
		Coordinated:   utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CoordinatedLeaderElection),
	})

	panic("unreachable")
}

// filteredControllerDescriptors returns all controllerDescriptors after filtering through filterFunc.
func filteredControllerDescriptors(controllerDescriptors map[string]*ControllerDescriptor, filterFunc leadermigration.FilterFunc, expected leadermigration.FilterResult) map[string]*ControllerDescriptor {
	resultControllers := make(map[string]*ControllerDescriptor)
	for name, controllerDesc := range controllerDescriptors {
		if filterFunc(name) == expected {
			resultControllers[name] = controllerDesc
		}
	}
	return resultControllers
}
