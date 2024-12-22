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

package metricsutil

import (
	"fmt"
	"io"
	"sort"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/cli-runtime/pkg/printers"
	metricsapi "k8s.io/metrics/pkg/apis/metrics"
)

var (
	NamespaceColumn = "NAMESPACE"
	PodColumn       = "POD"
)

type ResourceMetricsInfo struct {
	Name      string
	Metrics   v1.ResourceList
	Available v1.ResourceList
}

type TopCmdPrinter struct {
	out               io.Writer
	measuredResources []v1.ResourceName
	nodeColumns       []string
	podColumns        []string
}

func NewTopCmdPrinter(out io.Writer) *TopCmdPrinter {
	printer := &TopCmdPrinter{
		out: out,
		measuredResources: []v1.ResourceName{
			v1.ResourceCPU,
			v1.ResourceMemory,
		},
		nodeColumns: []string{"NAME", "CPU(cores)", "CPU(%)", "MEMORY(bytes)", "MEMORY(%)"},
		podColumns:  []string{"NAME", "CPU(cores)", "MEMORY(bytes)"},
	}

	return printer
}

func (printer *TopCmdPrinter) PrintNodeMetrics(metrics []metricsapi.NodeMetrics, availableResources map[string]v1.ResourceList, noHeaders bool, sortBy string) error {
	if len(metrics) == 0 {
		return nil
	}
	w := printers.GetNewTabWriter(printer.out)
	defer w.Flush()

	measuredResources := printer.measuredResources
	sort.Sort(NewNodeMetricsSorter(metrics, sortBy))

	if !noHeaders {
		printColumnNames(w, printer.nodeColumns)
	}
	var usage v1.ResourceList
	for _, m := range metrics {
		m.Usage.DeepCopyInto(&usage)
		printer.printMetricsLine(w, &ResourceMetricsInfo{
			Name:      m.Name,
			Metrics:   usage,
			Available: availableResources[m.Name],
		}, measuredResources)
		delete(availableResources, m.Name)
	}

	// print lines for nodes of which the metrics is unreachable.
	for nodeName := range availableResources {
		printer.printMissingMetricsNodeLine(w, nodeName, measuredResources)
	}
	return nil
}

func (printer *TopCmdPrinter) PrintPodMetrics(metrics []metricsapi.PodMetrics, printContainers bool, withNamespace bool, noHeaders bool, sortBy string, sum bool) error {
	if len(metrics) == 0 {
		return nil
	}
	w := printers.GetNewTabWriter(printer.out)
	defer w.Flush()

	columnWidth := len(printer.podColumns)
	if !noHeaders {
		if withNamespace {
			printValue(w, NamespaceColumn)
			columnWidth++
		}
		if printContainers {
			printValue(w, PodColumn)
			columnWidth++
		}
		printColumnNames(w, printer.podColumns)
	}

	sort.Sort(NewPodMetricsSorter(metrics, withNamespace, sortBy, printer.measuredResources))

	for _, m := range metrics {
		if printContainers {
			sort.Sort(NewContainerMetricsSorter(m.Containers, sortBy))
			printer.printSinglePodContainerMetrics(w, &m, withNamespace, printer.measuredResources)
		} else {
			printer.printSinglePodMetrics(w, &m, withNamespace, printer.measuredResources)
		}

	}

	if sum {
		adder := NewResourceAdder(printer.measuredResources)
		for _, m := range metrics {
			adder.AddPodMetrics(&m)
		}
		printer.printPodResourcesSum(w, adder.total, columnWidth, printer.measuredResources)
	}

	return nil
}

func printColumnNames(out io.Writer, names []string) {
	for _, name := range names {
		printValue(out, name)
	}
	fmt.Fprint(out, "\n")
}

