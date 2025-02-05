/*
Copyright 2019 The Kubernetes Authors.

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

package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/lithammer/dedent"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmscheme "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/scheme"
	kubeadmapiv1 "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/v1beta4"
	"k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/validation"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	phases "k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/join"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	cmdutil "k8s.io/kubernetes/cmd/kubeadm/app/cmd/util"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"k8s.io/kubernetes/cmd/kubeadm/app/discovery"
	"k8s.io/kubernetes/cmd/kubeadm/app/discovery/token"
	"k8s.io/kubernetes/cmd/kubeadm/app/util/apiclient"
	configutil "k8s.io/kubernetes/cmd/kubeadm/app/util/config"
	kubeconfigutil "k8s.io/kubernetes/cmd/kubeadm/app/util/kubeconfig"
)

var (
	joinWorkerNodeDoneMsg = dedent.Dedent(`
		This node has joined the cluster:
		* Certificate signing request was sent to apiserver and a response was received.
		* The Kubelet was informed of the new secure connection details.

		Run 'kubectl get nodes' on the control-plane to see this node join the cluster.

		`)

	joinControlPlaneDoneTemp = template.Must(template.New("join").Parse(dedent.Dedent(`
		This node has joined the cluster and a new control plane instance was created:

		* Certificate signing request was sent to apiserver and approval was received.
		* The Kubelet was informed of the new secure connection details.
		* Control plane label and taint were applied to the new node.
		* The Kubernetes control plane instances scaled up.
		{{.etcdMessage}}

		To start administering your cluster from this node, you need to run the following as a regular user:

			mkdir -p $HOME/.kube
			sudo cp -i {{.KubeConfigPath}} $HOME/.kube/config
			sudo chown $(id -u):$(id -g) $HOME/.kube/config

		Run 'kubectl get nodes' to see this node join the cluster.

		`)))

	joinLongDescription = dedent.Dedent(`
		When joining a kubeadm initialized cluster, we need to establish
		bidirectional trust. This is split into discovery (having the Node
		trust the Kubernetes Control Plane) and TLS bootstrap (having the
		Kubernetes Control Plane trust the Node).

		There are 2 main schemes for discovery. The first is to use a shared
		token along with the IP address of the API server. The second is to
		provide a file - a subset of the standard kubeconfig file. The
		discovery/kubeconfig file supports token, client-go authentication
		plugins ("exec"), "tokenFile", and "authProvider". This file can be a
		local file or downloaded via an HTTPS URL. The forms are
		kubeadm join --discovery-token abcdef.1234567890abcdef 1.2.3.4:6443,
		kubeadm join --discovery-file path/to/file.conf, or kubeadm join
		--discovery-file https://url/file.conf. Only one form can be used. If
		the discovery information is loaded from a URL, HTTPS must be used.
		Also, in that case the host installed CA bundle is used to verify
		the connection.

		If you use a shared token for discovery, you should also pass the
		--discovery-token-ca-cert-hash flag to validate the public key of the
		root certificate authority (CA) presented by the Kubernetes Control Plane.
		The value of this flag is specified as "<hash-type>:<hex-encoded-value>",
		where the supported hash type is "sha256". The hash is calculated over
		the bytes of the Subject Public Key Info (SPKI) object (as in RFC7469).
		This value is available in the output of "kubeadm init" or can be
		calculated using standard tools. The --discovery-token-ca-cert-hash flag
		may be repeated multiple times to allow more than one public key.

		If you cannot know the CA public key hash ahead of time, you can pass
		the --discovery-token-unsafe-skip-ca-verification flag to disable this
		verification. This weakens the kubeadm security model since other nodes
		can potentially impersonate the Kubernetes Control Plane.

		The TLS bootstrap mechanism is also driven via a shared token. This is
		used to temporarily authenticate with the Kubernetes Control Plane to submit a
		certificate signing request (CSR) for a locally created key pair. By
		default, kubeadm will set up the Kubernetes Control Plane to automatically
		approve these signing requests. This token is passed in with the
		--tls-bootstrap-token abcdef.1234567890abcdef flag.

		Often times the same token is used for both parts. In this case, the
		--token flag can be used instead of specifying each token individually.
		`)
)

// joinOptions defines all the options exposed via flags by kubeadm join.
// Please note that this structure includes the public kubeadm config API, but only a subset of the options
// supported by this api will be exposed as a flag.
type joinOptions struct {
	cfgPath               string
	token                 string `datapolicy:"token"`
	controlPlane          bool
	ignorePreflightErrors []string
	externalcfg           *kubeadmapiv1.JoinConfiguration
	patchesDir            string
	dryRun                bool
	skipCRIDetect         bool
}

// compile-time assert that the local data object satisfies the phases data interface.
var _ phases.JoinData = &joinData{}

// joinData defines all the runtime information used when running the kubeadm join workflow;
// this data is shared across all the phases that are included in the workflow.
type joinData struct {
	cfg                   *kubeadmapi.JoinConfiguration
	initCfg               *kubeadmapi.InitConfiguration
	tlsBootstrapCfg       *clientcmdapi.Config
	client                clientset.Interface
	ignorePreflightErrors sets.Set[string]
	outputWriter          io.Writer
	patchesDir            string
	dryRun                bool
	dryRunDir             string
}

// newCmdJoin returns "kubeadm join" command.
// NB. joinOptions is exposed as parameter for allowing unit testing of
// the newJoinData method, that implements all the command options validation logic
func newCmdJoin(out io.Writer, joinOptions *joinOptions) *cobra.Command {
	if joinOptions == nil {
		joinOptions = newJoinOptions()
	}
	joinRunner := workflow.NewRunner()

	cmd := &cobra.Command{
		Use:   "join [api-server-endpoint]",
		Short: "Run this on any machine you wish to join an existing cluster",
		Long:  joinLongDescription,
		RunE: func(cmd *cobra.Command, args []string) error {

			c, err := joinRunner.InitData(args)
			if err != nil {
				return err
			}

			data, ok := c.(*joinData)
			if !ok {
				return errors.New("invalid data struct")
			}

			if err := joinRunner.Run(args); err != nil {
				return err
			}

			// if the node is hosting a new control plane instance
			if data.cfg.ControlPlane != nil {
				// outputs the join control plane done message and exit
				etcdMessage := ""
				if data.initCfg.Etcd.External == nil {
					etcdMessage = "* A new etcd member was added to the local/stacked etcd cluster."
				}

				ctx := map[string]string{
					"KubeConfigPath": kubeadmconstants.GetAdminKubeConfigPath(),
					"etcdMessage":    etcdMessage,
				}
				if err := joinControlPlaneDoneTemp.Execute(data.outputWriter, ctx); err != nil {
					return err
				}

			} else {
				// otherwise, if the node joined as a worker node;
				// outputs the join done message and exit
				fmt.Fprint(data.outputWriter, joinWorkerNodeDoneMsg)
			}

			return nil
		},
		// We accept the control-plane location as an optional positional argument
		Args: cobra.MaximumNArgs(1),
	}

	addJoinConfigFlags(cmd.Flags(), joinOptions.externalcfg)
	addJoinOtherFlags(cmd.Flags(), joinOptions)

	joinRunner.AppendPhase(phases.NewPreflightPhase())
	joinRunner.AppendPhase(phases.NewControlPlanePreparePhase())
	joinRunner.AppendPhase(phases.NewCheckEtcdPhase())
	joinRunner.AppendPhase(phases.NewKubeletStartPhase())
	joinRunner.AppendPhase(phases.NewEtcdJoinPhase())
	joinRunner.AppendPhase(phases.NewKubeletWaitBootstrapPhase())
	joinRunner.AppendPhase(phases.NewControlPlaneJoinPhase())
	joinRunner.AppendPhase(phases.NewWaitControlPlanePhase())

	// sets the data builder function, that will be used by the runner
	// both when running the entire workflow or single phases
	joinRunner.SetDataInitializer(func(cmd *cobra.Command, args []string) (workflow.RunData, error) {
		if cmd.Flags().Lookup(options.NodeCRISocket) == nil {
			// skip CRI detection
			// assume that the command execution does not depend on CRISocket when --cri-socket flag is not set
			joinOptions.skipCRIDetect = true
		}
		data, err := newJoinData(cmd, args, joinOptions, out, kubeadmconstants.GetAdminKubeConfigPath())
		if err != nil {
			return nil, err
		}
		// If the flag for skipping phases was empty, use the values from config
		if len(joinRunner.Options.SkipPhases) == 0 {
			joinRunner.Options.SkipPhases = data.cfg.SkipPhases
		}
		return data, nil
	})

	// binds the Runner to kubeadm join command by altering
	// command help, adding --skip-phases flag and by adding phases subcommands
	joinRunner.BindToCommand(cmd)

	return cmd
}

// addJoinConfigFlags adds join flags bound to the config to the specified flagset
func addJoinConfigFlags(flagSet *flag.FlagSet, cfg *kubeadmapiv1.JoinConfiguration) {
	flagSet.StringVar(
		&cfg.NodeRegistration.Name, options.NodeName, cfg.NodeRegistration.Name,
		`Specify the node name.`,
	)
	flagSet.StringVar(
		&cfg.ControlPlane.CertificateKey, options.CertificateKey, cfg.ControlPlane.CertificateKey,
		"Use this key to decrypt the certificate secrets uploaded by init. The certificate key is a hex encoded string that is an AES key of size 32 bytes.",
	)
	// add control plane endpoint flags to the specified flagset
	flagSet.StringVar(
		&cfg.ControlPlane.LocalAPIEndpoint.AdvertiseAddress, options.APIServerAdvertiseAddress, cfg.ControlPlane.LocalAPIEndpoint.AdvertiseAddress,
		"If the node should host a new control plane instance, the IP address the API Server will advertise it's listening on. If not set the default network interface will be used.",
	)
	flagSet.Int32Var(
		&cfg.ControlPlane.LocalAPIEndpoint.BindPort, options.APIServerBindPort, cfg.ControlPlane.LocalAPIEndpoint.BindPort,
		"If the node should host a new control plane instance, the port for the API Server to bind to.",
	)
	// adds bootstrap token specific discovery flags to the specified flagset
	flagSet.StringVar(
		&cfg.Discovery.BootstrapToken.Token, options.TokenDiscovery, "",
		"For token-based discovery, the token used to validate cluster information fetched from the API server.",
	)
	flagSet.StringSliceVar(
		&cfg.Discovery.BootstrapToken.CACertHashes, options.TokenDiscoveryCAHash, []string{},
		"For token-based discovery, validate that the root CA public key matches this hash (format: \"<type>:<value>\").",
	)
	flagSet.BoolVar(
		&cfg.Discovery.BootstrapToken.UnsafeSkipCAVerification, options.TokenDiscoverySkipCAHash, false,
		"For token-based discovery, allow joining without --discovery-token-ca-cert-hash pinning.",
	)
	//	discovery via kube config file flag
	flagSet.StringVar(
		&cfg.Discovery.File.KubeConfigPath, options.FileDiscovery, "",
		"For file-based discovery, a file or URL from which to load cluster information.",
	)
	flagSet.StringVar(
		&cfg.Discovery.TLSBootstrapToken, options.TLSBootstrapToken, cfg.Discovery.TLSBootstrapToken,
		`Specify the token used to temporarily authenticate with the Kubernetes Control Plane while joining the node.`,
	)
	cmdutil.AddCRISocketFlag(flagSet, &cfg.NodeRegistration.CRISocket)
}

// addJoinOtherFlags adds join flags that are not bound to a configuration file to the given flagset
func addJoinOtherFlags(flagSet *flag.FlagSet, joinOptions *joinOptions) {
	options.AddConfigFlag(flagSet, &joinOptions.cfgPath)
	flagSet.StringSliceVar(
		&joinOptions.ignorePreflightErrors, options.IgnorePreflightErrors, joinOptions.ignorePreflightErrors,
		"A list of checks whose errors will be shown as warnings. Example: 'IsPrivilegedUser,Swap'. Value 'all' ignores errors from all checks.",
	)
	flagSet.StringVar(
		&joinOptions.token, options.TokenStr, "",
		"Use this token for both discovery-token and tls-bootstrap-token when those values are not provided.",
	)
	flagSet.BoolVar(
		&joinOptions.controlPlane, options.ControlPlane, joinOptions.controlPlane,
		"Create a new control plane instance on this node",
	)
	flagSet.BoolVar(
		&joinOptions.dryRun, options.DryRun, joinOptions.dryRun,
		"Don't apply any changes; just output what would be done.",
	)
	options.AddPatchesFlag(flagSet, &joinOptions.patchesDir)
}

// newJoinOptions returns a struct ready for being used for creating cmd join flags.
func newJoinOptions() *joinOptions {
	// initialize the public kubeadm config API by applying defaults
	externalcfg := &kubeadmapiv1.JoinConfiguration{}

	// Add optional config objects to host flags.
	// un-set objects will be cleaned up afterwards (into newJoinData func)
	externalcfg.Discovery.File = &kubeadmapiv1.FileDiscovery{}
	externalcfg.Discovery.BootstrapToken = &kubeadmapiv1.BootstrapTokenDiscovery{}
	externalcfg.ControlPlane = &kubeadmapiv1.JoinControlPlane{}

	// This object is used for storage of control-plane flags.
	joinControlPlane := &kubeadmapiv1.JoinControlPlane{}

	// Apply defaults
	kubeadmscheme.Scheme.Default(externalcfg)
	kubeadmapiv1.SetDefaults_JoinControlPlane(joinControlPlane)

	return &joinOptions{
		externalcfg: externalcfg,
	}
}

// newJoinData returns a new joinData struct to be used for the execution of the kubeadm join workflow.
// This func takes care of validating joinOptions passed to the command, and then it converts
// options into the internal JoinConfiguration type that is used as input all the phases in the kubeadm join workflow
func newJoinData(cmd *cobra.Command, args []string, opt *joinOptions, out io.Writer, adminKubeConfigPath string) (*joinData, error) {

	// Validate the mixed arguments with --config and return early on errors
	if err := validation.ValidateMixedArguments(cmd.Flags()); err != nil {
		return nil, err
	}

	// Re-apply defaults to the public kubeadm API (this will set only values not exposed/not set as a flags)
	kubeadmscheme.Scheme.Default(opt.externalcfg)

	// Validate standalone flags values and/or combination of flags and then assigns
	// validated values to the public kubeadm config API when applicable

	// if a token is provided, use this value for both discovery-token and tls-bootstrap-token when those values are not provided
	if len(opt.token) > 0 {
		if len(opt.externalcfg.Discovery.TLSBootstrapToken) == 0 {
			opt.externalcfg.Discovery.TLSBootstrapToken = opt.token
		}
		if len(opt.externalcfg.Discovery.BootstrapToken.Token) == 0 {
			opt.externalcfg.Discovery.BootstrapToken.Token = opt.token
		}
	}

	// if a file or URL from which to load cluster information was not provided, unset the Discovery.File object
	if len(opt.externalcfg.Discovery.File.KubeConfigPath) == 0 {
		opt.externalcfg.Discovery.File = nil
	}

	// if an APIServerEndpoint from which to retrieve cluster information was not provided, unset the Discovery.BootstrapToken object
	if len(args) == 0 {
		opt.externalcfg.Discovery.BootstrapToken = nil
	} else {
		if len(opt.cfgPath) == 0 && len(args) > 1 {
			klog.Warningf("[preflight] WARNING: More than one API server endpoint supplied on command line %v. Using the first one.", args)
		}
		opt.externalcfg.Discovery.BootstrapToken.APIServerEndpoint = args[0]
	}

	// If not passing --control-plane, unset the ControlPlane object
	if !opt.controlPlane {
		// Use a defaulted JoinControlPlane object to detect if the user has passed
		// other control-plane related flags.
		defaultJCP := &kubeadmapiv1.JoinControlPlane{}
		kubeadmapiv1.SetDefaults_JoinControlPlane(defaultJCP)

		// This list must match the JCP flags in addJoinConfigFlags()
		joinControlPlaneFlags := []string{
			options.CertificateKey,
			options.APIServerAdvertiseAddress,
			options.APIServerBindPort,
		}

		if *opt.externalcfg.ControlPlane != *defaultJCP {
			klog.Warningf("[preflight] WARNING: --%s is also required when passing control-plane "+
				"related flags such as [%s]", options.ControlPlane, strings.Join(joinControlPlaneFlags, ", "))
		}
		opt.externalcfg.ControlPlane = nil
	}

	// if the admin.conf file already exists, use it for skipping the discovery process.
	// NB. this case can happen when we are joining a control-plane node only (and phases are invoked atomically)
	var tlsBootstrapCfg *clientcmdapi.Config
	if _, err := os.Stat(adminKubeConfigPath); err == nil && opt.controlPlane {
		// use the admin.conf as tlsBootstrapCfg, that is the kubeconfig file used for reading the kubeadm-config during discovery
		klog.V(1).Infof("[preflight] found %s. Use it for skipping discovery", adminKubeConfigPath)
		tlsBootstrapCfg, err = clientcmd.LoadFromFile(adminKubeConfigPath)
		if err != nil {
			return nil, errors.Wrapf(err, "Error loading %s", adminKubeConfigPath)
		}
	}

	// Either use the config file if specified, or convert public kubeadm API to the internal JoinConfiguration
	// and validates JoinConfiguration
	if opt.externalcfg.NodeRegistration.Name == "" {
		klog.V(1).Infoln("[preflight] found NodeName empty; using OS hostname as NodeName")
	}

	if opt.externalcfg.ControlPlane != nil && opt.externalcfg.ControlPlane.LocalAPIEndpoint.AdvertiseAddress == "" {
		klog.V(1).Infoln("[preflight] found advertiseAddress empty; using default interface's IP address as advertiseAddress")
	}

	// in case the command doesn't have flags for discovery, makes the join cfg validation pass checks on discovery
	if cmd.Flags().Lookup(options.FileDiscovery) == nil {
		if _, err := os.Stat(adminKubeConfigPath); os.IsNotExist(err) {
			return nil, errors.Errorf("File %s does not exists. Please use 'kubeadm join phase control-plane-prepare' subcommands to generate it.", adminKubeConfigPath)
		}
		klog.V(1).Infof("[preflight] found discovery flags missing for this command. using FileDiscovery: %s", adminKubeConfigPath)
		opt.externalcfg.Discovery.File = &kubeadmapiv1.FileDiscovery{KubeConfigPath: adminKubeConfigPath}
		opt.externalcfg.Discovery.BootstrapToken = nil //NB. this could be removed when we get better control on args (e.g. phases without discovery should have NoArgs )
	}

	cfg, err := configutil.LoadOrDefaultJoinConfiguration(opt.cfgPath, opt.externalcfg, configutil.LoadOrDefaultConfigurationOptions{
		SkipCRIDetect: opt.skipCRIDetect,
	})
	if err != nil {
		return nil, err
	}

	ignorePreflightErrorsSet, err := validation.ValidateIgnorePreflightErrors(opt.ignorePreflightErrors, cfg.NodeRegistration.IgnorePreflightErrors)
	if err != nil {
		return nil, err
	}
	// Also set the union of pre-flight errors to JoinConfiguration, to provide a consistent view of the runtime configuration:
	cfg.NodeRegistration.IgnorePreflightErrors = sets.List(ignorePreflightErrorsSet)

	// override node name and CRI socket from the command line opt
	if opt.externalcfg.NodeRegistration.Name != "" {
		cfg.NodeRegistration.Name = opt.externalcfg.NodeRegistration.Name
	}
	if opt.externalcfg.NodeRegistration.CRISocket != "" {
		cfg.NodeRegistration.CRISocket = opt.externalcfg.NodeRegistration.CRISocket
	}

	if cfg.ControlPlane != nil {
		if err := configutil.VerifyAPIServerBindAddress(cfg.ControlPlane.LocalAPIEndpoint.AdvertiseAddress); err != nil {
			return nil, err
		}
	}

	// if dry running, creates a temporary folder to save kubeadm generated files
	dryRunDir := ""
	if opt.dryRun || cfg.DryRun {
		if dryRunDir, err = kubeadmconstants.GetDryRunDir(kubeadmconstants.EnvVarJoinDryRunDir, "kubeadm-join-dryrun", klog.Warningf); err != nil {
			return nil, errors.Wrap(err, "couldn't create a temporary directory on dryrun")
		}
	}

	return &joinData{
		cfg:                   cfg,
		dryRun:                cmdutil.ValueFromFlagsOrConfig(cmd.Flags(), options.DryRun, cfg.DryRun, opt.dryRun).(bool),
		tlsBootstrapCfg:       tlsBootstrapCfg,
		ignorePreflightErrors: ignorePreflightErrorsSet,
		outputWriter:          out,
		patchesDir:            opt.patchesDir,
		dryRunDir:             dryRunDir,
	}, nil
}

// CertificateKey returns the key used to encrypt the certs.
func (j *joinData) CertificateKey() string {
	if j.cfg.ControlPlane != nil {
		return j.cfg.ControlPlane.CertificateKey
	}
	return ""
}

// Cfg returns the JoinConfiguration.
func (j *joinData) Cfg() *kubeadmapi.JoinConfiguration {
	return j.cfg
}

// DryRun returns the DryRun flag.
func (j *joinData) DryRun() bool {
	return j.dryRun
}

// KubeConfigDir returns the path of the Kubernetes configuration folder or the temporary folder path in case of DryRun.
func (j *joinData) KubeConfigDir() string {
	if j.dryRun {
		return j.dryRunDir
	}
	return kubeadmconstants.KubernetesDir
}

// KubeletDir returns the path of the kubelet configuration folder or the temporary folder in case of DryRun.
func (j *joinData) KubeletDir() string {
	if j.dryRun {
		return j.dryRunDir
	}
	return kubeadmconstants.KubeletRunDirectory
}

// ManifestDir returns the path where manifest should be stored or the temporary folder path in case of DryRun.
func (j *joinData) ManifestDir() string {
	if j.dryRun {
		return j.dryRunDir
	}
	return kubeadmconstants.GetStaticPodDirectory()
}

// CertificateWriteDir returns the path where certs should be stored or the temporary folder path in case of DryRun.
func (j *joinData) CertificateWriteDir() string {
	if j.dryRun {
		return j.dryRunDir
	}
	return j.initCfg.CertificatesDir
}

// TLSBootstrapCfg returns the cluster-info (kubeconfig).
func (j *joinData) TLSBootstrapCfg() (*clientcmdapi.Config, error) {
	if j.tlsBootstrapCfg != nil {
		return j.tlsBootstrapCfg, nil
	}

	var (
		client clientset.Interface
		err    error
	)
	if j.dryRun {
		client, err = j.Client()
		if err != nil {
			return nil, errors.Wrap(err, "could not create a client for TLS bootstrap")
		}
	}
	klog.V(1).Infoln("[preflight] Discovering cluster-info")
	tlsBootstrapCfg, err := discovery.For(client, j.cfg)
	j.tlsBootstrapCfg = tlsBootstrapCfg
	return tlsBootstrapCfg, err
}

// InitCfg returns the InitConfiguration.
func (j *joinData) InitCfg() (*kubeadmapi.InitConfiguration, error) {
	if j.initCfg != nil {
		return j.initCfg, nil
	}
	if _, err := j.TLSBootstrapCfg(); err != nil {
		return nil, err
	}
	klog.V(1).Infoln("[preflight] Fetching init configuration")
	var client clientset.Interface
	if j.dryRun {
		var err error
		client, err = j.Client()
		if err != nil {
			return nil, errors.Wrap(err, "could not get dry-run client for fetching InitConfiguration")
		}
	}
	initCfg, err := fetchInitConfigurationFromJoinConfiguration(j.cfg, client, j.tlsBootstrapCfg)
	j.initCfg = initCfg
	return initCfg, err
}

// Client returns the Client for accessing the cluster with the identity defined in admin.conf.
func (j *joinData) Client() (clientset.Interface, error) {
	pathAdmin := filepath.Join(j.KubeConfigDir(), kubeadmconstants.AdminKubeConfigFileName)

	if j.dryRun {
		dryRun := apiclient.NewDryRun()
		// For the dynamic dry-run client use this kubeconfig only if it exists.
		// That would happen presumably after TLS bootstrap.
		if _, err := os.Stat(pathAdmin); err == nil {
			if err := dryRun.WithKubeConfigFile(pathAdmin); err != nil {
				return nil, err
			}
		} else if j.tlsBootstrapCfg != nil {
			if err := dryRun.WithKubeConfig(j.tlsBootstrapCfg); err != nil {
				return nil, err
			}
		} else if j.cfg.Discovery.BootstrapToken != nil {
			insecureConfig := token.BuildInsecureBootstrapKubeConfig(j.cfg.Discovery.BootstrapToken.APIServerEndpoint)
			resetConfig, err := clientcmd.NewDefaultClientConfig(*insecureConfig, &clientcmd.ConfigOverrides{}).ClientConfig()
			if err != nil {
				return nil, errors.Wrap(err, "failed to create API client configuration from kubeconfig")
			}
			if err := dryRun.WithRestConfig(resetConfig); err != nil {
				return nil, err
			}
		}

		dryRun.WithDefaultMarshalFunction().
			WithWriter(os.Stdout).
			AppendReactor(dryRun.GetClusterInfoReactor()).
			AppendReactor(dryRun.GetKubeadmConfigReactor()).
			AppendReactor(dryRun.GetKubeadmCertsReactor()).
			AppendReactor(dryRun.GetKubeProxyConfigReactor()).
			AppendReactor(dryRun.GetKubeletConfigReactor()).
			AppendReactor(dryRun.GetNodeReactor()).
			AppendReactor(dryRun.PatchNodeReactor())

		j.client = dryRun.FakeClient()
		return j.client, nil
	}

	client, err := kubeconfigutil.ClientSetFromFile(pathAdmin)
	if err != nil {
		return nil, errors.Wrap(err, "[preflight] couldn't create Kubernetes client")
	}
	j.client = client
	return client, nil
}

// IgnorePreflightErrors returns the list of preflight errors to ignore.
func (j *joinData) IgnorePreflightErrors() sets.Set[string] {
	return j.ignorePreflightErrors
}

// OutputWriter returns the io.Writer used to write messages such as the "join done" message.
func (j *joinData) OutputWriter() io.Writer {
	return j.outputWriter
}

// PatchesDir returns the folder where patches for components are stored
func (j *joinData) PatchesDir() string {
	// If provided, make the flag value override the one in config.
	if len(j.patchesDir) > 0 {
		return j.patchesDir
	}
	if j.cfg.Patches != nil {
		return j.cfg.Patches.Directory
	}
	return ""
}

// fetchInitConfigurationFromJoinConfiguration retrieves the init configuration from a join configuration, performing the discovery
func fetchInitConfigurationFromJoinConfiguration(cfg *kubeadmapi.JoinConfiguration, client clientset.Interface, tlsBootstrapCfg *clientcmdapi.Config) (*kubeadmapi.InitConfiguration, error) {
	var err error

	klog.V(1).Infoln("[preflight] Retrieving KubeConfig objects")
	if client == nil {
		// creates a client to access the cluster using the bootstrap token identity
		client, err = kubeconfigutil.ToClientSet(tlsBootstrapCfg)
		if err != nil {
			return nil, errors.Wrap(err, "unable to access the cluster")
		}
	}
	initConfiguration, err := fetchInitConfiguration(client)
	if err != nil {
		return nil, err
	}

	// Create the final KubeConfig file with the cluster name discovered after fetching the cluster configuration
	_, clusterinfo := kubeconfigutil.GetClusterFromKubeConfig(tlsBootstrapCfg)
	tlsBootstrapCfg.Clusters = map[string]*clientcmdapi.Cluster{
		initConfiguration.ClusterName: clusterinfo,
	}
	tlsBootstrapCfg.Contexts[tlsBootstrapCfg.CurrentContext].Cluster = initConfiguration.ClusterName

	// injects into the kubeadm configuration the information about the joining node
	initConfiguration.NodeRegistration = cfg.NodeRegistration
	if cfg.ControlPlane != nil {
		initConfiguration.LocalAPIEndpoint = cfg.ControlPlane.LocalAPIEndpoint
	}

	return initConfiguration, nil
}

// fetchInitConfiguration reads the cluster configuration from the kubeadm-admin configMap
func fetchInitConfiguration(client clientset.Interface) (*kubeadmapi.InitConfiguration, error) {
	initConfiguration, err := configutil.FetchInitConfigurationFromCluster(client, nil, "preflight", true, false)
	if err != nil {
		return nil, errors.Wrap(err, "unable to fetch the kubeadm-config ConfigMap")
	}

	return initConfiguration, nil
}
