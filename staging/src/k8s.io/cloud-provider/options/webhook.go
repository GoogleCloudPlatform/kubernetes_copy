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

package options

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/cloud-provider/config"
	netutils "k8s.io/utils/net"
)

const (
	CloudControllerManagerWebhookPort = 10260
)

type WebhookOptions struct {
	// Webhooks is the list of webhook names that should be enabled or disabled
	Webhooks []string
}

func NewWebhookOptions() *WebhookOptions {
	o := &WebhookOptions{}
	return o
}

func (o *WebhookOptions) AddFlags(fs *pflag.FlagSet, allWebhooks, disabledByDefaultWebhooks []string) {
	fs.StringSliceVar(&o.Webhooks, "webhooks", o.Webhooks, fmt.Sprintf(""+
		"A list of webhooks to enable. '*' enables all on-by-default webhooks, 'foo' enables the webhook "+
		"named 'foo', '-foo' disables the webhook named 'foo'.\nAll webhooks: %s\nDisabled-by-default webhooks: %s",
		strings.Join(allWebhooks, ", "), strings.Join(disabledByDefaultWebhooks, ", ")))
}

func (o *WebhookOptions) Validate(allWebhooks, disabledByDefaultWebhooks []string) []error {
	allErrors := []error{}

	allWebhooksSet := sets.NewString(allWebhooks...)
	toValidate := sets.NewString(o.Webhooks...)
	toValidate.Insert(disabledByDefaultWebhooks...)
	for _, webhook := range toValidate.List() {
		if webhook == "*" {
			continue
		}
		webhook = strings.TrimPrefix(webhook, "-")
		if !allWebhooksSet.Has(webhook) {
			allErrors = append(allErrors, fmt.Errorf("%q is not in the list of known webhooks", webhook))
		}
	}

	return allErrors
}

func (o *WebhookOptions) ApplyTo(cfg *config.WebhookConfiguration) error {
	if o == nil {
		return nil
	}

	cfg.Webhooks = o.Webhooks

	return nil
}

type WebhookServingOptions struct {
	*apiserveroptions.SecureServingOptions
}

func NewWebhookServingOptions(defaults ProviderDefaults) *WebhookServingOptions {
	var (
		bindAddress net.IP
		bindPort    int
	)

	if defaults.WebhookBindAddress != nil {
		bindAddress = *defaults.WebhookBindAddress
	} else {
		bindAddress = netutils.ParseIPSloppy("0.0.0.0")
	}

	if defaults.WebhookBindPort != nil {
		bindPort = *defaults.WebhookBindPort
	} else {
		bindPort = CloudControllerManagerWebhookPort
	}

	return &WebhookServingOptions{
		SecureServingOptions: &apiserveroptions.SecureServingOptions{
			BindAddress: bindAddress,
			BindPort:    bindPort,
			ServerCert: apiserveroptions.GeneratableKeyCert{
				CertDirectory: "",
				PairName:      "cloud-controller-manager-webhook",
			},
		},
	}
}

func (o *WebhookServingOptions) AddFlags(fs *pflag.FlagSet) {
	fs.IPVar(&o.BindAddress, "webhook-bind-address", o.BindAddress, ""+
		"The IP address on which to listen for the --webhook-secure-port port. The "+
		"associated interface(s) must be reachable by the rest of the cluster, and by CLI/web "+
		fmt.Sprintf("clients. If set to an unspecified address (0.0.0.0 or ::), all interfaces will be used. If unset, defaults to %v.", o.BindAddress))

	fs.IntVar(&o.BindPort, "webhook-secure-port", o.BindPort, "Secure port to serve cloud provider webhooks. If 0, don't serve webhooks at all.")

	fs.StringVar(&o.ServerCert.CertDirectory, "webhook-cert-dir", o.ServerCert.CertDirectory, ""+
		"The directory where the TLS certs are located. "+
		"If --tls-cert-file and --tls-private-key-file are provided, this flag will be ignored.")

	fs.StringVar(&o.ServerCert.CertKey.CertFile, "webhook-tls-cert-file", o.ServerCert.CertKey.CertFile, ""+
		"File containing the default x509 Certificate for HTTPS. (CA cert, if any, concatenated "+
		"after server cert). If HTTPS serving is enabled, and --tls-cert-file and "+
		"--tls-private-key-file are not provided, a self-signed certificate and key "+
		"are generated for the public address and saved to the directory specified by --cert-dir.")

	fs.StringVar(&o.ServerCert.CertKey.KeyFile, "webhook-tls-private-key-file", o.ServerCert.CertKey.KeyFile,
		"File containing the default x509 private key matching --tls-cert-file.")
}

func (o *WebhookServingOptions) Validate() []error {
	allErrors := []error{}
	if o.BindPort < 0 || o.BindPort > 65535 {
		allErrors = append(allErrors, fmt.Errorf("--webhook-secure-port %v must be between 0 and 65535, inclusive. A value of 0 disables the webhook endpoint entirely.", o.BindPort))
	}

	if (len(o.ServerCert.CertKey.CertFile) != 0 || len(o.ServerCert.CertKey.KeyFile) != 0) && o.ServerCert.GeneratedCert != nil {
		allErrors = append(allErrors, fmt.Errorf("cert/key file and in-memory certificate cannot both be set"))
	}

	return allErrors
}

func (o *WebhookServingOptions) ApplyTo(cfg **server.SecureServingInfo, webhookCfg config.WebhookConfiguration) error {
	if o == nil {
		return nil
	}

	if o.BindPort <= 0 {
		return nil
	}
	// no need to bind to the address if there are no webhook enabled.
	if len(webhookCfg.Webhooks) == 0 {
		return nil
	}

	var err error
	var listener net.Listener
	addr := net.JoinHostPort(o.BindAddress.String(), strconv.Itoa(o.BindPort))

	l := net.ListenConfig{}

	listener, o.BindPort, err = createListener(addr, l)
	if err != nil {
		return fmt.Errorf("failed to create listener: %v", err)
	}

	*cfg = &server.SecureServingInfo{
		Listener: listener,
	}

	serverCertFile, serverKeyFile := o.ServerCert.CertKey.CertFile, o.ServerCert.CertKey.KeyFile
	if len(serverCertFile) != 0 || len(serverKeyFile) != 0 {
		var err error
		// get context from caller for proper contextual logging
		(*cfg).Cert, err = dynamiccertificates.NewDynamicServingContentFromFiles(context.TODO(), "serving-cert", serverCertFile, serverKeyFile)
		if err != nil {
			return err
		}
	} else if o.ServerCert.GeneratedCert != nil {
		(*cfg).Cert = o.ServerCert.GeneratedCert
	}

	return nil
}

func createListener(addr string, config net.ListenConfig) (net.Listener, int, error) {
	ln, err := config.Listen(context.TODO(), "tcp", addr)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to listen on %v: %v", addr, err)
	}

	// get port
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, 0, fmt.Errorf("invalid listen address: %q", ln.Addr().String())
	}

	return ln, tcpAddr.Port, nil
}
