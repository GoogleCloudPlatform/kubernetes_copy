package testdoc
import (
	"fmt"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

)

// Helper methods

// Logs the name of the test
func TestName(s string) {
    fmt.Print("<testdoc:name>",s,"</testdoc:name>\n")
}

// Logs individual steps of the test
func TestStep(step string) {
    fmt.Print("<testdoc:step>",step,"</testdoc:step>\n")
}

// Logs the Pod specification in YAML format
func PodSpec(p *v1.Pod) {
    fmt.Print("<testdoc:podspec>",getYaml(p),"</testdoc:podspec>\n")
}

// Logs general log output for the test case
func TestLog(log string) {
    fmt.Print("<testdoc:log>",log,"</testdoc:log>\n")
}

// Logs the status of the Pod
func PodStatus(status string) {
    fmt.Print("<testdoc:status>",status,"</testdoc:status>\n")
}

// Converts Pod object to YAML for logging purposes
func getYaml(pod *v1.Pod) string {
    data, err := yaml.Marshal(pod)
    if err != nil {
        return ""
    }
    return string(data)
}
