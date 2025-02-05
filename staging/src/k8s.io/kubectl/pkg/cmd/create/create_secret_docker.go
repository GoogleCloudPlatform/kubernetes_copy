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

package create

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/hash"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/utils/ptr"
)

var (
	secretForDockerRegistryLong = templates.LongDesc(i18n.T(`
		Create a new secret for use with Docker registries.

		Dockercfg secrets are used to authenticate against Docker registries.

		When using the Docker command line to push images, you can authenticate to a given registry by running:
			'$ docker login DOCKER_REGISTRY_SERVER --username=DOCKER_USER --password=DOCKER_PASSWORD --email=DOCKER_EMAIL'.

	That produces a ~/.dockercfg file that is used by subsequent 'docker push' and 'docker pull' commands to
		authenticate to the registry. The email address is optional.

		When creating applications, you may have a Docker registry that requires authentication.  In order for the
		nodes to pull images on your behalf, they must have the credentials.  You can provide this information
		by creating a dockercfg secret and attaching it to your service account.`))

	secretForDockerRegistryExample = templates.Examples(i18n.T(`
		  # If you do not already have a .dockercfg file, create a dockercfg secret directly
		  kubectl create secret docker-registry my-secret --docker-server=DOCKER_REGISTRY_SERVER --docker-username=DOCKER_USER --docker-password=DOCKER_PASSWORD --docker-email=DOCKER_EMAIL

		  # Create a new secret named my-secret from ~/.docker/config.json
		  kubectl create secret docker-registry my-secret --from-file=path/to/.docker/config.json`))
)

// DockerConfigJSON represents a local docker auth config file
// for pulling images.
type DockerConfigJSON struct {
	Auths DockerConfig `json:"auths" datapolicy:"token"`
	// +optional
	HttpHeaders map[string]string `json:"HttpHeaders,omitempty" datapolicy:"token"`
}

// DockerConfig represents the config file used by the docker CLI.
// This config that represents the credentials that should be used
// when pulling images from specific image repositories.
type DockerConfig map[string]DockerConfigEntry

// DockerConfigEntry holds the user information that grant the access to docker registry
type DockerConfigEntry struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty" datapolicy:"password"`
	Email    string `json:"email,omitempty"`
	Auth     string `json:"auth,omitempty" datapolicy:"token"`
}

// CreateSecretDockerRegistryOptions holds the options for 'create secret docker-registry' sub command
type CreateSecretDockerRegistryOptions struct {
	// PrintFlags holds options necessary for obtaining a printer
	PrintFlags *genericclioptions.PrintFlags
	PrintObj   func(obj runtime.Object) error

	// Name of secret (required)
	Name string
	// FileSources to derive the secret from (optional)
	FileSources []string
	// Username for registry (required)
	Username string
	// Email for registry (optional)
	Email string
	// Password for registry (required)
	Password string `datapolicy:"password"`
	// Server for registry (required)
	Server string
	// AppendHash; if true, derive a hash from the Secret and append it to the name
	AppendHash bool
	// Immutable; if true, the Secret will be immutable (optional)
	Immutable bool

	FieldManager     string
	CreateAnnotation bool
	Namespace        string
	EnforceNamespace bool

	Client              corev1client.CoreV1Interface
	DryRunStrategy      cmdutil.DryRunStrategy
	ValidationDirective string

	genericiooptions.IOStreams
}

// NewSecretDockerRegistryOptions creates a new *CreateSecretDockerRegistryOptions with default value
func NewSecretDockerRegistryOptions(ioStreams genericiooptions.IOStreams) *CreateSecretDockerRegistryOptions {
	return &CreateSecretDockerRegistryOptions{
		Server:     "https://index.docker.io/v1/",
		PrintFlags: genericclioptions.NewPrintFlags("created").WithTypeSetter(scheme.Scheme),
		IOStreams:  ioStreams,
	}
}

// NewCmdCreateSecretDockerRegistry is a macro command for creating secrets to work with Docker registries
func NewCmdCreateSecretDockerRegistry(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewSecretDockerRegistryOptions(ioStreams)

	cmd := &cobra.Command{
		Use:                   "docker-registry NAME --docker-username=user --docker-password=password --docker-email=email [--docker-server=string] [--from-file=[key=]source] [--dry-run=server|client|none]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Create a secret for use with a Docker registry"),
		Long:                  secretForDockerRegistryLong,
		Example:               secretForDockerRegistryExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run())
		},
	}

	o.PrintFlags.AddFlags(cmd)

	cmdutil.AddApplyAnnotationFlags(cmd)
	cmdutil.AddValidateFlags(cmd)
	cmdutil.AddDryRunFlag(cmd)

	cmd.Flags().StringVar(&o.Username, "docker-username", o.Username, i18n.T("Username for Docker registry authentication"))
	cmd.Flags().StringVar(&o.Password, "docker-password", o.Password, i18n.T("Password for Docker registry authentication"))
	cmd.Flags().StringVar(&o.Email, "docker-email", o.Email, i18n.T("Email for Docker registry"))
	cmd.Flags().StringVar(&o.Server, "docker-server", o.Server, i18n.T("Server location for Docker registry"))
	cmd.Flags().BoolVar(&o.AppendHash, "append-hash", o.AppendHash, "Append a hash of the secret to its name.")
	cmd.Flags().BoolVar(&o.Immutable, "immutable", o.Immutable, "Create immutable secret.")
	cmd.Flags().StringSliceVar(&o.FileSources, "from-file", o.FileSources, "Key files can be specified using their file path, "+
		"in which case a default name of "+corev1.DockerConfigJsonKey+" will be given to them, "+
		"or optionally with a name and file path, in which case the given name will be used. "+
		"Specifying a directory will iterate each named file in the directory that is a valid secret key. "+
		"For this command, the key should always be "+corev1.DockerConfigJsonKey+".")

	cmdutil.AddFieldManagerFlagVar(cmd, &o.FieldManager, "kubectl-create")

	return cmd
}

