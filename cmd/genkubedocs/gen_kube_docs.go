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

	"github.com/spf13/cobra/doc"
	"github.com/spf13/pflag"
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
	path := ""
	module := ""
	if len(os.Args) == 3 {
		path = os.Args[1]
		module = os.Args[2]
	} else {
		fmt.Fprintf(os.Stderr, "usage: %s [output directory] [module] \n", os.Args[0])
		os.Exit(1)
	}

	ctx := context.Background()
	outDir, err := genutils.OutDir(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get output directory: %v\n", err)
		os.Exit(1)
	}

	switch module {
	case "kube-apiserver":
		// generate docs for kube-apiserver
		apiserver := apiservapp.NewAPIServerCommand()
		doc.GenMarkdownTree(apiserver, outDir)
	case "kube-controller-manager":
		// generate docs for kube-controller-manager
		controllermanager := cmapp.NewControllerManagerCommand()
		doc.GenMarkdownTree(controllermanager, outDir)
	case "kube-proxy":
		// generate docs for kube-proxy
		proxy := proxyapp.NewProxyCommand()
		doc.GenMarkdownTree(proxy, outDir)
	case "kube-scheduler":
		// generate docs for kube-scheduler
		scheduler := schapp.NewSchedulerCommand()
		doc.GenMarkdownTree(scheduler, outDir)
	case "kubelet":
		// generate docs for kubelet
		kubelet := kubeletapp.NewKubeletCommand(ctx)
		doc.GenMarkdownTree(kubelet, outDir)
	case "kubeadm":
		// resets global flags created by kubelet or other commands e.g.
		// --azure-container-registry-config from pkg/credentialprovider/azure
		// --version pkg/version/verflag
		pflag.CommandLine = pflag.NewFlagSet(os.Args[0], pflag.ExitOnError)

		// generate docs for kubeadm
		kubeadm := kubeadmapp.NewKubeadmCommand(bytes.NewReader(nil), io.Discard, io.Discard)
		doc.GenMarkdownTree(kubeadm, outDir)

		// cleanup generated code for usage as include in the website
		MarkdownPostProcessing(kubeadm, outDir, cleanupForInclude)
	default:
		fmt.Fprintf(os.Stderr, "Module %s is not supported", module)
		os.Exit(1)
	}
}
