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

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	mangen "github.com/cpuguy83/go-md2man/v2/md2man"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	kubectlcmd "k8s.io/kubectl/pkg/cmd"
	"k8s.io/kubernetes/cmd/genutils"
	apiservapp "k8s.io/kubernetes/cmd/kube-apiserver/app"
	cmapp "k8s.io/kubernetes/cmd/kube-controller-manager/app"
	proxyapp "k8s.io/kubernetes/cmd/kube-proxy/app"
	schapp "k8s.io/kubernetes/cmd/kube-scheduler/app"
	kubeadmapp "k8s.io/kubernetes/cmd/kubeadm/app/cmd"
	kubeletapp "k8s.io/kubernetes/cmd/kubelet/app"
)

func main() {
	// use os.Args instead of "flags" because "flags" will mess up the man pages!
	path := "docs/man/man1"
	module := ""
	if len(os.Args) == 3 {
		path = os.Args[1]
		module = os.Args[2]
	} else {
		fmt.Fprintf(os.Stderr, "usage: %s [output directory] [module] \n", os.Args[0])
		os.Exit(1)
	}

	outDir, err := genutils.OutDir(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get output directory: %v\n", err)
		os.Exit(1)
	}

	// Set environment variables used by command so the output is consistent,
	// regardless of where we run.
	os.Setenv("HOME", "/home/username")
	ctx := context.Background()

	switch module {
	case "kube-apiserver":
		// generate manpage for kube-apiserver
		apiserver := apiservapp.NewAPIServerCommand()
		genMarkdown(apiserver, "", outDir)
		for _, c := range apiserver.Commands() {
			genMarkdown(c, "kube-apiserver", outDir)
		}
	case "kube-controller-manager":
		// generate manpage for kube-controller-manager
		controllermanager := cmapp.NewControllerManagerCommand()
		genMarkdown(controllermanager, "", outDir)
		for _, c := range controllermanager.Commands() {
			genMarkdown(c, "kube-controller-manager", outDir)
		}
	case "kube-proxy":
		// generate manpage for kube-proxy
		proxy := proxyapp.NewProxyCommand()
		genMarkdown(proxy, "", outDir)
		for _, c := range proxy.Commands() {
			genMarkdown(c, "kube-proxy", outDir)
		}
	case "kube-scheduler":
		// generate manpage for kube-scheduler
		scheduler := schapp.NewSchedulerCommand()
		genMarkdown(scheduler, "", outDir)
		for _, c := range scheduler.Commands() {
			genMarkdown(c, "kube-scheduler", outDir)
		}
	case "kubelet":
		// generate manpage for kubelet
		kubelet := kubeletapp.NewKubeletCommand(ctx)
		genMarkdown(kubelet, "", outDir)
		for _, c := range kubelet.Commands() {
			genMarkdown(c, "kubelet", outDir)
		}
	case "kubectl":
		// generate manpage for kubectl
		kubectl := kubectlcmd.NewKubectlCommand(kubectlcmd.KubectlOptions{IOStreams: genericiooptions.IOStreams{In: bytes.NewReader(nil), Out: io.Discard, ErrOut: io.Discard}})
		genMarkdown(kubectl, "", outDir)
		for _, c := range kubectl.Commands() {
			genMarkdown(c, "kubectl", outDir)
		}
	case "kubeadm":
		// generate manpage for kubeadm
		kubeadm := kubeadmapp.NewKubeadmCommand(bytes.NewReader(nil), io.Discard, io.Discard)
		genMarkdown(kubeadm, "", outDir)
		for _, c := range kubeadm.Commands() {
			genMarkdown(c, "kubeadm", outDir)
		}
	default:
		fmt.Fprintf(os.Stderr, "Module %s is not supported", module)
		os.Exit(1)
	}
}

func preamble(out *bytes.Buffer, name, short, long string) {
	out.WriteString(`% KUBERNETES(1) kubernetes User Manuals
% Eric Paris
% Jan 2015
# NAME
`)
	fmt.Fprintf(out, "%s \\- %s\n\n", name, short)
	fmt.Fprintf(out, "# SYNOPSIS\n")
	fmt.Fprintf(out, "**%s** [OPTIONS]\n\n", name)
	fmt.Fprintf(out, "# DESCRIPTION\n")
	fmt.Fprintf(out, "%s\n\n", long)
}

func printFlags(out *bytes.Buffer, flags *pflag.FlagSet) {
	flags.VisitAll(func(flag *pflag.Flag) {
		format := "**--%s**=%s\n\t%s\n\n"
		if flag.Value.Type() == "string" {
			// put quotes on the value
			format = "**--%s**=%q\n\t%s\n\n"
		}

		// Todo, when we mark a shorthand is deprecated, but specify an empty message.
		// The flag.ShorthandDeprecated is empty as the shorthand is deprecated.
		// Using len(flag.ShorthandDeprecated) > 0 can't handle this, others are ok.
		if !(len(flag.ShorthandDeprecated) > 0) && len(flag.Shorthand) > 0 {
			format = "**-%s**, " + format
			fmt.Fprintf(out, format, flag.Shorthand, flag.Name, flag.DefValue, flag.Usage)
		} else {
			fmt.Fprintf(out, format, flag.Name, flag.DefValue, flag.Usage)
		}
	})
}

func printOptions(out *bytes.Buffer, command *cobra.Command) {
	flags := command.NonInheritedFlags()
	if flags.HasFlags() {
		fmt.Fprintf(out, "# OPTIONS\n")
		printFlags(out, flags)
		fmt.Fprintf(out, "\n")
	}
	flags = command.InheritedFlags()
	if flags.HasFlags() {
		fmt.Fprintf(out, "# OPTIONS INHERITED FROM PARENT COMMANDS\n")
		printFlags(out, flags)
		fmt.Fprintf(out, "\n")
	}
}

func genMarkdown(command *cobra.Command, parent, docsDir string) {
	dparent := strings.Replace(parent, " ", "-", -1)
	name := command.Name()
	dname := name
	if len(parent) > 0 {
		dname = dparent + "-" + name
		name = parent + " " + name
	}

	out := new(bytes.Buffer)
	short := command.Short
	long := command.Long
	if len(long) == 0 {
		long = short
	}

	preamble(out, name, short, long)
	printOptions(out, command)

	if len(command.Example) > 0 {
		fmt.Fprintf(out, "# EXAMPLE\n")
		fmt.Fprintf(out, "```\n%s\n```\n", command.Example)
	}

	if len(command.Commands()) > 0 || len(parent) > 0 {
		fmt.Fprintf(out, "# SEE ALSO\n")
		if len(parent) > 0 {
			fmt.Fprintf(out, "**%s(1)**, ", dparent)
		}
		for _, c := range command.Commands() {
			fmt.Fprintf(out, "**%s-%s(1)**, ", dname, c.Name())
			genMarkdown(c, name, docsDir)
		}
		fmt.Fprintf(out, "\n")
	}

	out.WriteString(`
# HISTORY
January 2015, Originally compiled by Eric Paris (eparis at redhat dot com) based on the kubernetes source material, but hopefully they have been automatically generated since!
`)

	final := mangen.Render(out.Bytes())

	filename := docsDir + dname + ".1"
	outFile, err := os.Create(filename)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer outFile.Close()
	_, err = outFile.Write(final)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

}
