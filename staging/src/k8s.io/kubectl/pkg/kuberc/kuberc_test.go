/*
Copyright 2024 The Kubernetes Authors.

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

package kuberc

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"testing"

	"github.com/spf13/cobra"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/config"
)

type fakeCmds[T supportedTypes] struct {
	name  string
	flags []fakeFlag[T]
}

type supportedTypes interface {
	string | bool
}

type fakeFlag[T supportedTypes] struct {
	name      string
	value     T
	shorthand string
}

type testApplyOverride[T supportedTypes] struct {
	name               string
	nestedCmds         []fakeCmds[T]
	args               []string
	getPreferencesFunc func(kuberc string, errOut io.Writer) (*config.Preference, error)
	expectedFLags      []fakeFlag[T]
	expectedErr        error
}

type testApplyAlias[T supportedTypes] struct {
	name               string
	nestedCmds         []fakeCmds[T]
	args               []string
	getPreferencesFunc func(kuberc string, errOut io.Writer) (*config.Preference, error)
	expectedFLags      []fakeFlag[T]
	expectedCmd        string
	expectedArgs       []string
	expectedErr        error
}

func TestApplyOverride(t *testing.T) {
	tests := []testApplyOverride[string]{
		{
			name: "command override",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
		},
		{
			name: "subcommand override",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
		},
		{
			name: "subcommand override with prefix incorrectly matches",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "first",
							value: "test",
						},
						{
							name:  "firstflag",
							value: "test2",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"--firstflag",
				"explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "first",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "first",
					value: "changed",
				},
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "use explicit kuberc, subcommand explicit takes precedence",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"--kuberc",
				"test-custom-kuberc-path",
				"--firstflag=explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				if kuberc != "test-custom-kuberc-path" {
					return nil, fmt.Errorf("unexpected kuberc: %s", kuberc)
				}
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "use explicit kuberc, subcommand explicit takes precedence kuberc flag first",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"--kuberc=test-custom-kuberc-path",
				"command1",
				"command2",
				"--firstflag=explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				if kuberc != "test-custom-kuberc-path" {
					return nil, fmt.Errorf("unexpected kuberc: %s", kuberc)
				}
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "use explicit kuberc equal, subcommand explicit takes precedence",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"--kuberc=test-custom-kuberc-path",
				"--firstflag=explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				if kuberc != "test-custom-kuberc-path" {
					return nil, fmt.Errorf("unexpected kuberc: %s", kuberc)
				}
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "use explicit kuberc equal, subcommand explicit takes precedence multi spaces",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"--kuberc=test-custom-kuberc-path",
				"--firstflag=explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				if kuberc != "test-custom-kuberc-path" {
					return nil, fmt.Errorf("unexpected kuberc: %s", kuberc)
				}
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "  command1   command2   ",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "use explicit kuberc equal at the end, subcommand explicit takes precedence",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"--firstflag=explicit",
				"--kuberc=test-custom-kuberc-path",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				if kuberc != "test-custom-kuberc-path" {
					return nil, fmt.Errorf("unexpected kuberc: %s", kuberc)
				}
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "subcommand explicit takes precedence",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"--firstflag=explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "subcommand explicit takes precedence with space",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"--firstflag",
				"explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "subcommand explicit takes precedence with space and with shorthand",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:      "firstflag",
							value:     "test",
							shorthand: "r",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"-r",
				"explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "subcommand explicit takes precedence with space and with shorthand and equal sign",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:      "firstflag",
							value:     "test",
							shorthand: "r",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"-r=explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "subcommand check the not overridden flag",
			nestedCmds: []fakeCmds[string]{
				{
					name:  "command1",
					flags: nil,
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
						{
							name:  "secondflag",
							value: "secondflagvalue",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"--firstflag",
				"explicit",
				"--secondflag=changed",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
				{
					name:  "secondflag",
					value: "changed",
				},
			},
		},
		{
			name: "command1 also has same flag",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "shouldnuse",
						},
						{
							name:  "secondflag",
							value: "shouldnuse",
						},
					},
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"command2",
				"--firstflag",
				"explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1 command2",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
		},
		{
			name: "alias ignores command override",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"alias",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "alias",
							Command: "command1",
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "test",
				},
			},
		},
		{
			name: "alias command override",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"testalias",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "testalias",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "testalias",
							Command: "command1",
						},
					},
				}, nil
			},
			expectedErr: fmt.Errorf("alias testalias can not be overridden"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmdtesting.WithAlphaEnvs([]util.FeatureGate{util.KubeRC}, t, func(t *testing.T) {
				rootCmd := &cobra.Command{
					Use: "root",
				}
				prefHandler := NewPreferences()
				prefHandler.AddFlags(rootCmd.PersistentFlags())
				pref, ok := prefHandler.(*Preferences)
				if !ok {
					t.Fatal("unexpected type. Expected *Preferences")
				}
				addCommands(rootCmd, test.nestedCmds)
				pref.getPreferencesFunc = test.getPreferencesFunc
				errWriter := &bytes.Buffer{}
				_, err := pref.Apply(rootCmd, test.args, errWriter)
				if test.expectedErr == nil && err != nil {
					t.Fatalf("unexpected error %v\n", err)
				}
				if test.expectedErr != nil {
					if test.expectedErr.Error() != err.Error() {
						t.Fatalf("error %s expected but actual is %s", test.expectedErr, err)
					}
					return
				}

				actualCmd, _, err := rootCmd.Find(test.args[1:])
				if err != nil {
					t.Fatalf("unable to find the command %v\n", err)
				}

				err = actualCmd.ParseFlags(test.args[1:])
				if err != nil {
					t.Fatalf("unexpected error %v\n", err)
				}

				if errWriter.String() != "" {
					t.Fatalf("unexpected error message %s\n", errWriter.String())
				}

				for _, expectedFlag := range test.expectedFLags {
					actualFlag := actualCmd.Flag(expectedFlag.name)
					if actualFlag.Value.String() != expectedFlag.value {
						t.Fatalf("unexpected flag value expected %s actual %s", expectedFlag.value, actualFlag.Value.String())
					}
				}
			})
		})
	}
}

func TestApplOverrideBool(t *testing.T) {
	tests := []testApplyOverride[bool]{
		{
			name: "command override",
			nestedCmds: []fakeCmds[bool]{
				{
					name: "command1",
					flags: []fakeFlag[bool]{
						{
							name:  "firstflag",
							value: true,
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "false",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[bool]{
				{
					name:  "firstflag",
					value: false,
				},
			},
		},
		{
			name: "command override explicit pass",
			nestedCmds: []fakeCmds[bool]{
				{
					name: "command1",
					flags: []fakeFlag[bool]{
						{
							name:  "firstflag",
							value: true,
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"--firstflag",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "false",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[bool]{
				{
					name:  "firstflag",
					value: true,
				},
			},
		},
		{
			name: "command override explicit pass with shorthand",
			nestedCmds: []fakeCmds[bool]{
				{
					name: "command1",
					flags: []fakeFlag[bool]{
						{
							name:      "firstflag",
							value:     true,
							shorthand: "f",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"-f",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "false",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[bool]{
				{
					name:  "firstflag",
					value: true,
				},
			},
		},
		{
			name: "command override explicit pass with combined multiple shorthand",
			nestedCmds: []fakeCmds[bool]{
				{
					name: "command1",
					flags: []fakeFlag[bool]{
						{
							name:      "firstflag",
							value:     false,
							shorthand: "f",
						},
						{
							name:      "secondflag",
							value:     false,
							shorthand: "v",
						},
						{
							name:      "thirdflag",
							value:     true,
							shorthand: "d",
						},
					},
				},
			},
			args: []string{
				"root",
				"command1",
				"-dfv",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Overrides: []config.CommandOverride{
						{
							Command: "command1",
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "false",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[bool]{
				{
					name:  "firstflag",
					value: true,
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmdtesting.WithAlphaEnvs([]util.FeatureGate{util.KubeRC}, t, func(t *testing.T) {
				rootCmd := &cobra.Command{
					Use: "root",
				}
				prefHandler := NewPreferences()
				prefHandler.AddFlags(rootCmd.PersistentFlags())
				pref, ok := prefHandler.(*Preferences)
				if !ok {
					t.Fatal("unexpected type. Expected *Preferences")
				}
				addCommands(rootCmd, test.nestedCmds)
				pref.getPreferencesFunc = test.getPreferencesFunc
				errWriter := &bytes.Buffer{}
				_, err := pref.Apply(rootCmd, test.args, errWriter)
				if err != nil {
					t.Fatalf("unexpected error %v\n", err)
				}
				actualCmd, _, err := rootCmd.Find(test.args[1:])
				if err != nil {
					t.Fatalf("unable to find the command %v\n", err)
				}

				err = actualCmd.ParseFlags(test.args[1:])
				if err != nil {
					t.Fatalf("unexpected error %v\n", err)
				}

				if errWriter.String() != "" {
					t.Fatalf("unexpected error message %s\n", errWriter.String())
				}

				for _, expectedFlag := range test.expectedFLags {
					actualFlag := actualCmd.Flag(expectedFlag.name)
					actualValue, err := strconv.ParseBool(actualFlag.Value.String())
					if err != nil {
						t.Fatalf("unexpected error %v\n", err)
					}
					if actualValue != expectedFlag.value {
						t.Fatalf("unexpected flag value expected %t actual %s", expectedFlag.value, actualFlag.Value.String())
					}
				}
			})
		})
	}
}

func TestApplyAliasBool(t *testing.T) {
	tests := []testApplyAlias[bool]{
		{
			name: "command override",
			nestedCmds: []fakeCmds[bool]{
				{
					name: "command1",
					flags: []fakeFlag[bool]{
						{
							name:  "firstflag",
							value: false,
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "true",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[bool]{
				{
					name:  "firstflag",
					value: true,
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "command override explicit pass",
			nestedCmds: []fakeCmds[bool]{
				{
					name: "command1",
					flags: []fakeFlag[bool]{
						{
							name:  "firstflag",
							value: false,
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"--firstflag",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "false",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[bool]{
				{
					name:  "firstflag",
					value: true,
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "command override explicit pass with shorthand",
			nestedCmds: []fakeCmds[bool]{
				{
					name: "command1",
					flags: []fakeFlag[bool]{
						{
							name:      "firstflag",
							value:     false,
							shorthand: "f",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"-f",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "false",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[bool]{
				{
					name:  "firstflag",
					value: true,
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "command override explicit pass with combination of multiple shorthand",
			nestedCmds: []fakeCmds[bool]{
				{
					name: "command1",
					flags: []fakeFlag[bool]{
						{
							name:      "firstflag",
							value:     false,
							shorthand: "f",
						},
						{
							name:      "secondflag",
							value:     true,
							shorthand: "v",
						},
						{
							name:      "thirdflag",
							value:     false,
							shorthand: "d",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"-vfd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "false",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[bool]{
				{
					name:  "firstflag",
					value: true,
				},
				{
					name:  "secondflag",
					value: true,
				},
				{
					name:  "thirdflag",
					value: true,
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmdtesting.WithAlphaEnvs([]util.FeatureGate{util.KubeRC}, t, func(t *testing.T) {
				rootCmd := &cobra.Command{
					Use: "root",
				}
				prefHandler := NewPreferences()
				prefHandler.AddFlags(rootCmd.PersistentFlags())
				pref, ok := prefHandler.(*Preferences)
				if !ok {
					t.Fatal("unexpected type. Expected *Preferences")
				}
				addCommands(rootCmd, test.nestedCmds)
				pref.getPreferencesFunc = test.getPreferencesFunc
				errWriter := &bytes.Buffer{}
				lastArgs, err := pref.Apply(rootCmd, test.args, errWriter)
				if test.expectedErr == nil && err != nil {
					t.Fatalf("unexpected error %v\n", err)
				}
				if test.expectedErr != nil {
					if test.expectedErr.Error() != err.Error() {
						t.Fatalf("error %s expected but actual is %s", test.expectedErr, err)
					}
					return
				}

				actualCmd, _, err := rootCmd.Find(lastArgs[1:])
				if err != nil {
					t.Fatal(err)
				}

				err = actualCmd.ParseFlags(lastArgs)
				if err != nil {
					t.Fatalf("unexpected error %v\n", err)
				}

				if errWriter.String() != "" {
					t.Fatalf("unexpected error message %s\n", errWriter.String())
				}

				if test.expectedCmd != actualCmd.Name() {
					t.Fatalf("unexpected command expected %s actual %s", test.expectedCmd, actualCmd.Name())
				}

				for _, expectedFlag := range test.expectedFLags {
					actualFlag := actualCmd.Flag(expectedFlag.name)
					actualValue, err := strconv.ParseBool(actualFlag.Value.String())
					if err != nil {
						t.Fatalf("unexpected error %v\n", err)
					}
					if actualValue != expectedFlag.value {
						t.Fatalf("unexpected flag value expected %t actual %s", expectedFlag.value, actualFlag.Value.String())
					}
				}

				for _, expectedArg := range test.expectedArgs {
					found := false
					for _, actualArg := range lastArgs {
						if actualArg == expectedArg {
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("expected arg %s can not be found", expectedArg)
					}
				}
			})
		})
	}
}

func TestApplyAlias(t *testing.T) {
	tests := []testApplyAlias[string]{
		{
			name: "command override",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "command override prependArgs",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							PrependArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "command override prependArgs with args",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"arg1",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							PrependArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
				"arg1",
			},
		},
		{
			name: "command override prependArgs with appendArgs",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							PrependArgs: []string{
								"resources",
								"nodes",
							},
							AppendArgs: []string{
								"arg1",
								"arg2",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
				"arg1",
				"arg2",
			},
		},
		{
			name: "command override prependArgs with appendArgs with args",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"arg1",
				"arg2",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							PrependArgs: []string{
								"resources",
								"nodes",
							},
							AppendArgs: []string{
								"arg3",
								"arg4",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
				"arg1",
				"arg2",
				"arg3",
				"arg4",
			},
		},
		{
			name: "command override prependArgs with appendArgs with args with flagas",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"arg1",
				"--firstflag",
				"explicit",
				"arg2",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							PrependArgs: []string{
								"resources",
								"nodes",
							},
							AppendArgs: []string{
								"arg3",
								"arg4",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
				"arg1",
				"arg2",
				"arg3",
				"arg4",
			},
		},
		{
			name: "invalid duplicate aliasname",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedErr: fmt.Errorf("duplicate alias name getcmd"),
		},
		{
			name: "alias name with flags having dashes as prefix ",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "--firstflag",
									Default: "changed",
								},
							},
						},
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedErr: fmt.Errorf("flag name --firstflag should be in long form without dashes"),
		},
		{
			name: "invalid aliasname",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd!!",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedErr: fmt.Errorf("invalid alias name, can only include alphabetical characters"),
		},
		{
			name: "invalid aliasname with spaces",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd subalias",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedErr: fmt.Errorf("invalid alias name, can only include alphabetical characters"),
		},
		{
			name: "command override",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"--firstflag=explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "command override with shorthand",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:      "firstflag",
							value:     "test",
							shorthand: "r",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"-r=explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "command override with shorthand and space",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:      "firstflag",
							value:     "test",
							shorthand: "r",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"-r",
				"explicit",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "command override",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
						{
							name:  "secondflag",
							value: "secondflagvalue",
						},
					},
				},
			},
			args: []string{
				"root",
				"getcmd",
				"--firstflag=explicit",
				"--secondflag=changed",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "getcmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "explicit",
				},
				{
					name:  "secondflag",
					value: "changed",
				},
			},
			expectedCmd: "getcmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "simple aliasing",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"aliascmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "aliascmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
			expectedCmd: "aliascmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "simple aliasing with kuberc flag first",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"--kuberc=kuberc",
				"aliascmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "aliascmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
			expectedCmd: "aliascmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "simple aliasing with kuberc flag after",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"aliascmd",
				"--kuberc=kuberc",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "aliascmd",
							Command: "command1",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed",
				},
			},
			expectedCmd: "aliascmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "subcommand aliasing",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "shouldntuse",
						},
						{
							name:  "secondflag",
							value: "shouldntuse",
						},
					},
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"aliascmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "aliascmd",
							Command: "command1 command2",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed2",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed2",
				},
			},
			expectedCmd: "aliascmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
		{
			name: "subcommand aliasing with spaces",
			nestedCmds: []fakeCmds[string]{
				{
					name: "command1",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "shouldntuse",
						},
						{
							name:  "secondflag",
							value: "shouldntuse",
						},
					},
				},
				{
					name: "command2",
					flags: []fakeFlag[string]{
						{
							name:  "firstflag",
							value: "test",
						},
					},
				},
			},
			args: []string{
				"root",
				"aliascmd",
			},
			getPreferencesFunc: func(kuberc string, errOut io.Writer) (*config.Preference, error) {
				return &config.Preference{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Preference",
						APIVersion: "kubectl.config.k8s.io/v1alpha1",
					},
					Aliases: []config.AliasOverride{
						{
							Name:    "aliascmd",
							Command: "   command1   command2  ",
							AppendArgs: []string{
								"resources",
								"nodes",
							},
							Flags: []config.CommandOverrideFlag{
								{
									Name:    "firstflag",
									Default: "changed2",
								},
							},
						},
					},
				}, nil
			},
			expectedFLags: []fakeFlag[string]{
				{
					name:  "firstflag",
					value: "changed2",
				},
			},
			expectedCmd: "aliascmd",
			expectedArgs: []string{
				"resources",
				"nodes",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmdtesting.WithAlphaEnvs([]util.FeatureGate{util.KubeRC}, t, func(t *testing.T) {
				rootCmd := &cobra.Command{
					Use: "root",
				}
				prefHandler := NewPreferences()
				prefHandler.AddFlags(rootCmd.PersistentFlags())
				pref, ok := prefHandler.(*Preferences)
				if !ok {
					t.Fatal("unexpected type. Expected *Preferences")
				}
				addCommands(rootCmd, test.nestedCmds)
				pref.getPreferencesFunc = test.getPreferencesFunc
				errWriter := &bytes.Buffer{}
				lastArgs, err := pref.Apply(rootCmd, test.args, errWriter)
				if test.expectedErr == nil && err != nil {
					t.Fatalf("unexpected error %v\n", err)
				}
				if test.expectedErr != nil {
					if test.expectedErr.Error() != err.Error() {
						t.Fatalf("error %s expected but actual is %s", test.expectedErr, err)
					}
					return
				}

				actualCmd, _, err := rootCmd.Find(lastArgs[1:])
				if err != nil {
					t.Fatal(err)
				}

				err = actualCmd.ParseFlags(lastArgs)
				if err != nil {
					t.Fatalf("unexpected error %v\n", err)
				}

				if errWriter.String() != "" {
					t.Fatalf("unexpected error message %s\n", errWriter.String())
				}

				if test.expectedCmd != actualCmd.Name() {
					t.Fatalf("unexpected command expected %s actual %s", test.expectedCmd, actualCmd.Name())
				}

				for _, expectedFlag := range test.expectedFLags {
					actualFlag := actualCmd.Flag(expectedFlag.name)
					if actualFlag.Value.String() != expectedFlag.value {
						t.Fatalf("unexpected flag value expected %s actual %s", expectedFlag.value, actualFlag.Value.String())
					}
				}

				for _, expectedArg := range test.expectedArgs {
					found := false
					for _, actualArg := range lastArgs {
						if actualArg == expectedArg {
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("expected arg %s can not be found", expectedArg)
					}
				}
			})
		})
	}
}

func TestGetExplicitKuberc(t *testing.T) {
	tests := []struct {
		args        []string
		expected    string
		expectedErr error
	}{
		{
			args:     []string{"kubectl", "get", "--kuberc", "/tmp/filepath"},
			expected: "/tmp/filepath",
		},
		{
			args:     []string{"kubectl", "get", "--kuberc=/tmp/filepath"},
			expected: "/tmp/filepath",
		},
		{
			args:     []string{"kubectl", "get", "--kuberc=/tmp/filepath", "--", "/bin/bash", "--kuberc", "anotherpath"},
			expected: "/tmp/filepath",
		},
		{
			args:     []string{"kubectl", "get", "--kuberc", "/tmp/filepath", "--", "/bin/bash", "--kuberc", "anotherpath"},
			expected: "/tmp/filepath",
		},
		{
			args:        []string{"kubectl", "get", "--kuberc="},
			expectedErr: fmt.Errorf("kuberc file is not found"),
		},
		{
			args:        []string{"kubectl", "get", "--kuberc"},
			expectedErr: fmt.Errorf("kuberc file is not found"),
		},
		{
			args:     []string{"kubectl", "get", "--", "/bin/bash", "--kuberc", "anotherpath"},
			expected: "",
		},
	}
	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			actual, err := getExplicitKuberc(test.args)
			if err != nil {
				if err.Error() != test.expectedErr.Error() {
					t.Fatalf("unexpected error %v\n", err)
				}
			}
			if test.expected != actual {
				t.Fatalf("unexpected value %s expected %s", actual, test.expected)
			}
		})
	}
}

// Add list of commands in nested way.
// First iteration adds command into rootCmd,
// Second iteration adds command into the previous one.
func addCommands[T supportedTypes](rootCmd *cobra.Command, commands []fakeCmds[T]) {
	if len(commands) == 0 {
		return
	}

	subCmd := &cobra.Command{
		Use: commands[0].name,
	}

	for _, flg := range commands[0].flags {
		switch v := any(flg.value).(type) {
		case string:
			if flg.shorthand != "" {
				subCmd.Flags().StringP(flg.name, flg.shorthand, v, "")
			} else {
				subCmd.Flags().String(flg.name, v, "")
			}
		case bool:
			if flg.shorthand != "" {
				subCmd.Flags().BoolP(flg.name, flg.shorthand, v, "")
			} else {
				subCmd.Flags().Bool(flg.name, v, "")
			}
		}

	}
	rootCmd.AddCommand(subCmd)

	addCommands[T](subCmd, commands[1:])
}
