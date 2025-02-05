/*
Copyright 2016 The Kubernetes Authors.

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

package clusterinfo

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

const (
	defaultPodLogsTimeout = 20 * time.Second
	timeout               = 5 * time.Minute
)

type ClusterInfoDumpOptions struct {
	PrintFlags *genericclioptions.PrintFlags
	PrintObj   printers.ResourcePrinterFunc
	Options    *corev1.PodLogOptions

	OutputDir     string
	AllNamespaces bool
	Namespaces    []string

	// PodLogOptions
	SinceTime    string
	SinceSeconds time.Duration

	Timeout          time.Duration
	AppsClient       appsv1client.AppsV1Interface
	CoreClient       corev1client.CoreV1Interface
	Namespace        string
	RESTClientGetter genericclioptions.RESTClientGetter
	LogsForObject    polymorphichelpers.LogsForObjectFunc

	genericiooptions.IOStreams
}

func NewCmdClusterInfoDump(restClientGetter genericclioptions.RESTClientGetter, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := &ClusterInfoDumpOptions{
		PrintFlags: genericclioptions.NewPrintFlags("").WithTypeSetter(scheme.Scheme).WithDefaultOutput("json"),

		IOStreams: ioStreams,
	}

	cmd := &cobra.Command{
		Use:     "dump",
		Short:   i18n.T("Dump relevant information for debugging and diagnosis"),
		Long:    dumpLong,
		Example: dumpExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(restClientGetter, cmd))
			cmdutil.CheckErr(o.Run())
		},
	}

	o.PrintFlags.AddFlags(cmd)

	cmd.Flags().StringVar(&o.SinceTime, "since-time", o.SinceTime, i18n.T("Only return logs after a specific date (RFC3339). Defaults to all logs. Only one of since-time / since may be used."))
	cmd.Flags().DurationVar(&o.SinceSeconds, "since", o.SinceSeconds, "Only return logs newer than a relative duration like 5s, 2m, or 3h. Defaults to all logs. Only one of since-time / since may be used.")
	cmd.Flags().StringVar(&o.OutputDir, "output-directory", o.OutputDir, i18n.T("Where to output the files.  If empty or '-' uses stdout, otherwise creates a directory hierarchy in that directory"))
	cmd.Flags().StringSliceVar(&o.Namespaces, "namespaces", o.Namespaces, "A comma separated list of namespaces to dump.")
	cmd.Flags().BoolVarP(&o.AllNamespaces, "all-namespaces", "A", o.AllNamespaces, "If true, dump all namespaces.  If true, --namespaces is ignored.")
	cmdutil.AddPodRunningTimeoutFlag(cmd, defaultPodLogsTimeout)
	return cmd
}

var (
	dumpLong = templates.LongDesc(i18n.T(`
    Dump cluster information out suitable for debugging and diagnosing cluster problems.  By default, dumps everything to
    stdout. You can optionally specify a directory with --output-directory.  If you specify a directory, Kubernetes will
    build a set of files in that directory.  By default, only dumps things in the current namespace and 'kube-system' namespace, but you can
    switch to a different namespace with the --namespaces flag, or specify --all-namespaces to dump all namespaces.

    The command also dumps the logs of all of the pods in the cluster; these logs are dumped into different directories
    based on namespace and pod name.`))

	dumpExample = templates.Examples(i18n.T(`
    # Dump current cluster state to stdout
    kubectl cluster-info dump

    # Dump current cluster state to /path/to/cluster-state
    kubectl cluster-info dump --output-directory=/path/to/cluster-state

    # Dump all namespaces to stdout
    kubectl cluster-info dump --all-namespaces

    # Dump a set of namespaces to /path/to/cluster-state
    kubectl cluster-info dump --namespaces default,kube-system --output-directory=/path/to/cluster-state`))
)

func setupOutputWriter(dir string, defaultWriter io.Writer, filename string, fileExtension string) io.Writer {
	if len(dir) == 0 || dir == "-" {
		return defaultWriter
	}
	fullFile := filepath.Join(dir, filename) + fileExtension
	parent := filepath.Dir(fullFile)
	cmdutil.CheckErr(os.MkdirAll(parent, 0755))

	file, err := os.Create(fullFile)
	cmdutil.CheckErr(err)
	return file
}

func (o *ClusterInfoDumpOptions) ToLogOptions() (*corev1.PodLogOptions, error) {
	logOptions := &corev1.PodLogOptions{}

	if len(o.SinceTime) > 0 {
		t, err := util.ParseRFC3339(o.SinceTime, metav1.Now)
		if err != nil {
			return nil, err
		}

		logOptions.SinceTime = &t
	}

	if o.SinceSeconds != 0 {
		// round up to the nearest second
		sec := int64(o.SinceSeconds.Round(time.Second).Seconds())
		logOptions.SinceSeconds = &sec
	}

	return logOptions, nil
}

func (o *ClusterInfoDumpOptions) Complete(restClientGetter genericclioptions.RESTClientGetter, cmd *cobra.Command) error {
	printer, err := o.PrintFlags.ToPrinter()
	if err != nil {
		return err
	}

	o.PrintObj = printer.PrintObj

	config, err := restClientGetter.ToRESTConfig()
	if err != nil {
		return err
	}

	o.CoreClient, err = corev1client.NewForConfig(config)
	if err != nil {
		return err
	}

	o.AppsClient, err = appsv1client.NewForConfig(config)
	if err != nil {
		return err
	}

	o.Timeout, err = cmdutil.GetPodRunningTimeoutFlag(cmd)
	if err != nil {
		return err
	}

	o.Namespace, _, err = restClientGetter.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.Options, err = o.ToLogOptions()
	if err != nil {
		return err
	}

	o.RESTClientGetter = restClientGetter
	o.LogsForObject = polymorphichelpers.LogsForObjectFn

	return nil
}

func (o *ClusterInfoDumpOptions) Run() error {
	nodes, err := o.CoreClient.Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	fileExtension := ".txt"
	if o.PrintFlags.OutputFormat != nil {
		switch *o.PrintFlags.OutputFormat {
		case "json":
			fileExtension = ".json"
		case "yaml":
			fileExtension = ".yaml"
		}
	}

	if err := o.PrintObj(nodes, setupOutputWriter(o.OutputDir, o.Out, "nodes", fileExtension)); err != nil {
		return err
	}

	var namespaces []string
	if o.AllNamespaces {
		namespaceList, err := o.CoreClient.Namespaces().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		for ix := range namespaceList.Items {
			namespaces = append(namespaces, namespaceList.Items[ix].Name)
		}
	} else {
		if len(o.Namespaces) == 0 {
			namespaces = []string{
				metav1.NamespaceSystem,
				o.Namespace,
			}
		} else {
			namespaces = o.Namespaces
		}
	}
	for _, namespace := range namespaces {
		// TODO: this is repetitive in the extreme.  Use reflection or
		// something to make this a for loop.
		events, err := o.CoreClient.Events(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		if err := o.PrintObj(events, setupOutputWriter(o.OutputDir, o.Out, path.Join(namespace, "events"), fileExtension)); err != nil {
			return err
		}

		rcs, err := o.CoreClient.ReplicationControllers(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		if err := o.PrintObj(rcs, setupOutputWriter(o.OutputDir, o.Out, path.Join(namespace, "replication-controllers"), fileExtension)); err != nil {
			return err
		}

		svcs, err := o.CoreClient.Services(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		if err := o.PrintObj(svcs, setupOutputWriter(o.OutputDir, o.Out, path.Join(namespace, "services"), fileExtension)); err != nil {
			return err
		}

		sets, err := o.AppsClient.DaemonSets(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		if err := o.PrintObj(sets, setupOutputWriter(o.OutputDir, o.Out, path.Join(namespace, "daemonsets"), fileExtension)); err != nil {
			return err
		}

		deps, err := o.AppsClient.Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		if err := o.PrintObj(deps, setupOutputWriter(o.OutputDir, o.Out, path.Join(namespace, "deployments"), fileExtension)); err != nil {
			return err
		}

		rps, err := o.AppsClient.ReplicaSets(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		if err := o.PrintObj(rps, setupOutputWriter(o.OutputDir, o.Out, path.Join(namespace, "replicasets"), fileExtension)); err != nil {
			return err
		}

		pods, err := o.CoreClient.Pods(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}

		if err := o.PrintObj(pods, setupOutputWriter(o.OutputDir, o.Out, path.Join(namespace, "pods"), fileExtension)); err != nil {
			return err
		}

		printContainer := func(writer io.Writer, container corev1.Container, pod *corev1.Pod, options *corev1.PodLogOptions) {
			writer.Write([]byte(fmt.Sprintf("==== START logs for container %s of pod %s/%s ====\n", container.Name, pod.Namespace, pod.Name)))
			defer writer.Write([]byte(fmt.Sprintf("==== END logs for container %s of pod %s/%s ====\n", container.Name, pod.Namespace, pod.Name)))

			requests, err := o.LogsForObject(o.RESTClientGetter, pod, options, timeout, false)
			if err != nil {
				// Print error and return.
				writer.Write([]byte(fmt.Sprintf("Create log request error: %s\n", err.Error())))
				return
			}

			for _, request := range requests {
				data, err := request.DoRaw(context.TODO())
				if err != nil {
					// Print error and return.
					writer.Write([]byte(fmt.Sprintf("Request log error: %s\n", err.Error())))
					return
				}
				writer.Write(data)
			}
		}

		for ix := range pods.Items {
			pod := &pods.Items[ix]
			initcontainers := pod.Spec.InitContainers
			containers := pod.Spec.Containers
			writer := setupOutputWriter(o.OutputDir, o.Out, path.Join(namespace, pod.Name, "logs"), ".txt")

			for i := range initcontainers {
				var options = o.Options.DeepCopy()
				options.Container = initcontainers[i].Name
				printContainer(writer, initcontainers[i], pod, options)
			}
			for i := range containers {
				var options = o.Options.DeepCopy()
				options.Container = containers[i].Name
				printContainer(writer, containers[i], pod, options)
			}
		}
	}

	dest := o.OutputDir
	if len(dest) > 0 && dest != "-" {
		fmt.Fprintf(o.Out, "Cluster info dumped to %s\n", dest)
	}
	return nil
}
