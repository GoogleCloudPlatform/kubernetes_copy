/*
Copyright 2015 The Kubernetes Authors.

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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/hash"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

// NewCmdCreateSecret groups subcommands to create various types of secrets.
// This is the entry point of create_secret.go which will be called by create.go
func NewCmdCreateSecret(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "secret (docker-registry | generic | tls)",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Create a secret using a specified subcommand"),
		Long:                  secretLong,
		Run:                   cmdutil.DefaultSubCommandRun(ioStreams.ErrOut),
	}
	cmd.AddCommand(NewCmdCreateSecretDockerRegistry(f, ioStreams))
	cmd.AddCommand(NewCmdCreateSecretTLS(f, ioStreams))
	cmd.AddCommand(NewCmdCreateSecretGeneric(f, ioStreams))

	return cmd
}

var (
	secretLong = templates.LongDesc(i18n.T(`
		Create a secret with specified type.
		
		A docker-registry type secret is for accessing a container registry.

		A generic type secret indicate an Opaque secret type.

		A tls type secret holds TLS certificate and its associated key.`))

	secretForGenericLong = templates.LongDesc(i18n.T(`
		Create a secret based on a file, directory, or specified literal value.
	
		The default type for a secret is Opaque unless otherwise specified by the --type flag.

		A single secret may package one or more key/value pairs.

		When creating a secret based on a file, the key will default to the basename of the file, and the value will
		default to the file content. If the basename is an invalid key or you wish to chose your own, you may specify
		an alternate key.

		When creating a secret based on a directory, each file whose basename is a valid key in the directory will be
		packaged into the secret. Any directory entries except regular files are ignored (e.g. subdirectories,
		symlinks, devices, pipes, etc).`))

	secretForGenericExample = templates.Examples(i18n.T(`
	  # Create an empty secret my-secret of type Opaque
	  kubectl create secret generic empty-secret

	  # Create a new secret named my-secret with keys for each file in folder bar
	  kubectl create secret generic my-secret --from-file=path/to/bar

	  # Create a new secret named my-secret with specified keys instead of names on disk
	  kubectl create secret generic my-secret --from-file=ssh-privatekey=path/to/id_rsa --from-file=ssh-publickey=path/to/id_rsa.pub

	  # Create a new secret named my-secret with key1=supersecret and key2=topsecret
	  kubectl create secret generic my-secret --from-literal=key1=supersecret --from-literal=key2=topsecret

	  # Create a new secret named my-secret using a combination of a file and a literal
	  kubectl create secret generic my-secret --from-file=ssh-privatekey=path/to/id_rsa --from-literal=passphrase=topsecret

	  # Create a new secret named my-secret from env files
	  kubectl create secret generic my-secret --from-env-file=path/to/foo.env --from-env-file=path/to/bar.env`))
)

// CreateSecretOptions holds the options for 'create secret' sub command
type CreateSecretOptions struct {
	// PrintFlags holds options necessary for obtaining a printer
	PrintFlags *genericclioptions.PrintFlags
	PrintObj   func(obj runtime.Object) error

	// Name of secret (required)
	Name string
	// Type of secret (optional)
	Type string
	// FileSources to derive the secret from (optional)
	FileSources []string
	// LiteralSources to derive the secret from (optional)
	LiteralSources []string
	// EnvFileSources to derive the secret from (optional)
	EnvFileSources []string
	// AppendHash; if true, derive a hash from the Secret data and type and append it to the name
	AppendHash bool

	FieldManager     string
	CreateAnnotation bool
	Namespace        string
	EnforceNamespace bool

	Client              corev1client.CoreV1Interface
	DryRunStrategy      cmdutil.DryRunStrategy
	ValidationDirective string

	genericiooptions.IOStreams
}

// NewSecretOptions creates a new *CreateSecretOptions with default value
func NewSecretOptions(ioStreams genericiooptions.IOStreams) *CreateSecretOptions {
	return &CreateSecretOptions{
		PrintFlags: genericclioptions.NewPrintFlags("created").WithTypeSetter(scheme.Scheme),
		IOStreams:  ioStreams,
	}
}

// NewCmdCreateSecretGeneric is a command to create generic secrets from files, directories, or literal values
func NewCmdCreateSecretGeneric(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewSecretOptions(ioStreams)

	cmd := &cobra.Command{
		Use:                   "generic NAME [--type=string] [--from-file=[key=]source] [--from-literal=key1=value1] [--dry-run=server|client|none]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Create a secret from a local file, directory, or literal value. The default type for a secret is Opaque unless otherwise specified by the --type flag."),
		Long:                  secretForGenericLong,
		Example:               secretForGenericExample,
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

	cmd.Flags().StringSliceVar(&o.FileSources, "from-file", o.FileSources, "Key files can be specified using their file path, in which case a default name will be given to them, or optionally with a name and file path, in which case the given name will be used.  Specifying a directory will iterate each named file in the directory that is a valid secret key.")
	cmd.Flags().StringArrayVar(&o.LiteralSources, "from-literal", o.LiteralSources, "Specify a key and literal value to insert in secret (i.e. mykey=somevalue)")
	cmd.Flags().StringSliceVar(&o.EnvFileSources, "from-env-file", o.EnvFileSources, "Specify the path to a file to read lines of key=val pairs to create a secret.")
	cmd.Flags().StringVar(&o.Type, "type", o.Type, i18n.T("The string value indicate type of secret which is used to facilitate programmatic handling of secret data. Type can be built-in or user defined and vary in terms of validations performed and constraints kubernetes imposed on them. Empty type value indicate Opaque secret. Learn more about built-in types and usage here : https://kubernetes.io/docs/concepts/configuration/secret/#secret-types "))
	cmd.Flags().BoolVar(&o.AppendHash, "append-hash", o.AppendHash, "Append a hash of the secret to its name.")

	cmdutil.AddFieldManagerFlagVar(cmd, &o.FieldManager, "kubectl-create")

	return cmd
}

// Complete loads data from the command line environment
func (o *CreateSecretOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
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

	return nil
}

// Validate checks if CreateSecretOptions has sufficient value to run
func (o *CreateSecretOptions) Validate() error {
	if len(o.Name) == 0 {
		return fmt.Errorf("name must be specified")
	}
	if len(o.EnvFileSources) > 0 && (len(o.FileSources) > 0 || len(o.LiteralSources) > 0) {
		return fmt.Errorf("from-env-file cannot be combined with from-file or from-literal")
	}
	return nil
}

// Run calls createSecret which will create secret based on CreateSecretOptions
// and makes an API call to the server
func (o *CreateSecretOptions) Run() error {
	secret, err := o.createSecret()
	if err != nil {
		return err
	}
	err = util.CreateOrUpdateAnnotation(o.CreateAnnotation, secret, scheme.DefaultJSONEncoder())
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
		secret, err = o.Client.Secrets(o.Namespace).Create(context.TODO(), secret, createOptions)
		if err != nil {
			return fmt.Errorf("failed to create secret %v", err)
		}
	}

	return o.PrintObj(secret)
}

// createSecret fills in key value pair from the information given in
// CreateSecretOptions into *corev1.Secret
func (o *CreateSecretOptions) createSecret() (*corev1.Secret, error) {
	namespace := ""
	if o.EnforceNamespace {
		namespace = o.Namespace
	}
	secret := newSecretObj(o.Name, namespace, corev1.SecretType(o.Type))
	if len(o.LiteralSources) > 0 {
		if err := handleSecretFromLiteralSources(secret, o.LiteralSources); err != nil {
			return nil, err
		}
	}
	if len(o.FileSources) > 0 {
		if err := handleSecretFromFileSources(secret, o.FileSources); err != nil {
			return nil, err
		}
	}
	if len(o.EnvFileSources) > 0 {
		if err := handleSecretFromEnvFileSources(secret, o.EnvFileSources); err != nil {
			return nil, err
		}
	}
	if o.AppendHash {
		hash, err := hash.SecretHash(secret)
		if err != nil {
			return nil, err
		}
		secret.Name = fmt.Sprintf("%s-%s", secret.Name, hash)
	}

	return secret, nil
}

// newSecretObj will create a new Secret Object given name, namespace and secretType
func newSecretObj(name, namespace string, secretType corev1.SecretType) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: secretType,
		Data: map[string][]byte{},
	}
}

// handleSecretFromLiteralSources adds the specified literal source
// information into the provided secret
func handleSecretFromLiteralSources(secret *corev1.Secret, literalSources []string) error {
	for _, literalSource := range literalSources {
		keyName, value, err := util.ParseLiteralSource(literalSource)
		if err != nil {
			return err
		}
		if err = addKeyFromLiteralToSecret(secret, keyName, []byte(value)); err != nil {
			return err
		}
	}

	return nil
}

// handleSecretFromFileSources adds the specified file source information into the provided secret
func handleSecretFromFileSources(secret *corev1.Secret, fileSources []string) error {
	for _, fileSource := range fileSources {
		keyName, filePath, err := util.ParseFileSource(fileSource)
		if err != nil {
			return err
		}
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			switch err := err.(type) {
			case *os.PathError:
				return fmt.Errorf("error reading %s: %v", filePath, err.Err)
			default:
				return fmt.Errorf("error reading %s: %v", filePath, err)
			}
		}
		// if the filePath is a directory
		if fileInfo.IsDir() {
			if strings.Contains(fileSource, "=") {
				return fmt.Errorf("cannot give a key name for a directory path")
			}
			fileList, err := os.ReadDir(filePath)
			if err != nil {
				return fmt.Errorf("error listing files in %s: %v", filePath, err)
			}
			for _, item := range fileList {
				itemPath := filepath.Join(filePath, item.Name())
				if item.Type().IsRegular() {
					keyName = item.Name()
					if err := addKeyFromFileToSecret(secret, keyName, itemPath); err != nil {
						return err
					}
				}
			}
			// if the filepath is a file
		} else {
			if err := addKeyFromFileToSecret(secret, keyName, filePath); err != nil {
				return err
			}
		}

	}

	return nil
}

// handleSecretFromEnvFileSources adds the specified env files source information
// into the provided secret
func handleSecretFromEnvFileSources(secret *corev1.Secret, envFileSources []string) error {
	for _, envFileSource := range envFileSources {
		info, err := os.Stat(envFileSource)
		if err != nil {
			switch err := err.(type) {
			case *os.PathError:
				return fmt.Errorf("error reading %s: %v", envFileSource, err.Err)
			default:
				return fmt.Errorf("error reading %s: %v", envFileSource, err)
			}
		}
		if info.IsDir() {
			return fmt.Errorf("env secret file cannot be a directory")
		}
		err = cmdutil.AddFromEnvFile(envFileSource, func(key, value string) error {
			return addKeyFromLiteralToSecret(secret, key, []byte(value))
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// addKeyFromFileToSecret adds a key with the given name to a Secret, populating
// the value with the content of the given file path, or returns an error.
func addKeyFromFileToSecret(secret *corev1.Secret, keyName, filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	return addKeyFromLiteralToSecret(secret, keyName, data)
}

// addKeyFromLiteralToSecret adds the given key and data to the given secret,
// returning an error if the key is not valid or if the key already exists.
func addKeyFromLiteralToSecret(secret *corev1.Secret, keyName string, data []byte) error {
	if errs := validation.IsConfigMapKey(keyName); len(errs) != 0 {
		return fmt.Errorf("%q is not valid key name for a Secret %s", keyName, strings.Join(errs, ";"))
	}
	if _, entryExists := secret.Data[keyName]; entryExists {
		return fmt.Errorf("cannot add key %s, another key by that name already exists", keyName)
	}
	secret.Data[keyName] = data

	return nil
}