func (printer *TopCmdPrinter) printSinglePodMetrics(out io.Writer, m *metricsapi.PodMetrics, withNamespace bool, measuredResources []v1.ResourceName) {
	podMetrics := getPodMetrics(m, measuredResources)
	if withNamespace {
		printValue(out, m.Namespace)
	}
	printer.printMetricsLine(out, &ResourceMetricsInfo{
		Name:      m.Name,
		Metrics:   podMetrics,
		Available: v1.ResourceList{},
	}, measuredResources)
}

func (printer *TopCmdPrinter) printSinglePodContainerMetrics(out io.Writer, m *metricsapi.PodMetrics, withNamespace bool, measuredResources []v1.ResourceName) {
	for _, c := range m.Containers {
		if withNamespace {
			printValue(out, m.Namespace)
		}
		printValue(out, m.Name)
		printer.printMetricsLine(out, &ResourceMetricsInfo{
			Name:      c.Name,
			Metrics:   c.Usage,
			Available: v1.ResourceList{},
		}, measuredResources)
	}
}

func getPodMetrics(m *metricsapi.PodMetrics, measuredResources []v1.ResourceName) v1.ResourceList {
	podMetrics := make(v1.ResourceList)
	for _, res := range measuredResources {
		podMetrics[res], _ = resource.ParseQuantity("0")
	}

	for _, c := range m.Containers {
		for _, res := range measuredResources {
			quantity := podMetrics[res]
			quantity.Add(c.Usage[res])
			podMetrics[res] = quantity
		}
	}
	return podMetrics
}

func (printer *TopCmdPrinter) printMetricsLine(out io.Writer, metrics *ResourceMetricsInfo, measuredResources []v1.ResourceName) {
	printValue(out, metrics.Name)
	printer.printAllResourceUsages(out, metrics, measuredResources)
	fmt.Fprint(out, "\n")
}

func (printer *TopCmdPrinter) printMissingMetricsNodeLine(out io.Writer, nodeName string, measuredResources []v1.ResourceName) {
	printValue(out, nodeName)
	unknownMetricsStatus := "<unknown>"
	for i := 0; i < len(measuredResources); i++ {
		printValue(out, unknownMetricsStatus)
		printValue(out, unknownMetricsStatus)
	}
	fmt.Fprint(out, "\n")
}

func printValue(out io.Writer, value interface{}) {
	fmt.Fprintf(out, "%v\t", value)
}

func (printer *TopCmdPrinter) printAllResourceUsages(out io.Writer, metrics *ResourceMetricsInfo, measuredResources []v1.ResourceName) {
	for _, res := range measuredResources {
		quantity := metrics.Metrics[res]
		printSingleResourceUsage(out, res, quantity)
		fmt.Fprint(out, "\t")
		if available, found := metrics.Available[res]; found {
			fraction := float64(quantity.MilliValue()) / float64(available.MilliValue()) * 100
			fmt.Fprintf(out, "%d%%\t", int64(fraction))
		}
	}
}

func printSingleResourceUsage(out io.Writer, resourceType v1.ResourceName, quantity resource.Quantity) {
	switch resourceType {
	case v1.ResourceCPU:
		fmt.Fprintf(out, "%vm", quantity.MilliValue())
	case v1.ResourceMemory:
		fmt.Fprintf(out, "%vMi", quantity.Value()/(1024*1024))
	default:
		fmt.Fprintf(out, "%v", quantity.Value())
	}
}

func (printer *TopCmdPrinter) printPodResourcesSum(out io.Writer, total v1.ResourceList, columnWidth int, measuredResources []v1.ResourceName) {
	for i := 0; i < columnWidth-2; i++ {
		printValue(out, "")
	}
	printValue(out, "________")
	printValue(out, "________")
	fmt.Fprintf(out, "\n")
	for i := 0; i < columnWidth-3; i++ {
		printValue(out, "")
	}
	printer.printMetricsLine(out, &ResourceMetricsInfo{
		Name:      "",
		Metrics:   total,
		Available: v1.ResourceList{},
	}, measuredResources)

}
