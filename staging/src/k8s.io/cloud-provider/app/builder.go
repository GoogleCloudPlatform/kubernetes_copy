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

package app

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/term"
	"k8s.io/component-base/version/verflag"
	"k8s.io/klog/v2"
)

type CommandBuilder struct {
	webhookConfigs                 map[string]WebhookConfig
	controllerInitFuncConstructors map[string]ControllerInitFuncConstructor
	controllerAliases              map[string]string
	additionalFlags                cliflag.NamedFlagSets
	options                        *options.CloudControllerManagerOptions
	cloudInitializer               InitCloudFunc
	stopCh                         <-chan struct{}
	cmdName                        string
	long                           string
	defaults                       *options.ProviderDefaults
}

func NewBuilder() *CommandBuilder {
	cb := CommandBuilder{}
	cb.webhookConfigs = make(map[string]WebhookConfig)
	cb.controllerInitFuncConstructors = make(map[string]ControllerInitFuncConstructor)
	cb.controllerAliases = make(map[string]string)
	return &cb
}

func (cb *CommandBuilder) SetOptions(options *options.CloudControllerManagerOptions) {
	cb.options = options
}

func (cb *CommandBuilder) AddFlags(additionalFlags cliflag.NamedFlagSets) {
	cb.additionalFlags = additionalFlags
}

func (cb *CommandBuilder) RegisterController(name string, constructor ControllerInitFuncConstructor, aliases map[string]string) {
	cb.controllerInitFuncConstructors[name] = constructor
	for key, val := range aliases {
		if name == val {
			cb.controllerAliases[key] = val
		}
	}
}

func (cb *CommandBuilder) RegisterDefaultControllers() {
	for key, val := range DefaultInitFuncConstructors {
		cb.controllerInitFuncConstructors[key] = val
	}
	for key, val := range names.CCMControllerAliases() {
		cb.controllerAliases[key] = val
	}
}

func (cb *CommandBuilder) RegisterWebhook(name string, config WebhookConfig) {
	cb.webhookConfigs[name] = config
}

func (cb *CommandBuilder) SetCloudInitializer(cloudInitializer InitCloudFunc) {
	cb.cloudInitializer = cloudInitializer
}

func (cb *CommandBuilder) SetStopChannel(stopCh <-chan struct{}) {
	cb.stopCh = stopCh
}

func (cb *CommandBuilder) SetCmdName(name string) {
	cb.cmdName = name
}

func (cb *CommandBuilder) SetLongDesc(long string) {
	cb.long = long
}

// SetProviderDefaults can be called to change the default values for some
// options when a flag is not set
func (cb *CommandBuilder) SetProviderDefaults(defaults options.ProviderDefaults) {
	cb.defaults = &defaults
}

func (cb *CommandBuilder) setdefaults() {
	if cb.stopCh == nil {
		cb.stopCh = wait.NeverStop
	}

	if cb.cmdName == "" {
		cb.cmdName = "cloud-controller-manager"
	}

	if cb.long == "" {
		cb.long = `The Cloud controller manager is a daemon that embeds the cloud specific control loops shipped with Kubernetes.`
	}

	if cb.defaults == nil {
		cb.defaults = &options.ProviderDefaults{}
	}

	if cb.options == nil {
		opts, err := options.NewCloudControllerManagerOptionsWithProviderDefaults(*cb.defaults)
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to initialize command options: %v\n", err)
			os.Exit(1)
		}
		cb.options = opts
	}
}

func (cb *CommandBuilder) BuildCommand(logger klog.Logger) *cobra.Command {
	cb.setdefaults()
	cmd := &cobra.Command{
		Use:  cb.cmdName,
		Long: cb.long,
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()
			cliflag.PrintFlags(cmd.Flags())

			config, err := cb.options.Config(ControllerNames(cb.controllerInitFuncConstructors), ControllersDisabledByDefault.List(),
				cb.controllerAliases, WebhookNames(cb.webhookConfigs), WebhooksDisabledByDefault.List())
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				return err
			}
			completedConfig := config.Complete()
			cloud := cb.cloudInitializer(logger, completedConfig)
			controllerInitializers := ConstructControllerInitializers(logger, cb.controllerInitFuncConstructors, completedConfig, cloud)
			webhooks := NewWebhookHandlers(cb.webhookConfigs, completedConfig, cloud)

			if err := Run(completedConfig, cloud, controllerInitializers, webhooks, cb.stopCh); err != nil {
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
	namedFlagSets := cb.options.Flags(ControllerNames(cb.controllerInitFuncConstructors), ControllersDisabledByDefault.List(), cb.controllerAliases,
		WebhookNames(cb.webhookConfigs), WebhooksDisabledByDefault.List())
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name())

	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}
	for _, f := range cb.additionalFlags.FlagSets {
		fs.AddFlagSet(f)
	}

	usageFmt := "Usage:\n  %s\n"
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), usageFmt, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStderr(), namedFlagSets, cols)
		return nil
	})
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStdout(), namedFlagSets, cols)
	})

	return cmd
}
