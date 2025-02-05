/*
Copyright 2018 The Kubernetes Authors.

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

package etcd

import (
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/util/compatibility"
	utilversion "k8s.io/component-base/version"

	"k8s.io/kubernetes/test/utils/image"
)

// GetSupportedEmulatedVersions provides the list of supported emulated versions in the etcd data.
// Tests aiming for full coverage of versions should test fixtures of all supported versions.
func GetSupportedEmulatedVersions() []string {
	return []string{
		compatibility.DefaultKubeEffectiveVersionForTest().BinaryVersion().SubtractMinor(2).String(),
		compatibility.DefaultKubeEffectiveVersionForTest().BinaryVersion().SubtractMinor(1).String(),
		compatibility.DefaultKubeEffectiveVersionForTest().BinaryVersion().String(),
	}
}

// GetEtcdStorageData returns etcd data for all persisted objects at the latest release version.
// It is exported so that it can be reused across multiple tests.
// It returns a new map on every invocation to prevent different tests from mutating shared state.
func GetEtcdStorageData() map[schema.GroupVersionResource]StorageData {
	return GetEtcdStorageDataServedAt(utilversion.DefaultKubeBinaryVersion, false)
}

// GetEtcdStorageDataServedAt returns etcd data for all persisted objects at a particular release version.
// It is exported so that it can be reused across multiple tests.
// It returns a new map on every invocation to prevent different tests from mutating shared state.
func GetEtcdStorageDataServedAt(version string, removeAlphas bool) map[schema.GroupVersionResource]StorageData {
	return GetEtcdStorageDataForNamespaceServedAt("etcdstoragepathtestnamespace", version, removeAlphas)
}

// GetEtcdStorageDataForNamespace returns etcd data for all persisted objects at the latest release version.
// It is exported so that it can be reused across multiple tests.
// It returns a new map on every invocation to prevent different tests from mutating shared state.
// Namespaced objects keys are computed for the specified namespace.
func GetEtcdStorageDataForNamespace(namespace string) map[schema.GroupVersionResource]StorageData {
	return GetEtcdStorageDataForNamespaceServedAt(namespace, utilversion.DefaultKubeBinaryVersion, false)
}

// GetEtcdStorageDataForNamespaceServedAt returns etcd data for all persisted objects at a particular release version.
// It is exported so that it can be reused across multiple tests.
// It returns a new map on every invocation to prevent different tests from mutating shared state.
// Namespaced objects keys are computed for the specified namespace.
func GetEtcdStorageDataForNamespaceServedAt(namespace string, v string, removeAlphas bool) map[schema.GroupVersionResource]StorageData {
	image := image.GetE2EImage(image.BusyBox)
	etcdStorageData := map[schema.GroupVersionResource]StorageData{
		// k8s.io/kubernetes/pkg/api/v1
		gvr("", "v1", "configmaps"): {
			Stub:              `{"data": {"foo": "bar"}, "metadata": {"name": "cm1"}}`,
			ExpectedEtcdPath:  "/registry/configmaps/" + namespace + "/cm1",
			IntroducedVersion: "1.2",
		},
		gvr("", "v1", "services"): {
			Stub:              `{"metadata": {"name": "service1"}, "spec": {"type": "LoadBalancer", "ports": [{"port": 10000, "targetPort": 11000}], "selector": {"test": "data"}}}`,
			ExpectedEtcdPath:  "/registry/services/specs/" + namespace + "/service1",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "podtemplates"): {
			Stub:              `{"metadata": {"name": "pt1name"}, "template": {"metadata": {"labels": {"pt": "01"}}, "spec": {"containers": [{"image": "` + image + `", "name": "container9"}]}}}`,
			ExpectedEtcdPath:  "/registry/podtemplates/" + namespace + "/pt1name",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "pods"): {
			Stub:              `{"metadata": {"name": "pod1"}, "spec": {"containers": [{"image": "` + image + `", "name": "container7", "resources": {"limits": {"cpu": "1M"}, "requests": {"cpu": "1M"}}}]}}`,
			ExpectedEtcdPath:  "/registry/pods/" + namespace + "/pod1",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "endpoints"): {
			Stub:              `{"metadata": {"name": "ep1name"}, "subsets": [{"addresses": [{"hostname": "bar-001", "ip": "192.168.3.1"}], "ports": [{"port": 8000}]}]}`,
			ExpectedEtcdPath:  "/registry/services/endpoints/" + namespace + "/ep1name",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "resourcequotas"): {
			Stub:              `{"metadata": {"name": "rq1name"}, "spec": {"hard": {"cpu": "5M"}}}`,
			ExpectedEtcdPath:  "/registry/resourcequotas/" + namespace + "/rq1name",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "limitranges"): {
			Stub:              `{"metadata": {"name": "lr1name"}, "spec": {"limits": [{"type": "Pod"}]}}`,
			ExpectedEtcdPath:  "/registry/limitranges/" + namespace + "/lr1name",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "namespaces"): {
			Stub:              `{"metadata": {"name": "namespace1"}, "spec": {"finalizers": ["kubernetes"]}}`,
			ExpectedEtcdPath:  "/registry/namespaces/namespace1",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "nodes"): {
			Stub:              `{"metadata": {"name": "node1"}, "spec": {"unschedulable": true}}`,
			ExpectedEtcdPath:  "/registry/minions/node1",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "persistentvolumes"): {
			Stub:              `{"metadata": {"name": "pv1name"}, "spec": {"accessModes": ["ReadWriteOnce"], "capacity": {"storage": "3M"}, "hostPath": {"path": "/tmp/test/"}}}`,
			ExpectedEtcdPath:  "/registry/persistentvolumes/pv1name",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "events"): {
			Stub:              `{"involvedObject": {"namespace": "` + namespace + `"}, "message": "some data here", "metadata": {"name": "event1"}}`,
			ExpectedEtcdPath:  "/registry/events/" + namespace + "/event1",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "persistentvolumeclaims"): {
			Stub:              `{"metadata": {"name": "pvc1"}, "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"limits": {"storage": "1M"}, "requests": {"storage": "2M"}}, "selector": {"matchLabels": {"pvc": "stuff"}}}}`,
			ExpectedEtcdPath:  "/registry/persistentvolumeclaims/" + namespace + "/pvc1",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "serviceaccounts"): {
			Stub:              `{"metadata": {"name": "sa1name"}, "secrets": [{"name": "secret00"}]}`,
			ExpectedEtcdPath:  "/registry/serviceaccounts/" + namespace + "/sa1name",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "secrets"): {
			Stub:              `{"data": {"key": "ZGF0YSBmaWxl"}, "metadata": {"name": "secret1"}}`,
			ExpectedEtcdPath:  "/registry/secrets/" + namespace + "/secret1",
			IntroducedVersion: "1.0",
		},
		gvr("", "v1", "replicationcontrollers"): {
			Stub:              `{"metadata": {"name": "rc1"}, "spec": {"selector": {"new": "stuff"}, "template": {"metadata": {"labels": {"new": "stuff"}}, "spec": {"containers": [{"image": "` + image + `", "name": "container8"}]}}}}`,
			ExpectedEtcdPath:  "/registry/controllers/" + namespace + "/rc1",
			IntroducedVersion: "1.0",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/apps/v1
		gvr("apps", "v1", "daemonsets"): {
			Stub:              `{"metadata": {"name": "ds6"}, "spec": {"selector": {"matchLabels": {"a": "b"}}, "template": {"metadata": {"labels": {"a": "b"}}, "spec": {"containers": [{"image": "` + image + `", "name": "container6"}]}}}}`,
			ExpectedEtcdPath:  "/registry/daemonsets/" + namespace + "/ds6",
			IntroducedVersion: "1.9",
		},
		gvr("apps", "v1", "deployments"): {
			Stub:              `{"metadata": {"name": "deployment4"}, "spec": {"selector": {"matchLabels": {"f": "z"}}, "template": {"metadata": {"labels": {"f": "z"}}, "spec": {"containers": [{"image": "` + image + `", "name": "container6"}]}}}}`,
			ExpectedEtcdPath:  "/registry/deployments/" + namespace + "/deployment4",
			IntroducedVersion: "1.9",
		},
		gvr("apps", "v1", "statefulsets"): {
			Stub:              `{"metadata": {"name": "ss3"}, "spec": {"selector": {"matchLabels": {"a": "b"}}, "template": {"metadata": {"labels": {"a": "b"}}}}}`,
			ExpectedEtcdPath:  "/registry/statefulsets/" + namespace + "/ss3",
			IntroducedVersion: "1.9",
		},
		gvr("apps", "v1", "replicasets"): {
			Stub:              `{"metadata": {"name": "rs3"}, "spec": {"selector": {"matchLabels": {"g": "h"}}, "template": {"metadata": {"labels": {"g": "h"}}, "spec": {"containers": [{"image": "` + image + `", "name": "container4"}]}}}}`,
			ExpectedEtcdPath:  "/registry/replicasets/" + namespace + "/rs3",
			IntroducedVersion: "1.9",
		},
		gvr("apps", "v1", "controllerrevisions"): {
			Stub:              `{"metadata":{"name":"crs3"},"data":{"name":"abc","namespace":"default","creationTimestamp":null,"Spec":{"Replicas":0,"Selector":{"matchLabels":{"foo":"bar"}},"Template":{"creationTimestamp":null,"labels":{"foo":"bar"},"Spec":{"Volumes":null,"InitContainers":null,"Containers":null,"RestartPolicy":"Always","TerminationGracePeriodSeconds":null,"ActiveDeadlineSeconds":null,"DNSPolicy":"ClusterFirst","NodeSelector":null,"ServiceAccountName":"","AutomountServiceAccountToken":null,"NodeName":"","SecurityContext":null,"ImagePullSecrets":null,"Hostname":"","Subdomain":"","Affinity":null,"SchedulerName":"","Tolerations":null,"HostAliases":null}},"VolumeClaimTemplates":null,"ServiceName":""},"Status":{"ObservedGeneration":null,"Replicas":0}},"revision":0}`,
			ExpectedEtcdPath:  "/registry/controllerrevisions/" + namespace + "/crs3",
			IntroducedVersion: "1.9",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/autoscaling/v1
		gvr("autoscaling", "v1", "horizontalpodautoscalers"): {
			Stub:              `{"metadata": {"name": "hpa2"}, "spec": {"maxReplicas": 3, "scaleTargetRef": {"kind": "something", "name": "cross"}}}`,
			ExpectedEtcdPath:  "/registry/horizontalpodautoscalers/" + namespace + "/hpa2",
			ExpectedGVK:       gvkP("autoscaling", "v2", "HorizontalPodAutoscaler"),
			IntroducedVersion: "1.2",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/autoscaling/v2
		gvr("autoscaling", "v2", "horizontalpodautoscalers"): {
			Stub:              `{"metadata": {"name": "hpa4"}, "spec": {"maxReplicas": 3, "scaleTargetRef": {"kind": "something", "name": "cross"}}}`,
			ExpectedEtcdPath:  "/registry/horizontalpodautoscalers/" + namespace + "/hpa4",
			IntroducedVersion: "1.23",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/batch/v1
		gvr("batch", "v1", "jobs"): {
			Stub:              `{"metadata": {"name": "job1"}, "spec": {"manualSelector": true, "selector": {"matchLabels": {"controller-uid": "uid1"}}, "template": {"metadata": {"labels": {"controller-uid": "uid1"}}, "spec": {"containers": [{"image": "` + image + `", "name": "container1"}], "dnsPolicy": "ClusterFirst", "restartPolicy": "Never"}}}}`,
			ExpectedEtcdPath:  "/registry/jobs/" + namespace + "/job1",
			IntroducedVersion: "1.2",
		},
		gvr("batch", "v1", "cronjobs"): {
			Stub:              `{"metadata": {"name": "cjv1"}, "spec": {"jobTemplate": {"spec": {"template": {"metadata": {"labels": {"controller-uid": "uid0"}}, "spec": {"containers": [{"image": "` + image + `", "name": "container0"}], "dnsPolicy": "ClusterFirst", "restartPolicy": "Never"}}}}, "schedule": "* * * * *"}}`,
			ExpectedEtcdPath:  "/registry/cronjobs/" + namespace + "/cjv1",
			IntroducedVersion: "1.21",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/certificates/v1
		gvr("certificates.k8s.io", "v1", "certificatesigningrequests"): {
			Stub:              `{"metadata": {"name": "csr2"}, "spec": {"signerName":"example.com/signer", "usages":["any"], "request": "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURSBSRVFVRVNULS0tLS0KTUlJQnlqQ0NBVE1DQVFBd2dZa3hDekFKQmdOVkJBWVRBbFZUTVJNd0VRWURWUVFJRXdwRFlXeHBabTl5Ym1saApNUll3RkFZRFZRUUhFdzFOYjNWdWRHRnBiaUJXYVdWM01STXdFUVlEVlFRS0V3cEhiMjluYkdVZ1NXNWpNUjh3CkhRWURWUVFMRXhaSmJtWnZjbTFoZEdsdmJpQlVaV05vYm05c2IyZDVNUmN3RlFZRFZRUURFdzUzZDNjdVoyOXYKWjJ4bExtTnZiVENCbnpBTkJna3Foa2lHOXcwQkFRRUZBQU9CalFBd2dZa0NnWUVBcFp0WUpDSEo0VnBWWEhmVgpJbHN0UVRsTzRxQzAzaGpYK1prUHl2ZFlkMVE0K3FiQWVUd1htQ1VLWUhUaFZSZDVhWFNxbFB6eUlCd2llTVpyCldGbFJRZGRaMUl6WEFsVlJEV3dBbzYwS2VjcWVBWG5uVUsrNWZYb1RJL1VnV3NocmU4dEoreC9UTUhhUUtSL0oKY0lXUGhxYVFoc0p1elpidkFkR0E4MEJMeGRNQ0F3RUFBYUFBTUEwR0NTcUdTSWIzRFFFQkJRVUFBNEdCQUlobAo0UHZGcStlN2lwQVJnSTVaTStHWng2bXBDejQ0RFRvMEprd2ZSRGYrQnRyc2FDMHE2OGVUZjJYaFlPc3E0ZmtIClEwdUEwYVZvZzNmNWlKeENhM0hwNWd4YkpRNnpWNmtKMFRFc3VhYU9oRWtvOXNkcENvUE9uUkJtMmkvWFJEMkQKNmlOaDhmOHowU2hHc0ZxakRnRkh5RjNvK2xVeWorVUM2SDFRVzdibgotLS0tLUVORCBDRVJUSUZJQ0FURSBSRVFVRVNULS0tLS0="}}`,
			ExpectedEtcdPath:  "/registry/certificatesigningrequests/csr2",
			IntroducedVersion: "1.19",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/certificates/v1alpha1
		gvr("certificates.k8s.io", "v1alpha1", "clustertrustbundles"): {
			Stub:              `{"metadata": {"name": "example.com:signer:abc"}, "spec": {"signerName":"example.com/signer", "trustBundle": "-----BEGIN CERTIFICATE-----\nMIIBBDCBt6ADAgECAgEAMAUGAytlcDAQMQ4wDAYDVQQDEwVyb290MTAiGA8wMDAx\nMDEwMTAwMDAwMFoYDzAwMDEwMTAxMDAwMDAwWjAQMQ4wDAYDVQQDEwVyb290MTAq\nMAUGAytlcAMhAF2MoFeGa97gK2NGT1h6p1/a1GlMXAXbcjI/OShyIobPozIwMDAP\nBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBTWDdK2CNQiHqRjPaAWYPPtIykQgjAF\nBgMrZXADQQCtom9WGl7m2SAa4tXM9Soo/mbInBsRhn187BMoqTAHInHchKup5/3y\nl1tYJSZZsEXnXrCvw2qLCBNif6+2YYgE\n-----END CERTIFICATE-----\n"}}`,
			ExpectedEtcdPath:  "/registry/clustertrustbundles/example.com:signer:abc",
			IntroducedVersion: "1.26",
			RemovedVersion:    "1.37",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/coordination/v1
		gvr("coordination.k8s.io", "v1", "leases"): {
			Stub:              `{"metadata": {"name": "leasev1"}, "spec": {"holderIdentity": "holder", "leaseDurationSeconds": 5}}`,
			ExpectedEtcdPath:  "/registry/leases/" + namespace + "/leasev1",
			IntroducedVersion: "1.14",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/coordination/v1alpha2
		gvr("coordination.k8s.io", "v1alpha2", "leasecandidates"): {
			Stub:              `{"metadata": {"name": "leasecandidatev1alpha2"}, "spec": {"leaseName": "lease", "binaryVersion": "0.1.0", "emulationVersion": "0.1.0", "strategy": "OldestEmulationVersion"}}`,
			ExpectedEtcdPath:  "/registry/leasecandidates/" + namespace + "/leasecandidatev1alpha2",
			IntroducedVersion: "1.32",
			RemovedVersion:    "1.38",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/discovery/v1
		gvr("discovery.k8s.io", "v1", "endpointslices"): {
			Stub:              `{"metadata": {"name": "slicev1"}, "addressType": "IPv4", "protocol": "TCP", "ports": [], "endpoints": []}`,
			ExpectedEtcdPath:  "/registry/endpointslices/" + namespace + "/slicev1",
			IntroducedVersion: "1.21",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/events/v1
		gvr("events.k8s.io", "v1", "events"): {
			Stub:              `{"metadata": {"name": "event3"}, "regarding": {"namespace": "` + namespace + `"}, "note": "some data here", "eventTime": "2017-08-09T15:04:05.000000Z", "reportingInstance": "node-xyz", "reportingController": "k8s.io/my-controller", "action": "DidNothing", "reason": "Laziness", "type": "Normal"}`,
			ExpectedEtcdPath:  "/registry/events/" + namespace + "/event3",
			ExpectedGVK:       gvkP("", "v1", "Event"),
			IntroducedVersion: "1.19",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/networking/v1
		gvr("networking.k8s.io", "v1", "ingresses"): {
			Stub:              `{"metadata": {"name": "ingress3"}, "spec": {"defaultBackend": {"service":{"name":"service", "port":{"number": 5000}}}}}`,
			ExpectedEtcdPath:  "/registry/ingress/" + namespace + "/ingress3",
			IntroducedVersion: "1.19",
		},
		gvr("networking.k8s.io", "v1", "ingressclasses"): {
			Stub:              `{"metadata": {"name": "ingressclass3"}, "spec": {"controller": "example.com/controller"}}`,
			ExpectedEtcdPath:  "/registry/ingressclasses/ingressclass3",
			IntroducedVersion: "1.19",
		},
		gvr("networking.k8s.io", "v1", "networkpolicies"): {
			Stub:              `{"metadata": {"name": "np2"}, "spec": {"podSelector": {"matchLabels": {"e": "f"}}}}`,
			ExpectedEtcdPath:  "/registry/networkpolicies/" + namespace + "/np2",
			IntroducedVersion: "1.7",
		},
		gvr("networking.k8s.io", "v1", "ipaddresses"): {
			Stub:              `{"metadata": {"name": "192.168.2.3"}, "spec": {"parentRef": {"resource": "services","name": "test", "namespace": "ns"}}}`,
			ExpectedEtcdPath:  "/registry/ipaddresses/192.168.2.3",
			ExpectedGVK:       gvkP("networking.k8s.io", "v1beta1", "IPAddress"),
			IntroducedVersion: "1.33",
		},
		gvr("networking.k8s.io", "v1", "servicecidrs"): {
			Stub:              `{"metadata": {"name": "range-b2"}, "spec": {"cidrs": ["192.168.0.0/16","fd00:1::/120"]}}`,
			ExpectedEtcdPath:  "/registry/servicecidrs/range-b2",
			ExpectedGVK:       gvkP("networking.k8s.io", "v1beta1", "ServiceCIDR"),
			IntroducedVersion: "1.33",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/networking/v1beta1
		gvr("networking.k8s.io", "v1beta1", "ipaddresses"): {
			Stub:              `{"metadata": {"name": "192.168.1.3"}, "spec": {"parentRef": {"resource": "services","name": "test", "namespace": "ns"}}}`,
			ExpectedEtcdPath:  "/registry/ipaddresses/192.168.1.3",
			IntroducedVersion: "1.31",
			RemovedVersion:    "1.37",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/networking/v1beta1
		gvr("networking.k8s.io", "v1beta1", "servicecidrs"): {
			Stub:              `{"metadata": {"name": "range-b1"}, "spec": {"cidrs": ["192.168.0.0/16","fd00:1::/120"]}}`,
			ExpectedEtcdPath:  "/registry/servicecidrs/range-b1",
			IntroducedVersion: "1.31",
			RemovedVersion:    "1.37",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/policy/v1
		gvr("policy", "v1", "poddisruptionbudgets"): {
			Stub:              `{"metadata": {"name": "pdbv1"}, "spec": {"selector": {"matchLabels": {"anokkey": "anokvalue"}}}}`,
			ExpectedEtcdPath:  "/registry/poddisruptionbudgets/" + namespace + "/pdbv1",
			IntroducedVersion: "1.21",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/storagemigration/v1alpha1
		gvr("storagemigration.k8s.io", "v1alpha1", "storageversionmigrations"): {
			Stub:              `{"metadata": {"name": "test-migration"}, "spec":{"resource": {"group": "test-group", "resource": "test-resource", "version": "test-version"}}}`,
			ExpectedEtcdPath:  "/registry/storageversionmigrations/test-migration",
			IntroducedVersion: "1.30",
			RemovedVersion:    "1.36",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/flowcontrol/v1beta3
		gvr("flowcontrol.apiserver.k8s.io", "v1beta3", "flowschemas"): {
			Stub:              `{"metadata": {"name": "fs-2"}, "spec": {"priorityLevelConfiguration": {"name": "name1"}}}`,
			ExpectedEtcdPath:  "/registry/flowschemas/fs-2",
			ExpectedGVK:       gvkP("flowcontrol.apiserver.k8s.io", "v1", "FlowSchema"),
			IntroducedVersion: "1.26",
			RemovedVersion:    "1.32",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/flowcontrol/v1beta3
		gvr("flowcontrol.apiserver.k8s.io", "v1beta3", "prioritylevelconfigurations"): {
			Stub:              `{"metadata": {"name": "conf4"}, "spec": {"type": "Limited", "limited": {"nominalConcurrencyShares":3, "limitResponse": {"type": "Reject"}}}}`,
			ExpectedEtcdPath:  "/registry/prioritylevelconfigurations/conf4",
			ExpectedGVK:       gvkP("flowcontrol.apiserver.k8s.io", "v1", "PriorityLevelConfiguration"),
			IntroducedVersion: "1.26",
			RemovedVersion:    "1.32",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/flowcontrol/v1
		gvr("flowcontrol.apiserver.k8s.io", "v1", "flowschemas"): {
			Stub:              `{"metadata": {"name": "fs-3"}, "spec": {"priorityLevelConfiguration": {"name": "name1"}}}`,
			ExpectedEtcdPath:  "/registry/flowschemas/fs-3",
			IntroducedVersion: "1.29",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/flowcontrol/v1
		gvr("flowcontrol.apiserver.k8s.io", "v1", "prioritylevelconfigurations"): {
			Stub:              `{"metadata": {"name": "conf5"}, "spec": {"type": "Limited", "limited": {"nominalConcurrencyShares":3, "limitResponse": {"type": "Reject"}}}}`,
			ExpectedEtcdPath:  "/registry/prioritylevelconfigurations/conf5",
			IntroducedVersion: "1.29",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/storage/v1
		gvr("storage.k8s.io", "v1", "volumeattachments"): {
			Stub:              `{"metadata": {"name": "va3"}, "spec": {"attacher": "gce", "nodeName": "localhost", "source": {"persistentVolumeName": "pv3"}}}`,
			ExpectedEtcdPath:  "/registry/volumeattachments/va3",
			IntroducedVersion: "1.13",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/storage/v1alpha1
		gvr("storage.k8s.io", "v1alpha1", "volumeattributesclasses"): {
			Stub:              `{"metadata": {"name": "vac1"}, "driverName": "example.com/driver", "parameters": {"foo": "bar"}}`,
			ExpectedEtcdPath:  "/registry/volumeattributesclasses/vac1",
			ExpectedGVK:       gvkP("storage.k8s.io", "v1beta1", "VolumeAttributesClass"),
			IntroducedVersion: "1.29",
			RemovedVersion:    "1.35",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/storage/v1beta1
		gvr("storage.k8s.io", "v1beta1", "volumeattributesclasses"): {
			Stub:              `{"metadata": {"name": "vac2"}, "driverName": "example.com/driver", "parameters": {"foo": "bar"}}`,
			ExpectedEtcdPath:  "/registry/volumeattributesclasses/vac2",
			IntroducedVersion: "1.31",
			RemovedVersion:    "1.37",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/storage/v1
		gvr("storage.k8s.io", "v1", "storageclasses"): {
			Stub:              `{"metadata": {"name": "sc2"}, "provisioner": "aws"}`,
			ExpectedEtcdPath:  "/registry/storageclasses/sc2",
			IntroducedVersion: "1.6",
		},
		gvr("storage.k8s.io", "v1", "csistoragecapacities"): {
			Stub:              `{"metadata": {"name": "csc-12345-3"}, "storageClassName": "sc1"}`,
			ExpectedEtcdPath:  "/registry/csistoragecapacities/" + namespace + "/csc-12345-3",
			IntroducedVersion: "1.24",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/rbac/v1
		gvr("rbac.authorization.k8s.io", "v1", "roles"): {
			Stub:              `{"metadata": {"name": "role3"}, "rules": [{"apiGroups": ["v1"], "resources": ["events"], "verbs": ["watch"]}]}`,
			ExpectedEtcdPath:  "/registry/roles/" + namespace + "/role3",
			IntroducedVersion: "1.8",
		},
		gvr("rbac.authorization.k8s.io", "v1", "clusterroles"): {
			Stub:              `{"metadata": {"name": "crole3"}, "rules": [{"nonResourceURLs": ["/version"], "verbs": ["get"]}]}`,
			ExpectedEtcdPath:  "/registry/clusterroles/crole3",
			IntroducedVersion: "1.8",
		},
		gvr("rbac.authorization.k8s.io", "v1", "rolebindings"): {
			Stub:              `{"metadata": {"name": "roleb3"}, "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "somecr"}, "subjects": [{"apiVersion": "rbac.authorization.k8s.io/v1alpha1", "kind": "Group", "name": "system:authenticated"}]}`,
			ExpectedEtcdPath:  "/registry/rolebindings/" + namespace + "/roleb3",
			IntroducedVersion: "1.8",
		},
		gvr("rbac.authorization.k8s.io", "v1", "clusterrolebindings"): {
			Stub:              `{"metadata": {"name": "croleb3"}, "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "somecr"}, "subjects": [{"apiVersion": "rbac.authorization.k8s.io/v1alpha1", "kind": "Group", "name": "system:authenticated"}]}`,
			ExpectedEtcdPath:  "/registry/clusterrolebindings/croleb3",
			IntroducedVersion: "1.8",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/admissionregistration/v1
		gvr("admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations"): {
			Stub:              `{"metadata":{"name":"hook2","creationTimestamp":null},"webhooks":[{"name":"externaladmissionhook.k8s.io","clientConfig":{"service":{"namespace":"ns","name":"n"},"caBundle":null},"rules":[{"operations":["CREATE"],"apiGroups":["group"],"apiVersions":["version"],"resources":["resource"]}],"failurePolicy":"Ignore","sideEffects":"None","admissionReviewVersions":["v1beta1"]}]}`,
			ExpectedEtcdPath:  "/registry/validatingwebhookconfigurations/hook2",
			IntroducedVersion: "1.16",
		},
		gvr("admissionregistration.k8s.io", "v1", "mutatingwebhookconfigurations"): {
			Stub:              `{"metadata":{"name":"hook2","creationTimestamp":null},"webhooks":[{"name":"externaladmissionhook.k8s.io","clientConfig":{"service":{"namespace":"ns","name":"n"},"caBundle":null},"rules":[{"operations":["CREATE"],"apiGroups":["group"],"apiVersions":["version"],"resources":["resource"]}],"failurePolicy":"Ignore","sideEffects":"None","admissionReviewVersions":["v1beta1"]}]}`,
			ExpectedEtcdPath:  "/registry/mutatingwebhookconfigurations/hook2",
			IntroducedVersion: "1.16",
		},
		gvr("admissionregistration.k8s.io", "v1", "validatingadmissionpolicies"): {
			Stub:              `{"metadata":{"name":"vap1","creationTimestamp":null},"spec":{"paramKind":{"apiVersion":"test.example.com/v1","kind":"Example"},"matchConstraints":{"resourceRules": [{"resourceNames": ["fakeName"], "apiGroups":["apps"],"apiVersions":["v1"],"operations":["CREATE", "UPDATE"], "resources":["deployments"]}]},"validations":[{"expression":"object.spec.replicas <= params.maxReplicas","message":"Too many replicas"}]}}`,
			ExpectedEtcdPath:  "/registry/validatingadmissionpolicies/vap1",
			IntroducedVersion: "1.30",
		},
		gvr("admissionregistration.k8s.io", "v1", "validatingadmissionpolicybindings"): {
			Stub:              `{"metadata":{"name":"pb1","creationTimestamp":null},"spec":{"policyName":"replicalimit-policy.example.com","paramRef":{"name":"replica-limit-test.example.com","parameterNotFoundAction":"Deny"},"validationActions":["Deny"]}}`,
			ExpectedEtcdPath:  "/registry/validatingadmissionpolicybindings/pb1",
			IntroducedVersion: "1.30",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/admissionregistration/v1beta1
		gvr("admissionregistration.k8s.io", "v1beta1", "validatingadmissionpolicies"): {
			Stub:              `{"metadata":{"name":"vap1b1","creationTimestamp":null},"spec":{"paramKind":{"apiVersion":"test.example.com/v1","kind":"Example"},"matchConstraints":{"resourceRules": [{"resourceNames": ["fakeName"], "apiGroups":["apps"],"apiVersions":["v1"],"operations":["CREATE", "UPDATE"], "resources":["deployments"]}]},"validations":[{"expression":"object.spec.replicas <= params.maxReplicas","message":"Too many replicas"}]}}`,
			ExpectedEtcdPath:  "/registry/validatingadmissionpolicies/vap1b1",
			ExpectedGVK:       gvkP("admissionregistration.k8s.io", "v1", "ValidatingAdmissionPolicy"),
			IntroducedVersion: "1.28",
			RemovedVersion:    "1.34",
		},
		gvr("admissionregistration.k8s.io", "v1beta1", "validatingadmissionpolicybindings"): {
			Stub:              `{"metadata":{"name":"pb1b1","creationTimestamp":null},"spec":{"policyName":"replicalimit-policy.example.com","paramRef":{"name":"replica-limit-test.example.com","parameterNotFoundAction":"Deny"},"validationActions":["Deny"]}}`,
			ExpectedEtcdPath:  "/registry/validatingadmissionpolicybindings/pb1b1",
			ExpectedGVK:       gvkP("admissionregistration.k8s.io", "v1", "ValidatingAdmissionPolicyBinding"),
			IntroducedVersion: "1.28",
			RemovedVersion:    "1.34",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/admissionregistration/v1alpha1
		gvr("admissionregistration.k8s.io", "v1alpha1", "mutatingadmissionpolicies"): {
			Stub:              `{"metadata":{"name":"map1","creationTimestamp":null},"spec":{"paramKind":{"apiVersion":"test.example.com/v1","kind":"Example"},"matchConstraints":{"resourceRules": [{"resourceNames": ["fakeName"], "apiGroups":["apps"],"apiVersions":["v1"],"operations":["CREATE", "UPDATE"], "resources":["deployments"]}]},"reinvocationPolicy": "IfNeeded","mutations":[{"applyConfiguration": {"expression":"Object{metadata: Object.metadata{labels: {'example':'true'}}}"}, "patchType":"ApplyConfiguration"}]}}`,
			ExpectedEtcdPath:  "/registry/mutatingadmissionpolicies/map1",
			IntroducedVersion: "1.32",
			RemovedVersion:    "1.38",
		},
		gvr("admissionregistration.k8s.io", "v1alpha1", "mutatingadmissionpolicybindings"): {
			Stub:              `{"metadata":{"name":"mpb1","creationTimestamp":null},"spec":{"policyName":"replicalimit-policy.example.com","paramRef":{"name":"replica-limit-test.example.com"}}}`,
			ExpectedEtcdPath:  "/registry/mutatingadmissionpolicybindings/mpb1",
			IntroducedVersion: "1.32",
			RemovedVersion:    "1.38",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/scheduling/v1
		gvr("scheduling.k8s.io", "v1", "priorityclasses"): {
			Stub:              `{"metadata":{"name":"pc3"},"Value":1000}`,
			ExpectedEtcdPath:  "/registry/priorityclasses/pc3",
			IntroducedVersion: "1.14",
		},
		// --

		// k8s.io/kube-aggregator/pkg/apis/apiregistration/v1
		// depends on aggregator using the same ungrouped RESTOptionsGetter as the kube apiserver, not SimpleRestOptionsFactory in aggregator.go
		gvr("apiregistration.k8s.io", "v1", "apiservices"): {
			Stub:              `{"metadata": {"name": "as2.foo.com"}, "spec": {"group": "foo.com", "version": "as2", "groupPriorityMinimum":100, "versionPriority":10}}`,
			ExpectedEtcdPath:  "/registry/apiregistration.k8s.io/apiservices/as2.foo.com",
			IntroducedVersion: "1.10",
		},
		// --

		// k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1
		gvr("apiextensions.k8s.io", "v1", "customresourcedefinitions"): {
			Stub: `{"metadata": {"name": "openshiftwebconsoleconfigs.webconsole2.operator.openshift.io"},"spec": {` +
				`"scope": "Cluster","group": "webconsole2.operator.openshift.io",` +
				`"versions": [{"name":"v1alpha1","storage":true,"served":true,"schema":{"openAPIV3Schema":{"type":"object"}}}],` +
				`"names": {"kind": "OpenShiftWebConsoleConfig","plural": "openshiftwebconsoleconfigs","singular": "openshiftwebconsoleconfig"}}}`,
			ExpectedEtcdPath:  "/registry/apiextensions.k8s.io/customresourcedefinitions/openshiftwebconsoleconfigs.webconsole2.operator.openshift.io",
			ExpectedGVK:       gvkP("apiextensions.k8s.io", "v1beta1", "CustomResourceDefinition"),
			IntroducedVersion: "1.16",
		},
		gvr("cr.bar.com", "v1", "foos"): {
			Stub:              `{"kind": "Foo", "apiVersion": "cr.bar.com/v1", "metadata": {"name": "cr1foo"}, "color": "blue"}`, // requires TypeMeta due to CRD scheme's UnstructuredObjectTyper
			ExpectedEtcdPath:  "/registry/cr.bar.com/foos/" + namespace + "/cr1foo",
			IntroducedVersion: "1.0",
		},
		gvr("custom.fancy.com", "v2", "pants"): {
			Stub:              `{"kind": "Pant", "apiVersion": "custom.fancy.com/v2", "metadata": {"name": "cr2pant"}, "isFancy": true}`, // requires TypeMeta due to CRD scheme's UnstructuredObjectTyper
			ExpectedEtcdPath:  "/registry/custom.fancy.com/pants/cr2pant",
			IntroducedVersion: "1.0",
		},
		gvr("awesome.bears.com", "v1", "pandas"): {
			Stub:              `{"kind": "Panda", "apiVersion": "awesome.bears.com/v1", "metadata": {"name": "cr3panda"}, "spec":{"replicas": 100}}`, // requires TypeMeta due to CRD scheme's UnstructuredObjectTyper
			ExpectedEtcdPath:  "/registry/awesome.bears.com/pandas/cr3panda",
			IntroducedVersion: "1.0",
		},
		gvr("awesome.bears.com", "v3", "pandas"): {
			Stub:              `{"kind": "Panda", "apiVersion": "awesome.bears.com/v3", "metadata": {"name": "cr4panda"}, "spec":{"replicas": 300}}`, // requires TypeMeta due to CRD scheme's UnstructuredObjectTyper
			ExpectedEtcdPath:  "/registry/awesome.bears.com/pandas/cr4panda",
			ExpectedGVK:       gvkP("awesome.bears.com", "v1", "Panda"),
			IntroducedVersion: "1.0",
		},
		gvr("random.numbers.com", "v1", "integers"): {
			Stub:              `{"kind": "Integer", "apiVersion": "random.numbers.com/v1", "metadata": {"name": "fortytwo"}, "value": 42, "garbage": "oiujnasdf"}`, // requires TypeMeta due to CRD scheme's UnstructuredObjectTyper
			ExpectedEtcdPath:  "/registry/random.numbers.com/integers/fortytwo",
			IntroducedVersion: "1.0",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/node/v1
		gvr("node.k8s.io", "v1", "runtimeclasses"): {
			Stub:              `{"metadata": {"name": "rc3"}, "handler": "h3"}`,
			ExpectedEtcdPath:  "/registry/runtimeclasses/rc3",
			IntroducedVersion: "1.20",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/resource/v1alpha3
		gvr("resource.k8s.io", "v1alpha3", "deviceclasses"): {
			Stub:              `{"metadata": {"name": "class1name"}}`,
			ExpectedEtcdPath:  "/registry/deviceclasses/class1name",
			ExpectedGVK:       gvkP("resource.k8s.io", "v1beta1", "DeviceClass"),
			IntroducedVersion: "1.31",
			RemovedVersion:    "1.37",
		},
		gvr("resource.k8s.io", "v1alpha3", "resourceclaims"): {
			Stub:              `{"metadata": {"name": "claim1name"}, "spec": {"devices": {"requests": [{"name": "req-0", "deviceClassName": "example-class", "allocationMode": "ExactCount", "count": 1}]}}}`,
			ExpectedEtcdPath:  "/registry/resourceclaims/" + namespace + "/claim1name",
			ExpectedGVK:       gvkP("resource.k8s.io", "v1beta1", "ResourceClaim"),
			IntroducedVersion: "1.31",
			RemovedVersion:    "1.37",
		},
		gvr("resource.k8s.io", "v1alpha3", "resourceclaimtemplates"): {
			Stub:              `{"metadata": {"name": "claimtemplate1name"}, "spec": {"spec": {"devices": {"requests": [{"name": "req-0", "deviceClassName": "example-class", "allocationMode": "ExactCount", "count": 1}]}}}}`,
			ExpectedEtcdPath:  "/registry/resourceclaimtemplates/" + namespace + "/claimtemplate1name",
			ExpectedGVK:       gvkP("resource.k8s.io", "v1beta1", "ResourceClaimTemplate"),
			IntroducedVersion: "1.31",
			RemovedVersion:    "1.37",
		},
		gvr("resource.k8s.io", "v1alpha3", "resourceslices"): {
			Stub:              `{"metadata": {"name": "node1slice"}, "spec": {"nodeName": "worker1", "driver": "dra.example.com", "pool": {"name": "worker1", "resourceSliceCount": 1}}}`,
			ExpectedEtcdPath:  "/registry/resourceslices/node1slice",
			ExpectedGVK:       gvkP("resource.k8s.io", "v1beta1", "ResourceSlice"),
			IntroducedVersion: "1.31",
			RemovedVersion:    "1.37",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/resource/v1beta1
		gvr("resource.k8s.io", "v1beta1", "deviceclasses"): {
			Stub:              `{"metadata": {"name": "class2name"}}`,
			ExpectedEtcdPath:  "/registry/deviceclasses/class2name",
			IntroducedVersion: "1.32",
			RemovedVersion:    "1.38",
		},
		gvr("resource.k8s.io", "v1beta1", "resourceclaims"): {
			Stub:              `{"metadata": {"name": "claim2name"}, "spec": {"devices": {"requests": [{"name": "req-0", "deviceClassName": "example-class", "allocationMode": "ExactCount", "count": 1}]}}}`,
			ExpectedEtcdPath:  "/registry/resourceclaims/" + namespace + "/claim2name",
			IntroducedVersion: "1.32",
			RemovedVersion:    "1.38",
		},
		gvr("resource.k8s.io", "v1beta1", "resourceclaimtemplates"): {
			Stub:              `{"metadata": {"name": "claimtemplate2name"}, "spec": {"spec": {"devices": {"requests": [{"name": "req-0", "deviceClassName": "example-class", "allocationMode": "ExactCount", "count": 1}]}}}}`,
			ExpectedEtcdPath:  "/registry/resourceclaimtemplates/" + namespace + "/claimtemplate2name",
			IntroducedVersion: "1.32",
			RemovedVersion:    "1.38",
		},
		gvr("resource.k8s.io", "v1beta1", "resourceslices"): {
			Stub:              `{"metadata": {"name": "node2slice"}, "spec": {"nodeName": "worker1", "driver": "dra.example.com", "pool": {"name": "worker1", "resourceSliceCount": 1}}}`,
			ExpectedEtcdPath:  "/registry/resourceslices/node2slice",
			IntroducedVersion: "1.32",
			RemovedVersion:    "1.38",
		},
		// --

		// k8s.io/apiserver/pkg/apis/apiserverinternal/v1alpha1
		gvr("internal.apiserver.k8s.io", "v1alpha1", "storageversions"): {
			Stub:             `{"metadata":{"name":"sv1.test"},"spec":{}}`,
			ExpectedEtcdPath: "/registry/storageversions/sv1.test",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/storage/v1
		gvr("storage.k8s.io", "v1", "csinodes"): {
			Stub:              `{"metadata": {"name": "csini2"}, "spec": {"drivers": [{"name": "test-driver", "nodeID": "localhost", "topologyKeys": ["company.com/zone1", "company.com/zone2"]}]}}`,
			ExpectedEtcdPath:  "/registry/csinodes/csini2",
			IntroducedVersion: "1.17",
		},
		// --

		// k8s.io/kubernetes/pkg/apis/storage/v1
		gvr("storage.k8s.io", "v1", "csidrivers"): {
			Stub:              `{"metadata": {"name": "csid2"}, "spec": {"attachRequired": true, "podInfoOnMount": true}}`,
			ExpectedEtcdPath:  "/registry/csidrivers/csid2",
			IntroducedVersion: "1.18",
		},
		// --
	}

	// Delete types no longer served or not yet added at a particular emulated version.
	for key, data := range etcdStorageData {
		if data.IntroducedVersion != "" && version.MustParse(data.IntroducedVersion).GreaterThan(version.MustParse(v)) {
			delete(etcdStorageData, key)
		}

		if data.RemovedVersion != "" && version.MustParse(v).AtLeast(version.MustParse(data.RemovedVersion)) {
			delete(etcdStorageData, key)
		}
	}

	if removeAlphas {
		for key := range etcdStorageData {
			if strings.Contains(key.Version, "alpha") {
				delete(etcdStorageData, key)
			}
		}
	}
	validateStorageData(etcdStorageData)
	return etcdStorageData
}

func validateStorageData(etcdStorageData map[schema.GroupVersionResource]StorageData) {
	exceptions := map[schema.GroupVersionResource]bool{
		gvr("internal.apiserver.k8s.io", "v1alpha1", "storageversions"): true,
	}

	for key, storageData := range etcdStorageData {
		if _, ok := exceptions[key]; ok {
			continue
		}
		version := key.Version
		if strings.Contains(version, "alpha") || strings.Contains(version, "beta") {
			if storageData.RemovedVersion == "" {
				panic(fmt.Sprintf("Error. Non-GA resource %s must have a removed version", key.String()))
			}
		}
		if storageData.IntroducedVersion == "" {
			panic(fmt.Sprintf("Error. Non-GA resource %s must have an introduced version", key.String()))
		}
	}
}

// StorageData contains information required to create an object and verify its storage in etcd
// It must be paired with a specific resource
type StorageData struct {
	Stub              string                   // Valid JSON stub to use during create
	Prerequisites     []Prerequisite           // Optional, ordered list of JSON objects to create before stub
	ExpectedEtcdPath  string                   // Expected location of object in etcd, do not use any variables, constants, etc to derive this value - always supply the full raw string
	ExpectedGVK       *schema.GroupVersionKind // The GVK that we expect this object to be stored as - leave this nil to use the default
	IntroducedVersion string                   // The version that this type is introduced
	RemovedVersion    string                   // The version that this type is removed. May be empty for stable resources
}

// Prerequisite contains information required to create a resource (but not verify it)
type Prerequisite struct {
	GvrData schema.GroupVersionResource
	Stub    string
}

// GetCustomResourceDefinitionData returns the resource definitions that back the custom resources
// included in GetEtcdStorageData.  They should be created using CreateTestCRDs before running any tests.
// We can switch this to v1 CRDs based on transitive call site analysis.
// Call sites:
// 1. TestDedupOwnerReferences - beta doesn't matter
// 2. TestWebhookAdmissionWithWatchCache/TestWebhookAdmissionWithoutWatchCache - beta doesn't matter
// 3. TestApplyStatus - the version fields don't matter.  Pruning isn't checked, just ownership.
// 4. TestDryRun - versions and pruning don't matter
// 5. TestStorageVersionBootstrap - versions and pruning don't matter.
// 6. TestEtcdStoragePath - beta doesn't matter
// 7. TestCrossGroupStorage - beta doesn't matter
// 8. TestOverlappingCustomResourceCustomResourceDefinition - beta doesn't matter
// 9. TestOverlappingCustomResourceAPIService - beta doesn't matter
func GetCustomResourceDefinitionData() []*apiextensionsv1.CustomResourceDefinition {
	return []*apiextensionsv1.CustomResourceDefinition{
		// namespaced
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foos.cr.bar.com",
			},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "cr.bar.com",
				Scope: apiextensionsv1.NamespaceScoped,
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural: "foos",
					Kind:   "Foo",
				},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{
						Name:    "v1",
						Served:  true,
						Storage: true,
						Schema:  fixtures.AllowAllSchema(),
					},
				},
			},
		},
		// cluster scoped
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pants.custom.fancy.com",
			},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "custom.fancy.com",
				Scope: apiextensionsv1.ClusterScoped,
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural: "pants",
					Kind:   "Pant",
				},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{
						Name:    "v2",
						Served:  true,
						Storage: true,
						Schema:  fixtures.AllowAllSchema(),
					},
				},
			},
		},
		// cluster scoped with legacy version field and pruning.
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "integers.random.numbers.com",
			},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "random.numbers.com",
				Scope: apiextensionsv1.ClusterScoped,
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural: "integers",
					Kind:   "Integer",
				},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{
						Name:    "v1",
						Served:  true,
						Storage: true,
						Schema: &apiextensionsv1.CustomResourceValidation{
							OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"value": {
										Type: "number",
									},
								},
							}},
					},
				},
			},
		},
		// cluster scoped with versions field
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pandas.awesome.bears.com",
			},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "awesome.bears.com",
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{
						Name:    "v1",
						Served:  true,
						Storage: true,
						Schema:  fixtures.AllowAllSchema(),
						Subresources: &apiextensionsv1.CustomResourceSubresources{
							Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
							Scale: &apiextensionsv1.CustomResourceSubresourceScale{
								SpecReplicasPath:   ".spec.replicas",
								StatusReplicasPath: ".status.replicas",
								LabelSelectorPath:  func() *string { path := ".status.selector"; return &path }(),
							},
						},
					},
					{
						Name:    "v2",
						Served:  false,
						Storage: false,
						Schema:  fixtures.AllowAllSchema(),
						Subresources: &apiextensionsv1.CustomResourceSubresources{
							Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
							Scale: &apiextensionsv1.CustomResourceSubresourceScale{
								SpecReplicasPath:   ".spec.replicas",
								StatusReplicasPath: ".status.replicas",
								LabelSelectorPath:  func() *string { path := ".status.selector"; return &path }(),
							},
						},
					},
					{
						Name:    "v3",
						Served:  true,
						Storage: false,
						Schema:  fixtures.AllowAllSchema(),
						Subresources: &apiextensionsv1.CustomResourceSubresources{
							Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
							Scale: &apiextensionsv1.CustomResourceSubresourceScale{
								SpecReplicasPath:   ".spec.replicas",
								StatusReplicasPath: ".status.replicas",
								LabelSelectorPath:  func() *string { path := ".status.selector"; return &path }(),
							},
						},
					},
				},
				Scope: apiextensionsv1.ClusterScoped,
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural: "pandas",
					Kind:   "Panda",
				},
			},
		},
	}
}

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

func gvkP(g, v, k string) *schema.GroupVersionKind {
	return &schema.GroupVersionKind{Group: g, Version: v, Kind: k}
}

func gvk(g, v, k string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: g, Version: v, Kind: k}
}
