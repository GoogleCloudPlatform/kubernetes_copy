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

package testdoc

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	"sigs.k8s.io/yaml"
)

// TestName logs the name of the test.
func TestName(name string) {
	fmt.Printf("<testdoc:name>%s</testdoc:name>\n", name)
}

// TestStep logs individual steps of the test.
func TestStep(step string) {
	fmt.Printf("<testdoc:step>%s</testdoc:step>\n", step)
}

// PodSpec logs the Pod specification in YAML format.
func PodSpec(pod *v1.Pod) {
	fmt.Printf("<testdoc:podspec>%s</testdoc:podspec>\n", getYaml(pod))
}

// TestLog logs general output for the test case.
func TestLog(log string) {
	fmt.Printf("<testdoc:log>%s</testdoc:log>\n", log)
}

// PodStatus logs the status of the Pod.
func PodStatus(status string) {
	fmt.Printf("<testdoc:status>%s</testdoc:status>\n", status)
}

// getYaml converts a Pod object to YAML format for logging purposes.
// Uses framework.ExpectNoError for error handling.
func getYaml(pod *v1.Pod) string {
	data, err := yaml.Marshal(pod)
	framework.ExpectNoError(err, "Failed to convert Pod to YAML")
	return string(data)
}