// Complete loads data from the command line environment
func (o *CreateSecretDockerRegistryOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error
	o.Name, err = NameFromCommandArgs(cmd, args)
	if err != nil {
		return err
	}

	restConfig, err := f.ToRESTConfig()
	if err != nil {
		return err
	}

	o.Client, err = corev1client.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	o.CreateAnnotation = cmdutil.GetFlagBool(cmd, cmdutil.ApplyAnnotationsFlag)

	o.DryRunStrategy, err = cmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return err
	}

	o.Namespace, o.EnforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	cmdutil.PrintFlagsWithDryRunStrategy(o.PrintFlags, o.DryRunStrategy)
	printer, err := o.PrintFlags.ToPrinter()
	if err != nil {
		return err
	}

	o.PrintObj = func(obj runtime.Object) error {
		return printer.PrintObj(obj, o.Out)
	}

	o.ValidationDirective, err = cmdutil.GetValidationDirective(cmd)
	if err != nil {
		return err
	}

	for i := range o.FileSources {
		if !strings.Contains(o.FileSources[i], "=") {
			o.FileSources[i] = corev1.DockerConfigJsonKey + "=" + o.FileSources[i]
		}
	}
	return nil
}

// Validate checks if CreateSecretDockerRegistryOptions has sufficient value to run
func (o *CreateSecretDockerRegistryOptions) Validate() error {
	if len(o.Name) == 0 {
		return fmt.Errorf("name must be specified")
	}
	if len(o.FileSources) == 0 && (len(o.Username) == 0 || len(o.Password) == 0 || len(o.Server) == 0) {
		return fmt.Errorf("either --from-file or the combination of --docker-username, --docker-password and --docker-server is required")
	}
	return nil
}

// Run calls createSecretDockerRegistry which will create secretDockerRegistry based on CreateSecretDockerRegistryOptions
// and makes an API call to the server
func (o *CreateSecretDockerRegistryOptions) Run() error {
	secretDockerRegistry, err := o.createSecretDockerRegistry()
	if err != nil {
		return err
	}
	err = util.CreateOrUpdateAnnotation(o.CreateAnnotation, secretDockerRegistry, scheme.DefaultJSONEncoder())
	if err != nil {
		return err
	}
	if o.DryRunStrategy != cmdutil.DryRunClient {
		createOptions := metav1.CreateOptions{}
		if o.FieldManager != "" {
			createOptions.FieldManager = o.FieldManager
		}
		createOptions.FieldValidation = o.ValidationDirective
		if o.DryRunStrategy == cmdutil.DryRunServer {
			createOptions.DryRun = []string{metav1.DryRunAll}
		}
		secretDockerRegistry, err = o.Client.Secrets(o.Namespace).Create(context.TODO(), secretDockerRegistry, createOptions)
		if err != nil {
			return fmt.Errorf("failed to create secret %v", err)
		}
	}

	return o.PrintObj(secretDockerRegistry)
}

// createSecretDockerRegistry fills in key value pair from the information given in
// CreateSecretDockerRegistryOptions into *corev1.Secret
func (o *CreateSecretDockerRegistryOptions) createSecretDockerRegistry() (*corev1.Secret, error) {
	namespace := ""
	if o.EnforceNamespace {
		namespace = o.Namespace
	}
	secretDockerRegistry := newSecretObj(o.Name, namespace, corev1.SecretTypeDockerConfigJson)
	if len(o.FileSources) > 0 {
		if err := handleSecretFromFileSources(secretDockerRegistry, o.FileSources); err != nil {
			return nil, err
		}
	} else {
		dockerConfigJSONContent, err := handleDockerCfgJSONContent(o.Username, o.Password, o.Email, o.Server)
		if err != nil {
			return nil, err
		}
		secretDockerRegistry.Data[corev1.DockerConfigJsonKey] = dockerConfigJSONContent
	}
	if o.AppendHash {
		hash, err := hash.SecretHash(secretDockerRegistry)
		if err != nil {
			return nil, err
		}
		secretDockerRegistry.Name = fmt.Sprintf("%s-%s", secretDockerRegistry.Name, hash)
	}
	if o.Immutable {
		secretDockerRegistry.Immutable = ptr.To[bool](true)
	}
	return secretDockerRegistry, nil
}

// handleDockerCfgJSONContent serializes a ~/.docker/config.json file
func handleDockerCfgJSONContent(username, password, email, server string) ([]byte, error) {
	dockerConfigAuth := DockerConfigEntry{
		Username: username,
		Password: password,
		Email:    email,
		Auth:     encodeDockerConfigFieldAuth(username, password),
	}
	dockerConfigJSON := DockerConfigJSON{
		Auths: map[string]DockerConfigEntry{server: dockerConfigAuth},
	}

	return json.Marshal(dockerConfigJSON)
}

// encodeDockerConfigFieldAuth returns base64 encoding of the username and password string
func encodeDockerConfigFieldAuth(username, password string) string {
	fieldValue := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(fieldValue))
}
