/*
Copyright 2017 The Kubernetes Authors.

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

package noderestriction

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/util/feature"
	corev1lister "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeletapis "k8s.io/kubelet/pkg/apis"
	authenticationapi "k8s.io/kubernetes/pkg/apis/authentication"
	certificatesapi "k8s.io/kubernetes/pkg/apis/certificates"
	"k8s.io/kubernetes/pkg/apis/coordination"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/policy"
	resourceapi "k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/kubernetes/pkg/apis/storage"
	"k8s.io/kubernetes/pkg/auth/nodeidentifier"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/pointer"
)

func makeTestPod(namespace, name, node string, mirror bool) (*api.Pod, *corev1.Pod) {
	corePod := &api.Pod{}
	corePod.Namespace = namespace
	corePod.UID = types.UID("pod-uid")
	corePod.Name = name
	corePod.Spec.NodeName = node
	v1Pod := &corev1.Pod{}
	v1Pod.Namespace = namespace
	v1Pod.UID = types.UID("pod-uid")
	v1Pod.Name = name
	v1Pod.Spec.NodeName = node
	if mirror {
		corePod.Annotations = map[string]string{api.MirrorPodAnnotationKey: "true"}
		v1Pod.Annotations = map[string]string{api.MirrorPodAnnotationKey: "true"}

		// Insert a valid owner reference by default.
		controller := true
		owner := metav1.OwnerReference{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       node,
			UID:        types.UID(node + "-uid"),
			Controller: &controller,
		}
		corePod.OwnerReferences = []metav1.OwnerReference{owner}
		v1Pod.OwnerReferences = []metav1.OwnerReference{owner}
	}
	return corePod, v1Pod
}

func withLabels(pod *api.Pod, labels map[string]string) *api.Pod {
	labeledPod := pod.DeepCopy()
	if labels == nil {
		labeledPod.Labels = nil
		return labeledPod
	}
	// Clone.
	labeledPod.Labels = map[string]string{}
	for key, value := range labels {
		labeledPod.Labels[key] = value
	}
	return labeledPod
}

func makeTestPodEviction(name string) *policy.Eviction {
	eviction := &policy.Eviction{}
	eviction.Name = name
	eviction.Namespace = "ns"
	return eviction
}

func makeTokenRequest(podname string, poduid types.UID, audiences []string) *authenticationapi.TokenRequest {
	tr := &authenticationapi.TokenRequest{
		Spec: authenticationapi.TokenRequestSpec{
			Audiences: audiences,
		},
	}
	if podname != "" {
		tr.Spec.BoundObjectRef = &authenticationapi.BoundObjectReference{
			Kind:       "Pod",
			APIVersion: "v1",
			Name:       podname,
			UID:        poduid,
		}
	}
	return tr
}

func setAllLabels(node *api.Node, value string) *api.Node {
	node = setAllowedCreateLabels(node, value)
	node = setAllowedUpdateLabels(node, value)
	node = setForbiddenCreateLabels(node, value)
	node = setForbiddenUpdateLabels(node, value)
	return node
}

func setAllowedCreateLabels(node *api.Node, value string) *api.Node {
	node = setAllowedUpdateLabels(node, value)
	return node
}

func setAllowedUpdateLabels(node *api.Node, value string) *api.Node {
	node = node.DeepCopy()
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if value == "" {
		value = "value"
	}
	// non-kube labels
	node.Labels["foo"] = value
	node.Labels["example.com/foo"] = value

	// kubelet labels
	node.Labels["kubernetes.io/hostname"] = value
	node.Labels["failure-domain.beta.kubernetes.io/zone"] = value
	node.Labels["failure-domain.beta.kubernetes.io/region"] = value
	node.Labels["topology.kubernetes.io/zone"] = value
	node.Labels["topology.kubernetes.io/region"] = value
	node.Labels["beta.kubernetes.io/instance-type"] = value
	node.Labels["node.kubernetes.io/instance-type"] = value
	node.Labels["beta.kubernetes.io/os"] = value
	node.Labels["beta.kubernetes.io/arch"] = value
	node.Labels["kubernetes.io/os"] = value
	node.Labels["kubernetes.io/arch"] = value

	// kubelet label prefixes
	node.Labels["kubelet.kubernetes.io/foo"] = value
	node.Labels["foo.kubelet.kubernetes.io/foo"] = value
	node.Labels["node.kubernetes.io/foo"] = value
	node.Labels["foo.node.kubernetes.io/foo"] = value

	// test all explicitly allowed labels and prefixes
	for _, key := range kubeletapis.KubeletLabels() {
		node.Labels[key] = value
	}
	for _, namespace := range kubeletapis.KubeletLabelNamespaces() {
		node.Labels[namespace+"/foo"] = value
		node.Labels["foo."+namespace+"/foo"] = value
	}

	return node
}

func setForbiddenCreateLabels(node *api.Node, value string) *api.Node {
	node = node.DeepCopy()
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if value == "" {
		value = "value"
	}
	// node restriction labels are forbidden
	node.Labels["node-restriction.kubernetes.io/foo"] = value
	node.Labels["foo.node-restriction.kubernetes.io/foo"] = value
	node.Labels["other.kubernetes.io/foo"] = value
	node.Labels["other.k8s.io/foo"] = value
	return node
}

func setForbiddenUpdateLabels(node *api.Node, value string) *api.Node {
	node = node.DeepCopy()
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if value == "" {
		value = "value"
	}
	// node restriction labels are forbidden
	node.Labels["node-restriction.kubernetes.io/foo"] = value
	node.Labels["foo.node-restriction.kubernetes.io/foo"] = value
	// arbitrary kubernetes labels are forbidden on update
	node.Labels["other.kubernetes.io/foo"] = value
	node.Labels["other.k8s.io/foo"] = value
	return node
}

type admitTestCase struct {
	name            string
	podsGetter      corev1lister.PodLister
	nodesGetter     corev1lister.NodeLister
	csiDriverGetter storagelisters.CSIDriverLister
	pvcGetter       corev1lister.PersistentVolumeClaimLister
	pvGetter        corev1lister.PersistentVolumeLister
	attributes      admission.Attributes
	features        featuregate.FeatureGate
	setupFunc       func(t *testing.T)
	err             string
}

func (a *admitTestCase) run(t *testing.T) {
	t.Run(a.name, func(t *testing.T) {
		if a.setupFunc != nil {
			a.setupFunc(t)
		}
		c := NewPlugin(nodeidentifier.NewDefaultNodeIdentifier())
		if a.features != nil {
			c.InspectFeatureGates(a.features)
		}
		c.podsGetter = a.podsGetter
		c.nodesGetter = a.nodesGetter
		c.csiDriverGetter = a.csiDriverGetter
		c.pvcGetter = a.pvcGetter
		c.pvGetter = a.pvGetter
		err := c.Admit(context.TODO(), a.attributes, nil)
		if (err == nil) != (len(a.err) == 0) {
			t.Errorf("nodePlugin.Admit() error = %v, expected %v", err, a.err)
			return
		}
		if len(a.err) > 0 && !strings.Contains(err.Error(), a.err) {
			t.Errorf("nodePlugin.Admit() error = %v, expected %v", err, a.err)
		}
	})
}

func Test_nodePlugin_Admit(t *testing.T) {
	var (
		mynode = &user.DefaultInfo{Name: "system:node:mynode", Groups: []string{"system:nodes"}}
		bob    = &user.DefaultInfo{Name: "bob"}

		mynodeObjMeta    = metav1.ObjectMeta{Name: "mynode", UID: "mynode-uid"}
		mynodeObj        = &api.Node{ObjectMeta: mynodeObjMeta}
		mynodeObjConfigA = &api.Node{ObjectMeta: mynodeObjMeta, Spec: api.NodeSpec{ConfigSource: &api.NodeConfigSource{
			ConfigMap: &api.ConfigMapNodeConfigSource{
				Name:             "foo",
				Namespace:        "bar",
				UID:              "fooUID",
				KubeletConfigKey: "kubelet",
			}}}}
		mynodeObjConfigB = &api.Node{ObjectMeta: mynodeObjMeta, Spec: api.NodeSpec{ConfigSource: &api.NodeConfigSource{
			ConfigMap: &api.ConfigMapNodeConfigSource{
				Name:             "qux",
				Namespace:        "bar",
				UID:              "quxUID",
				KubeletConfigKey: "kubelet",
			}}}}

		mynodeObjTaintA = &api.Node{ObjectMeta: mynodeObjMeta, Spec: api.NodeSpec{Taints: []api.Taint{{Key: "mykey", Value: "A"}}}}
		mynodeObjTaintB = &api.Node{ObjectMeta: mynodeObjMeta, Spec: api.NodeSpec{Taints: []api.Taint{{Key: "mykey", Value: "B"}}}}
		othernodeObj    = &api.Node{ObjectMeta: metav1.ObjectMeta{Name: "othernode"}}

		coremymirrorpod, v1mymirrorpod           = makeTestPod("ns", "mymirrorpod", "mynode", true)
		coreothermirrorpod, v1othermirrorpod     = makeTestPod("ns", "othermirrorpod", "othernode", true)
		coreunboundmirrorpod, v1unboundmirrorpod = makeTestPod("ns", "unboundmirrorpod", "", true)
		coremypod, v1mypod                       = makeTestPod("ns", "mypod", "mynode", false)
		coreotherpod, v1otherpod                 = makeTestPod("ns", "otherpod", "othernode", false)
		coreunboundpod, v1unboundpod             = makeTestPod("ns", "unboundpod", "", false)
		coreunnamedpod, _                        = makeTestPod("ns", "", "mynode", false)

		mymirrorpodEviction      = makeTestPodEviction("mymirrorpod")
		othermirrorpodEviction   = makeTestPodEviction("othermirrorpod")
		unboundmirrorpodEviction = makeTestPodEviction("unboundmirrorpod")
		mypodEviction            = makeTestPodEviction("mypod")
		otherpodEviction         = makeTestPodEviction("otherpod")
		unboundpodEviction       = makeTestPodEviction("unboundpod")
		unnamedEviction          = makeTestPodEviction("")

		configmapResource = api.Resource("configmap").WithVersion("v1")
		configmapKind     = api.Kind("ConfigMap").WithVersion("v1")

		podResource  = api.Resource("pods").WithVersion("v1")
		podKind      = api.Kind("Pod").WithVersion("v1")
		evictionKind = policy.Kind("Eviction").WithVersion("v1beta1")

		nodeResource = api.Resource("nodes").WithVersion("v1")
		nodeKind     = api.Kind("Node").WithVersion("v1")

		svcacctResource  = api.Resource("serviceaccounts").WithVersion("v1")
		tokenrequestKind = api.Kind("TokenRequest").WithVersion("v1")

		leaseResource = coordination.Resource("leases").WithVersion("v1beta1")
		leaseKind     = coordination.Kind("Lease").WithVersion("v1beta1")
		lease         = &coordination.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mynode",
				Namespace: api.NamespaceNodeLease,
			},
			Spec: coordination.LeaseSpec{
				HolderIdentity:       pointer.String("mynode"),
				LeaseDurationSeconds: pointer.Int32(40),
				RenewTime:            &metav1.MicroTime{Time: time.Now()},
			},
		}
		leaseWrongNS = &coordination.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mynode",
				Namespace: "foo",
			},
			Spec: coordination.LeaseSpec{
				HolderIdentity:       pointer.String("mynode"),
				LeaseDurationSeconds: pointer.Int32(40),
				RenewTime:            &metav1.MicroTime{Time: time.Now()},
			},
		}
		leaseWrongName = &coordination.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: api.NamespaceNodeLease,
			},
			Spec: coordination.LeaseSpec{
				HolderIdentity:       pointer.String("mynode"),
				LeaseDurationSeconds: pointer.Int32(40),
				RenewTime:            &metav1.MicroTime{Time: time.Now()},
			},
		}

		csiNodeResource = storage.Resource("csinodes").WithVersion("v1")
		csiNodeKind     = schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "CSINode"}
		nodeInfo        = &storage.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: "mynode",
			},
			Spec: storage.CSINodeSpec{
				Drivers: []storage.CSINodeDriver{
					{
						Name:         "com.example.csi/mydriver",
						NodeID:       "com.example.csi/mynode",
						TopologyKeys: []string{"com.example.csi/zone"},
					},
				},
			},
		}
		nodeInfoWrongName = &storage.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: storage.CSINodeSpec{
				Drivers: []storage.CSINodeDriver{
					{
						Name:         "com.example.csi/mydriver",
						NodeID:       "com.example.csi/foo",
						TopologyKeys: []string{"com.example.csi/zone"},
					},
				},
			},
		}

		existingNodesIndex = cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		existingNodes      = corev1lister.NewNodeLister(existingNodesIndex)

		noExistingPodsIndex = cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		noExistingPods      = corev1lister.NewPodLister(noExistingPodsIndex)

		existingPodsIndex = cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		existingPods      = corev1lister.NewPodLister(existingPodsIndex)

		labelsA = map[string]string{
			"label-a": "value-a",
		}
		labelsAB = map[string]string{
			"label-a": "value-a",
			"label-b": "value-b",
		}
		aLabeledPod  = withLabels(coremypod, labelsA)
		abLabeledPod = withLabels(coremypod, labelsAB)

		privKey, _ = rsa.GenerateKey(rand.Reader, 2048)

		csiDriverWithAudience = &storagev1.CSIDriver{
			ObjectMeta: metav1.ObjectMeta{
				Name: "com.example.csi.mydriver",
			},
			Spec: storagev1.CSIDriverSpec{
				TokenRequests: []storagev1.TokenRequest{
					{
						Audience: "foo",
					},
				},
			},
		}

		csiDriverIndex  = cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		csiDriverLister = storagelisters.NewCSIDriverLister(csiDriverIndex)

		noexistingCSIDriverIndex  = cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		noexistingCSIDriverLister = storagelisters.NewCSIDriverLister(noexistingCSIDriverIndex)

		pvcWithCSIDriver = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvclaim",
				Namespace: "ns",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName: "pvname",
			},
		}

		ephemeralVolumePVCWithCSIDriver = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "myephemeralpod-myvol",
				Namespace: "ns",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName: "pvname",
			},
		}

		pvcIndex  = cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		pvcLister = corev1lister.NewPersistentVolumeClaimLister(pvcIndex)

		noexistingPVCIndex  = cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		noexistingPVCLister = corev1lister.NewPersistentVolumeClaimLister(noexistingPVCIndex)

		pvWithCSIDriver = &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pvname",
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: "com.example.csi.mydriver"},
				},
			},
		}

		pvIndex  = cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		pvLister = corev1lister.NewPersistentVolumeLister(pvIndex)

		noexistingPVIndex  = cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
		noexistingPVLister = corev1lister.NewPersistentVolumeLister(noexistingPVIndex)
	)

	// create pods for validating the service account node audience restriction
	projectedVolumeSourceEmptyAudience := &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: ""}}}}
	projectedVolumeSource := &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "foo"}}}}
	csiDriverVolumeSource := &corev1.CSIVolumeSource{Driver: "com.example.csi.mydriver"}
	persistentVolumeClaimVolumeSource := &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pvclaim"}
	ephemeralVolumeSource := &corev1.EphemeralVolumeSource{VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{}}

	coremypodWithProjectedServiceAccountEmptyAudience, v1mypodWithProjectedServiceAccountEmptyAudience := makeTestPod("ns", "mysapod", "mynode", false)
	v1mypodWithProjectedServiceAccountEmptyAudience.Spec.Volumes = []corev1.Volume{{VolumeSource: corev1.VolumeSource{Projected: projectedVolumeSourceEmptyAudience}}}

	coremypodWithProjectedServiceAccount, v1mypodWithProjectedServiceAccount := makeTestPod("ns", "mysapod", "mynode", false)
	v1mypodWithProjectedServiceAccount.Spec.Volumes = []corev1.Volume{{VolumeSource: corev1.VolumeSource{Projected: projectedVolumeSource}}}

	coremypodWithCSI, v1mypodWithCSI := makeTestPod("ns", "mycsipod", "mynode", false)
	v1mypodWithCSI.Spec.Volumes = []corev1.Volume{{VolumeSource: corev1.VolumeSource{CSI: csiDriverVolumeSource}}}

	coremypodWithPVCRefCSI, v1mypodWithPVCRefCSI := makeTestPod("ns", "mypvcpod", "mynode", false)
	v1mypodWithPVCRefCSI.Spec.Volumes = []corev1.Volume{{VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: persistentVolumeClaimVolumeSource}}}

	coremypodWithEphemeralVolume, v1mypodWithEphemeralVolume := makeTestPod("ns", "myephemeralpod", "mynode", false)
	v1mypodWithEphemeralVolume.Spec.Volumes = []corev1.Volume{{Name: "myvol", VolumeSource: corev1.VolumeSource{Ephemeral: ephemeralVolumeSource}}}

	coremypodWithPVCAndCSI, v1mypodWithPVCAndCSI := makeTestPod("ns", "mypvcandcsipod", "mynode", false)
	v1mypodWithPVCAndCSI.Spec.Volumes = []corev1.Volume{{VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: persistentVolumeClaimVolumeSource}}, {VolumeSource: corev1.VolumeSource{CSI: csiDriverVolumeSource}}}

	checkNilError(t, csiDriverIndex.Add(csiDriverWithAudience))
	checkNilError(t, pvcIndex.Add(pvcWithCSIDriver))
	checkNilError(t, pvcIndex.Add(ephemeralVolumePVCWithCSIDriver))
	checkNilError(t, pvIndex.Add(pvWithCSIDriver))

	existingPodsIndex.Add(v1mymirrorpod)
	existingPodsIndex.Add(v1othermirrorpod)
	existingPodsIndex.Add(v1unboundmirrorpod)
	existingPodsIndex.Add(v1mypod)
	existingPodsIndex.Add(v1otherpod)
	existingPodsIndex.Add(v1unboundpod)

	checkNilError(t, existingPodsIndex.Add(v1mypodWithProjectedServiceAccountEmptyAudience))
	checkNilError(t, existingPodsIndex.Add(v1mypodWithProjectedServiceAccount))
	checkNilError(t, existingPodsIndex.Add(v1mypodWithCSI))
	checkNilError(t, existingPodsIndex.Add(v1mypodWithPVCRefCSI))
	checkNilError(t, existingPodsIndex.Add(v1mypodWithEphemeralVolume))
	checkNilError(t, existingPodsIndex.Add(v1mypodWithPVCAndCSI))

	existingNodesIndex.Add(&corev1.Node{ObjectMeta: mynodeObjMeta})

	sapod, _ := makeTestPod("ns", "mysapod", "mynode", true)
	sapod.Spec.ServiceAccountName = "foo"

	secretpod, _ := makeTestPod("ns", "mysecretpod", "mynode", true)
	secretpod.Spec.Volumes = []api.Volume{{VolumeSource: api.VolumeSource{Secret: &api.SecretVolumeSource{SecretName: "foo"}}}}

	configmappod, _ := makeTestPod("ns", "myconfigmappod", "mynode", true)
	configmappod.Spec.Volumes = []api.Volume{{VolumeSource: api.VolumeSource{ConfigMap: &api.ConfigMapVolumeSource{LocalObjectReference: api.LocalObjectReference{Name: "foo"}}}}}

	ctbpod, _ := makeTestPod("ns", "myctbpod", "mynode", true)
	ctbpod.Spec.Volumes = []api.Volume{{VolumeSource: api.VolumeSource{Projected: &api.ProjectedVolumeSource{Sources: []api.VolumeProjection{{ClusterTrustBundle: &api.ClusterTrustBundleProjection{Name: pointer.String("foo")}}}}}}}

	pvcpod, _ := makeTestPod("ns", "mypvcpod", "mynode", true)
	pvcpod.Spec.Volumes = []api.Volume{{VolumeSource: api.VolumeSource{PersistentVolumeClaim: &api.PersistentVolumeClaimVolumeSource{ClaimName: "foo"}}}}

	tests := []admitTestCase{
		// Mirror pods bound to us
		{
			name:       "allow creating a mirror pod bound to self",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coremymirrorpod, nil, podKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "forbid update of mirror pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coremymirrorpod, coremymirrorpod, podKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "allow delete of mirror pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "forbid create of mirror pod status bound to self",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coremymirrorpod, nil, podKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "status", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "allow update of mirror pod status bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coremymirrorpod, coremymirrorpod, podKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "forbid delete of mirror pod status bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "status", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "allow create of eviction for mirror pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mymirrorpodEviction, nil, evictionKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "forbid update of eviction for mirror pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mymirrorpodEviction, nil, evictionKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "eviction", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of eviction for mirror pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mymirrorpodEviction, nil, evictionKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "eviction", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "allow create of unnamed eviction for mirror pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unnamedEviction, nil, evictionKind, coremymirrorpod.Namespace, coremymirrorpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},

		// Mirror pods bound to another node
		{
			name:       "forbid creating a mirror pod bound to another",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreothermirrorpod, nil, podKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid update of mirror pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreothermirrorpod, coreothermirrorpod, podKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of mirror pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid create of mirror pod status bound to another",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreothermirrorpod, nil, podKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "status", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid update of mirror pod status bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreothermirrorpod, coreothermirrorpod, podKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid delete of mirror pod status bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "status", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of eviction for mirror pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(othermirrorpodEviction, nil, evictionKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid update of eviction for mirror pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(othermirrorpodEviction, nil, evictionKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "eviction", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of eviction for mirror pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(othermirrorpodEviction, nil, evictionKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "eviction", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of unnamed eviction for mirror pod to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unnamedEviction, nil, evictionKind, coreothermirrorpod.Namespace, coreothermirrorpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},

		// Mirror pods not bound to any node
		{
			name:       "forbid creating a mirror pod unbound",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreunboundmirrorpod, nil, podKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid update of mirror pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreunboundmirrorpod, coreunboundmirrorpod, podKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of mirror pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid create of mirror pod status unbound",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreunboundmirrorpod, nil, podKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "status", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid update of mirror pod status unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreunboundmirrorpod, coreunboundmirrorpod, podKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid delete of mirror pod status unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "status", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of eviction for mirror pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unboundmirrorpodEviction, nil, evictionKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid update of eviction for mirror pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unboundmirrorpodEviction, nil, evictionKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "eviction", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of eviction for mirror pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unboundmirrorpodEviction, nil, evictionKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "eviction", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of unnamed eviction for mirror pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unnamedEviction, nil, evictionKind, coreunboundmirrorpod.Namespace, coreunboundmirrorpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},

		// Normal pods bound to us
		{
			name:       "forbid creating a normal pod bound to self",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coremypod, nil, podKind, coremypod.Namespace, coremypod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "can only create mirror pods",
		},
		{
			name:       "forbid update of normal pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coremypod, coremypod, podKind, coremypod.Namespace, coremypod.Name, podResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "allow delete of normal pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coremypod.Namespace, coremypod.Name, podResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "forbid create of normal pod status bound to self",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coremypod, nil, podKind, coremypod.Namespace, coremypod.Name, podResource, "status", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "allow update of normal pod status bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coremypod, coremypod, podKind, coremypod.Namespace, coremypod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "forbid delete of normal pod status bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coremypod.Namespace, coremypod.Name, podResource, "status", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid addition of pod status preexisting labels",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(abLabeledPod, aLabeledPod, podKind, coremypod.Namespace, coremypod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "cannot update labels through pod status",
		},
		{
			name:       "forbid deletion of pod status preexisting labels",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(aLabeledPod, abLabeledPod, podKind, coremypod.Namespace, coremypod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "cannot update labels through pod status",
		},
		{
			name:       "forbid deletion of all pod status preexisting labels",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(aLabeledPod, coremypod, podKind, coremypod.Namespace, coremypod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "cannot update labels through pod status",
		},
		{
			name:       "forbid addition of pod status labels",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coremypod, aLabeledPod, podKind, coremypod.Namespace, coremypod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "cannot update labels through pod status",
		},
		{
			name:       "forbid update of eviction for normal pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mypodEviction, nil, evictionKind, coremypod.Namespace, coremypod.Name, podResource, "eviction", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of eviction for normal pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mypodEviction, nil, evictionKind, coremypod.Namespace, coremypod.Name, podResource, "eviction", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "allow create of unnamed eviction for normal pod bound to self",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unnamedEviction, nil, evictionKind, coremypod.Namespace, coremypod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},

		// Normal pods bound to another
		{
			name:       "forbid creating a normal pod bound to another",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreotherpod, nil, podKind, coreotherpod.Namespace, coreotherpod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "can only create mirror pods",
		},
		{
			name:       "forbid update of normal pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreotherpod, coreotherpod, podKind, coreotherpod.Namespace, coreotherpod.Name, podResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of normal pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreotherpod.Namespace, coreotherpod.Name, podResource, "", admission.Delete, &metav1.UpdateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid create of normal pod status bound to another",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreotherpod, nil, podKind, coreotherpod.Namespace, coreotherpod.Name, podResource, "status", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid update of normal pod status bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreotherpod, coreotherpod, podKind, coreotherpod.Namespace, coreotherpod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid delete of normal pod status bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreotherpod.Namespace, coreotherpod.Name, podResource, "status", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of eviction for normal pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(otherpodEviction, nil, evictionKind, otherpodEviction.Namespace, otherpodEviction.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid update of eviction for normal pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(otherpodEviction, nil, evictionKind, otherpodEviction.Namespace, otherpodEviction.Name, podResource, "eviction", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of eviction for normal pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(otherpodEviction, nil, evictionKind, otherpodEviction.Namespace, otherpodEviction.Name, podResource, "eviction", admission.Delete, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of unnamed eviction for normal pod bound to another",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unnamedEviction, nil, evictionKind, coreotherpod.Namespace, coreotherpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},

		// Normal pods not bound to any node
		{
			name:       "forbid creating a normal pod unbound",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreunboundpod, nil, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "can only create mirror pods",
		},
		{
			name:       "forbid update of normal pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreunboundpod, coreunboundpod, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of normal pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid create of normal pod status unbound",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreunboundpod, nil, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "status", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid update of normal pod status unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreunboundpod, coreunboundpod, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid delete of normal pod status unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "status", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of eviction for normal pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unboundpodEviction, nil, evictionKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},
		{
			name:       "forbid update of eviction for normal pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unboundpodEviction, nil, evictionKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "eviction", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of eviction for normal pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unboundpodEviction, nil, evictionKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "eviction", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of unnamed eviction for normal unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unnamedEviction, nil, evictionKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "spec.nodeName set to itself",
		},

		// Missing pod
		{
			name:       "forbid delete of unknown pod",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "not found",
		},
		{
			name:       "forbid create of eviction for unknown pod",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(mypodEviction, nil, evictionKind, coremypod.Namespace, coremypod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "not found",
		},
		{
			name:       "forbid update of eviction for unknown pod",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(mypodEviction, nil, evictionKind, coremypod.Namespace, coremypod.Name, podResource, "eviction", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of eviction for unknown pod",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(mypodEviction, nil, evictionKind, coremypod.Namespace, coremypod.Name, podResource, "eviction", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of unnamed eviction for unknown pod",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(unnamedEviction, nil, evictionKind, coremypod.Namespace, coremypod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "not found",
		},

		// Eviction for unnamed pod
		{
			name:       "allow create of eviction for unnamed pod",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mypodEviction, nil, evictionKind, coreunnamedpod.Namespace, coreunnamedpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			// use the submitted eviction resource name as the pod name
			err: "",
		},
		{
			name:       "forbid update of eviction for unnamed pod",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mypodEviction, nil, evictionKind, coreunnamedpod.Namespace, coreunnamedpod.Name, podResource, "eviction", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid delete of eviction for unnamed pod",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mypodEviction, nil, evictionKind, coreunnamedpod.Namespace, coreunnamedpod.Name, podResource, "eviction", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: unexpected operation",
		},
		{
			name:       "forbid create of unnamed eviction for unnamed pod",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(unnamedEviction, nil, evictionKind, coreunnamedpod.Namespace, coreunnamedpod.Name, podResource, "eviction", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "could not determine pod from request data",
		},

		// Resource pods
		{
			name:       "forbid create of pod referencing service account",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(sapod, nil, podKind, sapod.Namespace, sapod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "reference a service account",
		},
		{
			name:       "forbid create of pod referencing secret",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(secretpod, nil, podKind, secretpod.Namespace, secretpod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "reference secrets",
		},
		{
			name:       "forbid create of pod referencing configmap",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(configmappod, nil, podKind, configmappod.Namespace, configmappod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "reference configmaps",
		},
		{
			name:       "forbid create of pod referencing clustertrustbundle",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(ctbpod, nil, podKind, ctbpod.Namespace, ctbpod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "reference clustertrustbundles",
		},
		{
			name:       "forbid create of pod referencing persistentvolumeclaim",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(pvcpod, nil, podKind, pvcpod.Namespace, pvcpod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "reference persistentvolumeclaims",
		},

		// My node object
		{
			name:       "allow create of my node",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(mynodeObj, nil, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow create of my node pulling name from object",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(mynodeObj, nil, nodeKind, mynodeObj.Namespace, "mynode", nodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow create of my node with taints",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(mynodeObjTaintA, nil, nodeKind, mynodeObj.Namespace, "mynode", nodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow create of my node with labels",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(setAllowedCreateLabels(mynodeObj, ""), nil, nodeKind, mynodeObj.Namespace, "mynode", nodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "forbid create of my node with forbidden labels",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(setForbiddenCreateLabels(mynodeObj, ""), nil, nodeKind, mynodeObj.Namespace, "mynode", nodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        `is not allowed to set the following labels: foo.node-restriction.kubernetes.io/foo, node-restriction.kubernetes.io/foo, other.k8s.io/foo, other.kubernetes.io/foo`,
		},
		{
			name:       "allow update of my node",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObj, mynodeObj, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow delete of my node",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of my node status",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObj, mynodeObj, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "forbid create of my node with non-nil configSource",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(mynodeObjConfigA, nil, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "is not allowed to create pods with a non-nil configSource",
		},
		{
			name:       "forbid update of my node: nil configSource to new non-nil configSource",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObjConfigA, mynodeObj, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "update configSource to a new non-nil configSource",
		},
		{
			name:       "forbid update of my node: non-nil configSource to new non-nil configSource",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObjConfigB, mynodeObjConfigA, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "update configSource to a new non-nil configSource",
		},
		{
			name:       "allow update of my node: non-nil configSource unchanged",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObjConfigA, mynodeObjConfigA, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of my node: non-nil configSource to nil configSource",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObj, mynodeObjConfigA, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of my node: no change to taints",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObjTaintA, mynodeObjTaintA, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of my node: add allowed labels",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(setAllowedUpdateLabels(mynodeObj, ""), mynodeObj, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of my node: remove allowed labels",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObj, setAllowedUpdateLabels(mynodeObj, ""), nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of my node: modify allowed labels",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(setAllowedUpdateLabels(mynodeObj, "b"), setAllowedUpdateLabels(mynodeObj, "a"), nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of my node: no change to labels",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(setAllLabels(mynodeObj, ""), setAllLabels(mynodeObj, ""), nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of my node: add allowed labels while forbidden labels exist unmodified",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(setAllLabels(mynodeObj, ""), setForbiddenUpdateLabels(mynodeObj, ""), nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of my node: remove allowed labels while forbidden labels exist unmodified",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(setForbiddenUpdateLabels(mynodeObj, ""), setAllLabels(mynodeObj, ""), nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "forbid update of my node: add taints",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObjTaintA, mynodeObj, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "is not allowed to modify taints",
		},
		{
			name:       "forbid update of my node: remove taints",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObj, mynodeObjTaintA, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "is not allowed to modify taints",
		},
		{
			name:       "forbid update of my node: change taints",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObjTaintA, mynodeObjTaintB, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "is not allowed to modify taints",
		},
		{
			name:       "forbid update of my node: add labels",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(setForbiddenUpdateLabels(mynodeObj, ""), mynodeObj, nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        `is not allowed to modify labels: foo.node-restriction.kubernetes.io/foo, node-restriction.kubernetes.io/foo, other.k8s.io/foo, other.kubernetes.io/foo`,
		},
		{
			name:       "forbid update of my node: remove labels",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(mynodeObj, setForbiddenUpdateLabels(mynodeObj, ""), nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        `is not allowed to modify labels: foo.node-restriction.kubernetes.io/foo, node-restriction.kubernetes.io/foo, other.k8s.io/foo, other.kubernetes.io/foo`,
		},
		{
			name:       "forbid update of my node: change labels",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(setForbiddenUpdateLabels(mynodeObj, "new"), setForbiddenUpdateLabels(mynodeObj, "old"), nodeKind, mynodeObj.Namespace, mynodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        `is not allowed to modify labels: foo.node-restriction.kubernetes.io/foo, node-restriction.kubernetes.io/foo, other.k8s.io/foo, other.kubernetes.io/foo`,
		},

		// Other node object
		{
			name:       "forbid create of other node",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(othernodeObj, nil, nodeKind, othernodeObj.Namespace, othernodeObj.Name, nodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "is not allowed to modify node",
		},
		{
			name:       "forbid create of other node pulling name from object",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(othernodeObj, nil, nodeKind, othernodeObj.Namespace, "", nodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "is not allowed to modify node",
		},
		{
			name:       "forbid update of other node",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(othernodeObj, othernodeObj, nodeKind, othernodeObj.Namespace, othernodeObj.Name, nodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "is not allowed to modify node",
		},
		{
			name:       "forbid delete of other node",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, nodeKind, othernodeObj.Namespace, othernodeObj.Name, nodeResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "is not allowed to modify node",
		},
		{
			name:       "forbid update of other node status",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(othernodeObj, othernodeObj, nodeKind, othernodeObj.Namespace, othernodeObj.Name, nodeResource, "status", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "is not allowed to modify node",
		},

		// Service accounts
		{
			name:       "forbid create of unbound token",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(makeTokenRequest("", "", []string{"foo"}), nil, tokenrequestKind, "ns", "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "not bound to a pod",
		},
		{
			name:       "forbid create of token bound to nonexistent pod",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(makeTokenRequest("nopod", "someuid", []string{"foo"}), nil, tokenrequestKind, "ns", "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "not found",
		},
		{
			name:       "forbid create of token bound to pod without uid",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypod.Name, "", []string{"foo"}), nil, tokenrequestKind, "ns", "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "pod binding without a uid",
		},
		{
			name:       "forbid create of token bound to pod scheduled on another node",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(makeTokenRequest(coreotherpod.Name, coreotherpod.UID, []string{"foo"}), nil, tokenrequestKind, coreotherpod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "pod scheduled on a different node",
		},
		{
			name:       "allow create of token bound to pod scheduled this node",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypod.Name, coremypod.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},
		// Service accounts tests with token audience restrictions
		{
			name:       "allow create of token when audience in PSAT volume and ServiceAccountNodeAudienceRestriction is enabled, empty audience",
			podsGetter: existingPods,
			features:   feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithProjectedServiceAccountEmptyAudience.Name, coremypodWithProjectedServiceAccountEmptyAudience.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},
		{
			name:       "allow create of token when audience in PSAT volume and ServiceAccountNodeAudienceRestriction is enabled, single audience",
			podsGetter: existingPods,
			features:   feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithProjectedServiceAccount.Name, coremypodWithProjectedServiceAccount.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},
		{
			name:       "forbid create of token with multiple audiences in token request and ServiceAccountNodeAudienceRestriction is enabled",
			podsGetter: existingPods,
			features:   feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithProjectedServiceAccount.Name, coremypodWithProjectedServiceAccount.UID, []string{"foo", "bar"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "node may only request 0 or 1 audiences",
		},
		{
			name:       "forbid create of token when audience not in pod spec and ServiceAccountNodeAudienceRestriction is enabled",
			podsGetter: existingPods,
			features:   feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypod.Name, coremypod.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        `serviceaccounts "mysa" is forbidden: audience "foo" not found in pod spec volume`,
		},
		{
			name:            "allow create of token when audience in pod --> csi --> driver --> tokenrequest with audience and ServiceAccountNodeAudienceRestriction is enabled",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithCSI.Name, v1mypodWithCSI.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},
		{
			name:            "forbid create of token when audience in pod --> csi --> driver --> tokenrequest does not have audience and ServiceAccountNodeAudienceRestriction is enabled",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithCSI.Name, v1mypodWithCSI.UID, []string{"bar"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        `audience "bar" not found in pod spec volume`,
		},
		{
			name:            "forbid create of token when audience in pod --> csi --> driver --> tokenrequest with audience and ServiceAccountNodeAudienceRestriction is enabled, csidriver not found",
			podsGetter:      existingPods,
			csiDriverGetter: noexistingCSIDriverLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithCSI.Name, v1mypodWithCSI.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        `error validating audience "foo": csidriver.storage.k8s.io "com.example.csi.mydriver" not found`,
		},
		{
			name:            "allow create of token when audience in pod --> pvc --> pv --> csi --> driver --> tokenrequest with audience and ServiceAccountNodeAudienceRestriction is enabled",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			pvcGetter:       pvcLister,
			pvGetter:        pvLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithPVCRefCSI.Name, v1mypodWithPVCRefCSI.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},
		{
			name:            "forbid create of token when audience in pod --> pvc --> pv --> csi --> driver --> tokenrequest does not have audience and ServiceAccountNodeAudienceRestriction is enabled",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			pvcGetter:       pvcLister,
			pvGetter:        pvLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithPVCRefCSI.Name, v1mypodWithPVCRefCSI.UID, []string{"bar"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        `audience "bar" not found in pod spec volume`,
		},
		{
			name:            "forbid create of token when audience in pod --> pvc --> pv --> csi --> driver --> tokenrequest with audience and ServiceAccountNodeAudienceRestriction is enabled, pvc not found",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			pvcGetter:       noexistingPVCLister,
			pvGetter:        pvLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithPVCRefCSI.Name, v1mypodWithPVCRefCSI.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        `error validating audience "foo": persistentvolumeclaim "pvclaim" not found`,
		},
		{
			name:            "forbid create of token when audience in pod --> pvc --> pv --> csi --> driver --> tokenrequest with audience and ServiceAccountNodeAudienceRestriction is enabled, pv not found",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			pvcGetter:       pvcLister,
			pvGetter:        noexistingPVLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithPVCRefCSI.Name, v1mypodWithPVCRefCSI.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        `error validating audience "foo": persistentvolume "pvname" not found`,
		},
		{
			name:            "allow create of token when audience in pod --> ephemeral --> pvc --> pv --> csi --> driver --> tokenrequest with audience and ServiceAccountNodeAudienceRestriction is enabled",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			pvcGetter:       pvcLister,
			pvGetter:        pvLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithEphemeralVolume.Name, v1mypodWithEphemeralVolume.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},
		{
			name:            "forbid create of token when audience in pod --> ephemeral --> pvc --> pv --> csi --> driver --> tokenrequest does not have audience and ServiceAccountNodeAudienceRestriction is enabled",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			pvcGetter:       pvcLister,
			pvGetter:        pvLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithEphemeralVolume.Name, v1mypodWithEphemeralVolume.UID, []string{"bar"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        `audience "bar" not found in pod spec volume`,
		},
		{
			name:            "allow create of token when ServiceAccountNodeAudienceRestriction is disabled, pvc not found should not be checked",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			pvcGetter:       noexistingPVCLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithPVCAndCSI.Name, v1mypodWithPVCAndCSI.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},
		{
			name:            "allow create of token when audience in pod --> csi --> driver --> tokenrequest with audience and ServiceAccountNodeAudienceRestriction is enabled, pv not found",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			pvcGetter:       pvcLister,
			pvGetter:        noexistingPVLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, true)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithPVCAndCSI.Name, v1mypodWithPVCAndCSI.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},
		{
			name:            "allow create of token when ServiceAccountNodeAudienceRestriction is disabled, pv not found should not be checked",
			podsGetter:      existingPods,
			csiDriverGetter: csiDriverLister,
			pvcGetter:       pvcLister,
			pvGetter:        noexistingPVLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, false)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithPVCAndCSI.Name, v1mypodWithPVCAndCSI.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},
		{
			name:            "allow create of token when ServiceAccountNodeAudienceRestriction is disabled, csidriver not found should not be checked",
			podsGetter:      existingPods,
			csiDriverGetter: noexistingCSIDriverLister,
			features:        feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ServiceAccountNodeAudienceRestriction, false)
			},
			attributes: admission.NewAttributesRecord(makeTokenRequest(coremypodWithCSI.Name, v1mypodWithCSI.UID, []string{"foo"}), nil, tokenrequestKind, coremypod.Namespace, "mysa", svcacctResource, "token", admission.Create, &metav1.CreateOptions{}, false, mynode),
		},

		// Unrelated objects
		{
			name:       "allow create of unrelated object",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(&api.ConfigMap{}, nil, configmapKind, "myns", "mycm", configmapResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow update of unrelated object",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(&api.ConfigMap{}, &api.ConfigMap{}, configmapKind, "myns", "mycm", configmapResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allow delete of unrelated object",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, configmapKind, "myns", "mycm", configmapResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "",
		},

		// Unrelated user
		{
			name:       "allow unrelated user creating a normal pod unbound",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreunboundpod, nil, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, bob),
			err:        "",
		},
		{
			name:       "allow unrelated user update of normal pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreunboundpod, coreunboundpod, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "", admission.Update, &metav1.UpdateOptions{}, false, bob),
			err:        "",
		},
		{
			name:       "allow unrelated user delete of normal pod unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "", admission.Delete, &metav1.DeleteOptions{}, false, bob),
			err:        "",
		},
		{
			name:       "allow unrelated user create of normal pod status unbound",
			podsGetter: noExistingPods,
			attributes: admission.NewAttributesRecord(coreunboundpod, nil, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "status", admission.Create, &metav1.CreateOptions{}, false, bob),
			err:        "",
		},
		{
			name:       "allow unrelated user update of normal pod status unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(coreunboundpod, coreunboundpod, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "status", admission.Update, &metav1.UpdateOptions{}, false, bob),
			err:        "",
		},
		{
			name:       "allow unrelated user delete of normal pod status unbound",
			podsGetter: existingPods,
			attributes: admission.NewAttributesRecord(nil, nil, podKind, coreunboundpod.Namespace, coreunboundpod.Name, podResource, "status", admission.Delete, &metav1.DeleteOptions{}, false, bob),
			err:        "",
		},
		// Node leases
		{
			name:       "disallowed create lease in namespace other than kube-node-lease - feature enabled",
			attributes: admission.NewAttributesRecord(leaseWrongNS, nil, leaseKind, leaseWrongNS.Namespace, leaseWrongNS.Name, leaseResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "forbidden: ",
		},
		{
			name:       "disallowed update lease in namespace other than kube-node-lease - feature enabled",
			attributes: admission.NewAttributesRecord(leaseWrongNS, leaseWrongNS, leaseKind, leaseWrongNS.Namespace, leaseWrongNS.Name, leaseResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: ",
		},
		{
			name:       "disallowed delete lease in namespace other than kube-node-lease - feature enabled",
			attributes: admission.NewAttributesRecord(nil, nil, leaseKind, leaseWrongNS.Namespace, leaseWrongNS.Name, leaseResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: ",
		},
		{
			name:       "disallowed create another node's lease - feature enabled",
			attributes: admission.NewAttributesRecord(leaseWrongName, nil, leaseKind, leaseWrongName.Namespace, leaseWrongName.Name, leaseResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "forbidden: ",
		},
		{
			name:       "disallowed update another node's lease - feature enabled",
			attributes: admission.NewAttributesRecord(leaseWrongName, leaseWrongName, leaseKind, leaseWrongName.Namespace, leaseWrongName.Name, leaseResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: ",
		},
		{
			name:       "disallowed delete another node's lease - feature enabled",
			attributes: admission.NewAttributesRecord(nil, nil, leaseKind, leaseWrongName.Namespace, leaseWrongName.Name, leaseResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: ",
		},
		{
			name:       "allowed create node lease - feature enabled",
			attributes: admission.NewAttributesRecord(lease, nil, leaseKind, lease.Namespace, lease.Name, leaseResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allowed update node lease - feature enabled",
			attributes: admission.NewAttributesRecord(lease, lease, leaseKind, lease.Namespace, lease.Name, leaseResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allowed delete node lease - feature enabled",
			attributes: admission.NewAttributesRecord(nil, nil, leaseKind, lease.Namespace, lease.Name, leaseResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "",
		},
		// CSINode
		{
			name:       "disallowed create another node's CSINode",
			attributes: admission.NewAttributesRecord(nodeInfoWrongName, nil, csiNodeKind, nodeInfoWrongName.Namespace, nodeInfoWrongName.Name, csiNodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "forbidden: ",
		},
		{
			name:       "disallowed update another node's CSINode",
			attributes: admission.NewAttributesRecord(nodeInfoWrongName, nodeInfoWrongName, csiNodeKind, nodeInfoWrongName.Namespace, nodeInfoWrongName.Name, csiNodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "forbidden: ",
		},
		{
			name:       "disallowed delete another node's CSINode",
			attributes: admission.NewAttributesRecord(nil, nil, csiNodeKind, nodeInfoWrongName.Namespace, nodeInfoWrongName.Name, csiNodeResource, "", admission.Delete, &metav1.DeleteOptions{}, false, mynode),
			err:        "forbidden: ",
		},
		{
			name:       "allowed create node CSINode",
			attributes: admission.NewAttributesRecord(nodeInfo, nil, csiNodeKind, nodeInfo.Namespace, nodeInfo.Name, csiNodeResource, "", admission.Create, &metav1.CreateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allowed update node CSINode",
			attributes: admission.NewAttributesRecord(nodeInfo, nodeInfo, csiNodeKind, nodeInfo.Namespace, nodeInfo.Name, csiNodeResource, "", admission.Update, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		{
			name:       "allowed delete node CSINode",
			attributes: admission.NewAttributesRecord(nil, nil, csiNodeKind, nodeInfo.Namespace, nodeInfo.Name, csiNodeResource, "", admission.Delete, &metav1.UpdateOptions{}, false, mynode),
			err:        "",
		},
		// CSR
		{
			name:       "allowed CSR create correct node serving",
			attributes: createCSRAttributes("system:node:mynode", certificatesapi.KubeletServingSignerName, true, privKey, mynode),
			err:        "",
		},
		{
			name:       "allowed CSR create correct node client",
			attributes: createCSRAttributes("system:node:mynode", certificatesapi.KubeAPIServerClientKubeletSignerName, true, privKey, mynode),
			err:        "",
		},
		{
			name:       "allowed CSR create non-node CSR",
			attributes: createCSRAttributes("some-other-identity", certificatesapi.KubeAPIServerClientSignerName, true, privKey, mynode),
			err:        "",
		},
		{
			name:       "deny CSR create incorrect node",
			attributes: createCSRAttributes("system:node:othernode", certificatesapi.KubeletServingSignerName, true, privKey, mynode),
			err:        "forbidden: can only create a node CSR with CN=system:node:mynode",
		},
		{
			name:       "allow CSR create incorrect node with feature gate disabled",
			attributes: createCSRAttributes("system:node:othernode", certificatesapi.KubeletServingSignerName, true, privKey, mynode),
			err:        "",
			features:   feature.DefaultFeatureGate,
			setupFunc: func(t *testing.T) {
				t.Helper()
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.AllowInsecureKubeletCertificateSigningRequests, true)
			},
		},
		{
			name:       "deny CSR create invalid",
			attributes: createCSRAttributes("system:node:mynode", certificatesapi.KubeletServingSignerName, false, privKey, mynode),
			err:        "unable to parse csr: asn1: syntax error: sequence truncated",
		},
	}
	for _, tt := range tests {
		tt.nodesGetter = existingNodes
		tt.run(t)
	}
}

func Test_nodePlugin_Admit_OwnerReference(t *testing.T) {
	expectedNodeIndex := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
	expectedNodeIndex.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mynode", UID: "mynode-uid"}})
	expectedNode := corev1lister.NewNodeLister(expectedNodeIndex)

	unexpectedNodeIndex := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
	unexpectedNodeIndex.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mynode", UID: "mynode-unexpected-uid"}})
	unexpectedNode := corev1lister.NewNodeLister(unexpectedNodeIndex)

	noNodesIndex := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
	noNodes := corev1lister.NewNodeLister(noNodesIndex)

	noExistingPodsIndex := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
	noExistingPods := corev1lister.NewPodLister(noExistingPodsIndex)

	mynode := &user.DefaultInfo{Name: "system:node:mynode", Groups: []string{"system:nodes"}}
	validOwner := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Node",
		Name:       "mynode",
		UID:        "mynode-uid",
		Controller: pointer.BoolPtr(true),
	}
	invalidName := validOwner
	invalidName.Name = "other"
	invalidKind := validOwner
	invalidKind.Kind = "Pod"
	invalidAPI := validOwner
	invalidAPI.APIVersion = "v2"
	invalidControllerNil := validOwner
	invalidControllerNil.Controller = nil
	invalidControllerFalse := validOwner
	invalidControllerFalse.Controller = pointer.BoolPtr(false)
	invalidBlockDeletion := validOwner
	invalidBlockDeletion.BlockOwnerDeletion = pointer.BoolPtr(true)

	tests := []struct {
		name        string
		owners      []metav1.OwnerReference
		nodesGetter corev1lister.NodeLister
		expectErr   string
	}{
		{
			name:      "no owner",
			owners:    nil,
			expectErr: "pods \"test\" is forbidden: node \"mynode\" can only create pods with an owner reference set to itself",
		},
		{
			name:   "valid owner",
			owners: []metav1.OwnerReference{validOwner},
		},
		{
			name:      "duplicate owner",
			owners:    []metav1.OwnerReference{validOwner, validOwner},
			expectErr: "can only create pods with a single owner reference set to itself",
		},
		{
			name:      "invalid name",
			owners:    []metav1.OwnerReference{invalidName},
			expectErr: "can only create pods with an owner reference set to itself",
		},
		{
			name:        "invalid UID",
			owners:      []metav1.OwnerReference{validOwner},
			nodesGetter: unexpectedNode,
			expectErr:   "UID mismatch",
		},
		{
			name:        "node not found",
			owners:      []metav1.OwnerReference{validOwner},
			nodesGetter: noNodes,
			expectErr:   "not found",
		},
		{
			name:      "invalid API version",
			owners:    []metav1.OwnerReference{invalidAPI},
			expectErr: "can only create pods with an owner reference set to itself",
		},
		{
			name:      "invalid kind",
			owners:    []metav1.OwnerReference{invalidKind},
			expectErr: "can only create pods with an owner reference set to itself",
		},
		{
			name:      "nil controller",
			owners:    []metav1.OwnerReference{invalidControllerNil},
			expectErr: "can only create pods with a controller owner reference set to itself",
		},
		{
			name:      "false controller",
			owners:    []metav1.OwnerReference{invalidControllerFalse},
			expectErr: "can only create pods with a controller owner reference set to itself",
		},
		{
			name:      "invalid blockOwnerDeletion",
			owners:    []metav1.OwnerReference{invalidBlockDeletion},
			expectErr: "must not set blockOwnerDeletion on an owner reference",
		},
	}

	for _, test := range tests {
		if test.nodesGetter == nil {
			test.nodesGetter = expectedNode
		}

		pod, _ := makeTestPod("ns", "test", "mynode", true)
		pod.OwnerReferences = test.owners
		a := &admitTestCase{
			name:        test.name,
			podsGetter:  noExistingPods,
			nodesGetter: test.nodesGetter,
			attributes:  createPodAttributes(pod, mynode),
			err:         test.expectErr,
		}
		a.run(t)
	}
}

func Test_getModifiedLabels(t *testing.T) {
	tests := []struct {
		name string
		a    map[string]string
		b    map[string]string
		want sets.String
	}{
		{
			name: "empty",
			a:    nil,
			b:    nil,
			want: sets.NewString(),
		},
		{
			name: "no change",
			a:    map[string]string{"x": "1", "y": "2", "z": "3"},
			b:    map[string]string{"x": "1", "y": "2", "z": "3"},
			want: sets.NewString(),
		},
		{
			name: "added",
			a:    map[string]string{},
			b:    map[string]string{"a": "0"},
			want: sets.NewString("a"),
		},
		{
			name: "removed",
			a:    map[string]string{"z": "3"},
			b:    map[string]string{},
			want: sets.NewString("z"),
		},
		{
			name: "changed",
			a:    map[string]string{"z": "3"},
			b:    map[string]string{"z": "4"},
			want: sets.NewString("z"),
		},
		{
			name: "added empty",
			a:    map[string]string{},
			b:    map[string]string{"a": ""},
			want: sets.NewString("a"),
		},
		{
			name: "removed empty",
			a:    map[string]string{"z": ""},
			b:    map[string]string{},
			want: sets.NewString("z"),
		},
		{
			name: "changed to empty",
			a:    map[string]string{"z": "3"},
			b:    map[string]string{"z": ""},
			want: sets.NewString("z"),
		},
		{
			name: "changed from empty",
			a:    map[string]string{"z": ""},
			b:    map[string]string{"z": "3"},
			want: sets.NewString("z"),
		},
		{
			name: "added, removed, and changed",
			a:    map[string]string{"a": "1", "b": "2"},
			b:    map[string]string{"a": "2", "c": "3"},
			want: sets.NewString("a", "b", "c"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getModifiedLabels(tt.a, tt.b); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getModifiedLabels() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAdmitPVCStatus(t *testing.T) {
	expectedNodeIndex := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
	expectedNodeIndex.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mynode", UID: "mynode-uid"}})
	expectedNode := corev1lister.NewNodeLister(expectedNodeIndex)
	noExistingPodsIndex := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
	noExistingPods := corev1lister.NewPodLister(noExistingPodsIndex)
	mynode := &user.DefaultInfo{Name: "system:node:mynode", Groups: []string{"system:nodes"}}

	nodeExpansionFailed := api.PersistentVolumeClaimNodeResizeInfeasible

	tests := []struct {
		name                    string
		resource                schema.GroupVersionResource
		subresource             string
		newObj                  runtime.Object
		oldObj                  runtime.Object
		expansionFeatureEnabled bool
		recoveryFeatureEnabled  bool
		expectError             string
	}{
		{
			name: "should not allow full pvc update from nodes",
			oldObj: makeTestPVC(
				api.PersistentVolumeClaimResizing,
				"10G", nil,
			),
			subresource: "",
			newObj: makeTestPVC(
				"", "10G", nil,
			),
			expectError: "is forbidden: may only update PVC status",
		},
		{
			name: "should allow capacity and condition updates, if expansion is enabled",
			oldObj: makeTestPVC(
				api.PersistentVolumeClaimResizing,
				"10G", nil,
			),
			expansionFeatureEnabled: true,
			subresource:             "status",
			newObj: makeTestPVC(
				api.PersistentVolumeClaimFileSystemResizePending,
				"10G", nil,
			),
			expectError: "",
		},
		{
			name: "should not allow updates to allocatedResources with just expansion enabled",
			oldObj: makeTestPVC(
				api.PersistentVolumeClaimResizing,
				"10G", nil,
			),
			subresource:             "status",
			expansionFeatureEnabled: true,
			newObj: makeTestPVC(
				api.PersistentVolumeClaimFileSystemResizePending,
				"15G", nil,
			),
			expectError: "is not allowed to update fields other than",
		},
		{
			name: "should allow updates to allocatedResources with expansion and recovery enabled",
			oldObj: makeTestPVC(
				api.PersistentVolumeClaimResizing,
				"10G", nil,
			),
			subresource:             "status",
			expansionFeatureEnabled: true,
			recoveryFeatureEnabled:  true,
			newObj: makeTestPVC(
				api.PersistentVolumeClaimFileSystemResizePending,
				"15G", nil,
			),
			expectError: "",
		},
		{
			name: "should allow updates to resizeStatus with expansion and recovery enabled",
			oldObj: makeTestPVC(
				api.PersistentVolumeClaimResizing,
				"10G", nil,
			),
			subresource:             "status",
			expansionFeatureEnabled: true,
			recoveryFeatureEnabled:  true,
			newObj: makeTestPVC(
				api.PersistentVolumeClaimResizing,
				"10G", &nodeExpansionFailed,
			),
			expectError: "",
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			operation := admission.Update
			apiResource := api.SchemeGroupVersion.WithResource("persistentvolumeclaims")
			attributes := admission.NewAttributesRecord(
				test.newObj, test.oldObj, schema.GroupVersionKind{},
				metav1.NamespaceDefault, "foo", apiResource, test.subresource, operation, &metav1.CreateOptions{}, false, mynode)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.RecoverVolumeExpansionFailure, test.recoveryFeatureEnabled)
			a := &admitTestCase{
				name:        test.name,
				podsGetter:  noExistingPods,
				nodesGetter: expectedNode,
				attributes:  attributes,
				features:    feature.DefaultFeatureGate,
				err:         test.expectError,
			}
			a.run(t)
		})

	}
}

func makeTestPVC(
	condition api.PersistentVolumeClaimConditionType,
	allocatedResources string,
	resizeStatus *api.ClaimResourceStatus) *api.PersistentVolumeClaim {
	pvc := &api.PersistentVolumeClaim{
		Spec: api.PersistentVolumeClaimSpec{
			VolumeName: "volume1",
			Resources: api.VolumeResourceRequirements{
				Requests: api.ResourceList{
					api.ResourceStorage: resource.MustParse("10G"),
				},
			},
		},
		Status: api.PersistentVolumeClaimStatus{
			Capacity: api.ResourceList{
				api.ResourceStorage: resource.MustParse(allocatedResources),
			},
			Phase: api.ClaimBound,
			AllocatedResources: api.ResourceList{
				api.ResourceStorage: resource.MustParse(allocatedResources),
			},
		},
	}
	if resizeStatus != nil {
		claimStatusMap := map[api.ResourceName]api.ClaimResourceStatus{
			api.ResourceStorage: *resizeStatus,
		}
		pvc.Status.AllocatedResourceStatuses = claimStatusMap
	}

	if len(condition) > 0 {
		pvc.Status.Conditions = []api.PersistentVolumeClaimCondition{
			{
				Type:   condition,
				Status: api.ConditionTrue,
			},
		}
	}

	return pvc
}

func createPodAttributes(pod *api.Pod, user user.Info) admission.Attributes {
	podResource := api.Resource("pods").WithVersion("v1")
	podKind := api.Kind("Pod").WithVersion("v1")
	return admission.NewAttributesRecord(pod, nil, podKind, pod.Namespace, pod.Name, podResource, "", admission.Create, &metav1.CreateOptions{}, false, user)
}

func createCSRAttributes(cn, signer string, validCsr bool, key any, user user.Info) admission.Attributes {
	csrResource := certificatesapi.Resource("certificatesigningrequests").WithVersion("v1")
	csrKind := certificatesapi.Kind("CertificateSigningRequest").WithVersion("v1")

	csrPem := []byte("-----BEGIN CERTIFICATE REQUEST-----\n-----END CERTIFICATE REQUEST-----")
	if validCsr {
		structuredCsr := x509.CertificateRequest{
			Subject: pkix.Name{
				CommonName: cn,
			},
		}
		csrDer, _ := x509.CreateCertificateRequest(rand.Reader, &structuredCsr, key)
		csrPem = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDer})
	}

	csreq := &certificatesapi.CertificateSigningRequest{
		Spec: certificatesapi.CertificateSigningRequestSpec{
			Request:    csrPem,
			SignerName: signer,
		},
	}
	return admission.NewAttributesRecord(csreq, nil, csrKind, "", "", csrResource, "", admission.Create, &metav1.CreateOptions{}, false, user)

}

func TestAdmitResourceSlice(t *testing.T) {
	apiResource := resourceapi.SchemeGroupVersion.WithResource("resourceslices")
	nodename := "mynode"
	mynode := &user.DefaultInfo{Name: "system:node:" + nodename, Groups: []string{"system:nodes"}}
	createErr := "can only create ResourceSlice with the same NodeName as the requesting node"
	deleteErr := "can only delete ResourceSlice with the same NodeName as the requesting node"

	sliceNode := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "something",
		},
		Spec: resourceapi.ResourceSliceSpec{
			NodeName: nodename,
		},
	}
	sliceOtherNode := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "something",
		},
		Spec: resourceapi.ResourceSliceSpec{
			NodeName: nodename + "-other",
		},
	}
	sliceNoNode := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "something",
		},
		Spec: resourceapi.ResourceSliceSpec{
			NodeName: "",
		},
	}

	tests := map[string]struct {
		operation      admission.Operation
		options        runtime.Object
		obj, oldObj    runtime.Object
		featureEnabled bool
		expectError    string
	}{
		"create allowed, enabled": {
			operation:      admission.Create,
			options:        &metav1.CreateOptions{},
			obj:            sliceNode,
			featureEnabled: true,
			expectError:    "",
		},
		"create disallowed, enabled": {
			operation:      admission.Create,
			options:        &metav1.CreateOptions{},
			obj:            sliceOtherNode,
			featureEnabled: true,
			expectError:    createErr,
		},
		"create disallowed, no node name, enabled": {
			operation:      admission.Create,
			options:        &metav1.CreateOptions{},
			obj:            sliceNoNode,
			featureEnabled: true,
			expectError:    createErr,
		},
		"create allowed, disabled": {
			operation:      admission.Create,
			options:        &metav1.CreateOptions{},
			obj:            sliceNode,
			featureEnabled: false,
			expectError:    "",
		},
		"create disallowed, disabled": {
			operation:      admission.Create,
			options:        &metav1.CreateOptions{},
			obj:            sliceOtherNode,
			featureEnabled: false,
			expectError:    createErr,
		},
		"create disallowed, no node name, disabled": {
			operation:      admission.Create,
			options:        &metav1.CreateOptions{},
			obj:            sliceNoNode,
			featureEnabled: false,
			expectError:    createErr,
		},
		"update allowed, same node": {
			operation:      admission.Update,
			options:        &metav1.UpdateOptions{},
			obj:            sliceNode,
			featureEnabled: true,
			expectError:    "",
		},
		"update allowed, other node": {
			operation:      admission.Update,
			options:        &metav1.UpdateOptions{},
			obj:            sliceOtherNode,
			featureEnabled: true,
			expectError:    "",
		},
		"update allowed, no node": {
			operation:      admission.Update,
			options:        &metav1.UpdateOptions{},
			obj:            sliceNoNode,
			featureEnabled: true,
			expectError:    "",
		},
		"delete allowed, enabled": {
			operation:      admission.Delete,
			options:        &metav1.DeleteOptions{},
			oldObj:         sliceNode,
			featureEnabled: true,
			expectError:    "",
		},
		"delete disallowed, enabled": {
			operation:      admission.Delete,
			options:        &metav1.DeleteOptions{},
			oldObj:         sliceOtherNode,
			featureEnabled: true,
			expectError:    deleteErr,
		},
		"delete disallowed, no node name, enabled": {
			operation:      admission.Delete,
			options:        &metav1.DeleteOptions{},
			oldObj:         sliceNoNode,
			featureEnabled: true,
			expectError:    deleteErr,
		},
		"delete allowed, disabled": {
			operation:      admission.Delete,
			options:        &metav1.DeleteOptions{},
			oldObj:         sliceNode,
			featureEnabled: false,
			expectError:    "",
		},
		"delete disallowed, disabled": {
			operation:      admission.Delete,
			options:        &metav1.DeleteOptions{},
			oldObj:         sliceOtherNode,
			featureEnabled: false,
			expectError:    deleteErr,
		},
		"delete disallowed, no node name, disabled": {
			operation:      admission.Delete,
			options:        &metav1.DeleteOptions{},
			oldObj:         sliceNoNode,
			featureEnabled: false,
			expectError:    deleteErr,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			attributes := admission.NewAttributesRecord(
				test.obj, test.oldObj, schema.GroupVersionKind{},
				"", "foo", apiResource, "", test.operation, test.options, false, mynode)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.DynamicResourceAllocation, test.featureEnabled)
			a := &admitTestCase{
				name:       name,
				attributes: attributes,
				features:   feature.DefaultFeatureGate,
				err:        test.expectError,
			}
			a.run(t)
		})

	}
}

func checkNilError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
