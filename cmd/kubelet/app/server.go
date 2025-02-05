/*
Copyright 2021 The Kubernetes Authors.

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

// Package app makes it easy to create a kubelet server for various contexts.
package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelsdkresource "go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	noopoteltrace "go.opentelemetry.io/otel/trace/noop"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/healthz"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate"
	"k8s.io/client-go/util/connrotation"
	"k8s.io/client-go/util/keyutil"
	cloudprovider "k8s.io/cloud-provider"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/configz"
	"k8s.io/component-base/featuregate"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/tracing"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	nodeutil "k8s.io/component-helpers/node/util"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	"k8s.io/kubernetes/cmd/kubelet/app/options"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/capabilities"
	"k8s.io/kubernetes/pkg/credentialprovider"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubeletscheme "k8s.io/kubernetes/pkg/kubelet/apis/config/scheme"
	kubeletconfigv1beta1conversion "k8s.io/kubernetes/pkg/kubelet/apis/config/v1beta1"
	kubeletconfigvalidation "k8s.io/kubernetes/pkg/kubelet/apis/config/validation"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	kubeletcertificate "k8s.io/kubernetes/pkg/kubelet/certificate"
	"k8s.io/kubernetes/pkg/kubelet/certificate/bootstrap"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig/configfiles"
	kubeletmetrics "k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/server"
	"k8s.io/kubernetes/pkg/kubelet/stats/pidlimit"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	utilfs "k8s.io/kubernetes/pkg/util/filesystem"
	"k8s.io/kubernetes/pkg/util/flock"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/util/rlimit"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/utils/cpuset"
	"k8s.io/utils/exec"
	netutils "k8s.io/utils/net"
)

func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
	// Prevent memory leak from OTel metrics, which we don't use:
	// https://github.com/open-telemetry/opentelemetry-go-contrib/issues/5190
	otel.SetMeterProvider(noop.NewMeterProvider())
}

// NewKubeletCommand creates a *cobra.Command object with default parameters
func NewKubeletCommand() *cobra.Command {
	cleanFlagSet := pflag.NewFlagSet(server.ComponentKubelet, pflag.ContinueOnError)
	cleanFlagSet.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	kubeletFlags := options.NewKubeletFlags()

	kubeletConfig, err := options.NewKubeletConfiguration()
	// programmer error
	if err != nil {
		klog.ErrorS(err, "Failed to create a new kubelet configuration")
		os.Exit(1)
	}

	cmd := &cobra.Command{
		Use: server.ComponentKubelet,
		Long: `The kubelet is the primary "node agent" that runs on each
node. It can register the node with the apiserver using one of: the hostname; a flag to
override the hostname; or specific logic for a cloud provider.

The kubelet works in terms of a PodSpec. A PodSpec is a YAML or JSON object
that describes a pod. The kubelet takes a set of PodSpecs that are provided through
various mechanisms (primarily through the apiserver) and ensures that the containers
described in those PodSpecs are running and healthy. The kubelet doesn't manage
containers which were not created by Kubernetes.

Other than from an PodSpec from the apiserver, there are two ways that a container
manifest can be provided to the Kubelet.

File: Path passed as a flag on the command line. Files under this path will be monitored
periodically for updates. The monitoring period is 20s by default and is configurable
via a flag.

HTTP endpoint: HTTP endpoint passed as a parameter on the command line. This endpoint
is checked every 20 seconds (also configurable with a flag).`,
		// The Kubelet has special flag parsing requirements to enforce flag precedence rules,
		// so we do all our parsing manually in Run, below.
		// DisableFlagParsing=true provides the full set of flags passed to the kubelet in the
		// `args` arg to Run, without Cobra's interference.
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// initial flag parse, since we disable cobra's flag parsing
			if err := cleanFlagSet.Parse(args); err != nil {
				return fmt.Errorf("failed to parse kubelet flag: %w", err)
			}

			// check if there are non-flag arguments in the command line
			cmds := cleanFlagSet.Args()
			if len(cmds) > 0 {
				return fmt.Errorf("unknown command %+s", cmds[0])
			}

			// short-circuit on help
			help, err := cleanFlagSet.GetBool("help")
			if err != nil {
				return errors.New(`"help" flag is non-bool, programmer error, please correct`)
			}
			if help {
				return cmd.Help()
			}

			// short-circuit on verflag
			verflag.PrintAndExitIfRequested()

			// set feature gates from initial flags-based config
			if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(kubeletConfig.FeatureGates); err != nil {
				return fmt.Errorf("failed to set feature gates from initial flags-based config: %w", err)
			}

			// validate the initial KubeletFlags
			if err := options.ValidateKubeletFlags(kubeletFlags); err != nil {
				return fmt.Errorf("failed to validate kubelet flags: %w", err)
			}

			if cleanFlagSet.Changed("pod-infra-container-image") {
				klog.InfoS("--pod-infra-container-image will not be pruned by the image garbage collector in kubelet and should also be set in the remote runtime")
				_ = cmd.Flags().MarkDeprecated("pod-infra-container-image", "--pod-infra-container-image will be removed in 1.35. Image garbage collector will get sandbox image information from CRI.")
			}

			// load kubelet config file, if provided
			if len(kubeletFlags.KubeletConfigFile) > 0 {
				kubeletConfig, err = loadConfigFile(kubeletFlags.KubeletConfigFile)
				if err != nil {
					return fmt.Errorf("failed to load kubelet config file, path: %s, error: %w", kubeletFlags.KubeletConfigFile, err)
				}
			}
			// Merge the kubelet configurations if --config-dir is set
			if len(kubeletFlags.KubeletDropinConfigDirectory) > 0 {
				if err := mergeKubeletConfigurations(kubeletConfig, kubeletFlags.KubeletDropinConfigDirectory); err != nil {
					return fmt.Errorf("failed to merge kubelet configs: %w", err)
				}
			}

			if len(kubeletFlags.KubeletConfigFile) > 0 || len(kubeletFlags.KubeletDropinConfigDirectory) > 0 {
				// We must enforce flag precedence by re-parsing the command line into the new object.
				// This is necessary to preserve backwards-compatibility across binary upgrades.
				// See issue #56171 for more details.
				if err := kubeletConfigFlagPrecedence(kubeletConfig, args); err != nil {
					return fmt.Errorf("failed to precedence kubeletConfigFlag: %w", err)
				}
				// update feature gates based on new config
				if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(kubeletConfig.FeatureGates); err != nil {
					return fmt.Errorf("failed to set feature gates from initial flags-based config: %w", err)
				}
			}

			// Config and flags parsed, now we can initialize logging.
			logs.InitLogs()
			if err := logsapi.ValidateAndApplyAsField(&kubeletConfig.Logging, utilfeature.DefaultFeatureGate, field.NewPath("logging")); err != nil {
				return fmt.Errorf("initialize logging: %v", err)
			}
			cliflag.PrintFlags(cleanFlagSet)

			// We always validate the local configuration (command line + config file).
			// This is the default "last-known-good" config for dynamic config, and must always remain valid.
			if err := kubeletconfigvalidation.ValidateKubeletConfiguration(kubeletConfig, utilfeature.DefaultFeatureGate); err != nil {
				return fmt.Errorf("failed to validate kubelet configuration, error: %w, path: %s", err, kubeletConfig)
			}

			if (kubeletConfig.KubeletCgroups != "" && kubeletConfig.KubeReservedCgroup != "") && (strings.Index(kubeletConfig.KubeletCgroups, kubeletConfig.KubeReservedCgroup) != 0) {
				klog.InfoS("unsupported configuration:KubeletCgroups is not within KubeReservedCgroup")
			}

			// construct a KubeletServer from kubeletFlags and kubeletConfig
			kubeletServer := &options.KubeletServer{
				KubeletFlags:         *kubeletFlags,
				KubeletConfiguration: *kubeletConfig,
			}

			// use kubeletServer to construct the default KubeletDeps
			kubeletDeps, err := UnsecuredDependencies(kubeletServer, utilfeature.DefaultFeatureGate)
			if err != nil {
				return fmt.Errorf("failed to construct kubelet dependencies: %w", err)
			}

			if err := checkPermissions(); err != nil {
				klog.ErrorS(err, "kubelet running with insufficient permissions")
			}

			// make the kubelet's config safe for logging
			config := kubeletServer.KubeletConfiguration.DeepCopy()
			for k := range config.StaticPodURLHeader {
				config.StaticPodURLHeader[k] = []string{"<masked>"}
			}
			// log the kubelet's config for inspection
			klog.V(5).InfoS("KubeletConfiguration", "configuration", klog.Format(config))

			// set up signal context for kubelet shutdown
			ctx := genericapiserver.SetupSignalContext()

			utilfeature.DefaultMutableFeatureGate.AddMetrics()
			// run the kubelet
			return Run(ctx, kubeletServer, kubeletDeps, utilfeature.DefaultFeatureGate)
		},
	}

	// keep cleanFlagSet separate, so Cobra doesn't pollute it with the global flags
	kubeletFlags.AddFlags(cleanFlagSet)
	options.AddKubeletConfigFlags(cleanFlagSet, kubeletConfig)
	options.AddGlobalFlags(cleanFlagSet)
	cleanFlagSet.BoolP("help", "h", false, fmt.Sprintf("help for %s", cmd.Name()))

	// ugly, but necessary, because Cobra's default UsageFunc and HelpFunc pollute the flagset with global flags
	const usageFmt = "Usage:\n  %s\n\nFlags:\n%s"
	cmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), usageFmt, cmd.UseLine(), cleanFlagSet.FlagUsagesWrapped(2))
		return nil
	})
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine(), cleanFlagSet.FlagUsagesWrapped(2))
	})

	return cmd
}

// mergeKubeletConfigurations merges the provided drop-in configurations with the base kubelet configuration.
// The drop-in configurations are processed in lexical order based on the file names. This means that the
// configurations in files with lower numeric prefixes are applied first, followed by higher numeric prefixes.
// For example, if the drop-in directory contains files named "10-config.conf" and "20-config.conf",
// the configurations in "10-config.conf" will be applied first, and then the configurations in "20-config.conf" will be applied,
// potentially overriding the previous values.
func mergeKubeletConfigurations(kubeletConfig *kubeletconfiginternal.KubeletConfiguration, kubeletDropInConfigDir string) error {
	const dropinFileExtension = ".conf"

	// TODO: move the "internal --> versioned --> merge --> default --> internal" operation inside the loop,
	// and use the version of each drop-in file as the versioned target once config files can be in versions other than just v1beta1
	versionedConfig := &kubeletconfigv1beta1.KubeletConfiguration{}
	if err := kubeletconfigv1beta1conversion.Convert_config_KubeletConfiguration_To_v1beta1_KubeletConfiguration(kubeletConfig, versionedConfig, nil); err != nil {
		return err
	}

	baseKubeletConfigJSON, err := json.Marshal(versionedConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal base config: %w", err)
	}
	// Walk through the drop-in directory and update the configuration for each file
	if err := filepath.WalkDir(kubeletDropInConfigDir, func(path string, info fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(info.Name()) == dropinFileExtension {
			dropinConfigJSON, gvk, err := loadDropinConfigFileIntoJSON(path)
			if err != nil {
				return fmt.Errorf("failed to load kubelet dropin file, path: %s, error: %w", path, err)
			}
			if gvk == nil || gvk.Empty() {
				return fmt.Errorf("failed to load kubelet dropin file, path: %s, no apiVersion/kind", path)
			}
			// TODO: expand this once more than a single kubelet config version exists
			if *gvk != kubeletconfigv1beta1.SchemeGroupVersion.WithKind("KubeletConfiguration") {
				return fmt.Errorf("failed to load kubelet dropin file, path: %s, unknown apiVersion/kind: %v", path, gvk.String())
			}
			mergedConfigJSON, err := jsonpatch.MergePatch(baseKubeletConfigJSON, dropinConfigJSON)
			if err != nil {
				return fmt.Errorf("failed to merge drop-in and current config: %w", err)
			}
			baseKubeletConfigJSON = mergedConfigJSON
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to walk through kubelet dropin directory %q: %w", kubeletDropInConfigDir, err)
	}

	// reset versioned config and decode
	versionedConfig = &kubeletconfigv1beta1.KubeletConfiguration{}
	if err := json.Unmarshal(baseKubeletConfigJSON, versionedConfig); err != nil {
		return fmt.Errorf("failed to unmarshal merged JSON into kubelet configuration: %w", err)
	}
	// apply defaulting after decoding
	kubeletconfigv1beta1conversion.SetDefaults_KubeletConfiguration(versionedConfig)

	// convert back to internal config
	if err := kubeletconfigv1beta1conversion.Convert_v1beta1_KubeletConfiguration_To_config_KubeletConfiguration(versionedConfig, kubeletConfig, nil); err != nil {
		return fmt.Errorf("failed to convert merged config to internal kubelet configuration: %w", err)
	}

	return nil
}

// newFlagSetWithGlobals constructs a new pflag.FlagSet with global flags registered
// on it.
func newFlagSetWithGlobals() *pflag.FlagSet {
	fs := pflag.NewFlagSet("", pflag.ExitOnError)
	// set the normalize func, similar to k8s.io/component-base/cli//flags.go:InitFlags
	fs.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	// explicitly add flags from libs that register global flags
	options.AddGlobalFlags(fs)
	return fs
}

// newFakeFlagSet constructs a pflag.FlagSet with the same flags as fs, but where
// all values have noop Set implementations
func newFakeFlagSet(fs *pflag.FlagSet) *pflag.FlagSet {
	ret := pflag.NewFlagSet("", pflag.ExitOnError)
	ret.SetNormalizeFunc(fs.GetNormalizeFunc())
	fs.VisitAll(func(f *pflag.Flag) {
		ret.VarP(cliflag.NoOp{}, f.Name, f.Shorthand, f.Usage)
	})
	return ret
}

// kubeletConfigFlagPrecedence re-parses flags over the KubeletConfiguration object.
// We must enforce flag precedence by re-parsing the command line into the new object.
// This is necessary to preserve backwards-compatibility across binary upgrades.
// See issue #56171 for more details.
func kubeletConfigFlagPrecedence(kc *kubeletconfiginternal.KubeletConfiguration, args []string) error {
	// We use a throwaway kubeletFlags and a fake global flagset to avoid double-parses,
	// as some Set implementations accumulate values from multiple flag invocations.
	fs := newFakeFlagSet(newFlagSetWithGlobals())
	// register throwaway KubeletFlags
	options.NewKubeletFlags().AddFlags(fs)
	// register new KubeletConfiguration
	options.AddKubeletConfigFlags(fs, kc)
	// Remember original feature gates, so we can merge with flag gates later
	original := kc.FeatureGates
	// avoid duplicate printing the flag deprecation warnings during re-parsing
	fs.SetOutput(io.Discard)
	// re-parse flags
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Add back feature gates that were set in the original kc, but not in flags
	for k, v := range original {
		if _, ok := kc.FeatureGates[k]; !ok {
			kc.FeatureGates[k] = v
		}
	}
	return nil
}

func loadConfigFile(name string) (*kubeletconfiginternal.KubeletConfiguration, error) {
	const errFmt = "failed to load Kubelet config file %s, error %v"
	// compute absolute path based on current working dir
	kubeletConfigFile, err := filepath.Abs(name)
	if err != nil {
		return nil, fmt.Errorf(errFmt, name, err)
	}
	loader, err := configfiles.NewFsLoader(&utilfs.DefaultFs{}, kubeletConfigFile)
	if err != nil {
		return nil, fmt.Errorf(errFmt, name, err)
	}
	kc, err := loader.Load()
	if err != nil {
		return nil, fmt.Errorf(errFmt, name, err)
	}

	// EvictionHard may be nil if it was not set in kubelet's config file.
	// EvictionHard can have OS-specific fields, which is why there's no default value for it.
	// See: https://github.com/kubernetes/kubernetes/pull/110263
	if kc.EvictionHard == nil {
		kc.EvictionHard = eviction.DefaultEvictionHard
	} else if kc.MergeDefaultEvictionSettings {
		for k, v := range eviction.DefaultEvictionHard {
			if _, exists := kc.EvictionHard[k]; !exists {
				kc.EvictionHard[k] = v
			}
		}
	}
	return kc, err
}

func loadDropinConfigFileIntoJSON(name string) ([]byte, *schema.GroupVersionKind, error) {
	const errFmt = "failed to load drop-in kubelet config file %s, error %v"
	// compute absolute path based on current working dir
	kubeletConfigFile, err := filepath.Abs(name)
	if err != nil {
		return nil, nil, fmt.Errorf(errFmt, name, err)
	}
	loader, err := configfiles.NewFsLoader(&utilfs.DefaultFs{}, kubeletConfigFile)
	if err != nil {
		return nil, nil, fmt.Errorf(errFmt, name, err)
	}
	return loader.LoadIntoJSON()
}

// UnsecuredDependencies returns a Dependencies suitable for being run, or an error if the server setup
// is not valid.  It will not start any background processes, and does not include authentication/authorization
func UnsecuredDependencies(s *options.KubeletServer, featureGate featuregate.FeatureGate) (*kubelet.Dependencies, error) {
	// Initialize the TLS Options
	tlsOptions, err := InitializeTLS(&s.KubeletFlags, &s.KubeletConfiguration)
	if err != nil {
		return nil, err
	}

	mounter := mount.New(s.ExperimentalMounterPath)
	subpather := subpath.New(mounter)
	hu := hostutil.NewHostUtil()
	pluginRunner := exec.New()

	plugins, err := ProbeVolumePlugins(featureGate)
	if err != nil {
		return nil, err
	}
	var tp oteltrace.TracerProvider
	tp = noopoteltrace.NewTracerProvider()
	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletTracing) {
		tp, err = newTracerProvider(s)
		if err != nil {
			return nil, err
		}
	}
	return &kubelet.Dependencies{
		Auth:                nil, // default does not enforce auth[nz]
		CAdvisorInterface:   nil, // cadvisor.New launches background processes (bg http.ListenAndServe, and some bg cleaners), not set here
		Cloud:               nil, // cloud provider might start background processes
		ContainerManager:    nil,
		KubeClient:          nil,
		HeartbeatClient:     nil,
		EventClient:         nil,
		TracerProvider:      tp,
		HostUtil:            hu,
		Mounter:             mounter,
		Subpather:           subpather,
		OOMAdjuster:         oom.NewOOMAdjuster(),
		OSInterface:         kubecontainer.RealOS{},
		VolumePlugins:       plugins,
		DynamicPluginProber: GetDynamicPluginProber(s.VolumePluginDir, pluginRunner),
		TLSOptions:          tlsOptions}, nil
}

// Run runs the specified KubeletServer with the given Dependencies. This should never exit.
// The kubeDeps argument may be nil - if so, it is initialized from the settings on KubeletServer.
// Otherwise, the caller is assumed to have set up the Dependencies object and a default one will
// not be generated.
func Run(ctx context.Context, s *options.KubeletServer, kubeDeps *kubelet.Dependencies, featureGate featuregate.FeatureGate) error {
	// To help debugging, immediately log version
	klog.InfoS("Kubelet version", "kubeletVersion", version.Get())

	klog.InfoS("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS", os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	if err := initForOS(s.KubeletFlags.WindowsService, s.KubeletFlags.WindowsPriorityClass); err != nil {
		return fmt.Errorf("failed OS init: %w", err)
	}
	if err := run(ctx, s, kubeDeps, featureGate); err != nil {
		return fmt.Errorf("failed to run Kubelet: %w", err)
	}
	return nil
}

func setConfigz(cz *configz.Config, kc *kubeletconfiginternal.KubeletConfiguration) error {
	scheme, _, err := kubeletscheme.NewSchemeAndCodecs()
	if err != nil {
		return err
	}
	versioned := kubeletconfigv1beta1.KubeletConfiguration{}
	if err := scheme.Convert(kc, &versioned, nil); err != nil {
		return err
	}
	cz.Set(versioned)
	return nil
}

func initConfigz(kc *kubeletconfiginternal.KubeletConfiguration) error {
	cz, err := configz.New("kubeletconfig")
	if err != nil {
		klog.ErrorS(err, "Failed to register configz")
		return err
	}
	if err := setConfigz(cz, kc); err != nil {
		klog.ErrorS(err, "Failed to register config")
		return err
	}
	return nil
}

// makeEventRecorder sets up kubeDeps.Recorder if it's nil. It's a no-op otherwise.
func makeEventRecorder(ctx context.Context, kubeDeps *kubelet.Dependencies, nodeName types.NodeName) {
	if kubeDeps.Recorder != nil {
		return
	}
	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	kubeDeps.Recorder = eventBroadcaster.NewRecorder(legacyscheme.Scheme, v1.EventSource{Component: server.ComponentKubelet, Host: string(nodeName)})
	eventBroadcaster.StartStructuredLogging(3)
	if kubeDeps.EventClient != nil {
		klog.V(4).InfoS("Sending events to api server")
		eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeDeps.EventClient.Events("")})
	} else {
		klog.InfoS("No api server defined - no events will be sent to API server")
	}
}

func getReservedCPUs(machineInfo *cadvisorapi.MachineInfo, cpus string) (cpuset.CPUSet, error) {
	emptyCPUSet := cpuset.New()

	if cpus == "" {
		return emptyCPUSet, nil
	}

	topo, err := topology.Discover(machineInfo)
	if err != nil {
		return emptyCPUSet, fmt.Errorf("unable to discover CPU topology info: %s", err)
	}
	reservedCPUSet, err := cpuset.Parse(cpus)
	if err != nil {
		return emptyCPUSet, fmt.Errorf("unable to parse reserved-cpus list: %s", err)
	}
	allCPUSet := topo.CPUDetails.CPUs()
	if !reservedCPUSet.IsSubsetOf(allCPUSet) {
		return emptyCPUSet, fmt.Errorf("reserved-cpus: %s is not a subset of online-cpus: %s", cpus, allCPUSet.String())
	}
	return reservedCPUSet, nil
}

func run(ctx context.Context, s *options.KubeletServer, kubeDeps *kubelet.Dependencies, featureGate featuregate.FeatureGate) (err error) {
	// Set global feature gates based on the value on the initial KubeletServer
	err = utilfeature.DefaultMutableFeatureGate.SetFromMap(s.KubeletConfiguration.FeatureGates)
	if err != nil {
		return err
	}
	// validate the initial KubeletServer (we set feature gates first, because this validation depends on feature gates)
	if err := options.ValidateKubeletServer(s); err != nil {
		return err
	}

	// Warn if MemoryQoS enabled with cgroups v1
	if utilfeature.DefaultFeatureGate.Enabled(features.MemoryQoS) &&
		!kubeletutil.IsCgroup2UnifiedMode() {
		klog.InfoS("Warning: MemoryQoS feature only works with cgroups v2 on Linux, but enabled with cgroups v1")
	}
	// Obtain Kubelet Lock File
	if s.ExitOnLockContention && s.LockFilePath == "" {
		return errors.New("cannot exit on lock file contention: no lock file specified")
	}
	done := make(chan struct{})
	if s.LockFilePath != "" {
		klog.InfoS("Acquiring file lock", "path", s.LockFilePath)
		if err := flock.Acquire(s.LockFilePath); err != nil {
			return fmt.Errorf("unable to acquire file lock on %q: %w", s.LockFilePath, err)
		}
		if s.ExitOnLockContention {
			klog.InfoS("Watching for inotify events", "path", s.LockFilePath)
			if err := watchForLockfileContention(s.LockFilePath, done); err != nil {
				return err
			}
		}
	}

	// Register current configuration with /configz endpoint
	err = initConfigz(&s.KubeletConfiguration)
	if err != nil {
		klog.ErrorS(err, "Failed to register kubelet configuration with configz")
	}

	if len(s.ShowHiddenMetricsForVersion) > 0 {
		metrics.SetShowHidden()
	}

	// About to get clients and such, detect standaloneMode
	standaloneMode := true
	if len(s.KubeConfig) > 0 {
		standaloneMode = false
	}

	if kubeDeps == nil {
		kubeDeps, err = UnsecuredDependencies(s, featureGate)
		if err != nil {
			return err
		}
	}

	if kubeDeps.Cloud == nil {
		if !cloudprovider.IsExternal(s.CloudProvider) && len(s.CloudProvider) != 0 {
			// internal cloud provider loops are disabled
			cloudprovider.DisableWarningForProvider(s.CloudProvider)
			return cloudprovider.ErrorForDisabledProvider(s.CloudProvider)
		}
	}

	hostName, err := nodeutil.GetHostname(s.HostnameOverride)
	if err != nil {
		return err
	}
	nodeName, err := getNodeName(kubeDeps.Cloud, hostName)
	if err != nil {
		return err
	}

	// if in standalone mode, indicate as much by setting all clients to nil
	switch {
	case standaloneMode:
		kubeDeps.KubeClient = nil
		kubeDeps.EventClient = nil
		kubeDeps.HeartbeatClient = nil
		klog.InfoS("Standalone mode, no API client")

	case kubeDeps.KubeClient == nil, kubeDeps.EventClient == nil, kubeDeps.HeartbeatClient == nil:
		clientConfig, onHeartbeatFailure, err := buildKubeletClientConfig(ctx, s, kubeDeps.TracerProvider, nodeName)
		if err != nil {
			return err
		}
		if onHeartbeatFailure == nil {
			return errors.New("onHeartbeatFailure must be a valid function other than nil")
		}
		kubeDeps.OnHeartbeatFailure = onHeartbeatFailure

		kubeDeps.KubeClient, err = clientset.NewForConfig(clientConfig)
		if err != nil {
			return fmt.Errorf("failed to initialize kubelet client: %w", err)
		}

		// make a separate client for events
		eventClientConfig := *clientConfig
		eventClientConfig.QPS = float32(s.EventRecordQPS)
		eventClientConfig.Burst = int(s.EventBurst)
		kubeDeps.EventClient, err = v1core.NewForConfig(&eventClientConfig)
		if err != nil {
			return fmt.Errorf("failed to initialize kubelet event client: %w", err)
		}

		// make a separate client for heartbeat with throttling disabled and a timeout attached
		heartbeatClientConfig := *clientConfig
		heartbeatClientConfig.Timeout = s.KubeletConfiguration.NodeStatusUpdateFrequency.Duration
		// The timeout is the minimum of the lease duration and status update frequency
		leaseTimeout := time.Duration(s.KubeletConfiguration.NodeLeaseDurationSeconds) * time.Second
		if heartbeatClientConfig.Timeout > leaseTimeout {
			heartbeatClientConfig.Timeout = leaseTimeout
		}

		heartbeatClientConfig.QPS = float32(-1)
		kubeDeps.HeartbeatClient, err = clientset.NewForConfig(&heartbeatClientConfig)
		if err != nil {
			return fmt.Errorf("failed to initialize kubelet heartbeat client: %w", err)
		}
	}

	if kubeDeps.Auth == nil {
		auth, runAuthenticatorCAReload, err := BuildAuth(nodeName, kubeDeps.KubeClient, s.KubeletConfiguration)
		if err != nil {
			return err
		}
		kubeDeps.Auth = auth
		runAuthenticatorCAReload(ctx.Done())
	}

	if err := kubelet.PreInitRuntimeService(&s.KubeletConfiguration, kubeDeps); err != nil {
		return err
	}

	// Get cgroup driver setting from CRI
	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletCgroupDriverFromCRI) {
		if err := getCgroupDriverFromCRI(ctx, s, kubeDeps); err != nil {
			return err
		}
	}

	var cgroupRoots []string
	nodeAllocatableRoot := cm.NodeAllocatableRoot(s.CgroupRoot, s.CgroupsPerQOS, s.CgroupDriver)
	cgroupRoots = append(cgroupRoots, nodeAllocatableRoot)
	kubeletCgroup, err := cm.GetKubeletContainer(s.KubeletCgroups)
	if err != nil {
		klog.InfoS("Failed to get the kubelet's cgroup. Kubelet system container metrics may be missing.", "err", err)
	} else if kubeletCgroup != "" {
		cgroupRoots = append(cgroupRoots, kubeletCgroup)
	}

	if s.RuntimeCgroups != "" {
		// RuntimeCgroups is optional, so ignore if it isn't specified
		cgroupRoots = append(cgroupRoots, s.RuntimeCgroups)
	}

	if s.SystemCgroups != "" {
		// SystemCgroups is optional, so ignore if it isn't specified
		cgroupRoots = append(cgroupRoots, s.SystemCgroups)
	}

	if kubeDeps.CAdvisorInterface == nil {
		imageFsInfoProvider := cadvisor.NewImageFsInfoProvider(s.ContainerRuntimeEndpoint)
		kubeDeps.CAdvisorInterface, err = cadvisor.New(imageFsInfoProvider, s.RootDirectory, cgroupRoots, cadvisor.UsingLegacyCadvisorStats(s.ContainerRuntimeEndpoint), s.LocalStorageCapacityIsolation)
		if err != nil {
			return err
		}
	}

	// Setup event recorder if required.
	makeEventRecorder(ctx, kubeDeps, nodeName)

	if kubeDeps.ContainerManager == nil {
		if s.CgroupsPerQOS && s.CgroupRoot == "" {
			klog.InfoS("--cgroups-per-qos enabled, but --cgroup-root was not specified.  defaulting to /")
			s.CgroupRoot = "/"
		}

		machineInfo, err := kubeDeps.CAdvisorInterface.MachineInfo()
		if err != nil {
			return err
		}
		reservedSystemCPUs, err := getReservedCPUs(machineInfo, s.ReservedSystemCPUs)
		if err != nil {
			return err
		}
		if reservedSystemCPUs.Size() > 0 {
			// at cmd option validation phase it is tested either --system-reserved-cgroup or --kube-reserved-cgroup is specified, so overwrite should be ok
			klog.InfoS("Option --reserved-cpus is specified, it will overwrite the cpu setting in KubeReserved and SystemReserved", "kubeReserved", s.KubeReserved, "systemReserved", s.SystemReserved)
			if s.KubeReserved != nil {
				delete(s.KubeReserved, "cpu")
			}
			if s.SystemReserved == nil {
				s.SystemReserved = make(map[string]string)
			}
			s.SystemReserved["cpu"] = strconv.Itoa(reservedSystemCPUs.Size())
			klog.InfoS("After cpu setting is overwritten", "kubeReserved", s.KubeReserved, "systemReserved", s.SystemReserved)
		}

		kubeReserved, err := parseResourceList(s.KubeReserved)
		if err != nil {
			return fmt.Errorf("--kube-reserved value failed to parse: %w", err)
		}
		systemReserved, err := parseResourceList(s.SystemReserved)
		if err != nil {
			return fmt.Errorf("--system-reserved value failed to parse: %w", err)
		}

		var hardEvictionThresholds []evictionapi.Threshold
		// If the user requested to ignore eviction thresholds, then do not set valid values for hardEvictionThresholds here.
		if !s.ExperimentalNodeAllocatableIgnoreEvictionThreshold {
			hardEvictionThresholds, err = eviction.ParseThresholdConfig([]string{}, s.EvictionHard, nil, nil, nil)
			if err != nil {
				return err
			}
		}
		experimentalQOSReserved, err := cm.ParseQOSReserved(s.QOSReserved)
		if err != nil {
			return fmt.Errorf("--qos-reserved value failed to parse: %w", err)
		}

		var cpuManagerPolicyOptions map[string]string
		if utilfeature.DefaultFeatureGate.Enabled(features.CPUManagerPolicyOptions) {
			cpuManagerPolicyOptions = s.CPUManagerPolicyOptions
		} else if s.CPUManagerPolicyOptions != nil {
			return fmt.Errorf("CPU Manager policy options %v require feature gates %q, %q enabled",
				s.CPUManagerPolicyOptions, features.CPUManager, features.CPUManagerPolicyOptions)
		}

		var topologyManagerPolicyOptions map[string]string
		if utilfeature.DefaultFeatureGate.Enabled(features.TopologyManagerPolicyOptions) {
			topologyManagerPolicyOptions = s.TopologyManagerPolicyOptions
		} else if s.TopologyManagerPolicyOptions != nil {
			return fmt.Errorf("topology manager policy options %v require feature gates %q enabled",
				s.TopologyManagerPolicyOptions, features.TopologyManagerPolicyOptions)
		}
		if utilfeature.DefaultFeatureGate.Enabled(features.NodeSwap) {
			if !kubeletutil.IsCgroup2UnifiedMode() && s.MemorySwap.SwapBehavior == kubelettypes.LimitedSwap {
				// This feature is not supported for cgroupv1 so we are failing early.
				return fmt.Errorf("swap feature is enabled and LimitedSwap but it is only supported with cgroupv2")
			}
			if !s.FailSwapOn && s.MemorySwap.SwapBehavior == "" {
				// This is just a log because we are using a default of NoSwap.
				klog.InfoS("NoSwap is set due to memorySwapBehavior not specified", "memorySwapBehavior", s.MemorySwap.SwapBehavior, "FailSwapOn", s.FailSwapOn)
			}
		}

		kubeDeps.ContainerManager, err = cm.NewContainerManager(
			kubeDeps.Mounter,
			kubeDeps.CAdvisorInterface,
			cm.NodeConfig{
				NodeName:              nodeName,
				RuntimeCgroupsName:    s.RuntimeCgroups,
				SystemCgroupsName:     s.SystemCgroups,
				KubeletCgroupsName:    s.KubeletCgroups,
				KubeletOOMScoreAdj:    s.OOMScoreAdj,
				CgroupsPerQOS:         s.CgroupsPerQOS,
				CgroupRoot:            s.CgroupRoot,
				CgroupDriver:          s.CgroupDriver,
				KubeletRootDir:        s.RootDirectory,
				ProtectKernelDefaults: s.ProtectKernelDefaults,
				NodeAllocatableConfig: cm.NodeAllocatableConfig{
					KubeReservedCgroupName:   s.KubeReservedCgroup,
					SystemReservedCgroupName: s.SystemReservedCgroup,
					EnforceNodeAllocatable:   sets.New(s.EnforceNodeAllocatable...),
					KubeReserved:             kubeReserved,
					SystemReserved:           systemReserved,
					ReservedSystemCPUs:       reservedSystemCPUs,
					HardEvictionThresholds:   hardEvictionThresholds,
				},
				QOSReserved:                  *experimentalQOSReserved,
				CPUManagerPolicy:             s.CPUManagerPolicy,
				CPUManagerPolicyOptions:      cpuManagerPolicyOptions,
				CPUManagerReconcilePeriod:    s.CPUManagerReconcilePeriod.Duration,
				MemoryManagerPolicy:          s.MemoryManagerPolicy,
				MemoryManagerReservedMemory:  s.ReservedMemory,
				PodPidsLimit:                 s.PodPidsLimit,
				EnforceCPULimits:             s.CPUCFSQuota,
				CPUCFSQuotaPeriod:            s.CPUCFSQuotaPeriod.Duration,
				TopologyManagerPolicy:        s.TopologyManagerPolicy,
				TopologyManagerScope:         s.TopologyManagerScope,
				TopologyManagerPolicyOptions: topologyManagerPolicyOptions,
			},
			s.FailSwapOn,
			kubeDeps.Recorder,
			kubeDeps.KubeClient,
		)

		if err != nil {
			return err
		}
	}

	if kubeDeps.PodStartupLatencyTracker == nil {
		kubeDeps.PodStartupLatencyTracker = kubeletutil.NewPodStartupLatencyTracker()
	}

	if kubeDeps.NodeStartupLatencyTracker == nil {
		kubeDeps.NodeStartupLatencyTracker = kubeletutil.NewNodeStartupLatencyTracker()
	}

	// TODO(vmarmol): Do this through container config.
	oomAdjuster := kubeDeps.OOMAdjuster
	if err := oomAdjuster.ApplyOOMScoreAdj(0, int(s.OOMScoreAdj)); err != nil {
		klog.InfoS("Failed to ApplyOOMScoreAdj", "err", err)
	}

	if err := RunKubelet(ctx, s, kubeDeps); err != nil {
		return err
	}

	if s.HealthzPort > 0 {
		mux := http.NewServeMux()
		healthz.InstallHandler(mux)
		go wait.Until(func() {
			err := http.ListenAndServe(net.JoinHostPort(s.HealthzBindAddress, strconv.Itoa(int(s.HealthzPort))), mux)
			if err != nil {
				klog.ErrorS(err, "Failed to start healthz server")
			}
		}, 5*time.Second, wait.NeverStop)
	}

	// If systemd is used, notify it that we have started
	go daemon.SdNotify(false, "READY=1")

	select {
	case <-done:
		break
	case <-ctx.Done():
		break
	}

	return nil
}

// buildKubeletClientConfig constructs the appropriate client config for the kubelet depending on whether
// bootstrapping is enabled or client certificate rotation is enabled.
func buildKubeletClientConfig(ctx context.Context, s *options.KubeletServer, tp oteltrace.TracerProvider, nodeName types.NodeName) (*restclient.Config, func(), error) {
	if s.RotateCertificates {
		// Rules for client rotation and the handling of kube config files:
		//
		// 1. If the client provides only a kubeconfig file, we must use that as the initial client
		//    kubeadm needs the initial data in the kubeconfig to be placed into the cert store
		// 2. If the client provides only an initial bootstrap kubeconfig file, we must create a
		//    kubeconfig file at the target location that points to the cert store, but until
		//    the file is present the client config will have no certs
		// 3. If the client provides both and the kubeconfig is valid, we must ignore the bootstrap
		//    kubeconfig.
		// 4. If the client provides both and the kubeconfig is expired or otherwise invalid, we must
		//    replace the kubeconfig with a new file that points to the cert dir
		//
		// The desired configuration for bootstrapping is to use a bootstrap kubeconfig and to have
		// the kubeconfig file be managed by this process. For backwards compatibility with kubeadm,
		// which provides a high powered kubeconfig on the master with cert/key data, we must
		// bootstrap the cert manager with the contents of the initial client config.

		klog.InfoS("Client rotation is on, will bootstrap in background")
		certConfig, clientConfig, err := bootstrap.LoadClientConfig(s.KubeConfig, s.BootstrapKubeconfig, s.CertDirectory)
		if err != nil {
			return nil, nil, err
		}

		// use the correct content type for cert rotation, but don't set QPS
		setContentTypeForClient(certConfig, s.ContentType)

		kubeClientConfigOverrides(s, clientConfig)

		clientCertificateManager, err := buildClientCertificateManager(certConfig, clientConfig, s.CertDirectory, nodeName)
		if err != nil {
			return nil, nil, err
		}

		legacyregistry.RawMustRegister(metrics.NewGaugeFunc(
			&metrics.GaugeOpts{
				Subsystem: kubeletmetrics.KubeletSubsystem,
				Name:      "certificate_manager_client_ttl_seconds",
				Help: "Gauge of the TTL (time-to-live) of the Kubelet's client certificate. " +
					"The value is in seconds until certificate expiry (negative if already expired). " +
					"If client certificate is invalid or unused, the value will be +INF.",
				StabilityLevel: metrics.ALPHA,
			},
			func() float64 {
				if c := clientCertificateManager.Current(); c != nil && c.Leaf != nil {
					return math.Trunc(time.Until(c.Leaf.NotAfter).Seconds())
				}
				return math.Inf(1)
			},
		))

		// the rotating transport will use the cert from the cert manager instead of these files
		transportConfig := restclient.AnonymousClientConfig(clientConfig)

		// we set exitAfter to five minutes because we use this client configuration to request new certs - if we are unable
		// to request new certs, we will be unable to continue normal operation. Exiting the process allows a wrapper
		// or the bootstrapping credentials to potentially lay down new initial config.
		closeAllConns, err := kubeletcertificate.UpdateTransport(wait.NeverStop, transportConfig, clientCertificateManager, 5*time.Minute)
		if err != nil {
			return nil, nil, err
		}
		var onHeartbeatFailure func()
		// Kubelet needs to be able to recover from stale http connections.
		// HTTP2 has a mechanism to detect broken connections by sending periodical pings.
		// HTTP1 only can have one persistent connection, and it will close all Idle connections
		// once the Kubelet heartbeat fails. However, since there are many edge cases that we can't
		// control, users can still opt-in to the previous behavior for closing the connections by
		// setting the environment variable DISABLE_HTTP2.
		if s := os.Getenv("DISABLE_HTTP2"); len(s) > 0 {
			klog.InfoS("HTTP2 has been explicitly disabled, Kubelet will forcefully close active connections on heartbeat failures")
			onHeartbeatFailure = closeAllConns
		} else {
			onHeartbeatFailure = func() { utilnet.CloseIdleConnectionsFor(transportConfig.Transport) }
		}

		klog.V(2).InfoS("Starting client certificate rotation")
		clientCertificateManager.Start()

		return transportConfig, onHeartbeatFailure, nil
	}

	if len(s.BootstrapKubeconfig) > 0 {
		if err := bootstrap.LoadClientCert(ctx, s.KubeConfig, s.BootstrapKubeconfig, s.CertDirectory, nodeName); err != nil {
			return nil, nil, err
		}
	}

	clientConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: s.KubeConfig},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid kubeconfig: %w", err)
	}

	kubeClientConfigOverrides(s, clientConfig)
	// Kubelet needs to be able to recover from stale http connections.
	// HTTP2 has a mechanism to detect broken connections by sending periodical pings.
	// HTTP1 only can have one persistent connection, and it will close all Idle connections
	// once the Kubelet heartbeat fails. However, since there are many edge cases that we can't
	// control, users can still opt-in to the previous behavior for closing the connections by
	// setting the environment variable DISABLE_HTTP2.
	var onHeartbeatFailure func()
	if s := os.Getenv("DISABLE_HTTP2"); len(s) > 0 {
		klog.InfoS("HTTP2 has been explicitly disabled, updating Kubelet client Dialer to forcefully close active connections on heartbeat failures")
		onHeartbeatFailure, err = updateDialer(clientConfig)
		if err != nil {
			return nil, nil, err
		}
	} else {
		onHeartbeatFailure = func() {
			utilnet.CloseIdleConnectionsFor(clientConfig.Transport)
		}
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletTracing) {
		clientConfig.Wrap(tracing.WrapperFor(tp))
	}
	return clientConfig, onHeartbeatFailure, nil
}

// updateDialer instruments a restconfig with a dial. the returned function allows forcefully closing all active connections.
func updateDialer(clientConfig *restclient.Config) (func(), error) {
	if clientConfig.Transport != nil || clientConfig.Dial != nil {
		return nil, fmt.Errorf("there is already a transport or dialer configured")
	}
	d := connrotation.NewDialer((&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext)
	clientConfig.Dial = d.DialContext
	return d.CloseAll, nil
}

// buildClientCertificateManager creates a certificate manager that will use certConfig to request a client certificate
// if no certificate is available, or the most recent clientConfig (which is assumed to point to the cert that the manager will
// write out).
func buildClientCertificateManager(certConfig, clientConfig *restclient.Config, certDir string, nodeName types.NodeName) (certificate.Manager, error) {
	newClientsetFn := func(current *tls.Certificate) (clientset.Interface, error) {
		// If we have a valid certificate, use that to fetch CSRs. Otherwise use the bootstrap
		// credentials. In the future it would be desirable to change the behavior of bootstrap
		// to always fall back to the external bootstrap credentials when such credentials are
		// provided by a fundamental trust system like cloud VM identity or an HSM module.
		config := certConfig
		if current != nil {
			config = clientConfig
		}
		return clientset.NewForConfig(config)
	}

	return kubeletcertificate.NewKubeletClientCertificateManager(
		certDir,
		nodeName,

		// this preserves backwards compatibility with kubeadm which passes
		// a high powered certificate to the kubelet as --kubeconfig and expects
		// it to be rotated out immediately
		clientConfig.CertData,
		clientConfig.KeyData,

		clientConfig.CertFile,
		clientConfig.KeyFile,
		newClientsetFn,
	)
}

func kubeClientConfigOverrides(s *options.KubeletServer, clientConfig *restclient.Config) {
	setContentTypeForClient(clientConfig, s.ContentType)
	// Override kubeconfig qps/burst settings from flags
	clientConfig.QPS = float32(s.KubeAPIQPS)
	clientConfig.Burst = int(s.KubeAPIBurst)
}

// getNodeName returns the node name according to the cloud provider
// if cloud provider is specified. Otherwise, returns the hostname of the node.
func getNodeName(cloud cloudprovider.Interface, hostname string) (types.NodeName, error) {
	if cloud == nil {
		return types.NodeName(hostname), nil
	}

	instances, ok := cloud.Instances()
	if !ok {
		return "", fmt.Errorf("failed to get instances from cloud provider")
	}

	nodeName, err := instances.CurrentNodeName(context.TODO(), hostname)
	if err != nil {
		return "", fmt.Errorf("error fetching current node name from cloud provider: %w", err)
	}

	klog.V(2).InfoS("Cloud provider determined current node", "nodeName", klog.KRef("", string(nodeName)))

	return nodeName, nil
}

// InitializeTLS checks for a configured TLSCertFile and TLSPrivateKeyFile: if unspecified a new self-signed
// certificate and key file are generated. Returns a configured server.TLSOptions object.
func InitializeTLS(kf *options.KubeletFlags, kc *kubeletconfiginternal.KubeletConfiguration) (*server.TLSOptions, error) {
	if !kc.ServerTLSBootstrap && kc.TLSCertFile == "" && kc.TLSPrivateKeyFile == "" {
		kc.TLSCertFile = filepath.Join(kf.CertDirectory, "kubelet.crt")
		kc.TLSPrivateKeyFile = filepath.Join(kf.CertDirectory, "kubelet.key")

		canReadCertAndKey, err := certutil.CanReadCertAndKey(kc.TLSCertFile, kc.TLSPrivateKeyFile)
		if err != nil {
			return nil, err
		}
		if !canReadCertAndKey {
			hostName, err := nodeutil.GetHostname(kf.HostnameOverride)
			if err != nil {
				return nil, err
			}
			cert, key, err := certutil.GenerateSelfSignedCertKey(hostName, nil, nil)
			if err != nil {
				return nil, fmt.Errorf("unable to generate self signed cert: %w", err)
			}

			if err := certutil.WriteCert(kc.TLSCertFile, cert); err != nil {
				return nil, err
			}

			if err := keyutil.WriteKey(kc.TLSPrivateKeyFile, key); err != nil {
				return nil, err
			}

			klog.V(4).InfoS("Using self-signed cert", "TLSCertFile", kc.TLSCertFile, "TLSPrivateKeyFile", kc.TLSPrivateKeyFile)
		}
	}

	tlsCipherSuites, err := cliflag.TLSCipherSuites(kc.TLSCipherSuites)
	if err != nil {
		return nil, err
	}

	if len(tlsCipherSuites) > 0 {
		insecureCiphers := cliflag.InsecureTLSCiphers()
		for i := 0; i < len(tlsCipherSuites); i++ {
			for cipherName, cipherID := range insecureCiphers {
				if tlsCipherSuites[i] == cipherID {
					klog.InfoS("Use of insecure cipher detected.", "cipher", cipherName)
				}
			}
		}
	}

	minTLSVersion, err := cliflag.TLSVersion(kc.TLSMinVersion)
	if err != nil {
		return nil, err
	}

	if minTLSVersion == tls.VersionTLS13 {
		if len(tlsCipherSuites) != 0 {
			klog.InfoS("Warning: TLS 1.3 cipher suites are not configurable, ignoring --tls-cipher-suites")
		}
	}

	tlsOptions := &server.TLSOptions{
		Config: &tls.Config{
			MinVersion:   minTLSVersion,
			CipherSuites: tlsCipherSuites,
		},
		CertFile: kc.TLSCertFile,
		KeyFile:  kc.TLSPrivateKeyFile,
	}

	if len(kc.Authentication.X509.ClientCAFile) > 0 {
		clientCAs, err := certutil.NewPool(kc.Authentication.X509.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("unable to load client CA file %s: %w", kc.Authentication.X509.ClientCAFile, err)
		}
		// Specify allowed CAs for client certificates
		tlsOptions.Config.ClientCAs = clientCAs
		// Populate PeerCertificates in requests, but don't reject connections without verified certificates
		tlsOptions.Config.ClientAuth = tls.RequestClientCert
	}

	return tlsOptions, nil
}

// setContentTypeForClient sets the appropriate content type into the rest config
// and handles defaulting AcceptContentTypes based on that input.
func setContentTypeForClient(cfg *restclient.Config, contentType string) {
	if len(contentType) == 0 {
		return
	}
	cfg.ContentType = contentType
	switch contentType {
	case runtime.ContentTypeProtobuf:
		cfg.AcceptContentTypes = strings.Join([]string{runtime.ContentTypeProtobuf, runtime.ContentTypeJSON}, ",")
	default:
		// otherwise let the rest client perform defaulting
	}
}

// RunKubelet is responsible for setting up and running a kubelet.  It is used in three different applications:
//
//	1 Integration tests
//	2 Kubelet binary
//	3 Standalone 'kubernetes' binary
//
// Eventually, #2 will be replaced with instances of #3
func RunKubelet(ctx context.Context, kubeServer *options.KubeletServer, kubeDeps *kubelet.Dependencies) error {
	hostname, err := nodeutil.GetHostname(kubeServer.HostnameOverride)
	if err != nil {
		return err
	}
	// Query the cloud provider for our node name, default to hostname if kubeDeps.Cloud == nil
	nodeName, err := getNodeName(kubeDeps.Cloud, hostname)
	if err != nil {
		return err
	}
	hostnameOverridden := len(kubeServer.HostnameOverride) > 0
	// Setup event recorder if required.
	makeEventRecorder(ctx, kubeDeps, nodeName)

	nodeIPs, invalidNodeIps, err := nodeutil.ParseNodeIPArgument(kubeServer.NodeIP, kubeServer.CloudProvider)
	if err != nil {
		return fmt.Errorf("bad --node-ip %q: %v", kubeServer.NodeIP, err)
	}
	if len(invalidNodeIps) != 0 {
		klog.FromContext(ctx).Info("Could not parse some node IP(s), ignoring them", "IPs", invalidNodeIps)
	}

	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: true,
	})

	credentialprovider.SetPreferredDockercfgPath(kubeServer.RootDirectory)
	klog.V(2).InfoS("Using root directory", "path", kubeServer.RootDirectory)

	if kubeDeps.OSInterface == nil {
		kubeDeps.OSInterface = kubecontainer.RealOS{}
	}

	k, err := createAndInitKubelet(kubeServer,
		kubeDeps,
		hostname,
		hostnameOverridden,
		nodeName,
		nodeIPs)
	if err != nil {
		return fmt.Errorf("failed to create kubelet: %w", err)
	}

	// NewMainKubelet should have set up a pod source config if one didn't exist
	// when the builder was run. This is just a precaution.
	if kubeDeps.PodConfig == nil {
		return fmt.Errorf("failed to create kubelet, pod source config was nil")
	}
	podCfg := kubeDeps.PodConfig

	if err := rlimit.SetNumFiles(uint64(kubeServer.MaxOpenFiles)); err != nil {
		klog.ErrorS(err, "Failed to set rlimit on max file handles")
	}

	startKubelet(k, podCfg, &kubeServer.KubeletConfiguration, kubeDeps, kubeServer.EnableServer)
	klog.InfoS("Started kubelet")

	return nil
}

func startKubelet(k kubelet.Bootstrap, podCfg *config.PodConfig, kubeCfg *kubeletconfiginternal.KubeletConfiguration, kubeDeps *kubelet.Dependencies, enableServer bool) {
	// start the kubelet
	go k.Run(podCfg.Updates())

	// start the kubelet server
	if enableServer {
		go k.ListenAndServe(kubeCfg, kubeDeps.TLSOptions, kubeDeps.Auth, kubeDeps.TracerProvider)
	}
	if kubeCfg.ReadOnlyPort > 0 {
		go k.ListenAndServeReadOnly(netutils.ParseIPSloppy(kubeCfg.Address), uint(kubeCfg.ReadOnlyPort), kubeDeps.TracerProvider)
	}
	go k.ListenAndServePodResources()
}

func createAndInitKubelet(kubeServer *options.KubeletServer,
	kubeDeps *kubelet.Dependencies,
	hostname string,
	hostnameOverridden bool,
	nodeName types.NodeName,
	nodeIPs []net.IP) (k kubelet.Bootstrap, err error) {
	// TODO: block until all sources have delivered at least one update to the channel, or break the sync loop
	// up into "per source" synchronizations

	k, err = kubelet.NewMainKubelet(&kubeServer.KubeletConfiguration,
		kubeDeps,
		&kubeServer.ContainerRuntimeOptions,
		hostname,
		hostnameOverridden,
		nodeName,
		nodeIPs,
		kubeServer.ProviderID,
		kubeServer.CloudProvider,
		kubeServer.CertDirectory,
		kubeServer.RootDirectory,
		kubeServer.PodLogsDir,
		kubeServer.ImageCredentialProviderConfigFile,
		kubeServer.ImageCredentialProviderBinDir,
		kubeServer.RegisterNode,
		kubeServer.RegisterWithTaints,
		kubeServer.AllowedUnsafeSysctls,
		kubeServer.ExperimentalMounterPath,
		kubeServer.KernelMemcgNotification,
		kubeServer.ExperimentalNodeAllocatableIgnoreEvictionThreshold,
		kubeServer.MinimumGCAge,
		kubeServer.MaxPerPodContainerCount,
		kubeServer.MaxContainerCount,
		kubeServer.RegisterSchedulable,
		kubeServer.NodeLabels,
		kubeServer.NodeStatusMaxImages,
		kubeServer.KubeletFlags.SeccompDefault || kubeServer.KubeletConfiguration.SeccompDefault)
	if err != nil {
		return nil, err
	}

	k.BirthCry()

	k.StartGarbageCollection()

	return k, nil
}

// parseResourceList parses the given configuration map into an API
// ResourceList or returns an error.
func parseResourceList(m map[string]string) (v1.ResourceList, error) {
	if len(m) == 0 {
		return nil, nil
	}
	rl := make(v1.ResourceList)
	for k, v := range m {
		switch v1.ResourceName(k) {
		// CPU, memory, local storage, and PID resources are supported.
		case v1.ResourceCPU, v1.ResourceMemory, v1.ResourceEphemeralStorage, pidlimit.PIDs:
			q, err := resource.ParseQuantity(v)
			if err != nil {
				return nil, fmt.Errorf("failed to parse quantity %q for %q resource: %w", v, k, err)
			}
			if q.Sign() == -1 {
				return nil, fmt.Errorf("resource quantity for %q cannot be negative: %v", k, v)
			}
			rl[v1.ResourceName(k)] = q
		default:
			return nil, fmt.Errorf("cannot reserve %q resource", k)
		}
	}
	return rl, nil
}

func newTracerProvider(s *options.KubeletServer) (oteltrace.TracerProvider, error) {
	if s.KubeletConfiguration.Tracing == nil {
		return noopoteltrace.NewTracerProvider(), nil
	}
	hostname, err := nodeutil.GetHostname(s.HostnameOverride)
	if err != nil {
		return nil, fmt.Errorf("could not determine hostname for tracer provider: %w", err)
	}
	resourceOpts := []otelsdkresource.Option{
		otelsdkresource.WithAttributes(
			semconv.ServiceNameKey.String(server.ComponentKubelet),
			semconv.HostNameKey.String(hostname),
		),
	}
	tp, err := tracing.NewProvider(context.Background(), s.KubeletConfiguration.Tracing, []otlptracegrpc.Option{}, resourceOpts)
	if err != nil {
		return nil, fmt.Errorf("could not configure tracer provider: %w", err)
	}
	return tp, nil
}

func getCgroupDriverFromCRI(ctx context.Context, s *options.KubeletServer, kubeDeps *kubelet.Dependencies) error {
	klog.V(4).InfoS("Getting CRI runtime configuration information")

	var (
		runtimeConfig *runtimeapi.RuntimeConfigResponse
		err           error
	)
	// Retry a couple of times, hoping that any errors are transient.
	// Fail quickly on known, non transient errors.
	for i := 0; i < 3; i++ {
		runtimeConfig, err = kubeDeps.RemoteRuntimeService.RuntimeConfig(ctx)
		if err != nil {
			s, ok := status.FromError(err)
			if !ok || s.Code() != codes.Unimplemented {
				// We could introduce a backoff delay or jitter, but this is largely catching cases
				// where the runtime is still starting up and we request too early.
				// Give it a little more time.
				time.Sleep(time.Second * 2)
				continue
			}
			// CRI implementation doesn't support RuntimeConfig, fallback
			klog.InfoS("CRI implementation should be updated to support RuntimeConfig when KubeletCgroupDriverFromCRI feature gate has been enabled. Falling back to using cgroupDriver from kubelet config.")
			return nil
		}
	}
	if err != nil {
		return err
	}

	// Calling GetLinux().GetCgroupDriver() won't segfault, but it will always default to systemd
	// which is not intended by the fields not being populated
	linuxConfig := runtimeConfig.GetLinux()
	if linuxConfig == nil {
		return nil
	}

	switch d := linuxConfig.GetCgroupDriver(); d {
	case runtimeapi.CgroupDriver_SYSTEMD:
		s.CgroupDriver = "systemd"
	case runtimeapi.CgroupDriver_CGROUPFS:
		s.CgroupDriver = "cgroupfs"
	default:
		return fmt.Errorf("runtime returned an unknown cgroup driver %d", d)
	}
	klog.InfoS("Using cgroup driver setting received from the CRI runtime", "cgroupDriver", s.CgroupDriver)
	return nil
}
