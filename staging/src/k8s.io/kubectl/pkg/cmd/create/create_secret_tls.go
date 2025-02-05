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
	"crypto/tls"
	"fmt"
	"os"

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
	secretForTLSLong = templates.LongDesc(i18n.T(`
		Create a TLS secret from the given public/private key pair.

		The public/private key pair must exist beforehand. The public key certificate must be .PEM encoded and match
		the given private key.`))

	secretForTLSExample = templates.Examples(i18n.T(`
	  # Create a new TLS secret named tls-secret with the given key pair
	  kubectl create secret tls tls-secret --cert=path/to/tls.crt --key=path/to/tls.key`))
)

// CreateSecretTLSOptions holds the options for 'create secret tls' sub command
type CreateSecretTLSOptions struct {
	// PrintFlags holds options necessary for obtaining a printer
	PrintFlags *genericclioptions.PrintFlags
	PrintObj   func(obj runtime.Object) error

	// Name is the name of this TLS secret.
	Name string
	// Key is the path to the user's private key.
	Key string
	// Cert is the path to the user's public key certificate.
	Cert string
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

// NewSecretTLSOptions creates a new *CreateSecretTLSOptions with default value
func NewSecretTLSOptions(ioStrems genericiooptions.IOStreams) *CreateSecretTLSOptions {
	return &CreateSecretTLSOptions{
		PrintFlags: genericclioptions.NewPrintFlags("created").WithTypeSetter(scheme.Scheme),
		IOStreams:  ioStrems,
	}
}

// NewCmdCreateSecretTLS is a macro command for creating secrets to work with TLS client or server
func NewCmdCreateSecretTLS(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewSecretTLSOptions(ioStreams)

	cmd := &cobra.Command{
		Use:                   "tls NAME --cert=path/to/cert/file --key=path/to/key/file [--dry-run=server|client|none]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Create a TLS secret"),
		Long:                  secretForTLSLong,
		Example:               secretForTLSExample,
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

	cmd.Flags().StringVar(&o.Cert, "cert", o.Cert, i18n.T("Path to PEM encoded public key certificate."))
	cmd.Flags().StringVar(&o.Key, "key", o.Key, i18n.T("Path to private key associated with given certificate."))
	cmd.Flags().BoolVar(&o.AppendHash, "append-hash", o.AppendHash, "Append a hash of the secret to its name.")
	cmd.Flags().BoolVar(&o.Immutable, "immutable", o.Immutable, "Create immutable secret.")

	cmdutil.AddFieldManagerFlagVar(cmd, &o.FieldManager, "kubectl-create")

	return cmd
}

// Complete loads data from the command line environment
func (o *CreateSecretTLSOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
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

// Validate checks if CreateSecretTLSOptions hass sufficient value to run
func (o *CreateSecretTLSOptions) Validate() error {
	// TODO: This is not strictly necessary. We can generate a self signed cert
	// if no key/cert is given. The only requirement is that we either get both
	// or none. See test/e2e/ingress_utils for self signed cert generation.
	if len(o.Key) == 0 || len(o.Cert) == 0 {
		return fmt.Errorf("key and cert must be specified")
	}
	return nil
}

// Run calls createSecretTLS which will create secretTLS based on CreateSecretTLSOptions
// and makes an API call to the server
func (o *CreateSecretTLSOptions) Run() error {
	secretTLS, err := o.createSecretTLS()
	if err != nil {
		return err
	}
	err = util.CreateOrUpdateAnnotation(o.CreateAnnotation, secretTLS, scheme.DefaultJSONEncoder())
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
		secretTLS, err = o.Client.Secrets(o.Namespace).Create(context.TODO(), secretTLS, createOptions)
		if err != nil {
			return fmt.Errorf("failed to create secret %v", err)
		}
	}
	return o.PrintObj(secretTLS)
}

// createSecretTLS fills in key value pair from the information given in
// CreateSecretTLSOptions into *corev1.Secret
func (o *CreateSecretTLSOptions) createSecretTLS() (*corev1.Secret, error) {
	namespace := ""
	if o.EnforceNamespace {
		namespace = o.Namespace
	}
	tlsCert, err := readFile(o.Cert)
	if err != nil {
		return nil, err
	}
	tlsKey, err := readFile(o.Key)
	if err != nil {
		return nil, err
	}
	if _, err := tls.X509KeyPair(tlsCert, tlsKey); err != nil {
		return nil, err
	}
	// TODO: Add more validation.
	// 1. If the certificate contains intermediates, it is a valid chain.
	// 2. Format etc.

	secretTLS := newSecretObj(o.Name, namespace, corev1.SecretTypeTLS)
	secretTLS.Data[corev1.TLSCertKey] = []byte(tlsCert)
	secretTLS.Data[corev1.TLSPrivateKeyKey] = []byte(tlsKey)
	if o.AppendHash {
		hash, err := hash.SecretHash(secretTLS)
		if err != nil {
			return nil, err
		}
		secretTLS.Name = fmt.Sprintf("%s-%s", secretTLS.Name, hash)
	}
	if o.Immutable {
		secretTLS.Immutable = ptr.To[bool](true)
	}

	return secretTLS, nil
}

// readFile just reads a file into a byte array.
func readFile(file string) ([]byte, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return []byte{}, fmt.Errorf("Cannot read file %v, %v", file, err)
	}
	return b, nil
}
