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

package validation

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeletapis "k8s.io/kubelet/pkg/apis"
	podtest "k8s.io/kubernetes/pkg/api/pod/testing"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/capabilities"
	"k8s.io/kubernetes/pkg/features"
	utilpointer "k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
)

const (
	dnsLabelErrMsg                    = "a lowercase RFC 1123 label must consist of"
	dnsSubdomainLabelErrMsg           = "a lowercase RFC 1123 subdomain"
	envVarNameErrMsg                  = "a valid environment variable name must consist of"
	relaxedEnvVarNameFmtErrMsg string = "a valid environment variable name must consist only of printable ASCII characters other than '='"
	noUserNamespace                   = false
)

var (
	containerRestartPolicyAlways    = core.ContainerRestartPolicyAlways
	containerRestartPolicyOnFailure = core.ContainerRestartPolicy("OnFailure")
	containerRestartPolicyNever     = core.ContainerRestartPolicy("Never")
	containerRestartPolicyInvalid   = core.ContainerRestartPolicy("invalid")
	containerRestartPolicyEmpty     = core.ContainerRestartPolicy("")
	defaultGracePeriod              = ptr.To[int64](30)
)

type topologyPair struct {
	key   string
	value string
}

func line() string {
	_, _, line, ok := runtime.Caller(1)
	var s string
	if ok {
		s = fmt.Sprintf("%d", line)
	} else {
		s = "<??>"
	}
	return s
}

func prettyErrorList(errs field.ErrorList) string {
	var s string
	for _, e := range errs {
		s += fmt.Sprintf("\t%s\n", e)
	}
	return s
}

func newHostPathType(pathType string) *core.HostPathType {
	hostPathType := new(core.HostPathType)
	*hostPathType = core.HostPathType(pathType)
	return hostPathType
}

func testVolume(name string, namespace string, spec core.PersistentVolumeSpec) *core.PersistentVolume {
	objMeta := metav1.ObjectMeta{Name: name}
	if namespace != "" {
		objMeta.Namespace = namespace
	}

	return &core.PersistentVolume{
		ObjectMeta: objMeta,
		Spec:       spec,
	}
}

func TestValidatePersistentVolumes(t *testing.T) {
	validMode := core.PersistentVolumeFilesystem
	invalidMode := core.PersistentVolumeMode("fakeVolumeMode")
	scenarios := map[string]struct {
		isExpectedFailure           bool
		enableVolumeAttributesClass bool
		volume                      *core.PersistentVolume
	}{
		"good-volume": {
			isExpectedFailure: false,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"good-volume-with-capacity-unit": {
			isExpectedFailure: false,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10Gi"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"good-volume-without-capacity-unit": {
			isExpectedFailure: false,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"good-volume-with-storage-class": {
			isExpectedFailure: false,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				StorageClassName: "valid",
			}),
		},
		"good-volume-with-retain-policy": {
			isExpectedFailure: false,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				PersistentVolumeReclaimPolicy: core.PersistentVolumeReclaimRetain,
			}),
		},
		"good-volume-with-volume-mode": {
			isExpectedFailure: false,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				VolumeMode: &validMode,
			}),
		},
		"invalid-accessmode": {
			isExpectedFailure: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{"fakemode"},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"invalid-reclaimpolicy": {
			isExpectedFailure: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				PersistentVolumeReclaimPolicy: "fakeReclaimPolicy",
			}),
		},
		"invalid-volume-mode": {
			isExpectedFailure: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				VolumeMode: &invalidMode,
			}),
		},
		"with-read-write-once-pod": {
			isExpectedFailure: false,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{"ReadWriteOncePod"},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"with-read-write-once-pod-and-others": {
			isExpectedFailure: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{"ReadWriteOncePod", "ReadWriteMany"},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"unexpected-namespace": {
			isExpectedFailure: true,
			volume: testVolume("foo", "unexpected-namespace", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"missing-volume-source": {
			isExpectedFailure: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			}),
		},
		"bad-name": {
			isExpectedFailure: true,
			volume: testVolume("123*Bad(Name", "unexpected-namespace", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"missing-name": {
			isExpectedFailure: true,
			volume: testVolume("", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"missing-capacity": {
			isExpectedFailure: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"bad-volume-zero-capacity": {
			isExpectedFailure: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("0"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"missing-accessmodes": {
			isExpectedFailure: true,
			volume: testVolume("goodname", "missing-accessmodes", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			}),
		},
		"too-many-sources": {
			isExpectedFailure: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("5G"),
				},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
					GCEPersistentDisk: &core.GCEPersistentDiskVolumeSource{PDName: "foo", FSType: "ext4"},
				},
			}),
		},
		"host mount of / with recycle reclaim policy": {
			isExpectedFailure: true,
			volume: testVolume("bad-recycle-do-not-want", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				PersistentVolumeReclaimPolicy: core.PersistentVolumeReclaimRecycle,
			}),
		},
		"host mount of / with recycle reclaim policy 2": {
			isExpectedFailure: true,
			volume: testVolume("bad-recycle-do-not-want", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/a/..",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				PersistentVolumeReclaimPolicy: core.PersistentVolumeReclaimRecycle,
			}),
		},
		"invalid-storage-class-name": {
			isExpectedFailure: true,
			volume: testVolume("invalid-storage-class-name", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				StorageClassName: "-invalid-",
			}),
		},
		"bad-hostpath-volume-backsteps": {
			isExpectedFailure: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo/..",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				StorageClassName: "backstep-hostpath",
			}),
		},
		"volume-node-affinity": {
			isExpectedFailure: false,
			volume:            testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
		},
		"volume-empty-node-affinity": {
			isExpectedFailure: true,
			volume:            testVolumeWithNodeAffinity(&core.VolumeNodeAffinity{}),
		},
		"volume-bad-node-affinity": {
			isExpectedFailure: true,
			volume: testVolumeWithNodeAffinity(
				&core.VolumeNodeAffinity{
					Required: &core.NodeSelector{
						NodeSelectorTerms: []core.NodeSelectorTerm{{
							MatchExpressions: []core.NodeSelectorRequirement{{
								Operator: core.NodeSelectorOpIn,
								Values:   []string{"test-label-value"},
							}},
						}},
					},
				}),
		},
		"invalid-volume-attributes-class-name": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: true,
			volume: testVolume("invalid-volume-attributes-class-name", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				StorageClassName:          "invalid",
				VolumeAttributesClassName: ptr.To("-invalid-"),
			}),
		},
		"invalid-empty-volume-attributes-class-name": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: true,
			volume: testVolume("invalid-empty-volume-attributes-class-name", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				StorageClassName:          "invalid",
				VolumeAttributesClassName: ptr.To(""),
			}),
		},
		"volume-with-good-volume-attributes-class-and-matched-volume-resource-when-feature-gate-is-on": {
			isExpectedFailure:           false,
			enableVolumeAttributesClass: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					CSI: &core.CSIPersistentVolumeSource{
						Driver:       "test-driver",
						VolumeHandle: "test-123",
					},
				},
				StorageClassName:          "valid",
				VolumeAttributesClassName: ptr.To("valid"),
			}),
		},
		"volume-with-good-volume-attributes-class-and-mismatched-volume-resource-when-feature-gate-is-on": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: true,
			volume: testVolume("foo", "", core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				StorageClassName:          "valid",
				VolumeAttributesClassName: ptr.To("valid"),
			}),
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolumeAttributesClass, scenario.enableVolumeAttributesClass)

			opts := ValidationOptionsForPersistentVolume(scenario.volume, nil)
			errs := ValidatePersistentVolume(scenario.volume, opts)
			if len(errs) == 0 && scenario.isExpectedFailure {
				t.Errorf("Unexpected success for scenario: %s", name)
			}
			if len(errs) > 0 && !scenario.isExpectedFailure {
				t.Errorf("Unexpected failure for scenario: %s - %+v", name, errs)
			}
		})
	}

}

func TestValidatePersistentVolumeSpec(t *testing.T) {
	fsmode := core.PersistentVolumeFilesystem
	blockmode := core.PersistentVolumeBlock
	scenarios := map[string]struct {
		isExpectedFailure bool
		isInlineSpec      bool
		pvSpec            *core.PersistentVolumeSpec
	}{
		"pv-pvspec-valid": {
			isExpectedFailure: false,
			isInlineSpec:      false,
			pvSpec: &core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				StorageClassName:              "testclass",
				PersistentVolumeReclaimPolicy: core.PersistentVolumeReclaimRecycle,
				AccessModes:                   []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				VolumeMode:   &fsmode,
				NodeAffinity: simpleVolumeNodeAffinity("foo", "bar"),
			},
		},
		"inline-pvspec-with-capacity": {
			isExpectedFailure: true,
			isInlineSpec:      true,
			pvSpec: &core.PersistentVolumeSpec{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
				PersistentVolumeSource: core.PersistentVolumeSource{
					CSI: &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123", ReadOnly: true},
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			},
		},
		"inline-pvspec-with-podSec": {
			isExpectedFailure: true,
			isInlineSpec:      true,
			pvSpec: &core.PersistentVolumeSpec{
				PersistentVolumeSource: core.PersistentVolumeSource{
					CSI: &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123", ReadOnly: true},
				},
				AccessModes:      []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				StorageClassName: "testclass",
			},
		},
		"inline-pvspec-with-non-fs-volume-mode": {
			isExpectedFailure: true,
			isInlineSpec:      true,
			pvSpec: &core.PersistentVolumeSpec{
				PersistentVolumeSource: core.PersistentVolumeSource{
					CSI: &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123", ReadOnly: true},
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				VolumeMode:  &blockmode,
			},
		},
		"inline-pvspec-with-non-retain-reclaim-policy": {
			isExpectedFailure: true,
			isInlineSpec:      true,
			pvSpec: &core.PersistentVolumeSpec{
				PersistentVolumeReclaimPolicy: core.PersistentVolumeReclaimRecycle,
				PersistentVolumeSource: core.PersistentVolumeSource{
					CSI: &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123", ReadOnly: true},
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			},
		},
		"inline-pvspec-with-node-affinity": {
			isExpectedFailure: true,
			isInlineSpec:      true,
			pvSpec: &core.PersistentVolumeSpec{
				PersistentVolumeSource: core.PersistentVolumeSource{
					CSI: &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123", ReadOnly: true},
				},
				AccessModes:  []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				NodeAffinity: simpleVolumeNodeAffinity("foo", "bar"),
			},
		},
		"inline-pvspec-with-non-csi-source": {
			isExpectedFailure: true,
			isInlineSpec:      true,
			pvSpec: &core.PersistentVolumeSpec{
				PersistentVolumeSource: core.PersistentVolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/foo",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			},
		},
		"inline-pvspec-valid-with-access-modes-and-mount-options": {
			isExpectedFailure: false,
			isInlineSpec:      true,
			pvSpec: &core.PersistentVolumeSpec{
				PersistentVolumeSource: core.PersistentVolumeSource{
					CSI: &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123", ReadOnly: true},
				},
				AccessModes:  []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
				MountOptions: []string{"soft", "read-write"},
			},
		},
		"inline-pvspec-valid-with-access-modes": {
			isExpectedFailure: false,
			isInlineSpec:      true,
			pvSpec: &core.PersistentVolumeSpec{
				PersistentVolumeSource: core.PersistentVolumeSource{
					CSI: &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123", ReadOnly: true},
				},
				AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			},
		},
		"inline-pvspec-with-missing-acess-modes": {
			isExpectedFailure: true,
			isInlineSpec:      true,
			pvSpec: &core.PersistentVolumeSpec{
				PersistentVolumeSource: core.PersistentVolumeSource{
					CSI: &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123", ReadOnly: true},
				},
				MountOptions: []string{"soft", "read-write"},
			},
		},
		"invalid-node-affinity": {
			isExpectedFailure: true,
			isInlineSpec:      false,
			pvSpec: &core.PersistentVolumeSpec{
				NodeAffinity: &core.VolumeNodeAffinity{
					Required: &core.NodeSelector{
						NodeSelectorTerms: []core.NodeSelectorTerm{{
							MatchExpressions: []core.NodeSelectorRequirement{{
								Key:      "foo",
								Operator: core.NodeSelectorOpIn,
								Values:   []string{"-1"},
							}},
						}},
					},
				},
			},
		},
	}
	for name, scenario := range scenarios {
		opts := ValidationOptionsForPersistentVolume(&core.PersistentVolume{
			Spec: *scenario.pvSpec.DeepCopy(),
		}, nil)
		errs := ValidatePersistentVolumeSpec(scenario.pvSpec, "", scenario.isInlineSpec, field.NewPath("field"), opts)
		if len(errs) == 0 && scenario.isExpectedFailure {
			t.Errorf("Unexpected success for scenario: %s", name)
		}
		if len(errs) > 0 && !scenario.isExpectedFailure {
			t.Errorf("Unexpected failure for scenario: %s - %+v", name, errs)
		}
	}
}

func TestValidatePersistentVolumeSourceUpdate(t *testing.T) {
	validVolume := testVolume("foo", "", core.PersistentVolumeSpec{
		Capacity: core.ResourceList{
			core.ResourceName(core.ResourceStorage): resource.MustParse("1G"),
		},
		AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
		PersistentVolumeSource: core.PersistentVolumeSource{
			HostPath: &core.HostPathVolumeSource{
				Path: "/foo",
				Type: newHostPathType(string(core.HostPathDirectory)),
			},
		},
		StorageClassName: "valid",
	})
	validPvSourceNoUpdate := validVolume.DeepCopy()
	invalidPvSourceUpdateType := validVolume.DeepCopy()
	invalidPvSourceUpdateType.Spec.PersistentVolumeSource = core.PersistentVolumeSource{
		FlexVolume: &core.FlexPersistentVolumeSource{
			Driver: "kubernetes.io/blue",
			FSType: "ext4",
		},
	}
	invalidPvSourceUpdateDeep := validVolume.DeepCopy()
	invalidPvSourceUpdateDeep.Spec.PersistentVolumeSource = core.PersistentVolumeSource{
		HostPath: &core.HostPathVolumeSource{
			Path: "/updated",
			Type: newHostPathType(string(core.HostPathDirectory)),
		},
	}

	validCSIVolume := testVolume("csi-volume", "", core.PersistentVolumeSpec{
		Capacity: core.ResourceList{
			core.ResourceName(core.ResourceStorage): resource.MustParse("1G"),
		},
		AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
		PersistentVolumeSource: core.PersistentVolumeSource{
			CSI: &core.CSIPersistentVolumeSource{
				Driver:       "come.google.gcepd",
				VolumeHandle: "foobar",
			},
		},
		StorageClassName: "gp2",
	})

	expandSecretRef := &core.SecretReference{
		Name:      "expansion-secret",
		Namespace: "default",
	}

	// shortSecretRef refers to the secretRefs which are validated with IsDNS1035Label
	shortSecretName := "key-name"
	shortSecretRef := &core.SecretReference{
		Name:      shortSecretName,
		Namespace: "default",
	}

	// longSecretRef refers to the secretRefs which are validated with IsDNS1123Subdomain
	longSecretName := "key-name.example.com"
	longSecretRef := &core.SecretReference{
		Name:      longSecretName,
		Namespace: "default",
	}

	// invalidSecrets missing name, namespace and both
	inValidSecretRef := &core.SecretReference{
		Name:      "",
		Namespace: "",
	}
	invalidSecretRefmissingName := &core.SecretReference{
		Name:      "",
		Namespace: "default",
	}
	invalidSecretRefmissingNamespace := &core.SecretReference{
		Name:      "invalidnamespace",
		Namespace: "",
	}

	scenarios := map[string]struct {
		isExpectedFailure bool
		oldVolume         *core.PersistentVolume
		newVolume         *core.PersistentVolume
	}{
		"condition-no-update": {
			isExpectedFailure: false,
			oldVolume:         validVolume,
			newVolume:         validPvSourceNoUpdate,
		},
		"condition-update-source-type": {
			isExpectedFailure: true,
			oldVolume:         validVolume,
			newVolume:         invalidPvSourceUpdateType,
		},
		"condition-update-source-deep": {
			isExpectedFailure: true,
			oldVolume:         validVolume,
			newVolume:         invalidPvSourceUpdateDeep,
		},
		"csi-expansion-enabled-with-pv-secret": {
			isExpectedFailure: false,
			oldVolume:         validCSIVolume,
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, expandSecretRef, "controllerExpand"),
		},
		"csi-expansion-enabled-with-old-pv-secret": {
			isExpectedFailure: true,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, expandSecretRef, "controllerExpand"),
			newVolume: getCSIVolumeWithSecret(validCSIVolume, &core.SecretReference{
				Name:      "foo-secret",
				Namespace: "default",
			}, "controllerExpand"),
		},
		"csi-expansion-enabled-with-shortSecretRef": {
			isExpectedFailure: false,
			oldVolume:         validCSIVolume,
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "controllerExpand"),
		},
		"csi-expansion-enabled-with-longSecretRef": {
			isExpectedFailure: false, // updating controllerExpandSecretRef is allowed only from nil
			oldVolume:         validCSIVolume,
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "controllerExpand"),
		},
		"csi-expansion-enabled-from-shortSecretRef-to-shortSecretRef": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "controllerExpand"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "controllerExpand"),
		},
		"csi-expansion-enabled-from-shortSecretRef-to-longSecretRef": {
			isExpectedFailure: true, // updating controllerExpandSecretRef is allowed only from nil
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "controllerExpand"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "controllerExpand"),
		},
		"csi-expansion-enabled-from-longSecretRef-to-longSecretRef": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "controllerExpand"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "controllerExpand"),
		},
		"csi-cntrlpublish-enabled-with-shortSecretRef": {
			isExpectedFailure: true, // updating secretRef will fail as the object is immutable eventhough the secretRef is valid
			oldVolume:         validCSIVolume,
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "controllerPublish"),
		},
		"csi-cntrlpublish-enabled-with-longSecretRef": {
			isExpectedFailure: true, // updating secretRef will fail as the object is immutable eventhough the secretRef is valid
			oldVolume:         validCSIVolume,
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "controllerPublish"),
		},
		"csi-cntrlpublish-enabled-from-shortSecretRef-to-shortSecretRef": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "controllerPublish"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "controllerPublish"),
		},
		"csi-cntrlpublish-enabled-from-shortSecretRef-to-longSecretRef": {
			isExpectedFailure: true, // updating secretRef will fail as the object is immutable eventhough the secretRef is valid
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "controllerPublish"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "controllerPublish"),
		},
		"csi-cntrlpublish-enabled-from-longSecretRef-to-longSecretRef": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "controllerPublish"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "controllerPublish"),
		},
		"csi-nodepublish-enabled-with-shortSecretRef": {
			isExpectedFailure: true, // updating secretRef will fail as the object is immutable eventhough the secretRef is valid
			oldVolume:         validCSIVolume,
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "nodePublish"),
		},
		"csi-nodepublish-enabled-with-longSecretRef": {
			isExpectedFailure: true, // updating secretRef will fail as the object is immutable eventhough the secretRef is valid
			oldVolume:         validCSIVolume,
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "nodePublish"),
		},
		"csi-nodepublish-enabled-from-shortSecretRef-to-shortSecretRef": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "nodePublish"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "nodePublish"),
		},
		"csi-nodepublish-enabled-from-shortSecretRef-to-longSecretRef": {
			isExpectedFailure: true,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "nodePublish"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "nodePublish"),
		},
		"csi-nodepublish-enabled-from-longSecretRef-to-longSecretRef": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "nodePublish"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "nodePublish"),
		},
		"csi-nodestage-enabled-with-shortSecretRef": {
			isExpectedFailure: true, // updating secretRef will fail as the object is immutable eventhough the secretRef is valid
			oldVolume:         validCSIVolume,
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "nodeStage"),
		},
		"csi-nodestage-enabled-with-longSecretRef": {
			isExpectedFailure: true, // updating secretRef will fail as the object is immutable eventhough the secretRef is valid
			oldVolume:         validCSIVolume,
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "nodeStage"),
		},
		"csi-nodestage-enabled-from-shortSecretRef-to-longSecretRef": {
			isExpectedFailure: true, // updating secretRef will fail as the object is immutable eventhough the secretRef is valid
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "nodeStage"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "nodeStage"),
		},

		// At present, there is no validation exist for nodeStage secretRef in
		// ValidatePersistentVolumeSpec->validateCSIPersistentVolumeSource, due to that, below
		// checks/validations pass!

		"csi-nodestage-enabled-from-invalidSecretRef-to-invalidSecretRef": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, inValidSecretRef, "nodeStage"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, inValidSecretRef, "nodeStage"),
		},
		"csi-nodestage-enabled-from-invalidSecretRefmissingname-to-invalidSecretRefmissingname": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, invalidSecretRefmissingName, "nodeStage"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, invalidSecretRefmissingName, "nodeStage"),
		},
		"csi-nodestage-enabled-from-invalidSecretRefmissingnamespace-to-invalidSecretRefmissingnamespace": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, invalidSecretRefmissingNamespace, "nodeStage"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, invalidSecretRefmissingNamespace, "nodeStage"),
		},
		"csi-nodestage-enabled-from-shortSecretRef-to-shortSecretRef": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "nodeStage"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, shortSecretRef, "nodeStage"),
		},
		"csi-nodestage-enabled-from-longSecretRef-to-longSecretRef": {
			isExpectedFailure: false,
			oldVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "nodeStage"),
			newVolume:         getCSIVolumeWithSecret(validCSIVolume, longSecretRef, "nodeStage"),
		},
	}
	for name, scenario := range scenarios {
		opts := ValidationOptionsForPersistentVolume(scenario.newVolume, scenario.oldVolume)
		errs := ValidatePersistentVolumeUpdate(scenario.newVolume, scenario.oldVolume, opts)
		if len(errs) == 0 && scenario.isExpectedFailure {
			t.Errorf("Unexpected success for scenario: %s", name)
		}
		if len(errs) > 0 && !scenario.isExpectedFailure {
			t.Errorf("Unexpected failure for scenario: %s - %+v", name, errs)
		}
	}
}

func TestValidationOptionsForPersistentVolume(t *testing.T) {
	tests := map[string]struct {
		oldPv                       *core.PersistentVolume
		enableVolumeAttributesClass bool
		expectValidationOpts        PersistentVolumeSpecValidationOptions
	}{
		"nil old pv": {
			oldPv:                nil,
			expectValidationOpts: PersistentVolumeSpecValidationOptions{},
		},
		"nil old pv and feature-gate VolumeAttrributesClass is on": {
			oldPv:                       nil,
			enableVolumeAttributesClass: true,
			expectValidationOpts:        PersistentVolumeSpecValidationOptions{EnableVolumeAttributesClass: true},
		},
		"nil old pv and feature-gate VolumeAttrributesClass is off": {
			oldPv:                       nil,
			enableVolumeAttributesClass: false,
			expectValidationOpts:        PersistentVolumeSpecValidationOptions{EnableVolumeAttributesClass: false},
		},
		"old pv has volumeAttributesClass and feature-gate VolumeAttrributesClass is on": {
			oldPv: &core.PersistentVolume{
				Spec: core.PersistentVolumeSpec{
					VolumeAttributesClassName: ptr.To("foo"),
				},
			},
			enableVolumeAttributesClass: true,
			expectValidationOpts:        PersistentVolumeSpecValidationOptions{EnableVolumeAttributesClass: true},
		},
		"old pv has volumeAttributesClass and feature-gate VolumeAttrributesClass is off": {
			oldPv: &core.PersistentVolume{
				Spec: core.PersistentVolumeSpec{
					VolumeAttributesClassName: ptr.To("foo"),
				},
			},
			enableVolumeAttributesClass: false,
			expectValidationOpts:        PersistentVolumeSpecValidationOptions{EnableVolumeAttributesClass: true},
		},
		"old pv has invalid label-value in node affinity": {
			oldPv: &core.PersistentVolume{
				Spec: core.PersistentVolumeSpec{
					NodeAffinity: &core.VolumeNodeAffinity{
						Required: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "foo",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"-1"},
								}},
							}},
						},
					},
				},
			},
			expectValidationOpts: PersistentVolumeSpecValidationOptions{AllowInvalidLabelValueInRequiredNodeAffinity: true},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolumeAttributesClass, tc.enableVolumeAttributesClass)

			opts := ValidationOptionsForPersistentVolume(nil, tc.oldPv)
			if opts != tc.expectValidationOpts {
				t.Errorf("Expected opts: %+v, received: %+v", opts, tc.expectValidationOpts)
			}
		})
	}
}

func getCSIVolumeWithSecret(pv *core.PersistentVolume, secret *core.SecretReference, secretfield string) *core.PersistentVolume {
	pvCopy := pv.DeepCopy()
	switch secretfield {
	case "controllerExpand":
		pvCopy.Spec.CSI.ControllerExpandSecretRef = secret
	case "controllerPublish":
		pvCopy.Spec.CSI.ControllerPublishSecretRef = secret
	case "nodePublish":
		pvCopy.Spec.CSI.NodePublishSecretRef = secret
	case "nodeStage":
		pvCopy.Spec.CSI.NodeStageSecretRef = secret
	default:
		panic("unknown string")
	}

	return pvCopy
}

func pvcWithVolumeAttributesClassName(vacName *string) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		Spec: core.PersistentVolumeClaimSpec{
			VolumeAttributesClassName: vacName,
		},
	}
}

func pvcWithDataSource(dataSource *core.TypedLocalObjectReference) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		Spec: core.PersistentVolumeClaimSpec{
			DataSource: dataSource,
		},
	}
}
func pvcWithDataSourceRef(ref *core.TypedObjectReference) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		Spec: core.PersistentVolumeClaimSpec{
			DataSourceRef: ref,
		},
	}
}

func pvcTemplateWithVolumeAttributesClassName(vacName *string) *core.PersistentVolumeClaimTemplate {
	return &core.PersistentVolumeClaimTemplate{
		Spec: core.PersistentVolumeClaimSpec{
			VolumeAttributesClassName: vacName,
		},
	}
}

func testLocalVolume(path string, affinity *core.VolumeNodeAffinity) core.PersistentVolumeSpec {
	return core.PersistentVolumeSpec{
		Capacity: core.ResourceList{
			core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
		},
		AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
		PersistentVolumeSource: core.PersistentVolumeSource{
			Local: &core.LocalVolumeSource{
				Path: path,
			},
		},
		NodeAffinity:     affinity,
		StorageClassName: "test-storage-class",
	}
}

func TestValidateLocalVolumes(t *testing.T) {
	scenarios := map[string]struct {
		isExpectedFailure bool
		volume            *core.PersistentVolume
	}{
		"alpha invalid local volume nil annotations": {
			isExpectedFailure: true,
			volume: testVolume(
				"invalid-local-volume-nil-annotations",
				"",
				testLocalVolume("/foo", nil)),
		},
		"valid local volume": {
			isExpectedFailure: false,
			volume: testVolume("valid-local-volume", "",
				testLocalVolume("/foo", simpleVolumeNodeAffinity("foo", "bar"))),
		},
		"invalid local volume no node affinity": {
			isExpectedFailure: true,
			volume: testVolume("invalid-local-volume-no-node-affinity", "",
				testLocalVolume("/foo", nil)),
		},
		"invalid local volume empty path": {
			isExpectedFailure: true,
			volume: testVolume("invalid-local-volume-empty-path", "",
				testLocalVolume("", simpleVolumeNodeAffinity("foo", "bar"))),
		},
		"invalid-local-volume-backsteps": {
			isExpectedFailure: true,
			volume: testVolume("foo", "",
				testLocalVolume("/foo/..", simpleVolumeNodeAffinity("foo", "bar"))),
		},
		"valid-local-volume-relative-path": {
			isExpectedFailure: false,
			volume: testVolume("foo", "",
				testLocalVolume("foo", simpleVolumeNodeAffinity("foo", "bar"))),
		},
	}

	for name, scenario := range scenarios {
		opts := ValidationOptionsForPersistentVolume(scenario.volume, nil)
		errs := ValidatePersistentVolume(scenario.volume, opts)
		if len(errs) == 0 && scenario.isExpectedFailure {
			t.Errorf("Unexpected success for scenario: %s", name)
		}
		if len(errs) > 0 && !scenario.isExpectedFailure {
			t.Errorf("Unexpected failure for scenario: %s - %+v", name, errs)
		}
	}
}

func testVolumeWithVolumeAttributesClass(vacName *string) *core.PersistentVolume {
	return testVolume("test-volume-with-volume-attributes-class", "",
		core.PersistentVolumeSpec{
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			PersistentVolumeSource: core.PersistentVolumeSource{
				CSI: &core.CSIPersistentVolumeSource{
					Driver:       "test-driver",
					VolumeHandle: "test-123",
				},
			},
			StorageClassName:          "test-storage-class",
			VolumeAttributesClassName: vacName,
		})
}

func testVolumeWithNodeAffinity(affinity *core.VolumeNodeAffinity) *core.PersistentVolume {
	return testVolume("test-affinity-volume", "",
		core.PersistentVolumeSpec{
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			PersistentVolumeSource: core.PersistentVolumeSource{
				GCEPersistentDisk: &core.GCEPersistentDiskVolumeSource{
					PDName: "foo",
				},
			},
			StorageClassName: "test-storage-class",
			NodeAffinity:     affinity,
		})
}

func simpleVolumeNodeAffinity(key, value string) *core.VolumeNodeAffinity {
	return &core.VolumeNodeAffinity{
		Required: &core.NodeSelector{
			NodeSelectorTerms: []core.NodeSelectorTerm{{
				MatchExpressions: []core.NodeSelectorRequirement{{
					Key:      key,
					Operator: core.NodeSelectorOpIn,
					Values:   []string{value},
				}},
			}},
		},
	}
}

func multipleVolumeNodeAffinity(terms [][]topologyPair) *core.VolumeNodeAffinity {
	nodeSelectorTerms := []core.NodeSelectorTerm{}
	for _, term := range terms {
		matchExpressions := []core.NodeSelectorRequirement{}
		for _, topology := range term {
			matchExpressions = append(matchExpressions, core.NodeSelectorRequirement{
				Key:      topology.key,
				Operator: core.NodeSelectorOpIn,
				Values:   []string{topology.value},
			})
		}
		nodeSelectorTerms = append(nodeSelectorTerms, core.NodeSelectorTerm{
			MatchExpressions: matchExpressions,
		})
	}

	return &core.VolumeNodeAffinity{
		Required: &core.NodeSelector{
			NodeSelectorTerms: nodeSelectorTerms,
		},
	}
}

func TestValidateVolumeNodeAffinityUpdate(t *testing.T) {
	scenarios := map[string]struct {
		isExpectedFailure bool
		oldPV             *core.PersistentVolume
		newPV             *core.PersistentVolume
	}{
		"nil-nothing-changed": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(nil),
			newPV:             testVolumeWithNodeAffinity(nil),
		},
		"affinity-nothing-changed": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
		},
		"affinity-changed": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar2")),
		},
		"affinity-non-beta-label-changed": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo2", "bar")),
		},
		"affinity-zone-beta-unchanged": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaZone, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaZone, "bar")),
		},
		"affinity-zone-beta-label-to-GA": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaZone, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelTopologyZone, "bar")),
		},
		"affinity-zone-beta-label-to-non-GA": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaZone, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
		},
		"affinity-zone-GA-label-changed": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelTopologyZone, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaZone, "bar")),
		},
		"affinity-region-beta-unchanged": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaRegion, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaRegion, "bar")),
		},
		"affinity-region-beta-label-to-GA": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaRegion, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelTopologyRegion, "bar")),
		},
		"affinity-region-beta-label-to-non-GA": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaRegion, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
		},
		"affinity-region-GA-label-changed": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelTopologyRegion, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelFailureDomainBetaRegion, "bar")),
		},
		"affinity-os-beta-label-unchanged": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelOS, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelOS, "bar")),
		},
		"affinity-os-beta-label-to-GA": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelOS, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelOSStable, "bar")),
		},
		"affinity-os-beta-label-to-non-GA": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelOS, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
		},
		"affinity-os-GA-label-changed": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelOSStable, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelOS, "bar")),
		},
		"affinity-arch-beta-label-unchanged": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelArch, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelArch, "bar")),
		},
		"affinity-arch-beta-label-to-GA": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelArch, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelArchStable, "bar")),
		},
		"affinity-arch-beta-label-to-non-GA": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelArch, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
		},
		"affinity-arch-GA-label-changed": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelArchStable, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(kubeletapis.LabelArch, "bar")),
		},
		"affinity-instanceType-beta-label-unchanged": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelInstanceType, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelInstanceType, "bar")),
		},
		"affinity-instanceType-beta-label-to-GA": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelInstanceType, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelInstanceTypeStable, "bar")),
		},
		"affinity-instanceType-beta-label-to-non-GA": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelInstanceType, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
		},
		"affinity-instanceType-GA-label-changed": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelInstanceTypeStable, "bar")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelInstanceType, "bar")),
		},
		"affinity-same-terms-expressions-length-beta-to-GA-partially-changed": {
			isExpectedFailure: false,
			oldPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
				topologyPair{v1.LabelFailureDomainBetaRegion, "bar"},
			}, {
				topologyPair{kubeletapis.LabelOS, "bar"},
				topologyPair{kubeletapis.LabelArch, "bar"},
				topologyPair{v1.LabelInstanceType, "bar"},
			},
			})),
			newPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelTopologyZone, "bar"},
				topologyPair{v1.LabelFailureDomainBetaRegion, "bar"},
			}, {
				topologyPair{kubeletapis.LabelOS, "bar"},
				topologyPair{v1.LabelArchStable, "bar"},
				topologyPair{v1.LabelInstanceTypeStable, "bar"},
			},
			})),
		},
		"affinity-same-terms-expressions-length-beta-to-non-GA-partially-changed": {
			isExpectedFailure: true,
			oldPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
				topologyPair{v1.LabelFailureDomainBetaRegion, "bar"},
			},
			})),
			newPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
				topologyPair{"foo", "bar"},
			},
			})),
		},
		"affinity-same-terms-expressions-length-GA-partially-changed": {
			isExpectedFailure: true,
			oldPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelTopologyZone, "bar"},
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
				topologyPair{v1.LabelOSStable, "bar"},
			},
			})),
			newPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
				topologyPair{v1.LabelOSStable, "bar"},
			},
			})),
		},
		"affinity-same-terms-expressions-length-beta-fully-changed": {
			isExpectedFailure: false,
			oldPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
				topologyPair{v1.LabelFailureDomainBetaRegion, "bar"},
			}, {
				topologyPair{kubeletapis.LabelOS, "bar"},
				topologyPair{kubeletapis.LabelArch, "bar"},
				topologyPair{v1.LabelInstanceType, "bar"},
			},
			})),
			newPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelTopologyZone, "bar"},
				topologyPair{v1.LabelTopologyRegion, "bar"},
			}, {
				topologyPair{v1.LabelOSStable, "bar"},
				topologyPair{v1.LabelArchStable, "bar"},
				topologyPair{v1.LabelInstanceTypeStable, "bar"},
			},
			})),
		},
		"affinity-same-terms-expressions-length-beta-GA-mixed-fully-changed": {
			isExpectedFailure: true,
			oldPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
				topologyPair{v1.LabelTopologyZone, "bar"},
			},
			})),
			newPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{"foo", "bar"},
			}, {
				topologyPair{v1.LabelTopologyZone, "bar"},
				topologyPair{v1.LabelFailureDomainBetaZone, "bar2"},
			},
			})),
		},
		"affinity-same-terms-length-different-expressions-length-beta-changed": {
			isExpectedFailure: true,
			oldPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
			},
			})),
			newPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{v1.LabelTopologyZone, "bar"},
				topologyPair{v1.LabelFailureDomainBetaRegion, "bar"},
			},
			})),
		},
		"affinity-different-terms-expressions-length-beta-changed": {
			isExpectedFailure: true,
			oldPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{v1.LabelFailureDomainBetaZone, "bar"},
			},
			})),
			newPV: testVolumeWithNodeAffinity(multipleVolumeNodeAffinity([][]topologyPair{{
				topologyPair{v1.LabelTopologyZone, "bar"},
			}, {
				topologyPair{v1.LabelArchStable, "bar"},
			},
			})),
		},
		"nil-to-obj": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(nil),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
		},
		"obj-to-nil": {
			isExpectedFailure: true,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity("foo", "bar")),
			newPV:             testVolumeWithNodeAffinity(nil),
		},
		"old affinity already has invalid label-value": {
			isExpectedFailure: false,
			oldPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelInstanceType, "-1")),
			newPV:             testVolumeWithNodeAffinity(simpleVolumeNodeAffinity(v1.LabelInstanceTypeStable, "-1")),
		},
	}

	for name, scenario := range scenarios {
		originalNewPV := scenario.newPV.DeepCopy()
		originalOldPV := scenario.oldPV.DeepCopy()
		opts := ValidationOptionsForPersistentVolume(scenario.newPV, scenario.oldPV)
		errs := ValidatePersistentVolumeUpdate(scenario.newPV, scenario.oldPV, opts)
		if len(errs) == 0 && scenario.isExpectedFailure {
			t.Errorf("Unexpected success for scenario: %s", name)
		}
		if len(errs) > 0 && !scenario.isExpectedFailure {
			t.Errorf("Unexpected failure for scenario: %s - %+v", name, errs)
		}
		if diff := cmp.Diff(originalNewPV, scenario.newPV); len(diff) > 0 {
			t.Errorf("newPV was modified: %s", diff)
		}
		if diff := cmp.Diff(originalOldPV, scenario.oldPV); len(diff) > 0 {
			t.Errorf("oldPV was modified: %s", diff)
		}
	}
}

func TestValidatePeristentVolumeAttributesClassUpdate(t *testing.T) {
	scenarios := map[string]struct {
		isExpectedFailure           bool
		enableVolumeAttributesClass bool
		oldPV                       *core.PersistentVolume
		newPV                       *core.PersistentVolume
	}{
		"nil-nothing-changed": {
			isExpectedFailure:           false,
			enableVolumeAttributesClass: true,
			oldPV:                       testVolumeWithVolumeAttributesClass(nil),
			newPV:                       testVolumeWithVolumeAttributesClass(nil),
		},
		"vac-nothing-changed": {
			isExpectedFailure:           false,
			enableVolumeAttributesClass: true,
			oldPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
		},
		"vac-changed": {
			isExpectedFailure:           false,
			enableVolumeAttributesClass: true,
			oldPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("bar")),
		},
		"nil-to-string": {
			isExpectedFailure:           false,
			enableVolumeAttributesClass: true,
			oldPV:                       testVolumeWithVolumeAttributesClass(nil),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
		},
		"nil-to-empty-string": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: true,
			oldPV:                       testVolumeWithVolumeAttributesClass(nil),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("")),
		},
		"string-to-nil": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: true,
			oldPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
			newPV:                       testVolumeWithVolumeAttributesClass(nil),
		},
		"string-to-empty-string": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: true,
			oldPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("")),
		},
		"vac-nothing-changed-when-feature-gate-is-off": {
			isExpectedFailure:           false,
			enableVolumeAttributesClass: false,
			oldPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
		},
		"vac-changed-when-feature-gate-is-off": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: false,
			oldPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("bar")),
		},
		"nil-to-string-when-feature-gate-is-off": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: false,
			oldPV:                       testVolumeWithVolumeAttributesClass(nil),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
		},
		"nil-to-empty-string-when-feature-gate-is-off": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: false,
			oldPV:                       testVolumeWithVolumeAttributesClass(nil),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("")),
		},
		"string-to-nil-when-feature-gate-is-off": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: false,
			oldPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
			newPV:                       testVolumeWithVolumeAttributesClass(nil),
		},
		"string-to-empty-string-when-feature-gate-is-off": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: false,
			oldPV:                       testVolumeWithVolumeAttributesClass(ptr.To("foo")),
			newPV:                       testVolumeWithVolumeAttributesClass(ptr.To("")),
		},
	}

	for name, scenario := range scenarios {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolumeAttributesClass, scenario.enableVolumeAttributesClass)

		originalNewPV := scenario.newPV.DeepCopy()
		originalOldPV := scenario.oldPV.DeepCopy()
		opts := ValidationOptionsForPersistentVolume(scenario.newPV, scenario.oldPV)
		errs := ValidatePersistentVolumeUpdate(scenario.newPV, scenario.oldPV, opts)
		if len(errs) == 0 && scenario.isExpectedFailure {
			t.Errorf("Unexpected success for scenario: %s", name)
		}
		if len(errs) > 0 && !scenario.isExpectedFailure {
			t.Errorf("Unexpected failure for scenario: %s - %+v", name, errs)
		}
		if diff := cmp.Diff(originalNewPV, scenario.newPV); len(diff) > 0 {
			t.Errorf("newPV was modified: %s", diff)
		}
		if diff := cmp.Diff(originalOldPV, scenario.oldPV); len(diff) > 0 {
			t.Errorf("oldPV was modified: %s", diff)
		}
	}
}

func testVolumeClaim(name string, namespace string, spec core.PersistentVolumeClaimSpec) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
}

func testVolumeClaimWithStatus(
	name, namespace string,
	spec core.PersistentVolumeClaimSpec,
	status core.PersistentVolumeClaimStatus) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
		Status:     status,
	}
}

func testVolumeClaimStorageClass(name string, namespace string, annval string, spec core.PersistentVolumeClaimSpec) *core.PersistentVolumeClaim {
	annotations := map[string]string{
		v1.BetaStorageClassAnnotation: annval,
	}

	return &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: spec,
	}
}

func testVolumeClaimAnnotation(name string, namespace string, ann string, annval string, spec core.PersistentVolumeClaimSpec) *core.PersistentVolumeClaim {
	annotations := map[string]string{
		ann: annval,
	}

	return &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: spec,
	}
}

func testVolumeClaimStorageClassInSpec(name, namespace, scName string, spec core.PersistentVolumeClaimSpec) *core.PersistentVolumeClaim {
	spec.StorageClassName = &scName
	return &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: spec,
	}
}

func testVolumeClaimStorageClassNilInSpec(name, namespace string, spec core.PersistentVolumeClaimSpec) *core.PersistentVolumeClaim {
	spec.StorageClassName = nil
	return &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: spec,
	}
}

func testVolumeSnapshotDataSourceInSpec(name string, kind string, apiGroup string) *core.PersistentVolumeClaimSpec {
	scName := "csi-plugin"
	dataSourceInSpec := core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		StorageClassName: &scName,
		DataSource: &core.TypedLocalObjectReference{
			APIGroup: &apiGroup,
			Kind:     kind,
			Name:     name,
		},
	}

	return &dataSourceInSpec
}

func TestAlphaVolumeSnapshotDataSource(t *testing.T) {
	successTestCases := []core.PersistentVolumeClaimSpec{
		*testVolumeSnapshotDataSourceInSpec("test_snapshot", "VolumeSnapshot", "snapshot.storage.k8s.io"),
	}
	failedTestCases := []core.PersistentVolumeClaimSpec{
		*testVolumeSnapshotDataSourceInSpec("", "VolumeSnapshot", "snapshot.storage.k8s.io"),
		*testVolumeSnapshotDataSourceInSpec("test_snapshot", "", "snapshot.storage.k8s.io"),
	}

	for _, tc := range successTestCases {
		opts := PersistentVolumeClaimSpecValidationOptions{}
		if errs := ValidatePersistentVolumeClaimSpec(&tc, field.NewPath("spec"), opts); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
	for _, tc := range failedTestCases {
		opts := PersistentVolumeClaimSpecValidationOptions{}
		if errs := ValidatePersistentVolumeClaimSpec(&tc, field.NewPath("spec"), opts); len(errs) == 0 {
			t.Errorf("expected failure: %v", errs)
		}
	}
}

func testVolumeClaimStorageClassInAnnotationAndSpec(name, namespace, scNameInAnn, scName string, spec core.PersistentVolumeClaimSpec) *core.PersistentVolumeClaim {
	spec.StorageClassName = &scName
	return &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{v1.BetaStorageClassAnnotation: scNameInAnn},
		},
		Spec: spec,
	}
}

func testVolumeClaimStorageClassInAnnotationAndNilInSpec(name, namespace, scNameInAnn string, spec core.PersistentVolumeClaimSpec) *core.PersistentVolumeClaim {
	spec.StorageClassName = nil
	return &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{v1.BetaStorageClassAnnotation: scNameInAnn},
		},
		Spec: spec,
	}
}

func testValidatePVC(t *testing.T, ephemeral bool) {
	invalidClassName := "-invalid-"
	validClassName := "valid"
	invalidAPIGroup := "^invalid"
	invalidMode := core.PersistentVolumeMode("fakeVolumeMode")
	validMode := core.PersistentVolumeFilesystem
	goodName := "foo"
	goodNS := "ns"
	if ephemeral {
		// Must be empty for ephemeral inline volumes.
		goodName = ""
		goodNS = ""
	}
	goodClaimSpec := core.PersistentVolumeClaimSpec{
		Selector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "key2",
				Operator: "Exists",
			}},
		},
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		StorageClassName: &validClassName,
		VolumeMode:       &validMode,
	}
	now := metav1.Now()
	ten := int64(10)

	scenarios := map[string]struct {
		isExpectedFailure           bool
		enableVolumeAttributesClass bool
		claim                       *core.PersistentVolumeClaim
	}{
		"good-claim": {
			isExpectedFailure: false,
			claim:             testVolumeClaim(goodName, goodNS, goodClaimSpec),
		},
		"missing-name": {
			isExpectedFailure: !ephemeral,
			claim:             testVolumeClaim("", goodNS, goodClaimSpec),
		},
		"missing-namespace": {
			isExpectedFailure: !ephemeral,
			claim:             testVolumeClaim(goodName, "", goodClaimSpec),
		},
		"with-generate-name": {
			isExpectedFailure: ephemeral,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.GenerateName = "pvc-"
				return claim
			}(),
		},
		"with-uid": {
			isExpectedFailure: ephemeral,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.UID = "ac051fac-2ead-46d9-b8b4-4e0fbeb7455d"
				return claim
			}(),
		},
		"with-resource-version": {
			isExpectedFailure: ephemeral,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.ResourceVersion = "1"
				return claim
			}(),
		},
		"with-generation": {
			isExpectedFailure: ephemeral,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.Generation = 100
				return claim
			}(),
		},
		"with-creation-timestamp": {
			isExpectedFailure: ephemeral,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.CreationTimestamp = now
				return claim
			}(),
		},
		"with-deletion-grace-period-seconds": {
			isExpectedFailure: ephemeral,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.DeletionGracePeriodSeconds = &ten
				return claim
			}(),
		},
		"with-owner-references": {
			isExpectedFailure: ephemeral,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: "v1",
					Kind:       "pod",
					Name:       "foo",
					UID:        "ac051fac-2ead-46d9-b8b4-4e0fbeb7455d",
				},
				}
				return claim
			}(),
		},
		"with-finalizers": {
			isExpectedFailure: ephemeral,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.Finalizers = []string{
					"example.com/foo",
				}
				return claim
			}(),
		},
		"with-managed-fields": {
			isExpectedFailure: ephemeral,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.ManagedFields = []metav1.ManagedFieldsEntry{{
					FieldsType: "FieldsV1",
					Operation:  "Apply",
					APIVersion: "apps/v1",
					Manager:    "foo",
				},
				}
				return claim
			}(),
		},
		"with-good-labels": {
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.Labels = map[string]string{
					"apps.kubernetes.io/name": "test",
				}
				return claim
			}(),
		},
		"with-bad-labels": {
			isExpectedFailure: true,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.Labels = map[string]string{
					"hello-world": "hyphen not allowed",
				}
				return claim
			}(),
		},
		"with-good-annotations": {
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.Labels = map[string]string{
					"foo": "bar",
				}
				return claim
			}(),
		},
		"with-bad-annotations": {
			isExpectedFailure: true,
			claim: func() *core.PersistentVolumeClaim {
				claim := testVolumeClaim(goodName, goodNS, goodClaimSpec)
				claim.Labels = map[string]string{
					"hello-world": "hyphen not allowed",
				}
				return claim
			}(),
		},
		"with-read-write-once-pod": {
			isExpectedFailure: false,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{"ReadWriteOncePod"},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
			}),
		},
		"with-read-write-once-pod-and-others": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{"ReadWriteOncePod", "ReadWriteMany"},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
			}),
		},
		"invalid-claim-zero-capacity": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "key2",
						Operator: "Exists",
					}},
				},
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
					core.ReadOnlyMany,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("0G"),
					},
				},
				StorageClassName: &validClassName,
			}),
		},
		"invalid-label-selector": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "key2",
						Operator: "InvalidOp",
						Values:   []string{"value1", "value2"},
					}},
				},
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
					core.ReadOnlyMany,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
			}),
		},
		"invalid-accessmode": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{"fakemode"},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
			}),
		},
		"no-access-modes": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
			}),
		},
		"no-resource-requests": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
				},
			}),
		},
		"invalid-resource-requests": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
					},
				},
			}),
		},
		"negative-storage-request": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "key2",
						Operator: "Exists",
					}},
				},
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
					core.ReadOnlyMany,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("-10G"),
					},
				},
			}),
		},
		"zero-storage-request": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "key2",
						Operator: "Exists",
					}},
				},
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
					core.ReadOnlyMany,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("0G"),
					},
				},
			}),
		},
		"invalid-storage-class-name": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "key2",
						Operator: "Exists",
					}},
				},
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
					core.ReadOnlyMany,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
				StorageClassName: &invalidClassName,
			}),
		},
		"invalid-volume-mode": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
					core.ReadOnlyMany,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
				VolumeMode: &invalidMode,
			}),
		},
		"mismatch-data-source-and-ref": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
				DataSource: &core.TypedLocalObjectReference{
					Kind: "PersistentVolumeClaim",
					Name: "pvc1",
				},
				DataSourceRef: &core.TypedObjectReference{
					Kind: "PersistentVolumeClaim",
					Name: "pvc2",
				},
			}),
		},
		"invaild-apigroup-in-data-source": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
				DataSource: &core.TypedLocalObjectReference{
					APIGroup: &invalidAPIGroup,
					Kind:     "Foo",
					Name:     "foo1",
				},
			}),
		},
		"invaild-apigroup-in-data-source-ref": {
			isExpectedFailure: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
				DataSourceRef: &core.TypedObjectReference{
					APIGroup: &invalidAPIGroup,
					Kind:     "Foo",
					Name:     "foo1",
				},
			}),
		},
		"invalid-volume-attributes-class-name": {
			isExpectedFailure:           true,
			enableVolumeAttributesClass: true,
			claim: testVolumeClaim(goodName, goodNS, core.PersistentVolumeClaimSpec{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "key2",
						Operator: "Exists",
					}},
				},
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
					core.ReadOnlyMany,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
				VolumeAttributesClassName: &invalidClassName,
			}),
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolumeAttributesClass, scenario.enableVolumeAttributesClass)

			var errs field.ErrorList
			if ephemeral {
				volumes := []core.Volume{{
					Name: "foo",
					VolumeSource: core.VolumeSource{
						Ephemeral: &core.EphemeralVolumeSource{
							VolumeClaimTemplate: &core.PersistentVolumeClaimTemplate{
								ObjectMeta: scenario.claim.ObjectMeta,
								Spec:       scenario.claim.Spec,
							},
						},
					},
				},
				}
				opts := PodValidationOptions{}
				_, errs = ValidateVolumes(volumes, nil, field.NewPath(""), opts)
			} else {
				opts := ValidationOptionsForPersistentVolumeClaim(scenario.claim, nil)
				errs = ValidatePersistentVolumeClaim(scenario.claim, opts)
			}
			if len(errs) == 0 && scenario.isExpectedFailure {
				t.Error("Unexpected success for scenario")
			}
			if len(errs) > 0 && !scenario.isExpectedFailure {
				t.Errorf("Unexpected failure: %+v", errs)
			}
		})
	}
}

func TestValidatePersistentVolumeClaim(t *testing.T) {
	testValidatePVC(t, false)
}

func TestValidateEphemeralVolume(t *testing.T) {
	testValidatePVC(t, true)
}

func TestAlphaPVVolumeModeUpdate(t *testing.T) {
	block := core.PersistentVolumeBlock
	file := core.PersistentVolumeFilesystem

	scenarios := map[string]struct {
		isExpectedFailure bool
		oldPV             *core.PersistentVolume
		newPV             *core.PersistentVolume
	}{
		"valid-update-volume-mode-block-to-block": {
			isExpectedFailure: false,
			oldPV:             createTestVolModePV(&block),
			newPV:             createTestVolModePV(&block),
		},
		"valid-update-volume-mode-file-to-file": {
			isExpectedFailure: false,
			oldPV:             createTestVolModePV(&file),
			newPV:             createTestVolModePV(&file),
		},
		"invalid-update-volume-mode-to-block": {
			isExpectedFailure: true,
			oldPV:             createTestVolModePV(&file),
			newPV:             createTestVolModePV(&block),
		},
		"invalid-update-volume-mode-to-file": {
			isExpectedFailure: true,
			oldPV:             createTestVolModePV(&block),
			newPV:             createTestVolModePV(&file),
		},
		"invalid-update-volume-mode-nil-to-file": {
			isExpectedFailure: true,
			oldPV:             createTestVolModePV(nil),
			newPV:             createTestVolModePV(&file),
		},
		"invalid-update-volume-mode-nil-to-block": {
			isExpectedFailure: true,
			oldPV:             createTestVolModePV(nil),
			newPV:             createTestVolModePV(&block),
		},
		"invalid-update-volume-mode-file-to-nil": {
			isExpectedFailure: true,
			oldPV:             createTestVolModePV(&file),
			newPV:             createTestVolModePV(nil),
		},
		"invalid-update-volume-mode-block-to-nil": {
			isExpectedFailure: true,
			oldPV:             createTestVolModePV(&block),
			newPV:             createTestVolModePV(nil),
		},
		"invalid-update-volume-mode-nil-to-nil": {
			isExpectedFailure: false,
			oldPV:             createTestVolModePV(nil),
			newPV:             createTestVolModePV(nil),
		},
		"invalid-update-volume-mode-empty-to-mode": {
			isExpectedFailure: true,
			oldPV:             createTestPV(),
			newPV:             createTestVolModePV(&block),
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			opts := ValidationOptionsForPersistentVolume(scenario.newPV, scenario.oldPV)
			// ensure we have a resource version specified for updates
			errs := ValidatePersistentVolumeUpdate(scenario.newPV, scenario.oldPV, opts)
			if len(errs) == 0 && scenario.isExpectedFailure {
				t.Errorf("Unexpected success for scenario: %s", name)
			}
			if len(errs) > 0 && !scenario.isExpectedFailure {
				t.Errorf("Unexpected failure for scenario: %s - %+v", name, errs)
			}
		})
	}
}

func TestValidatePersistentVolumeClaimUpdate(t *testing.T) {
	block := core.PersistentVolumeBlock
	file := core.PersistentVolumeFilesystem
	invaildAPIGroup := "^invalid"

	validClaim := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
	})

	validClaimStorageClass := testVolumeClaimStorageClass("foo", "ns", "fast", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	})
	validClaimAnnotation := testVolumeClaimAnnotation("foo", "ns", "description", "foo-description", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	})
	validUpdateClaim := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})
	invalidUpdateClaimResources := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("20G"),
			},
		},
		VolumeName: "volume",
	})
	invalidUpdateClaimAccessModes := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})
	validClaimVolumeModeFile := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
		VolumeMode: &file,
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})
	validClaimVolumeModeBlock := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
		VolumeMode: &block,
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})
	invalidClaimVolumeModeNil := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
		VolumeMode: nil,
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})
	invalidUpdateClaimStorageClass := testVolumeClaimStorageClass("foo", "ns", "fast2", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})
	validUpdateClaimMutableAnnotation := testVolumeClaimAnnotation("foo", "ns", "description", "updated-or-added-foo-description", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})
	validAddClaimAnnotation := testVolumeClaimAnnotation("foo", "ns", "description", "updated-or-added-foo-description", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})
	validSizeUpdate := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("15G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
	})

	invalidSizeUpdate := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("5G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
	})

	unboundSizeUpdate := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("12G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimPending,
	})

	validClaimStorageClassInSpec := testVolumeClaimStorageClassInSpec("foo", "ns", "fast", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	})

	validClaimStorageClassInSpecChanged := testVolumeClaimStorageClassInSpec("foo", "ns", "fast2", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	})

	validClaimStorageClassNil := testVolumeClaimStorageClassNilInSpec("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	})

	invalidClaimStorageClassInSpec := testVolumeClaimStorageClassInSpec("foo", "ns", "fast2", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	})

	validClaimStorageClassInAnnotationAndSpec := testVolumeClaimStorageClassInAnnotationAndSpec(
		"foo", "ns", "fast", "fast", core.PersistentVolumeClaimSpec{
			AccessModes: []core.PersistentVolumeAccessMode{
				core.ReadOnlyMany,
			},
			Resources: core.VolumeResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
			},
		})

	validClaimStorageClassInAnnotationAndNilInSpec := testVolumeClaimStorageClassInAnnotationAndNilInSpec(
		"foo", "ns", "fast", core.PersistentVolumeClaimSpec{
			AccessModes: []core.PersistentVolumeAccessMode{
				core.ReadOnlyMany,
			},
			Resources: core.VolumeResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
			},
		})

	invalidClaimStorageClassInAnnotationAndSpec := testVolumeClaimStorageClassInAnnotationAndSpec(
		"foo", "ns", "fast2", "fast", core.PersistentVolumeClaimSpec{
			AccessModes: []core.PersistentVolumeAccessMode{
				core.ReadOnlyMany,
			},
			Resources: core.VolumeResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
			},
		})

	validClaimRWOPAccessMode := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOncePod,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})

	validClaimRWOPAccessModeAddAnnotation := testVolumeClaimAnnotation("foo", "ns", "description", "updated-or-added-foo-description", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOncePod,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
	})
	validClaimShrinkInitial := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("15G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
		Capacity: core.ResourceList{
			core.ResourceStorage: resource.MustParse("10G"),
		},
	})

	unboundShrink := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("12G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimPending,
		Capacity: core.ResourceList{
			core.ResourceStorage: resource.MustParse("10G"),
		},
	})

	validClaimShrink := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceStorage: resource.MustParse("13G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
		Capacity: core.ResourceList{
			core.ResourceStorage: resource.MustParse("10G"),
		},
	})

	invalidShrinkToStatus := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceStorage: resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
		Capacity: core.ResourceList{
			core.ResourceStorage: resource.MustParse("10G"),
		},
	})

	invalidClaimShrink := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceStorage: resource.MustParse("3G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
		Capacity: core.ResourceList{
			core.ResourceStorage: resource.MustParse("10G"),
		},
	})

	invalidClaimDataSourceAPIGroup := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
		VolumeMode: &file,
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
		DataSource: &core.TypedLocalObjectReference{
			APIGroup: &invaildAPIGroup,
			Kind:     "Foo",
			Name:     "foo",
		},
	})

	invalidClaimDataSourceRefAPIGroup := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
		VolumeMode: &file,
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		VolumeName: "volume",
		DataSourceRef: &core.TypedObjectReference{
			APIGroup: &invaildAPIGroup,
			Kind:     "Foo",
			Name:     "foo",
		},
	})

	validClaimNilVolumeAttributesClass := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
	})
	validClaimEmptyVolumeAttributesClass := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		VolumeAttributesClassName: utilpointer.String(""),
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
	})
	validClaimVolumeAttributesClass1 := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		VolumeAttributesClassName: utilpointer.String("vac1"),
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
	})
	validClaimVolumeAttributesClass2 := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		VolumeAttributesClassName: utilpointer.String("vac2"),
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimBound,
	})

	scenarios := map[string]struct {
		isExpectedFailure           bool
		oldClaim                    *core.PersistentVolumeClaim
		newClaim                    *core.PersistentVolumeClaim
		enableRecoverFromExpansion  bool
		enableVolumeAttributesClass bool
	}{
		"valid-update-volumeName-only": {
			isExpectedFailure: false,
			oldClaim:          validClaim,
			newClaim:          validUpdateClaim,
		},
		"valid-no-op-update": {
			isExpectedFailure: false,
			oldClaim:          validUpdateClaim,
			newClaim:          validUpdateClaim,
		},
		"invalid-update-change-resources-on-bound-claim": {
			isExpectedFailure: true,
			oldClaim:          validUpdateClaim,
			newClaim:          invalidUpdateClaimResources,
		},
		"invalid-update-change-access-modes-on-bound-claim": {
			isExpectedFailure: true,
			oldClaim:          validUpdateClaim,
			newClaim:          invalidUpdateClaimAccessModes,
		},
		"valid-update-volume-mode-block-to-block": {
			isExpectedFailure: false,
			oldClaim:          validClaimVolumeModeBlock,
			newClaim:          validClaimVolumeModeBlock,
		},
		"valid-update-volume-mode-file-to-file": {
			isExpectedFailure: false,
			oldClaim:          validClaimVolumeModeFile,
			newClaim:          validClaimVolumeModeFile,
		},
		"invalid-update-volume-mode-to-block": {
			isExpectedFailure: true,
			oldClaim:          validClaimVolumeModeFile,
			newClaim:          validClaimVolumeModeBlock,
		},
		"invalid-update-volume-mode-to-file": {
			isExpectedFailure: true,
			oldClaim:          validClaimVolumeModeBlock,
			newClaim:          validClaimVolumeModeFile,
		},
		"invalid-update-volume-mode-nil-to-file": {
			isExpectedFailure: true,
			oldClaim:          invalidClaimVolumeModeNil,
			newClaim:          validClaimVolumeModeFile,
		},
		"invalid-update-volume-mode-nil-to-block": {
			isExpectedFailure: true,
			oldClaim:          invalidClaimVolumeModeNil,
			newClaim:          validClaimVolumeModeBlock,
		},
		"invalid-update-volume-mode-block-to-nil": {
			isExpectedFailure: true,
			oldClaim:          validClaimVolumeModeBlock,
			newClaim:          invalidClaimVolumeModeNil,
		},
		"invalid-update-volume-mode-file-to-nil": {
			isExpectedFailure: true,
			oldClaim:          validClaimVolumeModeFile,
			newClaim:          invalidClaimVolumeModeNil,
		},
		"invalid-update-volume-mode-empty-to-mode": {
			isExpectedFailure: true,
			oldClaim:          validClaim,
			newClaim:          validClaimVolumeModeBlock,
		},
		"invalid-update-volume-mode-mode-to-empty": {
			isExpectedFailure: true,
			oldClaim:          validClaimVolumeModeBlock,
			newClaim:          validClaim,
		},
		"invalid-update-change-storage-class-annotation-after-creation": {
			isExpectedFailure: true,
			oldClaim:          validClaimStorageClass,
			newClaim:          invalidUpdateClaimStorageClass,
		},
		"valid-update-mutable-annotation": {
			isExpectedFailure: false,
			oldClaim:          validClaimAnnotation,
			newClaim:          validUpdateClaimMutableAnnotation,
		},
		"valid-update-add-annotation": {
			isExpectedFailure: false,
			oldClaim:          validClaim,
			newClaim:          validAddClaimAnnotation,
		},
		"valid-size-update-resize-disabled": {
			oldClaim: validClaim,
			newClaim: validSizeUpdate,
		},
		"valid-size-update-resize-enabled": {
			isExpectedFailure: false,
			oldClaim:          validClaim,
			newClaim:          validSizeUpdate,
		},
		"invalid-size-update-resize-enabled": {
			isExpectedFailure: true,
			oldClaim:          validClaim,
			newClaim:          invalidSizeUpdate,
		},
		"unbound-size-update-resize-enabled": {
			isExpectedFailure: true,
			oldClaim:          validClaim,
			newClaim:          unboundSizeUpdate,
		},
		"valid-upgrade-storage-class-annotation-to-spec": {
			isExpectedFailure: false,
			oldClaim:          validClaimStorageClass,
			newClaim:          validClaimStorageClassInSpec,
		},
		"valid-upgrade-nil-storage-class-spec-to-spec": {
			isExpectedFailure: false,
			oldClaim:          validClaimStorageClassNil,
			newClaim:          validClaimStorageClassInSpec,
		},
		"invalid-upgrade-not-nil-storage-class-spec-to-spec": {
			isExpectedFailure: true,
			oldClaim:          validClaimStorageClassInSpec,
			newClaim:          validClaimStorageClassInSpecChanged,
		},
		"invalid-upgrade-to-nil-storage-class-spec-to-spec": {
			isExpectedFailure: true,
			oldClaim:          validClaimStorageClassInSpec,
			newClaim:          validClaimStorageClassNil,
		},
		"valid-upgrade-storage-class-annotation-and-nil-spec-to-spec-retro": {
			isExpectedFailure: false,
			oldClaim:          validClaimStorageClassInAnnotationAndNilInSpec,
			newClaim:          validClaimStorageClassInAnnotationAndSpec,
		},
		"invalid-upgrade-storage-class-annotation-and-spec-to-spec-retro": {
			isExpectedFailure: true,
			oldClaim:          validClaimStorageClassInAnnotationAndSpec,
			newClaim:          validClaimStorageClassInSpecChanged,
		},
		"invalid-upgrade-storage-class-annotation-and-no-spec": {
			isExpectedFailure: true,
			oldClaim:          validClaimStorageClassInAnnotationAndNilInSpec,
			newClaim:          validClaimStorageClassInSpecChanged,
		},
		"invalid-upgrade-storage-class-annotation-to-spec": {
			isExpectedFailure: true,
			oldClaim:          validClaimStorageClass,
			newClaim:          invalidClaimStorageClassInSpec,
		},
		"valid-upgrade-storage-class-annotation-to-annotation-and-spec": {
			isExpectedFailure: false,
			oldClaim:          validClaimStorageClass,
			newClaim:          validClaimStorageClassInAnnotationAndSpec,
		},
		"invalid-upgrade-storage-class-annotation-to-annotation-and-spec": {
			isExpectedFailure: true,
			oldClaim:          validClaimStorageClass,
			newClaim:          invalidClaimStorageClassInAnnotationAndSpec,
		},
		"invalid-upgrade-storage-class-in-spec": {
			isExpectedFailure: true,
			oldClaim:          validClaimStorageClassInSpec,
			newClaim:          invalidClaimStorageClassInSpec,
		},
		"invalid-downgrade-storage-class-spec-to-annotation": {
			isExpectedFailure: true,
			oldClaim:          validClaimStorageClassInSpec,
			newClaim:          validClaimStorageClass,
		},
		"valid-update-rwop-used-and-rwop-feature-disabled": {
			isExpectedFailure: false,
			oldClaim:          validClaimRWOPAccessMode,
			newClaim:          validClaimRWOPAccessModeAddAnnotation,
		},
		"valid-expand-shrink-resize-enabled": {
			oldClaim:                   validClaimShrinkInitial,
			newClaim:                   validClaimShrink,
			enableRecoverFromExpansion: true,
		},
		"invalid-expand-shrink-resize-enabled": {
			oldClaim:                   validClaimShrinkInitial,
			newClaim:                   invalidClaimShrink,
			enableRecoverFromExpansion: true,
			isExpectedFailure:          true,
		},
		"invalid-expand-shrink-to-status-resize-enabled": {
			oldClaim:                   validClaimShrinkInitial,
			newClaim:                   invalidShrinkToStatus,
			enableRecoverFromExpansion: true,
			isExpectedFailure:          true,
		},
		"invalid-expand-shrink-recover-disabled": {
			oldClaim:                   validClaimShrinkInitial,
			newClaim:                   validClaimShrink,
			enableRecoverFromExpansion: false,
			isExpectedFailure:          true,
		},
		"unbound-size-shrink-resize-enabled": {
			oldClaim:                   validClaimShrinkInitial,
			newClaim:                   unboundShrink,
			enableRecoverFromExpansion: true,
			isExpectedFailure:          true,
		},
		"allow-update-pvc-when-data-source-used": {
			oldClaim:          invalidClaimDataSourceAPIGroup,
			newClaim:          invalidClaimDataSourceAPIGroup,
			isExpectedFailure: false,
		},
		"allow-update-pvc-when-data-source-ref-used": {
			oldClaim:          invalidClaimDataSourceRefAPIGroup,
			newClaim:          invalidClaimDataSourceRefAPIGroup,
			isExpectedFailure: false,
		},
		"valid-update-volume-attributes-class-from-nil": {
			oldClaim:                    validClaimNilVolumeAttributesClass,
			newClaim:                    validClaimVolumeAttributesClass1,
			enableVolumeAttributesClass: true,
			isExpectedFailure:           false,
		},
		"valid-update-volume-attributes-class-from-empty": {
			oldClaim:                    validClaimEmptyVolumeAttributesClass,
			newClaim:                    validClaimVolumeAttributesClass1,
			enableVolumeAttributesClass: true,
			isExpectedFailure:           false,
		},
		"valid-update-volume-attributes-class": {
			oldClaim:                    validClaimVolumeAttributesClass1,
			newClaim:                    validClaimVolumeAttributesClass2,
			enableVolumeAttributesClass: true,
			isExpectedFailure:           false,
		},
		"invalid-update-volume-attributes-class": {
			oldClaim:                    validClaimVolumeAttributesClass1,
			newClaim:                    validClaimNilVolumeAttributesClass,
			enableVolumeAttributesClass: true,
			isExpectedFailure:           true,
		},
		"invalid-update-volume-attributes-class-to-nil": {
			oldClaim:                    validClaimVolumeAttributesClass1,
			newClaim:                    validClaimNilVolumeAttributesClass,
			enableVolumeAttributesClass: true,
			isExpectedFailure:           true,
		},
		"invalid-update-volume-attributes-class-to-empty": {
			oldClaim:                    validClaimVolumeAttributesClass1,
			newClaim:                    validClaimEmptyVolumeAttributesClass,
			enableVolumeAttributesClass: true,
			isExpectedFailure:           true,
		},
		"invalid-update-volume-attributes-class-when-claim-not-bound": {
			oldClaim: func() *core.PersistentVolumeClaim {
				clone := validClaimVolumeAttributesClass1.DeepCopy()
				clone.Status.Phase = core.ClaimPending
				return clone
			}(),
			newClaim: func() *core.PersistentVolumeClaim {
				clone := validClaimVolumeAttributesClass2.DeepCopy()
				clone.Status.Phase = core.ClaimPending
				return clone
			}(),
			enableVolumeAttributesClass: true,
			isExpectedFailure:           true,
		},
		"invalid-update-volume-attributes-class-to-nil-without-featuregate-enabled": {
			oldClaim:                    validClaimVolumeAttributesClass1,
			newClaim:                    validClaimNilVolumeAttributesClass,
			enableVolumeAttributesClass: false,
			isExpectedFailure:           true,
		},
		"invalid-update-volume-attributes-class-without-featuregate-enabled": {
			oldClaim:                    validClaimVolumeAttributesClass1,
			newClaim:                    validClaimVolumeAttributesClass2,
			enableVolumeAttributesClass: false,
			isExpectedFailure:           true,
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RecoverVolumeExpansionFailure, scenario.enableRecoverFromExpansion)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolumeAttributesClass, scenario.enableVolumeAttributesClass)

			scenario.oldClaim.ResourceVersion = "1"
			scenario.newClaim.ResourceVersion = "1"
			opts := ValidationOptionsForPersistentVolumeClaim(scenario.newClaim, scenario.oldClaim)
			errs := ValidatePersistentVolumeClaimUpdate(scenario.newClaim, scenario.oldClaim, opts)
			if len(errs) == 0 && scenario.isExpectedFailure {
				t.Errorf("Unexpected success for scenario: %s", name)
			}
			if len(errs) > 0 && !scenario.isExpectedFailure {
				t.Errorf("Unexpected failure for scenario: %s - %+v", name, errs)
			}
		})
	}
}

func TestValidationOptionsForPersistentVolumeClaim(t *testing.T) {
	invaildAPIGroup := "^invalid"

	tests := map[string]struct {
		oldPvc                      *core.PersistentVolumeClaim
		enableVolumeAttributesClass bool
		expectValidationOpts        PersistentVolumeClaimSpecValidationOptions
	}{
		"nil pv": {
			oldPvc: nil,
			expectValidationOpts: PersistentVolumeClaimSpecValidationOptions{
				EnableRecoverFromExpansionFailure: true,
				EnableVolumeAttributesClass:       false,
			},
		},
		"invaild apiGroup in dataSource allowed because the old pvc is used": {
			oldPvc: pvcWithDataSource(&core.TypedLocalObjectReference{APIGroup: &invaildAPIGroup}),
			expectValidationOpts: PersistentVolumeClaimSpecValidationOptions{
				EnableRecoverFromExpansionFailure:     true,
				AllowInvalidAPIGroupInDataSourceOrRef: true,
			},
		},
		"invaild apiGroup in dataSourceRef allowed because the old pvc is used": {
			oldPvc: pvcWithDataSourceRef(&core.TypedObjectReference{APIGroup: &invaildAPIGroup}),
			expectValidationOpts: PersistentVolumeClaimSpecValidationOptions{
				EnableRecoverFromExpansionFailure:     true,
				AllowInvalidAPIGroupInDataSourceOrRef: true,
			},
		},
		"volume attributes class allowed because feature enable": {
			oldPvc:                      pvcWithVolumeAttributesClassName(utilpointer.String("foo")),
			enableVolumeAttributesClass: true,
			expectValidationOpts: PersistentVolumeClaimSpecValidationOptions{
				EnableRecoverFromExpansionFailure: true,
				EnableVolumeAttributesClass:       true,
			},
		},
		"volume attributes class validated because used and feature disabled": {
			oldPvc:                      pvcWithVolumeAttributesClassName(utilpointer.String("foo")),
			enableVolumeAttributesClass: false,
			expectValidationOpts: PersistentVolumeClaimSpecValidationOptions{
				EnableRecoverFromExpansionFailure: true,
				EnableVolumeAttributesClass:       true,
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolumeAttributesClass, tc.enableVolumeAttributesClass)

			opts := ValidationOptionsForPersistentVolumeClaim(nil, tc.oldPvc)
			if opts != tc.expectValidationOpts {
				t.Errorf("Expected opts: %+v, received: %+v", tc.expectValidationOpts, opts)
			}
		})
	}
}

func TestValidationOptionsForPersistentVolumeClaimTemplate(t *testing.T) {
	tests := map[string]struct {
		oldPvcTemplate              *core.PersistentVolumeClaimTemplate
		enableVolumeAttributesClass bool
		expectValidationOpts        PersistentVolumeClaimSpecValidationOptions
	}{
		"nil pv": {
			oldPvcTemplate:       nil,
			expectValidationOpts: PersistentVolumeClaimSpecValidationOptions{},
		},
		"volume attributes class allowed because feature enable": {
			oldPvcTemplate:              pvcTemplateWithVolumeAttributesClassName(utilpointer.String("foo")),
			enableVolumeAttributesClass: true,
			expectValidationOpts: PersistentVolumeClaimSpecValidationOptions{
				EnableVolumeAttributesClass: true,
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolumeAttributesClass, tc.enableVolumeAttributesClass)

			opts := ValidationOptionsForPersistentVolumeClaimTemplate(nil, tc.oldPvcTemplate)
			if opts != tc.expectValidationOpts {
				t.Errorf("Expected opts: %+v, received: %+v", opts, tc.expectValidationOpts)
			}
		})
	}
}

func TestValidateKeyToPath(t *testing.T) {
	testCases := []struct {
		kp      core.KeyToPath
		ok      bool
		errtype field.ErrorType
	}{{
		kp: core.KeyToPath{Key: "k", Path: "p"},
		ok: true,
	}, {
		kp: core.KeyToPath{Key: "k", Path: "p/p/p/p"},
		ok: true,
	}, {
		kp: core.KeyToPath{Key: "k", Path: "p/..p/p../p..p"},
		ok: true,
	}, {
		kp: core.KeyToPath{Key: "k", Path: "p", Mode: utilpointer.Int32(0644)},
		ok: true,
	}, {
		kp:      core.KeyToPath{Key: "", Path: "p"},
		ok:      false,
		errtype: field.ErrorTypeRequired,
	}, {
		kp:      core.KeyToPath{Key: "k", Path: ""},
		ok:      false,
		errtype: field.ErrorTypeRequired,
	}, {
		kp:      core.KeyToPath{Key: "k", Path: "..p"},
		ok:      false,
		errtype: field.ErrorTypeInvalid,
	}, {
		kp:      core.KeyToPath{Key: "k", Path: "../p"},
		ok:      false,
		errtype: field.ErrorTypeInvalid,
	}, {
		kp:      core.KeyToPath{Key: "k", Path: "p/../p"},
		ok:      false,
		errtype: field.ErrorTypeInvalid,
	}, {
		kp:      core.KeyToPath{Key: "k", Path: "p/.."},
		ok:      false,
		errtype: field.ErrorTypeInvalid,
	}, {
		kp:      core.KeyToPath{Key: "k", Path: "p", Mode: utilpointer.Int32(01000)},
		ok:      false,
		errtype: field.ErrorTypeInvalid,
	}, {
		kp:      core.KeyToPath{Key: "k", Path: "p", Mode: utilpointer.Int32(-1)},
		ok:      false,
		errtype: field.ErrorTypeInvalid,
	},
	}

	for i, tc := range testCases {
		errs := validateKeyToPath(&tc.kp, field.NewPath("field"))
		if tc.ok && len(errs) > 0 {
			t.Errorf("[%d] unexpected errors: %v", i, errs)
		} else if !tc.ok && len(errs) == 0 {
			t.Errorf("[%d] expected error type %v", i, tc.errtype)
		} else if len(errs) > 1 {
			t.Errorf("[%d] expected only one error, got %d", i, len(errs))
		} else if !tc.ok {
			if errs[0].Type != tc.errtype {
				t.Errorf("[%d] expected error type %v, got %v", i, tc.errtype, errs[0].Type)
			}
		}
	}
}

func TestValidateNFSVolumeSource(t *testing.T) {
	testCases := []struct {
		name      string
		nfs       *core.NFSVolumeSource
		errtype   field.ErrorType
		errfield  string
		errdetail string
	}{{
		name:     "missing server",
		nfs:      &core.NFSVolumeSource{Server: "", Path: "/tmp"},
		errtype:  field.ErrorTypeRequired,
		errfield: "server",
	}, {
		name:     "missing path",
		nfs:      &core.NFSVolumeSource{Server: "my-server", Path: ""},
		errtype:  field.ErrorTypeRequired,
		errfield: "path",
	}, {
		name:      "abs path",
		nfs:       &core.NFSVolumeSource{Server: "my-server", Path: "tmp"},
		errtype:   field.ErrorTypeInvalid,
		errfield:  "path",
		errdetail: "must be an absolute path",
	},
	}

	for i, tc := range testCases {
		errs := validateNFSVolumeSource(tc.nfs, field.NewPath("field"))

		if len(errs) > 0 && tc.errtype == "" {
			t.Errorf("[%d: %q] unexpected error(s): %v", i, tc.name, errs)
		} else if len(errs) == 0 && tc.errtype != "" {
			t.Errorf("[%d: %q] expected error type %v", i, tc.name, tc.errtype)
		} else if len(errs) >= 1 {
			if errs[0].Type != tc.errtype {
				t.Errorf("[%d: %q] expected error type %v, got %v", i, tc.name, tc.errtype, errs[0].Type)
			} else if !strings.HasSuffix(errs[0].Field, "."+tc.errfield) {
				t.Errorf("[%d: %q] expected error on field %q, got %q", i, tc.name, tc.errfield, errs[0].Field)
			} else if !strings.Contains(errs[0].Detail, tc.errdetail) {
				t.Errorf("[%d: %q] expected error detail %q, got %q", i, tc.name, tc.errdetail, errs[0].Detail)
			}
		}
	}
}

func TestValidateGlusterfs(t *testing.T) {
	testCases := []struct {
		name     string
		gfs      *core.GlusterfsVolumeSource
		errtype  field.ErrorType
		errfield string
	}{{
		name:     "missing endpointname",
		gfs:      &core.GlusterfsVolumeSource{EndpointsName: "", Path: "/tmp"},
		errtype:  field.ErrorTypeRequired,
		errfield: "endpoints",
	}, {
		name:     "missing path",
		gfs:      &core.GlusterfsVolumeSource{EndpointsName: "my-endpoint", Path: ""},
		errtype:  field.ErrorTypeRequired,
		errfield: "path",
	}, {
		name:     "missing endpointname and path",
		gfs:      &core.GlusterfsVolumeSource{EndpointsName: "", Path: ""},
		errtype:  field.ErrorTypeRequired,
		errfield: "endpoints",
	},
	}

	for i, tc := range testCases {
		errs := validateGlusterfsVolumeSource(tc.gfs, field.NewPath("field"))

		if len(errs) > 0 && tc.errtype == "" {
			t.Errorf("[%d: %q] unexpected error(s): %v", i, tc.name, errs)
		} else if len(errs) == 0 && tc.errtype != "" {
			t.Errorf("[%d: %q] expected error type %v", i, tc.name, tc.errtype)
		} else if len(errs) >= 1 {
			if errs[0].Type != tc.errtype {
				t.Errorf("[%d: %q] expected error type %v, got %v", i, tc.name, tc.errtype, errs[0].Type)
			} else if !strings.HasSuffix(errs[0].Field, "."+tc.errfield) {
				t.Errorf("[%d: %q] expected error on field %q, got %q", i, tc.name, tc.errfield, errs[0].Field)
			}
		}
	}
}

func TestValidateGlusterfsPersistentVolumeSource(t *testing.T) {
	var epNs *string
	namespace := ""
	epNs = &namespace

	testCases := []struct {
		name     string
		gfs      *core.GlusterfsPersistentVolumeSource
		errtype  field.ErrorType
		errfield string
	}{{
		name:     "missing endpointname",
		gfs:      &core.GlusterfsPersistentVolumeSource{EndpointsName: "", Path: "/tmp"},
		errtype:  field.ErrorTypeRequired,
		errfield: "endpoints",
	}, {
		name:     "missing path",
		gfs:      &core.GlusterfsPersistentVolumeSource{EndpointsName: "my-endpoint", Path: ""},
		errtype:  field.ErrorTypeRequired,
		errfield: "path",
	}, {
		name:     "non null endpointnamespace with empty string",
		gfs:      &core.GlusterfsPersistentVolumeSource{EndpointsName: "my-endpoint", Path: "/tmp", EndpointsNamespace: epNs},
		errtype:  field.ErrorTypeInvalid,
		errfield: "endpointsNamespace",
	}, {
		name:     "missing endpointname and path",
		gfs:      &core.GlusterfsPersistentVolumeSource{EndpointsName: "", Path: ""},
		errtype:  field.ErrorTypeRequired,
		errfield: "endpoints",
	},
	}

	for i, tc := range testCases {
		errs := validateGlusterfsPersistentVolumeSource(tc.gfs, field.NewPath("field"))

		if len(errs) > 0 && tc.errtype == "" {
			t.Errorf("[%d: %q] unexpected error(s): %v", i, tc.name, errs)
		} else if len(errs) == 0 && tc.errtype != "" {
			t.Errorf("[%d: %q] expected error type %v", i, tc.name, tc.errtype)
		} else if len(errs) >= 1 {
			if errs[0].Type != tc.errtype {
				t.Errorf("[%d: %q] expected error type %v, got %v", i, tc.name, tc.errtype, errs[0].Type)
			} else if !strings.HasSuffix(errs[0].Field, "."+tc.errfield) {
				t.Errorf("[%d: %q] expected error on field %q, got %q", i, tc.name, tc.errfield, errs[0].Field)
			}
		}
	}
}

func TestValidateCSIVolumeSource(t *testing.T) {
	testCases := []struct {
		name     string
		csi      *core.CSIVolumeSource
		errtype  field.ErrorType
		errfield string
	}{{
		name: "all required fields ok",
		csi:  &core.CSIVolumeSource{Driver: "test-driver"},
	}, {
		name:     "missing driver name",
		csi:      &core.CSIVolumeSource{Driver: ""},
		errtype:  field.ErrorTypeRequired,
		errfield: "driver",
	}, {
		name: "driver name: ok no punctuations",
		csi:  &core.CSIVolumeSource{Driver: "comgooglestoragecsigcepd"},
	}, {
		name: "driver name: ok dot only",
		csi:  &core.CSIVolumeSource{Driver: "io.kubernetes.storage.csi.flex"},
	}, {
		name: "driver name: ok dash only",
		csi:  &core.CSIVolumeSource{Driver: "io-kubernetes-storage-csi-flex"},
	}, {
		name:     "driver name: invalid underscore",
		csi:      &core.CSIVolumeSource{Driver: "io_kubernetes_storage_csi_flex"},
		errtype:  field.ErrorTypeInvalid,
		errfield: "driver",
	}, {
		name:     "driver name: invalid dot underscores",
		csi:      &core.CSIVolumeSource{Driver: "io.kubernetes.storage_csi.flex"},
		errtype:  field.ErrorTypeInvalid,
		errfield: "driver",
	}, {
		name: "driver name: ok beginning with number",
		csi:  &core.CSIVolumeSource{Driver: "2io.kubernetes.storage-csi.flex"},
	}, {
		name: "driver name: ok ending with number",
		csi:  &core.CSIVolumeSource{Driver: "io.kubernetes.storage-csi.flex2"},
	}, {
		name:     "driver name: invalid dot dash underscores",
		csi:      &core.CSIVolumeSource{Driver: "io.kubernetes-storage.csi_flex"},
		errtype:  field.ErrorTypeInvalid,
		errfield: "driver",
	},

		{
			name: "driver name: ok length 1",
			csi:  &core.CSIVolumeSource{Driver: "a"},
		}, {
			name:     "driver name: invalid length > 63",
			csi:      &core.CSIVolumeSource{Driver: strings.Repeat("g", 65)},
			errtype:  field.ErrorTypeTooLong,
			errfield: "driver",
		}, {
			name:     "driver name: invalid start char",
			csi:      &core.CSIVolumeSource{Driver: "_comgooglestoragecsigcepd"},
			errtype:  field.ErrorTypeInvalid,
			errfield: "driver",
		}, {
			name:     "driver name: invalid end char",
			csi:      &core.CSIVolumeSource{Driver: "comgooglestoragecsigcepd/"},
			errtype:  field.ErrorTypeInvalid,
			errfield: "driver",
		}, {
			name:     "driver name: invalid separators",
			csi:      &core.CSIVolumeSource{Driver: "com/google/storage/csi~gcepd"},
			errtype:  field.ErrorTypeInvalid,
			errfield: "driver",
		}, {
			name: "valid nodePublishSecretRef",
			csi:  &core.CSIVolumeSource{Driver: "com.google.gcepd", NodePublishSecretRef: &core.LocalObjectReference{Name: "foobar"}},
		}, {
			name:     "nodePublishSecretRef: invalid name missing",
			csi:      &core.CSIVolumeSource{Driver: "com.google.gcepd", NodePublishSecretRef: &core.LocalObjectReference{Name: ""}},
			errtype:  field.ErrorTypeRequired,
			errfield: "nodePublishSecretRef.name",
		},
	}

	for i, tc := range testCases {
		errs := validateCSIVolumeSource(tc.csi, field.NewPath("field"))

		if len(errs) > 0 && tc.errtype == "" {
			t.Errorf("[%d: %q] unexpected error(s): %v", i, tc.name, errs)
		} else if len(errs) == 0 && tc.errtype != "" {
			t.Errorf("[%d: %q] expected error type %v", i, tc.name, tc.errtype)
		} else if len(errs) >= 1 {
			if errs[0].Type != tc.errtype {
				t.Errorf("[%d: %q] expected error type %v, got %v", i, tc.name, tc.errtype, errs[0].Type)
			} else if !strings.HasSuffix(errs[0].Field, "."+tc.errfield) {
				t.Errorf("[%d: %q] expected error on field %q, got %q", i, tc.name, tc.errfield, errs[0].Field)
			}
		}
	}
}

func TestValidateCSIPersistentVolumeSource(t *testing.T) {
	testCases := []struct {
		name     string
		csi      *core.CSIPersistentVolumeSource
		errtype  field.ErrorType
		errfield string
	}{{
		name: "all required fields ok",
		csi:  &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123", ReadOnly: true},
	}, {
		name: "with default values ok",
		csi:  &core.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "test-123"},
	}, {
		name:     "missing driver name",
		csi:      &core.CSIPersistentVolumeSource{VolumeHandle: "test-123"},
		errtype:  field.ErrorTypeRequired,
		errfield: "driver",
	}, {
		name:     "missing volume handle",
		csi:      &core.CSIPersistentVolumeSource{Driver: "my-driver"},
		errtype:  field.ErrorTypeRequired,
		errfield: "volumeHandle",
	}, {
		name: "driver name: ok no punctuations",
		csi:  &core.CSIPersistentVolumeSource{Driver: "comgooglestoragecsigcepd", VolumeHandle: "test-123"},
	}, {
		name: "driver name: ok dot only",
		csi:  &core.CSIPersistentVolumeSource{Driver: "io.kubernetes.storage.csi.flex", VolumeHandle: "test-123"},
	}, {
		name: "driver name: ok dash only",
		csi:  &core.CSIPersistentVolumeSource{Driver: "io-kubernetes-storage-csi-flex", VolumeHandle: "test-123"},
	}, {
		name:     "driver name: invalid underscore",
		csi:      &core.CSIPersistentVolumeSource{Driver: "io_kubernetes_storage_csi_flex", VolumeHandle: "test-123"},
		errtype:  field.ErrorTypeInvalid,
		errfield: "driver",
	}, {
		name:     "driver name: invalid dot underscores",
		csi:      &core.CSIPersistentVolumeSource{Driver: "io.kubernetes.storage_csi.flex", VolumeHandle: "test-123"},
		errtype:  field.ErrorTypeInvalid,
		errfield: "driver",
	}, {
		name: "driver name: ok beginning with number",
		csi:  &core.CSIPersistentVolumeSource{Driver: "2io.kubernetes.storage-csi.flex", VolumeHandle: "test-123"},
	}, {
		name: "driver name: ok ending with number",
		csi:  &core.CSIPersistentVolumeSource{Driver: "io.kubernetes.storage-csi.flex2", VolumeHandle: "test-123"},
	}, {
		name:     "driver name: invalid dot dash underscores",
		csi:      &core.CSIPersistentVolumeSource{Driver: "io.kubernetes-storage.csi_flex", VolumeHandle: "test-123"},
		errtype:  field.ErrorTypeInvalid,
		errfield: "driver",
	}, {
		name:     "driver name: invalid length 0",
		csi:      &core.CSIPersistentVolumeSource{Driver: "", VolumeHandle: "test-123"},
		errtype:  field.ErrorTypeRequired,
		errfield: "driver",
	}, {
		name: "driver name: ok length 1",
		csi:  &core.CSIPersistentVolumeSource{Driver: "a", VolumeHandle: "test-123"},
	}, {
		name:     "driver name: invalid length > 63",
		csi:      &core.CSIPersistentVolumeSource{Driver: strings.Repeat("g", 65), VolumeHandle: "test-123"},
		errtype:  field.ErrorTypeTooLong,
		errfield: "driver",
	}, {
		name:     "driver name: invalid start char",
		csi:      &core.CSIPersistentVolumeSource{Driver: "_comgooglestoragecsigcepd", VolumeHandle: "test-123"},
		errtype:  field.ErrorTypeInvalid,
		errfield: "driver",
	}, {
		name:     "driver name: invalid end char",
		csi:      &core.CSIPersistentVolumeSource{Driver: "comgooglestoragecsigcepd/", VolumeHandle: "test-123"},
		errtype:  field.ErrorTypeInvalid,
		errfield: "driver",
	}, {
		name:     "driver name: invalid separators",
		csi:      &core.CSIPersistentVolumeSource{Driver: "com/google/storage/csi~gcepd", VolumeHandle: "test-123"},
		errtype:  field.ErrorTypeInvalid,
		errfield: "driver",
	}, {
		name:     "controllerExpandSecretRef: invalid name missing",
		csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", ControllerExpandSecretRef: &core.SecretReference{Namespace: "default"}},
		errtype:  field.ErrorTypeRequired,
		errfield: "controllerExpandSecretRef.name",
	}, {
		name:     "controllerExpandSecretRef: invalid namespace missing",
		csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", ControllerExpandSecretRef: &core.SecretReference{Name: "foobar"}},
		errtype:  field.ErrorTypeRequired,
		errfield: "controllerExpandSecretRef.namespace",
	}, {
		name: "valid controllerExpandSecretRef",
		csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", ControllerExpandSecretRef: &core.SecretReference{Name: "foobar", Namespace: "default"}},
	}, {
		name:     "controllerPublishSecretRef: invalid name missing",
		csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", ControllerPublishSecretRef: &core.SecretReference{Namespace: "default"}},
		errtype:  field.ErrorTypeRequired,
		errfield: "controllerPublishSecretRef.name",
	}, {
		name:     "controllerPublishSecretRef: invalid namespace missing",
		csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", ControllerPublishSecretRef: &core.SecretReference{Name: "foobar"}},
		errtype:  field.ErrorTypeRequired,
		errfield: "controllerPublishSecretRef.namespace",
	}, {
		name: "valid controllerPublishSecretRef",
		csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", ControllerPublishSecretRef: &core.SecretReference{Name: "foobar", Namespace: "default"}},
	}, {
		name: "valid nodePublishSecretRef",
		csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodePublishSecretRef: &core.SecretReference{Name: "foobar", Namespace: "default"}},
	}, {
		name:     "nodePublishSecretRef: invalid name missing",
		csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodePublishSecretRef: &core.SecretReference{Namespace: "foobar"}},
		errtype:  field.ErrorTypeRequired,
		errfield: "nodePublishSecretRef.name",
	}, {
		name:     "nodePublishSecretRef: invalid namespace missing",
		csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodePublishSecretRef: &core.SecretReference{Name: "foobar"}},
		errtype:  field.ErrorTypeRequired,
		errfield: "nodePublishSecretRef.namespace",
	}, {
		name:     "nodeExpandSecretRef: invalid name missing",
		csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodeExpandSecretRef: &core.SecretReference{Namespace: "default"}},
		errtype:  field.ErrorTypeRequired,
		errfield: "nodeExpandSecretRef.name",
	}, {
		name:     "nodeExpandSecretRef: invalid namespace missing",
		csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodeExpandSecretRef: &core.SecretReference{Name: "foobar"}},
		errtype:  field.ErrorTypeRequired,
		errfield: "nodeExpandSecretRef.namespace",
	}, {
		name: "valid nodeExpandSecretRef",
		csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodeExpandSecretRef: &core.SecretReference{Name: "foobar", Namespace: "default"}},
	}, {
		name: "Invalid nodePublishSecretRef",
		csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodePublishSecretRef: &core.SecretReference{Name: "foobar", Namespace: "default"}},
	},

		// tests with allowDNSSubDomainSecretName flag on/off
		{
			name: "valid nodeExpandSecretRef",
			csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodeExpandSecretRef: &core.SecretReference{Name: strings.Repeat("g", 63), Namespace: "default"}},
		}, {
			name: "valid long nodeExpandSecretRef",
			csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodeExpandSecretRef: &core.SecretReference{Name: strings.Repeat("g", 65), Namespace: "default"}},
		}, {
			name:     "Invalid nodeExpandSecretRef",
			csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodeExpandSecretRef: &core.SecretReference{Name: strings.Repeat("g", 255), Namespace: "default"}},
			errtype:  field.ErrorTypeInvalid,
			errfield: "nodeExpandSecretRef.name",
		}, {
			name: "valid nodePublishSecretRef",
			csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodePublishSecretRef: &core.SecretReference{Name: strings.Repeat("g", 63), Namespace: "default"}},
		}, {
			name: "valid long nodePublishSecretRef",
			csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodePublishSecretRef: &core.SecretReference{Name: strings.Repeat("g", 65), Namespace: "default"}},
		}, {
			name:     "Invalid nodePublishSecretRef",
			csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", NodePublishSecretRef: &core.SecretReference{Name: strings.Repeat("g", 255), Namespace: "default"}},
			errtype:  field.ErrorTypeInvalid,
			errfield: "nodePublishSecretRef.name",
		}, {
			name: "valid ControllerExpandSecretRef",
			csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", ControllerExpandSecretRef: &core.SecretReference{Name: strings.Repeat("g", 63), Namespace: "default"}},
		}, {
			name: "valid long ControllerExpandSecretRef",
			csi:  &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", ControllerExpandSecretRef: &core.SecretReference{Name: strings.Repeat("g", 65), Namespace: "default"}},
		}, {
			name:     "Invalid ControllerExpandSecretRef",
			csi:      &core.CSIPersistentVolumeSource{Driver: "com.google.gcepd", VolumeHandle: "foobar", ControllerExpandSecretRef: &core.SecretReference{Name: strings.Repeat("g", 255), Namespace: "default"}},
			errtype:  field.ErrorTypeInvalid,
			errfield: "controllerExpandSecretRef.name",
		},
	}

	for i, tc := range testCases {
		errs := validateCSIPersistentVolumeSource(tc.csi, field.NewPath("field"))

		if len(errs) > 0 && tc.errtype == "" {
			t.Errorf("[%d: %q] unexpected error(s): %v", i, tc.name, errs)
		} else if len(errs) == 0 && tc.errtype != "" {
			t.Errorf("[%d: %q] expected error type %v", i, tc.name, tc.errtype)
		} else if len(errs) >= 1 {
			if errs[0].Type != tc.errtype {
				t.Errorf("[%d: %q] expected error type %v, got %v", i, tc.name, tc.errtype, errs[0].Type)
			} else if !strings.HasSuffix(errs[0].Field, "."+tc.errfield) {
				t.Errorf("[%d: %q] expected error on field %q, got %q", i, tc.name, tc.errfield, errs[0].Field)
			}
		}
	}
}

// This test is a little too top-to-bottom.  Ideally we would test each volume
// type on its own, but we want to also make sure that the logic works through
// the one-of wrapper, so we just do it all in one place.
func TestValidateVolumes(t *testing.T) {
	validInitiatorName := "iqn.2015-02.example.com:init"
	invalidInitiatorName := "2015-02.example.com:init"

	type verr struct {
		etype  field.ErrorType
		field  string
		detail string
	}

	testCases := []struct {
		name string
		vol  core.Volume
		errs []verr
		opts PodValidationOptions
	}{
		// EmptyDir and basic volume names
		{
			name: "valid alpha name",
			vol: core.Volume{
				Name: "empty",
				VolumeSource: core.VolumeSource{
					EmptyDir: &core.EmptyDirVolumeSource{},
				},
			},
		}, {
			name: "valid num name",
			vol: core.Volume{
				Name: "123",
				VolumeSource: core.VolumeSource{
					EmptyDir: &core.EmptyDirVolumeSource{},
				},
			},
		}, {
			name: "valid alphanum name",
			vol: core.Volume{
				Name: "empty-123",
				VolumeSource: core.VolumeSource{
					EmptyDir: &core.EmptyDirVolumeSource{},
				},
			},
		}, {
			name: "valid numalpha name",
			vol: core.Volume{
				Name: "123-empty",
				VolumeSource: core.VolumeSource{
					EmptyDir: &core.EmptyDirVolumeSource{},
				},
			},
		}, {
			name: "zero-length name",
			vol: core.Volume{
				Name:         "",
				VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "name",
			}},
		}, {
			name: "name > 63 characters",
			vol: core.Volume{
				Name:         strings.Repeat("a", 64),
				VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "name",
				detail: "must be no more than",
			}},
		}, {
			name: "name has dots",
			vol: core.Volume{
				Name:         "a.b.c",
				VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "name",
				detail: "must not contain dots",
			}},
		}, {
			name: "name not a DNS label",
			vol: core.Volume{
				Name:         "Not a DNS label!",
				VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "name",
				detail: dnsLabelErrMsg,
			}},
		},
		// More than one source field specified.
		{
			name: "more than one source",
			vol: core.Volume{
				Name: "dups",
				VolumeSource: core.VolumeSource{
					EmptyDir: &core.EmptyDirVolumeSource{},
					HostPath: &core.HostPathVolumeSource{
						Path: "/mnt/path",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeForbidden,
				field:  "hostPath",
				detail: "may not specify more than 1 volume",
			}},
		},
		// HostPath Default
		{
			name: "default HostPath",
			vol: core.Volume{
				Name: "hostpath",
				VolumeSource: core.VolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/mnt/path",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			},
		},
		// HostPath Supported
		{
			name: "valid HostPath",
			vol: core.Volume{
				Name: "hostpath",
				VolumeSource: core.VolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/mnt/path",
						Type: newHostPathType(string(core.HostPathSocket)),
					},
				},
			},
		},
		// HostPath Invalid
		{
			name: "invalid HostPath",
			vol: core.Volume{
				Name: "hostpath",
				VolumeSource: core.VolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/mnt/path",
						Type: newHostPathType("invalid"),
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeNotSupported,
				field: "type",
			}},
		}, {
			name: "invalid HostPath backsteps",
			vol: core.Volume{
				Name: "hostpath",
				VolumeSource: core.VolumeSource{
					HostPath: &core.HostPathVolumeSource{
						Path: "/mnt/path/..",
						Type: newHostPathType(string(core.HostPathDirectory)),
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "path",
				detail: "must not contain '..'",
			}},
		},
		// GcePersistentDisk
		{
			name: "valid GcePersistentDisk",
			vol: core.Volume{
				Name: "gce-pd",
				VolumeSource: core.VolumeSource{
					GCEPersistentDisk: &core.GCEPersistentDiskVolumeSource{
						PDName:    "my-PD",
						FSType:    "ext4",
						Partition: 1,
						ReadOnly:  false,
					},
				},
			},
		},
		// AWSElasticBlockStore
		{
			name: "valid AWSElasticBlockStore",
			vol: core.Volume{
				Name: "aws-ebs",
				VolumeSource: core.VolumeSource{
					AWSElasticBlockStore: &core.AWSElasticBlockStoreVolumeSource{
						VolumeID:  "my-PD",
						FSType:    "ext4",
						Partition: 1,
						ReadOnly:  false,
					},
				},
			},
		},
		// GitRepo
		{
			name: "valid GitRepo",
			vol: core.Volume{
				Name: "git-repo",
				VolumeSource: core.VolumeSource{
					GitRepo: &core.GitRepoVolumeSource{
						Repository: "my-repo",
						Revision:   "hashstring",
						Directory:  "target",
					},
				},
			},
		}, {
			name: "valid GitRepo in .",
			vol: core.Volume{
				Name: "git-repo-dot",
				VolumeSource: core.VolumeSource{
					GitRepo: &core.GitRepoVolumeSource{
						Repository: "my-repo",
						Directory:  ".",
					},
				},
			},
		}, {
			name: "valid GitRepo with .. in name",
			vol: core.Volume{
				Name: "git-repo-dot-dot-foo",
				VolumeSource: core.VolumeSource{
					GitRepo: &core.GitRepoVolumeSource{
						Repository: "my-repo",
						Directory:  "..foo",
					},
				},
			},
		}, {
			name: "GitRepo starts with ../",
			vol: core.Volume{
				Name: "gitrepo",
				VolumeSource: core.VolumeSource{
					GitRepo: &core.GitRepoVolumeSource{
						Repository: "foo",
						Directory:  "../dots/bar",
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "gitRepo.directory",
				detail: `must not contain '..'`,
			}},
		}, {
			name: "GitRepo contains ..",
			vol: core.Volume{
				Name: "gitrepo",
				VolumeSource: core.VolumeSource{
					GitRepo: &core.GitRepoVolumeSource{
						Repository: "foo",
						Directory:  "dots/../bar",
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "gitRepo.directory",
				detail: `must not contain '..'`,
			}},
		}, {
			name: "GitRepo absolute target",
			vol: core.Volume{
				Name: "gitrepo",
				VolumeSource: core.VolumeSource{
					GitRepo: &core.GitRepoVolumeSource{
						Repository: "foo",
						Directory:  "/abstarget",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "gitRepo.directory",
			}},
		},
		// ISCSI
		{
			name: "valid ISCSI",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal: "127.0.0.1",
						IQN:          "iqn.2015-02.example.com:test",
						Lun:          1,
						FSType:       "ext4",
						ReadOnly:     false,
					},
				},
			},
		}, {
			name: "valid IQN: eui format",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal: "127.0.0.1",
						IQN:          "eui.0123456789ABCDEF",
						Lun:          1,
						FSType:       "ext4",
						ReadOnly:     false,
					},
				},
			},
		}, {
			name: "valid IQN: naa format",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal: "127.0.0.1",
						IQN:          "naa.62004567BA64678D0123456789ABCDEF",
						Lun:          1,
						FSType:       "ext4",
						ReadOnly:     false,
					},
				},
			},
		}, {
			name: "empty portal",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal: "",
						IQN:          "iqn.2015-02.example.com:test",
						Lun:          1,
						FSType:       "ext4",
						ReadOnly:     false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "iscsi.targetPortal",
			}},
		}, {
			name: "empty iqn",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal: "127.0.0.1",
						IQN:          "",
						Lun:          1,
						FSType:       "ext4",
						ReadOnly:     false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "iscsi.iqn",
			}},
		}, {
			name: "invalid IQN: iqn format",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal: "127.0.0.1",
						IQN:          "iqn.2015-02.example.com:test;ls;",
						Lun:          1,
						FSType:       "ext4",
						ReadOnly:     false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "iscsi.iqn",
			}},
		}, {
			name: "invalid IQN: eui format",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal: "127.0.0.1",
						IQN:          "eui.0123456789ABCDEFGHIJ",
						Lun:          1,
						FSType:       "ext4",
						ReadOnly:     false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "iscsi.iqn",
			}},
		}, {
			name: "invalid IQN: naa format",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal: "127.0.0.1",
						IQN:          "naa.62004567BA_4-78D.123456789ABCDEF",
						Lun:          1,
						FSType:       "ext4",
						ReadOnly:     false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "iscsi.iqn",
			}},
		}, {
			name: "valid initiatorName",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal:  "127.0.0.1",
						IQN:           "iqn.2015-02.example.com:test",
						Lun:           1,
						InitiatorName: &validInitiatorName,
						FSType:        "ext4",
						ReadOnly:      false,
					},
				},
			},
		}, {
			name: "invalid initiatorName",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal:  "127.0.0.1",
						IQN:           "iqn.2015-02.example.com:test",
						Lun:           1,
						InitiatorName: &invalidInitiatorName,
						FSType:        "ext4",
						ReadOnly:      false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "iscsi.initiatorname",
			}},
		}, {
			name: "empty secret",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal:      "127.0.0.1",
						IQN:               "iqn.2015-02.example.com:test",
						Lun:               1,
						FSType:            "ext4",
						ReadOnly:          false,
						DiscoveryCHAPAuth: true,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "iscsi.secretRef",
			}},
		}, {
			name: "empty secret",
			vol: core.Volume{
				Name: "iscsi",
				VolumeSource: core.VolumeSource{
					ISCSI: &core.ISCSIVolumeSource{
						TargetPortal:    "127.0.0.1",
						IQN:             "iqn.2015-02.example.com:test",
						Lun:             1,
						FSType:          "ext4",
						ReadOnly:        false,
						SessionCHAPAuth: true,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "iscsi.secretRef",
			}},
		},
		// Secret
		{
			name: "valid Secret",
			vol: core.Volume{
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName: "my-secret",
					},
				},
			},
		}, {
			name: "valid Secret with defaultMode",
			vol: core.Volume{
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName:  "my-secret",
						DefaultMode: utilpointer.Int32(0644),
					},
				},
			},
		}, {
			name: "valid Secret with projection and mode",
			vol: core.Volume{
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName: "my-secret",
						Items: []core.KeyToPath{{
							Key:  "key",
							Path: "filename",
							Mode: utilpointer.Int32(0644),
						}},
					},
				},
			},
		}, {
			name: "valid Secret with subdir projection",
			vol: core.Volume{
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName: "my-secret",
						Items: []core.KeyToPath{{
							Key:  "key",
							Path: "dir/filename",
						}},
					},
				},
			},
		}, {
			name: "secret with missing path",
			vol: core.Volume{
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName: "s",
						Items:      []core.KeyToPath{{Key: "key", Path: ""}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "secret.items[0].path",
			}},
		}, {
			name: "secret with leading ..",
			vol: core.Volume{
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName: "s",
						Items:      []core.KeyToPath{{Key: "key", Path: "../foo"}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "secret.items[0].path",
			}},
		}, {
			name: "secret with .. inside",
			vol: core.Volume{
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName: "s",
						Items:      []core.KeyToPath{{Key: "key", Path: "foo/../bar"}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "secret.items[0].path",
			}},
		}, {
			name: "secret with invalid positive defaultMode",
			vol: core.Volume{
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName:  "s",
						DefaultMode: utilpointer.Int32(01000),
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "secret.defaultMode",
			}},
		}, {
			name: "secret with invalid negative defaultMode",
			vol: core.Volume{
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName:  "s",
						DefaultMode: utilpointer.Int32(-1),
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "secret.defaultMode",
			}},
		},
		// ConfigMap
		{
			name: "valid ConfigMap",
			vol: core.Volume{
				Name: "cfgmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{
							Name: "my-cfgmap",
						},
					},
				},
			},
		}, {
			name: "valid ConfigMap with defaultMode",
			vol: core.Volume{
				Name: "cfgmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{
							Name: "my-cfgmap",
						},
						DefaultMode: utilpointer.Int32(0644),
					},
				},
			},
		}, {
			name: "valid ConfigMap with projection and mode",
			vol: core.Volume{
				Name: "cfgmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{
							Name: "my-cfgmap"},
						Items: []core.KeyToPath{{
							Key:  "key",
							Path: "filename",
							Mode: utilpointer.Int32(0644),
						}},
					},
				},
			},
		}, {
			name: "valid ConfigMap with subdir projection",
			vol: core.Volume{
				Name: "cfgmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{
							Name: "my-cfgmap"},
						Items: []core.KeyToPath{{
							Key:  "key",
							Path: "dir/filename",
						}},
					},
				},
			},
		}, {
			name: "configmap with missing path",
			vol: core.Volume{
				Name: "cfgmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{Name: "c"},
						Items:                []core.KeyToPath{{Key: "key", Path: ""}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "configMap.items[0].path",
			}},
		}, {
			name: "configmap with leading ..",
			vol: core.Volume{
				Name: "cfgmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{Name: "c"},
						Items:                []core.KeyToPath{{Key: "key", Path: "../foo"}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "configMap.items[0].path",
			}},
		}, {
			name: "configmap with .. inside",
			vol: core.Volume{
				Name: "cfgmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{Name: "c"},
						Items:                []core.KeyToPath{{Key: "key", Path: "foo/../bar"}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "configMap.items[0].path",
			}},
		}, {
			name: "configmap with invalid positive defaultMode",
			vol: core.Volume{
				Name: "cfgmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{Name: "c"},
						DefaultMode:          utilpointer.Int32(01000),
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "configMap.defaultMode",
			}},
		}, {
			name: "configmap with invalid negative defaultMode",
			vol: core.Volume{
				Name: "cfgmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{Name: "c"},
						DefaultMode:          utilpointer.Int32(-1),
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "configMap.defaultMode",
			}},
		},
		// Glusterfs
		{
			name: "valid Glusterfs",
			vol: core.Volume{
				Name: "glusterfs",
				VolumeSource: core.VolumeSource{
					Glusterfs: &core.GlusterfsVolumeSource{
						EndpointsName: "host1",
						Path:          "path",
						ReadOnly:      false,
					},
				},
			},
		}, {
			name: "empty hosts",
			vol: core.Volume{
				Name: "glusterfs",
				VolumeSource: core.VolumeSource{
					Glusterfs: &core.GlusterfsVolumeSource{
						EndpointsName: "",
						Path:          "path",
						ReadOnly:      false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "glusterfs.endpoints",
			}},
		}, {
			name: "empty path",
			vol: core.Volume{
				Name: "glusterfs",
				VolumeSource: core.VolumeSource{
					Glusterfs: &core.GlusterfsVolumeSource{
						EndpointsName: "host",
						Path:          "",
						ReadOnly:      false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "glusterfs.path",
			}},
		},
		// Flocker
		{
			name: "valid Flocker -- datasetUUID",
			vol: core.Volume{
				Name: "flocker",
				VolumeSource: core.VolumeSource{
					Flocker: &core.FlockerVolumeSource{
						DatasetUUID: "d846b09d-223d-43df-ab5b-d6db2206a0e4",
					},
				},
			},
		}, {
			name: "valid Flocker -- datasetName",
			vol: core.Volume{
				Name: "flocker",
				VolumeSource: core.VolumeSource{
					Flocker: &core.FlockerVolumeSource{
						DatasetName: "datasetName",
					},
				},
			},
		}, {
			name: "both empty",
			vol: core.Volume{
				Name: "flocker",
				VolumeSource: core.VolumeSource{
					Flocker: &core.FlockerVolumeSource{
						DatasetName: "",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "flocker",
			}},
		}, {
			name: "both specified",
			vol: core.Volume{
				Name: "flocker",
				VolumeSource: core.VolumeSource{
					Flocker: &core.FlockerVolumeSource{
						DatasetName: "datasetName",
						DatasetUUID: "d846b09d-223d-43df-ab5b-d6db2206a0e4",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "flocker",
			}},
		}, {
			name: "slash in flocker datasetName",
			vol: core.Volume{
				Name: "flocker",
				VolumeSource: core.VolumeSource{
					Flocker: &core.FlockerVolumeSource{
						DatasetName: "foo/bar",
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "flocker.datasetName",
				detail: "must not contain '/'",
			}},
		},
		// RBD
		{
			name: "valid RBD",
			vol: core.Volume{
				Name: "rbd",
				VolumeSource: core.VolumeSource{
					RBD: &core.RBDVolumeSource{
						CephMonitors: []string{"foo"},
						RBDImage:     "bar",
						FSType:       "ext4",
					},
				},
			},
		}, {
			name: "empty rbd monitors",
			vol: core.Volume{
				Name: "rbd",
				VolumeSource: core.VolumeSource{
					RBD: &core.RBDVolumeSource{
						CephMonitors: []string{},
						RBDImage:     "bar",
						FSType:       "ext4",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "rbd.monitors",
			}},
		}, {
			name: "empty image",
			vol: core.Volume{
				Name: "rbd",
				VolumeSource: core.VolumeSource{
					RBD: &core.RBDVolumeSource{
						CephMonitors: []string{"foo"},
						RBDImage:     "",
						FSType:       "ext4",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "rbd.image",
			}},
		},
		// Cinder
		{
			name: "valid Cinder",
			vol: core.Volume{
				Name: "cinder",
				VolumeSource: core.VolumeSource{
					Cinder: &core.CinderVolumeSource{
						VolumeID: "29ea5088-4f60-4757-962e-dba678767887",
						FSType:   "ext4",
						ReadOnly: false,
					},
				},
			},
		},
		// CephFS
		{
			name: "valid CephFS",
			vol: core.Volume{
				Name: "cephfs",
				VolumeSource: core.VolumeSource{
					CephFS: &core.CephFSVolumeSource{
						Monitors: []string{"foo"},
					},
				},
			},
		}, {
			name: "empty cephfs monitors",
			vol: core.Volume{
				Name: "cephfs",
				VolumeSource: core.VolumeSource{
					CephFS: &core.CephFSVolumeSource{
						Monitors: []string{},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "cephfs.monitors",
			}},
		},
		// DownwardAPI
		{
			name: "valid DownwardAPI",
			vol: core.Volume{
				Name: "downwardapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Path: "labels",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}, {
							Path: "labels with subscript",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels['key']",
							},
						}, {
							Path: "labels with complex subscript",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels['test.example.com/key']",
							},
						}, {
							Path: "annotations",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.annotations",
							},
						}, {
							Path: "annotations with subscript",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.annotations['key']",
							},
						}, {
							Path: "annotations with complex subscript",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.annotations['TEST.EXAMPLE.COM/key']",
							},
						}, {
							Path: "namespace",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.namespace",
							},
						}, {
							Path: "name",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.name",
							},
						}, {
							Path: "path/with/subdirs",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}, {
							Path: "path/./withdot",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}, {
							Path: "path/with/embedded..dotdot",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}, {
							Path: "path/with/leading/..dotdot",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}, {
							Path: "cpu_limit",
							ResourceFieldRef: &core.ResourceFieldSelector{
								ContainerName: "test-container",
								Resource:      "limits.cpu",
							},
						}, {
							Path: "cpu_request",
							ResourceFieldRef: &core.ResourceFieldSelector{
								ContainerName: "test-container",
								Resource:      "requests.cpu",
							},
						}, {
							Path: "memory_limit",
							ResourceFieldRef: &core.ResourceFieldSelector{
								ContainerName: "test-container",
								Resource:      "limits.memory",
							},
						}, {
							Path: "memory_request",
							ResourceFieldRef: &core.ResourceFieldSelector{
								ContainerName: "test-container",
								Resource:      "requests.memory",
							},
						}},
					},
				},
			},
		}, {
			name: "hugepages-downwardAPI-enabled",
			vol: core.Volume{
				Name: "downwardapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Path: "hugepages_request",
							ResourceFieldRef: &core.ResourceFieldSelector{
								ContainerName: "test-container",
								Resource:      "requests.hugepages-2Mi",
							},
						}, {
							Path: "hugepages_limit",
							ResourceFieldRef: &core.ResourceFieldSelector{
								ContainerName: "test-container",
								Resource:      "limits.hugepages-2Mi",
							},
						}},
					},
				},
			},
		}, {
			name: "downapi valid defaultMode",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						DefaultMode: utilpointer.Int32(0644),
					},
				},
			},
		}, {
			name: "downapi valid item mode",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Mode: utilpointer.Int32(0644),
							Path: "path",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}},
					},
				},
			},
		}, {
			name: "downapi invalid positive item mode",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Mode: utilpointer.Int32(01000),
							Path: "path",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "downwardAPI.mode",
			}},
		}, {
			name: "downapi invalid negative item mode",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Mode: utilpointer.Int32(-1),
							Path: "path",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "downwardAPI.mode",
			}},
		}, {
			name: "downapi empty metatada path",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Path: "",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "downwardAPI.path",
			}},
		}, {
			name: "downapi absolute path",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Path: "/absolutepath",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "downwardAPI.path",
			}},
		}, {
			name: "downapi dot dot path",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Path: "../../passwd",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}},
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "downwardAPI.path",
				detail: `must not contain '..'`,
			}},
		}, {
			name: "downapi dot dot file name",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Path: "..badFileName",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}},
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "downwardAPI.path",
				detail: `must not start with '..'`,
			}},
		}, {
			name: "downapi dot dot first level dirent",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Path: "..badDirName/goodFileName",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
						}},
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "downwardAPI.path",
				detail: `must not start with '..'`,
			}},
		}, {
			name: "downapi fieldRef and ResourceFieldRef together",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{{
							Path: "test",
							FieldRef: &core.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.labels",
							},
							ResourceFieldRef: &core.ResourceFieldSelector{
								ContainerName: "test-container",
								Resource:      "requests.memory",
							},
						}},
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "downwardAPI",
				detail: "fieldRef and resourceFieldRef can not be specified simultaneously",
			}},
		}, {
			name: "downapi invalid positive defaultMode",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						DefaultMode: utilpointer.Int32(01000),
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "downwardAPI.defaultMode",
			}},
		}, {
			name: "downapi invalid negative defaultMode",
			vol: core.Volume{
				Name: "downapi",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						DefaultMode: utilpointer.Int32(-1),
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "downwardAPI.defaultMode",
			}},
		},
		// FC
		{
			name: "FC valid targetWWNs and lun",
			vol: core.Volume{
				Name: "fc",
				VolumeSource: core.VolumeSource{
					FC: &core.FCVolumeSource{
						TargetWWNs: []string{"some_wwn"},
						Lun:        utilpointer.Int32(1),
						FSType:     "ext4",
						ReadOnly:   false,
					},
				},
			},
		}, {
			name: "FC valid wwids",
			vol: core.Volume{
				Name: "fc",
				VolumeSource: core.VolumeSource{
					FC: &core.FCVolumeSource{
						WWIDs:    []string{"some_wwid"},
						FSType:   "ext4",
						ReadOnly: false,
					},
				},
			},
		}, {
			name: "FC empty targetWWNs and wwids",
			vol: core.Volume{
				Name: "fc",
				VolumeSource: core.VolumeSource{
					FC: &core.FCVolumeSource{
						TargetWWNs: []string{},
						Lun:        utilpointer.Int32(1),
						WWIDs:      []string{},
						FSType:     "ext4",
						ReadOnly:   false,
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeRequired,
				field:  "fc.targetWWNs",
				detail: "must specify either targetWWNs or wwids",
			}},
		}, {
			name: "FC invalid: both targetWWNs and wwids simultaneously",
			vol: core.Volume{
				Name: "fc",
				VolumeSource: core.VolumeSource{
					FC: &core.FCVolumeSource{
						TargetWWNs: []string{"some_wwn"},
						Lun:        utilpointer.Int32(1),
						WWIDs:      []string{"some_wwid"},
						FSType:     "ext4",
						ReadOnly:   false,
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "fc.targetWWNs",
				detail: "targetWWNs and wwids can not be specified simultaneously",
			}},
		}, {
			name: "FC valid targetWWNs and empty lun",
			vol: core.Volume{
				Name: "fc",
				VolumeSource: core.VolumeSource{
					FC: &core.FCVolumeSource{
						TargetWWNs: []string{"wwn"},
						Lun:        nil,
						FSType:     "ext4",
						ReadOnly:   false,
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeRequired,
				field:  "fc.lun",
				detail: "lun is required if targetWWNs is specified",
			}},
		}, {
			name: "FC valid targetWWNs and invalid lun",
			vol: core.Volume{
				Name: "fc",
				VolumeSource: core.VolumeSource{
					FC: &core.FCVolumeSource{
						TargetWWNs: []string{"wwn"},
						Lun:        utilpointer.Int32(256),
						FSType:     "ext4",
						ReadOnly:   false,
					},
				},
			},
			errs: []verr{{
				etype:  field.ErrorTypeInvalid,
				field:  "fc.lun",
				detail: validation.InclusiveRangeError(0, 255),
			}},
		},
		// FlexVolume
		{
			name: "valid FlexVolume",
			vol: core.Volume{
				Name: "flex-volume",
				VolumeSource: core.VolumeSource{
					FlexVolume: &core.FlexVolumeSource{
						Driver: "kubernetes.io/blue",
						FSType: "ext4",
					},
				},
			},
		},
		// AzureFile
		{
			name: "valid AzureFile",
			vol: core.Volume{
				Name: "azure-file",
				VolumeSource: core.VolumeSource{
					AzureFile: &core.AzureFileVolumeSource{
						SecretName: "key",
						ShareName:  "share",
						ReadOnly:   false,
					},
				},
			},
		}, {
			name: "AzureFile empty secret",
			vol: core.Volume{
				Name: "azure-file",
				VolumeSource: core.VolumeSource{
					AzureFile: &core.AzureFileVolumeSource{
						SecretName: "",
						ShareName:  "share",
						ReadOnly:   false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "azureFile.secretName",
			}},
		}, {
			name: "AzureFile empty share",
			vol: core.Volume{
				Name: "azure-file",
				VolumeSource: core.VolumeSource{
					AzureFile: &core.AzureFileVolumeSource{
						SecretName: "name",
						ShareName:  "",
						ReadOnly:   false,
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "azureFile.shareName",
			}},
		},
		// Quobyte
		{
			name: "valid Quobyte",
			vol: core.Volume{
				Name: "quobyte",
				VolumeSource: core.VolumeSource{
					Quobyte: &core.QuobyteVolumeSource{
						Registry: "registry:7861",
						Volume:   "volume",
						ReadOnly: false,
						User:     "root",
						Group:    "root",
						Tenant:   "ThisIsSomeTenantUUID",
					},
				},
			},
		}, {
			name: "empty registry quobyte",
			vol: core.Volume{
				Name: "quobyte",
				VolumeSource: core.VolumeSource{
					Quobyte: &core.QuobyteVolumeSource{
						Volume: "/test",
						Tenant: "ThisIsSomeTenantUUID",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "quobyte.registry",
			}},
		}, {
			name: "wrong format registry quobyte",
			vol: core.Volume{
				Name: "quobyte",
				VolumeSource: core.VolumeSource{
					Quobyte: &core.QuobyteVolumeSource{
						Registry: "registry7861",
						Volume:   "/test",
						Tenant:   "ThisIsSomeTenantUUID",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "quobyte.registry",
			}},
		}, {
			name: "wrong format multiple registries quobyte",
			vol: core.Volume{
				Name: "quobyte",
				VolumeSource: core.VolumeSource{
					Quobyte: &core.QuobyteVolumeSource{
						Registry: "registry:7861,reg2",
						Volume:   "/test",
						Tenant:   "ThisIsSomeTenantUUID",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeInvalid,
				field: "quobyte.registry",
			}},
		}, {
			name: "empty volume quobyte",
			vol: core.Volume{
				Name: "quobyte",
				VolumeSource: core.VolumeSource{
					Quobyte: &core.QuobyteVolumeSource{
						Registry: "registry:7861",
						Tenant:   "ThisIsSomeTenantUUID",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "quobyte.volume",
			}},
		}, {
			name: "empty tenant quobyte",
			vol: core.Volume{
				Name: "quobyte",
				VolumeSource: core.VolumeSource{
					Quobyte: &core.QuobyteVolumeSource{
						Registry: "registry:7861",
						Volume:   "/test",
						Tenant:   "",
					},
				},
			},
		}, {
			name: "too long tenant quobyte",
			vol: core.Volume{
				Name: "quobyte",
				VolumeSource: core.VolumeSource{
					Quobyte: &core.QuobyteVolumeSource{
						Registry: "registry:7861",
						Volume:   "/test",
						Tenant:   "this is too long to be a valid uuid so this test has to fail on the maximum length validation of the tenant.",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "quobyte.tenant",
			}},
		},
		// AzureDisk
		{
			name: "valid AzureDisk",
			vol: core.Volume{
				Name: "azure-disk",
				VolumeSource: core.VolumeSource{
					AzureDisk: &core.AzureDiskVolumeSource{
						DiskName:    "foo",
						DataDiskURI: "https://blob/vhds/bar.vhd",
					},
				},
			},
		}, {
			name: "AzureDisk empty disk name",
			vol: core.Volume{
				Name: "azure-disk",
				VolumeSource: core.VolumeSource{
					AzureDisk: &core.AzureDiskVolumeSource{
						DiskName:    "",
						DataDiskURI: "https://blob/vhds/bar.vhd",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "azureDisk.diskName",
			}},
		}, {
			name: "AzureDisk empty disk uri",
			vol: core.Volume{
				Name: "azure-disk",
				VolumeSource: core.VolumeSource{
					AzureDisk: &core.AzureDiskVolumeSource{
						DiskName:    "foo",
						DataDiskURI: "",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "azureDisk.diskURI",
			}},
		},
		// ScaleIO
		{
			name: "valid scaleio volume",
			vol: core.Volume{
				Name: "scaleio-volume",
				VolumeSource: core.VolumeSource{
					ScaleIO: &core.ScaleIOVolumeSource{
						Gateway:    "http://abcd/efg",
						System:     "test-system",
						VolumeName: "test-vol-1",
					},
				},
			},
		}, {
			name: "ScaleIO with empty name",
			vol: core.Volume{
				Name: "scaleio-volume",
				VolumeSource: core.VolumeSource{
					ScaleIO: &core.ScaleIOVolumeSource{
						Gateway:    "http://abcd/efg",
						System:     "test-system",
						VolumeName: "",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "scaleIO.volumeName",
			}},
		}, {
			name: "ScaleIO with empty gateway",
			vol: core.Volume{
				Name: "scaleio-volume",
				VolumeSource: core.VolumeSource{
					ScaleIO: &core.ScaleIOVolumeSource{
						Gateway:    "",
						System:     "test-system",
						VolumeName: "test-vol-1",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "scaleIO.gateway",
			}},
		}, {
			name: "ScaleIO with empty system",
			vol: core.Volume{
				Name: "scaleio-volume",
				VolumeSource: core.VolumeSource{
					ScaleIO: &core.ScaleIOVolumeSource{
						Gateway:    "http://agc/efg/gateway",
						System:     "",
						VolumeName: "test-vol-1",
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "scaleIO.system",
			}},
		},
		// ProjectedVolumeSource
		{
			name: "ProjectedVolumeSource more than one projection in a source",
			vol: core.Volume{
				Name: "projected-volume",
				VolumeSource: core.VolumeSource{
					Projected: &core.ProjectedVolumeSource{
						Sources: []core.VolumeProjection{{
							Secret: &core.SecretProjection{
								LocalObjectReference: core.LocalObjectReference{
									Name: "foo",
								},
							},
						}, {
							Secret: &core.SecretProjection{
								LocalObjectReference: core.LocalObjectReference{
									Name: "foo",
								},
							},
							DownwardAPI: &core.DownwardAPIProjection{},
						}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeForbidden,
				field: "projected.sources[1]",
			}},
		}, {
			name: "ProjectedVolumeSource more than one projection in a source",
			vol: core.Volume{
				Name: "projected-volume",
				VolumeSource: core.VolumeSource{
					Projected: &core.ProjectedVolumeSource{
						Sources: []core.VolumeProjection{{
							Secret: &core.SecretProjection{},
						}, {
							Secret:      &core.SecretProjection{},
							DownwardAPI: &core.DownwardAPIProjection{},
						}},
					},
				},
			},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "projected.sources[0].secret.name",
			}, {
				etype: field.ErrorTypeRequired,
				field: "projected.sources[1].secret.name",
			}, {
				etype: field.ErrorTypeForbidden,
				field: "projected.sources[1]",
			}},
		},
		// ImageVolumeSource
		{
			name: "valid image volume on pod",
			vol: core.Volume{
				Name: "image-volume",
				VolumeSource: core.VolumeSource{
					Image: &core.ImageVolumeSource{
						Reference:  "quay.io/my/artifact:v1",
						PullPolicy: "IfNotPresent",
					},
				},
			},
			opts: PodValidationOptions{},
		}, {
			name: "no volume source",
			vol: core.Volume{
				Name:         "volume",
				VolumeSource: core.VolumeSource{},
			},
			opts: PodValidationOptions{},
			errs: []verr{{
				etype:  field.ErrorTypeRequired,
				field:  "field[0]",
				detail: "must specify a volume type",
			}},
		}, {
			name: "image volume with empty name",
			vol: core.Volume{
				Name: "",
				VolumeSource: core.VolumeSource{
					Image: &core.ImageVolumeSource{
						Reference:  "quay.io/my/artifact:v1",
						PullPolicy: "IfNotPresent",
					},
				},
			},
			opts: PodValidationOptions{},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "name",
			}},
		}, {
			name: "image volume with empty reference on pod",
			vol: core.Volume{
				Name: "image-volume",
				VolumeSource: core.VolumeSource{
					Image: &core.ImageVolumeSource{
						Reference:  "",
						PullPolicy: "IfNotPresent",
					},
				},
			},
			opts: PodValidationOptions{ResourceIsPod: true},
			errs: []verr{{
				etype: field.ErrorTypeRequired,
				field: "image.reference",
			}},
		}, {
			name: "image volume with empty reference on other object",
			vol: core.Volume{
				Name: "image-volume",
				VolumeSource: core.VolumeSource{
					Image: &core.ImageVolumeSource{
						Reference:  "",
						PullPolicy: "IfNotPresent",
					},
				},
			},
			opts: PodValidationOptions{ResourceIsPod: false},
		}, {
			name: "image volume with wrong pullPolicy",
			vol: core.Volume{
				Name: "image-volume",
				VolumeSource: core.VolumeSource{
					Image: &core.ImageVolumeSource{
						Reference:  "quay.io/my/artifact:v1",
						PullPolicy: "wrong",
					},
				},
			},
			opts: PodValidationOptions{},
			errs: []verr{{
				etype: field.ErrorTypeNotSupported,
				field: "image.pullPolicy",
			}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			names, errs := ValidateVolumes([]core.Volume{tc.vol}, nil, field.NewPath("field"), tc.opts)
			if len(errs) != len(tc.errs) {
				t.Fatalf("unexpected error(s): got %d, want %d: %v", len(tc.errs), len(errs), errs)
			}
			if len(errs) == 0 && (len(names) > 1 || !IsMatchedVolume(tc.vol.Name, names)) {
				t.Errorf("wrong names result: %v", names)
			}
			for i, err := range errs {
				expErr := tc.errs[i]
				if err.Type != expErr.etype {
					t.Errorf("unexpected error type:\n\twant: %q\n\t got: %q", expErr.etype, err.Type)
				}
				if err.Field != expErr.field && !strings.HasSuffix(err.Field, "."+expErr.field) {
					t.Errorf("unexpected error field:\n\twant: %q\n\t got: %q", expErr.field, err.Field)
				}
				if !strings.Contains(err.Detail, expErr.detail) {
					t.Errorf("unexpected error detail:\n\twant: %q\n\t got: %q", expErr.detail, err.Detail)
				}
			}
		})
	}

	dupsCase := []core.Volume{
		{Name: "abc", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}},
		{Name: "abc", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}},
	}
	_, errs := ValidateVolumes(dupsCase, nil, field.NewPath("field"), PodValidationOptions{})
	if len(errs) == 0 {
		t.Errorf("expected error")
	} else if len(errs) != 1 {
		t.Errorf("expected 1 error, got %d: %v", len(errs), errs)
	} else if errs[0].Type != field.ErrorTypeDuplicate {
		t.Errorf("expected error type %v, got %v", field.ErrorTypeDuplicate, errs[0].Type)
	}

	// Validate HugePages medium type for EmptyDir
	hugePagesCase := core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{Medium: core.StorageMediumHugePages}}

	// Enable HugePages
	if errs := validateVolumeSource(&hugePagesCase, field.NewPath("field").Index(0), "working", nil, PodValidationOptions{}); len(errs) != 0 {
		t.Errorf("Unexpected error when HugePages feature is enabled.")
	}

}

func TestHugePagesIsolation(t *testing.T) {
	testCases := map[string]struct {
		pod         *core.Pod
		expectError bool
	}{
		"Valid: request hugepages-2Mi": {
			pod: podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(
					podtest.MakeResourceRequirements(
						map[string]string{
							string(core.ResourceCPU):    "10",
							string(core.ResourceMemory): "10G",
							"hugepages-2Mi":             "1Gi",
						},
						map[string]string{
							string(core.ResourceCPU):    "10",
							string(core.ResourceMemory): "10G",
							"hugepages-2Mi":             "1Gi",
						}))))),
		},
		"Valid: request more than one hugepages size": {
			pod: podtest.MakePod("hugepages-shared",
				podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(
					podtest.MakeResourceRequirements(
						map[string]string{
							string(core.ResourceCPU):    "10",
							string(core.ResourceMemory): "10G",
							"hugepages-2Mi":             "1Gi",
							"hugepages-1Gi":             "2Gi",
						},
						map[string]string{
							string(core.ResourceCPU):    "10",
							string(core.ResourceMemory): "10G",
							"hugepages-2Mi":             "1Gi",
							"hugepages-1Gi":             "2Gi",
						}))))),
			expectError: false,
		},
		"Valid: request hugepages-1Gi, limit hugepages-2Mi and hugepages-1Gi": {
			pod: podtest.MakePod("hugepages-multiple",
				podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(
					podtest.MakeResourceRequirements(
						map[string]string{
							string(core.ResourceCPU):    "10",
							string(core.ResourceMemory): "10G",
							"hugepages-2Mi":             "1Gi",
							"hugepages-1Gi":             "2Gi",
						},
						map[string]string{
							string(core.ResourceCPU):    "10",
							string(core.ResourceMemory): "10G",
							"hugepages-2Mi":             "1Gi",
							"hugepages-1Gi":             "2Gi",
						}))))),
		},
		"Invalid: not requesting cpu and memory": {
			pod: podtest.MakePod("hugepages-requireCpuOrMemory",
				podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(
					podtest.MakeResourceRequirements(
						map[string]string{
							"hugepages-2Mi": "1Gi",
						},
						map[string]string{
							"hugepages-2Mi": "1Gi",
						}))))),
			expectError: true,
		},
		"Invalid: request 1Gi hugepages-2Mi but limit 2Gi": {
			pod: podtest.MakePod("hugepages-shared",
				podtest.SetContainers(podtest.MakeContainer("ctr",
					podtest.SetContainerResources(
						podtest.MakeResourceRequirements(
							map[string]string{
								string(core.ResourceCPU):    "10",
								string(core.ResourceMemory): "10G",
								"hugepages-2Mi":             "1Gi",
							},
							map[string]string{
								string(core.ResourceCPU):    "10",
								string(core.ResourceMemory): "10G",
								"hugepages-2Mi":             "2Gi",
							}))))),
			expectError: true,
		},
	}
	for tcName, tc := range testCases {
		t.Run(tcName, func(t *testing.T) {
			errs := ValidatePodCreate(tc.pod, PodValidationOptions{})
			if tc.expectError && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if !tc.expectError && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

func TestPVCVolumeMode(t *testing.T) {
	block := core.PersistentVolumeBlock
	file := core.PersistentVolumeFilesystem
	fake := core.PersistentVolumeMode("fake")
	empty := core.PersistentVolumeMode("")

	// Success Cases
	successCasesPVC := map[string]*core.PersistentVolumeClaim{
		"valid block value":      createTestVolModePVC(&block),
		"valid filesystem value": createTestVolModePVC(&file),
		"valid nil value":        createTestVolModePVC(nil),
	}
	for k, v := range successCasesPVC {
		opts := ValidationOptionsForPersistentVolumeClaim(v, nil)
		if errs := ValidatePersistentVolumeClaim(v, opts); len(errs) != 0 {
			t.Errorf("expected success for %s", k)
		}
	}

	// Error Cases
	errorCasesPVC := map[string]*core.PersistentVolumeClaim{
		"invalid value": createTestVolModePVC(&fake),
		"empty value":   createTestVolModePVC(&empty),
	}
	for k, v := range errorCasesPVC {
		opts := ValidationOptionsForPersistentVolumeClaim(v, nil)
		if errs := ValidatePersistentVolumeClaim(v, opts); len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
	}
}

func TestPVVolumeMode(t *testing.T) {
	block := core.PersistentVolumeBlock
	file := core.PersistentVolumeFilesystem
	fake := core.PersistentVolumeMode("fake")
	empty := core.PersistentVolumeMode("")

	// Success Cases
	successCasesPV := map[string]*core.PersistentVolume{
		"valid block value":      createTestVolModePV(&block),
		"valid filesystem value": createTestVolModePV(&file),
		"valid nil value":        createTestVolModePV(nil),
	}
	for k, v := range successCasesPV {
		opts := ValidationOptionsForPersistentVolume(v, nil)
		if errs := ValidatePersistentVolume(v, opts); len(errs) != 0 {
			t.Errorf("expected success for %s", k)
		}
	}

	// Error Cases
	errorCasesPV := map[string]*core.PersistentVolume{
		"invalid value": createTestVolModePV(&fake),
		"empty value":   createTestVolModePV(&empty),
	}
	for k, v := range errorCasesPV {
		opts := ValidationOptionsForPersistentVolume(v, nil)
		if errs := ValidatePersistentVolume(v, opts); len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
	}
}

func createTestVolModePVC(vmode *core.PersistentVolumeMode) *core.PersistentVolumeClaim {
	validName := "valid-storage-class"

	pvc := core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
		},
		Spec: core.PersistentVolumeClaimSpec{
			Resources: core.VolumeResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
				},
			},
			AccessModes:      []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			StorageClassName: &validName,
			VolumeMode:       vmode,
		},
	}
	return &pvc
}

func createTestVolModePV(vmode *core.PersistentVolumeMode) *core.PersistentVolume {

	// PersistentVolume with VolumeMode set (valid and invalid)
	pv := core.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "",
		},
		Spec: core.PersistentVolumeSpec{
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			PersistentVolumeSource: core.PersistentVolumeSource{
				HostPath: &core.HostPathVolumeSource{
					Path: "/foo",
					Type: newHostPathType(string(core.HostPathDirectory)),
				},
			},
			StorageClassName: "test-storage-class",
			VolumeMode:       vmode,
		},
	}
	return &pv
}

func createTestPV() *core.PersistentVolume {

	// PersistentVolume with VolumeMode set (valid and invalid)
	pv := core.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "",
		},
		Spec: core.PersistentVolumeSpec{
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			PersistentVolumeSource: core.PersistentVolumeSource{
				HostPath: &core.HostPathVolumeSource{
					Path: "/foo",
					Type: newHostPathType(string(core.HostPathDirectory)),
				},
			},
			StorageClassName: "test-storage-class",
		},
	}
	return &pv
}

func TestAlphaLocalStorageCapacityIsolation(t *testing.T) {

	testCases := []core.VolumeSource{
		{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(int64(5), resource.BinarySI)}},
	}

	for _, tc := range testCases {
		if errs := validateVolumeSource(&tc, field.NewPath("spec"), "tmpvol", nil, PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	containerLimitCase := core.ResourceRequirements{
		Limits: core.ResourceList{
			core.ResourceEphemeralStorage: *resource.NewMilliQuantity(
				int64(40000),
				resource.BinarySI),
		},
	}
	if errs := ValidateContainerResourceRequirements(&containerLimitCase, nil, field.NewPath("resources"), PodValidationOptions{}); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}
}

func TestValidateResourceQuotaWithAlphaLocalStorageCapacityIsolation(t *testing.T) {
	spec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU:                      resource.MustParse("100"),
			core.ResourceMemory:                   resource.MustParse("10000"),
			core.ResourceRequestsCPU:              resource.MustParse("100"),
			core.ResourceRequestsMemory:           resource.MustParse("10000"),
			core.ResourceLimitsCPU:                resource.MustParse("100"),
			core.ResourceLimitsMemory:             resource.MustParse("10000"),
			core.ResourcePods:                     resource.MustParse("10"),
			core.ResourceServices:                 resource.MustParse("0"),
			core.ResourceReplicationControllers:   resource.MustParse("10"),
			core.ResourceQuotas:                   resource.MustParse("10"),
			core.ResourceConfigMaps:               resource.MustParse("10"),
			core.ResourceSecrets:                  resource.MustParse("10"),
			core.ResourceEphemeralStorage:         resource.MustParse("10000"),
			core.ResourceRequestsEphemeralStorage: resource.MustParse("10000"),
			core.ResourceLimitsEphemeralStorage:   resource.MustParse("10000"),
		},
	}
	resourceQuota := &core.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "abc",
			Namespace: "foo",
		},
		Spec: spec,
	}

	if errs := ValidateResourceQuota(resourceQuota); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}
}

func TestValidatePorts(t *testing.T) {
	successCase := []core.ContainerPort{
		{Name: "abc", ContainerPort: 80, HostPort: 80, Protocol: "TCP"},
		{Name: "easy", ContainerPort: 82, Protocol: "TCP"},
		{Name: "as", ContainerPort: 83, Protocol: "UDP"},
		{Name: "do-re-me", ContainerPort: 84, Protocol: "SCTP"},
		{ContainerPort: 85, Protocol: "TCP"},
	}
	if errs := validateContainerPorts(successCase, field.NewPath("field")); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	nonCanonicalCase := []core.ContainerPort{
		{ContainerPort: 80, Protocol: "TCP"},
	}
	if errs := validateContainerPorts(nonCanonicalCase, field.NewPath("field")); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	errorCases := map[string]struct {
		P []core.ContainerPort
		T field.ErrorType
		F string
		D string
	}{
		"name > 15 characters": {
			[]core.ContainerPort{{Name: strings.Repeat("a", 16), ContainerPort: 80, Protocol: "TCP"}},
			field.ErrorTypeInvalid,
			"name", "15",
		},
		"name contains invalid characters": {
			[]core.ContainerPort{{Name: "a.b.c", ContainerPort: 80, Protocol: "TCP"}},
			field.ErrorTypeInvalid,
			"name", "alpha-numeric",
		},
		"name is a number": {
			[]core.ContainerPort{{Name: "80", ContainerPort: 80, Protocol: "TCP"}},
			field.ErrorTypeInvalid,
			"name", "at least one letter",
		},
		"name not unique": {
			[]core.ContainerPort{
				{Name: "abc", ContainerPort: 80, Protocol: "TCP"},
				{Name: "abc", ContainerPort: 81, Protocol: "TCP"},
			},
			field.ErrorTypeDuplicate,
			"[1].name", "",
		},
		"zero container port": {
			[]core.ContainerPort{{ContainerPort: 0, Protocol: "TCP"}},
			field.ErrorTypeRequired,
			"containerPort", "",
		},
		"invalid container port": {
			[]core.ContainerPort{{ContainerPort: 65536, Protocol: "TCP"}},
			field.ErrorTypeInvalid,
			"containerPort", "between",
		},
		"invalid host port": {
			[]core.ContainerPort{{ContainerPort: 80, HostPort: 65536, Protocol: "TCP"}},
			field.ErrorTypeInvalid,
			"hostPort", "between",
		},
		"invalid protocol case": {
			[]core.ContainerPort{{ContainerPort: 80, Protocol: "tcp"}},
			field.ErrorTypeNotSupported,
			"protocol", `supported values: "SCTP", "TCP", "UDP"`,
		},
		"invalid protocol": {
			[]core.ContainerPort{{ContainerPort: 80, Protocol: "ICMP"}},
			field.ErrorTypeNotSupported,
			"protocol", `supported values: "SCTP", "TCP", "UDP"`,
		},
		"protocol required": {
			[]core.ContainerPort{{Name: "abc", ContainerPort: 80}},
			field.ErrorTypeRequired,
			"protocol", "",
		},
	}
	for k, v := range errorCases {
		errs := validateContainerPorts(v.P, field.NewPath("field"))
		if len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
		for i := range errs {
			if errs[i].Type != v.T {
				t.Errorf("%s: expected error to have type %q: %q", k, v.T, errs[i].Type)
			}
			if !strings.Contains(errs[i].Field, v.F) {
				t.Errorf("%s: expected error field %q: %q", k, v.F, errs[i].Field)
			}
			if !strings.Contains(errs[i].Detail, v.D) {
				t.Errorf("%s: expected error detail %q, got %q", k, v.D, errs[i].Detail)
			}
		}
	}
}

func TestLocalStorageEnvWithFeatureGate(t *testing.T) {
	testCases := []core.EnvVar{{
		Name: "ephemeral-storage-limits",
		ValueFrom: &core.EnvVarSource{
			ResourceFieldRef: &core.ResourceFieldSelector{
				ContainerName: "test-container",
				Resource:      "limits.ephemeral-storage",
			},
		},
	}, {
		Name: "ephemeral-storage-requests",
		ValueFrom: &core.EnvVarSource{
			ResourceFieldRef: &core.ResourceFieldSelector{
				ContainerName: "test-container",
				Resource:      "requests.ephemeral-storage",
			},
		},
	},
	}
	for _, testCase := range testCases {
		if errs := validateEnvVarValueFrom(testCase, field.NewPath("field"), PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("expected success, got: %v", errs)
		}
	}
}

func TestHugePagesEnv(t *testing.T) {
	testCases := []core.EnvVar{{
		Name: "hugepages-limits",
		ValueFrom: &core.EnvVarSource{
			ResourceFieldRef: &core.ResourceFieldSelector{
				ContainerName: "test-container",
				Resource:      "limits.hugepages-2Mi",
			},
		},
	}, {
		Name: "hugepages-requests",
		ValueFrom: &core.EnvVarSource{
			ResourceFieldRef: &core.ResourceFieldSelector{
				ContainerName: "test-container",
				Resource:      "requests.hugepages-2Mi",
			},
		},
	},
	}
	// enable gate
	for _, testCase := range testCases {
		t.Run(testCase.Name, func(t *testing.T) {
			opts := PodValidationOptions{}
			if errs := validateEnvVarValueFrom(testCase, field.NewPath("field"), opts); len(errs) != 0 {
				t.Errorf("expected success, got: %v", errs)
			}
		})
	}
}

func TestRelaxedValidateEnv(t *testing.T) {
	successCase := []core.EnvVar{
		{Name: "!\"#$%&'()", Value: "value"},
		{Name: "* +,-./0123456789", Value: "value"},
		{Name: ":;<>?@", Value: "value"},
		{Name: "ABCDEFG", Value: "value"},
		{Name: "abcdefghijklmn", Value: "value"},
		{Name: "[\\]^_`{}|~", Value: "value"},
		{
			Name: "!\"#$%&'()",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.annotations['key']",
				},
			},
		}, {
			Name: "!\"#$%&'()",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.labels['key']",
				},
			},
		}, {
			Name: "* +,-./0123456789",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			},
		}, {
			Name: "* +,-./0123456789",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.namespace",
				},
			},
		}, {
			Name: "* +,-./0123456789",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.uid",
				},
			},
		}, {
			Name: ":;<>?@",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "spec.nodeName",
				},
			},
		}, {
			Name: ":;<>?@",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "spec.serviceAccountName",
				},
			},
		}, {
			Name: ":;<>?@",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "status.hostIP",
				},
			},
		}, {
			Name: ":;<>?@",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "status.podIP",
				},
			},
		}, {
			Name: "abcdefghijklmn",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "status.podIPs",
				},
			},
		},
		{
			Name: "abcdefghijklmn",
			ValueFrom: &core.EnvVarSource{
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "some-secret",
					},
					Key: "secret-key",
				},
			},
		}, {
			Name: "!\"#$%&'()",
			ValueFrom: &core.EnvVarSource{
				ConfigMapKeyRef: &core.ConfigMapKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "some-config-map",
					},
					Key: "some-key",
				},
			},
		},
	}
	if errs := ValidateEnv(successCase, field.NewPath("field"), PodValidationOptions{AllowRelaxedEnvironmentVariableValidation: true}); len(errs) != 0 {
		t.Errorf("expected success, got: %v", errs)
	}

	errorCases := []struct {
		name          string
		envs          []core.EnvVar
		expectedError string
	}{{
		name:          "illegal character",
		envs:          []core.EnvVar{{Name: "=abc"}},
		expectedError: `[0].name: Invalid value: "=abc": ` + relaxedEnvVarNameFmtErrMsg,
	}, {
		name:          "zero-length name",
		envs:          []core.EnvVar{{Name: ""}},
		expectedError: "[0].name: Required value",
	}, {
		name: "value and valueFrom specified",
		envs: []core.EnvVar{{
			Name:  "abc",
			Value: "foo",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			},
		}},
		expectedError: "[0].valueFrom: Invalid value: \"\": may not be specified when `value` is not empty",
	}, {
		name: "valueFrom without a source",
		envs: []core.EnvVar{{
			Name:      "abc",
			ValueFrom: &core.EnvVarSource{},
		}},
		expectedError: "[0].valueFrom: Invalid value: \"\": must specify one of: `fieldRef`, `resourceFieldRef`, `configMapKeyRef` or `secretKeyRef`",
	}, {
		name: "valueFrom.fieldRef and valueFrom.secretKeyRef specified",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "a-secret",
					},
					Key: "a-key",
				},
			},
		}},
		expectedError: "[0].valueFrom: Invalid value: \"\": may not have more than one field specified at a time",
	}, {
		name: "valueFrom.fieldRef and valueFrom.configMapKeyRef set",
		envs: []core.EnvVar{{
			Name: "some_var_name",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
				ConfigMapKeyRef: &core.ConfigMapKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "some-config-map",
					},
					Key: "some-key",
				},
			},
		}},
		expectedError: `[0].valueFrom: Invalid value: "": may not have more than one field specified at a time`,
	}, {
		name: "valueFrom.fieldRef and valueFrom.secretKeyRef specified",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "a-secret",
					},
					Key: "a-key",
				},
				ConfigMapKeyRef: &core.ConfigMapKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "some-config-map",
					},
					Key: "some-key",
				},
			},
		}},
		expectedError: `[0].valueFrom: Invalid value: "": may not have more than one field specified at a time`,
	}, {
		name: "valueFrom.secretKeyRef.name invalid",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "$%^&*#",
					},
					Key: "a-key",
				},
			},
		}},
	}, {
		name: "valueFrom.configMapKeyRef.name invalid",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				ConfigMapKeyRef: &core.ConfigMapKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "$%^&*#",
					},
					Key: "some-key",
				},
			},
		}},
	}, {
		name: "missing FieldPath on ObjectFieldSelector",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Required value`,
	}, {
		name: "missing APIVersion on ObjectFieldSelector",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.apiVersion: Required value`,
	}, {
		name: "invalid fieldPath",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.whoops",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Invalid value: "metadata.whoops": error converting fieldPath`,
	}, {
		name: "metadata.name with subscript",
		envs: []core.EnvVar{{
			Name: "labels",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.name['key']",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Invalid value: "metadata.name['key']": error converting fieldPath: field label does not support subscript`,
	}, {
		name: "metadata.labels without subscript",
		envs: []core.EnvVar{{
			Name: "labels",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.labels",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Unsupported value: "metadata.labels": supported values: "metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs"`,
	}, {
		name: "metadata.annotations without subscript",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.annotations",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Unsupported value: "metadata.annotations": supported values: "metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs"`,
	}, {
		name: "metadata.annotations with invalid key",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.annotations['invalid~key']",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `field[0].valueFrom.fieldRef: Invalid value: "invalid~key"`,
	}, {
		name: "metadata.labels with invalid key",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.labels['Www.k8s.io/test']",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `field[0].valueFrom.fieldRef: Invalid value: "Www.k8s.io/test"`,
	}, {
		name: "unsupported fieldPath",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "status.phase",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `valueFrom.fieldRef.fieldPath: Unsupported value: "status.phase": supported values: "metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs"`,
	},
	}
	for _, tc := range errorCases {
		if errs := ValidateEnv(tc.envs, field.NewPath("field"), PodValidationOptions{AllowRelaxedEnvironmentVariableValidation: true}); len(errs) == 0 {
			t.Errorf("expected failure for %s", tc.name)
		} else {
			for i := range errs {
				str := errs[i].Error()
				if str != "" && !strings.Contains(str, tc.expectedError) {
					t.Errorf("%s: expected error detail either empty or %q, got %q", tc.name, tc.expectedError, str)
				}
			}
		}
	}
}

func TestValidateEnv(t *testing.T) {
	successCase := []core.EnvVar{
		{Name: "abc", Value: "value"},
		{Name: "ABC", Value: "value"},
		{Name: "AbC_123", Value: "value"},
		{Name: "abc", Value: ""},
		{Name: "a.b.c", Value: "value"},
		{Name: "a-b-c", Value: "value"}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.annotations['key']",
				},
			},
		}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.labels['key']",
				},
			},
		}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			},
		}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.namespace",
				},
			},
		}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.uid",
				},
			},
		}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "spec.nodeName",
				},
			},
		}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "spec.serviceAccountName",
				},
			},
		}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "status.hostIP",
				},
			},
		}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "status.podIP",
				},
			},
		}, {
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "status.podIPs",
				},
			},
		}, {
			Name: "secret_value",
			ValueFrom: &core.EnvVarSource{
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "some-secret",
					},
					Key: "secret-key",
				},
			},
		}, {
			Name: "ENV_VAR_1",
			ValueFrom: &core.EnvVarSource{
				ConfigMapKeyRef: &core.ConfigMapKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "some-config-map",
					},
					Key: "some-key",
				},
			},
		},
	}
	if errs := ValidateEnv(successCase, field.NewPath("field"), PodValidationOptions{}); len(errs) != 0 {
		t.Errorf("expected success, got: %v", errs)
	}

	updateSuccessCase := []core.EnvVar{
		{Name: "!\"#$%&'()", Value: "value"},
		{Name: "* +,-./0123456789", Value: "value"},
		{Name: ":;<>?@", Value: "value"},
		{Name: "ABCDEFG", Value: "value"},
		{Name: "abcdefghijklmn", Value: "value"},
		{Name: "[\\]^_`{}|~", Value: "value"},
	}

	if errs := ValidateEnv(updateSuccessCase, field.NewPath("field"), PodValidationOptions{AllowRelaxedEnvironmentVariableValidation: true}); len(errs) != 0 {
		t.Errorf("expected success, got: %v", errs)
	}

	updateErrorCase := []struct {
		name          string
		envs          []core.EnvVar
		expectedError string
	}{
		{
			name: "invalid name a",
			envs: []core.EnvVar{
				{Name: "!\"#$%&'()", Value: "value"},
			},
			expectedError: `field[0].name: Invalid value: ` + "\"!\\\"#$%&'()\": " + envVarNameErrMsg,
		},
		{
			name: "invalid name b",
			envs: []core.EnvVar{
				{Name: "* +,-./0123456789", Value: "value"},
			},
			expectedError: `field[0].name: Invalid value: ` + "\"* +,-./0123456789\": " + envVarNameErrMsg,
		},
		{
			name: "invalid name c",
			envs: []core.EnvVar{
				{Name: ":;<>?@", Value: "value"},
			},
			expectedError: `field[0].name: Invalid value: ` + "\":;<>?@\": " + envVarNameErrMsg,
		},
		{
			name: "invalid name d",
			envs: []core.EnvVar{
				{Name: "[\\]^_{}|~", Value: "value"},
			},
			expectedError: `field[0].name: Invalid value: ` + "\"[\\\\]^_{}|~\": " + envVarNameErrMsg,
		},
	}

	for _, tc := range updateErrorCase {
		if errs := ValidateEnv(tc.envs, field.NewPath("field"), PodValidationOptions{}); len(errs) == 0 {
			t.Errorf("expected failure for %s", tc.name)
		} else {
			for i := range errs {
				str := errs[i].Error()
				if str != "" && !strings.Contains(str, tc.expectedError) {
					t.Errorf("%s: expected error detail either empty or %q, got %q", tc.name, tc.expectedError, str)
				}
			}
		}
	}

	errorCases := []struct {
		name          string
		envs          []core.EnvVar
		expectedError string
	}{{
		name:          "zero-length name",
		envs:          []core.EnvVar{{Name: ""}},
		expectedError: "[0].name: Required value",
	}, {
		name: "value and valueFrom specified",
		envs: []core.EnvVar{{
			Name:  "abc",
			Value: "foo",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			},
		}},
		expectedError: "[0].valueFrom: Invalid value: \"\": may not be specified when `value` is not empty",
	}, {
		name: "valueFrom without a source",
		envs: []core.EnvVar{{
			Name:      "abc",
			ValueFrom: &core.EnvVarSource{},
		}},
		expectedError: "[0].valueFrom: Invalid value: \"\": must specify one of: `fieldRef`, `resourceFieldRef`, `configMapKeyRef` or `secretKeyRef`",
	}, {
		name: "valueFrom.fieldRef and valueFrom.secretKeyRef specified",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "a-secret",
					},
					Key: "a-key",
				},
			},
		}},
		expectedError: "[0].valueFrom: Invalid value: \"\": may not have more than one field specified at a time",
	}, {
		name: "valueFrom.fieldRef and valueFrom.configMapKeyRef set",
		envs: []core.EnvVar{{
			Name: "some_var_name",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
				ConfigMapKeyRef: &core.ConfigMapKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "some-config-map",
					},
					Key: "some-key",
				},
			},
		}},
		expectedError: `[0].valueFrom: Invalid value: "": may not have more than one field specified at a time`,
	}, {
		name: "valueFrom.fieldRef and valueFrom.secretKeyRef specified",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "a-secret",
					},
					Key: "a-key",
				},
				ConfigMapKeyRef: &core.ConfigMapKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "some-config-map",
					},
					Key: "some-key",
				},
			},
		}},
		expectedError: `[0].valueFrom: Invalid value: "": may not have more than one field specified at a time`,
	}, {
		name: "valueFrom.secretKeyRef.name invalid",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "$%^&*#",
					},
					Key: "a-key",
				},
			},
		}},
	}, {
		name: "valueFrom.configMapKeyRef.name invalid",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				ConfigMapKeyRef: &core.ConfigMapKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: "$%^&*#",
					},
					Key: "some-key",
				},
			},
		}},
	}, {
		name: "missing FieldPath on ObjectFieldSelector",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Required value`,
	}, {
		name: "missing APIVersion on ObjectFieldSelector",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.apiVersion: Required value`,
	}, {
		name: "invalid fieldPath",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.whoops",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Invalid value: "metadata.whoops": error converting fieldPath`,
	}, {
		name: "metadata.name with subscript",
		envs: []core.EnvVar{{
			Name: "labels",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.name['key']",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Invalid value: "metadata.name['key']": error converting fieldPath: field label does not support subscript`,
	}, {
		name: "metadata.labels without subscript",
		envs: []core.EnvVar{{
			Name: "labels",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.labels",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Unsupported value: "metadata.labels": supported values: "metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs"`,
	}, {
		name: "metadata.annotations without subscript",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.annotations",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `[0].valueFrom.fieldRef.fieldPath: Unsupported value: "metadata.annotations": supported values: "metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs"`,
	}, {
		name: "metadata.annotations with invalid key",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.annotations['invalid~key']",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `field[0].valueFrom.fieldRef: Invalid value: "invalid~key"`,
	}, {
		name: "metadata.labels with invalid key",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "metadata.labels['Www.k8s.io/test']",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `field[0].valueFrom.fieldRef: Invalid value: "Www.k8s.io/test"`,
	}, {
		name: "unsupported fieldPath",
		envs: []core.EnvVar{{
			Name: "abc",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath:  "status.phase",
					APIVersion: "v1",
				},
			},
		}},
		expectedError: `valueFrom.fieldRef.fieldPath: Unsupported value: "status.phase": supported values: "metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs"`,
	},
	}
	for _, tc := range errorCases {
		if errs := ValidateEnv(tc.envs, field.NewPath("field"), PodValidationOptions{}); len(errs) == 0 {
			t.Errorf("expected failure for %s", tc.name)
		} else {
			for i := range errs {
				str := errs[i].Error()
				if str != "" && !strings.Contains(str, tc.expectedError) {
					t.Errorf("%s: expected error detail either empty or %q, got %q", tc.name, tc.expectedError, str)
				}
			}
		}
	}
}

func TestValidateEnvFrom(t *testing.T) {
	successCase := []core.EnvFromSource{{
		ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "pre_",
		ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "a.b",
		ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		SecretRef: &core.SecretEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "pre_",
		SecretRef: &core.SecretEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "a.b",
		SecretRef: &core.SecretEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	},
	}
	if errs := ValidateEnvFrom(successCase, nil, PodValidationOptions{}); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	updateSuccessCase := []core.EnvFromSource{{
		ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "* +,-./0123456789",
		ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: ":;<>?@",
		ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		SecretRef: &core.SecretEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "abcdefghijklmn",
		SecretRef: &core.SecretEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "[\\]^_`{}|~",
		SecretRef: &core.SecretEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}}

	if errs := ValidateEnvFrom(updateSuccessCase, field.NewPath("field"), PodValidationOptions{AllowRelaxedEnvironmentVariableValidation: true}); len(errs) != 0 {
		t.Errorf("expected success, got: %v", errs)
	}

	updateErrorCase := []struct {
		name          string
		envs          []core.EnvFromSource
		expectedError string
	}{
		{
			name: "invalid name a",
			envs: []core.EnvFromSource{
				{
					Prefix: "!\"#$%&'()",
					SecretRef: &core.SecretEnvSource{
						LocalObjectReference: core.LocalObjectReference{Name: "abc"},
					},
				},
			},
			expectedError: `field[0].prefix: Invalid value: ` + "\"!\\\"#$%&'()\": " + envVarNameErrMsg,
		},
		{
			name: "invalid name b",
			envs: []core.EnvFromSource{
				{
					Prefix: "* +,-./0123456789",
					SecretRef: &core.SecretEnvSource{
						LocalObjectReference: core.LocalObjectReference{Name: "abc"},
					},
				},
			},
			expectedError: `field[0].prefix: Invalid value: ` + "\"* +,-./0123456789\": " + envVarNameErrMsg,
		},
		{
			name: "invalid name c",
			envs: []core.EnvFromSource{
				{
					Prefix: ":;<>?@",
					SecretRef: &core.SecretEnvSource{
						LocalObjectReference: core.LocalObjectReference{Name: "abc"},
					},
				},
			},
			expectedError: `field[0].prefix: Invalid value: ` + "\":;<>?@\": " + envVarNameErrMsg,
		},
		{
			name: "invalid name d",
			envs: []core.EnvFromSource{
				{
					Prefix: "[\\]^_{}|~",
					SecretRef: &core.SecretEnvSource{
						LocalObjectReference: core.LocalObjectReference{Name: "abc"},
					},
				},
			},
			expectedError: `field[0].prefix: Invalid value: ` + "\"[\\\\]^_{}|~\": " + envVarNameErrMsg,
		},
	}

	for _, tc := range updateErrorCase {
		if errs := ValidateEnvFrom(tc.envs, field.NewPath("field"), PodValidationOptions{}); len(errs) == 0 {
			t.Errorf("expected failure for %s", tc.name)
		} else {
			for i := range errs {
				str := errs[i].Error()
				if str != "" && !strings.Contains(str, tc.expectedError) {
					t.Errorf("%s: expected error detail either empty or %q, got %q", tc.name, tc.expectedError, str)
				}
			}
		}
	}

	errorCases := []struct {
		name          string
		envs          []core.EnvFromSource
		expectedError string
	}{{
		name: "zero-length name",
		envs: []core.EnvFromSource{{
			ConfigMapRef: &core.ConfigMapEnvSource{
				LocalObjectReference: core.LocalObjectReference{Name: ""}},
		}},
		expectedError: "field[0].configMapRef.name: Required value",
	}, {
		name: "invalid name",
		envs: []core.EnvFromSource{{
			ConfigMapRef: &core.ConfigMapEnvSource{
				LocalObjectReference: core.LocalObjectReference{Name: "$"}},
		}},
		expectedError: "field[0].configMapRef.name: Invalid value",
	}, {
		name: "zero-length name",
		envs: []core.EnvFromSource{{
			SecretRef: &core.SecretEnvSource{
				LocalObjectReference: core.LocalObjectReference{Name: ""}},
		}},
		expectedError: "field[0].secretRef.name: Required value",
	}, {
		name: "invalid name",
		envs: []core.EnvFromSource{{
			SecretRef: &core.SecretEnvSource{
				LocalObjectReference: core.LocalObjectReference{Name: "&"}},
		}},
		expectedError: "field[0].secretRef.name: Invalid value",
	}, {
		name: "no refs",
		envs: []core.EnvFromSource{
			{},
		},
		expectedError: "field: Invalid value: \"\": must specify one of: `configMapRef` or `secretRef`",
	}, {
		name: "multiple refs",
		envs: []core.EnvFromSource{{
			SecretRef: &core.SecretEnvSource{
				LocalObjectReference: core.LocalObjectReference{Name: "abc"}},
			ConfigMapRef: &core.ConfigMapEnvSource{
				LocalObjectReference: core.LocalObjectReference{Name: "abc"}},
		}},
		expectedError: "field: Invalid value: \"\": may not have more than one field specified at a time",
	}, {
		name: "invalid secret ref name",
		envs: []core.EnvFromSource{{
			SecretRef: &core.SecretEnvSource{
				LocalObjectReference: core.LocalObjectReference{Name: "$%^&*#"}},
		}},
		expectedError: "field[0].secretRef.name: Invalid value: \"$%^&*#\": " + dnsSubdomainLabelErrMsg,
	}, {
		name: "invalid config ref name",
		envs: []core.EnvFromSource{{
			ConfigMapRef: &core.ConfigMapEnvSource{
				LocalObjectReference: core.LocalObjectReference{Name: "$%^&*#"}},
		}},
		expectedError: "field[0].configMapRef.name: Invalid value: \"$%^&*#\": " + dnsSubdomainLabelErrMsg,
	},
	}
	for _, tc := range errorCases {
		if errs := ValidateEnvFrom(tc.envs, field.NewPath("field"), PodValidationOptions{}); len(errs) == 0 {
			t.Errorf("expected failure for %s", tc.name)
		} else {
			for i := range errs {
				str := errs[i].Error()
				if str != "" && !strings.Contains(str, tc.expectedError) {
					t.Errorf("%s: expected error detail either empty or %q, got %q", tc.name, tc.expectedError, str)
				}
			}
		}
	}
}

func TestRelaxedValidateEnvFrom(t *testing.T) {
	successCase := []core.EnvFromSource{{
		ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "!\"#$%&'()",
		ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "* +,-./0123456789",
		ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		SecretRef: &core.SecretEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: ":;<>?@",
		SecretRef: &core.SecretEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	}, {
		Prefix: "[\\]^_`{}|~",
		SecretRef: &core.SecretEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "abc"},
		},
	},
	}
	if errs := ValidateEnvFrom(successCase, field.NewPath("field"), PodValidationOptions{AllowRelaxedEnvironmentVariableValidation: true}); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	errorCases := []struct {
		name          string
		envs          []core.EnvFromSource
		expectedError string
	}{
		{
			name: "zero-length name",
			envs: []core.EnvFromSource{{
				ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: ""}},
			}},
			expectedError: "field[0].configMapRef.name: Required value",
		},
		{
			name: "invalid prefix",
			envs: []core.EnvFromSource{{
				Prefix: "=abc",
				ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "abc"}},
			}},
			expectedError: `field[0].prefix: Invalid value: "=abc": ` + relaxedEnvVarNameFmtErrMsg,
		},
		{
			name: "zero-length name",
			envs: []core.EnvFromSource{{
				SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: ""}},
			}},
			expectedError: "field[0].secretRef.name: Required value",
		}, {
			name: "invalid name",
			envs: []core.EnvFromSource{{
				SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "&"}},
			}},
			expectedError: "field[0].secretRef.name: Invalid value",
		}, {
			name: "no refs",
			envs: []core.EnvFromSource{
				{},
			},
			expectedError: "field: Invalid value: \"\": must specify one of: `configMapRef` or `secretRef`",
		}, {
			name: "multiple refs",
			envs: []core.EnvFromSource{{
				SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "abc"}},
				ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "abc"}},
			}},
			expectedError: "field: Invalid value: \"\": may not have more than one field specified at a time",
		}, {
			name: "invalid secret ref name",
			envs: []core.EnvFromSource{{
				SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "$%^&*#"}},
			}},
			expectedError: "field[0].secretRef.name: Invalid value: \"$%^&*#\": " + dnsSubdomainLabelErrMsg,
		}, {
			name: "invalid config ref name",
			envs: []core.EnvFromSource{{
				ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "$%^&*#"}},
			}},
			expectedError: "field[0].configMapRef.name: Invalid value: \"$%^&*#\": " + dnsSubdomainLabelErrMsg,
		},
	}
	for _, tc := range errorCases {
		if errs := ValidateEnvFrom(tc.envs, field.NewPath("field"), PodValidationOptions{AllowRelaxedEnvironmentVariableValidation: true}); len(errs) == 0 {
			t.Errorf("expected failure for %s", tc.name)
		} else {
			for i := range errs {
				str := errs[i].Error()
				if str != "" && !strings.Contains(str, tc.expectedError) {
					t.Errorf("%s: expected error detail either empty or %q, got %q", tc.name, tc.expectedError, str)
				}
			}
		}
	}
}

func TestValidateVolumeMounts(t *testing.T) {
	volumes := []core.Volume{
		{Name: "abc", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "testclaim1"}}},
		{Name: "abc-123", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "testclaim2"}}},
		{Name: "123", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: "/foo/baz", Type: newHostPathType(string(core.HostPathUnset))}}},
		{Name: "ephemeral", VolumeSource: core.VolumeSource{Ephemeral: &core.EphemeralVolumeSource{VolumeClaimTemplate: &core.PersistentVolumeClaimTemplate{
			Spec: core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
			},
		}}}},
		{Name: "image-volume", VolumeSource: core.VolumeSource{Image: &core.ImageVolumeSource{Reference: "quay.io/my/artifact:v1", PullPolicy: "IfNotPresent"}}},
	}
	opts := PodValidationOptions{}
	vols, v1err := ValidateVolumes(volumes, nil, field.NewPath("field"), opts)
	if len(v1err) > 0 {
		t.Errorf("Invalid test volume - expected success %v", v1err)
		return
	}
	container := core.Container{
		SecurityContext: nil,
	}
	propagation := core.MountPropagationBidirectional

	successCase := []core.VolumeMount{
		{Name: "abc", MountPath: "/foo"},
		{Name: "123", MountPath: "/bar"},
		{Name: "abc-123", MountPath: "/baz"},
		{Name: "abc-123", MountPath: "/baa", SubPath: ""},
		{Name: "abc-123", MountPath: "/bab", SubPath: "baz"},
		{Name: "abc-123", MountPath: "d:", SubPath: ""},
		{Name: "abc-123", MountPath: "F:", SubPath: ""},
		{Name: "abc-123", MountPath: "G:\\mount", SubPath: ""},
		{Name: "abc-123", MountPath: "/bac", SubPath: ".baz"},
		{Name: "abc-123", MountPath: "/bad", SubPath: "..baz"},
		{Name: "ephemeral", MountPath: "/foobar"},
		{Name: "123", MountPath: "/rro-nil", ReadOnly: true, RecursiveReadOnly: nil},
		{Name: "123", MountPath: "/rro-disabled", ReadOnly: true, RecursiveReadOnly: ptr.To(core.RecursiveReadOnlyDisabled)},
		{Name: "123", MountPath: "/rro-disabled-2", ReadOnly: false, RecursiveReadOnly: ptr.To(core.RecursiveReadOnlyDisabled)},
		{Name: "123", MountPath: "/rro-ifpossible", ReadOnly: true, RecursiveReadOnly: ptr.To(core.RecursiveReadOnlyIfPossible)},
		{Name: "123", MountPath: "/rro-enabled", ReadOnly: true, RecursiveReadOnly: ptr.To(core.RecursiveReadOnlyEnabled)},
		{Name: "123", MountPath: "/rro-enabled-2", ReadOnly: true, RecursiveReadOnly: ptr.To(core.RecursiveReadOnlyEnabled), MountPropagation: ptr.To(core.MountPropagationNone)},
		{Name: "image-volume", MountPath: "/image-volume"},
	}
	goodVolumeDevices := []core.VolumeDevice{
		{Name: "xyz", DevicePath: "/foofoo"},
		{Name: "uvw", DevicePath: "/foofoo/share/test"},
	}
	if errs := ValidateVolumeMounts(successCase, GetVolumeDeviceMap(goodVolumeDevices), vols, &container, field.NewPath("field"), opts); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	errorCases := map[string][]core.VolumeMount{
		"empty name":                                       {{Name: "", MountPath: "/foo"}},
		"name not found":                                   {{Name: "", MountPath: "/foo"}},
		"empty mountpath":                                  {{Name: "abc", MountPath: ""}},
		"mountpath collision":                              {{Name: "foo", MountPath: "/path/a"}, {Name: "bar", MountPath: "/path/a"}},
		"absolute subpath":                                 {{Name: "abc", MountPath: "/bar", SubPath: "/baz"}},
		"subpath in ..":                                    {{Name: "abc", MountPath: "/bar", SubPath: "../baz"}},
		"subpath contains ..":                              {{Name: "abc", MountPath: "/bar", SubPath: "baz/../bat"}},
		"subpath ends in ..":                               {{Name: "abc", MountPath: "/bar", SubPath: "./.."}},
		"disabled MountPropagation feature gate":           {{Name: "abc", MountPath: "/bar", MountPropagation: &propagation}},
		"name exists in volumeDevice":                      {{Name: "xyz", MountPath: "/bar"}},
		"mountpath exists in volumeDevice":                 {{Name: "uvw", MountPath: "/mnt/exists"}},
		"both exist in volumeDevice":                       {{Name: "xyz", MountPath: "/mnt/exists"}},
		"rro but not ro":                                   {{Name: "123", MountPath: "/rro-bad1", ReadOnly: false, RecursiveReadOnly: ptr.To(core.RecursiveReadOnlyEnabled)}},
		"rro with incompatible propagation":                {{Name: "123", MountPath: "/rro-bad2", ReadOnly: true, RecursiveReadOnly: ptr.To(core.RecursiveReadOnlyEnabled), MountPropagation: ptr.To(core.MountPropagationHostToContainer)}},
		"rro-if-possible but not ro":                       {{Name: "123", MountPath: "/rro-bad1", ReadOnly: false, RecursiveReadOnly: ptr.To(core.RecursiveReadOnlyIfPossible)}},
		"subPath not allowed for image volume sources":     {{Name: "image-volume", MountPath: "/image-volume-err-1", SubPath: "/foo"}},
		"subPathExpr not allowed for image volume sources": {{Name: "image-volume", MountPath: "/image-volume-err-2", SubPathExpr: "$(POD_NAME)"}},
	}
	badVolumeDevice := []core.VolumeDevice{
		{Name: "xyz", DevicePath: "/mnt/exists"},
	}

	for k, v := range errorCases {
		if errs := ValidateVolumeMounts(v, GetVolumeDeviceMap(badVolumeDevice), vols, &container, field.NewPath("field"), opts); len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
	}
}

func TestValidateSubpathMutuallyExclusive(t *testing.T) {
	volumes := []core.Volume{
		{Name: "abc", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "testclaim1"}}},
		{Name: "abc-123", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "testclaim2"}}},
		{Name: "123", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: "/foo/baz", Type: newHostPathType(string(core.HostPathUnset))}}},
	}
	vols, v1err := ValidateVolumes(volumes, nil, field.NewPath("field"), PodValidationOptions{})
	if len(v1err) > 0 {
		t.Errorf("Invalid test volume - expected success %v", v1err)
		return
	}

	container := core.Container{
		SecurityContext: nil,
	}

	goodVolumeDevices := []core.VolumeDevice{
		{Name: "xyz", DevicePath: "/foofoo"},
		{Name: "uvw", DevicePath: "/foofoo/share/test"},
	}

	cases := map[string]struct {
		mounts      []core.VolumeMount
		expectError bool
	}{
		"subpath and subpathexpr not specified": {
			[]core.VolumeMount{{
				Name:      "abc-123",
				MountPath: "/bab",
			}},
			false,
		},
		"subpath expr specified": {
			[]core.VolumeMount{{
				Name:        "abc-123",
				MountPath:   "/bab",
				SubPathExpr: "$(POD_NAME)",
			}},
			false,
		},
		"subpath specified": {
			[]core.VolumeMount{{
				Name:      "abc-123",
				MountPath: "/bab",
				SubPath:   "baz",
			}},
			false,
		},
		"subpath and subpathexpr specified": {
			[]core.VolumeMount{{
				Name:        "abc-123",
				MountPath:   "/bab",
				SubPath:     "baz",
				SubPathExpr: "$(POD_NAME)",
			}},
			true,
		},
	}

	for name, test := range cases {
		errs := ValidateVolumeMounts(test.mounts, GetVolumeDeviceMap(goodVolumeDevices), vols, &container, field.NewPath("field"), PodValidationOptions{})

		if len(errs) != 0 && !test.expectError {
			t.Errorf("test %v failed: %+v", name, errs)
		}

		if len(errs) == 0 && test.expectError {
			t.Errorf("test %v failed, expected error", name)
		}
	}
}

func TestValidateDisabledSubpathExpr(t *testing.T) {

	volumes := []core.Volume{
		{Name: "abc", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "testclaim1"}}},
		{Name: "abc-123", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "testclaim2"}}},
		{Name: "123", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: "/foo/baz", Type: newHostPathType(string(core.HostPathUnset))}}},
	}
	vols, v1err := ValidateVolumes(volumes, nil, field.NewPath("field"), PodValidationOptions{})
	if len(v1err) > 0 {
		t.Errorf("Invalid test volume - expected success %v", v1err)
		return
	}

	container := core.Container{
		SecurityContext: nil,
	}

	goodVolumeDevices := []core.VolumeDevice{
		{Name: "xyz", DevicePath: "/foofoo"},
		{Name: "uvw", DevicePath: "/foofoo/share/test"},
	}

	cases := map[string]struct {
		mounts      []core.VolumeMount
		expectError bool
	}{
		"subpath expr not specified": {
			[]core.VolumeMount{{
				Name:      "abc-123",
				MountPath: "/bab",
			}},
			false,
		},
		"subpath expr specified": {
			[]core.VolumeMount{{
				Name:        "abc-123",
				MountPath:   "/bab",
				SubPathExpr: "$(POD_NAME)",
			}},
			false,
		},
	}

	for name, test := range cases {
		errs := ValidateVolumeMounts(test.mounts, GetVolumeDeviceMap(goodVolumeDevices), vols, &container, field.NewPath("field"), PodValidationOptions{})

		if len(errs) != 0 && !test.expectError {
			t.Errorf("test %v failed: %+v", name, errs)
		}

		if len(errs) == 0 && test.expectError {
			t.Errorf("test %v failed, expected error", name)
		}
	}
}

func TestValidateMountPropagation(t *testing.T) {
	bTrue := true
	bFalse := false
	privilegedContainer := &core.Container{
		SecurityContext: &core.SecurityContext{
			Privileged: &bTrue,
		},
	}
	nonPrivilegedContainer := &core.Container{
		SecurityContext: &core.SecurityContext{
			Privileged: &bFalse,
		},
	}
	defaultContainer := &core.Container{}

	propagationBidirectional := core.MountPropagationBidirectional
	propagationHostToContainer := core.MountPropagationHostToContainer
	propagationNone := core.MountPropagationNone
	propagationInvalid := core.MountPropagationMode("invalid")

	tests := []struct {
		mount       core.VolumeMount
		container   *core.Container
		expectError bool
	}{{
		// implicitly non-privileged container + no propagation
		core.VolumeMount{Name: "foo", MountPath: "/foo"},
		defaultContainer,
		false,
	}, {
		// implicitly non-privileged container + HostToContainer
		core.VolumeMount{Name: "foo", MountPath: "/foo", MountPropagation: &propagationHostToContainer},
		defaultContainer,
		false,
	}, {
		// non-privileged container + None
		core.VolumeMount{Name: "foo", MountPath: "/foo", MountPropagation: &propagationNone},
		defaultContainer,
		false,
	}, {
		// error: implicitly non-privileged container + Bidirectional
		core.VolumeMount{Name: "foo", MountPath: "/foo", MountPropagation: &propagationBidirectional},
		defaultContainer,
		true,
	}, {
		// explicitly non-privileged container + no propagation
		core.VolumeMount{Name: "foo", MountPath: "/foo"},
		nonPrivilegedContainer,
		false,
	}, {
		// explicitly non-privileged container + HostToContainer
		core.VolumeMount{Name: "foo", MountPath: "/foo", MountPropagation: &propagationHostToContainer},
		nonPrivilegedContainer,
		false,
	}, {
		// explicitly non-privileged container + HostToContainer
		core.VolumeMount{Name: "foo", MountPath: "/foo", MountPropagation: &propagationBidirectional},
		nonPrivilegedContainer,
		true,
	}, {
		// privileged container + no propagation
		core.VolumeMount{Name: "foo", MountPath: "/foo"},
		privilegedContainer,
		false,
	}, {
		// privileged container + HostToContainer
		core.VolumeMount{Name: "foo", MountPath: "/foo", MountPropagation: &propagationHostToContainer},
		privilegedContainer,
		false,
	}, {
		// privileged container + Bidirectional
		core.VolumeMount{Name: "foo", MountPath: "/foo", MountPropagation: &propagationBidirectional},
		privilegedContainer,
		false,
	}, {
		// error: privileged container + invalid mount propagation
		core.VolumeMount{Name: "foo", MountPath: "/foo", MountPropagation: &propagationInvalid},
		privilegedContainer,
		true,
	}, {
		// no container + Bidirectional
		core.VolumeMount{Name: "foo", MountPath: "/foo", MountPropagation: &propagationBidirectional},
		nil,
		false,
	},
	}

	volumes := []core.Volume{
		{Name: "foo", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: "/foo/baz", Type: newHostPathType(string(core.HostPathUnset))}}},
	}
	vols2, v2err := ValidateVolumes(volumes, nil, field.NewPath("field"), PodValidationOptions{})
	if len(v2err) > 0 {
		t.Errorf("Invalid test volume - expected success %v", v2err)
		return
	}
	for i, test := range tests {
		errs := ValidateVolumeMounts([]core.VolumeMount{test.mount}, nil, vols2, test.container, field.NewPath("field"), PodValidationOptions{})
		if test.expectError && len(errs) == 0 {
			t.Errorf("test %d expected error, got none", i)
		}
		if !test.expectError && len(errs) != 0 {
			t.Errorf("test %d expected success, got error: %v", i, errs)
		}
	}
}

func TestAlphaValidateVolumeDevices(t *testing.T) {
	volumes := []core.Volume{
		{Name: "abc", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "testclaim1"}}},
		{Name: "abc-123", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "testclaim2"}}},
		{Name: "def", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: "/foo/baz", Type: newHostPathType(string(core.HostPathUnset))}}},
		{Name: "ephemeral", VolumeSource: core.VolumeSource{Ephemeral: &core.EphemeralVolumeSource{VolumeClaimTemplate: &core.PersistentVolumeClaimTemplate{
			Spec: core.PersistentVolumeClaimSpec{
				AccessModes: []core.PersistentVolumeAccessMode{
					core.ReadWriteOnce,
				},
				Resources: core.VolumeResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
					},
				},
			},
		}}}},
	}

	vols, v1err := ValidateVolumes(volumes, nil, field.NewPath("field"), PodValidationOptions{})
	if len(v1err) > 0 {
		t.Errorf("Invalid test volumes - expected success %v", v1err)
		return
	}

	successCase := []core.VolumeDevice{
		{Name: "abc", DevicePath: "/foo"},
		{Name: "abc-123", DevicePath: "/usr/share/test"},
		{Name: "ephemeral", DevicePath: "/disk"},
	}
	goodVolumeMounts := []core.VolumeMount{
		{Name: "xyz", MountPath: "/foofoo"},
		{Name: "ghi", MountPath: "/foo/usr/share/test"},
	}

	errorCases := map[string][]core.VolumeDevice{
		"empty name":                    {{Name: "", DevicePath: "/foo"}},
		"duplicate name":                {{Name: "abc", DevicePath: "/foo"}, {Name: "abc", DevicePath: "/foo/bar"}},
		"name not found":                {{Name: "not-found", DevicePath: "/usr/share/test"}},
		"name found but invalid source": {{Name: "def", DevicePath: "/usr/share/test"}},
		"empty devicepath":              {{Name: "abc", DevicePath: ""}},
		"relative devicepath":           {{Name: "abc-123", DevicePath: "baz"}},
		"duplicate devicepath":          {{Name: "abc", DevicePath: "/foo"}, {Name: "abc-123", DevicePath: "/foo"}},
		"no backsteps":                  {{Name: "def", DevicePath: "/baz/../"}},
		"name exists in volumemounts":   {{Name: "abc", DevicePath: "/baz/../"}},
		"path exists in volumemounts":   {{Name: "xyz", DevicePath: "/this/path/exists"}},
		"both exist in volumemounts":    {{Name: "abc", DevicePath: "/this/path/exists"}},
	}
	badVolumeMounts := []core.VolumeMount{
		{Name: "abc", MountPath: "/foo"},
		{Name: "abc-123", MountPath: "/this/path/exists"},
	}

	// Success Cases:
	// Validate normal success cases - only PVC volumeSource or generic ephemeral volume
	if errs := ValidateVolumeDevices(successCase, GetVolumeMountMap(goodVolumeMounts), vols, field.NewPath("field")); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	// Error Cases:
	// Validate normal error cases - only PVC volumeSource
	for k, v := range errorCases {
		if errs := ValidateVolumeDevices(v, GetVolumeMountMap(badVolumeMounts), vols, field.NewPath("field")); len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
	}
}

func TestValidateProbe(t *testing.T) {
	handler := core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}}
	// These fields must be positive.
	positiveFields := [...]string{"InitialDelaySeconds", "TimeoutSeconds", "PeriodSeconds", "SuccessThreshold", "FailureThreshold"}
	successCases := []*core.Probe{nil}
	for _, field := range positiveFields {
		probe := &core.Probe{ProbeHandler: handler}
		reflect.ValueOf(probe).Elem().FieldByName(field).SetInt(10)
		successCases = append(successCases, probe)
	}

	for _, p := range successCases {
		if errs := validateProbe(p, defaultGracePeriod, field.NewPath("field"), PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := []*core.Probe{{TimeoutSeconds: 10, InitialDelaySeconds: 10}}
	for _, field := range positiveFields {
		probe := &core.Probe{ProbeHandler: handler}
		reflect.ValueOf(probe).Elem().FieldByName(field).SetInt(-10)
		errorCases = append(errorCases, probe)
	}
	for _, p := range errorCases {
		if errs := validateProbe(p, defaultGracePeriod, field.NewPath("field"), PodValidationOptions{}); len(errs) == 0 {
			t.Errorf("expected failure for %v", p)
		}
	}
}

func Test_validateProbe(t *testing.T) {
	fldPath := field.NewPath("test")
	type args struct {
		probe   *core.Probe
		fldPath *field.Path
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{{
		args: args{
			probe:   &core.Probe{},
			fldPath: fldPath,
		},
		want: field.ErrorList{field.Required(fldPath, "must specify a handler type")},
	}, {
		args: args{
			probe: &core.Probe{
				ProbeHandler: core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}},
			},
			fldPath: fldPath,
		},
		want: field.ErrorList{},
	}, {
		args: args{
			probe: &core.Probe{
				ProbeHandler:        core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}},
				InitialDelaySeconds: -1,
			},
			fldPath: fldPath,
		},
		want: field.ErrorList{field.Invalid(fldPath.Child("initialDelaySeconds"), -1, "must be greater than or equal to 0")},
	}, {
		args: args{
			probe: &core.Probe{
				ProbeHandler:   core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}},
				TimeoutSeconds: -1,
			},
			fldPath: fldPath,
		},
		want: field.ErrorList{field.Invalid(fldPath.Child("timeoutSeconds"), -1, "must be greater than or equal to 0")},
	}, {
		args: args{
			probe: &core.Probe{
				ProbeHandler:  core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}},
				PeriodSeconds: -1,
			},
			fldPath: fldPath,
		},
		want: field.ErrorList{field.Invalid(fldPath.Child("periodSeconds"), -1, "must be greater than or equal to 0")},
	}, {
		args: args{
			probe: &core.Probe{
				ProbeHandler:     core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}},
				SuccessThreshold: -1,
			},
			fldPath: fldPath,
		},
		want: field.ErrorList{field.Invalid(fldPath.Child("successThreshold"), -1, "must be greater than or equal to 0")},
	}, {
		args: args{
			probe: &core.Probe{
				ProbeHandler:     core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}},
				FailureThreshold: -1,
			},
			fldPath: fldPath,
		},
		want: field.ErrorList{field.Invalid(fldPath.Child("failureThreshold"), -1, "must be greater than or equal to 0")},
	}, {
		args: args{
			probe: &core.Probe{
				ProbeHandler:                  core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}},
				TerminationGracePeriodSeconds: utilpointer.Int64(-1),
			},
			fldPath: fldPath,
		},
		want: field.ErrorList{field.Invalid(fldPath.Child("terminationGracePeriodSeconds"), -1, "must be greater than 0")},
	}, {
		args: args{
			probe: &core.Probe{
				ProbeHandler:                  core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}},
				TerminationGracePeriodSeconds: utilpointer.Int64(0),
			},
			fldPath: fldPath,
		},
		want: field.ErrorList{field.Invalid(fldPath.Child("terminationGracePeriodSeconds"), 0, "must be greater than 0")},
	}, {
		args: args{
			probe: &core.Probe{
				ProbeHandler:                  core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"echo"}}},
				TerminationGracePeriodSeconds: utilpointer.Int64(1),
			},
			fldPath: fldPath,
		},
		want: field.ErrorList{},
	},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateProbe(tt.args.probe, defaultGracePeriod, tt.args.fldPath, PodValidationOptions{})
			if len(got) != len(tt.want) {
				t.Errorf("validateProbe() = %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i].Type != tt.want[i].Type ||
					got[i].Field != tt.want[i].Field {
					t.Errorf("validateProbe()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidateHandler(t *testing.T) {
	successCases := []core.ProbeHandler{
		{Exec: &core.ExecAction{Command: []string{"echo"}}},
		{HTTPGet: &core.HTTPGetAction{Path: "/", Port: intstr.FromInt32(1), Host: "", Scheme: "HTTP"}},
		{HTTPGet: &core.HTTPGetAction{Path: "/foo", Port: intstr.FromInt32(65535), Host: "host", Scheme: "HTTP"}},
		{HTTPGet: &core.HTTPGetAction{Path: "/", Port: intstr.FromString("port"), Host: "", Scheme: "HTTP"}},
		{HTTPGet: &core.HTTPGetAction{Path: "/", Port: intstr.FromString("port"), Host: "", Scheme: "HTTP", HTTPHeaders: []core.HTTPHeader{{Name: "Host", Value: "foo.example.com"}}}},
		{HTTPGet: &core.HTTPGetAction{Path: "/", Port: intstr.FromString("port"), Host: "", Scheme: "HTTP", HTTPHeaders: []core.HTTPHeader{{Name: "X-Forwarded-For", Value: "1.2.3.4"}, {Name: "X-Forwarded-For", Value: "5.6.7.8"}}}},
	}
	for _, h := range successCases {
		if errs := validateHandler(handlerFromProbe(&h), defaultGracePeriod, field.NewPath("field"), PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := []core.ProbeHandler{
		{},
		{Exec: &core.ExecAction{Command: []string{}}},
		{HTTPGet: &core.HTTPGetAction{Path: "", Port: intstr.FromInt32(0), Host: ""}},
		{HTTPGet: &core.HTTPGetAction{Path: "/foo", Port: intstr.FromInt32(65536), Host: "host"}},
		{HTTPGet: &core.HTTPGetAction{Path: "", Port: intstr.FromString(""), Host: ""}},
		{HTTPGet: &core.HTTPGetAction{Path: "/", Port: intstr.FromString("port"), Host: "", Scheme: "HTTP", HTTPHeaders: []core.HTTPHeader{{Name: "Host:", Value: "foo.example.com"}}}},
		{HTTPGet: &core.HTTPGetAction{Path: "/", Port: intstr.FromString("port"), Host: "", Scheme: "HTTP", HTTPHeaders: []core.HTTPHeader{{Name: "X_Forwarded_For", Value: "foo.example.com"}}}},
	}
	for _, h := range errorCases {
		if errs := validateHandler(handlerFromProbe(&h), defaultGracePeriod, field.NewPath("field"), PodValidationOptions{}); len(errs) == 0 {
			t.Errorf("expected failure for %#v", h)
		}
	}
}

func TestValidatePullPolicy(t *testing.T) {
	type T struct {
		Container      core.Container
		ExpectedPolicy core.PullPolicy
	}
	testCases := map[string]T{
		"NotPresent1": {
			core.Container{Name: "abc", Image: "image:latest", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
			core.PullIfNotPresent,
		},
		"NotPresent2": {
			core.Container{Name: "abc1", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
			core.PullIfNotPresent,
		},
		"Always1": {
			core.Container{Name: "123", Image: "image:latest", ImagePullPolicy: "Always"},
			core.PullAlways,
		},
		"Always2": {
			core.Container{Name: "1234", Image: "image", ImagePullPolicy: "Always"},
			core.PullAlways,
		},
		"Never1": {
			core.Container{Name: "abc-123", Image: "image:latest", ImagePullPolicy: "Never"},
			core.PullNever,
		},
		"Never2": {
			core.Container{Name: "abc-1234", Image: "image", ImagePullPolicy: "Never"},
			core.PullNever,
		},
	}
	for k, v := range testCases {
		ctr := &v.Container
		errs := validatePullPolicy(ctr.ImagePullPolicy, field.NewPath("field"))
		if len(errs) != 0 {
			t.Errorf("case[%s] expected success, got %#v", k, errs)
		}
		if ctr.ImagePullPolicy != v.ExpectedPolicy {
			t.Errorf("case[%s] expected policy %v, got %v", k, v.ExpectedPolicy, ctr.ImagePullPolicy)
		}
	}
}

func TestValidateResizePolicy(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)
	tSupportedResizeResources := sets.NewString(string(core.ResourceCPU), string(core.ResourceMemory))
	tSupportedResizePolicies := sets.NewString(string(core.NotRequired), string(core.RestartContainer))
	type T struct {
		PolicyList       []core.ContainerResizePolicy
		ExpectError      bool
		Errors           field.ErrorList
		PodRestartPolicy core.RestartPolicy
	}

	testCases := map[string]T{
		"ValidCPUandMemoryPolicies": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "NotRequired"},
				{ResourceName: "memory", RestartPolicy: "RestartContainer"},
			},
			ExpectError:      false,
			Errors:           nil,
			PodRestartPolicy: "Always",
		},
		"ValidCPUPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "RestartContainer"},
			},
			ExpectError:      false,
			Errors:           nil,
			PodRestartPolicy: "Always",
		},
		"ValidMemoryPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "memory", RestartPolicy: "NotRequired"},
			},
			ExpectError:      false,
			Errors:           nil,
			PodRestartPolicy: "Always",
		},
		"NoPolicy": {
			PolicyList:       []core.ContainerResizePolicy{},
			ExpectError:      false,
			Errors:           nil,
			PodRestartPolicy: "Always",
		},
		"ValidCPUandInvalidMemoryPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "NotRequired"},
				{ResourceName: "memory", RestartPolicy: "Restarrrt"},
			},
			ExpectError:      true,
			Errors:           field.ErrorList{field.NotSupported(field.NewPath("field"), core.ResourceResizeRestartPolicy("Restarrrt"), tSupportedResizePolicies.List())},
			PodRestartPolicy: "Always",
		},
		"ValidMemoryandInvalidCPUPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "RestartNotRequirrred"},
				{ResourceName: "memory", RestartPolicy: "RestartContainer"},
			},
			ExpectError:      true,
			Errors:           field.ErrorList{field.NotSupported(field.NewPath("field"), core.ResourceResizeRestartPolicy("RestartNotRequirrred"), tSupportedResizePolicies.List())},
			PodRestartPolicy: "Always",
		},
		"InvalidResourceNameValidPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpuuu", RestartPolicy: "NotRequired"},
			},
			ExpectError:      true,
			Errors:           field.ErrorList{field.NotSupported(field.NewPath("field"), core.ResourceName("cpuuu"), tSupportedResizeResources.List())},
			PodRestartPolicy: "Always",
		},
		"ValidResourceNameMissingPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "memory", RestartPolicy: ""},
			},
			ExpectError:      true,
			Errors:           field.ErrorList{field.Required(field.NewPath("field"), "")},
			PodRestartPolicy: "Always",
		},
		"RepeatedPolicies": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "NotRequired"},
				{ResourceName: "memory", RestartPolicy: "RestartContainer"},
				{ResourceName: "cpu", RestartPolicy: "RestartContainer"},
			},
			ExpectError:      true,
			Errors:           field.ErrorList{field.Duplicate(field.NewPath("field").Index(2), core.ResourceCPU)},
			PodRestartPolicy: "Always",
		},
		"InvalidCPUPolicyWithPodRestartPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "NotRequired"},
				{ResourceName: "memory", RestartPolicy: "RestartContainer"},
			},
			ExpectError:      true,
			Errors:           field.ErrorList{field.Invalid(field.NewPath("field"), core.ResourceResizeRestartPolicy("RestartContainer"), "must be 'NotRequired' when `restartPolicy` is 'Never'")},
			PodRestartPolicy: "Never",
		},
		"InvalidMemoryPolicyWithPodRestartPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "RestartContainer"},
				{ResourceName: "memory", RestartPolicy: "NotRequired"},
			},
			ExpectError:      true,
			Errors:           field.ErrorList{field.Invalid(field.NewPath("field"), core.ResourceResizeRestartPolicy("RestartContainer"), "must be 'NotRequired' when `restartPolicy` is 'Never'")},
			PodRestartPolicy: "Never",
		},
		"InvalidMemoryCPUPolicyWithPodRestartPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "RestartContainer"},
				{ResourceName: "memory", RestartPolicy: "RestartContainer"},
			},
			ExpectError:      true,
			Errors:           field.ErrorList{field.Invalid(field.NewPath("field"), core.ResourceResizeRestartPolicy("RestartContainer"), "must be 'NotRequired' when `restartPolicy` is 'Never'"), field.Invalid(field.NewPath("field"), core.ResourceResizeRestartPolicy("RestartContainer"), "must be 'NotRequired' when `restartPolicy` is 'Never'")},
			PodRestartPolicy: "Never",
		},
		"ValidMemoryCPUPolicyWithPodRestartPolicy": {
			PolicyList: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "NotRequired"},
				{ResourceName: "memory", RestartPolicy: "NotRequired"},
			},
			ExpectError:      false,
			Errors:           nil,
			PodRestartPolicy: "Never",
		},
	}
	for k, v := range testCases {
		errs := validateResizePolicy(v.PolicyList, field.NewPath("field"), &v.PodRestartPolicy)
		if !v.ExpectError && len(errs) > 0 {
			t.Errorf("Testcase %s - expected success, got error: %+v", k, errs)
		}
		if v.ExpectError {
			if len(errs) == 0 {
				t.Errorf("Testcase %s - expected error, got success", k)
			}
			delta := cmp.Diff(errs, v.Errors)
			if delta != "" {
				t.Errorf("Testcase %s - expected errors '%v', got '%v', diff: '%v'", k, v.Errors, errs, delta)
			}
		}
	}
}

func getResources(cpu, memory, ephemeralStorage, persistentStorage string) core.ResourceList {
	res := core.ResourceList{}
	if cpu != "" {
		res[core.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		res[core.ResourceMemory] = resource.MustParse(memory)
	}
	if ephemeralStorage != "" {
		res[core.ResourceEphemeralStorage] = resource.MustParse(ephemeralStorage)
	}
	if persistentStorage != "" {
		res[core.ResourceStorage] = resource.MustParse(persistentStorage)
	}
	return res
}

func TestValidateEphemeralContainers(t *testing.T) {
	containers := []core.Container{{Name: "ctr", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}}
	initContainers := []core.Container{{Name: "ictr", Image: "iimage", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}}
	vols := map[string]core.VolumeSource{
		"blk": {PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "pvc"}},
		"vol": {EmptyDir: &core.EmptyDirVolumeSource{}},
	}

	// Success Cases
	for title, ephemeralContainers := range map[string][]core.EphemeralContainer{
		"Empty Ephemeral Containers": {},
		"Single Container": {
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		},
		"Multiple Containers": {
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug1", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug2", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		},
		"Single Container with Target": {{
			EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
			TargetContainerName:      "ctr",
		}},
		"All allowed fields": {{
			EphemeralContainerCommon: core.EphemeralContainerCommon{

				Name:       "debug",
				Image:      "image",
				Command:    []string{"bash"},
				Args:       []string{"bash"},
				WorkingDir: "/",
				EnvFrom: []core.EnvFromSource{{
					ConfigMapRef: &core.ConfigMapEnvSource{
						LocalObjectReference: core.LocalObjectReference{Name: "dummy"},
						Optional:             &[]bool{true}[0],
					},
				}},
				Env: []core.EnvVar{
					{Name: "TEST", Value: "TRUE"},
				},
				VolumeMounts: []core.VolumeMount{
					{Name: "vol", MountPath: "/vol"},
				},
				VolumeDevices: []core.VolumeDevice{
					{Name: "blk", DevicePath: "/dev/block"},
				},
				TerminationMessagePath:   "/dev/termination-log",
				TerminationMessagePolicy: "File",
				ImagePullPolicy:          "IfNotPresent",
				SecurityContext: &core.SecurityContext{
					Capabilities: &core.Capabilities{
						Add: []core.Capability{"SYS_ADMIN"},
					},
				},
				Stdin:     true,
				StdinOnce: true,
				TTY:       true,
			},
		}},
	} {
		var PodRestartPolicy core.RestartPolicy
		PodRestartPolicy = "Never"
		if errs := validateEphemeralContainers(ephemeralContainers, containers, initContainers, vols, nil, field.NewPath("ephemeralContainers"), PodValidationOptions{}, &PodRestartPolicy, noUserNamespace); len(errs) != 0 {
			t.Errorf("expected success for '%s' but got errors: %v", title, errs)
		}

		PodRestartPolicy = "Always"
		if errs := validateEphemeralContainers(ephemeralContainers, containers, initContainers, vols, nil, field.NewPath("ephemeralContainers"), PodValidationOptions{}, &PodRestartPolicy, noUserNamespace); len(errs) != 0 {
			t.Errorf("expected success for '%s' but got errors: %v", title, errs)
		}

		PodRestartPolicy = "OnFailure"
		if errs := validateEphemeralContainers(ephemeralContainers, containers, initContainers, vols, nil, field.NewPath("ephemeralContainers"), PodValidationOptions{}, &PodRestartPolicy, noUserNamespace); len(errs) != 0 {
			t.Errorf("expected success for '%s' but got errors: %v", title, errs)
		}
	}

	// Failure Cases
	tcs := []struct {
		title, line         string
		ephemeralContainers []core.EphemeralContainer
		expectedErrors      field.ErrorList
	}{{
		"Name Collision with Container.Containers",
		line(),
		[]core.EphemeralContainer{
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "ctr", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug1", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		},
		field.ErrorList{{Type: field.ErrorTypeDuplicate, Field: "ephemeralContainers[0].name"}},
	}, {
		"Name Collision with Container.InitContainers",
		line(),
		[]core.EphemeralContainer{
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "ictr", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug1", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		},
		field.ErrorList{{Type: field.ErrorTypeDuplicate, Field: "ephemeralContainers[0].name"}},
	}, {
		"Name Collision with EphemeralContainers",
		line(),
		[]core.EphemeralContainer{
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug1", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug1", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		},
		field.ErrorList{{Type: field.ErrorTypeDuplicate, Field: "ephemeralContainers[1].name"}},
	}, {
		"empty Container",
		line(),
		[]core.EphemeralContainer{
			{EphemeralContainerCommon: core.EphemeralContainerCommon{}},
		},
		field.ErrorList{
			{Type: field.ErrorTypeRequired, Field: "ephemeralContainers[0].name"},
			{Type: field.ErrorTypeRequired, Field: "ephemeralContainers[0].image"},
			{Type: field.ErrorTypeRequired, Field: "ephemeralContainers[0].terminationMessagePolicy"},
			{Type: field.ErrorTypeRequired, Field: "ephemeralContainers[0].imagePullPolicy"},
		},
	}, {
		"empty Container Name",
		line(),
		[]core.EphemeralContainer{
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "ephemeralContainers[0].name"}},
	}, {
		"whitespace padded image name",
		line(),
		[]core.EphemeralContainer{
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug", Image: " image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "ephemeralContainers[0].image"}},
	}, {
		"invalid image pull policy",
		line(),
		[]core.EphemeralContainer{
			{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug", Image: "image", ImagePullPolicy: "PullThreeTimes", TerminationMessagePolicy: "File"}},
		},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "ephemeralContainers[0].imagePullPolicy"}},
	}, {
		"TargetContainerName doesn't exist",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
			TargetContainerName:      "bogus",
		}},
		field.ErrorList{{Type: field.ErrorTypeNotFound, Field: "ephemeralContainers[0].targetContainerName"}},
	}, {
		"Targets an ephemeral container",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debugception", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
			TargetContainerName:      "debug",
		}},
		field.ErrorList{{Type: field.ErrorTypeNotFound, Field: "ephemeralContainers[1].targetContainerName"}},
	}, {
		"Container uses disallowed field: Lifecycle",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debug",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				Lifecycle: &core.Lifecycle{
					PreStop: &core.LifecycleHandler{
						Exec: &core.ExecAction{Command: []string{"ls", "-l"}},
					},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].lifecycle"}},
	}, {
		"Container uses disallowed field: LivenessProbe",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debug",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				LivenessProbe: &core.Probe{
					ProbeHandler: core.ProbeHandler{
						TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(80)},
					},
					SuccessThreshold: 1,
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].livenessProbe"}},
	}, {
		"Container uses disallowed field: Ports",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debug",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				Ports: []core.ContainerPort{
					{Protocol: "TCP", ContainerPort: 80},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].ports"}},
	}, {
		"Container uses disallowed field: ReadinessProbe",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debug",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				ReadinessProbe: &core.Probe{
					ProbeHandler: core.ProbeHandler{
						TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(80)},
					},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].readinessProbe"}},
	}, {
		"Container uses disallowed field: StartupProbe",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debug",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				StartupProbe: &core.Probe{
					ProbeHandler: core.ProbeHandler{
						TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(80)},
					},
					SuccessThreshold: 1,
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].startupProbe"}},
	}, {
		"Container uses disallowed field: Resources",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debug",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceName(core.ResourceCPU): resource.MustParse("10"),
					},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].resources"}},
	}, {
		"Container uses disallowed field: VolumeMount.SubPath",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debug",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				VolumeMounts: []core.VolumeMount{
					{Name: "vol", MountPath: "/vol"},
					{Name: "vol", MountPath: "/volsub", SubPath: "foo"},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].volumeMounts[1].subPath"}},
	}, {
		"Container uses disallowed field: VolumeMount.SubPathExpr",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debug",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				VolumeMounts: []core.VolumeMount{
					{Name: "vol", MountPath: "/vol"},
					{Name: "vol", MountPath: "/volsub", SubPathExpr: "$(POD_NAME)"},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].volumeMounts[1].subPathExpr"}},
	}, {
		"Disallowed field with other errors should only return a single Forbidden",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debug",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				Lifecycle: &core.Lifecycle{
					PreStop: &core.LifecycleHandler{
						Exec: &core.ExecAction{Command: []string{}},
					},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].lifecycle"}},
	}, {
		"Container uses disallowed field: ResizePolicy",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "resources-resize-policy",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				ResizePolicy: []core.ContainerResizePolicy{
					{ResourceName: "cpu", RestartPolicy: "NotRequired"},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].resizePolicy"}},
	}, {
		"Forbidden RestartPolicy: Always",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "foo",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				RestartPolicy:            &containerRestartPolicyAlways,
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].restartPolicy"}},
	}, {
		"Forbidden RestartPolicy: OnFailure",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "foo",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				RestartPolicy:            &containerRestartPolicyOnFailure,
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].restartPolicy"}},
	}, {
		"Forbidden RestartPolicy: Never",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "foo",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				RestartPolicy:            &containerRestartPolicyNever,
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].restartPolicy"}},
	}, {
		"Forbidden RestartPolicy: invalid",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "foo",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				RestartPolicy:            &containerRestartPolicyInvalid,
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].restartPolicy"}},
	}, {
		"Forbidden RestartPolicy: empty",
		line(),
		[]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "foo",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				RestartPolicy:            &containerRestartPolicyEmpty,
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "ephemeralContainers[0].restartPolicy"}},
	},
	}

	var PodRestartPolicy core.RestartPolicy

	for _, tc := range tcs {
		t.Run(tc.title+"__@L"+tc.line, func(t *testing.T) {

			PodRestartPolicy = "Never"
			errs := validateEphemeralContainers(tc.ephemeralContainers, containers, initContainers, vols, nil, field.NewPath("ephemeralContainers"), PodValidationOptions{}, &PodRestartPolicy, noUserNamespace)
			if len(errs) == 0 {
				t.Fatal("expected error but received none")
			}

			PodRestartPolicy = "Always"
			errs = validateEphemeralContainers(tc.ephemeralContainers, containers, initContainers, vols, nil, field.NewPath("ephemeralContainers"), PodValidationOptions{}, &PodRestartPolicy, noUserNamespace)
			if len(errs) == 0 {
				t.Fatal("expected error but received none")
			}

			PodRestartPolicy = "OnFailure"
			errs = validateEphemeralContainers(tc.ephemeralContainers, containers, initContainers, vols, nil, field.NewPath("ephemeralContainers"), PodValidationOptions{}, &PodRestartPolicy, noUserNamespace)
			if len(errs) == 0 {
				t.Fatal("expected error but received none")
			}

			if diff := cmp.Diff(tc.expectedErrors, errs, cmpopts.IgnoreFields(field.Error{}, "BadValue", "Detail")); diff != "" {
				t.Errorf("unexpected diff in errors (-want, +got):\n%s", diff)
				t.Errorf("INFO: all errors:\n%s", prettyErrorList(errs))
			}
		})
	}
}

func TestValidateWindowsPodSecurityContext(t *testing.T) {
	validWindowsSC := &core.PodSecurityContext{WindowsOptions: &core.WindowsSecurityContextOptions{RunAsUserName: utilpointer.String("dummy")}}
	invalidWindowsSC := &core.PodSecurityContext{SELinuxOptions: &core.SELinuxOptions{Role: "dummyRole"}}
	cases := map[string]struct {
		podSec      *core.PodSpec
		expectErr   bool
		errorType   field.ErrorType
		errorDetail string
	}{
		"valid SC, windows, no error": {
			podSec:    &core.PodSpec{SecurityContext: validWindowsSC},
			expectErr: false,
		},
		"invalid SC, windows, error": {
			podSec:      &core.PodSpec{SecurityContext: invalidWindowsSC},
			errorType:   "FieldValueForbidden",
			errorDetail: "cannot be set for a windows pod",
			expectErr:   true,
		},
	}
	for k, v := range cases {
		t.Run(k, func(t *testing.T) {
			errs := validateWindows(v.podSec, field.NewPath("field"))
			if v.expectErr && len(errs) > 0 {
				if errs[0].Type != v.errorType || !strings.Contains(errs[0].Detail, v.errorDetail) {
					t.Errorf("[%s] Expected error type %q with detail %q, got %v", k, v.errorType, v.errorDetail, errs)
				}
			} else if v.expectErr && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if !v.expectErr && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

func TestValidateLinuxPodSecurityContext(t *testing.T) {
	runAsUser := int64(1)
	validLinuxSC := &core.PodSecurityContext{
		SELinuxOptions: &core.SELinuxOptions{
			User:  "user",
			Role:  "role",
			Type:  "type",
			Level: "level",
		},
		RunAsUser: &runAsUser,
	}
	invalidLinuxSC := &core.PodSecurityContext{
		WindowsOptions: &core.WindowsSecurityContextOptions{RunAsUserName: utilpointer.String("myUser")},
	}

	cases := map[string]struct {
		podSpec     *core.PodSpec
		expectErr   bool
		errorType   field.ErrorType
		errorDetail string
	}{
		"valid SC, linux, no error": {
			podSpec:   &core.PodSpec{SecurityContext: validLinuxSC},
			expectErr: false,
		},
		"invalid SC, linux, error": {
			podSpec:     &core.PodSpec{SecurityContext: invalidLinuxSC},
			errorType:   "FieldValueForbidden",
			errorDetail: "windows options cannot be set for a linux pod",
			expectErr:   true,
		},
	}
	for k, v := range cases {
		t.Run(k, func(t *testing.T) {
			errs := validateLinux(v.podSpec, field.NewPath("field"))
			if v.expectErr && len(errs) > 0 {
				if errs[0].Type != v.errorType || !strings.Contains(errs[0].Detail, v.errorDetail) {
					t.Errorf("[%s] Expected error type %q with detail %q, got %v", k, v.errorType, v.errorDetail, errs)
				}
			} else if v.expectErr && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if !v.expectErr && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

func TestValidateContainers(t *testing.T) {
	volumeDevices := make(map[string]core.VolumeSource)
	capabilities.ResetForTest()
	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: true,
	})

	successCase := []core.Container{
		{Name: "abc", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		// backwards compatibility to ensure containers in pod template spec do not check for this
		{Name: "def", Image: " ", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		{Name: "ghi", Image: " some  ", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		{Name: "123", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		{Name: "abc-123", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}, {
			Name:  "life-123",
			Image: "image",
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					Exec: &core.ExecAction{Command: []string{"ls", "-l"}},
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:  "resources-test",
			Image: "image",
			Resources: core.ResourceRequirements{
				Limits: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
					core.ResourceName("my.org/resource"):   resource.MustParse("10"),
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:  "resources-test-with-request-and-limit",
			Image: "image",
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
				},
				Limits: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:  "resources-request-limit-simple",
			Image: "image",
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceName(core.ResourceCPU): resource.MustParse("8"),
				},
				Limits: core.ResourceList{
					core.ResourceName(core.ResourceCPU): resource.MustParse("10"),
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:  "resources-request-limit-edge",
			Image: "image",
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
					core.ResourceName("my.org/resource"):   resource.MustParse("10"),
				},
				Limits: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
					core.ResourceName("my.org/resource"):   resource.MustParse("10"),
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:  "resources-request-limit-partials",
			Image: "image",
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("9.5"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
				},
				Limits: core.ResourceList{
					core.ResourceName(core.ResourceCPU):  resource.MustParse("10"),
					core.ResourceName("my.org/resource"): resource.MustParse("10"),
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:  "resources-request",
			Image: "image",
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("9.5"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:  "resources-resize-policy",
			Image: "image",
			ResizePolicy: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "NotRequired"},
				{ResourceName: "memory", RestartPolicy: "RestartContainer"},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:  "same-host-port-different-protocol",
			Image: "image",
			Ports: []core.ContainerPort{
				{ContainerPort: 80, HostPort: 80, Protocol: "TCP"},
				{ContainerPort: 80, HostPort: 80, Protocol: "UDP"},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:                     "fallback-to-logs-termination-message",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "FallbackToLogsOnError",
		}, {
			Name:                     "file-termination-message",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:                     "env-from-source",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			EnvFrom: []core.EnvFromSource{{
				ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{
						Name: "test",
					},
				},
			}},
		},
		{Name: "abc-1234", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File", SecurityContext: fakeValidSecurityContext(true)}, {
			Name:  "live-123",
			Image: "image",
			LivenessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				SuccessThreshold: 1,
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:  "startup-123",
			Image: "image",
			StartupProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				SuccessThreshold: 1,
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:                     "resize-policy-cpu",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			ResizePolicy: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "NotRequired"},
			},
		}, {
			Name:                     "resize-policy-mem",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			ResizePolicy: []core.ContainerResizePolicy{
				{ResourceName: "memory", RestartPolicy: "RestartContainer"},
			},
		}, {
			Name:                     "resize-policy-cpu-and-mem",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			ResizePolicy: []core.ContainerResizePolicy{
				{ResourceName: "memory", RestartPolicy: "NotRequired"},
				{ResourceName: "cpu", RestartPolicy: "RestartContainer"},
			},
		},
	}

	var PodRestartPolicy core.RestartPolicy = "Always"
	if errs := validateContainers(successCase, volumeDevices, nil, defaultGracePeriod, field.NewPath("field"), PodValidationOptions{}, &PodRestartPolicy, noUserNamespace); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	capabilities.ResetForTest()
	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: false,
	})
	errorCases := []struct {
		title, line    string
		containers     []core.Container
		expectedErrors field.ErrorList
	}{{
		"zero-length name",
		line(),
		[]core.Container{{Name: "", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "containers[0].name"}},
	}, {
		"zero-length-image",
		line(),
		[]core.Container{{Name: "abc", Image: "", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "containers[0].image"}},
	}, {
		"name > 63 characters",
		line(),
		[]core.Container{{Name: strings.Repeat("a", 64), Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].name"}},
	}, {
		"name not a DNS label",
		line(),
		[]core.Container{{Name: "a.b.c", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].name"}},
	}, {
		"name not unique",
		line(),
		[]core.Container{
			{Name: "abc", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
			{Name: "abc", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		},
		field.ErrorList{{Type: field.ErrorTypeDuplicate, Field: "containers[1].name"}},
	}, {
		"zero-length image",
		line(),
		[]core.Container{{Name: "abc", Image: "", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "containers[0].image"}},
	}, {
		"host port not unique",
		line(),
		[]core.Container{
			{Name: "abc", Image: "image", Ports: []core.ContainerPort{{ContainerPort: 80, HostPort: 80, Protocol: "TCP"}},
				ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
			{Name: "def", Image: "image", Ports: []core.ContainerPort{{ContainerPort: 81, HostPort: 80, Protocol: "TCP"}},
				ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		},
		field.ErrorList{{Type: field.ErrorTypeDuplicate, Field: "containers[1].ports[0].hostPort"}},
	}, {
		"invalid env var name",
		line(),
		[]core.Container{
			{Name: "abc", Image: "image", Env: []core.EnvVar{{Name: "ev!1"}}, ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].env[0].name"}},
	}, {
		"unknown volume name",
		line(),
		[]core.Container{
			{Name: "abc", Image: "image", VolumeMounts: []core.VolumeMount{{Name: "anything", MountPath: "/foo"}},
				ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		},
		field.ErrorList{{Type: field.ErrorTypeNotFound, Field: "containers[0].volumeMounts[0].name"}},
	}, {
		"invalid lifecycle, no exec command.",
		line(),
		[]core.Container{{
			Name:  "life-123",
			Image: "image",
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					Exec: &core.ExecAction{},
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "containers[0].lifecycle.preStop.exec.command"}},
	}, {
		"invalid lifecycle, no http path.",
		line(),
		[]core.Container{{
			Name:  "life-123",
			Image: "image",
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					HTTPGet: &core.HTTPGetAction{
						Port:   intstr.FromInt32(80),
						Scheme: "HTTP",
					},
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "containers[0].lifecycle.preStop.httpGet.path"}},
	}, {
		"invalid lifecycle, no http port.",
		line(),
		[]core.Container{{
			Name:  "life-123",
			Image: "image",
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					HTTPGet: &core.HTTPGetAction{
						Path:   "/",
						Scheme: "HTTP",
					},
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].lifecycle.preStop.httpGet.port"}},
	}, {
		"invalid lifecycle, no http scheme.",
		line(),
		[]core.Container{{
			Name:  "life-123",
			Image: "image",
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					HTTPGet: &core.HTTPGetAction{
						Path: "/",
						Port: intstr.FromInt32(80),
					},
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "containers[0].lifecycle.preStop.httpGet.scheme"}},
	}, {
		"invalid lifecycle, no tcp socket port.",
		line(),
		[]core.Container{{
			Name:  "life-123",
			Image: "image",
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					TCPSocket: &core.TCPSocketAction{},
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].lifecycle.preStop.tcpSocket.port"}},
	}, {
		"invalid lifecycle, zero tcp socket port.",
		line(),
		[]core.Container{{
			Name:  "life-123",
			Image: "image",
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(0),
					},
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].lifecycle.preStop.tcpSocket.port"}},
	}, {
		"invalid lifecycle, no action.",
		line(),
		[]core.Container{{
			Name:  "life-123",
			Image: "image",
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "containers[0].lifecycle.preStop"}},
	}, {
		"invalid readiness probe, terminationGracePeriodSeconds set.",
		line(),
		[]core.Container{{
			Name:  "life-123",
			Image: "image",
			ReadinessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				TerminationGracePeriodSeconds: utilpointer.Int64(10),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].readinessProbe.terminationGracePeriodSeconds"}},
	}, {
		"invalid liveness probe, no tcp socket port.",
		line(),
		[]core.Container{{
			Name:  "live-123",
			Image: "image",
			LivenessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{},
				},
				SuccessThreshold: 1,
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].livenessProbe.tcpSocket.port"}},
	}, {
		"invalid liveness probe, no action.",
		line(),
		[]core.Container{{
			Name:  "live-123",
			Image: "image",
			LivenessProbe: &core.Probe{
				ProbeHandler:     core.ProbeHandler{},
				SuccessThreshold: 1,
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "containers[0].livenessProbe"}},
	}, {
		"invalid liveness probe, successThreshold != 1",
		line(),
		[]core.Container{{
			Name:  "live-123",
			Image: "image",
			LivenessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				SuccessThreshold: 2,
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].livenessProbe.successThreshold"}},
	}, {
		"invalid startup probe, successThreshold != 1",
		line(),
		[]core.Container{{
			Name:  "startup-123",
			Image: "image",
			StartupProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				SuccessThreshold: 2,
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].startupProbe.successThreshold"}},
	}, {
		"invalid liveness probe, negative numbers",
		line(),
		[]core.Container{{
			Name:  "live-123",
			Image: "image",
			LivenessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				InitialDelaySeconds:           -1,
				TimeoutSeconds:                -1,
				PeriodSeconds:                 -1,
				SuccessThreshold:              -1,
				FailureThreshold:              -1,
				TerminationGracePeriodSeconds: utilpointer.Int64(-1),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{
			{Type: field.ErrorTypeInvalid, Field: "containers[0].livenessProbe.initialDelaySeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].livenessProbe.timeoutSeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].livenessProbe.periodSeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].livenessProbe.successThreshold"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].livenessProbe.failureThreshold"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].livenessProbe.terminationGracePeriodSeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].livenessProbe.successThreshold"},
		},
	}, {
		"invalid readiness probe, negative numbers",
		line(),
		[]core.Container{{
			Name:  "ready-123",
			Image: "image",
			ReadinessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				InitialDelaySeconds:           -1,
				TimeoutSeconds:                -1,
				PeriodSeconds:                 -1,
				SuccessThreshold:              -1,
				FailureThreshold:              -1,
				TerminationGracePeriodSeconds: utilpointer.Int64(-1),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{
			{Type: field.ErrorTypeInvalid, Field: "containers[0].readinessProbe.initialDelaySeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].readinessProbe.timeoutSeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].readinessProbe.periodSeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].readinessProbe.successThreshold"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].readinessProbe.failureThreshold"},
			// terminationGracePeriodSeconds returns multiple validation errors here:
			// containers[0].readinessProbe.terminationGracePeriodSeconds: Invalid value: -1: must be greater than 0
			{Type: field.ErrorTypeInvalid, Field: "containers[0].readinessProbe.terminationGracePeriodSeconds"},
			// containers[0].readinessProbe.terminationGracePeriodSeconds: Invalid value: -1: must not be set for readinessProbes
			{Type: field.ErrorTypeInvalid, Field: "containers[0].readinessProbe.terminationGracePeriodSeconds"},
		},
	}, {
		"invalid startup probe, negative numbers",
		line(),
		[]core.Container{{
			Name:  "startup-123",
			Image: "image",
			StartupProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				InitialDelaySeconds:           -1,
				TimeoutSeconds:                -1,
				PeriodSeconds:                 -1,
				SuccessThreshold:              -1,
				FailureThreshold:              -1,
				TerminationGracePeriodSeconds: utilpointer.Int64(-1),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{
			{Type: field.ErrorTypeInvalid, Field: "containers[0].startupProbe.initialDelaySeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].startupProbe.timeoutSeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].startupProbe.periodSeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].startupProbe.successThreshold"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].startupProbe.failureThreshold"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].startupProbe.terminationGracePeriodSeconds"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].startupProbe.successThreshold"},
		},
	}, {
		"invalid message termination policy",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "Unknown",
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "containers[0].terminationMessagePolicy"}},
	}, {
		"empty message termination policy",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "",
		}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "containers[0].terminationMessagePolicy"}},
	}, {
		"privilege disabled",
		line(),
		[]core.Container{{
			Name:                     "abc",
			Image:                    "image",
			SecurityContext:          fakeValidSecurityContext(true),
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "containers[0].securityContext.privileged"}},
	}, {
		"invalid compute resource",
		line(),
		[]core.Container{{
			Name:  "abc-123",
			Image: "image",
			Resources: core.ResourceRequirements{
				Limits: core.ResourceList{
					"disk": resource.MustParse("10G"),
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{
			{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.limits[disk]"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.limits[disk]"},
		},
	}, {
		"Resource CPU invalid",
		line(),
		[]core.Container{{
			Name:  "abc-123",
			Image: "image",
			Resources: core.ResourceRequirements{
				Limits: getResources("-10", "0", "", ""),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.limits[cpu]"}},
	}, {
		"Resource Requests CPU invalid",
		line(),
		[]core.Container{{
			Name:  "abc-123",
			Image: "image",
			Resources: core.ResourceRequirements{
				Requests: getResources("-10", "0", "", ""),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.requests[cpu]"}},
	}, {
		"Resource Memory invalid",
		line(),
		[]core.Container{{
			Name:  "abc-123",
			Image: "image",
			Resources: core.ResourceRequirements{
				Limits: getResources("0", "-10", "", ""),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.limits[memory]"}},
	}, {
		"Request limit simple invalid",
		line(),
		[]core.Container{{
			Name:  "abc-123",
			Image: "image",
			Resources: core.ResourceRequirements{
				Limits:   getResources("5", "3", "", ""),
				Requests: getResources("6", "3", "", ""),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.requests"}},
	}, {
		"Invalid storage limit request",
		line(),
		[]core.Container{{
			Name:  "abc-123",
			Image: "image",
			Resources: core.ResourceRequirements{
				Limits: core.ResourceList{
					core.ResourceName("attachable-volumes-aws-ebs"): *resource.NewQuantity(10, resource.DecimalSI),
				},
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{
			{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.limits[attachable-volumes-aws-ebs]"},
			{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.limits[attachable-volumes-aws-ebs]"},
		},
	}, {
		"CPU request limit multiple invalid",
		line(),
		[]core.Container{{
			Name:  "abc-123",
			Image: "image",
			Resources: core.ResourceRequirements{
				Limits:   getResources("5", "3", "", ""),
				Requests: getResources("6", "3", "", ""),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.requests"}},
	}, {
		"Memory request limit multiple invalid",
		line(),
		[]core.Container{{
			Name:  "abc-123",
			Image: "image",
			Resources: core.ResourceRequirements{
				Limits:   getResources("5", "3", "", ""),
				Requests: getResources("5", "4", "", ""),
			},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].resources.requests"}},
	}, {
		"Invalid env from",
		line(),
		[]core.Container{{
			Name:                     "env-from-source",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			EnvFrom: []core.EnvFromSource{{
				ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{
						Name: "$%^&*#",
					},
				},
			}},
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "containers[0].envFrom[0].configMapRef.name"}},
	}, {
		"Unsupported resize policy for memory",
		line(),
		[]core.Container{{
			Name:                     "resize-policy-mem-invalid",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			ResizePolicy: []core.ContainerResizePolicy{
				{ResourceName: "memory", RestartPolicy: "RestartContainerrrr"},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "containers[0].resizePolicy"}},
	}, {
		"Unsupported resize policy for CPU",
		line(),
		[]core.Container{{
			Name:                     "resize-policy-cpu-invalid",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			ResizePolicy: []core.ContainerResizePolicy{
				{ResourceName: "cpu", RestartPolicy: "RestartNotRequired"},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "containers[0].resizePolicy"}},
	}, {
		"Forbidden RestartPolicy: Always",
		line(),
		[]core.Container{{
			Name:                     "foo",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "containers[0].restartPolicy"}},
	}, {
		"Forbidden RestartPolicy: OnFailure",
		line(),
		[]core.Container{{
			Name:                     "foo",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyOnFailure,
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "containers[0].restartPolicy"}},
	}, {
		"Forbidden RestartPolicy: Never",
		line(),
		[]core.Container{{
			Name:                     "foo",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyNever,
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "containers[0].restartPolicy"}},
	}, {
		"Forbidden RestartPolicy: invalid",
		line(),
		[]core.Container{{
			Name:                     "foo",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyInvalid,
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "containers[0].restartPolicy"}},
	}, {
		"Forbidden RestartPolicy: empty",
		line(),
		[]core.Container{{
			Name:                     "foo",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyEmpty,
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "containers[0].restartPolicy"}},
	},
	}

	for _, tc := range errorCases {
		t.Run(tc.title+"__@L"+tc.line, func(t *testing.T) {
			errs := validateContainers(tc.containers, volumeDevices, nil, defaultGracePeriod, field.NewPath("containers"), PodValidationOptions{}, &PodRestartPolicy, noUserNamespace)
			if len(errs) == 0 {
				t.Fatal("expected error but received none")
			}

			if diff := cmp.Diff(tc.expectedErrors, errs, cmpopts.IgnoreFields(field.Error{}, "BadValue", "Detail")); diff != "" {
				t.Errorf("unexpected diff in errors (-want, +got):\n%s", diff)
				t.Errorf("INFO: all errors:\n%s", prettyErrorList(errs))
			}
		})
	}
}

func TestValidateInitContainers(t *testing.T) {
	volumeDevices := make(map[string]core.VolumeSource)
	capabilities.ResetForTest()
	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: true,
	})

	containers := []core.Container{{
		Name:                     "app",
		Image:                    "nginx",
		ImagePullPolicy:          "IfNotPresent",
		TerminationMessagePolicy: "File",
	},
	}

	successCase := []core.Container{{
		Name:  "container-1-same-host-port-different-protocol",
		Image: "image",
		Ports: []core.ContainerPort{
			{ContainerPort: 80, HostPort: 80, Protocol: "TCP"},
			{ContainerPort: 80, HostPort: 80, Protocol: "UDP"},
		},
		ImagePullPolicy:          "IfNotPresent",
		TerminationMessagePolicy: "File",
	}, {
		Name:  "container-2-same-host-port-different-protocol",
		Image: "image",
		Ports: []core.ContainerPort{
			{ContainerPort: 80, HostPort: 80, Protocol: "TCP"},
			{ContainerPort: 80, HostPort: 80, Protocol: "UDP"},
		},
		ImagePullPolicy:          "IfNotPresent",
		TerminationMessagePolicy: "File",
	}, {
		Name:                     "container-3-restart-always-with-lifecycle-hook-and-probes",
		Image:                    "image",
		ImagePullPolicy:          "IfNotPresent",
		TerminationMessagePolicy: "File",
		RestartPolicy:            &containerRestartPolicyAlways,
		Lifecycle: &core.Lifecycle{
			PostStart: &core.LifecycleHandler{
				Exec: &core.ExecAction{
					Command: []string{"echo", "post start"},
				},
			},
			PreStop: &core.LifecycleHandler{
				Exec: &core.ExecAction{
					Command: []string{"echo", "pre stop"},
				},
			},
		},
		LivenessProbe: &core.Probe{
			ProbeHandler: core.ProbeHandler{
				TCPSocket: &core.TCPSocketAction{
					Port: intstr.FromInt32(80),
				},
			},
			SuccessThreshold: 1,
		},
		ReadinessProbe: &core.Probe{
			ProbeHandler: core.ProbeHandler{
				TCPSocket: &core.TCPSocketAction{
					Port: intstr.FromInt32(80),
				},
			},
		},
		StartupProbe: &core.Probe{
			ProbeHandler: core.ProbeHandler{
				TCPSocket: &core.TCPSocketAction{
					Port: intstr.FromInt32(80),
				},
			},
			SuccessThreshold: 1,
		},
	}, {
		Name:                     "container-4-allowed-resize-policy",
		Image:                    "image",
		ImagePullPolicy:          "IfNotPresent",
		TerminationMessagePolicy: "File",
		ResizePolicy: []core.ContainerResizePolicy{
			{ResourceName: "cpu", RestartPolicy: "NotRequired"},
		},
	},
	}
	var PodRestartPolicy core.RestartPolicy = "Never"
	if errs := validateInitContainers(successCase, containers, volumeDevices, nil, defaultGracePeriod, field.NewPath("field"), PodValidationOptions{AllowSidecarResizePolicy: true}, &PodRestartPolicy, noUserNamespace); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	capabilities.ResetForTest()
	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: false,
	})
	errorCases := []struct {
		title, line    string
		initContainers []core.Container
		expectedErrors field.ErrorList
	}{{
		"empty name",
		line(),
		[]core.Container{{
			Name:                     "",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "initContainers[0].name", BadValue: ""}},
	}, {
		"name collision with regular container",
		line(),
		[]core.Container{{
			Name:                     "app",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeDuplicate, Field: "initContainers[0].name", BadValue: "app"}},
	}, {
		"invalid termination message policy",
		line(),
		[]core.Container{{
			Name:                     "init",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "Unknown",
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "initContainers[0].terminationMessagePolicy", BadValue: core.TerminationMessagePolicy("Unknown")}},
	}, {
		"duplicate names",
		line(),
		[]core.Container{{
			Name:                     "init",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}, {
			Name:                     "init",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeDuplicate, Field: "initContainers[1].name", BadValue: "init"}},
	}, {
		"duplicate ports",
		line(),
		[]core.Container{{
			Name:  "abc",
			Image: "image",
			Ports: []core.ContainerPort{{
				ContainerPort: 8080, HostPort: 8080, Protocol: "TCP",
			}, {
				ContainerPort: 8080, HostPort: 8080, Protocol: "TCP",
			}},
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
		}},
		field.ErrorList{{Type: field.ErrorTypeDuplicate, Field: "initContainers[0].ports[1].hostPort", BadValue: "TCP//8080"}},
	}, {
		"uses disallowed field: Lifecycle",
		line(),
		[]core.Container{{
			Name:                     "debug",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					Exec: &core.ExecAction{Command: []string{"ls", "-l"}},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "initContainers[0].lifecycle", BadValue: ""}},
	}, {
		"uses disallowed field: LivenessProbe",
		line(),
		[]core.Container{{
			Name:                     "debug",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			LivenessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(80)},
				},
				SuccessThreshold: 1,
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "initContainers[0].livenessProbe", BadValue: ""}},
	}, {
		"uses disallowed field: ReadinessProbe",
		line(),
		[]core.Container{{
			Name:                     "debug",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			ReadinessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(80)},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "initContainers[0].readinessProbe", BadValue: ""}},
	}, {
		"Container uses disallowed field: StartupProbe",
		line(),
		[]core.Container{{
			Name:                     "debug",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			StartupProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(80)},
				},
				SuccessThreshold: 1,
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "initContainers[0].startupProbe", BadValue: ""}},
	}, {
		"Disallowed field with other errors should only return a single Forbidden",
		line(),
		[]core.Container{{
			Name:                     "debug",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			StartupProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(80)},
				},
				InitialDelaySeconds:           -1,
				TimeoutSeconds:                -1,
				PeriodSeconds:                 -1,
				SuccessThreshold:              -1,
				FailureThreshold:              -1,
				TerminationGracePeriodSeconds: utilpointer.Int64(-1),
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeForbidden, Field: "initContainers[0].startupProbe", BadValue: ""}},
	}, {
		"Not supported RestartPolicy: OnFailure",
		line(),
		[]core.Container{{
			Name:                     "init",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyOnFailure,
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "initContainers[0].restartPolicy", BadValue: containerRestartPolicyOnFailure}},
	}, {
		"Not supported RestartPolicy: Never",
		line(),
		[]core.Container{{
			Name:                     "init",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyNever,
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "initContainers[0].restartPolicy", BadValue: containerRestartPolicyNever}},
	}, {
		"Not supported RestartPolicy: invalid",
		line(),
		[]core.Container{{
			Name:                     "init",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyInvalid,
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "initContainers[0].restartPolicy", BadValue: containerRestartPolicyInvalid}},
	}, {
		"Not supported RestartPolicy: empty",
		line(),
		[]core.Container{{
			Name:                     "init",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyEmpty,
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "initContainers[0].restartPolicy", BadValue: containerRestartPolicyEmpty}},
	}, {
		"invalid startup probe in restartable container, successThreshold != 1",
		line(),
		[]core.Container{{
			Name:                     "restartable-init",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			StartupProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(80)},
				},
				SuccessThreshold: 2,
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "initContainers[0].startupProbe.successThreshold", BadValue: int32(2)}},
	}, {
		"invalid readiness probe, terminationGracePeriodSeconds set.",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			ReadinessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				TerminationGracePeriodSeconds: utilpointer.Int64(10),
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "initContainers[0].readinessProbe.terminationGracePeriodSeconds", BadValue: utilpointer.Int64(10)}},
	}, {
		"invalid liveness probe, successThreshold != 1",
		line(),
		[]core.Container{{
			Name:                     "live-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			LivenessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(80),
					},
				},
				SuccessThreshold: 2,
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "initContainers[0].livenessProbe.successThreshold", BadValue: int32(2)}},
	}, {
		"invalid lifecycle, no exec command.",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					Exec: &core.ExecAction{},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "initContainers[0].lifecycle.preStop.exec.command", BadValue: ""}},
	}, {
		"invalid lifecycle, no http path.",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					HTTPGet: &core.HTTPGetAction{
						Port:   intstr.FromInt32(80),
						Scheme: "HTTP",
					},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "initContainers[0].lifecycle.preStop.httpGet.path", BadValue: ""}},
	}, {
		"invalid lifecycle, no http port.",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					HTTPGet: &core.HTTPGetAction{
						Path:   "/",
						Scheme: "HTTP",
					},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "initContainers[0].lifecycle.preStop.httpGet.port", BadValue: 0}},
	}, {
		"invalid lifecycle, no http scheme.",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					HTTPGet: &core.HTTPGetAction{
						Path: "/",
						Port: intstr.FromInt32(80),
					},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeNotSupported, Field: "initContainers[0].lifecycle.preStop.httpGet.scheme", BadValue: core.URIScheme("")}},
	}, {
		"invalid lifecycle, no tcp socket port.",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					TCPSocket: &core.TCPSocketAction{},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "initContainers[0].lifecycle.preStop.tcpSocket.port", BadValue: 0}},
	}, {
		"invalid lifecycle, zero tcp socket port.",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{
					TCPSocket: &core.TCPSocketAction{
						Port: intstr.FromInt32(0),
					},
				},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "initContainers[0].lifecycle.preStop.tcpSocket.port", BadValue: 0}},
	}, {
		"invalid lifecycle, no action.",
		line(),
		[]core.Container{{
			Name:                     "life-123",
			Image:                    "image",
			ImagePullPolicy:          "IfNotPresent",
			TerminationMessagePolicy: "File",
			RestartPolicy:            &containerRestartPolicyAlways,
			Lifecycle: &core.Lifecycle{
				PreStop: &core.LifecycleHandler{},
			},
		}},
		field.ErrorList{{Type: field.ErrorTypeRequired, Field: "initContainers[0].lifecycle.preStop", BadValue: ""}},
	},
		{
			"Not supported ResizePolicy: invalid",
			line(),
			[]core.Container{{
				Name:                     "init",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				ResizePolicy: []core.ContainerResizePolicy{
					{ResourceName: "cpu", RestartPolicy: "NotRequired"},
				},
			}},
			field.ErrorList{{Type: field.ErrorTypeInvalid, Field: "initContainers[0].resizePolicy", BadValue: []core.ContainerResizePolicy{{ResourceName: "cpu", RestartPolicy: "NotRequired"}}}},
		},
	}

	for _, tc := range errorCases {
		t.Run(tc.title+"__@L"+tc.line, func(t *testing.T) {
			errs := validateInitContainers(tc.initContainers, containers, volumeDevices, nil, defaultGracePeriod, field.NewPath("initContainers"), PodValidationOptions{}, &PodRestartPolicy, noUserNamespace)
			if len(errs) == 0 {
				t.Fatal("expected error but received none")
			}

			if diff := cmp.Diff(tc.expectedErrors, errs, cmpopts.IgnoreFields(field.Error{}, "Detail")); diff != "" {
				t.Errorf("unexpected diff in errors (-want, +got):\n%s", diff)
				t.Errorf("INFO: all errors:\n%s", prettyErrorList(errs))
			}
		})
	}
}

func TestValidateRestartPolicy(t *testing.T) {
	successCases := []core.RestartPolicy{
		core.RestartPolicyAlways,
		core.RestartPolicyOnFailure,
		core.RestartPolicyNever,
	}
	for _, policy := range successCases {
		if errs := validateRestartPolicy(&policy, field.NewPath("field")); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := []core.RestartPolicy{"", "newpolicy"}

	for k, policy := range errorCases {
		if errs := validateRestartPolicy(&policy, field.NewPath("field")); len(errs) == 0 {
			t.Errorf("expected failure for %d", k)
		}
	}
}

func TestValidateDNSPolicy(t *testing.T) {
	successCases := []core.DNSPolicy{core.DNSClusterFirst, core.DNSDefault, core.DNSClusterFirstWithHostNet, core.DNSNone}
	for _, policy := range successCases {
		if errs := validateDNSPolicy(&policy, field.NewPath("field")); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := []core.DNSPolicy{core.DNSPolicy("invalid"), core.DNSPolicy("")}
	for _, policy := range errorCases {
		if errs := validateDNSPolicy(&policy, field.NewPath("field")); len(errs) == 0 {
			t.Errorf("expected failure for %v", policy)
		}
	}
}

func TestValidatePodDNSConfig(t *testing.T) {
	generateTestSearchPathFunc := func(numChars int) string {
		res := ""
		for i := 0; i < numChars; i++ {
			res = res + "a"
		}
		return res
	}
	testOptionValue := "2"
	testDNSNone := core.DNSNone
	testDNSClusterFirst := core.DNSClusterFirst

	testCases := []struct {
		desc          string
		dnsConfig     *core.PodDNSConfig
		dnsPolicy     *core.DNSPolicy
		opts          PodValidationOptions
		expectedError bool
	}{{
		desc:          "valid: empty DNSConfig",
		dnsConfig:     &core.PodDNSConfig{},
		expectedError: false,
	}, {
		desc: "valid: 1 option",
		dnsConfig: &core.PodDNSConfig{
			Options: []core.PodDNSConfigOption{
				{Name: "ndots", Value: &testOptionValue},
			},
		},
		expectedError: false,
	}, {
		desc: "valid: 1 nameserver",
		dnsConfig: &core.PodDNSConfig{
			Nameservers: []string{"127.0.0.1"},
		},
		expectedError: false,
	}, {
		desc: "valid: DNSNone with 1 nameserver",
		dnsConfig: &core.PodDNSConfig{
			Nameservers: []string{"127.0.0.1"},
		},
		dnsPolicy:     &testDNSNone,
		expectedError: false,
	}, {
		desc: "valid: 1 search path",
		dnsConfig: &core.PodDNSConfig{
			Searches: []string{"custom"},
		},
		expectedError: false,
	}, {
		desc: "valid: 1 search path with trailing period",
		dnsConfig: &core.PodDNSConfig{
			Searches: []string{"custom."},
		},
		expectedError: false,
	}, {
		desc: "valid: 3 nameservers and 6 search paths(legacy)",
		dnsConfig: &core.PodDNSConfig{
			Nameservers: []string{"127.0.0.1", "10.0.0.10", "8.8.8.8"},
			Searches:    []string{"custom", "mydomain.com", "local", "cluster.local", "svc.cluster.local", "default.svc.cluster.local."},
		},
		expectedError: false,
	}, {
		desc: "valid: 3 nameservers and 32 search paths",
		dnsConfig: &core.PodDNSConfig{
			Nameservers: []string{"127.0.0.1", "10.0.0.10", "8.8.8.8"},
			Searches:    []string{"custom", "mydomain.com", "local", "cluster.local", "svc.cluster.local", "default.svc.cluster.local.", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20", "21", "22", "23", "24", "25", "26", "27", "28", "29", "30", "31", "32"},
		},
		expectedError: false,
	}, {
		desc: "valid: 256 characters in search path list(legacy)",
		dnsConfig: &core.PodDNSConfig{
			// We can have 256 - (6 - 1) = 251 characters in total for 6 search paths.
			Searches: []string{
				generateTestSearchPathFunc(1),
				generateTestSearchPathFunc(50),
				generateTestSearchPathFunc(50),
				generateTestSearchPathFunc(50),
				generateTestSearchPathFunc(50),
				generateTestSearchPathFunc(50),
			},
		},
		expectedError: false,
	}, {
		desc: "valid: 2048 characters in search path list",
		dnsConfig: &core.PodDNSConfig{
			// We can have 2048 - (32 - 1) = 2017 characters in total for 32 search paths.
			Searches: []string{
				generateTestSearchPathFunc(64),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
			},
		},
		expectedError: false,
	}, {
		desc: "valid: ipv6 nameserver",
		dnsConfig: &core.PodDNSConfig{
			Nameservers: []string{"FE80::0202:B3FF:FE1E:8329"},
		},
		expectedError: false,
	}, {
		desc: "invalid: 4 nameservers",
		dnsConfig: &core.PodDNSConfig{
			Nameservers: []string{"127.0.0.1", "10.0.0.10", "8.8.8.8", "1.2.3.4"},
		},
		expectedError: true,
	}, {
		desc: "valid: 7 search paths",
		dnsConfig: &core.PodDNSConfig{
			Searches: []string{"custom", "mydomain.com", "local", "cluster.local", "svc.cluster.local", "default.svc.cluster.local", "exceeded"},
		},
	}, {
		desc: "invalid: 33 search paths",
		dnsConfig: &core.PodDNSConfig{
			Searches: []string{"custom", "mydomain.com", "local", "cluster.local", "svc.cluster.local", "default.svc.cluster.local.", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20", "21", "22", "23", "24", "25", "26", "27", "28", "29", "30", "31", "32", "33"},
		},
		expectedError: true,
	}, {
		desc: "valid: 257 characters in search path list",
		dnsConfig: &core.PodDNSConfig{
			// We can have 256 - (6 - 1) = 251 characters in total for 6 search paths.
			Searches: []string{
				generateTestSearchPathFunc(2),
				generateTestSearchPathFunc(50),
				generateTestSearchPathFunc(50),
				generateTestSearchPathFunc(50),
				generateTestSearchPathFunc(50),
				generateTestSearchPathFunc(50),
			},
		},
	}, {
		desc: "invalid: 2049 characters in search path list",
		dnsConfig: &core.PodDNSConfig{
			// We can have 2048 - (32 - 1) = 2017 characters in total for 32 search paths.
			Searches: []string{
				generateTestSearchPathFunc(65),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
				generateTestSearchPathFunc(63),
			},
		},
		expectedError: true,
	}, {
		desc: "invalid search path",
		dnsConfig: &core.PodDNSConfig{
			Searches: []string{"custom?"},
		},
		expectedError: true,
	}, {
		desc: "invalid nameserver",
		dnsConfig: &core.PodDNSConfig{
			Nameservers: []string{"invalid"},
		},
		expectedError: true,
	}, {
		desc: "invalid empty option name",
		dnsConfig: &core.PodDNSConfig{
			Options: []core.PodDNSConfigOption{
				{Value: &testOptionValue},
			},
		},
		expectedError: true,
	}, {
		desc: "invalid: DNSNone with 0 nameserver",
		dnsConfig: &core.PodDNSConfig{
			Searches: []string{"custom"},
		},
		dnsPolicy:     &testDNSNone,
		expectedError: true,
	},
	}

	for _, tc := range testCases {
		if tc.dnsPolicy == nil {
			tc.dnsPolicy = &testDNSClusterFirst
		}

		errs := validatePodDNSConfig(tc.dnsConfig, tc.dnsPolicy, field.NewPath("dnsConfig"), tc.opts)
		if len(errs) != 0 && !tc.expectedError {
			t.Errorf("%v: validatePodDNSConfig(%v) = %v, want nil", tc.desc, tc.dnsConfig, errs)
		} else if len(errs) == 0 && tc.expectedError {
			t.Errorf("%v: validatePodDNSConfig(%v) = nil, want error", tc.desc, tc.dnsConfig)
		}
	}
}

func TestValidatePodReadinessGates(t *testing.T) {
	successCases := []struct {
		desc           string
		readinessGates []core.PodReadinessGate
	}{{
		"no gate",
		[]core.PodReadinessGate{},
	}, {
		"one readiness gate",
		[]core.PodReadinessGate{{
			ConditionType: core.PodConditionType("example.com/condition"),
		}},
	}, {
		"two readiness gates",
		[]core.PodReadinessGate{{
			ConditionType: core.PodConditionType("example.com/condition1"),
		}, {
			ConditionType: core.PodConditionType("example.com/condition2"),
		}},
	},
	}
	for _, tc := range successCases {
		if errs := validateReadinessGates(tc.readinessGates, field.NewPath("field")); len(errs) != 0 {
			t.Errorf("expect tc %q to success: %v", tc.desc, errs)
		}
	}

	errorCases := []struct {
		desc           string
		readinessGates []core.PodReadinessGate
	}{{
		"invalid condition type",
		[]core.PodReadinessGate{{
			ConditionType: core.PodConditionType("invalid/condition/type"),
		}},
	},
	}
	for _, tc := range errorCases {
		if errs := validateReadinessGates(tc.readinessGates, field.NewPath("field")); len(errs) == 0 {
			t.Errorf("expected tc %q to fail", tc.desc)
		}
	}
}

func TestValidatePodConditions(t *testing.T) {
	successCases := []struct {
		desc          string
		podConditions []core.PodCondition
	}{{
		"no condition",
		[]core.PodCondition{},
	}, {
		"one system condition",
		[]core.PodCondition{{
			Type:   core.PodReady,
			Status: core.ConditionTrue,
		}},
	}, {
		"one system condition and one custom condition",
		[]core.PodCondition{{
			Type:   core.PodReady,
			Status: core.ConditionTrue,
		}, {
			Type:   core.PodConditionType("example.com/condition"),
			Status: core.ConditionFalse,
		}},
	}, {
		"two custom condition",
		[]core.PodCondition{{
			Type:   core.PodConditionType("foobar"),
			Status: core.ConditionTrue,
		}, {
			Type:   core.PodConditionType("example.com/condition"),
			Status: core.ConditionFalse,
		}},
	},
	}

	for _, tc := range successCases {
		if errs := validatePodConditions(tc.podConditions, field.NewPath("field")); len(errs) != 0 {
			t.Errorf("expected tc %q to success, but got: %v", tc.desc, errs)
		}
	}

	errorCases := []struct {
		desc          string
		podConditions []core.PodCondition
	}{{
		"one system condition and a invalid custom condition",
		[]core.PodCondition{{
			Type:   core.PodReady,
			Status: core.ConditionStatus("True"),
		}, {
			Type:   core.PodConditionType("invalid/custom/condition"),
			Status: core.ConditionStatus("True"),
		}},
	},
	}
	for _, tc := range errorCases {
		if errs := validatePodConditions(tc.podConditions, field.NewPath("field")); len(errs) == 0 {
			t.Errorf("expected tc %q to fail", tc.desc)
		}
	}
}

func TestValidatePodSpec(t *testing.T) {
	activeDeadlineSecondsMax := int64(math.MaxInt32)

	minUserID := int64(0)
	maxUserID := int64(2147483647)
	minGroupID := int64(0)
	maxGroupID := int64(2147483647)
	goodfsGroupChangePolicy := core.FSGroupChangeAlways
	badfsGroupChangePolicy1 := core.PodFSGroupChangePolicy("invalid")
	badfsGroupChangePolicy2 := core.PodFSGroupChangePolicy("")

	successCases := map[string]*core.Pod{
		"populate basic fields, leave defaults for most": podtest.MakePod(""),
		"populate all fields": podtest.MakePod("",
			podtest.SetInitContainers(podtest.MakeContainer("ictr")),
			podtest.SetVolumes(podtest.MakeEmptyVolume(("vol"))),
			podtest.SetNodeSelector(map[string]string{
				"key": "value",
			}),
			podtest.SetNodeName("foobar"),
			podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsMax),
			podtest.SetServiceAccountName("acct"),
		),
		"populate HostNetwork": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerPorts(core.ContainerPort{HostPort: 8080, ContainerPort: 8080, Protocol: "TCP"}))),
			podtest.SetSecurityContext(&core.PodSecurityContext{HostNetwork: true}),
		),
		"populate RunAsUser SupplementalGroups FSGroup with minID 0": podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				SupplementalGroups: []int64{minGroupID},
				RunAsUser:          &minUserID,
				FSGroup:            &minGroupID,
			}),
		),
		"populate RunAsUser SupplementalGroups FSGroup with maxID 2147483647": podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				SupplementalGroups: []int64{maxGroupID},
				RunAsUser:          &maxUserID,
				FSGroup:            &maxGroupID,
			}),
		),
		"populate HostIPC": podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				HostIPC: true,
			}),
		),
		"populate HostPID": podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				HostPID: true,
			}),
		),
		"populate Affinity": podtest.MakePod("",
			podtest.SetAffinity(&core.Affinity{
				NodeAffinity: &core.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
						NodeSelectorTerms: []core.NodeSelectorTerm{{
							MatchExpressions: []core.NodeSelectorRequirement{{
								Key:      "key2",
								Operator: core.NodeSelectorOpIn,
								Values:   []string{"value1", "value2"},
							}},
							MatchFields: []core.NodeSelectorRequirement{{
								Key:      "metadata.name",
								Operator: core.NodeSelectorOpIn,
								Values:   []string{"host1"},
							}},
						}},
					},
					PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
						Weight: 10,
						Preference: core.NodeSelectorTerm{
							MatchExpressions: []core.NodeSelectorRequirement{{
								Key:      "foo",
								Operator: core.NodeSelectorOpIn,
								Values:   []string{"bar"},
							}},
						},
					}},
				},
			}),
		),
		"populate HostAliases": podtest.MakePod("",
			podtest.SetHostAliases(core.HostAlias{IP: "12.34.56.78", Hostnames: []string{"host1", "host2"}}),
		),
		"populate HostAliases with `foo.bar` hostnames": podtest.MakePod("",
			podtest.SetHostAliases(core.HostAlias{IP: "12.34.56.78", Hostnames: []string{"host1.foo", "host2.bar"}}),
		),
		"populate HostAliases with HostNetwork": podtest.MakePod("",
			podtest.SetHostAliases(core.HostAlias{IP: "12.34.56.78", Hostnames: []string{"host1.foo", "host2.bar"}}),
			podtest.SetSecurityContext(&core.PodSecurityContext{
				HostNetwork: true,
			}),
		),
		"populate PriorityClassName": podtest.MakePod("",
			podtest.SetPriorityClassName("valid-name"),
		),
		"populate ShareProcessNamespace": podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				ShareProcessNamespace: &[]bool{true}[0],
			}),
		),
		"populate RuntimeClassName": podtest.MakePod("",
			podtest.SetRuntimeClassName("valid-sandbox"),
		),
		"populate Overhead": podtest.MakePod("",
			podtest.SetOverhead(core.ResourceList{}),
		),
		"populate FSGroupChangePolicy": podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				FSGroupChangePolicy: &goodfsGroupChangePolicy,
			}),
		),
		"resources resize policy for containers": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResizePolicy(
				core.ContainerResizePolicy{ResourceName: "cpu", RestartPolicy: "NotRequired"}),
			)),
		),
	}
	for k, v := range successCases {
		t.Run(k, func(t *testing.T) {
			opts := PodValidationOptions{
				ResourceIsPod: true,
			}
			if errs := ValidatePodSpec(&v.Spec, nil, field.NewPath("field"), opts); len(errs) != 0 {
				t.Errorf("expected success: %v", errs)
			}
		})
	}

	activeDeadlineSecondsZero := int64(0)
	activeDeadlineSecondsTooLarge := int64(math.MaxInt32 + 1)

	minUserID = int64(-1)
	maxUserID = int64(2147483648)
	minGroupID = int64(-1)
	maxGroupID = int64(2147483648)

	failureCases := map[string]core.Pod{
		"bad volume":               *podtest.MakePod("", podtest.SetVolumes(core.Volume{})),
		"no containers":            *podtest.MakePod("", podtest.SetContainers()),
		"bad container":            *podtest.MakePod("", podtest.SetContainers(core.Container{})),
		"bad init container":       *podtest.MakePod("", podtest.SetInitContainers(core.Container{})),
		"bad DNS policy":           *podtest.MakePod("", podtest.SetDNSPolicy(core.DNSPolicy("invalid"))),
		"bad service account name": *podtest.MakePod("", podtest.SetServiceAccountName("invalidName")),
		"bad restart policy":       *podtest.MakePod("", podtest.SetRestartPolicy("UnknowPolicy")),
		"with hostNetwork hostPort unspecified": *podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerPorts(core.ContainerPort{HostPort: 0, ContainerPort: 2600, Protocol: "TCP"}))),
			podtest.SetSecurityContext(&core.PodSecurityContext{
				HostNetwork: true,
			}),
		),
		"with hostNetwork hostPort not equal to containerPort": *podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerPorts(core.ContainerPort{HostPort: 8080, ContainerPort: 2600, Protocol: "TCP"}))),
			podtest.SetSecurityContext(&core.PodSecurityContext{
				HostNetwork: true,
			}),
		),
		"with hostAliases with invalid IP": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				HostNetwork: false,
			}),
			podtest.SetHostAliases(core.HostAlias{IP: "999.999.999.999", Hostnames: []string{"host1", "host2"}}),
		),
		"with hostAliases with invalid hostname": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				HostNetwork: false,
			}),
			podtest.SetHostAliases(core.HostAlias{IP: "12.34.56.78", Hostnames: []string{"@#$^#@#$"}}),
		),
		"bad supplementalGroups large than math.MaxInt32": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				SupplementalGroups: []int64{maxGroupID, 1234},
			}),
		),
		"bad supplementalGroups less than 0": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				SupplementalGroups: []int64{minGroupID, 1234},
			}),
		),
		"bad runAsUser large than math.MaxInt32": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				RunAsUser: &maxUserID,
			}),
		),
		"bad runAsUser less than 0": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				RunAsUser: &minUserID,
			}),
		),
		"bad fsGroup large than math.MaxInt32": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				FSGroup: &maxGroupID,
			}),
		),
		"bad fsGroup less than 0": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				FSGroup: &minGroupID,
			}),
		),
		"bad-active-deadline-seconds": *podtest.MakePod("",
			podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsZero),
		),
		"active-deadline-seconds-too-large": *podtest.MakePod("",
			podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsTooLarge),
		),
		"bad nodeName": *podtest.MakePod("",
			podtest.SetNodeName("node name"),
		),
		"bad PriorityClassName": *podtest.MakePod("",
			podtest.SetPriorityClassName("InvalidName"),
		),
		"ShareProcessNamespace and HostPID both set": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				HostPID:               true,
				ShareProcessNamespace: &[]bool{true}[0],
			}),
		),
		"bad RuntimeClassName": *podtest.MakePod("",
			podtest.SetRuntimeClassName("invalid/sandbox"),
		),
		"bad empty fsGroupchangepolicy": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				FSGroupChangePolicy: &badfsGroupChangePolicy2,
			}),
		),
		"bad invalid fsgroupchangepolicy": *podtest.MakePod("",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				FSGroupChangePolicy: &badfsGroupChangePolicy1,
			}),
		),
	}
	for k, v := range failureCases {
		opts := PodValidationOptions{
			ResourceIsPod: true,
		}
		if errs := ValidatePodSpec(&v.Spec, nil, field.NewPath("field"), opts); len(errs) == 0 {
			t.Errorf("expected failure for %q", k)
		}
	}
}

func TestValidatePod(t *testing.T) {
	validPVCSpec := core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}
	validPVCTemplate := core.PersistentVolumeClaimTemplate{
		Spec: validPVCSpec,
	}
	longPodName := strings.Repeat("a", 200)
	longVolName := strings.Repeat("b", 60)

	successCases := map[string]core.Pod{
		"basic fields": *podtest.MakePod("123"),
		"just about everything": *podtest.MakePod("abc.123.do-re-mi",
			podtest.SetInitContainers(podtest.MakeContainer("ictr")),
			podtest.SetVolumes(podtest.MakeEmptyVolume(("vol"))),
			podtest.SetNodeSelector(map[string]string{
				"key": "value",
			}),
			podtest.SetNodeName("foobar"),
			podtest.SetServiceAccountName("acct"),
		),
		"serialized node affinity requirements": *podtest.MakePod("123",
			podtest.SetAffinity(
				// TODO: Uncomment and move this block and move inside NodeAffinity once
				// RequiredDuringSchedulingRequiredDuringExecution is implemented
				//		RequiredDuringSchedulingRequiredDuringExecution: &core.NodeSelector{
				//			NodeSelectorTerms: []core.NodeSelectorTerm{
				//				{
				//					MatchExpressions: []core.NodeSelectorRequirement{
				//						{
				//							Key: "key1",
				//							Operator: core.NodeSelectorOpExists
				//						},
				//					},
				//				},
				//			},
				//		},
				&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "key2",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"value1", "value2"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"host1"},
								}},
							}},
						},
						PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
							Weight: 10,
							Preference: core.NodeSelectorTerm{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "foo",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"bar"},
								}},
							},
						}},
					},
				}),
		),
		"serialized node affinity requirements, II": *podtest.MakePod("123",
			podtest.SetAffinity(
				// TODO: Uncomment and move this block and move inside NodeAffinity once
				// RequiredDuringSchedulingRequiredDuringExecution is implemented
				//		RequiredDuringSchedulingRequiredDuringExecution: &core.NodeSelector{
				//			NodeSelectorTerms: []core.NodeSelectorTerm{
				//				{
				//					MatchExpressions: []core.NodeSelectorRequirement{
				//						{
				//							Key: "key1",
				//							Operator: core.NodeSelectorOpExists
				//						},
				//					},
				//				},
				//			},
				//		},
				&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{},
							}},
						},
						PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
							Weight: 10,
							Preference: core.NodeSelectorTerm{
								MatchExpressions: []core.NodeSelectorRequirement{},
							},
						}},
					},
				},
			),
		),
		"serialized pod affinity in affinity requirements in annotations": *podtest.MakePod("123",
			podtest.SetAffinity(
				// TODO: Uncomment and move this block into Annotations map once
				// RequiredDuringSchedulingRequiredDuringExecution is implemented
				//		"requiredDuringSchedulingRequiredDuringExecution": [{
				//			"labelSelector": {
				//				"matchExpressions": [{
				//					"key": "key2",
				//					"operator": "In",
				//					"values": ["value1", "value2"]
				//				}]
				//			},
				//			"namespaces":["ns"],
				//			"topologyKey": "zone"
				//		}]
				&core.Affinity{
					PodAffinity: &core.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "key2",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"value1", "value2"},
								}},
							},
							TopologyKey: "zone",
							Namespaces:  []string{"ns"},
							NamespaceSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "key",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"value1", "value2"},
								}},
							},
						}},
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1", "value2"},
									}},
								},
								Namespaces:  []string{"ns"},
								TopologyKey: "region",
							},
						}},
					},
				}),
		),
		"serialized pod anti affinity with different Label Operators in affinity requirements in annotations": *podtest.MakePod("123",
			podtest.SetAffinity(
				// TODO: Uncomment and move this block into Annotations map once
				// RequiredDuringSchedulingRequiredDuringExecution is implemented
				//		"requiredDuringSchedulingRequiredDuringExecution": [{
				//			"labelSelector": {
				//				"matchExpressions": [{
				//					"key": "key2",
				//					"operator": "In",
				//					"values": ["value1", "value2"]
				//				}]
				//			},
				//			"namespaces":["ns"],
				//			"topologyKey": "zone"
				//		}]
				&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "key2",
									Operator: metav1.LabelSelectorOpExists,
								}},
							},
							TopologyKey: "zone",
							Namespaces:  []string{"ns"},
						}},
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpDoesNotExist,
									}},
								},
								Namespaces:  []string{"ns"},
								TopologyKey: "region",
							},
						}},
					},
				}),
		),
		"populate forgiveness tolerations with exists operator in annotations.": *podtest.MakePod("123",
			podtest.SetTolerations(core.Toleration{Key: "foo", Operator: "Exists", Value: "", Effect: "NoExecute", TolerationSeconds: &[]int64{60}[0]}),
		),
		"populate forgiveness tolerations with equal operator in annotations.": *podtest.MakePod("123",
			podtest.SetTolerations(core.Toleration{Key: "foo", Operator: "Equal", Value: "bar", Effect: "NoExecute", TolerationSeconds: &[]int64{60}[0]}),
		),
		"populate tolerations equal operator in annotations.": *podtest.MakePod("123",
			podtest.SetTolerations(core.Toleration{Key: "foo", Operator: "Equal", Value: "bar", Effect: "NoSchedule"}),
		),
		"populate tolerations exists operator in annotations.": *podtest.MakePod("123",
			podtest.SetVolumes(podtest.MakeEmptyVolume("vol")),
		),
		"empty key with Exists operator is OK for toleration, empty toleration key means match all taint keys.": *podtest.MakePod("123",
			podtest.SetTolerations(core.Toleration{Operator: "Exists", Effect: "NoSchedule"}),
		),
		"empty operator is OK for toleration, defaults to Equal.": *podtest.MakePod("123",
			podtest.SetTolerations(core.Toleration{Key: "foo", Value: "bar", Effect: "NoSchedule"}),
		),
		"empty effect is OK for toleration, empty toleration effect means match all taint effects.": *podtest.MakePod("123",
			podtest.SetTolerations(core.Toleration{Key: "foo", Operator: "Equal", Value: "bar"}),
		),
		"negative tolerationSeconds is OK for toleration.": *podtest.MakePod("123",
			podtest.SetTolerations(
				core.Toleration{Key: "node.kubernetes.io/not-ready", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &[]int64{-2}[0]}),
		),
		"runtime default seccomp profile": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{
				core.SeccompPodAnnotationKey: core.SeccompProfileRuntimeDefault,
			},
			),
		),
		"docker default seccomp profile": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{
				core.SeccompPodAnnotationKey: "unconfined",
			},
			),
		),
		"localhost seccomp profile": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{
				core.SeccompPodAnnotationKey: "localhost/foo",
			},
			),
		),
		"localhost seccomp profile for a container": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{
				core.SeccompContainerAnnotationKeyPrefix + "foo": "localhost/foo",
			},
			),
		),
		"runtime default seccomp profile for a pod": *podtest.MakePod("123",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				SeccompProfile: &core.SeccompProfile{
					Type: core.SeccompProfileTypeRuntimeDefault,
				},
			},
			),
		),
		"runtime default seccomp profile for a container": *podtest.MakePod("123",
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerSecurityContext(core.SecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type: core.SeccompProfileTypeUnconfined,
					},
				}),
			)),
		),
		"unconfined seccomp profile for a pod": *podtest.MakePod("123",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				SeccompProfile: &core.SeccompProfile{
					Type: core.SeccompProfileTypeRuntimeDefault,
				},
			}),
		),
		"unconfined seccomp profile for a container": *podtest.MakePod("123",
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerSecurityContext(core.SecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type: core.SeccompProfileTypeUnconfined,
					},
				}),
			)),
		),
		"localhost seccomp profile for a pod": *podtest.MakePod("123",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				SeccompProfile: &core.SeccompProfile{
					Type:             core.SeccompProfileTypeLocalhost,
					LocalhostProfile: utilpointer.String("filename.json"),
				},
			}),
		),
		"localhost seccomp profile for a container, II": *podtest.MakePod("123",
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerSecurityContext(core.SecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type:             core.SeccompProfileTypeLocalhost,
						LocalhostProfile: utilpointer.String("filename.json"),
					},
				}),
			)),
		),
		"default AppArmor annotation for a container": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{
				v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "ctr": v1.DeprecatedAppArmorBetaProfileRuntimeDefault,
			}),
		),
		"default AppArmor annotation for an init container": *podtest.MakePod("123",
			podtest.SetInitContainers(podtest.MakeContainer("init-ctr")),
			podtest.SetAnnotations(map[string]string{
				v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "init-ctr": v1.DeprecatedAppArmorBetaProfileRuntimeDefault,
			}),
		),
		"localhost AppArmor annotation for a container": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{
				v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "ctr": v1.DeprecatedAppArmorBetaProfileNamePrefix + "foo",
			}),
		),
		"runtime default AppArmor profile for a pod": *podtest.MakePod("123",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				AppArmorProfile: &core.AppArmorProfile{
					Type: core.AppArmorProfileTypeRuntimeDefault,
				},
			}),
		),
		"runtime default AppArmor profile for a container": *podtest.MakePod("123",
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerSecurityContext(core.SecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type: core.AppArmorProfileTypeRuntimeDefault,
					},
				}),
			)),
		),
		"unconfined AppArmor profile for a pod": *podtest.MakePod("123",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				AppArmorProfile: &core.AppArmorProfile{
					Type: core.AppArmorProfileTypeUnconfined,
				},
			}),
		),
		"unconfined AppArmor profile for a container": *podtest.MakePod("123",
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerSecurityContext(core.SecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type: core.AppArmorProfileTypeUnconfined,
					},
				}),
			)),
		),
		"localhost AppArmor profile for a pod": *podtest.MakePod("123",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				AppArmorProfile: &core.AppArmorProfile{
					Type:             core.AppArmorProfileTypeLocalhost,
					LocalhostProfile: ptr.To("example-org/application-foo"),
				},
			}),
		),
		"localhost AppArmor profile for a container field": *podtest.MakePod("123",
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerSecurityContext(core.SecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type:             core.AppArmorProfileTypeLocalhost,
						LocalhostProfile: ptr.To("example-org/application-foo"),
					},
				}),
			)),
		),
		"matching AppArmor fields and annotations": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{
				core.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": core.DeprecatedAppArmorAnnotationValueLocalhostPrefix + "foo",
			}),
			podtest.SetContainers(podtest.MakeContainer("ctr",
				podtest.SetContainerSecurityContext(core.SecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type:             core.AppArmorProfileTypeLocalhost,
						LocalhostProfile: ptr.To("foo"),
					},
				}),
			)),
		),
		"matching AppArmor pod field and annotations": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{
				core.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": core.DeprecatedAppArmorAnnotationValueLocalhostPrefix + "foo",
			}),
			podtest.SetSecurityContext(&core.PodSecurityContext{
				AppArmorProfile: &core.AppArmorProfile{
					Type:             core.AppArmorProfileTypeLocalhost,
					LocalhostProfile: ptr.To("foo"),
				},
			}),
		),
		"syntactically valid sysctls": *podtest.MakePod("123",
			podtest.SetSecurityContext(&core.PodSecurityContext{
				Sysctls: []core.Sysctl{{
					Name:  "kernel.shmmni",
					Value: "32768",
				}, {
					Name:  "kernel.shmmax",
					Value: "1000000000",
				}, {
					Name:  "knet.ipv4.route.min_pmtu",
					Value: "1000",
				}},
			}),
		),
		"valid extended resources for init container": *podtest.MakePod("valid-extended",
			podtest.SetInitContainers(podtest.MakeContainer("valid-extended",
				podtest.SetContainerResources(
					podtest.MakeResourceRequirements(
						map[string]string{
							"example.com/a": "10",
						},
						map[string]string{
							"example.com/a": "10",
						},
					))),
			),
		),
		"valid extended resources for regular container": *podtest.MakePod("valid-extended",
			podtest.SetContainers(podtest.MakeContainer("valid-extended",
				podtest.SetContainerResources(
					podtest.MakeResourceRequirements(
						map[string]string{
							"example.com/a": "10",
						},
						map[string]string{
							"example.com/a": "10",
						},
					))),
			),
		),
		"valid serviceaccount token projected volume with serviceaccount name specified": *podtest.MakePod("valid-extended",
			podtest.SetServiceAccountName("some-service-account"),
			podtest.SetVolumes(core.Volume{
				Name: "projected-volume",
				VolumeSource: core.VolumeSource{
					Projected: &core.ProjectedVolumeSource{
						Sources: []core.VolumeProjection{{
							ServiceAccountToken: &core.ServiceAccountTokenProjection{
								Audience:          "foo-audience",
								ExpirationSeconds: 6000,
								Path:              "foo-path",
							},
						}},
					},
				},
			}),
		),
		"valid ClusterTrustBundlePEM projected volume referring to a CTB by name": *podtest.MakePod("valid-extended",
			podtest.SetVolumes(core.Volume{
				Name: "projected-volume",
				VolumeSource: core.VolumeSource{
					Projected: &core.ProjectedVolumeSource{
						Sources: []core.VolumeProjection{
							{
								ClusterTrustBundle: &core.ClusterTrustBundleProjection{
									Path: "foo-path",
									Name: utilpointer.String("foo"),
								},
							},
						},
					},
				},
			}),
		),
		"valid ClusterTrustBundlePEM projected volume referring to a CTB by signer name": *podtest.MakePod("valid-extended",
			podtest.SetVolumes(core.Volume{
				Name: "projected-volume",
				VolumeSource: core.VolumeSource{
					Projected: &core.ProjectedVolumeSource{
						Sources: []core.VolumeProjection{
							{
								ClusterTrustBundle: &core.ClusterTrustBundleProjection{
									Path:       "foo-path",
									SignerName: utilpointer.String("example.com/foo"),
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"version": "live",
										},
									},
								},
							},
						},
					},
				},
			}),
		),
		"ephemeral volume + PVC, no conflict between them": *podtest.MakePod("valid-extended",
			podtest.SetVolumes(
				core.Volume{Name: "pvc", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"}}},
				core.Volume{Name: "ephemeral", VolumeSource: core.VolumeSource{Ephemeral: &core.EphemeralVolumeSource{VolumeClaimTemplate: &validPVCTemplate}}},
			),
		),
		"negative pod-deletion-cost": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{core.PodDeletionCost: "-100"}),
		),
		"positive pod-deletion-cost": *podtest.MakePod("123",
			podtest.SetAnnotations(map[string]string{core.PodDeletionCost: "100"}),
		),
		"MatchLabelKeys/MismatchLabelKeys in required PodAffinity": *podtest.MakePod("123",
			podtest.SetAffinity(&core.Affinity{
				PodAffinity: &core.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "key",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1", "value2"},
									},
									{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpIn,
										Values:   []string{"value1"},
									},
									{
										Key:      "key3",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1"},
									},
								},
							},
							TopologyKey:       "k8s.io/zone",
							MatchLabelKeys:    []string{"key2"},
							MismatchLabelKeys: []string{"key3"},
						},
					},
				},
			}),
		),
		"MatchLabelKeys/MismatchLabelKeys in preferred PodAffinity": *podtest.MakePod("123",
			podtest.SetAffinity(&core.Affinity{
				PodAffinity: &core.PodAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
						{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1", "value2"},
										},
										{
											Key:      "key2",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{"value1"},
										},
										{
											Key:      "key3",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1"},
										},
									},
								},
								TopologyKey:       "k8s.io/zone",
								MatchLabelKeys:    []string{"key2"},
								MismatchLabelKeys: []string{"key3"},
							},
						},
					},
				},
			}),
		),
		"MatchLabelKeys/MismatchLabelKeys in required PodAntiAffinity": *podtest.MakePod("123",
			podtest.SetAffinity(&core.Affinity{
				PodAntiAffinity: &core.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "key",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1", "value2"},
									},
									{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpIn,
										Values:   []string{"value1"},
									},
									{
										Key:      "key3",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1"},
									},
								},
							},
							TopologyKey:       "k8s.io/zone",
							MatchLabelKeys:    []string{"key2"},
							MismatchLabelKeys: []string{"key3"},
						},
					},
				},
			}),
		),
		"MatchLabelKeys/MismatchLabelKeys in preferred PodAntiAffinity": *podtest.MakePod("123",
			podtest.SetAffinity(&core.Affinity{
				PodAntiAffinity: &core.PodAntiAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
						{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1", "value2"},
										},
										{
											Key:      "key2",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{"value1"},
										},
										{
											Key:      "key3",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1"},
										},
									},
								},
								TopologyKey:       "k8s.io/zone",
								MatchLabelKeys:    []string{"key2"},
								MismatchLabelKeys: []string{"key3"},
							},
						},
					},
				},
			}),
		),
		"LabelSelector can have the same key as MismatchLabelKeys": *podtest.MakePod("123",
			podtest.SetAffinity(&core.Affinity{
				// Note: On the contrary, in case of matchLabelKeys, keys in matchLabelKeys are not allowed to be specified in labelSelector by users.
				PodAffinity: &core.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "key",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1", "value2"},
									},
									{
										// This is the same key as in MismatchLabelKeys
										// but it's allowed.
										Key:      "key2",
										Operator: metav1.LabelSelectorOpIn,
										Values:   []string{"value1"},
									},
									{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1"},
									},
								},
							},
							TopologyKey:       "k8s.io/zone",
							MismatchLabelKeys: []string{"key2"},
						},
					},
				},
			}),
		),
	}

	for k, v := range successCases {
		t.Run(k, func(t *testing.T) {
			if errs := ValidatePodCreate(&v, PodValidationOptions{}); len(errs) != 0 {
				t.Errorf("expected success: %v", errs)
			}
		})
	}

	errorCases := map[string]struct {
		spec          core.Pod
		expectedError string
	}{
		"bad name": {
			expectedError: "metadata.name",
			spec:          *podtest.MakePod(""),
		},
		"image whitespace": {
			expectedError: "spec.containers[0].image",
			spec: *podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerImage(" "))),
			),
		},
		"image leading and trailing whitespace": {
			expectedError: "spec.containers[0].image",
			spec: *podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerImage(" something "))),
			),
		},
		"bad namespace": {
			expectedError: "metadata.namespace",
			spec:          *podtest.MakePod("123", podtest.SetNamespace("")),
		},
		"bad spec": {
			expectedError: "spec.containers[0].name",
			spec:          *podtest.MakePod("123", podtest.SetContainers(core.Container{})),
		},
		"bad label": {
			expectedError: "NoUppercaseOrSpecialCharsLike=Equals",
			spec: *podtest.MakePod("123",
				podtest.SetLabels(map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				}),
			),
		},
		"invalid node selector requirement in node affinity, operator can't be null": {
			expectedError: "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key: "key1",
								}},
							}},
						}},
				}),
			),
		},
		"invalid node selector requirement in node affinity, key is invalid": {
			expectedError: "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "invalid key ___@#",
									Operator: core.NodeSelectorOpExists,
								}},
							}},
						},
					},
				}),
			),
		},
		"invalid node field selector requirement in node affinity, more values for field selector": {
			expectedError: "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchFields[0].values",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"host1", "host2"},
								}},
							}},
						}},
				}),
			),
		},
		"invalid node field selector requirement in node affinity, invalid operator": {
			expectedError: "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchFields[0].operator",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpExists,
								}},
							}},
						},
					},
				}),
			),
		},
		"invalid node field selector requirement in node affinity, invalid key": {
			expectedError: "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchFields[0].key",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.namespace",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"ns1"},
								}},
							}},
						},
					},
				}),
			),
		},
		"invalid preferredSchedulingTerm in node affinity, weight should be in range 1-100": {
			expectedError: "must be in the range 1-100",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
							Weight: 199,
							Preference: core.NodeSelectorTerm{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "foo",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"bar"},
								}},
							},
						}},
					},
				}),
			),
		},
		"invalid requiredDuringSchedulingIgnoredDuringExecution node selector, nodeSelectorTerms must have at least one term": {
			expectedError: "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{},
						},
					},
				}),
			),
		},
		"invalid weight in preferredDuringSchedulingIgnoredDuringExecution in pod affinity annotations, weight should be in range 1-100": {
			expectedError: "must be in the range 1-100",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{{
							Weight: 109,
							PodAffinityTerm: core.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1", "value2"},
									}},
								},
								Namespaces:  []string{"ns"},
								TopologyKey: "region",
							},
						}},
					},
				}),
			),
		},
		"invalid labelSelector in preferredDuringSchedulingIgnoredDuringExecution in podaffinity annotations, values should be empty if the operator is Exists": {
			expectedError: "spec.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].podAffinityTerm.labelSelector.matchExpressions[0].values",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpExists,
										Values:   []string{"value1", "value2"},
									}},
								},
								Namespaces:  []string{"ns"},
								TopologyKey: "region",
							},
						}},
					},
				}),
			),
		},
		"invalid namespaceSelector in preferredDuringSchedulingIgnoredDuringExecution in podaffinity, In operator must include Values": {
			expectedError: "spec.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].podAffinityTerm.namespaceSelector.matchExpressions[0].values",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								NamespaceSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpIn,
									}},
								},
								Namespaces:  []string{"ns"},
								TopologyKey: "region",
							},
						}},
					},
				}),
			),
		},
		"invalid namespaceSelector in preferredDuringSchedulingIgnoredDuringExecution in podaffinity, Exists operator can not have values": {
			expectedError: "spec.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].podAffinityTerm.namespaceSelector.matchExpressions[0].values",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								NamespaceSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpExists,
										Values:   []string{"value1", "value2"},
									}},
								},
								Namespaces:  []string{"ns"},
								TopologyKey: "region",
							},
						}},
					},
				}),
			),
		},
		"invalid name space in preferredDuringSchedulingIgnoredDuringExecution in podaffinity annotations, namespace should be valid": {
			expectedError: "spec.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].podAffinityTerm.namespace",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpExists,
									}},
								},
								Namespaces:  []string{"INVALID_NAMESPACE"},
								TopologyKey: "region",
							},
						}},
					},
				}),
			),
		},
		"invalid hard pod affinity, empty topologyKey is not allowed for hard pod affinity": {
			expectedError: "can not be empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "key2",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"value1", "value2"},
								}},
							},
							Namespaces: []string{"ns"},
						}},
					},
				}),
			),
		},
		"invalid hard pod anti-affinity, empty topologyKey is not allowed for hard pod anti-affinity": {
			expectedError: "can not be empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "key2",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"value1", "value2"},
								}},
							},
							Namespaces: []string{"ns"},
						}},
					},
				}),
			),
		},
		"invalid soft pod affinity, empty topologyKey is not allowed for soft pod affinity": {
			expectedError: "can not be empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1", "value2"},
									}},
								},
								Namespaces: []string{"ns"},
							},
						}},
					},
				}),
			),
		},
		"invalid soft pod anti-affinity, empty topologyKey is not allowed for soft pod anti-affinity": {
			expectedError: "can not be empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{{
							Weight: 10,
							PodAffinityTerm: core.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{{
										Key:      "key2",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"value1", "value2"},
									}},
								},
								Namespaces: []string{"ns"},
							},
						}},
					},
				}),
			),
		},
		"invalid soft pod affinity, key in MatchLabelKeys isn't correctly defined": {
			expectedError: "prefix part must be non-empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
							{
								Weight: 10,
								PodAffinityTerm: core.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"value1", "value2"},
											},
										},
									},
									TopologyKey:    "k8s.io/zone",
									MatchLabelKeys: []string{"/simple"},
								},
							},
						},
					},
				}),
			),
		},
		"invalid hard pod affinity, key in MatchLabelKeys isn't correctly defined": {
			expectedError: "prefix part must be non-empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1", "value2"},
										},
									},
								},
								TopologyKey:    "k8s.io/zone",
								MatchLabelKeys: []string{"/simple"},
							},
						},
					},
				}),
			),
		},
		"invalid soft pod anti-affinity, key in MatchLabelKeys isn't correctly defined": {
			expectedError: "prefix part must be non-empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
							{
								Weight: 10,
								PodAffinityTerm: core.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"value1", "value2"},
											},
										},
									},
									TopologyKey:    "k8s.io/zone",
									MatchLabelKeys: []string{"/simple"},
								},
							},
						},
					},
				}),
			),
		},
		"invalid hard pod anti-affinity, key in MatchLabelKeys isn't correctly defined": {
			expectedError: "prefix part must be non-empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1", "value2"},
										},
									},
								},
								TopologyKey:    "k8s.io/zone",
								MatchLabelKeys: []string{"/simple"},
							},
						},
					},
				}),
			),
		},
		"invalid soft pod affinity, key in MismatchLabelKeys isn't correctly defined": {
			expectedError: "prefix part must be non-empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
							{
								Weight: 10,
								PodAffinityTerm: core.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"value1", "value2"},
											},
										},
									},
									TopologyKey:       "k8s.io/zone",
									MismatchLabelKeys: []string{"/simple"},
								},
							},
						},
					},
				}),
			),
		},
		"invalid hard pod affinity, key in MismatchLabelKeys isn't correctly defined": {
			expectedError: "prefix part must be non-empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1", "value2"},
										},
									},
								},
								TopologyKey:       "k8s.io/zone",
								MismatchLabelKeys: []string{"/simple"},
							},
						},
					},
				}),
			),
		},
		"invalid soft pod anti-affinity, key in MismatchLabelKeys isn't correctly defined": {
			expectedError: "prefix part must be non-empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
							{
								Weight: 10,
								PodAffinityTerm: core.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"value1", "value2"},
											},
										},
									},
									TopologyKey:       "k8s.io/zone",
									MismatchLabelKeys: []string{"/simple"},
								},
							},
						},
					},
				}),
			),
		},
		"invalid hard pod anti-affinity, key in MismatchLabelKeys isn't correctly defined": {
			expectedError: "prefix part must be non-empty",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1", "value2"},
										},
									},
								},
								TopologyKey:       "k8s.io/zone",
								MismatchLabelKeys: []string{"/simple"},
							},
						},
					},
				}),
			),
		},
		"invalid soft pod affinity, key exists in both matchLabelKeys and labelSelector": {
			expectedError: "exists in both matchLabelKeys and labelSelector",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
							{
								Weight: 10,
								PodAffinityTerm: core.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											// This one should be created from MatchLabelKeys.
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"value1"},
											},
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"value2"},
											},
										},
									},
									TopologyKey:    "k8s.io/zone",
									MatchLabelKeys: []string{"key"},
								},
							},
						},
					},
				}),
			),
		},
		"invalid hard pod affinity, key exists in both matchLabelKeys and labelSelector": {
			expectedError: "exists in both matchLabelKeys and labelSelector",
			spec: *podtest.MakePod("123",
				podtest.SetLabels(map[string]string{"key": "value1"}),
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										// This one should be created from MatchLabelKeys.
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{"value1"},
										},
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value2"},
										},
									},
								},
								TopologyKey:    "k8s.io/zone",
								MatchLabelKeys: []string{"key"},
							},
						},
					},
				}),
			),
		},
		"invalid soft pod anti-affinity, key exists in both matchLabelKeys and labelSelector": {
			expectedError: "exists in both matchLabelKeys and labelSelector",
			spec: *podtest.MakePod("123",
				podtest.SetLabels(map[string]string{"key": "value1"}),
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
							{
								Weight: 10,
								PodAffinityTerm: core.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											// This one should be created from MatchLabelKeys.
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"value1"},
											},
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"value2"},
											},
										},
									},
									TopologyKey:    "k8s.io/zone",
									MatchLabelKeys: []string{"key"},
								},
							},
						},
					},
				}),
			),
		},
		"invalid hard pod anti-affinity, key exists in both matchLabelKeys and labelSelector": {
			expectedError: "exists in both matchLabelKeys and labelSelector",
			spec: *podtest.MakePod("123",
				podtest.SetLabels(map[string]string{"key": "value1"}),
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										// This one should be created from MatchLabelKeys.
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{"value1"},
										},
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value2"},
										},
									},
								},
								TopologyKey:    "k8s.io/zone",
								MatchLabelKeys: []string{"key"},
							},
						},
					},
				}),
			),
		},
		"invalid soft pod affinity, key exists in both MatchLabelKeys and MismatchLabelKeys": {
			expectedError: "exists in both matchLabelKeys and mismatchLabelKeys",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
							{
								Weight: 10,
								PodAffinityTerm: core.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"value1", "value2"},
											},
										},
									},
									TopologyKey:       "k8s.io/zone",
									MatchLabelKeys:    []string{"samekey"},
									MismatchLabelKeys: []string{"samekey"},
								},
							},
						},
					},
				}),
			),
		},
		"invalid hard pod affinity, key exists in both MatchLabelKeys and MismatchLabelKeys": {
			expectedError: "exists in both matchLabelKeys and mismatchLabelKeys",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAffinity: &core.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1", "value2"},
										},
									},
								},
								TopologyKey:       "k8s.io/zone",
								MatchLabelKeys:    []string{"samekey"},
								MismatchLabelKeys: []string{"samekey"},
							},
						},
					},
				}),
			),
		},
		"invalid soft pod anti-affinity, key exists in both MatchLabelKeys and MismatchLabelKeys": {
			expectedError: "exists in both matchLabelKeys and mismatchLabelKeys",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
							{
								Weight: 10,
								PodAffinityTerm: core.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "key",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"value1", "value2"},
											},
										},
									},
									TopologyKey:       "k8s.io/zone",
									MatchLabelKeys:    []string{"samekey"},
									MismatchLabelKeys: []string{"samekey"},
								},
							},
						},
					},
				}),
			),
		},
		"invalid hard pod anti-affinity, key exists in both MatchLabelKeys and MismatchLabelKeys": {
			expectedError: "exists in both matchLabelKeys and mismatchLabelKeys",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					PodAntiAffinity: &core.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "key",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"value1", "value2"},
										},
									},
								},
								TopologyKey:       "k8s.io/zone",
								MatchLabelKeys:    []string{"samekey"},
								MismatchLabelKeys: []string{"samekey"},
							},
						},
					},
				}),
			),
		},
		"invalid toleration key": {
			expectedError: "spec.tolerations[0].key",
			spec: *podtest.MakePod("123",
				podtest.SetTolerations(core.Toleration{Key: "nospecialchars^=@", Operator: "Equal", Value: "bar", Effect: "NoSchedule"}),
			),
		},
		"invalid toleration operator": {
			expectedError: "spec.tolerations[0].operator",
			spec: *podtest.MakePod("123",
				podtest.SetTolerations(core.Toleration{Key: "foo", Operator: "In", Value: "bar", Effect: "NoSchedule"}),
			),
		},
		"value must be empty when `operator` is 'Exists'": {
			expectedError: "spec.tolerations[0].operator",
			spec: *podtest.MakePod("123",
				podtest.SetTolerations(core.Toleration{Key: "foo", Operator: "Exists", Value: "bar", Effect: "NoSchedule"}),
			),
		},
		"operator must be 'Exists' when `key` is empty": {
			expectedError: "spec.tolerations[0].operator",
			spec: *podtest.MakePod("123",
				podtest.SetTolerations(core.Toleration{Operator: "Equal", Value: "bar", Effect: "NoSchedule"}),
			),
		},
		"effect must be 'NoExecute' when `TolerationSeconds` is set": {
			expectedError: "spec.tolerations[0].effect",
			spec: *podtest.MakePod("pod-forgiveness-invalid",
				podtest.SetTolerations(core.Toleration{Key: "node.kubernetes.io/not-ready", Operator: "Exists", Effect: "NoSchedule", TolerationSeconds: &[]int64{20}[0]}),
			),
		},
		"must be a valid pod seccomp profile": {
			expectedError: "must be a valid seccomp profile",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.SeccompPodAnnotationKey: "foo",
				}),
			),
		},
		"must be a valid container seccomp profile": {
			expectedError: "must be a valid seccomp profile",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.SeccompContainerAnnotationKeyPrefix + "foo": "foo",
				}),
			),
		},
		"must be a non-empty container name in seccomp annotation": {
			expectedError: "name part must be non-empty",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.SeccompContainerAnnotationKeyPrefix: "foo",
				}),
			),
		},
		"must be a non-empty container profile in seccomp annotation": {
			expectedError: "must be a valid seccomp profile",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.SeccompContainerAnnotationKeyPrefix + "foo": "",
				}),
			),
		},
		"must match seccomp profile type and pod annotation": {
			expectedError: "seccomp type in annotation and field must match",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.SeccompPodAnnotationKey: "unconfined",
				}),
				podtest.SetSecurityContext(&core.PodSecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type: core.SeccompProfileTypeRuntimeDefault,
					},
				}),
			),
		},
		"must match seccomp profile type and container annotation": {
			expectedError: "seccomp type in annotation and field must match",
			spec: *podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("ctr",
					podtest.SetContainerSecurityContext(core.SecurityContext{
						SeccompProfile: &core.SeccompProfile{
							Type: core.SeccompProfileTypeRuntimeDefault,
						},
					}),
				)),
				podtest.SetAnnotations(map[string]string{
					core.SeccompContainerAnnotationKeyPrefix + "ctr": "unconfined",
				}),
			),
		},
		"must be a relative path in a node-local seccomp profile annotation": {
			expectedError: "must be a relative path",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.SeccompPodAnnotationKey: "localhost//foo",
				}),
			),
		},
		"must not start with '../'": {
			expectedError: "must not contain '..'",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.SeccompPodAnnotationKey: "localhost/../foo",
				}),
			),
		},
		"AppArmor profile must apply to a container": {
			expectedError: "metadata.annotations[container.apparmor.security.beta.kubernetes.io/fake-ctr]",
			spec: *podtest.MakePod("123",
				podtest.SetInitContainers(podtest.MakeContainer("init-ctr")),
				podtest.SetAnnotations(map[string]string{
					v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "ctr":      v1.DeprecatedAppArmorBetaProfileRuntimeDefault,
					v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "init-ctr": v1.DeprecatedAppArmorBetaProfileRuntimeDefault,
					v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "fake-ctr": v1.DeprecatedAppArmorBetaProfileRuntimeDefault,
				}),
			),
		},
		"AppArmor profile format must be valid": {
			expectedError: "invalid AppArmor profile name",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "ctr": "bad-name",
				}),
			),
		},
		"only default AppArmor profile may start with runtime/": {
			expectedError: "invalid AppArmor profile name",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "ctr": "runtime/foo",
				}),
			),
		},
		"unsupported pod AppArmor profile type": {
			expectedError: `Unsupported value: "test"`,
			spec: *podtest.MakePod("123",
				podtest.SetSecurityContext(&core.PodSecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type: "test",
					},
				}),
			),
		},
		"unsupported container AppArmor profile type": {
			expectedError: `Unsupported value: "test"`,
			spec: *podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("ctr",
					podtest.SetContainerSecurityContext(core.SecurityContext{
						AppArmorProfile: &core.AppArmorProfile{
							Type: "test",
						},
					}),
				)),
			),
		},
		"missing pod AppArmor profile type": {
			expectedError: "Required value: type is required when appArmorProfile is set",
			spec: *podtest.MakePod("123",
				podtest.SetSecurityContext(&core.PodSecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type: "",
					},
				}),
			),
		},
		"missing AppArmor localhost profile": {
			expectedError: "Required value: must be set when AppArmor type is Localhost",
			spec: *podtest.MakePod("123",
				podtest.SetSecurityContext(&core.PodSecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type: core.AppArmorProfileTypeLocalhost,
					},
				}),
			),
		},
		"empty AppArmor localhost profile": {
			expectedError: "Required value: must be set when AppArmor type is Localhost",
			spec: *podtest.MakePod("123",
				podtest.SetSecurityContext(&core.PodSecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type:             core.AppArmorProfileTypeLocalhost,
						LocalhostProfile: ptr.To(""),
					},
				}),
			),
		},
		"invalid AppArmor localhost profile type": {
			expectedError: `Invalid value: "foo-bar"`,
			spec: *podtest.MakePod("123",
				podtest.SetSecurityContext(&core.PodSecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type:             core.AppArmorProfileTypeRuntimeDefault,
						LocalhostProfile: ptr.To("foo-bar"),
					},
				}),
			),
		},
		"invalid AppArmor localhost profile": {
			expectedError: `Invalid value: "foo-bar "`,
			spec: *podtest.MakePod("123",
				podtest.SetSecurityContext(&core.PodSecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type:             core.AppArmorProfileTypeLocalhost,
						LocalhostProfile: ptr.To("foo-bar "),
					},
				}),
			),
		},
		"too long AppArmor localhost profile": {
			expectedError: "Too long: may not be more than 4095 bytes",
			spec: *podtest.MakePod("123",
				podtest.SetSecurityContext(&core.PodSecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type:             core.AppArmorProfileTypeLocalhost,
						LocalhostProfile: ptr.To(strings.Repeat("a", 4096)),
					},
				}),
			),
		},
		"mismatched AppArmor field and annotation types": {
			expectedError: "Forbidden: apparmor type in annotation and field must match",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": core.DeprecatedAppArmorAnnotationValueRuntimeDefault,
				}),
				podtest.SetContainers(podtest.MakeContainer("ctr",
					podtest.SetContainerSecurityContext(core.SecurityContext{
						AppArmorProfile: &core.AppArmorProfile{
							Type: core.AppArmorProfileTypeUnconfined,
						},
					}),
				)),
			),
		},
		"mismatched AppArmor pod field and annotation types": {
			expectedError: "Forbidden: apparmor type in annotation and field must match",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": core.DeprecatedAppArmorAnnotationValueRuntimeDefault,
				}),
				podtest.SetSecurityContext(&core.PodSecurityContext{
					AppArmorProfile: &core.AppArmorProfile{
						Type: core.AppArmorProfileTypeUnconfined,
					},
				}),
			),
		},
		"mismatched AppArmor localhost profiles": {
			expectedError: "Forbidden: apparmor profile in annotation and field must match",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{
					core.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": core.DeprecatedAppArmorAnnotationValueLocalhostPrefix + "foo",
				}),
				podtest.SetContainers(podtest.MakeContainer("ctr",
					podtest.SetContainerSecurityContext(core.SecurityContext{
						AppArmorProfile: &core.AppArmorProfile{
							Type:             core.AppArmorProfileTypeLocalhost,
							LocalhostProfile: ptr.To("bar"),
						},
					}),
				)),
			),
		},
		"invalid extended resource name in container request": {
			expectedError: "must be a standard resource for containers",
			spec: *podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("invalid",
					podtest.SetContainerResources(
						podtest.MakeResourceRequirements(
							map[string]string{
								"invalid-name": "2",
							},
							map[string]string{
								"invalid-name": "2",
							},
						))),
				),
			),
		},
		"invalid extended resource requirement: request must be == limit": {
			expectedError: "must be equal to example.com/a",
			spec: *podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("invalid",
					podtest.SetContainerResources(
						podtest.MakeResourceRequirements(
							map[string]string{
								"example.com/a": "2",
							},
							map[string]string{
								"example.com/a": "1",
							},
						))),
				),
			),
		},
		"invalid extended resource requirement without limit": {
			expectedError: "Limit must be set",
			spec: *podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("invalid",
					podtest.SetContainerResources(
						podtest.MakeResourceRequirements(
							map[string]string{
								"example.com/a": "2",
							},
							map[string]string{},
						))),
				),
			),
		},
		"invalid fractional extended resource in container request": {
			expectedError: "must be an integer",
			spec: *podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("invalid",
					podtest.SetContainerResources(
						podtest.MakeResourceRequirements(
							map[string]string{
								"example.com/a": "500m",
							},
							map[string]string{},
						))),
				),
			),
		},
		"invalid fractional extended resource in init container request": {
			expectedError: "must be an integer",
			spec: *podtest.MakePod("123",
				podtest.SetInitContainers(podtest.MakeContainer("invalid",
					podtest.SetContainerResources(
						podtest.MakeResourceRequirements(
							map[string]string{
								"example.com/a": "500m",
							},
							map[string]string{},
						))),
				),
			),
		},
		"invalid fractional extended resource in container limit": {
			expectedError: "must be an integer",
			spec: *podtest.MakePod("123",
				podtest.SetContainers(podtest.MakeContainer("invalid",
					podtest.SetContainerResources(
						podtest.MakeResourceRequirements(
							map[string]string{
								"example.com/a": "5",
							},
							map[string]string{
								"example.com/a": "2.5",
							},
						))),
				),
			),
		},
		"invalid fractional extended resource in init container limit": {
			expectedError: "must be an integer",
			spec: *podtest.MakePod("123",
				podtest.SetInitContainers(podtest.MakeContainer("invalid",
					podtest.SetContainerResources(
						podtest.MakeResourceRequirements(
							map[string]string{
								"example.com/a": "2.5",
							},
							map[string]string{
								"example.com/a": "2.5",
							},
						))),
				),
			),
		},
		"mirror-pod present without nodeName": {
			expectedError: "mirror",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{core.MirrorPodAnnotationKey: ""}),
			),
		},
		"mirror-pod populated without nodeName": {
			expectedError: "mirror",
			spec: *podtest.MakePod("123",
				podtest.SetAnnotations(map[string]string{core.MirrorPodAnnotationKey: "foo"}),
			),
		},
		"serviceaccount token projected volume with no serviceaccount name specified": {
			expectedError: "must not be specified when serviceAccountName is not set",
			spec: *podtest.MakePod("123",
				podtest.SetVolumes(core.Volume{
					Name: "projected-volume",
					VolumeSource: core.VolumeSource{
						Projected: &core.ProjectedVolumeSource{
							Sources: []core.VolumeProjection{{
								ServiceAccountToken: &core.ServiceAccountTokenProjection{
									Audience:          "foo-audience",
									ExpirationSeconds: 6000,
									Path:              "foo-path",
								},
							}},
						},
					},
				}),
			),
		},
		"ClusterTrustBundlePEM projected volume using both byName and bySigner": {
			expectedError: "only one of name and signerName may be used",
			spec: *podtest.MakePod("valid-extended",
				podtest.SetVolumes(core.Volume{
					Name: "projected-volume",
					VolumeSource: core.VolumeSource{
						Projected: &core.ProjectedVolumeSource{
							Sources: []core.VolumeProjection{
								{
									ClusterTrustBundle: &core.ClusterTrustBundleProjection{
										Path:       "foo-path",
										SignerName: utilpointer.String("example.com/foo"),
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												"version": "live",
											},
										},
										Name: utilpointer.String("foo"),
									},
								},
							},
						},
					},
				}),
			),
		},
		"ClusterTrustBundlePEM projected volume byName with no name": {
			expectedError: "must be a valid object name",
			spec: *podtest.MakePod("valid-extended",
				podtest.SetVolumes(core.Volume{
					Name: "projected-volume",
					VolumeSource: core.VolumeSource{
						Projected: &core.ProjectedVolumeSource{
							Sources: []core.VolumeProjection{
								{
									ClusterTrustBundle: &core.ClusterTrustBundleProjection{
										Path: "foo-path",
										Name: utilpointer.String(""),
									},
								},
							},
						},
					},
				}),
			),
		},
		"ClusterTrustBundlePEM projected volume bySigner with no signer name": {
			expectedError: "must be a valid signer name",
			spec: *podtest.MakePod("valid-extended",
				podtest.SetVolumes(core.Volume{
					Name: "projected-volume",
					VolumeSource: core.VolumeSource{
						Projected: &core.ProjectedVolumeSource{
							Sources: []core.VolumeProjection{
								{
									ClusterTrustBundle: &core.ClusterTrustBundleProjection{
										Path:       "foo-path",
										SignerName: utilpointer.String(""),
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												"foo": "bar",
											},
										},
									},
								},
							},
						},
					},
				}),
			),
		},
		"ClusterTrustBundlePEM projected volume bySigner with invalid signer name": {
			expectedError: "must be a fully qualified domain and path of the form",
			spec: *podtest.MakePod("valid-extended",
				podtest.SetVolumes(core.Volume{
					Name: "projected-volume",
					VolumeSource: core.VolumeSource{
						Projected: &core.ProjectedVolumeSource{
							Sources: []core.VolumeProjection{
								{
									ClusterTrustBundle: &core.ClusterTrustBundleProjection{
										Path:       "foo-path",
										SignerName: utilpointer.String("example.com/foo/invalid"),
									},
								},
							},
						},
					},
				}),
			),
		},
		"final PVC name for ephemeral volume must be valid": {
			expectedError: "spec.volumes[1].name: Invalid value: \"" + longVolName + "\": PVC name \"" + longPodName + "-" + longVolName + "\": must be no more than 253 characters",
			spec: *podtest.MakePod(longPodName,
				podtest.SetVolumes(
					core.Volume{Name: "pvc", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"}}},
					core.Volume{Name: longVolName, VolumeSource: core.VolumeSource{Ephemeral: &core.EphemeralVolumeSource{VolumeClaimTemplate: &validPVCTemplate}}},
				),
			),
		},
		"PersistentVolumeClaimVolumeSource must not reference a generated PVC": {
			expectedError: "spec.volumes[0].persistentVolumeClaim.claimName: Invalid value: \"123-ephemeral-volume\": must not reference a PVC that gets created for an ephemeral volume",
			spec: *podtest.MakePod("123",
				podtest.SetVolumes(
					core.Volume{Name: "pvc-volume", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "123-ephemeral-volume"}}},
					core.Volume{Name: "ephemeral-volume", VolumeSource: core.VolumeSource{Ephemeral: &core.EphemeralVolumeSource{VolumeClaimTemplate: &validPVCTemplate}}},
				),
			),
		},
		"invalid pod-deletion-cost": {
			expectedError: "metadata.annotations[controller.kubernetes.io/pod-deletion-cost]: Invalid value: \"text\": must be a 32bit integer",
			spec: *podtest.MakePod("valid-extended",
				podtest.SetAnnotations(map[string]string{core.PodDeletionCost: "text"}),
			),
		},
		"invalid leading zeros pod-deletion-cost": {
			expectedError: "metadata.annotations[controller.kubernetes.io/pod-deletion-cost]: Invalid value: \"008\": must be a 32bit integer",
			spec: *podtest.MakePod("valid-extended",
				podtest.SetAnnotations(map[string]string{core.PodDeletionCost: "008"}),
			),
		},
		"invalid leading plus sign pod-deletion-cost": {
			expectedError: "metadata.annotations[controller.kubernetes.io/pod-deletion-cost]: Invalid value: \"+10\": must be a 32bit integer",
			spec: *podtest.MakePod("valid-extended",
				podtest.SetAnnotations(map[string]string{core.PodDeletionCost: "+10"}),
			),
		},
		"invalid required node affinity, value of NodeSelectorRequirement should be a valid label value": {
			expectedError: "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]: Invalid value: \"-1\": a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')",
			spec: *podtest.MakePod("123",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "foo",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"-1"},
								}},
							}},
						}},
				}),
			),
		},
	}

	for k, v := range errorCases {
		t.Run(k, func(t *testing.T) {
			if errs := ValidatePodCreate(&v.spec, PodValidationOptions{}); len(errs) == 0 {
				t.Errorf("expected failure")
			} else if v.expectedError == "" {
				t.Errorf("missing expectedError, got %q", errs.ToAggregate().Error())
			} else if actualError := errs.ToAggregate().Error(); !strings.Contains(actualError, v.expectedError) {
				t.Errorf("expected error to contain %q, got %q", v.expectedError, actualError)
			}
		})
	}
}

func TestValidatePodCreateWithSchedulingGates(t *testing.T) {
	applyEssentials := func(pod *core.Pod) {
		pod.Spec.Containers = []core.Container{
			{Name: "con", Image: "pause", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"},
		}
		pod.Spec.RestartPolicy = core.RestartPolicyAlways
		pod.Spec.DNSPolicy = core.DNSClusterFirst
	}
	fldPath := field.NewPath("spec")

	tests := []struct {
		name            string
		pod             *core.Pod
		wantFieldErrors field.ErrorList
	}{{
		name: "create a Pod with nodeName and schedulingGates, feature enabled",
		pod: podtest.MakePod("pod",
			podtest.SetNodeName("node"),
			podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "foo"}),
		),
		wantFieldErrors: []*field.Error{field.Forbidden(fldPath.Child("nodeName"), "cannot be set until all schedulingGates have been cleared")},
	}, {
		name: "create a Pod with schedulingGates, feature enabled",
		pod: podtest.MakePod("pod",
			podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "foo"}),
		),
		wantFieldErrors: nil,
	},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyEssentials(tt.pod)
			errs := ValidatePodCreate(tt.pod, PodValidationOptions{})
			if diff := cmp.Diff(tt.wantFieldErrors, errs); diff != "" {
				t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestValidatePodUpdate(t *testing.T) {
	var (
		activeDeadlineSecondsZero     = int64(0)
		activeDeadlineSecondsNegative = int64(-30)
		activeDeadlineSecondsPositive = int64(30)
		activeDeadlineSecondsLarger   = int64(31)
		validfsGroupChangePolicy      = core.FSGroupChangeOnRootMismatch

		now    = metav1.Now()
		grace  = int64(30)
		grace2 = int64(31)
	)

	tests := []struct {
		test string
		old  core.Pod
		new  core.Pod
		err  string
		opts PodValidationOptions
	}{
		{new: *podtest.MakePod(""), old: *podtest.MakePod(""), err: "", test: "nothing"}, {
			new:  *podtest.MakePod("foo"),
			old:  *podtest.MakePod("bar"),
			err:  "metadata.name",
			test: "ids",
		}, {
			new:  *podtest.MakePod("foo", podtest.SetLabels(map[string]string{"foo": "bar"})),
			old:  *podtest.MakePod("foo", podtest.SetLabels(map[string]string{"bar": "foo"})),
			err:  "",
			test: "labels",
		}, {
			new:  *podtest.MakePod("foo", podtest.SetAnnotations(map[string]string{"foo": "bar"})),
			old:  *podtest.MakePod("foo", podtest.SetAnnotations(map[string]string{"bar": "foo"})),
			err:  "",
			test: "annotations",
		}, {
			new: *podtest.MakePod("foo", podtest.SetContainers(
				podtest.MakeContainer("foo", podtest.SetContainerImage("foo:V2")))),
			old: *podtest.MakePod("foo", podtest.SetContainers(
				podtest.MakeContainer("foo", podtest.SetContainerImage("foo:V1")),
				podtest.MakeContainer("bar", podtest.SetContainerImage("bar:V1")))),
			err:  "may not add or remove containers",
			test: "less containers",
		}, {
			new: *podtest.MakePod("foo", podtest.SetContainers(
				podtest.MakeContainer("foo", podtest.SetContainerImage("foo:V2")),
				podtest.MakeContainer("bar", podtest.SetContainerImage("bar:V2")))),
			old: *podtest.MakePod("foo", podtest.SetContainers(
				podtest.MakeContainer("foo", podtest.SetContainerImage("foo:V1")))),
			err:  "may not add or remove containers",
			test: "more containers",
		}, {
			new: *podtest.MakePod("foo", podtest.SetInitContainers(
				podtest.MakeContainer("foo", podtest.SetContainerImage("foo:V2")))),
			old: *podtest.MakePod("foo", podtest.SetInitContainers(
				podtest.MakeContainer("foo", podtest.SetContainerImage("foo:V1")),
				podtest.MakeContainer("bar", podtest.SetContainerImage("bar:V1")))),
			err:  "may not add or remove containers",
			test: "more init containers",
		}, {
			new:  *podtest.MakePod("foo"),
			old:  *podtest.MakePod("foo", podtest.SetObjectMeta(metav1.ObjectMeta{DeletionTimestamp: &now})),
			err:  "metadata.deletionTimestamp",
			test: "deletion timestamp removed",
		}, {
			new:  *podtest.MakePod("foo", podtest.SetObjectMeta(metav1.ObjectMeta{DeletionTimestamp: &now})),
			old:  *podtest.MakePod("foo"),
			err:  "metadata.deletionTimestamp",
			test: "deletion timestamp added",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetObjectMeta(metav1.ObjectMeta{DeletionTimestamp: &now, DeletionGracePeriodSeconds: &grace})),
			old: *podtest.MakePod("foo",
				podtest.SetObjectMeta(metav1.ObjectMeta{DeletionTimestamp: &now, DeletionGracePeriodSeconds: &grace2})),
			err:  "metadata.deletionGracePeriodSeconds",
			test: "deletion grace period seconds changed",
		}, {
			new: *podtest.MakePod("foo", podtest.SetContainers(
				podtest.MakeContainer("container", podtest.SetContainerImage("foo:V1")))),
			old: *podtest.MakePod("foo", podtest.SetContainers(
				podtest.MakeContainer("container", podtest.SetContainerImage("foo:V2")))),
			err:  "",
			test: "image change",
		}, {
			new: *podtest.MakePod("foo", podtest.SetInitContainers(
				podtest.MakeContainer("container", podtest.SetContainerImage("foo:V1")))),
			old: *podtest.MakePod("foo", podtest.SetInitContainers(
				podtest.MakeContainer("container", podtest.SetContainerImage("foo:V2")))),
			err:  "",
			test: "init container image change",
		}, {
			new: *podtest.MakePod("foo", podtest.SetContainers(
				podtest.MakeContainer("container", podtest.SetContainerImage("")))),
			old: *podtest.MakePod("foo", podtest.SetContainers(
				podtest.MakeContainer("container", podtest.SetContainerImage("foo:V2")))),
			err:  "spec.containers[0].image",
			test: "image change to empty",
		}, {
			new: *podtest.MakePod("foo", podtest.SetInitContainers(
				podtest.MakeContainer("container", podtest.SetContainerImage("")))),
			old: *podtest.MakePod("foo", podtest.SetInitContainers(
				podtest.MakeContainer("container", podtest.SetContainerImage("foo:V2")))),
			err:  "spec.initContainers[0].image",
			test: "init container image change to empty",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetEphemeralContainers(core.EphemeralContainer{
					EphemeralContainerCommon: core.EphemeralContainerCommon{
						Name:  "ephemeral",
						Image: "busybox",
					},
				}),
			),
			old:  *podtest.MakePod("foo"),
			err:  "Forbidden: pod updates may not change fields other than",
			test: "ephemeralContainer changes are not allowed via normal pod update",
		}, {
			new:  *podtest.MakePod(""),
			old:  *podtest.MakePod(""),
			err:  "",
			test: "activeDeadlineSeconds no change, nil",
		}, {
			new: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsPositive)),
			old: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsPositive)),
			err:  "",
			test: "activeDeadlineSeconds no change, set",
		}, {
			new: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsPositive)),
			old:  *podtest.MakePod(""),
			err:  "",
			test: "activeDeadlineSeconds change to positive from nil",
		}, {
			new: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsPositive)),
			old: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsLarger)),
			err:  "",
			test: "activeDeadlineSeconds change to smaller positive",
		}, {
			new: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsLarger)),
			old: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsPositive)),
			err:  "spec.activeDeadlineSeconds",
			test: "activeDeadlineSeconds change to larger positive",
		}, {
			new: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsNegative)),
			old:  *podtest.MakePod(""),
			err:  "spec.activeDeadlineSeconds",
			test: "activeDeadlineSeconds change to negative from nil",
		}, {
			new: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsNegative)),
			old: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsPositive)),
			err:  "spec.activeDeadlineSeconds",
			test: "activeDeadlineSeconds change to negative from positive",
		}, {
			new: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsZero)),
			old: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsPositive)),
			err:  "spec.activeDeadlineSeconds",
			test: "activeDeadlineSeconds change to zero from positive",
		}, {
			new: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsZero)),
			old:  *podtest.MakePod(""),
			err:  "spec.activeDeadlineSeconds",
			test: "activeDeadlineSeconds change to zero from nil",
		}, {
			new: *podtest.MakePod(""),
			old: *podtest.MakePod("",
				podtest.SetActiveDeadlineSeconds(activeDeadlineSecondsPositive)),
			err:  "spec.activeDeadlineSeconds",
			test: "activeDeadlineSeconds change to nil from positive",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("200m", "0", "1Gi", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "0", "1Gi", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "cpu limit change",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "100Mi", "", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "200Mi", "", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "memory limit change",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "100Mi", "1Gi", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "100Mi", "2Gi", ""),
					}))),
			),
			err:  "Forbidden: pod updates may not change fields other than",
			test: "storage limit change",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "0", "", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("200m", "0", "", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "cpu request change",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("0", "200Mi", "", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("0", "100Mi", "", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "memory request change",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "0", "2Gi", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "0", "1Gi", ""),
					}))),
			),
			err:  "Forbidden: pod updates may not change fields other than",
			test: "storage request change",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("200m", "400Mi", "1Gi", ""),
						Requests: getResources("200m", "400Mi", "1Gi", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("100m", "100Mi", "1Gi", ""),
						Requests: getResources("100m", "100Mi", "1Gi", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS unchanged, guaranteed -> guaranteed",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("200m", "200Mi", "2Gi", ""),
						Requests: getResources("100m", "100Mi", "1Gi", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("400m", "400Mi", "2Gi", ""),
						Requests: getResources("200m", "200Mi", "1Gi", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS unchanged, burstable -> burstable",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("200m", "200Mi", "", ""),
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS unchanged, burstable -> burstable, add limits",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("200m", "200Mi", "", ""),
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS unchanged, burstable -> burstable, remove limits",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("400m", "", "1Gi", ""),
						Requests: getResources("300m", "", "1Gi", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("200m", "500Mi", "1Gi", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS unchanged, burstable -> burstable, add requests",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("400m", "500Mi", "2Gi", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("200m", "300Mi", "2Gi", ""),
						Requests: getResources("100m", "200Mi", "", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS unchanged, burstable -> burstable, remove requests",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("200m", "200Mi", "", ""),
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("100m", "100Mi", "", ""),
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS change, guaranteed -> burstable",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("100m", "100Mi", "", ""),
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS change, burstable -> guaranteed",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("200m", "200Mi", "", ""),
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			old:  *podtest.MakePod("pod"),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS change, besteffort -> burstable",
		}, {
			new: *podtest.MakePod("pod"),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits:   getResources("200m", "200Mi", "", ""),
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "Pod QoS change, burstable -> besteffort",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetSecurityContext(&core.PodSecurityContext{
					FSGroupChangePolicy: &validfsGroupChangePolicy,
				}),
			),
			old: *podtest.MakePod("pod",
				podtest.SetSecurityContext(&core.PodSecurityContext{
					FSGroupChangePolicy: nil,
				}),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "fsGroupChangePolicy change",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerPorts(core.ContainerPort{HostPort: 8080, ContainerPort: 80}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerPorts(core.ContainerPort{HostPort: 8000, ContainerPort: 80}))),
			),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "port change",
		}, {
			new:  *podtest.MakePod("foo", podtest.SetLabels(map[string]string{"foo": "bar"})),
			old:  *podtest.MakePod("foo", podtest.SetLabels(map[string]string{"bar": "foo"})),
			err:  "",
			test: "bad label change",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value2"}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value1"}),
			),
			err:  "spec.tolerations: Forbidden",
			test: "existing toleration value modified in pod spec updates",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value2", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: nil}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value1", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: &[]int64{10}[0]}),
			),
			err:  "spec.tolerations: Forbidden",
			test: "existing toleration value modified in pod spec updates with modified tolerationSeconds",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value1", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: &[]int64{10}[0]}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value1", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: &[]int64{20}[0]}),
			),
			err:  "",
			test: "modified tolerationSeconds in existing toleration value in pod spec updates",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value2"}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName(""),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value1"}),
			),
			err:  "spec.tolerations: Forbidden",
			test: "toleration modified in updates to an unscheduled pod",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value1"}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value1"}),
			),
			err:  "",
			test: "tolerations unmodified in updates to a scheduled pod",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(
					core.Toleration{Key: "key1", Value: "value1", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: &[]int64{20}[0]},
					core.Toleration{Key: "key2", Value: "value2", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: &[]int64{30}[0]}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(
					core.Toleration{Key: "key1", Value: "value1", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: &[]int64{10}[0]}),
			),
			err:  "",
			test: "added valid new toleration to existing tolerations in pod spec updates",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(
					core.Toleration{Key: "key1", Value: "value1", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: &[]int64{20}[0]},
					core.Toleration{Key: "key2", Value: "value2", Operator: "Equal", Effect: "NoSchedule", TolerationSeconds: &[]int64{30}[0]},
				),
			),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetTolerations(core.Toleration{Key: "key1", Value: "value1", Operator: "Equal", Effect: "NoExecute", TolerationSeconds: &[]int64{10}[0]}),
			),
			err:  "spec.tolerations[1].effect",
			test: "added invalid new toleration to existing tolerations in pod spec updates",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("foo")),
			old:  *podtest.MakePod("foo"),
			err:  "spec: Forbidden: pod updates may not change fields",
			test: "removed nodeName from pod spec",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetAnnotations(map[string]string{core.MirrorPodAnnotationKey: ""}),
				podtest.SetNodeName("foo")),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName("foo")),
			err:  "metadata.annotations[kubernetes.io/config.mirror]",
			test: "added mirror pod annotation",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("foo")),
			old: *podtest.MakePod("foo",
				podtest.SetAnnotations(map[string]string{core.MirrorPodAnnotationKey: ""}),
				podtest.SetNodeName("foo")),
			err:  "metadata.annotations[kubernetes.io/config.mirror]",
			test: "removed mirror pod annotation",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetAnnotations(map[string]string{core.MirrorPodAnnotationKey: "foo"}),
				podtest.SetNodeName("foo")),
			old: *podtest.MakePod("foo",
				podtest.SetAnnotations(map[string]string{core.MirrorPodAnnotationKey: "bar"}),
				podtest.SetNodeName("foo")),
			err:  "metadata.annotations[kubernetes.io/config.mirror]",
			test: "changed mirror pod annotation",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetPriorityClassName("bar-priority"),
			),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetPriorityClassName("foo-priority"),
			),
			err:  "spec: Forbidden: pod updates",
			test: "changed priority class name",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetPriorityClassName(""),
			),
			old: *podtest.MakePod("foo",
				podtest.SetNodeName("node1"),
				podtest.SetPriorityClassName("foo-priority"),
			),
			err:  "spec: Forbidden: pod updates",
			test: "removed priority class name",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetTerminationGracePeriodSeconds(1),
			),
			old: *podtest.MakePod("foo",
				podtest.SetTerminationGracePeriodSeconds(-1),
			),
			err:  "",
			test: "update termination grace period seconds",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetTerminationGracePeriodSeconds(0),
			),
			old: *podtest.MakePod("foo",
				podtest.SetTerminationGracePeriodSeconds(-1),
			),
			err:  "spec: Forbidden: pod updates",
			test: "update termination grace period seconds not 1",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetOS(core.Windows),
				podtest.SetSecurityContext(&core.PodSecurityContext{SELinuxOptions: &core.SELinuxOptions{Role: "dummy"}}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetOS(core.Linux),
				podtest.SetSecurityContext(&core.PodSecurityContext{SELinuxOptions: &core.SELinuxOptions{Role: "dummy"}}),
			),
			err:  "Forbidden: pod updates may not change fields other than `spec.containers[*].image",
			test: "pod OS changing from Linux to Windows, IdentifyPodOS featuregate set",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetOS(core.Windows),
				podtest.SetSecurityContext(&core.PodSecurityContext{SELinuxOptions: &core.SELinuxOptions{Role: "dummy"}}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetOS(core.Linux),
				podtest.SetSecurityContext(&core.PodSecurityContext{SELinuxOptions: &core.SELinuxOptions{Role: "dummy"}}),
			),
			err:  "spec.securityContext.seLinuxOptions: Forbidden",
			test: "pod OS changing from Linux to Windows, IdentifyPodOS featuregate set, we'd get SELinux errors as well",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetOS("dummy"),
			),
			old:  *podtest.MakePod("foo"),
			err:  "Forbidden: pod updates may not change fields other than `spec.containers[*].image",
			test: "invalid PodOS update, IdentifyPodOS featuregate set",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetOS(core.Linux),
			),
			old: *podtest.MakePod("foo",
				podtest.SetOS(core.Windows),
			),
			err:  "Forbidden: pod updates may not change fields other than ",
			test: "update pod spec OS to a valid value, featuregate disabled",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "foo"}),
			),
			old:  *podtest.MakePod("foo"),
			err:  "Forbidden: only deletion is allowed, but found new scheduling gate 'foo'",
			test: "update pod spec schedulingGates: add new scheduling gate",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "bar"}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "foo"}),
			),
			err:  "Forbidden: only deletion is allowed, but found new scheduling gate 'bar'",
			test: "update pod spec schedulingGates: mutating an existing scheduling gate",
		}, {
			new: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			old: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(
					core.PodSchedulingGate{Name: "foo"},
					core.PodSchedulingGate{Name: "bar"}),
			),
			err:  "Forbidden: only deletion is allowed, but found new scheduling gate 'baz'",
			test: "update pod spec schedulingGates: mutating an existing scheduling gate along with deletion",
		}, {
			new: *podtest.MakePod("foo"),
			old: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "foo"}),
			),
			err:  "",
			test: "update pod spec schedulingGates: legal deletion",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetNodeSelector(map[string]string{
					"foo": "bar",
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			test: "adding node selector is allowed for gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetNodeSelector(map[string]string{
					"foo": "bar",
				},
				),
			),
			new: *podtest.MakePod("foo",
				podtest.SetNodeSelector(map[string]string{
					"foo":  "bar",
					"foo2": "bar2",
				},
				),
			),
			err:  "Forbidden: pod updates may not change fields other than `spec.containers[*].image",
			test: "adding node selector is not allowed for non-gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetNodeSelector(map[string]string{
					"foo": "bar",
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.nodeSelector: Invalid value:",
			test: "removing node selector is not allowed for gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetNodeSelector(map[string]string{
					"foo": "bar",
				},
				),
			),
			new:  *podtest.MakePod("foo"),
			err:  "Forbidden: pod updates may not change fields other than `spec.containers[*].image",
			test: "removing node selector is not allowed for non-gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetNodeSelector(map[string]string{
					"foo": "bar",
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetNodeSelector(map[string]string{
					"foo":  "bar",
					"foo2": "bar2",
				}),
			),
			test: "old pod spec has scheduling gate, new pod spec does not, and node selector is added",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetNodeSelector(map[string]string{
					"foo": "bar",
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetNodeSelector(map[string]string{
					"foo": "new value",
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.nodeSelector: Invalid value:",
			test: "modifying value of existing node selector is not allowed",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}}}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							// Add 1 MatchExpression and 1 MatchField.
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}, {
									Key:      "expr2",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo2"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			test: "addition to nodeAffinity is allowed for gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms: Invalid value:",
			test: "old RequiredDuringSchedulingIgnoredDuringExecution is non-nil, new RequiredDuringSchedulingIgnoredDuringExecution is nil, pod is gated",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							// Add 1 MatchExpression and 1 MatchField.
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}, {
									Key:      "expr2",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo2"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
			),
			err:  "Forbidden: pod updates may not change fields other than `spec.containers[*].image",
			test: "addition to nodeAffinity is not allowed for non-gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							// Add 1 MatchExpression and 1 MatchField.
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}, {
									Key:      "expr2",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo2"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
			),
			test: "old pod spec has scheduling gate, new pod spec does not, and node affinity addition occurs",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0]: Invalid value:",
			test: "nodeAffinity deletion from MatchExpressions not allowed",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							// Add 1 MatchExpression and 1 MatchField.
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0]: Invalid value:",
			test: "nodeAffinity deletion from MatchFields not allowed",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							// Add 1 MatchExpression and 1 MatchField.
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"bar"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0]: Invalid value:",
			test: "nodeAffinity modification of item in MatchExpressions not allowed",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"bar"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0]: Invalid value:",
			test: "nodeAffinity modification of item in MatchFields not allowed",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"bar"},
								}},
							}, {
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo2"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"bar2"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms: Invalid value:",
			test: "nodeSelectorTerms addition on gated pod should fail",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
							Weight: 1.0,
							Preference: core.NodeSelectorTerm{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							},
						}},
					}}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
							Weight: 1.0,
							Preference: core.NodeSelectorTerm{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo2"},
								}},
							},
						}},
					}}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			test: "preferredDuringSchedulingIgnoredDuringExecution can modified for gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
							Weight: 1.0,
							Preference: core.NodeSelectorTerm{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							},
						}},
					}}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
							Weight: 1.0,
							Preference: core.NodeSelectorTerm{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}, {
									Key:      "expr2",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo2"},
								}},
								MatchFields: []core.NodeSelectorRequirement{{
									Key:      "metadata.name",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"bar"},
								}},
							},
						}},
					}}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			test: "preferredDuringSchedulingIgnoredDuringExecution can have additions for gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
							Weight: 1.0,
							Preference: core.NodeSelectorTerm{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							},
						}},
					}}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			test: "preferredDuringSchedulingIgnoredDuringExecution can have removals for gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms: Invalid value:",
			test: "new node affinity is nil",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []core.PreferredSchedulingTerm{{
							Weight: 1.0,
							Preference: core.NodeSelectorTerm{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							},
						}},
					}}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			test: "preferredDuringSchedulingIgnoredDuringExecution can have removals for gated pods",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{
								{},
							},
						},
					},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0]: Invalid value:",
			test: "empty NodeSelectorTerm (selects nothing) cannot become populated (selects something)",
		}, {
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(nil),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			test: "nil affinity can be mutated for gated pods",
		},
		{
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(nil),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
					PodAffinity: &core.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{{
							TopologyKey: "foo",
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"foo": "bar"},
							},
						}},
					},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "pod updates may not change fields other than",
			test: "the podAffinity cannot be updated on gated pods",
		},
		{
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(nil),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"foo"},
								}},
							}},
						}},
					PodAntiAffinity: &core.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []core.PodAffinityTerm{
							{
								TopologyKey: "foo",
								LabelSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{"foo": "bar"},
								},
							},
						},
					},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  "pod updates may not change fields other than",
			test: "the podAntiAffinity cannot be updated on gated pods",
		},
		{
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"-1"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"-1"},
								}},
							}},
						}},
				}),
			),
			test: "allow update pod if old pod already has invalid label-value in node affinity",
			opts: PodValidationOptions{AllowInvalidLabelValueInRequiredNodeAffinity: true},
		},
		{
			old: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"bar"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			new: *podtest.MakePod("foo",
				podtest.SetAffinity(&core.Affinity{
					NodeAffinity: &core.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
							NodeSelectorTerms: []core.NodeSelectorTerm{{
								MatchExpressions: []core.NodeSelectorRequirement{{
									Key:      "expr",
									Operator: core.NodeSelectorOpIn,
									Values:   []string{"-1"},
								}},
							}},
						}},
				}),
				podtest.SetSchedulingGates(core.PodSchedulingGate{Name: "baz"}),
			),
			err:  `a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.'`,
			test: "not allow update node affinity to an invalid label-value",
		},
		{
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("200m", "0", "1Gi", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "0", "1Gi", ""),
					}))),
			),
			err:  "pod updates may not change fields other than",
			test: "cpu limit change with pod-level resources",
		}, {
			new: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "100Mi", "", ""),
					}))),
			),
			old: *podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "200Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			err:  "pod updates may not change fields other than",
			test: "memory limit change with pod-level resources",
		},
	}

	for _, test := range tests {
		test.new.ObjectMeta.ResourceVersion = "1"
		test.old.ObjectMeta.ResourceVersion = "1"

		// set required fields if old and new match and have no opinion on the value
		if test.new.Name == "" && test.old.Name == "" {
			test.new.Name = "name"
			test.old.Name = "name"
		}
		if test.new.Namespace == "" && test.old.Namespace == "" {
			test.new.Namespace = "namespace"
			test.old.Namespace = "namespace"
		}
		if test.new.Spec.Containers == nil && test.old.Spec.Containers == nil {
			test.new.Spec.Containers = []core.Container{{Name: "autoadded", Image: "image", TerminationMessagePolicy: "File", ImagePullPolicy: "Always"}}
			test.old.Spec.Containers = []core.Container{{Name: "autoadded", Image: "image", TerminationMessagePolicy: "File", ImagePullPolicy: "Always"}}
		}
		if len(test.new.Spec.DNSPolicy) == 0 && len(test.old.Spec.DNSPolicy) == 0 {
			test.new.Spec.DNSPolicy = core.DNSClusterFirst
			test.old.Spec.DNSPolicy = core.DNSClusterFirst
		}
		if len(test.new.Spec.RestartPolicy) == 0 && len(test.old.Spec.RestartPolicy) == 0 {
			test.new.Spec.RestartPolicy = "Always"
			test.old.Spec.RestartPolicy = "Always"
		}

		errs := ValidatePodUpdate(&test.new, &test.old, test.opts)
		if test.err == "" {
			if len(errs) != 0 {
				t.Errorf("unexpected invalid: %s (%+v)\nA: %+v\nB: %+v", test.test, errs, test.new, test.old)
			}
		} else {
			if len(errs) == 0 {
				t.Errorf("unexpected valid: %s\nA: %+v\nB: %+v", test.test, test.new, test.old)
			} else if actualErr := errs.ToAggregate().Error(); !strings.Contains(actualErr, test.err) {
				t.Errorf("unexpected error message: %s\nExpected error: %s\nActual error: %s", test.test, test.err, actualErr)
			}
		}
	}
}

func TestValidatePodStatusUpdate(t *testing.T) {
	tests := []struct {
		new  core.Pod
		old  core.Pod
		err  string
		test string
	}{{
		*podtest.MakePod("foo",
			podtest.SetNodeName("node1"),
			podtest.SetStatus(core.PodStatus{
				NominatedNodeName: "node1",
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetNodeName("node1"),
			podtest.SetStatus(core.PodStatus{}),
		),
		"",
		"removed nominatedNodeName",
	}, {
		*podtest.MakePod("foo",
			podtest.SetNodeName("node1"),
		),
		*podtest.MakePod("foo",
			podtest.SetNodeName("node1"),
			podtest.SetStatus(core.PodStatus{
				NominatedNodeName: "node1",
			}),
		),
		"",
		"add valid nominatedNodeName",
	}, {
		*podtest.MakePod("foo",
			podtest.SetNodeName("node1"),
			podtest.SetStatus(core.PodStatus{
				NominatedNodeName: "Node1",
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetNodeName("node1"),
		),
		"nominatedNodeName",
		"Add invalid nominatedNodeName",
	}, {
		*podtest.MakePod("foo",
			podtest.SetNodeName("node1"),
			podtest.SetStatus(core.PodStatus{
				NominatedNodeName: "node1",
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetNodeName("node1"),
			podtest.SetStatus(core.PodStatus{
				NominatedNodeName: "node2",
			}),
		),
		"",
		"Update nominatedNodeName",
	}, {
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					Name:        "init",
					Ready:       false,
					Started:     proto.Bool(false),
					State: core.ContainerState{
						Waiting: &core.ContainerStateWaiting{
							Reason: "PodInitializing",
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					Name:        "main",
					Ready:       false,
					Started:     proto.Bool(false),
					State: core.ContainerState{
						Waiting: &core.ContainerStateWaiting{
							Reason: "PodInitializing",
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo"),
		"",
		"Container statuses pending",
	}, {
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "init",
					Ready:       true,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					Name:        "init",
					Ready:       false,
					State: core.ContainerState{
						Waiting: &core.ContainerStateWaiting{
							Reason: "PodInitializing",
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					Name:        "main",
					Ready:       false,
					Started:     proto.Bool(false),
					State: core.ContainerState{
						Waiting: &core.ContainerStateWaiting{
							Reason: "PodInitializing",
						},
					},
				}},
			}),
		),
		"",
		"Container statuses running",
	}, {
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				EphemeralContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "busybox",
					Name:        "debug",
					Ready:       false,
					State: core.ContainerState{
						Waiting: &core.ContainerStateWaiting{
							Reason: "PodInitializing",
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		"",
		"Container statuses add ephemeral container",
	}, {
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				EphemeralContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "busybox",
					ImageID:     "docker-pullable://busybox@sha256:d0gf00d",
					Name:        "debug",
					Ready:       false,
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				EphemeralContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "busybox",
					Name:        "debug",
					Ready:       false,
					State: core.ContainerState{
						Waiting: &core.ContainerStateWaiting{
							Reason: "PodInitializing",
						},
					},
				}},
			}),
		),
		"",
		"Container statuses ephemeral container running",
	}, {
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				EphemeralContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "busybox",
					ImageID:     "docker-pullable://busybox@sha256:d0gf00d",
					Name:        "debug",
					Ready:       false,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
							StartedAt:   metav1.NewTime(time.Now()),
							FinishedAt:  metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				EphemeralContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "busybox",
					ImageID:     "docker-pullable://busybox@sha256:d0gf00d",
					Name:        "debug",
					Ready:       false,
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		"",
		"Container statuses ephemeral container exited",
	}, {
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "init",
					Ready:       true,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
							StartedAt:   metav1.NewTime(time.Now()),
							FinishedAt:  metav1.NewTime(time.Now()),
						},
					},
				}},
				EphemeralContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "busybox",
					ImageID:     "docker-pullable://busybox@sha256:d0gf00d",
					Name:        "debug",
					Ready:       false,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
							StartedAt:   metav1.NewTime(time.Now()),
							FinishedAt:  metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "init",
					Ready:       true,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				EphemeralContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "busybox",
					ImageID:     "docker-pullable://busybox@sha256:d0gf00d",
					Name:        "debug",
					Ready:       false,
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		"",
		"Container statuses all containers terminated",
	}, {
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				ResourceClaimStatuses: []core.PodResourceClaimStatus{
					{Name: "no-such-claim", ResourceClaimName: utilpointer.String("my-claim")},
				},
			}),
		),
		*podtest.MakePod("foo"),
		"status.resourceClaimStatuses[0].name: Invalid value: \"no-such-claim\": must match the name of an entry in `spec.resourceClaims`",
		"Non-existent PodResourceClaim",
	}, {
		*podtest.MakePod("foo",
			podtest.SetResourceClaims(core.PodResourceClaim{Name: "my-claim"}),
			podtest.SetStatus(core.PodStatus{
				ResourceClaimStatuses: []core.PodResourceClaimStatus{
					{Name: "my-claim", ResourceClaimName: utilpointer.String("%$!#")},
				},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetResourceClaims(core.PodResourceClaim{Name: "my-claim"}),
		),
		`status.resourceClaimStatuses[0].name: Invalid value: "%$!#": a lowercase RFC 1123 subdomain must consist of`,
		"Invalid ResourceClaim name",
	}, {
		*podtest.MakePod("foo",
			podtest.SetResourceClaims(
				core.PodResourceClaim{Name: "my-claim"},
				core.PodResourceClaim{Name: "my-other-claim"},
			),
			podtest.SetStatus(core.PodStatus{
				ResourceClaimStatuses: []core.PodResourceClaimStatus{
					{Name: "my-claim", ResourceClaimName: utilpointer.String("foo-my-claim-12345")},
					{Name: "my-other-claim", ResourceClaimName: nil},
					{Name: "my-other-claim", ResourceClaimName: nil},
				},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetResourceClaims(core.PodResourceClaim{Name: "my-claim"}),
		),
		`status.resourceClaimStatuses[2].name: Duplicate value: "my-other-claim"`,
		"Duplicate ResourceClaimStatuses.Name",
	}, {
		*podtest.MakePod("foo",
			podtest.SetResourceClaims(
				core.PodResourceClaim{Name: "my-claim"},
				core.PodResourceClaim{Name: "my-other-claim"},
			),
			podtest.SetStatus(core.PodStatus{
				ResourceClaimStatuses: []core.PodResourceClaimStatus{
					{Name: "my-claim", ResourceClaimName: utilpointer.String("foo-my-claim-12345")},
					{Name: "my-other-claim", ResourceClaimName: nil},
				},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetResourceClaims(core.PodResourceClaim{Name: "my-claim"}),
		),
		"",
		"ResourceClaimStatuses okay",
	}, {
		*podtest.MakePod("foo",
			podtest.SetInitContainers(podtest.MakeContainer("init")),
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "init",
					Ready:       true,
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetInitContainers(podtest.MakeContainer("init")),
			podtest.SetRestartPolicy(core.RestartPolicyNever),
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "init",
					Ready:       false,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		`status.initContainerStatuses[0].state: Forbidden: may not be transitioned to non-terminated state`,
		"init container cannot restart if RestartPolicyNever",
	}, {
		*podtest.MakePod("foo",
			podtest.SetInitContainers(podtest.MakeContainer("restartable-init", podtest.SetContainerRestartPolicy(containerRestartPolicyAlways))),
			podtest.SetRestartPolicy(core.RestartPolicyNever),
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "restartable-init",
					Ready:       true,
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetInitContainers(podtest.MakeContainer("restartable-init", podtest.SetContainerRestartPolicy(containerRestartPolicyAlways))),
			podtest.SetRestartPolicy(core.RestartPolicyNever),
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "restartable-init",
					Ready:       false,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		"",
		"restartable init container can restart if RestartPolicyNever",
	}, {
		*podtest.MakePod("foo",
			podtest.SetInitContainers(podtest.MakeContainer("restartable-init", podtest.SetContainerRestartPolicy(containerRestartPolicyAlways))),
			podtest.SetRestartPolicy(core.RestartPolicyOnFailure),
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "restartable-init",
					Ready:       true,
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetInitContainers(podtest.MakeContainer("restartable-init", podtest.SetContainerRestartPolicy(containerRestartPolicyAlways))),
			podtest.SetRestartPolicy(core.RestartPolicyOnFailure),
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "restartable-init",
					Ready:       false,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		"",
		"restartable init container can restart if RestartPolicyOnFailure",
	}, {
		*podtest.MakePod("foo",
			podtest.SetInitContainers(podtest.MakeContainer("restartable-init", podtest.SetContainerRestartPolicy(containerRestartPolicyAlways))),
			podtest.SetRestartPolicy(core.RestartPolicyAlways),
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "restartable-init",
					Ready:       true,
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetInitContainers(podtest.MakeContainer("restartable-init", podtest.SetContainerRestartPolicy(containerRestartPolicyAlways))),
			podtest.SetRestartPolicy(core.RestartPolicyAlways),
			podtest.SetStatus(core.PodStatus{
				InitContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "restartable-init",
					Ready:       false,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{
							ContainerID: "docker://numbers",
							Reason:      "Completed",
						},
					},
				}},
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					ImageID:     "docker-pullable://nginx@sha256:d0gf00d",
					Name:        "nginx",
					Ready:       true,
					Started:     proto.Bool(true),
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			}),
		),
		"",
		"restartable init container can restart if RestartPolicyAlways",
	}, {
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				QOSClass: core.PodQOSBurstable,
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				QOSClass: core.PodQOSGuaranteed,
			}),
		),
		"tatus.qosClass: Invalid value: \"Burstable\": field is immutable",
		"qosClass can not be changed",
	}, {
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				QOSClass: core.PodQOSBurstable,
			}),
		),
		*podtest.MakePod("foo",
			podtest.SetStatus(core.PodStatus{
				QOSClass: core.PodQOSBurstable,
			}),
		),
		"",
		"qosClass no change",
	},
	}

	for _, test := range tests {
		test.new.ObjectMeta.ResourceVersion = "1"
		test.old.ObjectMeta.ResourceVersion = "1"
		errs := ValidatePodStatusUpdate(&test.new, &test.old, PodValidationOptions{})
		if test.err == "" {
			if len(errs) != 0 {
				t.Errorf("unexpected invalid: %s (%+v)\nA: %+v\nB: %+v", test.test, errs, test.new, test.old)
			}
		} else {
			if len(errs) == 0 {
				t.Errorf("unexpected valid: %s\nA: %+v\nB: %+v", test.test, test.new, test.old)
			} else if actualErr := errs.ToAggregate().Error(); !strings.Contains(actualErr, test.err) {
				t.Errorf("unexpected error message: %s\nExpected error: %s\nActual error: %s", test.test, test.err, actualErr)
			}
		}
	}
}

func makeValidService() core.Service {
	clusterInternalTrafficPolicy := core.ServiceInternalTrafficPolicyCluster
	return core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "valid",
			Namespace:       "valid",
			Labels:          map[string]string{},
			Annotations:     map[string]string{},
			ResourceVersion: "1",
		},
		Spec: core.ServiceSpec{
			Selector:              map[string]string{"key": "val"},
			SessionAffinity:       "None",
			Type:                  core.ServiceTypeClusterIP,
			Ports:                 []core.ServicePort{{Name: "p", Protocol: "TCP", Port: 8675, TargetPort: intstr.FromInt32(8675)}},
			InternalTrafficPolicy: &clusterInternalTrafficPolicy,
		},
	}
}

func TestValidatePodEphemeralContainersUpdate(t *testing.T) {
	makePod := func(ephemeralContainers []core.EphemeralContainer) *core.Pod {
		return podtest.MakePod("",
			podtest.SetObjectMeta(metav1.ObjectMeta{
				Annotations:     map[string]string{},
				Labels:          map[string]string{},
				Name:            "pod",
				Namespace:       "ns",
				ResourceVersion: "1",
			}),
			podtest.SetEphemeralContainers(ephemeralContainers...),
			podtest.SetRestartPolicy(core.RestartPolicyOnFailure),
		)
	}

	// Some tests use Windows host pods as an example of fields that might
	// conflict between an ephemeral container and the rest of the pod.
	capabilities.ResetForTest()
	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: true,
	})
	makeWindowsHostPod := func(ephemeralContainers []core.EphemeralContainer) *core.Pod {
		return podtest.MakePod("",
			podtest.SetObjectMeta(metav1.ObjectMeta{
				Annotations:     map[string]string{},
				Labels:          map[string]string{},
				Name:            "pod",
				Namespace:       "ns",
				ResourceVersion: "1",
			}),
			podtest.SetContainers(podtest.MakeContainer("cnt",
				podtest.SetContainerSecurityContext(core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: proto.Bool(true),
					}}),
			)),
			podtest.SetEphemeralContainers(ephemeralContainers...),
			podtest.SetRestartPolicy(core.RestartPolicyOnFailure),
			podtest.SetSecurityContext(&core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: proto.Bool(true),
				},
			}),
		)
	}

	tests := []struct {
		name     string
		new, old *core.Pod
		err      string
	}{{
		"no ephemeral containers",
		makePod([]core.EphemeralContainer{}),
		makePod([]core.EphemeralContainer{}),
		"",
	}, {
		"No change in Ephemeral Containers",
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		"",
	}, {
		"Ephemeral Container list order changes",
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		"",
	}, {
		"Add an Ephemeral Container",
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		makePod([]core.EphemeralContainer{}),
		"",
	}, {
		"Add two Ephemeral Containers",
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger1",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		makePod([]core.EphemeralContainer{}),
		"",
	}, {
		"Add to an existing Ephemeral Containers",
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		"",
	}, {
		"Add to an existing Ephemeral Containers, list order changes",
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger3",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		"",
	}, {
		"Remove an Ephemeral Container",
		makePod([]core.EphemeralContainer{}),
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		"may not be removed",
	}, {
		"Replace an Ephemeral Container",
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "firstone",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "thentheother",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		"may not be removed",
	}, {
		"Change an Ephemeral Containers",
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger1",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		makePod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger1",
				Image:                    "debian",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}, {
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:                     "debugger2",
				Image:                    "busybox",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
			},
		}}),
		"may not be changed",
	}, {
		"Ephemeral container with potential conflict with regular containers, but conflict not present",
		makeWindowsHostPod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:            "debugger1",
				Image:           "image",
				ImagePullPolicy: "IfNotPresent",
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: proto.Bool(true),
					},
				},
				TerminationMessagePolicy: "File",
			},
		}}),
		makeWindowsHostPod(nil),
		"",
	}, {
		"Ephemeral container with potential conflict with regular containers, and conflict is present",
		makeWindowsHostPod([]core.EphemeralContainer{{
			EphemeralContainerCommon: core.EphemeralContainerCommon{
				Name:            "debugger1",
				Image:           "image",
				ImagePullPolicy: "IfNotPresent",
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: proto.Bool(false),
					},
				},
				TerminationMessagePolicy: "File",
			},
		}}),
		makeWindowsHostPod(nil),
		"spec.ephemeralContainers[0].securityContext.windowsOptions.hostProcess: Invalid value: false: pod hostProcess value must be identical",
	}, {
		"Add ephemeral container to static pod",
		func() *core.Pod {
			p := makePod(nil)
			p.Spec.NodeName = "some-name"
			p.ObjectMeta.Annotations = map[string]string{
				core.MirrorPodAnnotationKey: "foo",
			}
			p.Spec.EphemeralContainers = []core.EphemeralContainer{{
				EphemeralContainerCommon: core.EphemeralContainerCommon{
					Name:                     "debugger1",
					Image:                    "debian",
					ImagePullPolicy:          "IfNotPresent",
					TerminationMessagePolicy: "File",
				},
			}}
			return p
		}(),
		func() *core.Pod {
			p := makePod(nil)
			p.Spec.NodeName = "some-name"
			p.ObjectMeta.Annotations = map[string]string{
				core.MirrorPodAnnotationKey: "foo",
			}
			return p
		}(),
		"Forbidden: static pods do not support ephemeral containers",
	},
	}

	for _, tc := range tests {
		errs := ValidatePodEphemeralContainersUpdate(tc.new, tc.old, PodValidationOptions{})
		if tc.err == "" {
			if len(errs) != 0 {
				t.Errorf("unexpected invalid for test: %s\nErrors returned: %+v\nLocal diff of test objects (-old +new):\n%s", tc.name, errs, cmp.Diff(tc.old, tc.new))
			}
		} else {
			if len(errs) == 0 {
				t.Errorf("unexpected valid for test: %s\nLocal diff of test objects (-old +new):\n%s", tc.name, cmp.Diff(tc.old, tc.new))
			} else if actualErr := errs.ToAggregate().Error(); !strings.Contains(actualErr, tc.err) {
				t.Errorf("unexpected error message: %s\nExpected error: %s\nActual error: %s", tc.name, tc.err, actualErr)
			}
		}
	}
}

func TestValidateServiceCreate(t *testing.T) {
	requireDualStack := core.IPFamilyPolicyRequireDualStack
	singleStack := core.IPFamilyPolicySingleStack
	preferDualStack := core.IPFamilyPolicyPreferDualStack

	testCases := []struct {
		name         string
		tweakSvc     func(svc *core.Service) // given a basic valid service, each test case can customize it
		numErrs      int
		featureGates []featuregate.Feature
	}{{
		name: "missing namespace",
		tweakSvc: func(s *core.Service) {
			s.Namespace = ""
		},
		numErrs: 1,
	}, {
		name: "invalid namespace",
		tweakSvc: func(s *core.Service) {
			s.Namespace = "-123"
		},
		numErrs: 1,
	}, {
		name: "missing name",
		tweakSvc: func(s *core.Service) {
			s.Name = ""
		},
		numErrs: 1,
	}, {
		name: "invalid name",
		tweakSvc: func(s *core.Service) {
			s.Name = "-123"
		},
		numErrs: 1,
	}, {
		name: "too long name",
		tweakSvc: func(s *core.Service) {
			s.Name = strings.Repeat("a", 64)
		},
		numErrs: 1,
	}, {
		name: "invalid generateName",
		tweakSvc: func(s *core.Service) {
			s.GenerateName = "-123"
		},
		numErrs: 1,
	}, {
		name: "too long generateName",
		tweakSvc: func(s *core.Service) {
			s.GenerateName = strings.Repeat("a", 64)
		},
		numErrs: 1,
	}, {
		name: "invalid label",
		tweakSvc: func(s *core.Service) {
			s.Labels["NoUppercaseOrSpecialCharsLike=Equals"] = "bar"
		},
		numErrs: 1,
	}, {
		name: "invalid annotation",
		tweakSvc: func(s *core.Service) {
			s.Annotations["NoSpecialCharsLike=Equals"] = "bar"
		},
		numErrs: 1,
	}, {
		name: "nil selector",
		tweakSvc: func(s *core.Service) {
			s.Spec.Selector = nil
		},
		numErrs: 0,
	}, {
		name: "invalid selector",
		tweakSvc: func(s *core.Service) {
			s.Spec.Selector["NoSpecialCharsLike=Equals"] = "bar"
		},
		numErrs: 1,
	}, {
		name: "missing session affinity",
		tweakSvc: func(s *core.Service) {
			s.Spec.SessionAffinity = ""
		},
		numErrs: 1,
	}, {
		name: "missing type",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = ""
		},
		numErrs: 1,
	}, {
		name: "missing ports",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports = nil
		},
		numErrs: 1,
	}, {
		name: "missing ports but headless",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports = nil
			s.Spec.ClusterIP = core.ClusterIPNone
			s.Spec.ClusterIPs = []string{core.ClusterIPNone}
		},
		numErrs: 0,
	}, {
		name: "empty port[0] name",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Name = ""
		},
		numErrs: 0,
	}, {
		name: "empty port[1] name",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "", Protocol: "TCP", Port: 12345, TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 1,
	}, {
		name: "empty multi-port port[0] name",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Name = ""
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "p", Protocol: "TCP", Port: 12345, TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 1,
	}, {
		name: "invalid port name",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Name = "INVALID"
		},
		numErrs: 1,
	}, {
		name: "missing protocol",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Protocol = ""
		},
		numErrs: 1,
	}, {
		name: "invalid protocol",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Protocol = "INVALID"
		},
		numErrs: 1,
	}, {
		name: "invalid cluster ip",
		tweakSvc: func(s *core.Service) {
			s.Spec.ClusterIP = "invalid"
			s.Spec.ClusterIPs = []string{"invalid"}
		},
		numErrs: 1,
	}, {
		name: "missing port",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Port = 0
		},
		numErrs: 1,
	}, {
		name: "invalid port",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Port = 65536
		},
		numErrs: 1,
	}, {
		name: "invalid TargetPort int",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].TargetPort = intstr.FromInt32(65536)
		},
		numErrs: 1,
	}, {
		name: "valid port headless",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Port = 11722
			s.Spec.Ports[0].TargetPort = intstr.FromInt32(11722)
			s.Spec.ClusterIP = core.ClusterIPNone
			s.Spec.ClusterIPs = []string{core.ClusterIPNone}
		},
		numErrs: 0,
	}, {
		name: "invalid port headless 1",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Port = 11722
			s.Spec.Ports[0].TargetPort = intstr.FromInt32(11721)
			s.Spec.ClusterIP = core.ClusterIPNone
			s.Spec.ClusterIPs = []string{core.ClusterIPNone}
		},
		// in the v1 API, targetPorts on headless services were tolerated.
		// once we have version-specific validation, we can reject this on newer API versions, but until then, we have to tolerate it for compatibility.
		// numErrs: 1,
		numErrs: 0,
	}, {
		name: "invalid port headless 2",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Port = 11722
			s.Spec.Ports[0].TargetPort = intstr.FromString("target")
			s.Spec.ClusterIP = core.ClusterIPNone
			s.Spec.ClusterIPs = []string{core.ClusterIPNone}
		},
		// in the v1 API, targetPorts on headless services were tolerated.
		// once we have version-specific validation, we can reject this on newer API versions, but until then, we have to tolerate it for compatibility.
		// numErrs: 1,
		numErrs: 0,
	}, {
		name: "invalid publicIPs localhost",
		tweakSvc: func(s *core.Service) {
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.ExternalIPs = []string{"127.0.0.1"}
		},
		numErrs: 1,
	}, {
		name: "invalid publicIPs unspecified",
		tweakSvc: func(s *core.Service) {
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.ExternalIPs = []string{"0.0.0.0"}
		},
		numErrs: 1,
	}, {
		name: "invalid publicIPs loopback",
		tweakSvc: func(s *core.Service) {
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.ExternalIPs = []string{"127.0.0.1"}
		},
		numErrs: 1,
	}, {
		name: "invalid publicIPs host",
		tweakSvc: func(s *core.Service) {
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.ExternalIPs = []string{"myhost.mydomain"}
		},
		numErrs: 1,
	}, {
		name: "valid publicIPs",
		tweakSvc: func(s *core.Service) {
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.ExternalIPs = []string{"1.2.3.4"}
		},
		numErrs: 0,
	}, {
		name: "dup port name",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Name = "p"
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "p", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 1,
	}, {
		name: "valid load balancer protocol UDP 1",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.Ports[0].Protocol = "UDP"
		},
		numErrs: 0,
	}, {
		name: "valid load balancer protocol UDP 2",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.Ports[0] = core.ServicePort{Name: "q", Port: 12345, Protocol: "UDP", TargetPort: intstr.FromInt32(12345)}
		},
		numErrs: 0,
	}, {
		name: "load balancer with mix protocol",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "UDP", TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		name: "valid 1",
		tweakSvc: func(s *core.Service) {
			// do nothing
		},
		numErrs: 0,
	}, {
		name: "valid 2",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].Protocol = "UDP"
			s.Spec.Ports[0].TargetPort = intstr.FromInt32(12345)
		},
		numErrs: 0,
	}, {
		name: "valid 3",
		tweakSvc: func(s *core.Service) {
			s.Spec.Ports[0].TargetPort = intstr.FromString("http")
		},
		numErrs: 0,
	}, {
		name: "valid cluster ip - none ",
		tweakSvc: func(s *core.Service) {
			s.Spec.ClusterIP = core.ClusterIPNone
			s.Spec.ClusterIPs = []string{core.ClusterIPNone}
		},
		numErrs: 0,
	}, {
		name: "valid cluster ip - empty",
		tweakSvc: func(s *core.Service) {
			s.Spec.ClusterIPs = nil
			s.Spec.Ports[0].TargetPort = intstr.FromString("http")
		},
		numErrs: 0,
	}, {
		name: "valid type - clusterIP",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeClusterIP
		},
		numErrs: 0,
	}, {
		name: "valid type - loadbalancer",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
		},
		numErrs: 0,
	}, {
		name: "valid type - loadbalancer with allocateLoadBalancerNodePorts=false",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(false)
		},
		numErrs: 0,
	}, {
		name: "invalid type - missing AllocateLoadBalancerNodePorts for loadbalancer type",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
		},
		numErrs: 1,
	}, {
		name: "valid type loadbalancer 2 ports",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		name: "valid external load balancer 2 ports",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		name: "duplicate nodeports",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 1, Protocol: "TCP", NodePort: 1, TargetPort: intstr.FromInt32(1)})
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "r", Port: 2, Protocol: "TCP", NodePort: 1, TargetPort: intstr.FromInt32(2)})
		},
		numErrs: 1,
	}, {
		name: "duplicate nodeports (different protocols)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 1, Protocol: "TCP", NodePort: 1, TargetPort: intstr.FromInt32(1)})
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "r", Port: 2, Protocol: "UDP", NodePort: 1, TargetPort: intstr.FromInt32(2)})
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "s", Port: 3, Protocol: "SCTP", NodePort: 1, TargetPort: intstr.FromInt32(3)})
		},
		numErrs: 0,
	}, {
		name: "invalid duplicate ports (with same protocol)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeClusterIP
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(8080)})
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "r", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(80)})
		},
		numErrs: 1,
	}, {
		name: "valid duplicate ports (with different protocols)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeClusterIP
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(8080)})
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "r", Port: 12345, Protocol: "UDP", TargetPort: intstr.FromInt32(80)})
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "s", Port: 12345, Protocol: "SCTP", TargetPort: intstr.FromInt32(8088)})
		},
		numErrs: 0,
	}, {
		name: "valid type - cluster",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeClusterIP
		},
		numErrs: 0,
	}, {
		name: "valid type - nodeport",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
		},
		numErrs: 0,
	}, {
		name: "valid type - loadbalancer with allocateLoadBalancerNodePorts=true",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
		},
		numErrs: 0,
	}, {
		name: "valid type loadbalancer 2 ports",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		name: "valid type loadbalancer with NodePort",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", NodePort: 12345, TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		name: "valid type=NodePort service with NodePort",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", NodePort: 12345, TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		name: "valid type=NodePort service without NodePort",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		name: "valid cluster service without NodePort",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeClusterIP
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		name: "invalid cluster service with NodePort",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeClusterIP
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", NodePort: 12345, TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 1,
	}, {
		name: "invalid public service with duplicate NodePort",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "p1", Port: 1, Protocol: "TCP", NodePort: 1, TargetPort: intstr.FromInt32(1)})
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "p2", Port: 2, Protocol: "TCP", NodePort: 1, TargetPort: intstr.FromInt32(2)})
		},
		numErrs: 1,
	}, {
		name: "valid type=LoadBalancer",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 12345, Protocol: "TCP", TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		// Remove the limitation on exposing port 10250 externally
		name: "valid port type=LoadBalancer",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "kubelet", Port: 10250, Protocol: "TCP", TargetPort: intstr.FromInt32(12345)})
		},
		numErrs: 0,
	}, {
		name: "valid LoadBalancer source range annotation",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Annotations[core.AnnotationLoadBalancerSourceRangesKey] = "1.2.3.0/24,  5.6.0.0/16"
		},
		numErrs: 0,
	}, {
		name: "valid empty LoadBalancer source range annotation",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Annotations[core.AnnotationLoadBalancerSourceRangesKey] = ""
		},
		numErrs: 0,
	}, {
		name: "valid whitespace-only LoadBalancer source range annotation",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Annotations[core.AnnotationLoadBalancerSourceRangesKey] = "  "
		},
		numErrs: 0,
	}, {
		name: "invalid LoadBalancer source range annotation (hostname)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Annotations[core.AnnotationLoadBalancerSourceRangesKey] = "foo.bar"
		},
		numErrs: 1,
	}, {
		name: "invalid LoadBalancer source range annotation (invalid CIDR)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Annotations[core.AnnotationLoadBalancerSourceRangesKey] = "1.2.3.4/33"
		},
		numErrs: 1,
	}, {
		name: "invalid LoadBalancer source range annotation for non LoadBalancer type service",
		tweakSvc: func(s *core.Service) {
			s.Annotations[core.AnnotationLoadBalancerSourceRangesKey] = "1.2.3.0/24"
		},
		numErrs: 1,
	}, {
		name: "invalid empty-but-set LoadBalancer source range annotation for non LoadBalancer type service",
		tweakSvc: func(s *core.Service) {
			s.Annotations[core.AnnotationLoadBalancerSourceRangesKey] = ""
		},
		numErrs: 1,
	}, {
		name: "valid LoadBalancer source range",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.LoadBalancerSourceRanges = []string{"1.2.3.0/24", "5.6.0.0/16"}
		},
		numErrs: 0,
	}, {
		name: "valid LoadBalancer source range with whitespace",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.LoadBalancerSourceRanges = []string{"1.2.3.0/24  ", " 5.6.0.0/16"}
		},
		numErrs: 0,
	}, {
		name: "invalid empty LoadBalancer source range",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.LoadBalancerSourceRanges = []string{"   "}
		},
		numErrs: 1,
	}, {
		name: "invalid LoadBalancer source range (hostname)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.LoadBalancerSourceRanges = []string{"foo.bar"}
		},
		numErrs: 1,
	}, {
		name: "invalid LoadBalancer source range (invalid CIDR)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.LoadBalancerSourceRanges = []string{"1.2.3.4/33"}
		},
		numErrs: 1,
	}, {
		name: "invalid source range for non LoadBalancer type service",
		tweakSvc: func(s *core.Service) {
			s.Spec.LoadBalancerSourceRanges = []string{"1.2.3.0/24", "5.6.0.0/16"}
		},
		numErrs: 1,
	}, {
		name: "invalid source range annotation ignored with valid source range field",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Annotations[core.AnnotationLoadBalancerSourceRangesKey] = "foo.bar"
			s.Spec.LoadBalancerSourceRanges = []string{"1.2.3.0/24", "5.6.0.0/16"}
		},
		numErrs: 0,
	}, {
		name: "valid ExternalName",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeExternalName
			s.Spec.ExternalName = "foo.bar.example.com"
		},
		numErrs: 0,
	}, {
		name: "valid ExternalName (trailing dot)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeExternalName
			s.Spec.ExternalName = "foo.bar.example.com."
		},
		numErrs: 0,
	}, {
		name: "invalid ExternalName clusterIP (valid IP)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeExternalName
			s.Spec.ClusterIP = "1.2.3.4"
			s.Spec.ClusterIPs = []string{"1.2.3.4"}
			s.Spec.ExternalName = "foo.bar.example.com"
		},
		numErrs: 1,
	}, {
		name: "invalid ExternalName clusterIP (None)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeExternalName
			s.Spec.ClusterIP = "None"
			s.Spec.ClusterIPs = []string{"None"}
			s.Spec.ExternalName = "foo.bar.example.com"
		},
		numErrs: 1,
	}, {
		name: "invalid ExternalName (not a DNS name)",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeExternalName
			s.Spec.ExternalName = "-123"
		},
		numErrs: 1,
	}, {
		name: "LoadBalancer type cannot have None ClusterIP",
		tweakSvc: func(s *core.Service) {
			s.Spec.ClusterIP = "None"
			s.Spec.ClusterIPs = []string{"None"}
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
		},
		numErrs: 1,
	}, {
		name: "invalid node port with clusterIP None",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.Ports = append(s.Spec.Ports, core.ServicePort{Name: "q", Port: 1, Protocol: "TCP", NodePort: 1, TargetPort: intstr.FromInt32(1)})
			s.Spec.ClusterIP = "None"
			s.Spec.ClusterIPs = []string{"None"}
		},
		numErrs: 1,
	},
		// ESIPP section begins.
		{
			name: "invalid externalTraffic field",
			tweakSvc: func(s *core.Service) {
				s.Spec.Type = core.ServiceTypeLoadBalancer
				s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				s.Spec.ExternalTrafficPolicy = "invalid"
			},
			numErrs: 1,
		}, {
			name: "nil internalTraffic field when feature gate is on",
			tweakSvc: func(s *core.Service) {
				s.Spec.InternalTrafficPolicy = nil
			},
			numErrs: 1,
		}, {
			name: "internalTrafficPolicy field nil when type is ExternalName",
			tweakSvc: func(s *core.Service) {
				s.Spec.InternalTrafficPolicy = nil
				s.Spec.Type = core.ServiceTypeExternalName
				s.Spec.ExternalName = "foo.bar.com"
			},
			numErrs: 0,
		}, {
			// Typically this should fail validation, but in v1.22 we have existing clusters
			// that may have allowed internalTrafficPolicy when Type=ExternalName.
			// This test case ensures we don't break compatibility for internalTrafficPolicy
			// when Type=ExternalName
			name: "internalTrafficPolicy field is set when type is ExternalName",
			tweakSvc: func(s *core.Service) {
				cluster := core.ServiceInternalTrafficPolicyCluster
				s.Spec.InternalTrafficPolicy = &cluster
				s.Spec.Type = core.ServiceTypeExternalName
				s.Spec.ExternalName = "foo.bar.com"
			},
			numErrs: 0,
		}, {
			name: "invalid internalTraffic field",
			tweakSvc: func(s *core.Service) {
				invalid := core.ServiceInternalTrafficPolicy("invalid")
				s.Spec.InternalTrafficPolicy = &invalid
			},
			numErrs: 1,
		}, {
			name: "internalTrafficPolicy field set to Cluster",
			tweakSvc: func(s *core.Service) {
				cluster := core.ServiceInternalTrafficPolicyCluster
				s.Spec.InternalTrafficPolicy = &cluster
			},
			numErrs: 0,
		}, {
			name: "internalTrafficPolicy field set to Local",
			tweakSvc: func(s *core.Service) {
				local := core.ServiceInternalTrafficPolicyLocal
				s.Spec.InternalTrafficPolicy = &local
			},
			numErrs: 0,
		}, {
			name: "negative healthCheckNodePort field",
			tweakSvc: func(s *core.Service) {
				s.Spec.Type = core.ServiceTypeLoadBalancer
				s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyLocal
				s.Spec.HealthCheckNodePort = -1
			},
			numErrs: 1,
		}, {
			name: "negative healthCheckNodePort field",
			tweakSvc: func(s *core.Service) {
				s.Spec.Type = core.ServiceTypeLoadBalancer
				s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyLocal
				s.Spec.HealthCheckNodePort = 31100
			},
			numErrs: 0,
		},
		// ESIPP section ends.
		{
			name: "invalid timeoutSeconds field",
			tweakSvc: func(s *core.Service) {
				s.Spec.Type = core.ServiceTypeClusterIP
				s.Spec.SessionAffinity = core.ServiceAffinityClientIP
				s.Spec.SessionAffinityConfig = &core.SessionAffinityConfig{
					ClientIP: &core.ClientIPConfig{
						TimeoutSeconds: utilpointer.Int32(-1),
					},
				}
			},
			numErrs: 1,
		}, {
			name: "sessionAffinityConfig can't be set when session affinity is None",
			tweakSvc: func(s *core.Service) {
				s.Spec.Type = core.ServiceTypeLoadBalancer
				s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				s.Spec.SessionAffinity = core.ServiceAffinityNone
				s.Spec.SessionAffinityConfig = &core.SessionAffinityConfig{
					ClientIP: &core.ClientIPConfig{
						TimeoutSeconds: utilpointer.Int32(90),
					},
				}
			},
			numErrs: 1,
		},
		/* ip families validation */
		{
			name: "invalid, service with invalid ipFamilies",
			tweakSvc: func(s *core.Service) {
				invalidServiceIPFamily := core.IPFamily("not-a-valid-ip-family")
				s.Spec.IPFamilies = []core.IPFamily{invalidServiceIPFamily}
			},
			numErrs: 1,
		}, {
			name: "invalid, service with invalid ipFamilies (2nd)",
			tweakSvc: func(s *core.Service) {
				invalidServiceIPFamily := core.IPFamily("not-a-valid-ip-family")
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, invalidServiceIPFamily}
			},
			numErrs: 1,
		}, {
			name: "IPFamilyPolicy(singleStack) is set for two families",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &singleStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0, // this validated in alloc code.
		}, {
			name: "valid, IPFamilyPolicy(preferDualStack) is set for two families (note: alloc sets families)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &preferDualStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0,
		},

		{
			name: "invalid, service with 2+ ipFamilies",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol, core.IPv4Protocol}
			},
			numErrs: 1,
		}, {
			name: "invalid, service with same ip families",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol, core.IPv6Protocol}
			},
			numErrs: 1,
		}, {
			name: "valid, nil service ipFamilies",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilies = nil
			},
			numErrs: 0,
		}, {
			name: "valid, service with valid ipFamilies (v4)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, service with valid ipFamilies (v6)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, service with valid ipFamilies(v4,v6)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, service with valid ipFamilies(v6,v4)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol, core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, service preferred dual stack with single family",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &preferDualStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol}
			},
			numErrs: 0,
		},
		/* cluster IPs. some tests are redundant */
		{
			name: "invalid, garbage single ip",
			tweakSvc: func(s *core.Service) {
				s.Spec.ClusterIP = "garbage-ip"
				s.Spec.ClusterIPs = []string{"garbage-ip"}
			},
			numErrs: 1,
		}, {
			name: "invalid, garbage ips",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "garbage-ip"
				s.Spec.ClusterIPs = []string{"garbage-ip", "garbage-second-ip"}
			},
			numErrs: 2,
		}, {
			name: "invalid, garbage first ip",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "garbage-ip"
				s.Spec.ClusterIPs = []string{"garbage-ip", "2001::1"}
			},
			numErrs: 1,
		}, {
			name: "invalid, garbage second ip",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "2001::1"
				s.Spec.ClusterIPs = []string{"2001::1", "garbage-ip"}
			},
			numErrs: 1,
		}, {
			name: "invalid, NONE + IP",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "None"
				s.Spec.ClusterIPs = []string{"None", "2001::1"}
			},
			numErrs: 1,
		}, {
			name: "invalid, IP + NONE",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "2001::1"
				s.Spec.ClusterIPs = []string{"2001::1", "None"}
			},
			numErrs: 1,
		}, {
			name: "invalid, EMPTY STRING + IP",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = ""
				s.Spec.ClusterIPs = []string{"", "2001::1"}
			},
			numErrs: 2,
		}, {
			name: "invalid, IP + EMPTY STRING",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "2001::1"
				s.Spec.ClusterIPs = []string{"2001::1", ""}
			},
			numErrs: 1,
		}, {
			name: "invalid, same ip family (v6)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "2001::1"
				s.Spec.ClusterIPs = []string{"2001::1", "2001::4"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 2,
		}, {
			name: "invalid, same ip family (v4)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1", "10.0.0.10"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

			},
			numErrs: 2,
		}, {
			name: "invalid, more than two ips",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1", "2001::1", "10.0.0.10"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 1,
		}, {
			name: " multi ip, dualstack not set (request for downgrade)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &singleStack
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1", "2001::1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, headless-no-selector + multi family + gate off",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "None"
				s.Spec.ClusterIPs = []string{"None"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
				s.Spec.Selector = nil
			},
			numErrs: 0,
		}, {
			name: "valid, multi ip, single ipfamilies preferDualStack",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &preferDualStack
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1", "2001::1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		},

		{
			name: "valid, multi ip, single ipfamilies (must match when provided) + requireDualStack",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1", "2001::1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "invalid, families don't match (v4=>v6)",
			tweakSvc: func(s *core.Service) {
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol}
			},
			numErrs: 1,
		}, {
			name: "invalid, families don't match (v6=>v4)",
			tweakSvc: func(s *core.Service) {
				s.Spec.ClusterIP = "2001::1"
				s.Spec.ClusterIPs = []string{"2001::1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 1,
		}, {
			name: "valid. no field set",
			tweakSvc: func(s *core.Service) {
			},
			numErrs: 0,
		},

		{
			name: "valid, single ip",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &singleStack
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1"}
			},
			numErrs: 0,
		}, {
			name: "valid, single family",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &singleStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol}

			},
			numErrs: 0,
		}, {
			name: "valid, single ip + single family",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &singleStack
				s.Spec.ClusterIP = "2001::1"
				s.Spec.ClusterIPs = []string{"2001::1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol}

			},
			numErrs: 0,
		}, {
			name: "valid, single ip + single family (dual stack requested)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &preferDualStack
				s.Spec.ClusterIP = "2001::1"
				s.Spec.ClusterIPs = []string{"2001::1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol}

			},
			numErrs: 0,
		}, {
			name: "valid, single ip, multi ipfamilies",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, multi ips, multi ipfamilies (4,6)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1", "2001::1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, ips, multi ipfamilies (6,4)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "2001::1"
				s.Spec.ClusterIPs = []string{"2001::1", "10.0.0.1"}
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol, core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, multi ips (6,4)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "2001::1"
				s.Spec.ClusterIPs = []string{"2001::1", "10.0.0.1"}
			},
			numErrs: 0,
		}, {
			name: "valid, multi ipfamilies (6,4)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol, core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, multi ips (4,6)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "10.0.0.1"
				s.Spec.ClusterIPs = []string{"10.0.0.1", "2001::1"}
			},
			numErrs: 0,
		}, {
			name: "valid,  multi ipfamilies (4,6)",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid, dual stack",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
			},
			numErrs: 0,
		},

		{
			name: `valid appProtocol`,
			tweakSvc: func(s *core.Service) {
				s.Spec.Ports = []core.ServicePort{{
					Port:        12345,
					TargetPort:  intstr.FromInt32(12345),
					Protocol:    "TCP",
					AppProtocol: utilpointer.String("HTTP"),
				}}
			},
			numErrs: 0,
		}, {
			name: `valid custom appProtocol`,
			tweakSvc: func(s *core.Service) {
				s.Spec.Ports = []core.ServicePort{{
					Port:        12345,
					TargetPort:  intstr.FromInt32(12345),
					Protocol:    "TCP",
					AppProtocol: utilpointer.String("example.com/protocol"),
				}}
			},
			numErrs: 0,
		}, {
			name: `invalid appProtocol`,
			tweakSvc: func(s *core.Service) {
				s.Spec.Ports = []core.ServicePort{{
					Port:        12345,
					TargetPort:  intstr.FromInt32(12345),
					Protocol:    "TCP",
					AppProtocol: utilpointer.String("example.com/protocol_with{invalid}[characters]"),
				}}
			},
			numErrs: 1,
		},

		{
			name: "invalid cluster ip != clusterIP in multi ip service",
			tweakSvc: func(s *core.Service) {
				s.Spec.IPFamilyPolicy = &requireDualStack
				s.Spec.ClusterIP = "10.0.0.10"
				s.Spec.ClusterIPs = []string{"10.0.0.1", "2001::1"}
			},
			numErrs: 1,
		}, {
			name: "invalid cluster ip != clusterIP in single ip service",
			tweakSvc: func(s *core.Service) {
				s.Spec.ClusterIP = "10.0.0.10"
				s.Spec.ClusterIPs = []string{"10.0.0.1"}
			},
			numErrs: 1,
		}, {
			name: "Use AllocateLoadBalancerNodePorts when type is not LoadBalancer",
			tweakSvc: func(s *core.Service) {
				s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			},
			numErrs: 1,
		}, {
			name: "valid LoadBalancerClass when type is LoadBalancer",
			tweakSvc: func(s *core.Service) {
				s.Spec.Type = core.ServiceTypeLoadBalancer
				s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				s.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")
			},
			numErrs: 0,
		}, {
			name: "invalid LoadBalancerClass",
			tweakSvc: func(s *core.Service) {
				s.Spec.Type = core.ServiceTypeLoadBalancer
				s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				s.Spec.LoadBalancerClass = utilpointer.String("Bad/LoadBalancerClass")
			},
			numErrs: 1,
		}, {
			name: "invalid: set LoadBalancerClass when type is not LoadBalancer",
			tweakSvc: func(s *core.Service) {
				s.Spec.Type = core.ServiceTypeClusterIP
				s.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")
			},
			numErrs: 1,
		}, {
			name: "topology annotations are mismatched",
			tweakSvc: func(s *core.Service) {
				s.Annotations[core.DeprecatedAnnotationTopologyAwareHints] = "original"
				s.Annotations[core.AnnotationTopologyMode] = "different"
			},
			numErrs: 1,
		}, {
			name: "valid: trafficDistribution field set to PreferClose",
			tweakSvc: func(s *core.Service) {
				s.Spec.TrafficDistribution = utilpointer.String("PreferClose")
			},
			numErrs: 0,
		}, {
			name: "invalid: trafficDistribution field set to Random",
			tweakSvc: func(s *core.Service) {
				s.Spec.TrafficDistribution = utilpointer.String("Random")
			},
			numErrs: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.featureGates {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, tc.featureGates[i], true)
			}
			svc := makeValidService()
			tc.tweakSvc(&svc)
			errs := ValidateServiceCreate(&svc)
			if len(errs) != tc.numErrs {
				t.Errorf("Unexpected error list for case %q(expected:%v got %v) - Errors:\n %v", tc.name, tc.numErrs, len(errs), errs.ToAggregate())
			}
		})
	}
}

func TestValidateServiceExternalTrafficPolicy(t *testing.T) {
	testCases := []struct {
		name     string
		tweakSvc func(svc *core.Service) // Given a basic valid service, each test case can customize it.
		numErrs  int
	}{{
		name: "valid loadBalancer service with externalTrafficPolicy and healthCheckNodePort set",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyLocal
			s.Spec.HealthCheckNodePort = 34567
		},
		numErrs: 0,
	}, {
		name: "valid nodePort service with externalTrafficPolicy set",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyLocal
		},
		numErrs: 0,
	}, {
		name: "valid clusterIP service with none of externalTrafficPolicy and healthCheckNodePort set",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeClusterIP
		},
		numErrs: 0,
	}, {
		name: "cannot set healthCheckNodePort field on loadBalancer service with externalTrafficPolicy!=Local",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
			s.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			s.Spec.HealthCheckNodePort = 34567
		},
		numErrs: 1,
	}, {
		name: "cannot set healthCheckNodePort field on nodePort service",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyLocal
			s.Spec.HealthCheckNodePort = 34567
		},
		numErrs: 1,
	}, {
		name: "cannot set externalTrafficPolicy or healthCheckNodePort fields on clusterIP service",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeClusterIP
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyLocal
			s.Spec.HealthCheckNodePort = 34567
		},
		numErrs: 2,
	}, {
		name: "cannot set externalTrafficPolicy field on ExternalName service",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeExternalName
			s.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyLocal
		},
		numErrs: 1,
	}, {
		name: "externalTrafficPolicy is required on NodePort service",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeNodePort
		},
		numErrs: 1,
	}, {
		name: "externalTrafficPolicy is required on LoadBalancer service",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeLoadBalancer
		},
		numErrs: 1,
	}, {
		name: "externalTrafficPolicy is required on ClusterIP service with externalIPs",
		tweakSvc: func(s *core.Service) {
			s.Spec.Type = core.ServiceTypeClusterIP
			s.Spec.ExternalIPs = []string{"1.2.3,4"}
		},
		numErrs: 1,
	},
	}

	for _, tc := range testCases {
		svc := makeValidService()
		tc.tweakSvc(&svc)
		errs := validateServiceExternalTrafficPolicy(&svc)
		if len(errs) != tc.numErrs {
			t.Errorf("Unexpected error list for case %q: %v", tc.name, errs.ToAggregate())
		}
	}
}

func TestValidateReplicationControllerStatus(t *testing.T) {
	tests := []struct {
		name string

		replicas             int32
		fullyLabeledReplicas int32
		readyReplicas        int32
		availableReplicas    int32
		observedGeneration   int64

		expectedErr bool
	}{{
		name:                 "valid status",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        2,
		availableReplicas:    1,
		observedGeneration:   2,
		expectedErr:          false,
	}, {
		name:                 "invalid replicas",
		replicas:             -1,
		fullyLabeledReplicas: 3,
		readyReplicas:        2,
		availableReplicas:    1,
		observedGeneration:   2,
		expectedErr:          true,
	}, {
		name:                 "invalid fullyLabeledReplicas",
		replicas:             3,
		fullyLabeledReplicas: -1,
		readyReplicas:        2,
		availableReplicas:    1,
		observedGeneration:   2,
		expectedErr:          true,
	}, {
		name:                 "invalid readyReplicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        -1,
		availableReplicas:    1,
		observedGeneration:   2,
		expectedErr:          true,
	}, {
		name:                 "invalid availableReplicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        3,
		availableReplicas:    -1,
		observedGeneration:   2,
		expectedErr:          true,
	}, {
		name:                 "invalid observedGeneration",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        3,
		availableReplicas:    3,
		observedGeneration:   -1,
		expectedErr:          true,
	}, {
		name:                 "fullyLabeledReplicas greater than replicas",
		replicas:             3,
		fullyLabeledReplicas: 4,
		readyReplicas:        3,
		availableReplicas:    3,
		observedGeneration:   1,
		expectedErr:          true,
	}, {
		name:                 "readyReplicas greater than replicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        4,
		availableReplicas:    3,
		observedGeneration:   1,
		expectedErr:          true,
	}, {
		name:                 "availableReplicas greater than replicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        3,
		availableReplicas:    4,
		observedGeneration:   1,
		expectedErr:          true,
	}, {
		name:                 "availableReplicas greater than readyReplicas",
		replicas:             3,
		fullyLabeledReplicas: 3,
		readyReplicas:        2,
		availableReplicas:    3,
		observedGeneration:   1,
		expectedErr:          true,
	},
	}

	for _, test := range tests {
		status := core.ReplicationControllerStatus{
			Replicas:             test.replicas,
			FullyLabeledReplicas: test.fullyLabeledReplicas,
			ReadyReplicas:        test.readyReplicas,
			AvailableReplicas:    test.availableReplicas,
			ObservedGeneration:   test.observedGeneration,
		}

		if hasErr := len(ValidateReplicationControllerStatus(status, field.NewPath("status"))) > 0; hasErr != test.expectedErr {
			t.Errorf("%s: expected error: %t, got error: %t", test.name, test.expectedErr, hasErr)
		}
	}
}

func TestValidateReplicationControllerStatusUpdate(t *testing.T) {
	validSelector := map[string]string{"a": "b"}
	validPodTemplate := core.PodTemplate{
		Template: core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: core.PodSpec{
				RestartPolicy: core.RestartPolicyAlways,
				DNSPolicy:     core.DNSClusterFirst,
				Containers:    []core.Container{{Name: "abc", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}},
			},
		},
	}
	type rcUpdateTest struct {
		old    core.ReplicationController
		update core.ReplicationController
	}
	successCases := []rcUpdateTest{{
		old: core.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
				Template: &validPodTemplate.Template,
			},
			Status: core.ReplicationControllerStatus{
				Replicas: 2,
			},
		},
		update: core.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Replicas: 3,
				Selector: validSelector,
				Template: &validPodTemplate.Template,
			},
			Status: core.ReplicationControllerStatus{
				Replicas: 4,
			},
		},
	},
	}
	for _, successCase := range successCases {
		successCase.old.ObjectMeta.ResourceVersion = "1"
		successCase.update.ObjectMeta.ResourceVersion = "1"
		if errs := ValidateReplicationControllerStatusUpdate(&successCase.update, &successCase.old); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
	errorCases := map[string]rcUpdateTest{
		"negative replicas": {
			old: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Selector: validSelector,
					Template: &validPodTemplate.Template,
				},
				Status: core.ReplicationControllerStatus{
					Replicas: 3,
				},
			},
			update: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Replicas: 2,
					Selector: validSelector,
					Template: &validPodTemplate.Template,
				},
				Status: core.ReplicationControllerStatus{
					Replicas: -3,
				},
			},
		},
	}
	for testName, errorCase := range errorCases {
		if errs := ValidateReplicationControllerStatusUpdate(&errorCase.update, &errorCase.old); len(errs) == 0 {
			t.Errorf("expected failure: %s", testName)
		}
	}

}

func TestValidateReplicationControllerUpdate(t *testing.T) {
	validSelector := map[string]string{"a": "b"}
	validPodTemplate := core.PodTemplate{
		Template: core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: podtest.MakePodSpec(),
		},
	}
	readWriteVolumePodTemplate := core.PodTemplate{
		Template: core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: podtest.MakePodSpec(
				podtest.SetVolumes(core.Volume{Name: "gcepd", VolumeSource: core.VolumeSource{GCEPersistentDisk: &core.GCEPersistentDiskVolumeSource{PDName: "my-PD", FSType: "ext4", Partition: 1, ReadOnly: false}}}),
			),
		},
	}
	invalidSelector := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}
	invalidPodTemplate := core.PodTemplate{
		Template: core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: invalidSelector,
			},
			Spec: podtest.MakePodSpec(),
		},
	}
	type rcUpdateTest struct {
		old    core.ReplicationController
		update core.ReplicationController
	}
	successCases := []rcUpdateTest{{
		old: core.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
				Template: &validPodTemplate.Template,
			},
		},
		update: core.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Replicas: 3,
				Selector: validSelector,
				Template: &validPodTemplate.Template,
			},
		},
	}, {
		old: core.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
				Template: &validPodTemplate.Template,
			},
		},
		update: core.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Replicas: 1,
				Selector: validSelector,
				Template: &readWriteVolumePodTemplate.Template,
			},
		},
	},
	}
	for _, successCase := range successCases {
		successCase.old.ObjectMeta.ResourceVersion = "1"
		successCase.update.ObjectMeta.ResourceVersion = "1"
		if errs := ValidateReplicationControllerUpdate(&successCase.update, &successCase.old, PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
	errorCases := map[string]rcUpdateTest{
		"more than one read/write": {
			old: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Selector: validSelector,
					Template: &validPodTemplate.Template,
				},
			},
			update: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Replicas: 2,
					Selector: validSelector,
					Template: &readWriteVolumePodTemplate.Template,
				},
			},
		},
		"invalid selector": {
			old: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Selector: validSelector,
					Template: &validPodTemplate.Template,
				},
			},
			update: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Replicas: 2,
					Selector: invalidSelector,
					Template: &validPodTemplate.Template,
				},
			},
		},
		"invalid pod": {
			old: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Selector: validSelector,
					Template: &validPodTemplate.Template,
				},
			},
			update: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Replicas: 2,
					Selector: validSelector,
					Template: &invalidPodTemplate.Template,
				},
			},
		},
		"negative replicas": {
			old: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Selector: validSelector,
					Template: &validPodTemplate.Template,
				},
			},
			update: core.ReplicationController{
				ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
				Spec: core.ReplicationControllerSpec{
					Replicas: -1,
					Selector: validSelector,
					Template: &validPodTemplate.Template,
				},
			},
		},
	}
	for testName, errorCase := range errorCases {
		if errs := ValidateReplicationControllerUpdate(&errorCase.update, &errorCase.old, PodValidationOptions{}); len(errs) == 0 {
			t.Errorf("expected failure: %s", testName)
		}
	}
}

func TestValidateReplicationController(t *testing.T) {
	validSelector := map[string]string{"a": "b"}
	validPodTemplate := core.PodTemplate{
		Template: core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: podtest.MakePodSpec(),
		},
	}
	readWriteVolumePodTemplate := core.PodTemplate{
		Template: core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: podtest.MakePodSpec(
				podtest.SetVolumes(core.Volume{Name: "gcepd", VolumeSource: core.VolumeSource{GCEPersistentDisk: &core.GCEPersistentDiskVolumeSource{PDName: "my-PD", FSType: "ext4", Partition: 1, ReadOnly: false}}}),
			),
		},
	}
	hostnetPodTemplate := core.PodTemplate{
		Template: core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: validSelector,
			},
			Spec: podtest.MakePodSpec(
				podtest.SetSecurityContext(&core.PodSecurityContext{
					HostNetwork: true,
				}),
				podtest.SetContainers(podtest.MakeContainer("abc", podtest.SetContainerPorts(
					core.ContainerPort{
						ContainerPort: 12345,
						Protocol:      core.ProtocolTCP,
					}))),
			),
		},
	}
	invalidSelector := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}
	invalidPodTemplate := core.PodTemplate{
		Template: core.PodTemplateSpec{
			Spec: podtest.MakePodSpec(),
			ObjectMeta: metav1.ObjectMeta{
				Labels: invalidSelector,
			},
		},
	}
	successCases := []core.ReplicationController{{
		ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
		Spec: core.ReplicationControllerSpec{
			Selector: validSelector,
			Template: &validPodTemplate.Template,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "abc-123", Namespace: metav1.NamespaceDefault},
		Spec: core.ReplicationControllerSpec{
			Selector: validSelector,
			Template: &validPodTemplate.Template,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "abc-123", Namespace: metav1.NamespaceDefault},
		Spec: core.ReplicationControllerSpec{
			Replicas: 1,
			Selector: validSelector,
			Template: &readWriteVolumePodTemplate.Template,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "hostnet", Namespace: metav1.NamespaceDefault},
		Spec: core.ReplicationControllerSpec{
			Replicas: 1,
			Selector: validSelector,
			Template: &hostnetPodTemplate.Template,
		},
	}}
	for _, successCase := range successCases {
		if errs := ValidateReplicationController(&successCase, PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := map[string]core.ReplicationController{
		"zero-length ID": {
			ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
				Template: &validPodTemplate.Template,
			},
		},
		"missing-namespace": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc-123"},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
				Template: &validPodTemplate.Template,
			},
		},
		"empty selector": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Template: &validPodTemplate.Template,
			},
		},
		"selector_doesnt_match": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Selector: map[string]string{"foo": "bar"},
				Template: &validPodTemplate.Template,
			},
		},
		"invalid manifest": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
			},
		},
		"read-write persistent disk with > 1 pod": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc"},
			Spec: core.ReplicationControllerSpec{
				Replicas: 2,
				Selector: validSelector,
				Template: &readWriteVolumePodTemplate.Template,
			},
		},
		"negative_replicas": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Replicas: -1,
				Selector: validSelector,
			},
		},
		"invalid_label": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
				Labels: map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				},
			},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
				Template: &validPodTemplate.Template,
			},
		},
		"invalid_label 2": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
				Labels: map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				},
			},
			Spec: core.ReplicationControllerSpec{
				Template: &invalidPodTemplate.Template,
			},
		},
		"invalid_annotation": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
				Annotations: map[string]string{
					"NoUppercaseOrSpecialCharsLike=Equals": "bar",
				},
			},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
				Template: &validPodTemplate.Template,
			},
		},
		"invalid restart policy 1": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
				Template: &core.PodTemplateSpec{
					Spec: podtest.MakePodSpec(podtest.SetRestartPolicy(core.RestartPolicyOnFailure)),
					ObjectMeta: metav1.ObjectMeta{
						Labels: validSelector,
					},
				},
			},
		},
		"invalid restart policy 2": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "abc-123",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: core.ReplicationControllerSpec{
				Selector: validSelector,
				Template: &core.PodTemplateSpec{
					Spec: podtest.MakePodSpec(podtest.SetRestartPolicy(core.RestartPolicyNever)),
					ObjectMeta: metav1.ObjectMeta{
						Labels: validSelector,
					},
				},
			},
		},
		"template may not contain ephemeral containers": {
			ObjectMeta: metav1.ObjectMeta{Name: "abc-123", Namespace: metav1.NamespaceDefault},
			Spec: core.ReplicationControllerSpec{
				Replicas: 1,
				Selector: validSelector,
				Template: &core.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: validSelector,
					},
					Spec: podtest.MakePodSpec(
						podtest.SetEphemeralContainers(core.EphemeralContainer{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "debug", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File"}}),
					),
				},
			},
		},
	}
	for k, v := range errorCases {
		errs := ValidateReplicationController(&v, PodValidationOptions{})
		if len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
		for i := range errs {
			field := errs[i].Field
			if !strings.HasPrefix(field, "spec.template.") &&
				field != "metadata.name" &&
				field != "metadata.namespace" &&
				field != "spec.selector" &&
				field != "spec.template" &&
				field != "GCEPersistentDisk.ReadOnly" &&
				field != "spec.replicas" &&
				field != "spec.template.labels" &&
				field != "metadata.annotations" &&
				field != "metadata.labels" &&
				field != "status.replicas" {
				t.Errorf("%s: missing prefix for: %v", k, errs[i])
			}
		}
	}
}

func TestValidateNode(t *testing.T) {
	validSelector := map[string]string{"a": "b"}
	invalidSelector := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}
	successCases := []core.Node{{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "abc",
			Labels: validSelector,
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Type: core.NodeExternalIP, Address: "something"},
			},
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
				core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
				core.ResourceName("my.org/gpu"):        resource.MustParse("10"),
				core.ResourceName("hugepages-2Mi"):     resource.MustParse("10Gi"),
				core.ResourceName("hugepages-1Gi"):     resource.MustParse("0"),
			},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name: "abc",
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Type: core.NodeExternalIP, Address: "something"},
			},
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
				core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
			},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:   "abc",
			Labels: validSelector,
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Type: core.NodeExternalIP, Address: "something"},
			},
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
				core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
				core.ResourceName("my.org/gpu"):        resource.MustParse("10"),
				core.ResourceName("hugepages-2Mi"):     resource.MustParse("10Gi"),
				core.ResourceName("hugepages-1Gi"):     resource.MustParse("10Gi"),
			},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name: "dedicated-node1",
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Type: core.NodeExternalIP, Address: "something"},
			},
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
				core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
			},
		},
		Spec: core.NodeSpec{
			// Add a valid taint to a node
			Taints: []core.Taint{{Key: "GPU", Value: "true", Effect: "NoSchedule"}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name: "abc",
			Annotations: map[string]string{
				core.PreferAvoidPodsAnnotationKey: `
							{
							    "preferAvoidPods": [
							        {
							            "podSignature": {
							                "podController": {
							                    "apiVersion": "v1",
							                    "kind": "ReplicationController",
							                    "name": "foo",
							                    "uid": "abcdef123456",
							                    "controller": true
							                }
							            },
							            "reason": "some reason",
							            "message": "some message"
							        }
							    ]
							}`,
			},
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Type: core.NodeExternalIP, Address: "something"},
			},
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
				core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
			},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name: "abc",
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Type: core.NodeExternalIP, Address: "something"},
			},
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
				core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
			},
		},
		Spec: core.NodeSpec{
			PodCIDRs: []string{"192.168.0.0/16"},
		},
	},
	}
	for _, successCase := range successCases {
		if errs := ValidateNode(&successCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}

	errorCases := map[string]core.Node{
		"zero-length Name": {
			ObjectMeta: metav1.ObjectMeta{
				Name:   "",
				Labels: validSelector,
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{},
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
				},
			},
		},
		"invalid-labels": {
			ObjectMeta: metav1.ObjectMeta{
				Name:   "abc-123",
				Labels: invalidSelector,
			},
			Status: core.NodeStatus{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
				},
			},
		},
		"missing-taint-key": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "dedicated-node1",
			},
			Spec: core.NodeSpec{
				// Add a taint with an empty key to a node
				Taints: []core.Taint{{Key: "", Value: "special-user-1", Effect: "NoSchedule"}},
			},
		},
		"bad-taint-key": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "dedicated-node1",
			},
			Spec: core.NodeSpec{
				// Add a taint with an invalid  key to a node
				Taints: []core.Taint{{Key: "NoUppercaseOrSpecialCharsLike=Equals", Value: "special-user-1", Effect: "NoSchedule"}},
			},
		},
		"bad-taint-value": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "dedicated-node2",
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{
					{Type: core.NodeExternalIP, Address: "something"},
				},
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
				},
			},
			Spec: core.NodeSpec{
				// Add a taint with a bad value to a node
				Taints: []core.Taint{{Key: "dedicated", Value: "some\\bad\\value", Effect: "NoSchedule"}},
			},
		},
		"missing-taint-effect": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "dedicated-node3",
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{
					{Type: core.NodeExternalIP, Address: "something"},
				},
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
				},
			},
			Spec: core.NodeSpec{
				// Add a taint with an empty effect to a node
				Taints: []core.Taint{{Key: "dedicated", Value: "special-user-3", Effect: ""}},
			},
		},
		"invalid-taint-effect": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "dedicated-node3",
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{
					{Type: core.NodeExternalIP, Address: "something"},
				},
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
				},
			},
			Spec: core.NodeSpec{
				// Add a taint with NoExecute effect to a node
				Taints: []core.Taint{{Key: "dedicated", Value: "special-user-3", Effect: "NoScheduleNoAdmit"}},
			},
		},
		"duplicated-taints-with-same-key-effect": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "dedicated-node1",
			},
			Spec: core.NodeSpec{
				// Add two taints to the node with the same key and effect; should be rejected.
				Taints: []core.Taint{
					{Key: "dedicated", Value: "special-user-1", Effect: "NoSchedule"},
					{Key: "dedicated", Value: "special-user-2", Effect: "NoSchedule"},
				},
			},
		},
		"missing-podSignature": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "abc-123",
				Annotations: map[string]string{
					core.PreferAvoidPodsAnnotationKey: `
							{
							    "preferAvoidPods": [
							        {
							            "reason": "some reason",
							            "message": "some message"
							        }
							    ]
							}`,
				},
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{},
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
				},
			},
		},
		"invalid-podController": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "abc-123",
				Annotations: map[string]string{
					core.PreferAvoidPodsAnnotationKey: `
							{
							    "preferAvoidPods": [
							        {
							            "podSignature": {
							                "podController": {
							                    "apiVersion": "v1",
							                    "kind": "ReplicationController",
							                    "name": "foo",
                                                                           "uid": "abcdef123456",
                                                                           "controller": false
							                }
							            },
							            "reason": "some reason",
							            "message": "some message"
							        }
							    ]
							}`,
				},
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{},
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
				},
			},
		},
		"invalid-pod-cidr": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "abc",
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{
					{Type: core.NodeExternalIP, Address: "something"},
				},
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
				},
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0"},
			},
		},
		"duplicate-pod-cidr": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "abc",
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{
					{Type: core.NodeExternalIP, Address: "something"},
				},
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
				},
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"10.0.0.1/16", "10.0.0.1/16"},
			},
		},
	}
	for k, v := range errorCases {
		errs := ValidateNode(&v)
		if len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
		for i := range errs {
			field := errs[i].Field
			expectedFields := map[string]bool{
				"metadata.name":         true,
				"metadata.labels":       true,
				"metadata.annotations":  true,
				"metadata.namespace":    true,
				"spec.externalID":       true,
				"spec.taints[0].key":    true,
				"spec.taints[0].value":  true,
				"spec.taints[0].effect": true,
				"metadata.annotations.scheduler.alpha.kubernetes.io/preferAvoidPods[0].PodSignature":                          true,
				"metadata.annotations.scheduler.alpha.kubernetes.io/preferAvoidPods[0].PodSignature.PodController.Controller": true,
			}
			if val, ok := expectedFields[field]; ok {
				if !val {
					t.Errorf("%s: missing prefix for: %v", k, errs[i])
				}
			}
		}
	}
}

func TestValidateNodeUpdate(t *testing.T) {
	tests := []struct {
		oldNode core.Node
		node    core.Node
		valid   bool
	}{
		{core.Node{}, core.Node{}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo"}},
			core.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bar"},
			}, false},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"foo": "bar"},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"foo": "baz"},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"foo": "baz"},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"bar": "foo"},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"foo": "baz"},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0/16"},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.123.0.0/16"},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0/16"},
			},
		}, false},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Status: core.NodeStatus{
				Capacity: core.ResourceList{
					core.ResourceCPU:    resource.MustParse("10000"),
					core.ResourceMemory: resource.MustParse("100"),
				},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Status: core.NodeStatus{
				Capacity: core.ResourceList{
					core.ResourceCPU:    resource.MustParse("100"),
					core.ResourceMemory: resource.MustParse("10000"),
				},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"bar": "foo"},
			},
			Status: core.NodeStatus{
				Capacity: core.ResourceList{
					core.ResourceCPU:    resource.MustParse("10000"),
					core.ResourceMemory: resource.MustParse("100"),
				},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"bar": "fooobaz"},
			},
			Status: core.NodeStatus{
				Capacity: core.ResourceList{
					core.ResourceCPU:    resource.MustParse("100"),
					core.ResourceMemory: resource.MustParse("10000"),
				},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"bar": "foo"},
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{
					{Type: core.NodeExternalIP, Address: "1.2.3.4"},
				},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"bar": "fooobaz"},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"foo": "baz"},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo",
				Labels: map[string]string{"Foo": "baz"},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: core.NodeSpec{
				Unschedulable: false,
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: core.NodeSpec{
				Unschedulable: true,
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: core.NodeSpec{
				Unschedulable: false,
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{
					{Type: core.NodeExternalIP, Address: "1.1.1.1"},
					{Type: core.NodeExternalIP, Address: "1.1.1.1"},
				},
			},
		}, false},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: core.NodeSpec{
				Unschedulable: false,
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Status: core.NodeStatus{
				Addresses: []core.NodeAddress{
					{Type: core.NodeExternalIP, Address: "1.1.1.1"},
					{Type: core.NodeInternalIP, Address: "10.1.1.1"},
				},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
				Annotations: map[string]string{
					core.PreferAvoidPodsAnnotationKey: `
							{
							    "preferAvoidPods": [
							        {
							            "podSignature": {
							                "podController": {
							                    "apiVersion": "v1",
							                    "kind": "ReplicationController",
							                    "name": "foo",
                                                                           "uid": "abcdef123456",
                                                                           "controller": true
							                }
							            },
							            "reason": "some reason",
							            "message": "some message"
							        }
							    ]
							}`,
				},
			},
			Spec: core.NodeSpec{
				Unschedulable: false,
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
				Annotations: map[string]string{
					core.PreferAvoidPodsAnnotationKey: `
							{
							    "preferAvoidPods": [
							        {
							            "reason": "some reason",
							            "message": "some message"
							        }
							    ]
							}`,
				},
			},
		}, false},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
				Annotations: map[string]string{
					core.PreferAvoidPodsAnnotationKey: `
							{
							    "preferAvoidPods": [
							        {
							            "podSignature": {
							                "podController": {
							                    "apiVersion": "v1",
							                    "kind": "ReplicationController",
							                    "name": "foo",
							                    "uid": "abcdef123456",
							                    "controller": false
							                }
							            },
							            "reason": "some reason",
							            "message": "some message"
							        }
							    ]
							}`,
				},
			},
		}, false},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "valid-extended-resources",
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "valid-extended-resources",
			},
			Status: core.NodeStatus{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
					core.ResourceName("example.com/a"):     resource.MustParse("5"),
					core.ResourceName("example.com/b"):     resource.MustParse("10"),
				},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "invalid-fractional-extended-capacity",
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "invalid-fractional-extended-capacity",
			},
			Status: core.NodeStatus{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
					core.ResourceName("example.com/a"):     resource.MustParse("500m"),
				},
			},
		}, false},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "invalid-fractional-extended-allocatable",
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "invalid-fractional-extended-allocatable",
			},
			Status: core.NodeStatus{
				Capacity: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
					core.ResourceName("example.com/a"):     resource.MustParse("5"),
				},
				Allocatable: core.ResourceList{
					core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
					core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
					core.ResourceName("example.com/a"):     resource.MustParse("4.5"),
				},
			},
		}, false},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "update-provider-id-when-not-set",
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "update-provider-id-when-not-set",
			},
			Spec: core.NodeSpec{
				ProviderID: "provider:///new",
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "update-provider-id-when-set",
			},
			Spec: core.NodeSpec{
				ProviderID: "provider:///old",
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "update-provider-id-when-set",
			},
			Spec: core.NodeSpec{
				ProviderID: "provider:///new",
			},
		}, false},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-cidrs-as-is",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0/16"},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-cidrs-as-is",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0/16"},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-cidrs-as-is-2",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0/16", "2000::/10"},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-cidrs-as-is-2",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0/16", "2000::/10"},
			},
		}, true},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-cidrs-not-same-length",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0/16", "192.167.0.0/16", "2000::/10"},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-cidrs-not-same-length",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0/16", "2000::/10"},
			},
		}, false},
		{core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-cidrs-not-same",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"192.168.0.0/16", "2000::/10"},
			},
		}, core.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-cidrs-not-same",
			},
			Spec: core.NodeSpec{
				PodCIDRs: []string{"2000::/10", "192.168.0.0/16"},
			},
		}, false},
	}
	for i, test := range tests {
		test.oldNode.ObjectMeta.ResourceVersion = "1"
		test.node.ObjectMeta.ResourceVersion = "1"
		errs := ValidateNodeUpdate(&test.node, &test.oldNode)
		if test.valid && len(errs) > 0 {
			t.Errorf("%d: Unexpected error: %v", i, errs)
			t.Logf("%#v vs %#v", test.oldNode.ObjectMeta, test.node.ObjectMeta)
		}
		if !test.valid && len(errs) == 0 {
			t.Errorf("%d: Unexpected non-error", i)
		}
	}
}

func TestValidateServiceUpdate(t *testing.T) {
	requireDualStack := core.IPFamilyPolicyRequireDualStack
	preferDualStack := core.IPFamilyPolicyPreferDualStack
	singleStack := core.IPFamilyPolicySingleStack
	testCases := []struct {
		name     string
		tweakSvc func(oldSvc, newSvc *core.Service) // given basic valid services, each test case can customize them
		numErrs  int
	}{{
		name: "no change",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			// do nothing
		},
		numErrs: 0,
	}, {
		name: "change name",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Name += "2"
		},
		numErrs: 1,
	}, {
		name: "change namespace",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Namespace += "2"
		},
		numErrs: 1,
	}, {
		name: "change label valid",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Labels["key"] = "other-value"
		},
		numErrs: 0,
	}, {
		name: "add label",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Labels["key2"] = "value2"
		},
		numErrs: 0,
	}, {
		name: "change cluster IP",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			oldSvc.Spec.ClusterIP = "1.2.3.4"
			oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

			newSvc.Spec.ClusterIP = "8.6.7.5"
			newSvc.Spec.ClusterIPs = []string{"8.6.7.5"}
		},
		numErrs: 1,
	}, {
		name: "remove cluster IP",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			oldSvc.Spec.ClusterIP = "1.2.3.4"
			oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

			newSvc.Spec.ClusterIP = ""
			newSvc.Spec.ClusterIPs = nil
		},
		numErrs: 1,
	}, {
		name: "change affinity",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Spec.SessionAffinity = "ClientIP"
			newSvc.Spec.SessionAffinityConfig = &core.SessionAffinityConfig{
				ClientIP: &core.ClientIPConfig{
					TimeoutSeconds: utilpointer.Int32(90),
				},
			}
		},
		numErrs: 0,
	}, {
		name: "remove affinity",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Spec.SessionAffinity = ""
		},
		numErrs: 1,
	}, {
		name: "change type",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Spec.Type = core.ServiceTypeLoadBalancer
			newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
		},
		numErrs: 0,
	}, {
		name: "remove type",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Spec.Type = ""
		},
		numErrs: 1,
	}, {
		name: "change type -> nodeport",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Spec.Type = core.ServiceTypeNodePort
			newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
		},
		numErrs: 0,
	}, {
		name: "add loadBalancerSourceRanges",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
			oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			newSvc.Spec.Type = core.ServiceTypeLoadBalancer
			newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			newSvc.Spec.LoadBalancerSourceRanges = []string{"10.0.0.0/8"}
		},
		numErrs: 0,
	}, {
		name: "update loadBalancerSourceRanges",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
			oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			oldSvc.Spec.LoadBalancerSourceRanges = []string{"10.0.0.0/8"}
			newSvc.Spec.Type = core.ServiceTypeLoadBalancer
			newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			newSvc.Spec.LoadBalancerSourceRanges = []string{"10.100.0.0/16"}
		},
		numErrs: 0,
	}, {
		name: "LoadBalancer type cannot have None ClusterIP",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			newSvc.Spec.ClusterIP = "None"
			newSvc.Spec.ClusterIPs = []string{"None"}
			newSvc.Spec.Type = core.ServiceTypeLoadBalancer
			newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
			newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
		},
		numErrs: 1,
	}, {
		name: "`None` ClusterIP can NOT be changed",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			oldSvc.Spec.Type = core.ServiceTypeClusterIP
			newSvc.Spec.Type = core.ServiceTypeClusterIP

			oldSvc.Spec.ClusterIP = "None"
			oldSvc.Spec.ClusterIPs = []string{"None"}

			newSvc.Spec.ClusterIP = "1.2.3.4"
			newSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
		},
		numErrs: 1,
	}, {
		name: "`None` ClusterIP can NOT be removed",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			oldSvc.Spec.ClusterIP = "None"
			oldSvc.Spec.ClusterIPs = []string{"None"}

			newSvc.Spec.ClusterIP = ""
			newSvc.Spec.ClusterIPs = nil
		},
		numErrs: 1,
	}, {
		name: "ClusterIP can NOT be changed to None",
		tweakSvc: func(oldSvc, newSvc *core.Service) {
			oldSvc.Spec.ClusterIP = "1.2.3.4"
			oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

			newSvc.Spec.ClusterIP = "None"
			newSvc.Spec.ClusterIPs = []string{"None"}
		},
		numErrs: 1,
	},

		{
			name: "Service with ClusterIP type cannot change its set ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.Type = core.ServiceTypeClusterIP

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 1,
		}, {
			name: "Service with ClusterIP type can change its empty ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.Type = core.ServiceTypeClusterIP

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil
				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with ClusterIP type cannot change its set ClusterIP when changing type to NodePort",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 1,
		}, {
			name: "Service with ClusterIP type can change its empty ClusterIP when changing type to NodePort",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with ClusterIP type cannot change its ClusterIP when changing type to LoadBalancer",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 1,
		}, {
			name: "Service with ClusterIP type can change its empty ClusterIP when changing type to LoadBalancer",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with LoadBalancer type can change its AllocateLoadBalancerNodePorts from true to false",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(false)
			},
			numErrs: 0,
		}, {
			name: "Service with LoadBalancer type can change its AllocateLoadBalancerNodePorts from false to true",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(false)
				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			},
			numErrs: 0,
		}, {
			name: "Service with NodePort type cannot change its set ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 1,
		}, {
			name: "Service with NodePort type can change its empty ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with NodePort type cannot change its set ClusterIP when changing type to ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.Type = core.ServiceTypeClusterIP

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 1,
		}, {
			name: "Service with NodePort type can change its empty ClusterIP when changing type to ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.Type = core.ServiceTypeClusterIP

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with NodePort type cannot change its set ClusterIP when changing type to LoadBalancer",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 1,
		}, {
			name: "Service with NodePort type can change its empty ClusterIP when changing type to LoadBalancer",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with LoadBalancer type cannot change its set ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 1,
		}, {
			name: "Service with LoadBalancer type can change its empty ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with LoadBalancer type cannot change its set ClusterIP when changing type to ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.Type = core.ServiceTypeClusterIP

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 1,
		}, {
			name: "Service with LoadBalancer type can change its empty ClusterIP when changing type to ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.Type = core.ServiceTypeClusterIP

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with LoadBalancer type cannot change its set ClusterIP when changing type to NodePort",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 1,
		}, {
			name: "Service with LoadBalancer type can change its empty ClusterIP when changing type to NodePort",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with ExternalName type can change its empty ClusterIP when changing type to ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeExternalName
				newSvc.Spec.Type = core.ServiceTypeClusterIP

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "Service with ExternalName type can change its set ClusterIP when changing type to ClusterIP",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeExternalName
				newSvc.Spec.Type = core.ServiceTypeClusterIP

				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}

				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
			},
			numErrs: 0,
		}, {
			name: "invalid node port with clusterIP None",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster

				oldSvc.Spec.Ports = append(oldSvc.Spec.Ports, core.ServicePort{Name: "q", Port: 1, Protocol: "TCP", NodePort: 1, TargetPort: intstr.FromInt32(1)})
				newSvc.Spec.Ports = append(newSvc.Spec.Ports, core.ServicePort{Name: "q", Port: 1, Protocol: "TCP", NodePort: 1, TargetPort: intstr.FromInt32(1)})

				oldSvc.Spec.ClusterIP = ""
				oldSvc.Spec.ClusterIPs = nil

				newSvc.Spec.ClusterIP = "None"
				newSvc.Spec.ClusterIPs = []string{"None"}
			},
			numErrs: 1,
		},
		/* Service IP Family */
		{
			name: "convert from ExternalName",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeExternalName
				newSvc.Spec.Type = core.ServiceTypeClusterIP
			},
			numErrs: 0,
		}, {
			name: "invalid: convert to ExternalName",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				singleStack := core.IPFamilyPolicySingleStack

				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.ClusterIP = "10.0.0.10"
				oldSvc.Spec.ClusterIPs = []string{"10.0.0.10"}
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
				oldSvc.Spec.IPFamilyPolicy = &singleStack

				newSvc.Spec.Type = core.ServiceTypeExternalName
				newSvc.Spec.ExternalName = "foo"
				/*
					not removing these fields is a validation error
					strategy takes care of resetting Families & Policy if ClusterIPs
					were reset. But it does not get called in validation testing.
				*/
				newSvc.Spec.ClusterIP = "10.0.0.10"
				newSvc.Spec.ClusterIPs = []string{"10.0.0.10"}
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
				newSvc.Spec.IPFamilyPolicy = &singleStack

			},
			numErrs: 3,
		}, {
			name: "valid: convert to ExternalName",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				singleStack := core.IPFamilyPolicySingleStack
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.ClusterIP = "10.0.0.10"
				oldSvc.Spec.ClusterIPs = []string{"10.0.0.10"}
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
				oldSvc.Spec.IPFamilyPolicy = &singleStack

				newSvc.Spec.Type = core.ServiceTypeExternalName
				newSvc.Spec.ExternalName = "foo"
			},
			numErrs: 0,
		},

		{
			name: "same ServiceIPFamily",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "same ServiceIPFamily, change IPFamilyPolicy to singleStack",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = nil
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.IPFamilyPolicy = &singleStack
				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "same ServiceIPFamily, change IPFamilyPolicy singleStack => requireDualStack",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &singleStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		},

		{
			name: "add a new ServiceIPFamily",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0,
		},

		{
			name: "ExternalName while changing Service IPFamily",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ExternalName = "somename"
				oldSvc.Spec.Type = core.ServiceTypeExternalName

				newSvc.Spec.ExternalName = "somename"
				newSvc.Spec.Type = core.ServiceTypeExternalName
			},
			numErrs: 0,
		}, {
			name: "setting ipfamily from nil to v4",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.IPFamilies = nil

				newSvc.Spec.ExternalName = "somename"
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "setting ipfamily from nil to v6",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.IPFamilies = nil

				newSvc.Spec.ExternalName = "somename"
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol}
			},
			numErrs: 0,
		}, {
			name: "change primary ServiceIPFamily",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.4"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol}
			},
			numErrs: 2,
		},
		/* upgrade + downgrade from/to dualstack tests */
		{
			name: "valid: upgrade to dual stack with requiredDualStack",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &singleStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.4"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid: upgrade to dual stack with preferDualStack",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &singleStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.4"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				newSvc.Spec.IPFamilyPolicy = &preferDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		},

		{
			name: "valid: upgrade to dual stack, no specific secondary ip",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &singleStack

				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.4"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid: upgrade to dual stack, with specific secondary ip",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &singleStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.4"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.4", "2001::1"}
				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid: downgrade from dual to single",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4", "2001::1"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.4"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				newSvc.Spec.IPFamilyPolicy = &singleStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid: change families for a headless service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "None"
				oldSvc.Spec.ClusterIPs = []string{"None"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "None"
				newSvc.Spec.ClusterIPs = []string{"None"}
				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol, core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid: upgrade a headless service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "None"
				oldSvc.Spec.ClusterIPs = []string{"None"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &singleStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "None"
				newSvc.Spec.ClusterIPs = []string{"None"}
				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol, core.IPv4Protocol}
			},
			numErrs: 0,
		}, {
			name: "valid: downgrade a headless service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "None"
				oldSvc.Spec.ClusterIPs = []string{"None"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "None"
				newSvc.Spec.ClusterIPs = []string{"None"}
				newSvc.Spec.IPFamilyPolicy = &singleStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol}
			},
			numErrs: 0,
		},

		{
			name: "invalid flip families",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.40"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4", "2001::1"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "2001::1"
				newSvc.Spec.ClusterIPs = []string{"2001::1", "1.2.3.5"}
				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv6Protocol, core.IPv4Protocol}
			},
			numErrs: 4,
		}, {
			name: "invalid change first ip, in dualstack service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4", "2001::1"}
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5", "2001::1"}
				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 1,
		}, {
			name: "invalid, change second ip in dualstack service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4", "2001::1"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.4"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.4", "2002::1"}
				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 1,
		}, {
			name: "downgrade keeping the families",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4", "2001::1"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.4"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				newSvc.Spec.IPFamilyPolicy = &singleStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 0, // families and ips are trimmed in strategy
		}, {
			name: "invalid, downgrade without changing to singleStack",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4", "2001::1"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.4"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 2,
		}, {
			name: "invalid, downgrade and change primary ip",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4", "2001::1"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &requireDualStack
				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
				newSvc.Spec.IPFamilyPolicy = &singleStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
			},
			numErrs: 1,
		}, {
			name: "invalid: upgrade to dual stack and change primary",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.ClusterIP = "1.2.3.4"
				oldSvc.Spec.ClusterIPs = []string{"1.2.3.4"}
				oldSvc.Spec.Type = core.ServiceTypeClusterIP
				oldSvc.Spec.IPFamilyPolicy = &singleStack

				oldSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.ClusterIP = "1.2.3.5"
				newSvc.Spec.ClusterIPs = []string{"1.2.3.5"}
				newSvc.Spec.IPFamilyPolicy = &requireDualStack
				newSvc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol, core.IPv6Protocol}
			},
			numErrs: 1,
		}, {
			name: "update to valid app protocol",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Ports = []core.ServicePort{{Name: "a", Port: 443, TargetPort: intstr.FromInt32(3000), Protocol: "TCP"}}
				newSvc.Spec.Ports = []core.ServicePort{{Name: "a", Port: 443, TargetPort: intstr.FromInt32(3000), Protocol: "TCP", AppProtocol: utilpointer.String("https")}}
			},
			numErrs: 0,
		}, {
			name: "update to invalid app protocol",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Ports = []core.ServicePort{{Name: "a", Port: 443, TargetPort: intstr.FromInt32(3000), Protocol: "TCP"}}
				newSvc.Spec.Ports = []core.ServicePort{{Name: "a", Port: 443, TargetPort: intstr.FromInt32(3000), Protocol: "TCP", AppProtocol: utilpointer.String("~https")}}
			},
			numErrs: 1,
		}, {
			name: "Set AllocateLoadBalancerNodePorts when type is not LoadBalancer",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
			},
			numErrs: 1,
		}, {
			name: "update LoadBalancer type of service without change LoadBalancerClass",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				oldSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-old")

				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-old")
			},
			numErrs: 0,
		}, {
			name: "invalid: change LoadBalancerClass when update service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				oldSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-old")

				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-new")
			},
			numErrs: 1,
		}, {
			name: "invalid: unset LoadBalancerClass when update service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				oldSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-old")

				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.LoadBalancerClass = nil
			},
			numErrs: 1,
		}, {
			name: "invalid: set LoadBalancerClass when update service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				oldSvc.Spec.LoadBalancerClass = nil

				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-new")
			},
			numErrs: 1,
		}, {
			name: "update to LoadBalancer type of service with valid LoadBalancerClass",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP

				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")
			},
			numErrs: 0,
		}, {
			name: "update to LoadBalancer type of service without LoadBalancerClass",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP

				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.LoadBalancerClass = nil
			},
			numErrs: 0,
		}, {
			name: "invalid: set invalid LoadBalancerClass when update service to LoadBalancer",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP

				newSvc.Spec.Type = core.ServiceTypeLoadBalancer
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				newSvc.Spec.LoadBalancerClass = utilpointer.String("Bad/LoadBalancerclass")
			},
			numErrs: 2,
		}, {
			name: "invalid: set LoadBalancerClass when update service to non LoadBalancer type of service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeClusterIP

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")
			},
			numErrs: 2,
		}, {
			name: "invalid: set LoadBalancerClass when update service to non LoadBalancer type of service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeExternalName

				newSvc.Spec.Type = core.ServiceTypeExternalName
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")
			},
			numErrs: 3,
		}, {
			name: "invalid: set LoadBalancerClass when update service to non LoadBalancer type of service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeNodePort

				newSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")
			},
			numErrs: 2,
		}, {
			name: "invalid: set LoadBalancerClass when update from LoadBalancer service to non LoadBalancer type of service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				oldSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")

				newSvc.Spec.Type = core.ServiceTypeClusterIP
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")
			},
			numErrs: 2,
		}, {
			name: "invalid: set LoadBalancerClass when update from LoadBalancer service to non LoadBalancer type of service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				oldSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")

				newSvc.Spec.Type = core.ServiceTypeExternalName
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")
			},
			numErrs: 3,
		}, {
			name: "invalid: set LoadBalancerClass when update from LoadBalancer service to non LoadBalancer type of service",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				oldSvc.Spec.Type = core.ServiceTypeLoadBalancer
				oldSvc.Spec.AllocateLoadBalancerNodePorts = utilpointer.Bool(true)
				oldSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")

				newSvc.Spec.Type = core.ServiceTypeNodePort
				newSvc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyCluster
				newSvc.Spec.LoadBalancerClass = utilpointer.String("test.com/test-load-balancer-class")
			},
			numErrs: 2,
		}, {
			name: "update internalTrafficPolicy from Cluster to Local",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				cluster := core.ServiceInternalTrafficPolicyCluster
				oldSvc.Spec.InternalTrafficPolicy = &cluster

				local := core.ServiceInternalTrafficPolicyLocal
				newSvc.Spec.InternalTrafficPolicy = &local
			},
			numErrs: 0,
		}, {
			name: "update internalTrafficPolicy from Local to Cluster",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				local := core.ServiceInternalTrafficPolicyLocal
				oldSvc.Spec.InternalTrafficPolicy = &local

				cluster := core.ServiceInternalTrafficPolicyCluster
				newSvc.Spec.InternalTrafficPolicy = &cluster
			},
			numErrs: 0,
		}, {
			name: "topology annotations are mismatched",
			tweakSvc: func(oldSvc, newSvc *core.Service) {
				newSvc.Annotations[core.DeprecatedAnnotationTopologyAwareHints] = "original"
				newSvc.Annotations[core.AnnotationTopologyMode] = "different"
			},
			numErrs: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			oldSvc := makeValidService()
			newSvc := makeValidService()
			tc.tweakSvc(&oldSvc, &newSvc)
			errs := ValidateServiceUpdate(&newSvc, &oldSvc)
			if len(errs) != tc.numErrs {
				t.Errorf("Expected %d errors, got %d: %v", tc.numErrs, len(errs), errs.ToAggregate())
			}
		})
	}
}

func TestValidatePodResourceConsistency(t *testing.T) {
	path := field.NewPath("resources")
	tests := []struct {
		name           string
		podResources   core.ResourceRequirements
		containers     []core.Container
		expectedErrors []string
	}{{
		name: "aggregate container requests less than pod requests",
		podResources: core.ResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("10"),
				core.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
		containers: []core.Container{
			{
				Resources: core.ResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("5"),
						core.ResourceMemory: resource.MustParse("5Mi"),
					},
				},
			}, {
				Resources: core.ResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("4"),
						core.ResourceMemory: resource.MustParse("3Mi"),
					},
				},
			},
		},
	}, {
		name: "aggregate container requests equal to pod requests",
		podResources: core.ResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("10"),
				core.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
		containers: []core.Container{
			{
				Resources: core.ResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("5"),
						core.ResourceMemory: resource.MustParse("5Mi"),
					},
				},
			}, {
				Resources: core.ResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("5"),
						core.ResourceMemory: resource.MustParse("5Mi"),
					},
				},
			},
		},
	}, {
		name: "aggregate container requests greater than pod requests",
		podResources: core.ResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("10"),
				core.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
		containers: []core.Container{
			{
				Resources: core.ResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("6"),
						core.ResourceMemory: resource.MustParse("5Mi"),
					},
				},
			}, {
				Resources: core.ResourceRequirements{
					Requests: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("8"),
						core.ResourceMemory: resource.MustParse("3Mi"),
					},
				},
			},
		},
		expectedErrors: []string{"must be greater than or equal to aggregate container requests"},
	}, {
		name: "aggregate container limits less than pod limits",
		podResources: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("10"),
				core.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
		containers: []core.Container{
			{
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("5"),
						core.ResourceMemory: resource.MustParse("5Mi"),
					},
				},
			}, {
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("4"),
						core.ResourceMemory: resource.MustParse("3Mi"),
					},
				},
			},
		},
	}, {
		name: "aggregate container limits equal to pod limits",
		podResources: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("10"),
				core.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
		containers: []core.Container{
			{
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("5"),
						core.ResourceMemory: resource.MustParse("5Mi"),
					},
				},
			}, {
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("5"),
						core.ResourceMemory: resource.MustParse("5Mi"),
					},
				},
			},
		},
	}, {
		name: "aggregate container limits greater than pod limits",
		podResources: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("10"),
				core.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
		containers: []core.Container{
			{
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("5"),
						core.ResourceMemory: resource.MustParse("5Mi"),
					},
				},
			}, {
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("6"),
						core.ResourceMemory: resource.MustParse("9Mi"),
					},
				},
			},
		},
	}, {
		name: "indivdual container limits greater than pod limits",
		podResources: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("10"),
				core.ResourceMemory: resource.MustParse("10Mi"),
			},
		},
		containers: []core.Container{
			{
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("11"),
						core.ResourceMemory: resource.MustParse("12Mi"),
					},
				},
			},
		},
		expectedErrors: []string{
			"must be less than or equal to pod limits",
			"must be less than or equal to pod limits",
		},
	},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := core.PodSpec{
				Resources:  &tc.podResources,
				Containers: tc.containers,
			}
			errs := validatePodResourceConsistency(&spec, path)
			if len(errs) != len(tc.expectedErrors) {
				t.Errorf("expected %d errors, got %d errors, got errors: %v", len(tc.expectedErrors), len(errs), errs)
			}

			for _, expectedErr := range tc.expectedErrors {
				expectedErrExists := false
				for _, gotErr := range errs {
					if strings.Contains(gotErr.Error(), expectedErr) {
						expectedErrExists = true
						break
					}
				}

				if !expectedErrExists {
					t.Errorf("expected: %v, got errors: %v", expectedErr, errs)
				}
			}
		})
	}
}

func TestValidatePodResourceNames(t *testing.T) {
	table := []struct {
		input           core.ResourceName
		expectedFailure bool
	}{
		{"memory", false},
		{"cpu", false},
		{"storage", true},
		{"requests.cpu", true},
		{"requests.memory", true},
		{"requests.storage", true},
		{"limits.cpu", true},
		{"limits.memory", true},
		{"limits.storage", true},
		{"network", true},
		{"disk", true},
		{"", true},
		{".", true},
		{"..", true},
		{"my.favorite.app.co/12345", true},
		{"my.favorite.app.co/_12345", true},
		{"my.favorite.app.co/12345_", true},
		{"kubernetes.io/..", true},
		{core.ResourceName("kubernetes.io/" + strings.Repeat("a", 64)), true},
		{core.ResourceName("kubernetes.io/" + strings.Repeat("a", 64)), true},
		{core.ResourceName("kubernetes.io/" + core.ResourceCPU), true},
		{core.ResourceName("kubernetes.io/" + core.ResourceMemory), true},
		{"kubernetes.io//", true},
		{"kubernetes.io", true},
		{"kubernetes.io/will/not/work/", true},
	}
	for _, item := range table {
		errs := validatePodResourceName(item.input, field.NewPath("field"))
		if len(errs) != 0 && !item.expectedFailure {
			t.Errorf("expected no failure for input %q, got: %v", item.input, errs)
		}

		if len(errs) == 0 && item.expectedFailure {
			t.Errorf("expected failure for input %q", item.input)
		}
	}
}

func TestValidateResourceNames(t *testing.T) {
	table := []struct {
		input   core.ResourceName
		success bool
		expect  string
	}{
		{"memory", true, ""},
		{"cpu", true, ""},
		{"storage", true, ""},
		{"requests.cpu", true, ""},
		{"requests.memory", true, ""},
		{"requests.storage", true, ""},
		{"limits.cpu", true, ""},
		{"limits.memory", true, ""},
		{"network", false, ""},
		{"disk", false, ""},
		{"", false, ""},
		{".", false, ""},
		{"..", false, ""},
		{"my.favorite.app.co/12345", true, ""},
		{"my.favorite.app.co/_12345", false, ""},
		{"my.favorite.app.co/12345_", false, ""},
		{"kubernetes.io/..", false, ""},
		{core.ResourceName("kubernetes.io/" + strings.Repeat("a", 63)), true, ""},
		{core.ResourceName("kubernetes.io/" + strings.Repeat("a", 64)), false, ""},
		{"kubernetes.io//", false, ""},
		{"kubernetes.io", false, ""},
		{"kubernetes.io/will/not/work/", false, ""},
	}
	for k, item := range table {
		err := validateResourceName(item.input, field.NewPath("field"))
		if len(err) != 0 && item.success {
			t.Errorf("expected no failure for input %q", item.input)
		} else if len(err) == 0 && !item.success {
			t.Errorf("expected failure for input %q", item.input)
			for i := range err {
				detail := err[i].Detail
				if detail != "" && !strings.Contains(detail, item.expect) {
					t.Errorf("%d: expected error detail either empty or %s, got %s", k, item.expect, detail)
				}
			}
		}
	}
}

func TestValidateLimitRangeForLocalStorage(t *testing.T) {
	testCases := []struct {
		name string
		spec core.LimitRangeSpec
	}{{
		name: "all-fields-valid",
		spec: core.LimitRangeSpec{
			Limits: []core.LimitRangeItem{{
				Type:                 core.LimitTypePod,
				Max:                  getResources("", "", "10000Mi", ""),
				Min:                  getResources("", "", "100Mi", ""),
				MaxLimitRequestRatio: getResources("", "", "", ""),
			}, {
				Type:                 core.LimitTypeContainer,
				Max:                  getResources("", "", "10000Mi", ""),
				Min:                  getResources("", "", "100Mi", ""),
				Default:              getResources("", "", "500Mi", ""),
				DefaultRequest:       getResources("", "", "200Mi", ""),
				MaxLimitRequestRatio: getResources("", "", "", ""),
			}},
		},
	},
	}

	for _, testCase := range testCases {
		limitRange := &core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: testCase.name, Namespace: "foo"}, Spec: testCase.spec}
		if errs := ValidateLimitRange(limitRange); len(errs) != 0 {
			t.Errorf("Case %v, unexpected error: %v", testCase.name, errs)
		}
	}
}

func TestValidateLimitRange(t *testing.T) {
	successCases := []struct {
		name string
		spec core.LimitRangeSpec
	}{{
		name: "all-fields-valid",
		spec: core.LimitRangeSpec{
			Limits: []core.LimitRangeItem{{
				Type:                 core.LimitTypePod,
				Max:                  getResources("100m", "10000Mi", "", ""),
				Min:                  getResources("5m", "100Mi", "", ""),
				MaxLimitRequestRatio: getResources("10", "", "", ""),
			}, {
				Type:                 core.LimitTypeContainer,
				Max:                  getResources("100m", "10000Mi", "", ""),
				Min:                  getResources("5m", "100Mi", "", ""),
				Default:              getResources("50m", "500Mi", "", ""),
				DefaultRequest:       getResources("10m", "200Mi", "", ""),
				MaxLimitRequestRatio: getResources("10", "", "", ""),
			}, {
				Type: core.LimitTypePersistentVolumeClaim,
				Max:  getResources("", "", "", "10Gi"),
				Min:  getResources("", "", "", "5Gi"),
			}},
		},
	}, {
		name: "pvc-min-only",
		spec: core.LimitRangeSpec{
			Limits: []core.LimitRangeItem{{
				Type: core.LimitTypePersistentVolumeClaim,
				Min:  getResources("", "", "", "5Gi"),
			}},
		},
	}, {
		name: "pvc-max-only",
		spec: core.LimitRangeSpec{
			Limits: []core.LimitRangeItem{{
				Type: core.LimitTypePersistentVolumeClaim,
				Max:  getResources("", "", "", "10Gi"),
			}},
		},
	}, {
		name: "all-fields-valid-big-numbers",
		spec: core.LimitRangeSpec{
			Limits: []core.LimitRangeItem{{
				Type:                 core.LimitTypeContainer,
				Max:                  getResources("100m", "10000T", "", ""),
				Min:                  getResources("5m", "100Mi", "", ""),
				Default:              getResources("50m", "500Mi", "", ""),
				DefaultRequest:       getResources("10m", "200Mi", "", ""),
				MaxLimitRequestRatio: getResources("10", "", "", ""),
			}},
		},
	}, {
		name: "thirdparty-fields-all-valid-standard-container-resources",
		spec: core.LimitRangeSpec{
			Limits: []core.LimitRangeItem{{
				Type:                 "thirdparty.com/foo",
				Max:                  getResources("100m", "10000T", "", ""),
				Min:                  getResources("5m", "100Mi", "", ""),
				Default:              getResources("50m", "500Mi", "", ""),
				DefaultRequest:       getResources("10m", "200Mi", "", ""),
				MaxLimitRequestRatio: getResources("10", "", "", ""),
			}},
		},
	}, {
		name: "thirdparty-fields-all-valid-storage-resources",
		spec: core.LimitRangeSpec{
			Limits: []core.LimitRangeItem{{
				Type:                 "thirdparty.com/foo",
				Max:                  getResources("", "", "", "10000T"),
				Min:                  getResources("", "", "", "100Mi"),
				Default:              getResources("", "", "", "500Mi"),
				DefaultRequest:       getResources("", "", "", "200Mi"),
				MaxLimitRequestRatio: getResources("", "", "", ""),
			}},
		},
	},
	}

	for _, successCase := range successCases {
		limitRange := &core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: successCase.name, Namespace: "foo"}, Spec: successCase.spec}
		if errs := ValidateLimitRange(limitRange); len(errs) != 0 {
			t.Errorf("Case %v, unexpected error: %v", successCase.name, errs)
		}
	}

	errorCases := map[string]struct {
		R core.LimitRange
		D string
	}{
		"zero-length-name": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: "foo"}, Spec: core.LimitRangeSpec{}},
			"name or generateName is required",
		},
		"zero-length-namespace": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: ""}, Spec: core.LimitRangeSpec{}},
			"",
		},
		"invalid-name": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "^Invalid", Namespace: "foo"}, Spec: core.LimitRangeSpec{}},
			dnsSubdomainLabelErrMsg,
		},
		"invalid-namespace": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "^Invalid"}, Spec: core.LimitRangeSpec{}},
			dnsLabelErrMsg,
		},
		"duplicate-limit-type": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type: core.LimitTypePod,
					Max:  getResources("100m", "10000m", "", ""),
					Min:  getResources("0m", "100m", "", ""),
				}, {
					Type: core.LimitTypePod,
					Min:  getResources("0m", "100m", "", ""),
				}},
			}},
			"",
		},
		"default-limit-type-pod": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type:    core.LimitTypePod,
					Max:     getResources("100m", "10000m", "", ""),
					Min:     getResources("0m", "100m", "", ""),
					Default: getResources("10m", "100m", "", ""),
				}},
			}},
			"may not be specified when `type` is 'Pod'",
		},
		"default-request-limit-type-pod": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type:           core.LimitTypePod,
					Max:            getResources("100m", "10000m", "", ""),
					Min:            getResources("0m", "100m", "", ""),
					DefaultRequest: getResources("10m", "100m", "", ""),
				}},
			}},
			"may not be specified when `type` is 'Pod'",
		},
		"min value 100m is greater than max value 10m": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type: core.LimitTypePod,
					Max:  getResources("10m", "", "", ""),
					Min:  getResources("100m", "", "", ""),
				}},
			}},
			"min value 100m is greater than max value 10m",
		},
		"invalid spec default outside range": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type:    core.LimitTypeContainer,
					Max:     getResources("1", "", "", ""),
					Min:     getResources("100m", "", "", ""),
					Default: getResources("2000m", "", "", ""),
				}},
			}},
			"default value 2 is greater than max value 1",
		},
		"invalid spec default request outside range": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type:           core.LimitTypeContainer,
					Max:            getResources("1", "", "", ""),
					Min:            getResources("100m", "", "", ""),
					DefaultRequest: getResources("2000m", "", "", ""),
				}},
			}},
			"default request value 2 is greater than max value 1",
		},
		"invalid spec default request more than default": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type:           core.LimitTypeContainer,
					Max:            getResources("2", "", "", ""),
					Min:            getResources("100m", "", "", ""),
					Default:        getResources("500m", "", "", ""),
					DefaultRequest: getResources("800m", "", "", ""),
				}},
			}},
			"default request value 800m is greater than default limit value 500m",
		},
		"invalid spec maxLimitRequestRatio less than 1": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type:                 core.LimitTypePod,
					MaxLimitRequestRatio: getResources("800m", "", "", ""),
				}},
			}},
			"ratio 800m is less than 1",
		},
		"invalid spec maxLimitRequestRatio greater than max/min": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type:                 core.LimitTypeContainer,
					Max:                  getResources("", "2Gi", "", ""),
					Min:                  getResources("", "512Mi", "", ""),
					MaxLimitRequestRatio: getResources("", "10", "", ""),
				}},
			}},
			"ratio 10 is greater than max/min = 4.000000",
		},
		"invalid non standard limit type": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type:                 "foo",
					Max:                  getResources("", "", "", "10000T"),
					Min:                  getResources("", "", "", "100Mi"),
					Default:              getResources("", "", "", "500Mi"),
					DefaultRequest:       getResources("", "", "", "200Mi"),
					MaxLimitRequestRatio: getResources("", "", "", ""),
				}},
			}},
			"must be a standard limit type or fully qualified",
		},
		"min and max values missing, one required": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type: core.LimitTypePersistentVolumeClaim,
				}},
			}},
			"either minimum or maximum storage value is required, but neither was provided",
		},
		"invalid min greater than max": {
			core.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: core.LimitRangeSpec{
				Limits: []core.LimitRangeItem{{
					Type: core.LimitTypePersistentVolumeClaim,
					Min:  getResources("", "", "", "10Gi"),
					Max:  getResources("", "", "", "1Gi"),
				}},
			}},
			"min value 10Gi is greater than max value 1Gi",
		},
	}

	for k, v := range errorCases {
		errs := ValidateLimitRange(&v.R)
		if len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
		for i := range errs {
			detail := errs[i].Detail
			if !strings.Contains(detail, v.D) {
				t.Errorf("[%s]: expected error detail either empty or %q, got %q", k, v.D, detail)
			}
		}
	}

}

func TestValidatePersistentVolumeClaimStatusUpdate(t *testing.T) {
	validClaim := testVolumeClaim("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	})
	validConditionUpdate := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimPending,
		Conditions: []core.PersistentVolumeClaimCondition{
			{Type: core.PersistentVolumeClaimResizing, Status: core.ConditionTrue},
		},
	})
	validAllocatedResources := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimPending,
		Conditions: []core.PersistentVolumeClaimCondition{
			{Type: core.PersistentVolumeClaimResizing, Status: core.ConditionTrue},
		},
		AllocatedResources: core.ResourceList{
			core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
		},
	})

	invalidAllocatedResources := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimPending,
		Conditions: []core.PersistentVolumeClaimCondition{
			{Type: core.PersistentVolumeClaimResizing, Status: core.ConditionTrue},
		},
		AllocatedResources: core.ResourceList{
			core.ResourceName(core.ResourceStorage): resource.MustParse("-10G"),
		},
	})

	noStoraegeClaimStatus := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimPending,
		AllocatedResources: core.ResourceList{
			core.ResourceName(core.ResourceCPU): resource.MustParse("10G"),
		},
	})
	progressResizeStatus := core.PersistentVolumeClaimControllerResizeInProgress

	invalidResizeStatus := core.ClaimResourceStatus("foo")
	validResizeKeyCustom := core.ResourceName("example.com/foo")
	invalidNativeResizeKey := core.ResourceName("kubernetes.io/foo")

	validResizeStatusPVC := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
	}, core.PersistentVolumeClaimStatus{
		AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
			core.ResourceStorage: progressResizeStatus,
		},
	})

	validResizeStatusControllerResizeFailed := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
	}, core.PersistentVolumeClaimStatus{
		AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
			core.ResourceStorage: core.PersistentVolumeClaimControllerResizeInfeasible,
		},
	})

	validNodeResizePending := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
	}, core.PersistentVolumeClaimStatus{
		AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
			core.ResourceStorage: core.PersistentVolumeClaimNodeResizePending,
		},
	})

	validNodeResizeInProgress := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
	}, core.PersistentVolumeClaimStatus{
		AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
			core.ResourceStorage: core.PersistentVolumeClaimNodeResizeInProgress,
		},
	})

	validNodeResizeFailed := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
	}, core.PersistentVolumeClaimStatus{
		AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
			core.ResourceStorage: core.PersistentVolumeClaimNodeResizeInfeasible,
		},
	})

	invalidResizeStatusPVC := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
	}, core.PersistentVolumeClaimStatus{
		AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
			core.ResourceStorage: invalidResizeStatus,
		},
	})

	invalidNativeResizeStatusPVC := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
	}, core.PersistentVolumeClaimStatus{
		AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
			invalidNativeResizeKey: core.PersistentVolumeClaimNodeResizePending,
		},
	})

	validExternalResizeStatusPVC := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
	}, core.PersistentVolumeClaimStatus{
		AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
			validResizeKeyCustom: core.PersistentVolumeClaimNodeResizePending,
		},
	})

	multipleResourceStatusPVC := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
		},
	}, core.PersistentVolumeClaimStatus{
		AllocatedResources: core.ResourceList{
			core.ResourceStorage: resource.MustParse("5Gi"),
			validResizeKeyCustom: resource.MustParse("10Gi"),
		},
		AllocatedResourceStatuses: map[core.ResourceName]core.ClaimResourceStatus{
			core.ResourceStorage: core.PersistentVolumeClaimControllerResizeInfeasible,
			validResizeKeyCustom: core.PersistentVolumeClaimControllerResizeInProgress,
		},
	})

	invalidNativeResourceAllocatedKey := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimPending,
		Conditions: []core.PersistentVolumeClaimCondition{
			{Type: core.PersistentVolumeClaimResizing, Status: core.ConditionTrue},
		},
		AllocatedResources: core.ResourceList{
			invalidNativeResizeKey: resource.MustParse("14G"),
		},
	})

	validExternalAllocatedResource := testVolumeClaimWithStatus("foo", "ns", core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadWriteOnce,
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
	}, core.PersistentVolumeClaimStatus{
		Phase: core.ClaimPending,
		Conditions: []core.PersistentVolumeClaimCondition{
			{Type: core.PersistentVolumeClaimResizing, Status: core.ConditionTrue},
		},
		AllocatedResources: core.ResourceList{
			validResizeKeyCustom: resource.MustParse("14G"),
		},
	})

	scenarios := map[string]struct {
		isExpectedFailure          bool
		oldClaim                   *core.PersistentVolumeClaim
		newClaim                   *core.PersistentVolumeClaim
		enableRecoverFromExpansion bool
	}{
		"condition-update-with-enabled-feature-gate": {
			isExpectedFailure: false,
			oldClaim:          validClaim,
			newClaim:          validConditionUpdate,
		},
		"status-update-with-valid-allocatedResources-feature-enabled": {
			isExpectedFailure:          false,
			oldClaim:                   validClaim,
			newClaim:                   validAllocatedResources,
			enableRecoverFromExpansion: true,
		},
		"status-update-with-invalid-allocatedResources-native-key-feature-enabled": {
			isExpectedFailure:          true,
			oldClaim:                   validClaim,
			newClaim:                   invalidNativeResourceAllocatedKey,
			enableRecoverFromExpansion: true,
		},
		"status-update-with-valid-allocatedResources-external-key-feature-enabled": {
			isExpectedFailure:          false,
			oldClaim:                   validClaim,
			newClaim:                   validExternalAllocatedResource,
			enableRecoverFromExpansion: true,
		},

		"status-update-with-invalid-allocatedResources-feature-enabled": {
			isExpectedFailure:          true,
			oldClaim:                   validClaim,
			newClaim:                   invalidAllocatedResources,
			enableRecoverFromExpansion: true,
		},
		"status-update-with-no-storage-update": {
			isExpectedFailure:          true,
			oldClaim:                   validClaim,
			newClaim:                   noStoraegeClaimStatus,
			enableRecoverFromExpansion: true,
		},
		"staus-update-with-controller-resize-failed": {
			isExpectedFailure:          false,
			oldClaim:                   validClaim,
			newClaim:                   validResizeStatusControllerResizeFailed,
			enableRecoverFromExpansion: true,
		},
		"staus-update-with-node-resize-pending": {
			isExpectedFailure:          false,
			oldClaim:                   validClaim,
			newClaim:                   validNodeResizePending,
			enableRecoverFromExpansion: true,
		},
		"staus-update-with-node-resize-inprogress": {
			isExpectedFailure:          false,
			oldClaim:                   validClaim,
			newClaim:                   validNodeResizeInProgress,
			enableRecoverFromExpansion: true,
		},
		"staus-update-with-node-resize-failed": {
			isExpectedFailure:          false,
			oldClaim:                   validClaim,
			newClaim:                   validNodeResizeFailed,
			enableRecoverFromExpansion: true,
		},
		"staus-update-with-invalid-native-resource-status-key": {
			isExpectedFailure:          true,
			oldClaim:                   validClaim,
			newClaim:                   invalidNativeResizeStatusPVC,
			enableRecoverFromExpansion: true,
		},
		"staus-update-with-valid-external-resource-status-key": {
			isExpectedFailure:          false,
			oldClaim:                   validClaim,
			newClaim:                   validExternalResizeStatusPVC,
			enableRecoverFromExpansion: true,
		},
		"status-update-with-multiple-resources-key": {
			isExpectedFailure:          false,
			oldClaim:                   validClaim,
			newClaim:                   multipleResourceStatusPVC,
			enableRecoverFromExpansion: true,
		},
		"status-update-with-valid-pvc-resize-status": {
			isExpectedFailure:          false,
			oldClaim:                   validClaim,
			newClaim:                   validResizeStatusPVC,
			enableRecoverFromExpansion: true,
		},
		"status-update-with-invalid-pvc-resize-status": {
			isExpectedFailure:          true,
			oldClaim:                   validClaim,
			newClaim:                   invalidResizeStatusPVC,
			enableRecoverFromExpansion: true,
		},
		"status-update-with-old-pvc-valid-resourcestatus-newpvc-invalid-recovery-disabled": {
			isExpectedFailure:          true,
			oldClaim:                   validResizeStatusPVC,
			newClaim:                   invalidResizeStatusPVC,
			enableRecoverFromExpansion: false,
		},
		"status-update-with-old-pvc-valid-allocatedResource-newpvc-invalid-recovery-disabled": {
			isExpectedFailure:          true,
			oldClaim:                   validExternalAllocatedResource,
			newClaim:                   invalidNativeResourceAllocatedKey,
			enableRecoverFromExpansion: false,
		},
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RecoverVolumeExpansionFailure, scenario.enableRecoverFromExpansion)

			validateOpts := ValidationOptionsForPersistentVolumeClaim(scenario.newClaim, scenario.oldClaim)

			// ensure we have a resource version specified for updates
			scenario.oldClaim.ResourceVersion = "1"
			scenario.newClaim.ResourceVersion = "1"
			errs := ValidatePersistentVolumeClaimStatusUpdate(scenario.newClaim, scenario.oldClaim, validateOpts)
			if len(errs) == 0 && scenario.isExpectedFailure {
				t.Errorf("Unexpected success for scenario: %s", name)
			}
			if len(errs) > 0 && !scenario.isExpectedFailure {
				t.Errorf("Unexpected failure for scenario: %s - %+v", name, errs)
			}
		})
	}
}

func TestValidateResourceQuota(t *testing.T) {
	spec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU:                    resource.MustParse("100"),
			core.ResourceMemory:                 resource.MustParse("10000"),
			core.ResourceRequestsCPU:            resource.MustParse("100"),
			core.ResourceRequestsMemory:         resource.MustParse("10000"),
			core.ResourceLimitsCPU:              resource.MustParse("100"),
			core.ResourceLimitsMemory:           resource.MustParse("10000"),
			core.ResourcePods:                   resource.MustParse("10"),
			core.ResourceServices:               resource.MustParse("0"),
			core.ResourceReplicationControllers: resource.MustParse("10"),
			core.ResourceQuotas:                 resource.MustParse("10"),
			core.ResourceConfigMaps:             resource.MustParse("10"),
			core.ResourceSecrets:                resource.MustParse("10"),

			// These are unknown and not enforced unless DRA is enabled, but not invalid.
			"count/resourceclaims.resource.k8s.io":     resource.MustParse("1"),
			"gold.deviceclass.resource.k8s.io/devices": resource.MustParse("1"),
		},
	}

	terminatingSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU:       resource.MustParse("100"),
			core.ResourceLimitsCPU: resource.MustParse("200"),
		},
		Scopes: []core.ResourceQuotaScope{core.ResourceQuotaScopeTerminating},
	}

	nonTerminatingSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU: resource.MustParse("100"),
		},
		Scopes: []core.ResourceQuotaScope{core.ResourceQuotaScopeNotTerminating},
	}

	bestEffortSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourcePods: resource.MustParse("100"),
		},
		Scopes: []core.ResourceQuotaScope{core.ResourceQuotaScopeBestEffort},
	}

	nonBestEffortSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU: resource.MustParse("100"),
		},
		Scopes: []core.ResourceQuotaScope{core.ResourceQuotaScopeNotBestEffort},
	}

	crossNamespaceAffinitySpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU:       resource.MustParse("100"),
			core.ResourceLimitsCPU: resource.MustParse("200"),
		},
		Scopes: []core.ResourceQuotaScope{core.ResourceQuotaScopeCrossNamespacePodAffinity},
	}

	scopeSelectorSpec := core.ResourceQuotaSpec{
		ScopeSelector: &core.ScopeSelector{
			MatchExpressions: []core.ScopedResourceSelectorRequirement{{
				ScopeName: core.ResourceQuotaScopePriorityClass,
				Operator:  core.ScopeSelectorOpIn,
				Values:    []string{"cluster-services"},
			}},
		},
	}

	// storage is not yet supported as a quota tracked resource
	invalidQuotaResourceSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceStorage: resource.MustParse("10"),
		},
	}

	negativeSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU:                    resource.MustParse("-100"),
			core.ResourceMemory:                 resource.MustParse("-10000"),
			core.ResourcePods:                   resource.MustParse("-10"),
			core.ResourceServices:               resource.MustParse("-10"),
			core.ResourceReplicationControllers: resource.MustParse("-10"),
			core.ResourceQuotas:                 resource.MustParse("-10"),
			core.ResourceConfigMaps:             resource.MustParse("-10"),
			core.ResourceSecrets:                resource.MustParse("-10"),
		},
	}

	fractionalComputeSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU: resource.MustParse("100m"),
		},
	}

	fractionalPodSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourcePods:                   resource.MustParse(".1"),
			core.ResourceServices:               resource.MustParse(".5"),
			core.ResourceReplicationControllers: resource.MustParse("1.25"),
			core.ResourceQuotas:                 resource.MustParse("2.5"),
		},
	}

	invalidTerminatingScopePairsSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU: resource.MustParse("100"),
		},
		Scopes: []core.ResourceQuotaScope{core.ResourceQuotaScopeTerminating, core.ResourceQuotaScopeNotTerminating},
	}

	invalidBestEffortScopePairsSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourcePods: resource.MustParse("100"),
		},
		Scopes: []core.ResourceQuotaScope{core.ResourceQuotaScopeBestEffort, core.ResourceQuotaScopeNotBestEffort},
	}

	invalidCrossNamespaceAffinitySpec := core.ResourceQuotaSpec{
		ScopeSelector: &core.ScopeSelector{
			MatchExpressions: []core.ScopedResourceSelectorRequirement{{
				ScopeName: core.ResourceQuotaScopeCrossNamespacePodAffinity,
				Operator:  core.ScopeSelectorOpIn,
				Values:    []string{"cluster-services"},
			}},
		},
	}

	invalidScopeNameSpec := core.ResourceQuotaSpec{
		Hard: core.ResourceList{
			core.ResourceCPU: resource.MustParse("100"),
		},
		Scopes: []core.ResourceQuotaScope{core.ResourceQuotaScope("foo")},
	}

	testCases := map[string]struct {
		rq        core.ResourceQuota
		errDetail string
		errField  string
	}{
		"no-scope": {
			rq: core.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "abc",
					Namespace: "foo",
				},
				Spec: spec,
			},
		},
		"fractional-compute-spec": {
			rq: core.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "abc",
					Namespace: "foo",
				},
				Spec: fractionalComputeSpec,
			},
		},
		"terminating-spec": {
			rq: core.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "abc",
					Namespace: "foo",
				},
				Spec: terminatingSpec,
			},
		},
		"non-terminating-spec": {
			rq: core.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "abc",
					Namespace: "foo",
				},
				Spec: nonTerminatingSpec,
			},
		},
		"best-effort-spec": {
			rq: core.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "abc",
					Namespace: "foo",
				},
				Spec: bestEffortSpec,
			},
		},
		"cross-namespace-affinity-spec": {
			rq: core.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "abc",
					Namespace: "foo",
				},
				Spec: crossNamespaceAffinitySpec,
			},
		},
		"scope-selector-spec": {
			rq: core.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "abc",
					Namespace: "foo",
				},
				Spec: scopeSelectorSpec,
			},
		},
		"non-best-effort-spec": {
			rq: core.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "abc",
					Namespace: "foo",
				},
				Spec: nonBestEffortSpec,
			},
		},
		"zero-length Name": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: "foo"}, Spec: spec},
			errDetail: "name or generateName is required",
		},
		"zero-length Namespace": {
			rq:       core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: ""}, Spec: spec},
			errField: "metadata.namespace",
		},
		"invalid Name": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "^Invalid", Namespace: "foo"}, Spec: spec},
			errDetail: dnsSubdomainLabelErrMsg,
		},
		"invalid Namespace": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "^Invalid"}, Spec: spec},
			errDetail: dnsLabelErrMsg,
		},
		"negative-limits": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: negativeSpec},
			errDetail: isNegativeErrorMsg,
		},
		"fractional-api-resource": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: fractionalPodSpec},
			errDetail: isNotIntegerErrorMsg,
		},
		"invalid-quota-resource": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: invalidQuotaResourceSpec},
			errDetail: isInvalidQuotaResource,
		},
		"invalid-quota-terminating-pair": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: invalidTerminatingScopePairsSpec},
			errDetail: "conflicting scopes",
		},
		"invalid-quota-besteffort-pair": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: invalidBestEffortScopePairsSpec},
			errDetail: "conflicting scopes",
		},
		"invalid-quota-scope-name": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: invalidScopeNameSpec},
			errDetail: "unsupported scope",
		},
		"invalid-cross-namespace-affinity": {
			rq:        core.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "foo"}, Spec: invalidCrossNamespaceAffinitySpec},
			errDetail: "must be 'Exists' when scope is any of ResourceQuotaScopeTerminating, ResourceQuotaScopeNotTerminating, ResourceQuotaScopeBestEffort, ResourceQuotaScopeNotBestEffort or ResourceQuotaScopeCrossNamespacePodAffinity",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			errs := ValidateResourceQuota(&tc.rq)
			if len(tc.errDetail) == 0 && len(tc.errField) == 0 && len(errs) != 0 {
				t.Errorf("expected success: %v", errs)
			} else if (len(tc.errDetail) != 0 || len(tc.errField) != 0) && len(errs) == 0 {
				t.Errorf("expected failure")
			} else {
				for i := range errs {
					if !strings.Contains(errs[i].Detail, tc.errDetail) {
						t.Errorf("expected error detail either empty or %s, got %s", tc.errDetail, errs[i].Detail)
					}
				}
			}
		})
	}
}

func TestValidateNamespace(t *testing.T) {
	validLabels := map[string]string{"a": "b"}
	invalidLabels := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}
	successCases := []core.Namespace{{
		ObjectMeta: metav1.ObjectMeta{Name: "abc", Labels: validLabels},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "abc-123"},
		Spec: core.NamespaceSpec{
			Finalizers: []core.FinalizerName{"example.com/something", "example.com/other"},
		},
	},
	}
	for _, successCase := range successCases {
		if errs := ValidateNamespace(&successCase); len(errs) != 0 {
			t.Errorf("expected success: %v", errs)
		}
	}
	errorCases := map[string]struct {
		R core.Namespace
		D string
	}{
		"zero-length name": {
			core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ""}},
			"",
		},
		"defined-namespace": {
			core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "abc-123", Namespace: "makesnosense"}},
			"",
		},
		"invalid-labels": {
			core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "abc", Labels: invalidLabels}},
			"",
		},
	}
	for k, v := range errorCases {
		errs := ValidateNamespace(&v.R)
		if len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		}
	}
}

func TestValidateNamespaceFinalizeUpdate(t *testing.T) {
	tests := []struct {
		oldNamespace core.Namespace
		namespace    core.Namespace
		valid        bool
	}{
		{core.Namespace{}, core.Namespace{}, true},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo"}},
			core.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo"},
				Spec: core.NamespaceSpec{
					Finalizers: []core.FinalizerName{"Foo"},
				},
			}, false},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo"},
			Spec: core.NamespaceSpec{
				Finalizers: []core.FinalizerName{"foo.com/bar"},
			},
		},
			core.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo"},
				Spec: core.NamespaceSpec{
					Finalizers: []core.FinalizerName{"foo.com/bar", "what.com/bar"},
				},
			}, true},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "fooemptyfinalizer"},
			Spec: core.NamespaceSpec{
				Finalizers: []core.FinalizerName{"foo.com/bar"},
			},
		},
			core.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "fooemptyfinalizer"},
				Spec: core.NamespaceSpec{
					Finalizers: []core.FinalizerName{"", "foo.com/bar", "what.com/bar"},
				},
			}, false},
	}
	for i, test := range tests {
		test.namespace.ObjectMeta.ResourceVersion = "1"
		test.oldNamespace.ObjectMeta.ResourceVersion = "1"
		errs := ValidateNamespaceFinalizeUpdate(&test.namespace, &test.oldNamespace)
		if test.valid && len(errs) > 0 {
			t.Errorf("%d: Unexpected error: %v", i, errs)
			t.Logf("%#v vs %#v", test.oldNamespace, test.namespace)
		}
		if !test.valid && len(errs) == 0 {
			t.Errorf("%d: Unexpected non-error", i)
		}
	}
}

func TestValidateNamespaceStatusUpdate(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		oldNamespace core.Namespace
		namespace    core.Namespace
		valid        bool
	}{
		{core.Namespace{}, core.Namespace{
			Status: core.NamespaceStatus{
				Phase: core.NamespaceActive,
			},
		}, true},
		// Cannot set deletionTimestamp via status update
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo"}},
			core.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "foo",
					DeletionTimestamp: &now},
				Status: core.NamespaceStatus{
					Phase: core.NamespaceTerminating,
				},
			}, false},
		// Can update phase via status update
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "foo",
				DeletionTimestamp: &now}},
			core.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "foo",
					DeletionTimestamp: &now},
				Status: core.NamespaceStatus{
					Phase: core.NamespaceTerminating,
				},
			}, true},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo"}},
			core.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo"},
				Status: core.NamespaceStatus{
					Phase: core.NamespaceTerminating,
				},
			}, false},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo"}},
			core.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bar"},
				Status: core.NamespaceStatus{
					Phase: core.NamespaceTerminating,
				},
			}, false},
	}
	for i, test := range tests {
		test.namespace.ObjectMeta.ResourceVersion = "1"
		test.oldNamespace.ObjectMeta.ResourceVersion = "1"
		errs := ValidateNamespaceStatusUpdate(&test.namespace, &test.oldNamespace)
		if test.valid && len(errs) > 0 {
			t.Errorf("%d: Unexpected error: %v", i, errs)
			t.Logf("%#v vs %#v", test.oldNamespace.ObjectMeta, test.namespace.ObjectMeta)
		}
		if !test.valid && len(errs) == 0 {
			t.Errorf("%d: Unexpected non-error", i)
		}
	}
}

func TestValidateNamespaceUpdate(t *testing.T) {
	tests := []struct {
		oldNamespace core.Namespace
		namespace    core.Namespace
		valid        bool
	}{
		{core.Namespace{}, core.Namespace{}, true},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo1"}},
			core.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bar1"},
			}, false},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo2",
				Labels: map[string]string{"foo": "bar"},
			},
		}, core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo2",
				Labels: map[string]string{"foo": "baz"},
			},
		}, true},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo3",
			},
		}, core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo3",
				Labels: map[string]string{"foo": "baz"},
			},
		}, true},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo4",
				Labels: map[string]string{"bar": "foo"},
			},
		}, core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo4",
				Labels: map[string]string{"foo": "baz"},
			},
		}, true},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo5",
				Labels: map[string]string{"foo": "baz"},
			},
		}, core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo5",
				Labels: map[string]string{"Foo": "baz"},
			},
		}, true},
		{core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo6",
				Labels: map[string]string{"foo": "baz"},
			},
		}, core.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "foo6",
				Labels: map[string]string{"Foo": "baz"},
			},
			Spec: core.NamespaceSpec{
				Finalizers: []core.FinalizerName{"kubernetes"},
			},
			Status: core.NamespaceStatus{
				Phase: core.NamespaceTerminating,
			},
		}, true},
	}
	for i, test := range tests {
		test.namespace.ObjectMeta.ResourceVersion = "1"
		test.oldNamespace.ObjectMeta.ResourceVersion = "1"
		errs := ValidateNamespaceUpdate(&test.namespace, &test.oldNamespace)
		if test.valid && len(errs) > 0 {
			t.Errorf("%d: Unexpected error: %v", i, errs)
			t.Logf("%#v vs %#v", test.oldNamespace.ObjectMeta, test.namespace.ObjectMeta)
		}
		if !test.valid && len(errs) == 0 {
			t.Errorf("%d: Unexpected non-error", i)
		}
	}
}

func TestValidateSecret(t *testing.T) {
	// Opaque secret validation
	validSecret := func() core.Secret {
		return core.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
			Data: map[string][]byte{
				"data-1": []byte("bar"),
			},
		}
	}

	var (
		emptyName     = validSecret()
		invalidName   = validSecret()
		emptyNs       = validSecret()
		invalidNs     = validSecret()
		overMaxSize   = validSecret()
		invalidKey    = validSecret()
		leadingDotKey = validSecret()
		dotKey        = validSecret()
		doubleDotKey  = validSecret()
	)

	emptyName.Name = ""
	invalidName.Name = "NoUppercaseOrSpecialCharsLike=Equals"
	emptyNs.Namespace = ""
	invalidNs.Namespace = "NoUppercaseOrSpecialCharsLike=Equals"
	overMaxSize.Data = map[string][]byte{
		"over": make([]byte, core.MaxSecretSize+1),
	}
	invalidKey.Data["a*b"] = []byte("whoops")
	leadingDotKey.Data[".key"] = []byte("bar")
	dotKey.Data["."] = []byte("bar")
	doubleDotKey.Data[".."] = []byte("bar")

	// kubernetes.io/service-account-token secret validation
	validServiceAccountTokenSecret := func() core.Secret {
		return core.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "bar",
				Annotations: map[string]string{
					core.ServiceAccountNameKey: "foo",
				},
			},
			Type: core.SecretTypeServiceAccountToken,
			Data: map[string][]byte{
				"data-1": []byte("bar"),
			},
		}
	}

	var (
		emptyTokenAnnotation    = validServiceAccountTokenSecret()
		missingTokenAnnotation  = validServiceAccountTokenSecret()
		missingTokenAnnotations = validServiceAccountTokenSecret()
	)
	emptyTokenAnnotation.Annotations[core.ServiceAccountNameKey] = ""
	delete(missingTokenAnnotation.Annotations, core.ServiceAccountNameKey)
	missingTokenAnnotations.Annotations = nil

	tests := map[string]struct {
		secret core.Secret
		valid  bool
	}{
		"valid":                                     {validSecret(), true},
		"empty name":                                {emptyName, false},
		"invalid name":                              {invalidName, false},
		"empty namespace":                           {emptyNs, false},
		"invalid namespace":                         {invalidNs, false},
		"over max size":                             {overMaxSize, false},
		"invalid key":                               {invalidKey, false},
		"valid service-account-token secret":        {validServiceAccountTokenSecret(), true},
		"empty service-account-token annotation":    {emptyTokenAnnotation, false},
		"missing service-account-token annotation":  {missingTokenAnnotation, false},
		"missing service-account-token annotations": {missingTokenAnnotations, false},
		"leading dot key":                           {leadingDotKey, true},
		"dot key":                                   {dotKey, false},
		"double dot key":                            {doubleDotKey, false},
	}

	for name, tc := range tests {
		errs := ValidateSecret(&tc.secret)
		if tc.valid && len(errs) > 0 {
			t.Errorf("%v: Unexpected error: %v", name, errs)
		}
		if !tc.valid && len(errs) == 0 {
			t.Errorf("%v: Unexpected non-error", name)
		}
	}
}

func TestValidateSecretUpdate(t *testing.T) {
	validSecret := func() core.Secret {
		return core.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "foo",
				Namespace:       "bar",
				ResourceVersion: "20",
			},
			Data: map[string][]byte{
				"data-1": []byte("bar"),
			},
		}
	}

	falseVal := false
	trueVal := true

	secret := validSecret()
	immutableSecret := validSecret()
	immutableSecret.Immutable = &trueVal
	mutableSecret := validSecret()
	mutableSecret.Immutable = &falseVal

	secretWithData := validSecret()
	secretWithData.Data["data-2"] = []byte("baz")
	immutableSecretWithData := validSecret()
	immutableSecretWithData.Immutable = &trueVal
	immutableSecretWithData.Data["data-2"] = []byte("baz")

	secretWithChangedData := validSecret()
	secretWithChangedData.Data["data-1"] = []byte("foo")
	immutableSecretWithChangedData := validSecret()
	immutableSecretWithChangedData.Immutable = &trueVal
	immutableSecretWithChangedData.Data["data-1"] = []byte("foo")

	tests := []struct {
		name      string
		oldSecret core.Secret
		newSecret core.Secret
		valid     bool
	}{{
		name:      "mark secret immutable",
		oldSecret: secret,
		newSecret: immutableSecret,
		valid:     true,
	}, {
		name:      "revert immutable secret",
		oldSecret: immutableSecret,
		newSecret: secret,
		valid:     false,
	}, {
		name:      "makr immutable secret mutable",
		oldSecret: immutableSecret,
		newSecret: mutableSecret,
		valid:     false,
	}, {
		name:      "add data in secret",
		oldSecret: secret,
		newSecret: secretWithData,
		valid:     true,
	}, {
		name:      "add data in immutable secret",
		oldSecret: immutableSecret,
		newSecret: immutableSecretWithData,
		valid:     false,
	}, {
		name:      "change data in secret",
		oldSecret: secret,
		newSecret: secretWithChangedData,
		valid:     true,
	}, {
		name:      "change data in immutable secret",
		oldSecret: immutableSecret,
		newSecret: immutableSecretWithChangedData,
		valid:     false,
	},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateSecretUpdate(&tc.newSecret, &tc.oldSecret)
			if tc.valid && len(errs) > 0 {
				t.Errorf("Unexpected error: %v", errs)
			}
			if !tc.valid && len(errs) == 0 {
				t.Errorf("Unexpected lack of error")
			}
		})
	}
}

func TestValidateDockerConfigSecret(t *testing.T) {
	validDockerSecret := func() core.Secret {
		return core.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
			Type:       core.SecretTypeDockercfg,
			Data: map[string][]byte{
				core.DockerConfigKey: []byte(`{"https://index.docker.io/v1/": {"auth": "Y2x1ZWRyb29sZXIwMDAxOnBhc3N3b3Jk","email": "fake@example.com"}}`),
			},
		}
	}
	validDockerSecret2 := func() core.Secret {
		return core.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
			Type:       core.SecretTypeDockerConfigJSON,
			Data: map[string][]byte{
				core.DockerConfigJSONKey: []byte(`{"auths":{"https://index.docker.io/v1/": {"auth": "Y2x1ZWRyb29sZXIwMDAxOnBhc3N3b3Jk","email": "fake@example.com"}}}`),
			},
		}
	}

	var (
		missingDockerConfigKey  = validDockerSecret()
		emptyDockerConfigKey    = validDockerSecret()
		invalidDockerConfigKey  = validDockerSecret()
		missingDockerConfigKey2 = validDockerSecret2()
		emptyDockerConfigKey2   = validDockerSecret2()
		invalidDockerConfigKey2 = validDockerSecret2()
	)

	delete(missingDockerConfigKey.Data, core.DockerConfigKey)
	emptyDockerConfigKey.Data[core.DockerConfigKey] = []byte("")
	invalidDockerConfigKey.Data[core.DockerConfigKey] = []byte("bad")
	delete(missingDockerConfigKey2.Data, core.DockerConfigJSONKey)
	emptyDockerConfigKey2.Data[core.DockerConfigJSONKey] = []byte("")
	invalidDockerConfigKey2.Data[core.DockerConfigJSONKey] = []byte("bad")

	tests := map[string]struct {
		secret core.Secret
		valid  bool
	}{
		"valid dockercfg":     {validDockerSecret(), true},
		"missing dockercfg":   {missingDockerConfigKey, false},
		"empty dockercfg":     {emptyDockerConfigKey, false},
		"invalid dockercfg":   {invalidDockerConfigKey, false},
		"valid config.json":   {validDockerSecret2(), true},
		"missing config.json": {missingDockerConfigKey2, false},
		"empty config.json":   {emptyDockerConfigKey2, false},
		"invalid config.json": {invalidDockerConfigKey2, false},
	}

	for name, tc := range tests {
		errs := ValidateSecret(&tc.secret)
		if tc.valid && len(errs) > 0 {
			t.Errorf("%v: Unexpected error: %v", name, errs)
		}
		if !tc.valid && len(errs) == 0 {
			t.Errorf("%v: Unexpected non-error", name)
		}
	}
}

func TestValidateBasicAuthSecret(t *testing.T) {
	validBasicAuthSecret := func() core.Secret {
		return core.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
			Type:       core.SecretTypeBasicAuth,
			Data: map[string][]byte{
				core.BasicAuthUsernameKey: []byte("username"),
				core.BasicAuthPasswordKey: []byte("password"),
			},
		}
	}

	var (
		missingBasicAuthUsernamePasswordKeys = validBasicAuthSecret()
	)

	delete(missingBasicAuthUsernamePasswordKeys.Data, core.BasicAuthUsernameKey)
	delete(missingBasicAuthUsernamePasswordKeys.Data, core.BasicAuthPasswordKey)

	tests := map[string]struct {
		secret core.Secret
		valid  bool
	}{
		"valid":                         {validBasicAuthSecret(), true},
		"missing username and password": {missingBasicAuthUsernamePasswordKeys, false},
	}

	for name, tc := range tests {
		errs := ValidateSecret(&tc.secret)
		if tc.valid && len(errs) > 0 {
			t.Errorf("%v: Unexpected error: %v", name, errs)
		}
		if !tc.valid && len(errs) == 0 {
			t.Errorf("%v: Unexpected non-error", name)
		}
	}
}

func TestValidateSSHAuthSecret(t *testing.T) {
	validSSHAuthSecret := func() core.Secret {
		return core.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
			Type:       core.SecretTypeSSHAuth,
			Data: map[string][]byte{
				core.SSHAuthPrivateKey: []byte("foo-bar-baz"),
			},
		}
	}

	missingSSHAuthPrivateKey := validSSHAuthSecret()

	delete(missingSSHAuthPrivateKey.Data, core.SSHAuthPrivateKey)

	tests := map[string]struct {
		secret core.Secret
		valid  bool
	}{
		"valid":               {validSSHAuthSecret(), true},
		"missing private key": {missingSSHAuthPrivateKey, false},
	}

	for name, tc := range tests {
		errs := ValidateSecret(&tc.secret)
		if tc.valid && len(errs) > 0 {
			t.Errorf("%v: Unexpected error: %v", name, errs)
		}
		if !tc.valid && len(errs) == 0 {
			t.Errorf("%v: Unexpected non-error", name)
		}
	}
}

func TestValidateEndpointsCreate(t *testing.T) {
	successCases := map[string]struct {
		endpoints core.Endpoints
	}{
		"simple endpoint": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.1.1"}, {IP: "10.10.2.2"}},
					Ports:     []core.EndpointPort{{Name: "a", Port: 8675, Protocol: "TCP"}, {Name: "b", Port: 309, Protocol: "TCP"}},
				}, {
					Addresses: []core.EndpointAddress{{IP: "10.10.3.3"}},
					Ports:     []core.EndpointPort{{Name: "a", Port: 93, Protocol: "TCP"}, {Name: "b", Port: 76, Protocol: "TCP"}},
				}},
			},
		},
		"empty subsets": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
			},
		},
		"no name required for singleton port": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.1.1"}},
					Ports:     []core.EndpointPort{{Port: 8675, Protocol: "TCP"}},
				}},
			},
		},
		"valid appProtocol": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.1.1"}},
					Ports:     []core.EndpointPort{{Port: 8675, Protocol: "TCP", AppProtocol: utilpointer.String("HTTP")}},
				}},
			},
		},
		"empty ports": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.3.3"}},
				}},
			},
		},
	}

	for name, tc := range successCases {
		t.Run(name, func(t *testing.T) {
			errs := ValidateEndpointsCreate(&tc.endpoints)
			if len(errs) != 0 {
				t.Errorf("Expected no validation errors, got %v", errs)
			}

		})
	}

	errorCases := map[string]struct {
		endpoints   core.Endpoints
		errorType   field.ErrorType
		errorDetail string
	}{
		"missing namespace": {
			endpoints: core.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "mysvc"}},
			errorType: "FieldValueRequired",
		},
		"missing name": {
			endpoints: core.Endpoints{ObjectMeta: metav1.ObjectMeta{Namespace: "namespace"}},
			errorType: "FieldValueRequired",
		},
		"invalid namespace": {
			endpoints:   core.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "no@#invalid.;chars\"allowed"}},
			errorType:   "FieldValueInvalid",
			errorDetail: dnsLabelErrMsg,
		},
		"invalid name": {
			endpoints:   core.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "-_Invliad^&Characters", Namespace: "namespace"}},
			errorType:   "FieldValueInvalid",
			errorDetail: dnsSubdomainLabelErrMsg,
		},
		"empty addresses": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Ports: []core.EndpointPort{{Name: "a", Port: 93, Protocol: "TCP"}},
				}},
			},
			errorType: "FieldValueRequired",
		},
		"invalid IP": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "[2001:0db8:85a3:0042:1000:8a2e:0370:7334]"}},
					Ports:     []core.EndpointPort{{Name: "a", Port: 93, Protocol: "TCP"}},
				}},
			},
			errorType:   "FieldValueInvalid",
			errorDetail: "must be a valid IP address",
		},
		"Multiple ports, one without name": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.1.1"}},
					Ports:     []core.EndpointPort{{Port: 8675, Protocol: "TCP"}, {Name: "b", Port: 309, Protocol: "TCP"}},
				}},
			},
			errorType: "FieldValueRequired",
		},
		"Invalid port number": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.1.1"}},
					Ports:     []core.EndpointPort{{Name: "a", Port: 66000, Protocol: "TCP"}},
				}},
			},
			errorType:   "FieldValueInvalid",
			errorDetail: "between",
		},
		"Invalid protocol": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.1.1"}},
					Ports:     []core.EndpointPort{{Name: "a", Port: 93, Protocol: "Protocol"}},
				}},
			},
			errorType: "FieldValueNotSupported",
		},
		"Address missing IP": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{}},
					Ports:     []core.EndpointPort{{Name: "a", Port: 93, Protocol: "TCP"}},
				}},
			},
			errorType:   "FieldValueInvalid",
			errorDetail: "must be a valid IP address",
		},
		"Port missing number": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.1.1"}},
					Ports:     []core.EndpointPort{{Name: "a", Protocol: "TCP"}},
				}},
			},
			errorType:   "FieldValueInvalid",
			errorDetail: "between",
		},
		"Port missing protocol": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.1.1"}},
					Ports:     []core.EndpointPort{{Name: "a", Port: 93}},
				}},
			},
			errorType: "FieldValueRequired",
		},
		"Address is loopback": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "127.0.0.1"}},
					Ports:     []core.EndpointPort{{Name: "p", Port: 93, Protocol: "TCP"}},
				}},
			},
			errorType:   "FieldValueInvalid",
			errorDetail: "loopback",
		},
		"Address is link-local": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "169.254.169.254"}},
					Ports:     []core.EndpointPort{{Name: "p", Port: 93, Protocol: "TCP"}},
				}},
			},
			errorType:   "FieldValueInvalid",
			errorDetail: "link-local",
		},
		"Address is link-local multicast": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "224.0.0.1"}},
					Ports:     []core.EndpointPort{{Name: "p", Port: 93, Protocol: "TCP"}},
				}},
			},
			errorType:   "FieldValueInvalid",
			errorDetail: "link-local multicast",
		},
		"Invalid AppProtocol": {
			endpoints: core.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace"},
				Subsets: []core.EndpointSubset{{
					Addresses: []core.EndpointAddress{{IP: "10.10.1.1"}},
					Ports:     []core.EndpointPort{{Name: "p", Port: 93, Protocol: "TCP", AppProtocol: utilpointer.String("lots-of[invalid]-{chars}")}},
				}},
			},
			errorType:   "FieldValueInvalid",
			errorDetail: "name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character",
		},
	}

	for k, v := range errorCases {
		t.Run(k, func(t *testing.T) {
			if errs := ValidateEndpointsCreate(&v.endpoints); len(errs) == 0 || errs[0].Type != v.errorType || !strings.Contains(errs[0].Detail, v.errorDetail) {
				t.Errorf("Expected error type %s with detail %q, got %v", v.errorType, v.errorDetail, errs)
			}
		})
	}
}

func TestValidateEndpointsUpdate(t *testing.T) {
	baseEndpoints := core.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "mysvc", Namespace: "namespace", ResourceVersion: "1234"},
		Subsets: []core.EndpointSubset{{
			Addresses: []core.EndpointAddress{{IP: "10.1.2.3"}},
		}},
	}

	testCases := map[string]struct {
		tweakOldEndpoints func(ep *core.Endpoints)
		tweakNewEndpoints func(ep *core.Endpoints)
		numExpectedErrors int
	}{
		"update to valid app protocol": {
			tweakOldEndpoints: func(ep *core.Endpoints) {
				ep.Subsets[0].Ports = []core.EndpointPort{{Name: "a", Port: 8675, Protocol: "TCP"}}
			},
			tweakNewEndpoints: func(ep *core.Endpoints) {
				ep.Subsets[0].Ports = []core.EndpointPort{{Name: "a", Port: 8675, Protocol: "TCP", AppProtocol: utilpointer.String("https")}}
			},
			numExpectedErrors: 0,
		},
		"update to invalid app protocol": {
			tweakOldEndpoints: func(ep *core.Endpoints) {
				ep.Subsets[0].Ports = []core.EndpointPort{{Name: "a", Port: 8675, Protocol: "TCP"}}
			},
			tweakNewEndpoints: func(ep *core.Endpoints) {
				ep.Subsets[0].Ports = []core.EndpointPort{{Name: "a", Port: 8675, Protocol: "TCP", AppProtocol: utilpointer.String("~https")}}
			},
			numExpectedErrors: 1,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			oldEndpoints := baseEndpoints.DeepCopy()
			tc.tweakOldEndpoints(oldEndpoints)
			newEndpoints := baseEndpoints.DeepCopy()
			tc.tweakNewEndpoints(newEndpoints)

			errs := ValidateEndpointsUpdate(newEndpoints, oldEndpoints)
			if len(errs) != tc.numExpectedErrors {
				t.Errorf("Expected %d validation errors, got %d: %v", tc.numExpectedErrors, len(errs), errs)
			}

		})
	}
}

func TestValidateWindowsSecurityContext(t *testing.T) {
	tests := []struct {
		name        string
		sc          *core.PodSpec
		expectError bool
		errorMsg    string
		errorType   field.ErrorType
	}{{
		name:        "pod with SELinux Options",
		sc:          &core.PodSpec{Containers: []core.Container{{SecurityContext: &core.SecurityContext{SELinuxOptions: &core.SELinuxOptions{Role: "dummy"}}}}},
		expectError: true,
		errorMsg:    "cannot be set for a windows pod",
		errorType:   "FieldValueForbidden",
	}, {
		name:        "pod with SeccompProfile",
		sc:          &core.PodSpec{Containers: []core.Container{{SecurityContext: &core.SecurityContext{SeccompProfile: &core.SeccompProfile{LocalhostProfile: utilpointer.String("dummy")}}}}},
		expectError: true,
		errorMsg:    "cannot be set for a windows pod",
		errorType:   "FieldValueForbidden",
	}, {
		name:        "pod with AppArmorProfile",
		sc:          &core.PodSpec{Containers: []core.Container{{SecurityContext: &core.SecurityContext{AppArmorProfile: &core.AppArmorProfile{Type: core.AppArmorProfileTypeRuntimeDefault}}}}},
		expectError: true,
		errorMsg:    "cannot be set for a windows pod",
		errorType:   "FieldValueForbidden",
	}, {
		name:        "pod with WindowsOptions, no error",
		sc:          &core.PodSpec{Containers: []core.Container{{SecurityContext: &core.SecurityContext{WindowsOptions: &core.WindowsSecurityContextOptions{RunAsUserName: utilpointer.String("dummy")}}}}},
		expectError: false,
	},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errs := validateWindows(test.sc, field.NewPath("field"))
			if test.expectError && len(errs) > 0 {
				if errs[0].Type != test.errorType {
					t.Errorf("expected error type %q got %q", test.errorType, errs[0].Type)
				}
				if errs[0].Detail != test.errorMsg {
					t.Errorf("expected error detail %q, got %q", test.errorMsg, errs[0].Detail)
				}
			} else if test.expectError && len(errs) == 0 {
				t.Error("Unexpected success")
			}
			if !test.expectError && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

func TestValidateOSFields(t *testing.T) {
	// Contains the list of OS specific fields within pod spec.
	// All the fields in pod spec should be either osSpecific or osNeutral field
	// To make a field OS specific:
	// - Add documentation to the os specific field indicating which os it can/cannot be set for
	// - Add documentation to the os field in the api
	// - Add validation logic validateLinux, validateWindows functions to make sure the field is only set for eligible OSes
	osSpecificFields := sets.NewString(
		"Containers[*].SecurityContext.AppArmorProfile",
		"Containers[*].SecurityContext.AllowPrivilegeEscalation",
		"Containers[*].SecurityContext.Capabilities",
		"Containers[*].SecurityContext.Privileged",
		"Containers[*].SecurityContext.ProcMount",
		"Containers[*].SecurityContext.ReadOnlyRootFilesystem",
		"Containers[*].SecurityContext.RunAsGroup",
		"Containers[*].SecurityContext.RunAsUser",
		"Containers[*].SecurityContext.SELinuxOptions",
		"Containers[*].SecurityContext.SeccompProfile",
		"Containers[*].SecurityContext.WindowsOptions",
		"InitContainers[*].SecurityContext.AppArmorProfile",
		"InitContainers[*].SecurityContext.AllowPrivilegeEscalation",
		"InitContainers[*].SecurityContext.Capabilities",
		"InitContainers[*].SecurityContext.Privileged",
		"InitContainers[*].SecurityContext.ProcMount",
		"InitContainers[*].SecurityContext.ReadOnlyRootFilesystem",
		"InitContainers[*].SecurityContext.RunAsGroup",
		"InitContainers[*].SecurityContext.RunAsUser",
		"InitContainers[*].SecurityContext.SELinuxOptions",
		"InitContainers[*].SecurityContext.SeccompProfile",
		"InitContainers[*].SecurityContext.WindowsOptions",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.AppArmorProfile",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.AllowPrivilegeEscalation",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.Capabilities",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.Privileged",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.ProcMount",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.ReadOnlyRootFilesystem",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.RunAsGroup",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.RunAsUser",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.SELinuxOptions",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.SeccompProfile",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.WindowsOptions",
		"OS",
		"SecurityContext.AppArmorProfile",
		"SecurityContext.FSGroup",
		"SecurityContext.FSGroupChangePolicy",
		"SecurityContext.HostIPC",
		"SecurityContext.HostNetwork",
		"SecurityContext.HostPID",
		"SecurityContext.HostUsers",
		"SecurityContext.RunAsGroup",
		"SecurityContext.RunAsUser",
		"SecurityContext.SELinuxOptions",
		"SecurityContext.SELinuxChangePolicy",
		"SecurityContext.SeccompProfile",
		"SecurityContext.ShareProcessNamespace",
		"SecurityContext.SupplementalGroups",
		"SecurityContext.SupplementalGroupsPolicy",
		"SecurityContext.Sysctls",
		"SecurityContext.WindowsOptions",
	)
	osNeutralFields := sets.NewString(
		"ActiveDeadlineSeconds",
		"Affinity",
		"AutomountServiceAccountToken",
		"Containers[*].Args",
		"Containers[*].Command",
		"Containers[*].Env",
		"Containers[*].EnvFrom",
		"Containers[*].Image",
		"Containers[*].ImagePullPolicy",
		"Containers[*].Lifecycle",
		"Containers[*].LivenessProbe",
		"Containers[*].Name",
		"Containers[*].Ports",
		"Containers[*].ReadinessProbe",
		"Containers[*].Resources",
		"Containers[*].ResizePolicy[*].RestartPolicy",
		"Containers[*].ResizePolicy[*].ResourceName",
		"Containers[*].RestartPolicy",
		"Containers[*].SecurityContext.RunAsNonRoot",
		"Containers[*].Stdin",
		"Containers[*].StdinOnce",
		"Containers[*].StartupProbe",
		"Containers[*].VolumeDevices[*]",
		"Containers[*].VolumeMounts[*]",
		"Containers[*].TTY",
		"Containers[*].TerminationMessagePath",
		"Containers[*].TerminationMessagePolicy",
		"Containers[*].WorkingDir",
		"DNSPolicy",
		"EnableServiceLinks",
		"EphemeralContainers[*].EphemeralContainerCommon.Args",
		"EphemeralContainers[*].EphemeralContainerCommon.Command",
		"EphemeralContainers[*].EphemeralContainerCommon.Env",
		"EphemeralContainers[*].EphemeralContainerCommon.EnvFrom",
		"EphemeralContainers[*].EphemeralContainerCommon.Image",
		"EphemeralContainers[*].EphemeralContainerCommon.ImagePullPolicy",
		"EphemeralContainers[*].EphemeralContainerCommon.Lifecycle",
		"EphemeralContainers[*].EphemeralContainerCommon.LivenessProbe",
		"EphemeralContainers[*].EphemeralContainerCommon.Name",
		"EphemeralContainers[*].EphemeralContainerCommon.Ports",
		"EphemeralContainers[*].EphemeralContainerCommon.ReadinessProbe",
		"EphemeralContainers[*].EphemeralContainerCommon.Resources",
		"EphemeralContainers[*].EphemeralContainerCommon.ResizePolicy[*].RestartPolicy",
		"EphemeralContainers[*].EphemeralContainerCommon.ResizePolicy[*].ResourceName",
		"EphemeralContainers[*].EphemeralContainerCommon.RestartPolicy",
		"EphemeralContainers[*].EphemeralContainerCommon.Stdin",
		"EphemeralContainers[*].EphemeralContainerCommon.StdinOnce",
		"EphemeralContainers[*].EphemeralContainerCommon.TTY",
		"EphemeralContainers[*].EphemeralContainerCommon.TerminationMessagePath",
		"EphemeralContainers[*].EphemeralContainerCommon.TerminationMessagePolicy",
		"EphemeralContainers[*].EphemeralContainerCommon.WorkingDir",
		"EphemeralContainers[*].TargetContainerName",
		"EphemeralContainers[*].EphemeralContainerCommon.SecurityContext.RunAsNonRoot",
		"EphemeralContainers[*].EphemeralContainerCommon.StartupProbe",
		"EphemeralContainers[*].EphemeralContainerCommon.VolumeDevices[*]",
		"EphemeralContainers[*].EphemeralContainerCommon.VolumeMounts[*]",
		"HostAliases",
		"Hostname",
		"ImagePullSecrets",
		"InitContainers[*].Args",
		"InitContainers[*].Command",
		"InitContainers[*].Env",
		"InitContainers[*].EnvFrom",
		"InitContainers[*].Image",
		"InitContainers[*].ImagePullPolicy",
		"InitContainers[*].Lifecycle",
		"InitContainers[*].LivenessProbe",
		"InitContainers[*].Name",
		"InitContainers[*].Ports",
		"InitContainers[*].ReadinessProbe",
		"InitContainers[*].Resources",
		"InitContainers[*].ResizePolicy[*].RestartPolicy",
		"InitContainers[*].ResizePolicy[*].ResourceName",
		"InitContainers[*].RestartPolicy",
		"InitContainers[*].Stdin",
		"InitContainers[*].StdinOnce",
		"InitContainers[*].TTY",
		"InitContainers[*].TerminationMessagePath",
		"InitContainers[*].TerminationMessagePolicy",
		"InitContainers[*].WorkingDir",
		"InitContainers[*].SecurityContext.RunAsNonRoot",
		"InitContainers[*].StartupProbe",
		"InitContainers[*].VolumeDevices[*]",
		"InitContainers[*].VolumeMounts[*]",
		"NodeName",
		"NodeSelector",
		"PreemptionPolicy",
		"Priority",
		"PriorityClassName",
		"ReadinessGates",
		"ResourceClaims[*].Name",
		"ResourceClaims[*].ResourceClaimName",
		"ResourceClaims[*].ResourceClaimTemplateName",
		"Resources",
		"RestartPolicy",
		"RuntimeClassName",
		"SchedulerName",
		"SchedulingGates[*].Name",
		"SecurityContext.RunAsNonRoot",
		"ServiceAccountName",
		"SetHostnameAsFQDN",
		"Subdomain",
		"TerminationGracePeriodSeconds",
		"Volumes",
		"DNSConfig",
		"Overhead",
		"Tolerations",
		"TopologySpreadConstraints",
	)

	expect := sets.NewString().Union(osSpecificFields).Union(osNeutralFields)

	result := collectResourcePaths(t, expect, reflect.TypeOf(&core.PodSpec{}), nil)

	if !expect.Equal(result) {
		// expected fields missing from result
		missing := expect.Difference(result)
		// unexpected fields in result but not specified in expect
		unexpected := result.Difference(expect)
		if len(missing) > 0 {
			t.Errorf("the following fields were expected, but missing from the result. "+
				"If the field has been removed, please remove it from the osNeutralFields set "+
				"or remove it from the osSpecificFields set, as appropriate:\n%s",
				strings.Join(missing.List(), "\n"))
		}
		if len(unexpected) > 0 {
			t.Errorf("the following fields were in the result, but unexpected. "+
				"If the field is new, please add it to the osNeutralFields set "+
				"or add it to the osSpecificFields set, as appropriate:\n%s",
				strings.Join(unexpected.List(), "\n"))
		}
	}
}

func TestValidateSchedulingGates(t *testing.T) {
	fieldPath := field.NewPath("field")

	tests := []struct {
		name            string
		schedulingGates []core.PodSchedulingGate
		wantFieldErrors field.ErrorList
	}{{
		name:            "nil gates",
		schedulingGates: nil,
		wantFieldErrors: field.ErrorList{},
	}, {
		name: "empty string in gates",
		schedulingGates: []core.PodSchedulingGate{
			{Name: "foo"},
			{Name: ""},
		},
		wantFieldErrors: field.ErrorList{
			field.Invalid(fieldPath.Index(1), "", "name part must be non-empty"),
			field.Invalid(fieldPath.Index(1), "", "name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]')"),
		},
	}, {
		name: "legal gates",
		schedulingGates: []core.PodSchedulingGate{
			{Name: "foo"},
			{Name: "bar"},
		},
		wantFieldErrors: field.ErrorList{},
	}, {
		name: "illegal gates",
		schedulingGates: []core.PodSchedulingGate{
			{Name: "foo"},
			{Name: "\nbar"},
		},
		wantFieldErrors: []*field.Error{field.Invalid(fieldPath.Index(1), "\nbar", "name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]')")},
	}, {
		name: "duplicated gates (single duplication)",
		schedulingGates: []core.PodSchedulingGate{
			{Name: "foo"},
			{Name: "bar"},
			{Name: "bar"},
		},
		wantFieldErrors: []*field.Error{field.Duplicate(fieldPath.Index(2), "bar")},
	}, {
		name: "duplicated gates (multiple duplications)",
		schedulingGates: []core.PodSchedulingGate{
			{Name: "foo"},
			{Name: "bar"},
			{Name: "foo"},
			{Name: "baz"},
			{Name: "foo"},
			{Name: "bar"},
		},
		wantFieldErrors: field.ErrorList{
			field.Duplicate(fieldPath.Index(2), "foo"),
			field.Duplicate(fieldPath.Index(4), "foo"),
			field.Duplicate(fieldPath.Index(5), "bar"),
		},
	},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateSchedulingGates(tt.schedulingGates, fieldPath)
			if diff := cmp.Diff(tt.wantFieldErrors, errs); diff != "" {
				t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
			}
		})
	}
}

// collectResourcePaths traverses the object, computing all the struct paths.
func collectResourcePaths(t *testing.T, skipRecurseList sets.String, tp reflect.Type, path *field.Path) sets.String {
	if pathStr := path.String(); len(pathStr) > 0 && skipRecurseList.Has(pathStr) {
		return sets.NewString(pathStr)
	}

	paths := sets.NewString()
	switch tp.Kind() {
	case reflect.Pointer:
		paths.Insert(collectResourcePaths(t, skipRecurseList, tp.Elem(), path).List()...)
	case reflect.Struct:
		for i := 0; i < tp.NumField(); i++ {
			field := tp.Field(i)
			paths.Insert(collectResourcePaths(t, skipRecurseList, field.Type, path.Child(field.Name)).List()...)
		}
	case reflect.Map, reflect.Slice:
		paths.Insert(collectResourcePaths(t, skipRecurseList, tp.Elem(), path.Key("*")).List()...)
	case reflect.Interface:
		t.Fatalf("unexpected interface{} field %s", path.String())
	default:
		// if we hit a primitive type, we're at a leaf
		paths.Insert(path.String())
	}
	return paths
}

func TestValidateTLSSecret(t *testing.T) {
	successCases := map[string]core.Secret{
		"empty certificate chain": {
			ObjectMeta: metav1.ObjectMeta{Name: "tls-cert", Namespace: "namespace"},
			Data: map[string][]byte{
				core.TLSCertKey:       []byte("public key"),
				core.TLSPrivateKeyKey: []byte("private key"),
			},
		},
	}
	for k, v := range successCases {
		if errs := ValidateSecret(&v); len(errs) != 0 {
			t.Errorf("Expected success for %s, got %v", k, errs)
		}
	}
	errorCases := map[string]struct {
		secrets     core.Secret
		errorType   field.ErrorType
		errorDetail string
	}{
		"missing public key": {
			secrets: core.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "tls-cert"},
				Data: map[string][]byte{
					core.TLSCertKey: []byte("public key"),
				},
			},
			errorType: "FieldValueRequired",
		},
		"missing private key": {
			secrets: core.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "tls-cert"},
				Data: map[string][]byte{
					core.TLSCertKey: []byte("public key"),
				},
			},
			errorType: "FieldValueRequired",
		},
	}
	for k, v := range errorCases {
		if errs := ValidateSecret(&v.secrets); len(errs) == 0 || errs[0].Type != v.errorType || !strings.Contains(errs[0].Detail, v.errorDetail) {
			t.Errorf("[%s] Expected error type %s with detail %q, got %v", k, v.errorType, v.errorDetail, errs)
		}
	}
}

func TestValidateLinuxSecurityContext(t *testing.T) {
	runAsUser := int64(1)
	validLinuxSC := &core.SecurityContext{
		Privileged: utilpointer.Bool(false),
		Capabilities: &core.Capabilities{
			Add:  []core.Capability{"foo"},
			Drop: []core.Capability{"bar"},
		},
		SELinuxOptions: &core.SELinuxOptions{
			User:  "user",
			Role:  "role",
			Type:  "type",
			Level: "level",
		},
		RunAsUser: &runAsUser,
	}
	invalidLinuxSC := &core.SecurityContext{
		WindowsOptions: &core.WindowsSecurityContextOptions{RunAsUserName: utilpointer.String("myUser")},
	}
	cases := map[string]struct {
		sc          *core.PodSpec
		expectErr   bool
		errorType   field.ErrorType
		errorDetail string
	}{
		"valid SC, linux, no error": {
			sc:        &core.PodSpec{Containers: []core.Container{{SecurityContext: validLinuxSC}}},
			expectErr: false,
		},
		"invalid SC, linux, error": {
			sc:          &core.PodSpec{Containers: []core.Container{{SecurityContext: invalidLinuxSC}}},
			errorType:   "FieldValueForbidden",
			errorDetail: "windows options cannot be set for a linux pod",
			expectErr:   true,
		},
	}
	for k, v := range cases {
		t.Run(k, func(t *testing.T) {
			errs := validateLinux(v.sc, field.NewPath("field"))
			if v.expectErr && len(errs) > 0 {
				if errs[0].Type != v.errorType || !strings.Contains(errs[0].Detail, v.errorDetail) {
					t.Errorf("[%s] Expected error type %q with detail %q, got %v", k, v.errorType, v.errorDetail, errs)
				}
			} else if v.expectErr && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if !v.expectErr && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

func TestValidateSecurityContext(t *testing.T) {
	runAsUser := int64(1)
	fullValidSC := func() *core.SecurityContext {
		return &core.SecurityContext{
			Privileged: utilpointer.Bool(false),
			Capabilities: &core.Capabilities{
				Add:  []core.Capability{"foo"},
				Drop: []core.Capability{"bar"},
			},
			SELinuxOptions: &core.SELinuxOptions{
				User:  "user",
				Role:  "role",
				Type:  "type",
				Level: "level",
			},
			RunAsUser: &runAsUser,
		}
	}

	// setup data
	allSettings := fullValidSC()
	noCaps := fullValidSC()
	noCaps.Capabilities = nil

	noSELinux := fullValidSC()
	noSELinux.SELinuxOptions = nil

	noPrivRequest := fullValidSC()
	noPrivRequest.Privileged = nil

	noRunAsUser := fullValidSC()
	noRunAsUser.RunAsUser = nil

	procMountSet := fullValidSC()
	defPmt := core.DefaultProcMount
	procMountSet.ProcMount = &defPmt

	umPmt := core.UnmaskedProcMount
	procMountUnmasked := fullValidSC()
	procMountUnmasked.ProcMount = &umPmt

	successCases := map[string]struct {
		sc        *core.SecurityContext
		hostUsers bool
	}{
		"all settings":        {allSettings, false},
		"no capabilities":     {noCaps, false},
		"no selinux":          {noSELinux, false},
		"no priv request":     {noPrivRequest, false},
		"no run as user":      {noRunAsUser, false},
		"proc mount set":      {procMountSet, true},
		"proc mount unmasked": {procMountUnmasked, false},
	}
	for k, v := range successCases {
		if errs := ValidateSecurityContext(v.sc, field.NewPath("field"), v.hostUsers); len(errs) != 0 {
			t.Errorf("[%s] Expected success, got %v", k, errs)
		}
	}

	privRequestWithGlobalDeny := fullValidSC()
	privRequestWithGlobalDeny.Privileged = utilpointer.Bool(true)

	negativeRunAsUser := fullValidSC()
	negativeUser := int64(-1)
	negativeRunAsUser.RunAsUser = &negativeUser

	privWithoutEscalation := fullValidSC()
	privWithoutEscalation.Privileged = utilpointer.Bool(true)
	privWithoutEscalation.AllowPrivilegeEscalation = utilpointer.Bool(false)

	capSysAdminWithoutEscalation := fullValidSC()
	capSysAdminWithoutEscalation.Capabilities.Add = []core.Capability{"CAP_SYS_ADMIN"}
	capSysAdminWithoutEscalation.AllowPrivilegeEscalation = utilpointer.Bool(false)

	errorCases := map[string]struct {
		sc           *core.SecurityContext
		errorType    field.ErrorType
		errorDetail  string
		capAllowPriv bool
	}{
		"request privileged when capabilities forbids": {
			sc:          privRequestWithGlobalDeny,
			errorType:   "FieldValueForbidden",
			errorDetail: "disallowed by cluster policy",
		},
		"negative RunAsUser": {
			sc:          negativeRunAsUser,
			errorType:   "FieldValueInvalid",
			errorDetail: "must be between",
		},
		"with CAP_SYS_ADMIN and allowPrivilegeEscalation false": {
			sc:          capSysAdminWithoutEscalation,
			errorType:   "FieldValueInvalid",
			errorDetail: "cannot set `allowPrivilegeEscalation` to false and `capabilities.Add` CAP_SYS_ADMIN",
		},
		"with privileged and allowPrivilegeEscalation false": {
			sc:           privWithoutEscalation,
			errorType:    "FieldValueInvalid",
			errorDetail:  "cannot set `allowPrivilegeEscalation` to false and `privileged` to true",
			capAllowPriv: true,
		},
		"with unmasked proc mount type and no user namespace": {
			sc:          procMountUnmasked,
			errorType:   "FieldValueInvalid",
			errorDetail: "`hostUsers` must be false to use `Unmasked`",
		},
	}
	for k, v := range errorCases {
		capabilities.ResetForTest()
		capabilities.Initialize(capabilities.Capabilities{
			AllowPrivileged: v.capAllowPriv,
		})
		// note the unconditional `true` here for hostUsers. The failure case to test for ProcMount only includes it being true,
		// and the field is ignored if ProcMount isn't set. Thus, we can unconditionally set to `true` and simplify the test matrix setup.
		if errs := ValidateSecurityContext(v.sc, field.NewPath("field"), true); len(errs) == 0 || errs[0].Type != v.errorType || !strings.Contains(errs[0].Detail, v.errorDetail) {
			t.Errorf("[%s] Expected error type %q with detail %q, got %v", k, v.errorType, v.errorDetail, errs)
		}
	}
}

func fakeValidSecurityContext(priv bool) *core.SecurityContext {
	return &core.SecurityContext{
		Privileged: &priv,
	}
}

func TestValidPodLogOptions(t *testing.T) {
	now := metav1.Now()
	negative := int64(-1)
	zero := int64(0)
	positive := int64(1)
	stdoutStream := core.LogStreamStdout
	stderrStream := core.LogStreamStderr
	allStream := core.LogStreamAll
	invalidStream := "invalid"
	tests := []struct {
		opt                  core.PodLogOptions
		errs                 int
		allowStreamSelection bool
	}{
		{core.PodLogOptions{}, 0, false},
		{core.PodLogOptions{Previous: true}, 0, false},
		{core.PodLogOptions{Follow: true}, 0, false},
		{core.PodLogOptions{TailLines: &zero}, 0, false},
		{core.PodLogOptions{TailLines: &negative}, 1, false},
		{core.PodLogOptions{TailLines: &positive}, 0, false},
		{core.PodLogOptions{LimitBytes: &zero}, 1, false},
		{core.PodLogOptions{LimitBytes: &negative}, 1, false},
		{core.PodLogOptions{LimitBytes: &positive}, 0, false},
		{core.PodLogOptions{SinceSeconds: &negative}, 1, false},
		{core.PodLogOptions{SinceSeconds: &positive}, 0, false},
		{core.PodLogOptions{SinceSeconds: &zero}, 1, false},
		{core.PodLogOptions{SinceTime: &now}, 0, false},
		{
			opt: core.PodLogOptions{
				Stream: &stdoutStream,
			},
			allowStreamSelection: false,
			errs:                 1,
		},
		{
			opt: core.PodLogOptions{
				Stream: &stdoutStream,
			},
			allowStreamSelection: true,
		},
		{
			opt: core.PodLogOptions{
				Stream: &invalidStream,
			},
			allowStreamSelection: true,
			errs:                 1,
		},
		{
			opt: core.PodLogOptions{
				Stream:    &stderrStream,
				TailLines: &positive,
			},
			allowStreamSelection: true,
			errs:                 1,
		},
		{
			opt: core.PodLogOptions{
				Stream:    &allStream,
				TailLines: &positive,
			},
			allowStreamSelection: true,
		},
		{
			opt: core.PodLogOptions{
				Stream:     &stdoutStream,
				LimitBytes: &positive,
				SinceTime:  &now,
			},
			allowStreamSelection: true,
		},
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			errs := ValidatePodLogOptions(&test.opt, test.allowStreamSelection)
			if test.errs != len(errs) {
				t.Errorf("%d: Unexpected errors: %v", i, errs)
			}
		})
	}
}

func TestValidateConfigMap(t *testing.T) {
	newConfigMap := func(name, namespace string, data map[string]string, binaryData map[string][]byte) core.ConfigMap {
		return core.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Data:       data,
			BinaryData: binaryData,
		}
	}

	var (
		validConfigMap = newConfigMap("validname", "validns", map[string]string{"key": "value"}, map[string][]byte{"bin": []byte("value")})
		maxKeyLength   = newConfigMap("validname", "validns", map[string]string{strings.Repeat("a", 253): "value"}, nil)

		emptyName               = newConfigMap("", "validns", nil, nil)
		invalidName             = newConfigMap("NoUppercaseOrSpecialCharsLike=Equals", "validns", nil, nil)
		emptyNs                 = newConfigMap("validname", "", nil, nil)
		invalidNs               = newConfigMap("validname", "NoUppercaseOrSpecialCharsLike=Equals", nil, nil)
		invalidKey              = newConfigMap("validname", "validns", map[string]string{"a*b": "value"}, nil)
		leadingDotKey           = newConfigMap("validname", "validns", map[string]string{".ab": "value"}, nil)
		dotKey                  = newConfigMap("validname", "validns", map[string]string{".": "value"}, nil)
		doubleDotKey            = newConfigMap("validname", "validns", map[string]string{"..": "value"}, nil)
		overMaxKeyLength        = newConfigMap("validname", "validns", map[string]string{strings.Repeat("a", 254): "value"}, nil)
		overMaxSize             = newConfigMap("validname", "validns", map[string]string{"key": strings.Repeat("a", v1.MaxSecretSize+1)}, nil)
		duplicatedKey           = newConfigMap("validname", "validns", map[string]string{"key": "value1"}, map[string][]byte{"key": []byte("value2")})
		binDataInvalidKey       = newConfigMap("validname", "validns", nil, map[string][]byte{"a*b": []byte("value")})
		binDataLeadingDotKey    = newConfigMap("validname", "validns", nil, map[string][]byte{".ab": []byte("value")})
		binDataDotKey           = newConfigMap("validname", "validns", nil, map[string][]byte{".": []byte("value")})
		binDataDoubleDotKey     = newConfigMap("validname", "validns", nil, map[string][]byte{"..": []byte("value")})
		binDataOverMaxKeyLength = newConfigMap("validname", "validns", nil, map[string][]byte{strings.Repeat("a", 254): []byte("value")})
		binDataOverMaxSize      = newConfigMap("validname", "validns", nil, map[string][]byte{"bin": bytes.Repeat([]byte("a"), v1.MaxSecretSize+1)})
		binNonUtf8Value         = newConfigMap("validname", "validns", nil, map[string][]byte{"key": {0, 0xFE, 0, 0xFF}})
	)

	tests := map[string]struct {
		cfg     core.ConfigMap
		isValid bool
	}{
		"valid":                           {validConfigMap, true},
		"max key length":                  {maxKeyLength, true},
		"leading dot key":                 {leadingDotKey, true},
		"empty name":                      {emptyName, false},
		"invalid name":                    {invalidName, false},
		"invalid key":                     {invalidKey, false},
		"empty namespace":                 {emptyNs, false},
		"invalid namespace":               {invalidNs, false},
		"dot key":                         {dotKey, false},
		"double dot key":                  {doubleDotKey, false},
		"over max key length":             {overMaxKeyLength, false},
		"over max size":                   {overMaxSize, false},
		"duplicated key":                  {duplicatedKey, false},
		"binary data invalid key":         {binDataInvalidKey, false},
		"binary data leading dot key":     {binDataLeadingDotKey, true},
		"binary data dot key":             {binDataDotKey, false},
		"binary data double dot key":      {binDataDoubleDotKey, false},
		"binary data over max key length": {binDataOverMaxKeyLength, false},
		"binary data max size":            {binDataOverMaxSize, false},
		"binary data non utf-8 bytes":     {binNonUtf8Value, true},
	}

	for name, tc := range tests {
		errs := ValidateConfigMap(&tc.cfg)
		if tc.isValid && len(errs) > 0 {
			t.Errorf("%v: unexpected error: %v", name, errs)
		}
		if !tc.isValid && len(errs) == 0 {
			t.Errorf("%v: unexpected non-error", name)
		}
	}
}

func TestValidateConfigMapUpdate(t *testing.T) {
	newConfigMap := func(version, name, namespace string, data map[string]string) core.ConfigMap {
		return core.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       namespace,
				ResourceVersion: version,
			},
			Data: data,
		}
	}
	validConfigMap := func() core.ConfigMap {
		return newConfigMap("1", "validname", "validdns", map[string]string{"key": "value"})
	}

	falseVal := false
	trueVal := true

	configMap := validConfigMap()
	immutableConfigMap := validConfigMap()
	immutableConfigMap.Immutable = &trueVal
	mutableConfigMap := validConfigMap()
	mutableConfigMap.Immutable = &falseVal

	configMapWithData := validConfigMap()
	configMapWithData.Data["key-2"] = "value-2"
	immutableConfigMapWithData := validConfigMap()
	immutableConfigMapWithData.Immutable = &trueVal
	immutableConfigMapWithData.Data["key-2"] = "value-2"

	configMapWithChangedData := validConfigMap()
	configMapWithChangedData.Data["key"] = "foo"
	immutableConfigMapWithChangedData := validConfigMap()
	immutableConfigMapWithChangedData.Immutable = &trueVal
	immutableConfigMapWithChangedData.Data["key"] = "foo"

	noVersion := newConfigMap("", "validname", "validns", map[string]string{"key": "value"})

	cases := []struct {
		name   string
		newCfg core.ConfigMap
		oldCfg core.ConfigMap
		valid  bool
	}{{
		name:   "valid",
		newCfg: configMap,
		oldCfg: configMap,
		valid:  true,
	}, {
		name:   "invalid",
		newCfg: noVersion,
		oldCfg: configMap,
		valid:  false,
	}, {
		name:   "mark configmap immutable",
		oldCfg: configMap,
		newCfg: immutableConfigMap,
		valid:  true,
	}, {
		name:   "revert immutable configmap",
		oldCfg: immutableConfigMap,
		newCfg: configMap,
		valid:  false,
	}, {
		name:   "mark immutable configmap mutable",
		oldCfg: immutableConfigMap,
		newCfg: mutableConfigMap,
		valid:  false,
	}, {
		name:   "add data in configmap",
		oldCfg: configMap,
		newCfg: configMapWithData,
		valid:  true,
	}, {
		name:   "add data in immutable configmap",
		oldCfg: immutableConfigMap,
		newCfg: immutableConfigMapWithData,
		valid:  false,
	}, {
		name:   "change data in configmap",
		oldCfg: configMap,
		newCfg: configMapWithChangedData,
		valid:  true,
	}, {
		name:   "change data in immutable configmap",
		oldCfg: immutableConfigMap,
		newCfg: immutableConfigMapWithChangedData,
		valid:  false,
	},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateConfigMapUpdate(&tc.newCfg, &tc.oldCfg)
			if tc.valid && len(errs) > 0 {
				t.Errorf("Unexpected error: %v", errs)
			}
			if !tc.valid && len(errs) == 0 {
				t.Errorf("Unexpected lack of error")
			}
		})
	}
}

func TestValidateHasLabel(t *testing.T) {
	successCase := metav1.ObjectMeta{
		Name:      "123",
		Namespace: "ns",
		Labels: map[string]string{
			"other": "blah",
			"foo":   "bar",
		},
	}
	if errs := ValidateHasLabel(successCase, field.NewPath("field"), "foo", "bar"); len(errs) != 0 {
		t.Errorf("expected success: %v", errs)
	}

	missingCase := metav1.ObjectMeta{
		Name:      "123",
		Namespace: "ns",
		Labels: map[string]string{
			"other": "blah",
		},
	}
	if errs := ValidateHasLabel(missingCase, field.NewPath("field"), "foo", "bar"); len(errs) == 0 {
		t.Errorf("expected failure")
	}

	wrongValueCase := metav1.ObjectMeta{
		Name:      "123",
		Namespace: "ns",
		Labels: map[string]string{
			"other": "blah",
			"foo":   "notbar",
		},
	}
	if errs := ValidateHasLabel(wrongValueCase, field.NewPath("field"), "foo", "bar"); len(errs) == 0 {
		t.Errorf("expected failure")
	}
}

func TestIsValidSysctlName(t *testing.T) {
	valid := []string{
		"a.b.c.d",
		"a",
		"a_b",
		"a-b",
		"abc",
		"abc.def",
		"a/b/c/d",
		"a/b.c",
	}
	invalid := []string{
		"",
		"*",
		"ä",
		"a_",
		"_",
		"__",
		"_a",
		"_a._b",
		"-",
		".",
		"a.",
		".a",
		"a.b.",
		"a*.b",
		"a*b",
		"*a",
		"a.*",
		"*",
		"abc*",
		"a.abc*",
		"a.b.*",
		"Abc",
		"/",
		"/a",
		"a/abc*",
		"a/b/*",
		func(n int) string {
			x := make([]byte, n)
			for i := range x {
				x[i] = byte('a')
			}
			return string(x)
		}(256),
	}

	for _, s := range valid {
		if !IsValidSysctlName(s) {
			t.Errorf("%q expected to be a valid sysctl name", s)
		}
	}
	for _, s := range invalid {
		if IsValidSysctlName(s) {
			t.Errorf("%q expected to be an invalid sysctl name", s)
		}
	}
}

func TestValidateSysctls(t *testing.T) {
	valid := []string{
		"net.foo.bar",
		"kernel.shmmax",
		"net.ipv4.conf.enp3s0/200.forwarding",
		"net/ipv4/conf/enp3s0.200/forwarding",
	}
	invalid := []string{
		"i..nvalid",
		"_invalid",
	}

	invalidWithHostNet := []string{
		"net.ipv4.conf.enp3s0/200.forwarding",
		"net/ipv4/conf/enp3s0.200/forwarding",
	}

	invalidWithHostIPC := []string{
		"kernel.shmmax",
		"kernel.msgmax",
	}

	duplicates := []string{
		"kernel.shmmax",
		"kernel.shmmax",
	}
	opts := PodValidationOptions{
		AllowNamespacedSysctlsForHostNetAndHostIPC: false,
	}

	sysctls := make([]core.Sysctl, len(valid))
	validSecurityContext := &core.PodSecurityContext{
		Sysctls: sysctls,
	}
	for i, sysctl := range valid {
		sysctls[i].Name = sysctl
	}
	errs := validateSysctls(validSecurityContext, field.NewPath("foo"), opts)
	if len(errs) != 0 {
		t.Errorf("unexpected validation errors: %v", errs)
	}

	sysctls = make([]core.Sysctl, len(invalid))
	for i, sysctl := range invalid {
		sysctls[i].Name = sysctl
	}
	inValidSecurityContext := &core.PodSecurityContext{
		Sysctls: sysctls,
	}
	errs = validateSysctls(inValidSecurityContext, field.NewPath("foo"), opts)
	if len(errs) != 2 {
		t.Errorf("expected 2 validation errors. Got: %v", errs)
	} else {
		if got, expected := errs[0].Error(), "foo"; !strings.Contains(got, expected) {
			t.Errorf("unexpected errors: expected=%q, got=%q", expected, got)
		}
		if got, expected := errs[1].Error(), "foo"; !strings.Contains(got, expected) {
			t.Errorf("unexpected errors: expected=%q, got=%q", expected, got)
		}
	}

	sysctls = make([]core.Sysctl, len(duplicates))
	for i, sysctl := range duplicates {
		sysctls[i].Name = sysctl
	}
	securityContextWithDup := &core.PodSecurityContext{
		Sysctls: sysctls,
	}
	errs = validateSysctls(securityContextWithDup, field.NewPath("foo"), opts)
	if len(errs) != 1 {
		t.Errorf("unexpected validation errors: %v", errs)
	} else if errs[0].Type != field.ErrorTypeDuplicate {
		t.Errorf("expected error type %v, got %v", field.ErrorTypeDuplicate, errs[0].Type)
	}

	sysctls = make([]core.Sysctl, len(invalidWithHostNet))
	for i, sysctl := range invalidWithHostNet {
		sysctls[i].Name = sysctl
	}
	invalidSecurityContextWithHostNet := &core.PodSecurityContext{
		Sysctls:     sysctls,
		HostIPC:     false,
		HostNetwork: true,
	}
	errs = validateSysctls(invalidSecurityContextWithHostNet, field.NewPath("foo"), opts)
	if len(errs) != 2 {
		t.Errorf("unexpected validation errors: %v", errs)
	}
	opts.AllowNamespacedSysctlsForHostNetAndHostIPC = true
	errs = validateSysctls(invalidSecurityContextWithHostNet, field.NewPath("foo"), opts)
	if len(errs) != 0 {
		t.Errorf("unexpected validation errors: %v", errs)
	}

	sysctls = make([]core.Sysctl, len(invalidWithHostIPC))
	for i, sysctl := range invalidWithHostIPC {
		sysctls[i].Name = sysctl
	}
	invalidSecurityContextWithHostIPC := &core.PodSecurityContext{
		Sysctls:     sysctls,
		HostIPC:     true,
		HostNetwork: false,
	}
	opts.AllowNamespacedSysctlsForHostNetAndHostIPC = false
	errs = validateSysctls(invalidSecurityContextWithHostIPC, field.NewPath("foo"), opts)
	if len(errs) != 2 {
		t.Errorf("unexpected validation errors: %v", errs)
	}
	opts.AllowNamespacedSysctlsForHostNetAndHostIPC = true
	errs = validateSysctls(invalidSecurityContextWithHostIPC, field.NewPath("foo"), opts)
	if len(errs) != 0 {
		t.Errorf("unexpected validation errors: %v", errs)
	}
}

func newNodeNameEndpoint(nodeName string) *core.Endpoints {
	ep := &core.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "foo",
			Namespace:       metav1.NamespaceDefault,
			ResourceVersion: "1",
		},
		Subsets: []core.EndpointSubset{{
			NotReadyAddresses: []core.EndpointAddress{},
			Ports:             []core.EndpointPort{{Name: "https", Port: 443, Protocol: "TCP"}},
			Addresses: []core.EndpointAddress{{
				IP:       "8.8.8.8",
				Hostname: "zookeeper1",
				NodeName: &nodeName}}}}}
	return ep
}

func TestEndpointAddressNodeNameUpdateRestrictions(t *testing.T) {
	oldEndpoint := newNodeNameEndpoint("kubernetes-node-setup-by-backend")
	updatedEndpoint := newNodeNameEndpoint("kubernetes-changed-nodename")
	// Check that NodeName can be changed during update, this is to accommodate the case where nodeIP or PodCIDR is reused.
	// The same ip will now have a different nodeName.
	errList := ValidateEndpoints(updatedEndpoint)
	errList = append(errList, ValidateEndpointsUpdate(updatedEndpoint, oldEndpoint)...)
	if len(errList) != 0 {
		t.Error("Endpoint should allow changing of Subset.Addresses.NodeName on update")
	}
}

func TestEndpointAddressNodeNameInvalidDNSSubdomain(t *testing.T) {
	// Check NodeName DNS validation
	endpoint := newNodeNameEndpoint("illegal*.nodename")
	errList := ValidateEndpoints(endpoint)
	if len(errList) == 0 {
		t.Error("Endpoint should reject invalid NodeName")
	}
}

func TestEndpointAddressNodeNameCanBeAnIPAddress(t *testing.T) {
	endpoint := newNodeNameEndpoint("10.10.1.1")
	errList := ValidateEndpoints(endpoint)
	if len(errList) != 0 {
		t.Error("Endpoint should accept a NodeName that is an IP address")
	}
}

func TestValidateFlexVolumeSource(t *testing.T) {
	testcases := map[string]struct {
		source       *core.FlexVolumeSource
		expectedErrs map[string]string
	}{
		"valid": {
			source:       &core.FlexVolumeSource{Driver: "foo"},
			expectedErrs: map[string]string{},
		},
		"valid with options": {
			source:       &core.FlexVolumeSource{Driver: "foo", Options: map[string]string{"foo": "bar"}},
			expectedErrs: map[string]string{},
		},
		"no driver": {
			source:       &core.FlexVolumeSource{Driver: ""},
			expectedErrs: map[string]string{"driver": "Required value"},
		},
		"reserved option keys": {
			source: &core.FlexVolumeSource{
				Driver: "foo",
				Options: map[string]string{
					// valid options
					"myns.io":               "A",
					"myns.io/bar":           "A",
					"myns.io/kubernetes.io": "A",

					// invalid options
					"KUBERNETES.IO":     "A",
					"kubernetes.io":     "A",
					"kubernetes.io/":    "A",
					"kubernetes.io/foo": "A",

					"alpha.kubernetes.io":     "A",
					"alpha.kubernetes.io/":    "A",
					"alpha.kubernetes.io/foo": "A",

					"k8s.io":     "A",
					"k8s.io/":    "A",
					"k8s.io/foo": "A",

					"alpha.k8s.io":     "A",
					"alpha.k8s.io/":    "A",
					"alpha.k8s.io/foo": "A",
				},
			},
			expectedErrs: map[string]string{
				"options[KUBERNETES.IO]":           "reserved",
				"options[kubernetes.io]":           "reserved",
				"options[kubernetes.io/]":          "reserved",
				"options[kubernetes.io/foo]":       "reserved",
				"options[alpha.kubernetes.io]":     "reserved",
				"options[alpha.kubernetes.io/]":    "reserved",
				"options[alpha.kubernetes.io/foo]": "reserved",
				"options[k8s.io]":                  "reserved",
				"options[k8s.io/]":                 "reserved",
				"options[k8s.io/foo]":              "reserved",
				"options[alpha.k8s.io]":            "reserved",
				"options[alpha.k8s.io/]":           "reserved",
				"options[alpha.k8s.io/foo]":        "reserved",
			},
		},
	}

	for k, tc := range testcases {
		errs := validateFlexVolumeSource(tc.source, nil)
		for _, err := range errs {
			expectedErr, ok := tc.expectedErrs[err.Field]
			if !ok {
				t.Errorf("%s: unexpected err on field %s: %v", k, err.Field, err)
				continue
			}
			if !strings.Contains(err.Error(), expectedErr) {
				t.Errorf("%s: expected err on field %s to contain '%s', was %v", k, err.Field, expectedErr, err.Error())
				continue
			}
		}
		if len(errs) != len(tc.expectedErrs) {
			t.Errorf("%s: expected errs %#v, got %#v", k, tc.expectedErrs, errs)
			continue
		}
	}
}

func TestValidateOrSetClientIPAffinityConfig(t *testing.T) {
	successCases := map[string]*core.SessionAffinityConfig{
		"non-empty config, valid timeout: 1": {
			ClientIP: &core.ClientIPConfig{
				TimeoutSeconds: utilpointer.Int32(1),
			},
		},
		"non-empty config, valid timeout: core.MaxClientIPServiceAffinitySeconds-1": {
			ClientIP: &core.ClientIPConfig{
				TimeoutSeconds: utilpointer.Int32(core.MaxClientIPServiceAffinitySeconds - 1),
			},
		},
		"non-empty config, valid timeout: core.MaxClientIPServiceAffinitySeconds": {
			ClientIP: &core.ClientIPConfig{
				TimeoutSeconds: utilpointer.Int32(core.MaxClientIPServiceAffinitySeconds),
			},
		},
	}

	for name, test := range successCases {
		if errs := validateClientIPAffinityConfig(test, field.NewPath("field")); len(errs) != 0 {
			t.Errorf("case: %s, expected success: %v", name, errs)
		}
	}

	errorCases := map[string]*core.SessionAffinityConfig{
		"empty session affinity config": nil,
		"empty client IP config": {
			ClientIP: nil,
		},
		"empty timeoutSeconds": {
			ClientIP: &core.ClientIPConfig{
				TimeoutSeconds: nil,
			},
		},
		"non-empty config, invalid timeout: core.MaxClientIPServiceAffinitySeconds+1": {
			ClientIP: &core.ClientIPConfig{
				TimeoutSeconds: utilpointer.Int32(core.MaxClientIPServiceAffinitySeconds + 1),
			},
		},
		"non-empty config, invalid timeout: -1": {
			ClientIP: &core.ClientIPConfig{
				TimeoutSeconds: utilpointer.Int32(-1),
			},
		},
		"non-empty config, invalid timeout: 0": {
			ClientIP: &core.ClientIPConfig{
				TimeoutSeconds: utilpointer.Int32(0),
			},
		},
	}

	for name, test := range errorCases {
		if errs := validateClientIPAffinityConfig(test, field.NewPath("field")); len(errs) == 0 {
			t.Errorf("case: %v, expected failures: %v", name, errs)
		}
	}
}

func TestValidateWindowsSecurityContextOptions(t *testing.T) {
	toPtr := func(s string) *string {
		return &s
	}

	testCases := []struct {
		testName string

		windowsOptions         *core.WindowsSecurityContextOptions
		expectedErrorSubstring string
	}{{
		testName: "a nil pointer",
	}, {
		testName:       "an empty struct",
		windowsOptions: &core.WindowsSecurityContextOptions{},
	}, {
		testName: "a valid input",
		windowsOptions: &core.WindowsSecurityContextOptions{
			GMSACredentialSpecName: toPtr("dummy-gmsa-crep-spec-name"),
			GMSACredentialSpec:     toPtr("dummy-gmsa-crep-spec-contents"),
		},
	}, {
		testName: "a GMSA cred spec name that is not a valid resource name",
		windowsOptions: &core.WindowsSecurityContextOptions{
			// invalid because of the underscore
			GMSACredentialSpecName: toPtr("not_a-valid-gmsa-crep-spec-name"),
		},
		expectedErrorSubstring: dnsSubdomainLabelErrMsg,
	}, {
		testName: "empty GMSA cred spec contents",
		windowsOptions: &core.WindowsSecurityContextOptions{
			GMSACredentialSpec: toPtr(""),
		},
		expectedErrorSubstring: "gmsaCredentialSpec cannot be an empty string",
	}, {
		testName: "GMSA cred spec contents that are too long",
		windowsOptions: &core.WindowsSecurityContextOptions{
			GMSACredentialSpec: toPtr(strings.Repeat("a", maxGMSACredentialSpecLength+1)),
		},
		expectedErrorSubstring: "gmsaCredentialSpec size must be under",
	}, {
		testName: "RunAsUserName is nil",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: nil,
		},
	}, {
		testName: "a valid RunAsUserName",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("Container. User"),
		},
	}, {
		testName: "a valid RunAsUserName with NetBios Domain",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("Network Service\\Container. User"),
		},
	}, {
		testName: "a valid RunAsUserName with DNS Domain",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr(strings.Repeat("fOo", 20) + ".liSH\\Container. User"),
		},
	}, {
		testName: "a valid RunAsUserName with DNS Domain with a single character segment",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr(strings.Repeat("fOo", 20) + ".l\\Container. User"),
		},
	}, {
		testName: "a valid RunAsUserName with a long single segment DNS Domain",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr(strings.Repeat("a", 42) + "\\Container. User"),
		},
	}, {
		testName: "an empty RunAsUserName",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr(""),
		},
		expectedErrorSubstring: "runAsUserName cannot be an empty string",
	}, {
		testName: "RunAsUserName containing a control character",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("Container\tUser"),
		},
		expectedErrorSubstring: "runAsUserName cannot contain control characters",
	}, {
		testName: "RunAsUserName containing too many backslashes",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("Container\\Foo\\Lish"),
		},
		expectedErrorSubstring: "runAsUserName cannot contain more than one backslash",
	}, {
		testName: "RunAsUserName containing backslash but empty Domain",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("\\User"),
		},
		expectedErrorSubstring: "runAsUserName's Domain doesn't match the NetBios nor the DNS format",
	}, {
		testName: "RunAsUserName containing backslash but empty User",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("Container\\"),
		},
		expectedErrorSubstring: "runAsUserName's User cannot be empty",
	}, {
		testName: "RunAsUserName's NetBios Domain is too long",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("NetBios " + strings.Repeat("a", 8) + "\\user"),
		},
		expectedErrorSubstring: "runAsUserName's Domain doesn't match the NetBios",
	}, {
		testName: "RunAsUserName's DNS Domain is too long",
		windowsOptions: &core.WindowsSecurityContextOptions{
			// even if this tests the max Domain length, the Domain should still be "valid".
			RunAsUserName: toPtr(strings.Repeat(strings.Repeat("a", 63)+".", 4)[:253] + ".com\\user"),
		},
		expectedErrorSubstring: "runAsUserName's Domain length must be under",
	}, {
		testName: "RunAsUserName's User is too long",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr(strings.Repeat("a", maxRunAsUserNameUserLength+1)),
		},
		expectedErrorSubstring: "runAsUserName's User length must not be longer than",
	}, {
		testName: "RunAsUserName's User cannot contain only spaces or periods",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("... ..."),
		},
		expectedErrorSubstring: "runAsUserName's User cannot contain only periods or spaces",
	}, {
		testName: "RunAsUserName's NetBios Domain cannot start with a dot",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr(".FooLish\\User"),
		},
		expectedErrorSubstring: "runAsUserName's Domain doesn't match the NetBios",
	}, {
		testName: "RunAsUserName's NetBios Domain cannot contain invalid characters",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("Foo? Lish?\\User"),
		},
		expectedErrorSubstring: "runAsUserName's Domain doesn't match the NetBios",
	}, {
		testName: "RunAsUserName's DNS Domain cannot contain invalid characters",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr(strings.Repeat("a", 32) + ".com-\\user"),
		},
		expectedErrorSubstring: "runAsUserName's Domain doesn't match the NetBios nor the DNS format",
	}, {
		testName: "RunAsUserName's User cannot contain invalid characters",
		windowsOptions: &core.WindowsSecurityContextOptions{
			RunAsUserName: toPtr("Container/User"),
		},
		expectedErrorSubstring: "runAsUserName's User cannot contain the following characters",
	},
	}

	for _, testCase := range testCases {
		t.Run("validateWindowsSecurityContextOptions with"+testCase.testName, func(t *testing.T) {
			errs := validateWindowsSecurityContextOptions(testCase.windowsOptions, field.NewPath("field"))

			switch len(errs) {
			case 0:
				if testCase.expectedErrorSubstring != "" {
					t.Errorf("expected a failure containing the substring: %q", testCase.expectedErrorSubstring)
				}
			case 1:
				if testCase.expectedErrorSubstring == "" {
					t.Errorf("didn't expect a failure, got: %q", errs[0].Error())
				} else if !strings.Contains(errs[0].Error(), testCase.expectedErrorSubstring) {
					t.Errorf("expected a failure with the substring %q, got %q instead", testCase.expectedErrorSubstring, errs[0].Error())
				}
			default:
				t.Errorf("got %d failures", len(errs))
				for i, err := range errs {
					t.Errorf("error %d: %q", i, err.Error())
				}
			}
		})
	}
}

func testDataSourceInSpec(name, kind, apiGroup string) *core.PersistentVolumeClaimSpec {
	scName := "csi-plugin"
	dataSourceInSpec := core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		StorageClassName: &scName,
		DataSource: &core.TypedLocalObjectReference{
			APIGroup: &apiGroup,
			Kind:     kind,
			Name:     name,
		},
	}

	return &dataSourceInSpec
}

func TestAlphaVolumePVCDataSource(t *testing.T) {
	testCases := []struct {
		testName     string
		claimSpec    core.PersistentVolumeClaimSpec
		expectedFail bool
	}{{
		testName:  "test create from valid snapshot source",
		claimSpec: *testDataSourceInSpec("test_snapshot", "VolumeSnapshot", "snapshot.storage.k8s.io"),
	}, {
		testName:  "test create from valid pvc source",
		claimSpec: *testDataSourceInSpec("test_pvc", "PersistentVolumeClaim", ""),
	}, {
		testName:     "test missing name in snapshot datasource should fail",
		claimSpec:    *testDataSourceInSpec("", "VolumeSnapshot", "snapshot.storage.k8s.io"),
		expectedFail: true,
	}, {
		testName:     "test missing kind in snapshot datasource should fail",
		claimSpec:    *testDataSourceInSpec("test_snapshot", "", "snapshot.storage.k8s.io"),
		expectedFail: true,
	}, {
		testName:  "test create from valid generic custom resource source",
		claimSpec: *testDataSourceInSpec("test_generic", "Generic", "generic.storage.k8s.io"),
	}, {
		testName:     "test invalid datasource should fail",
		claimSpec:    *testDataSourceInSpec("test_pod", "Pod", ""),
		expectedFail: true,
	},
	}

	for _, tc := range testCases {
		opts := PersistentVolumeClaimSpecValidationOptions{}
		if tc.expectedFail {
			if errs := ValidatePersistentVolumeClaimSpec(&tc.claimSpec, field.NewPath("spec"), opts); len(errs) == 0 {
				t.Errorf("expected failure: %v", errs)
			}

		} else {
			if errs := ValidatePersistentVolumeClaimSpec(&tc.claimSpec, field.NewPath("spec"), opts); len(errs) != 0 {
				t.Errorf("expected success: %v", errs)
			}
		}
	}
}

func testAnyDataSource(t *testing.T, ds, dsRef bool) {
	testCases := []struct {
		testName     string
		claimSpec    core.PersistentVolumeClaimSpec
		expectedFail bool
	}{{
		testName:  "test create from valid snapshot source",
		claimSpec: *testDataSourceInSpec("test_snapshot", "VolumeSnapshot", "snapshot.storage.k8s.io"),
	}, {
		testName:  "test create from valid pvc source",
		claimSpec: *testDataSourceInSpec("test_pvc", "PersistentVolumeClaim", ""),
	}, {
		testName:     "test missing name in snapshot datasource should fail",
		claimSpec:    *testDataSourceInSpec("", "VolumeSnapshot", "snapshot.storage.k8s.io"),
		expectedFail: true,
	}, {
		testName:     "test missing kind in snapshot datasource should fail",
		claimSpec:    *testDataSourceInSpec("test_snapshot", "", "snapshot.storage.k8s.io"),
		expectedFail: true,
	}, {
		testName:  "test create from valid generic custom resource source",
		claimSpec: *testDataSourceInSpec("test_generic", "Generic", "generic.storage.k8s.io"),
	}, {
		testName:     "test invalid datasource should fail",
		claimSpec:    *testDataSourceInSpec("test_pod", "Pod", ""),
		expectedFail: true,
	},
	}

	for _, tc := range testCases {
		if dsRef {
			tc.claimSpec.DataSourceRef = &core.TypedObjectReference{
				APIGroup: tc.claimSpec.DataSource.APIGroup,
				Kind:     tc.claimSpec.DataSource.Kind,
				Name:     tc.claimSpec.DataSource.Name,
			}
		}
		if !ds {
			tc.claimSpec.DataSource = nil
		}
		opts := PersistentVolumeClaimSpecValidationOptions{}
		if tc.expectedFail {
			if errs := ValidatePersistentVolumeClaimSpec(&tc.claimSpec, field.NewPath("spec"), opts); len(errs) == 0 {
				t.Errorf("expected failure: %v", errs)
			}
		} else {
			if errs := ValidatePersistentVolumeClaimSpec(&tc.claimSpec, field.NewPath("spec"), opts); len(errs) != 0 {
				t.Errorf("expected success: %v", errs)
			}
		}
	}
}

func TestAnyDataSource(t *testing.T) {
	testAnyDataSource(t, true, false)
	testAnyDataSource(t, false, true)
	testAnyDataSource(t, true, false)
}

func pvcSpecWithCrossNamespaceSource(apiGroup *string, kind string, namespace *string, name string, isDataSourceSet bool) *core.PersistentVolumeClaimSpec {
	scName := "csi-plugin"
	spec := core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		StorageClassName: &scName,
		DataSourceRef: &core.TypedObjectReference{
			APIGroup:  apiGroup,
			Kind:      kind,
			Namespace: namespace,
			Name:      name,
		},
	}

	if isDataSourceSet {
		spec.DataSource = &core.TypedLocalObjectReference{
			APIGroup: apiGroup,
			Kind:     kind,
			Name:     name,
		}
	}
	return &spec
}

func TestCrossNamespaceSource(t *testing.T) {
	snapAPIGroup := "snapshot.storage.k8s.io"
	coreAPIGroup := ""
	unsupportedAPIGroup := "unsupported.example.com"
	snapKind := "VolumeSnapshot"
	pvcKind := "PersistentVolumeClaim"
	goodNS := "ns1"
	badNS := "a*b"
	emptyNS := ""
	goodName := "snapshot1"

	testCases := []struct {
		testName     string
		expectedFail bool
		claimSpec    *core.PersistentVolumeClaimSpec
	}{{
		testName:     "Feature gate enabled and valid xns DataSourceRef specified",
		expectedFail: false,
		claimSpec:    pvcSpecWithCrossNamespaceSource(&snapAPIGroup, snapKind, &goodNS, goodName, false),
	}, {
		testName:     "Feature gate enabled and xns DataSourceRef with PVC source specified",
		expectedFail: false,
		claimSpec:    pvcSpecWithCrossNamespaceSource(&coreAPIGroup, pvcKind, &goodNS, goodName, false),
	}, {
		testName:     "Feature gate enabled and xns DataSourceRef with unsupported source specified",
		expectedFail: false,
		claimSpec:    pvcSpecWithCrossNamespaceSource(&unsupportedAPIGroup, "UnsupportedKind", &goodNS, goodName, false),
	}, {
		testName:     "Feature gate enabled and xns DataSourceRef with nil apiGroup",
		expectedFail: true,
		claimSpec:    pvcSpecWithCrossNamespaceSource(nil, "UnsupportedKind", &goodNS, goodName, false),
	}, {
		testName:     "Feature gate enabled and xns DataSourceRef with invalid namespace specified",
		expectedFail: true,
		claimSpec:    pvcSpecWithCrossNamespaceSource(&snapAPIGroup, snapKind, &badNS, goodName, false),
	}, {
		testName:     "Feature gate enabled and xns DataSourceRef with nil namespace specified",
		expectedFail: false,
		claimSpec:    pvcSpecWithCrossNamespaceSource(&snapAPIGroup, snapKind, nil, goodName, false),
	}, {
		testName:     "Feature gate enabled and xns DataSourceRef with empty namespace specified",
		expectedFail: false,
		claimSpec:    pvcSpecWithCrossNamespaceSource(&snapAPIGroup, snapKind, &emptyNS, goodName, false),
	}, {
		testName:     "Feature gate enabled and both xns DataSourceRef and DataSource specified",
		expectedFail: true,
		claimSpec:    pvcSpecWithCrossNamespaceSource(&snapAPIGroup, snapKind, &goodNS, goodName, true),
	},
	}

	for _, tc := range testCases {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AnyVolumeDataSource, true)
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CrossNamespaceVolumeDataSource, true)
		opts := PersistentVolumeClaimSpecValidationOptions{}
		if tc.expectedFail {
			if errs := ValidatePersistentVolumeClaimSpec(tc.claimSpec, field.NewPath("spec"), opts); len(errs) == 0 {
				t.Errorf("%s: expected failure: %v", tc.testName, errs)
			}
		} else {
			if errs := ValidatePersistentVolumeClaimSpec(tc.claimSpec, field.NewPath("spec"), opts); len(errs) != 0 {
				t.Errorf("%s: expected success: %v", tc.testName, errs)
			}
		}
	}
}

func pvcSpecWithVolumeAttributesClassName(vacName *string) *core.PersistentVolumeClaimSpec {
	scName := "csi-plugin"
	spec := core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{
			core.ReadOnlyMany,
		},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceStorage): resource.MustParse("10G"),
			},
		},
		StorageClassName:          &scName,
		VolumeAttributesClassName: vacName,
	}
	return &spec
}

func TestVolumeAttributesClass(t *testing.T) {
	testCases := []struct {
		testName                    string
		expectedFail                bool
		enableVolumeAttributesClass bool
		claimSpec                   *core.PersistentVolumeClaimSpec
	}{
		{
			testName:                    "Feature gate enabled and valid no volumeAttributesClassName specified",
			expectedFail:                false,
			enableVolumeAttributesClass: true,
			claimSpec:                   pvcSpecWithVolumeAttributesClassName(nil),
		},
		{
			testName:                    "Feature gate enabled and an empty volumeAttributesClassName specified",
			expectedFail:                false,
			enableVolumeAttributesClass: true,
			claimSpec:                   pvcSpecWithVolumeAttributesClassName(utilpointer.String("")),
		},
		{
			testName:                    "Feature gate enabled and valid volumeAttributesClassName specified",
			expectedFail:                false,
			enableVolumeAttributesClass: true,
			claimSpec:                   pvcSpecWithVolumeAttributesClassName(utilpointer.String("foo")),
		},
		{
			testName:                    "Feature gate enabled and invalid volumeAttributesClassName specified",
			expectedFail:                true,
			enableVolumeAttributesClass: true,
			claimSpec:                   pvcSpecWithVolumeAttributesClassName(utilpointer.String("-invalid-")),
		},
	}
	for _, tc := range testCases {
		opts := PersistentVolumeClaimSpecValidationOptions{
			EnableVolumeAttributesClass: tc.enableVolumeAttributesClass,
		}
		if tc.expectedFail {
			if errs := ValidatePersistentVolumeClaimSpec(tc.claimSpec, field.NewPath("spec"), opts); len(errs) == 0 {
				t.Errorf("%s: expected failure: %v", tc.testName, errs)
			}
		} else {
			if errs := ValidatePersistentVolumeClaimSpec(tc.claimSpec, field.NewPath("spec"), opts); len(errs) != 0 {
				t.Errorf("%s: expected success: %v", tc.testName, errs)
			}
		}
	}
}

func TestValidateTopologySpreadConstraints(t *testing.T) {
	fieldPath := field.NewPath("field")
	subFldPath0 := fieldPath.Index(0)
	fieldPathMinDomains := subFldPath0.Child("minDomains")
	fieldPathMaxSkew := subFldPath0.Child("maxSkew")
	fieldPathTopologyKey := subFldPath0.Child("topologyKey")
	fieldPathWhenUnsatisfiable := subFldPath0.Child("whenUnsatisfiable")
	fieldPathTopologyKeyAndWhenUnsatisfiable := subFldPath0.Child("{topologyKey, whenUnsatisfiable}")
	fieldPathMatchLabelKeys := subFldPath0.Child("matchLabelKeys")
	nodeAffinityField := subFldPath0.Child("nodeAffinityPolicy")
	nodeTaintsField := subFldPath0.Child("nodeTaintsPolicy")
	labelSelectorField := subFldPath0.Child("labelSelector")
	unknown := core.NodeInclusionPolicy("Unknown")
	ignore := core.NodeInclusionPolicyIgnore
	honor := core.NodeInclusionPolicyHonor

	testCases := []struct {
		name            string
		constraints     []core.TopologySpreadConstraint
		wantFieldErrors field.ErrorList
		opts            PodValidationOptions
	}{{
		name: "all required fields ok",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.DoNotSchedule,
			MinDomains:        utilpointer.Int32(3),
		}},
		wantFieldErrors: field.ErrorList{},
	}, {
		name: "missing MaxSkew",
		constraints: []core.TopologySpreadConstraint{
			{TopologyKey: "k8s.io/zone", WhenUnsatisfiable: core.DoNotSchedule},
		},
		wantFieldErrors: []*field.Error{field.Invalid(fieldPathMaxSkew, int32(0), isNotPositiveErrorMsg)},
	}, {
		name: "negative MaxSkew",
		constraints: []core.TopologySpreadConstraint{
			{MaxSkew: -1, TopologyKey: "k8s.io/zone", WhenUnsatisfiable: core.DoNotSchedule},
		},
		wantFieldErrors: []*field.Error{field.Invalid(fieldPathMaxSkew, int32(-1), isNotPositiveErrorMsg)},
	}, {
		name: "can use MinDomains with ScheduleAnyway, when MinDomains = nil",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.ScheduleAnyway,
			MinDomains:        nil,
		}},
		wantFieldErrors: field.ErrorList{},
	}, {
		name: "negative minDomains is invalid",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.DoNotSchedule,
			MinDomains:        utilpointer.Int32(-1),
		}},
		wantFieldErrors: []*field.Error{field.Invalid(fieldPathMinDomains, utilpointer.Int32(-1), isNotPositiveErrorMsg)},
	}, {
		name: "cannot use non-nil MinDomains with ScheduleAnyway",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.ScheduleAnyway,
			MinDomains:        utilpointer.Int32(10),
		}},
		wantFieldErrors: []*field.Error{field.Invalid(fieldPathMinDomains, utilpointer.Int32(10), fmt.Sprintf("can only use minDomains if whenUnsatisfiable=%s, not %s", string(core.DoNotSchedule), string(core.ScheduleAnyway)))},
	}, {
		name: "use negative MinDomains with ScheduleAnyway(invalid)",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.ScheduleAnyway,
			MinDomains:        utilpointer.Int32(-1),
		}},
		wantFieldErrors: []*field.Error{
			field.Invalid(fieldPathMinDomains, utilpointer.Int32(-1), isNotPositiveErrorMsg),
			field.Invalid(fieldPathMinDomains, utilpointer.Int32(-1), fmt.Sprintf("can only use minDomains if whenUnsatisfiable=%s, not %s", string(core.DoNotSchedule), string(core.ScheduleAnyway))),
		},
	}, {
		name: "missing TopologyKey",
		constraints: []core.TopologySpreadConstraint{
			{MaxSkew: 1, WhenUnsatisfiable: core.DoNotSchedule},
		},
		wantFieldErrors: []*field.Error{field.Required(fieldPathTopologyKey, "can not be empty")},
	}, {
		name: "missing scheduling mode",
		constraints: []core.TopologySpreadConstraint{
			{MaxSkew: 1, TopologyKey: "k8s.io/zone"},
		},
		wantFieldErrors: []*field.Error{field.NotSupported(fieldPathWhenUnsatisfiable, core.UnsatisfiableConstraintAction(""), sets.List(supportedScheduleActions))},
	}, {
		name: "unsupported scheduling mode",
		constraints: []core.TopologySpreadConstraint{
			{MaxSkew: 1, TopologyKey: "k8s.io/zone", WhenUnsatisfiable: core.UnsatisfiableConstraintAction("N/A")},
		},
		wantFieldErrors: []*field.Error{field.NotSupported(fieldPathWhenUnsatisfiable, core.UnsatisfiableConstraintAction("N/A"), sets.List(supportedScheduleActions))},
	}, {
		name: "multiple constraints ok with all required fields",
		constraints: []core.TopologySpreadConstraint{
			{MaxSkew: 1, TopologyKey: "k8s.io/zone", WhenUnsatisfiable: core.DoNotSchedule},
			{MaxSkew: 2, TopologyKey: "k8s.io/node", WhenUnsatisfiable: core.ScheduleAnyway},
		},
		wantFieldErrors: field.ErrorList{},
	}, {
		name: "multiple constraints missing TopologyKey on partial ones",
		constraints: []core.TopologySpreadConstraint{
			{MaxSkew: 1, WhenUnsatisfiable: core.ScheduleAnyway},
			{MaxSkew: 2, TopologyKey: "k8s.io/zone", WhenUnsatisfiable: core.DoNotSchedule},
		},
		wantFieldErrors: []*field.Error{field.Required(fieldPathTopologyKey, "can not be empty")},
	}, {
		name: "duplicate constraints",
		constraints: []core.TopologySpreadConstraint{
			{MaxSkew: 1, TopologyKey: "k8s.io/zone", WhenUnsatisfiable: core.DoNotSchedule},
			{MaxSkew: 2, TopologyKey: "k8s.io/zone", WhenUnsatisfiable: core.DoNotSchedule},
		},
		wantFieldErrors: []*field.Error{
			field.Duplicate(fieldPathTopologyKeyAndWhenUnsatisfiable, fmt.Sprintf("{%v, %v}", "k8s.io/zone", core.DoNotSchedule)),
		},
	}, {
		name: "supported policy name set on NodeAffinityPolicy and NodeTaintsPolicy",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:            1,
			TopologyKey:        "k8s.io/zone",
			WhenUnsatisfiable:  core.DoNotSchedule,
			NodeAffinityPolicy: &honor,
			NodeTaintsPolicy:   &ignore,
		}},
		wantFieldErrors: []*field.Error{},
	}, {
		name: "unsupported policy name set on NodeAffinityPolicy",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:            1,
			TopologyKey:        "k8s.io/zone",
			WhenUnsatisfiable:  core.DoNotSchedule,
			NodeAffinityPolicy: &unknown,
			NodeTaintsPolicy:   &ignore,
		}},
		wantFieldErrors: []*field.Error{
			field.NotSupported(nodeAffinityField, &unknown, sets.List(supportedPodTopologySpreadNodePolicies)),
		},
	}, {
		name: "unsupported policy name set on NodeTaintsPolicy",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:            1,
			TopologyKey:        "k8s.io/zone",
			WhenUnsatisfiable:  core.DoNotSchedule,
			NodeAffinityPolicy: &honor,
			NodeTaintsPolicy:   &unknown,
		}},
		wantFieldErrors: []*field.Error{
			field.NotSupported(nodeTaintsField, &unknown, sets.List(supportedPodTopologySpreadNodePolicies)),
		},
	}, {
		name: "key in MatchLabelKeys isn't correctly defined",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			LabelSelector:     &metav1.LabelSelector{},
			WhenUnsatisfiable: core.DoNotSchedule,
			MatchLabelKeys:    []string{"/simple"},
		}},
		wantFieldErrors: field.ErrorList{field.Invalid(fieldPathMatchLabelKeys.Index(0), "/simple", "prefix part must be non-empty")},
	}, {
		name: "key exists in both matchLabelKeys and labelSelector",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.DoNotSchedule,
			MatchLabelKeys:    []string{"foo"},
			LabelSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "foo",
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{"value1", "value2"},
					},
				},
			},
		}},
		wantFieldErrors: field.ErrorList{field.Invalid(fieldPathMatchLabelKeys.Index(0), "foo", "exists in both matchLabelKeys and labelSelector")},
	}, {
		name: "key in MatchLabelKeys is forbidden to be specified when labelSelector is not set",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.DoNotSchedule,
			MatchLabelKeys:    []string{"foo"},
		}},
		wantFieldErrors: field.ErrorList{field.Forbidden(fieldPathMatchLabelKeys, "must not be specified when labelSelector is not set")},
	}, {
		name: "invalid matchLabels set on labelSelector when AllowInvalidTopologySpreadConstraintLabelSelector is false",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.DoNotSchedule,
			MinDomains:        nil,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "foo"}},
		}},
		wantFieldErrors: []*field.Error{field.Invalid(labelSelectorField.Child("matchLabels"), "NoUppercaseOrSpecialCharsLike=Equals", "name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]')")},
		opts:            PodValidationOptions{AllowInvalidTopologySpreadConstraintLabelSelector: false},
	}, {
		name: "invalid matchLabels set on labelSelector when AllowInvalidTopologySpreadConstraintLabelSelector is true",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.DoNotSchedule,
			MinDomains:        nil,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "foo"}},
		}},
		wantFieldErrors: []*field.Error{},
		opts:            PodValidationOptions{AllowInvalidTopologySpreadConstraintLabelSelector: true},
	}, {
		name: "valid matchLabels set on labelSelector when AllowInvalidTopologySpreadConstraintLabelSelector is false",
		constraints: []core.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "k8s.io/zone",
			WhenUnsatisfiable: core.DoNotSchedule,
			MinDomains:        nil,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "foo"}},
		}},
		wantFieldErrors: []*field.Error{},
		opts:            PodValidationOptions{AllowInvalidTopologySpreadConstraintLabelSelector: false},
	},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateTopologySpreadConstraints(tc.constraints, fieldPath, tc.opts)
			if diff := cmp.Diff(tc.wantFieldErrors, errs); diff != "" {
				t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestValidateOverhead(t *testing.T) {
	successCase := []struct {
		Name     string
		overhead core.ResourceList
	}{{
		Name: "Valid Overhead for CPU + Memory",
		overhead: core.ResourceList{
			core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
			core.ResourceName(core.ResourceMemory): resource.MustParse("10G"),
		},
	},
	}
	for _, tc := range successCase {
		if errs := validateOverhead(tc.overhead, field.NewPath("overheads"), PodValidationOptions{}); len(errs) != 0 {
			t.Errorf("%q unexpected error: %v", tc.Name, errs)
		}
	}

	errorCase := []struct {
		Name     string
		overhead core.ResourceList
	}{{
		Name: "Invalid Overhead Resources",
		overhead: core.ResourceList{
			core.ResourceName("my.org"): resource.MustParse("10m"),
		},
	},
	}
	for _, tc := range errorCase {
		if errs := validateOverhead(tc.overhead, field.NewPath("resources"), PodValidationOptions{}); len(errs) == 0 {
			t.Errorf("%q expected error", tc.Name)
		}
	}
}

// helper creates a pod with name, namespace and IPs
func makePod(podName string, podNamespace string, podIPs []core.PodIP) core.Pod {
	return core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: podNamespace},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name: "ctr", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File",
			}},
			RestartPolicy: core.RestartPolicyAlways,
			DNSPolicy:     core.DNSClusterFirst,
		},
		Status: core.PodStatus{
			PodIPs: podIPs,
		},
	}
}
func TestPodIPsValidation(t *testing.T) {
	testCases := []struct {
		pod         core.Pod
		expectError bool
	}{{
		expectError: false,
		pod:         makePod("nil-ips", "ns", nil),
	}, {
		expectError: false,
		pod:         makePod("empty-podips-list", "ns", []core.PodIP{}),
	}, {
		expectError: false,
		pod:         makePod("single-ip-family-6", "ns", []core.PodIP{{IP: "::1"}}),
	}, {
		expectError: false,
		pod:         makePod("single-ip-family-4", "ns", []core.PodIP{{IP: "1.1.1.1"}}),
	}, {
		expectError: false,
		pod:         makePod("dual-stack-4-6", "ns", []core.PodIP{{IP: "1.1.1.1"}, {IP: "::1"}}),
	}, {
		expectError: false,
		pod:         makePod("dual-stack-6-4", "ns", []core.PodIP{{IP: "::1"}, {IP: "1.1.1.1"}}),
	},
		/* failure cases start here */
		{
			expectError: true,
			pod:         makePod("invalid-pod-ip", "ns", []core.PodIP{{IP: "this-is-not-an-ip"}}),
		}, {
			expectError: true,
			pod:         makePod("dualstack-same-ip-family-6", "ns", []core.PodIP{{IP: "::1"}, {IP: "::2"}}),
		}, {
			expectError: true,
			pod:         makePod("dualstack-same-ip-family-4", "ns", []core.PodIP{{IP: "1.1.1.1"}, {IP: "2.2.2.2"}}),
		}, {
			expectError: true,
			pod:         makePod("dualstack-repeated-ip-family-6", "ns", []core.PodIP{{IP: "1.1.1.1"}, {IP: "::1"}, {IP: "::2"}}),
		}, {
			expectError: true,
			pod:         makePod("dualstack-repeated-ip-family-4", "ns", []core.PodIP{{IP: "1.1.1.1"}, {IP: "::1"}, {IP: "2.2.2.2"}}),
		},

		{
			expectError: true,
			pod:         makePod("dualstack-duplicate-ip-family-4", "ns", []core.PodIP{{IP: "1.1.1.1"}, {IP: "1.1.1.1"}, {IP: "::1"}}),
		}, {
			expectError: true,
			pod:         makePod("dualstack-duplicate-ip-family-6", "ns", []core.PodIP{{IP: "1.1.1.1"}, {IP: "::1"}, {IP: "::1"}}),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.pod.Name, func(t *testing.T) {
			for _, oldTestCase := range testCases {
				newPod := testCase.pod.DeepCopy()
				newPod.ResourceVersion = "1"

				oldPod := oldTestCase.pod.DeepCopy()
				oldPod.ResourceVersion = "1"
				oldPod.Name = newPod.Name

				errs := ValidatePodStatusUpdate(newPod, oldPod, PodValidationOptions{})

				if len(errs) == 0 && testCase.expectError {
					t.Fatalf("expected failure for %s, but there were none", testCase.pod.Name)
				}
				if len(errs) != 0 && !testCase.expectError {
					t.Fatalf("expected success for %s, but there were errors: %v", testCase.pod.Name, errs)
				}
			}
		})
	}
}

func makePodWithHostIPs(podName string, podNamespace string, hostIPs []core.HostIP) core.Pod {
	hostIP := ""
	if len(hostIPs) > 0 {
		hostIP = hostIPs[0].IP
	}
	return core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: podNamespace},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{
					Name: "ctr", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File",
				},
			},
			RestartPolicy: core.RestartPolicyAlways,
			DNSPolicy:     core.DNSClusterFirst,
		},
		Status: core.PodStatus{
			HostIP:  hostIP,
			HostIPs: hostIPs,
		},
	}
}

func TestHostIPsValidation(t *testing.T) {
	testCases := []struct {
		pod         core.Pod
		expectError bool
	}{
		{
			expectError: false,
			pod:         makePodWithHostIPs("nil-ips", "ns", nil),
		},
		{
			expectError: false,
			pod:         makePodWithHostIPs("empty-HostIPs-list", "ns", []core.HostIP{}),
		},
		{
			expectError: false,
			pod:         makePodWithHostIPs("single-ip-family-6", "ns", []core.HostIP{{IP: "::1"}}),
		},
		{
			expectError: false,
			pod:         makePodWithHostIPs("single-ip-family-4", "ns", []core.HostIP{{IP: "1.1.1.1"}}),
		},
		{
			expectError: false,
			pod:         makePodWithHostIPs("dual-stack-4-6", "ns", []core.HostIP{{IP: "1.1.1.1"}, {IP: "::1"}}),
		},
		{
			expectError: false,
			pod:         makePodWithHostIPs("dual-stack-6-4", "ns", []core.HostIP{{IP: "::1"}, {IP: "1.1.1.1"}}),
		},
		/* failure cases start here */
		{
			expectError: true,
			pod:         makePodWithHostIPs("invalid-pod-ip", "ns", []core.HostIP{{IP: "this-is-not-an-ip"}}),
		},
		{
			expectError: true,
			pod:         makePodWithHostIPs("dualstack-same-ip-family-6", "ns", []core.HostIP{{IP: "::1"}, {IP: "::2"}}),
		},
		{
			expectError: true,
			pod:         makePodWithHostIPs("dualstack-same-ip-family-4", "ns", []core.HostIP{{IP: "1.1.1.1"}, {IP: "2.2.2.2"}}),
		},
		{
			expectError: true,
			pod:         makePodWithHostIPs("dualstack-repeated-ip-family-6", "ns", []core.HostIP{{IP: "1.1.1.1"}, {IP: "::1"}, {IP: "::2"}}),
		},
		{
			expectError: true,
			pod:         makePodWithHostIPs("dualstack-repeated-ip-family-4", "ns", []core.HostIP{{IP: "1.1.1.1"}, {IP: "::1"}, {IP: "2.2.2.2"}}),
		},

		{
			expectError: true,
			pod:         makePodWithHostIPs("dualstack-duplicate-ip-family-4", "ns", []core.HostIP{{IP: "1.1.1.1"}, {IP: "1.1.1.1"}, {IP: "::1"}}),
		},
		{
			expectError: true,
			pod:         makePodWithHostIPs("dualstack-duplicate-ip-family-6", "ns", []core.HostIP{{IP: "1.1.1.1"}, {IP: "::1"}, {IP: "::1"}}),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.pod.Name, func(t *testing.T) {
			for _, oldTestCase := range testCases {
				newPod := testCase.pod.DeepCopy()
				newPod.ResourceVersion = "1"

				oldPod := oldTestCase.pod.DeepCopy()
				oldPod.ResourceVersion = "1"
				oldPod.Name = newPod.Name

				errs := ValidatePodStatusUpdate(newPod, oldPod, PodValidationOptions{})

				if len(errs) == 0 && testCase.expectError {
					t.Fatalf("expected failure for %s, but there were none", testCase.pod.Name)
				}
				if len(errs) != 0 && !testCase.expectError {
					t.Fatalf("expected success for %s, but there were errors: %v", testCase.pod.Name, errs)
				}
			}
		})
	}
}

// makes a node with pod cidr and a name
func makeNode(nodeName string, podCIDRs []string) core.Node {
	return core.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Type: core.NodeExternalIP, Address: "something"},
			},
			Capacity: core.ResourceList{
				core.ResourceName(core.ResourceCPU):    resource.MustParse("10"),
				core.ResourceName(core.ResourceMemory): resource.MustParse("0"),
			},
		},
		Spec: core.NodeSpec{
			PodCIDRs: podCIDRs,
		},
	}
}
func TestValidateNodeCIDRs(t *testing.T) {
	testCases := []struct {
		expectError bool
		node        core.Node
	}{{
		expectError: false,
		node:        makeNode("nil-pod-cidr", nil),
	}, {
		expectError: false,
		node:        makeNode("empty-pod-cidr", []string{}),
	}, {
		expectError: false,
		node:        makeNode("single-pod-cidr-4", []string{"192.168.0.0/16"}),
	}, {
		expectError: false,
		node:        makeNode("single-pod-cidr-6", []string{"2000::/10"}),
	},

		{
			expectError: false,
			node:        makeNode("multi-pod-cidr-6-4", []string{"2000::/10", "192.168.0.0/16"}),
		}, {
			expectError: false,
			node:        makeNode("multi-pod-cidr-4-6", []string{"192.168.0.0/16", "2000::/10"}),
		},
		// error cases starts here
		{
			expectError: true,
			node:        makeNode("invalid-pod-cidr", []string{"this-is-not-a-valid-cidr"}),
		}, {
			expectError: true,
			node:        makeNode("duplicate-pod-cidr-4", []string{"10.0.0.1/16", "10.0.0.1/16"}),
		}, {
			expectError: true,
			node:        makeNode("duplicate-pod-cidr-6", []string{"2000::/10", "2000::/10"}),
		}, {
			expectError: true,
			node:        makeNode("not-a-dualstack-no-v4", []string{"2000::/10", "3000::/10"}),
		}, {
			expectError: true,
			node:        makeNode("not-a-dualstack-no-v6", []string{"10.0.0.0/16", "10.1.0.0/16"}),
		}, {
			expectError: true,
			node:        makeNode("not-a-dualstack-repeated-v6", []string{"2000::/10", "10.0.0.0/16", "3000::/10"}),
		}, {
			expectError: true,
			node:        makeNode("not-a-dualstack-repeated-v4", []string{"10.0.0.0/16", "3000::/10", "10.1.0.0/16"}),
		},
	}
	for _, testCase := range testCases {
		errs := ValidateNode(&testCase.node)
		if len(errs) == 0 && testCase.expectError {
			t.Errorf("expected failure for %s, but there were none", testCase.node.Name)
			return
		}
		if len(errs) != 0 && !testCase.expectError {
			t.Errorf("expected success for %s, but there were errors: %v", testCase.node.Name, errs)
			return
		}
	}
}

func TestValidateSeccompAnnotationAndField(t *testing.T) {
	const containerName = "container"
	testProfile := "test"

	for _, test := range []struct {
		description string
		pod         *core.Pod
		validation  func(*testing.T, string, field.ErrorList, *v1.Pod)
	}{{
		description: "Field type unconfined and annotation does not match",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompPodAnnotationKey: "not-matching",
				},
			},
			Spec: core.PodSpec{
				SecurityContext: &core.PodSecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type: core.SeccompProfileTypeUnconfined,
					},
				},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.NotNil(t, allErrs, desc)
		},
	}, {
		description: "Field type default and annotation does not match",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompPodAnnotationKey: "not-matching",
				},
			},
			Spec: core.PodSpec{
				SecurityContext: &core.PodSecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type: core.SeccompProfileTypeRuntimeDefault,
					},
				},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.NotNil(t, allErrs, desc)
		},
	}, {
		description: "Field type localhost and annotation does not match",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompPodAnnotationKey: "not-matching",
				},
			},
			Spec: core.PodSpec{
				SecurityContext: &core.PodSecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type:             core.SeccompProfileTypeLocalhost,
						LocalhostProfile: &testProfile,
					},
				},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.NotNil(t, allErrs, desc)
		},
	}, {
		description: "Field type localhost and localhost/ prefixed annotation does not match",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompPodAnnotationKey: "localhost/not-matching",
				},
			},
			Spec: core.PodSpec{
				SecurityContext: &core.PodSecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type:             core.SeccompProfileTypeLocalhost,
						LocalhostProfile: &testProfile,
					},
				},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.NotNil(t, allErrs, desc)
		},
	}, {
		description: "Field type unconfined and annotation does not match (container)",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompContainerAnnotationKeyPrefix + containerName: "not-matching",
				},
			},
			Spec: core.PodSpec{
				Containers: []core.Container{{
					Name: containerName,
					SecurityContext: &core.SecurityContext{
						SeccompProfile: &core.SeccompProfile{
							Type: core.SeccompProfileTypeUnconfined,
						},
					},
				}},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.NotNil(t, allErrs, desc)
		},
	}, {
		description: "Field type default and annotation does not match (container)",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompContainerAnnotationKeyPrefix + containerName: "not-matching",
				},
			},
			Spec: core.PodSpec{
				Containers: []core.Container{{
					Name: containerName,
					SecurityContext: &core.SecurityContext{
						SeccompProfile: &core.SeccompProfile{
							Type: core.SeccompProfileTypeRuntimeDefault,
						},
					},
				}},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.NotNil(t, allErrs, desc)
		},
	}, {
		description: "Field type localhost and annotation does not match (container)",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompContainerAnnotationKeyPrefix + containerName: "not-matching",
				},
			},
			Spec: core.PodSpec{
				Containers: []core.Container{{
					Name: containerName,
					SecurityContext: &core.SecurityContext{
						SeccompProfile: &core.SeccompProfile{
							Type:             core.SeccompProfileTypeLocalhost,
							LocalhostProfile: &testProfile,
						},
					},
				}},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.NotNil(t, allErrs, desc)
		},
	}, {
		description: "Field type localhost and localhost/ prefixed annotation does not match (container)",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompContainerAnnotationKeyPrefix + containerName: "localhost/not-matching",
				},
			},
			Spec: core.PodSpec{
				Containers: []core.Container{{
					Name: containerName,
					SecurityContext: &core.SecurityContext{
						SeccompProfile: &core.SeccompProfile{
							Type:             core.SeccompProfileTypeLocalhost,
							LocalhostProfile: &testProfile,
						},
					},
				}},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.NotNil(t, allErrs, desc)
		},
	}, {
		description: "Nil errors must not be appended (pod)",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompPodAnnotationKey: "localhost/anyprofile",
				},
			},
			Spec: core.PodSpec{
				SecurityContext: &core.PodSecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type: "Abc",
					},
				},
				Containers: []core.Container{{
					Name: containerName,
				}},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.Empty(t, allErrs, desc)
		},
	}, {
		description: "Nil errors must not be appended (container)",
		pod: &core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.SeccompContainerAnnotationKeyPrefix + containerName: "localhost/not-matching",
				},
			},
			Spec: core.PodSpec{
				Containers: []core.Container{{
					SecurityContext: &core.SecurityContext{
						SeccompProfile: &core.SeccompProfile{
							Type: "Abc",
						},
					},
					Name: containerName,
				}},
			},
		},
		validation: func(t *testing.T, desc string, allErrs field.ErrorList, pod *v1.Pod) {
			require.Empty(t, allErrs, desc)
		},
	},
	} {
		output := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
		}
		for i, ctr := range test.pod.Spec.Containers {
			output.Spec.Containers = append(output.Spec.Containers, v1.Container{})
			if ctr.SecurityContext != nil && ctr.SecurityContext.SeccompProfile != nil {
				output.Spec.Containers[i].SecurityContext = &v1.SecurityContext{
					SeccompProfile: &v1.SeccompProfile{
						Type:             v1.SeccompProfileType(ctr.SecurityContext.SeccompProfile.Type),
						LocalhostProfile: ctr.SecurityContext.SeccompProfile.LocalhostProfile,
					},
				}
			}
		}
		errList := validateSeccompAnnotationsAndFields(test.pod.ObjectMeta, &test.pod.Spec, field.NewPath(""))
		test.validation(t, test.description, errList, output)
	}
}

func TestValidateSeccompAnnotationsAndFieldsMatch(t *testing.T) {
	rootFld := field.NewPath("")
	tests := []struct {
		description     string
		annotationValue string
		seccompField    *core.SeccompProfile
		fldPath         *field.Path
		expectedErr     *field.Error
	}{{
		description: "seccompField nil should return empty",
		expectedErr: nil,
	}, {
		description:     "unconfined annotation and SeccompProfileTypeUnconfined should return empty",
		annotationValue: "unconfined",
		seccompField:    &core.SeccompProfile{Type: core.SeccompProfileTypeUnconfined},
		expectedErr:     nil,
	}, {
		description:     "runtime/default annotation and SeccompProfileTypeRuntimeDefault should return empty",
		annotationValue: "runtime/default",
		seccompField:    &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
		expectedErr:     nil,
	}, {
		description:     "docker/default annotation and SeccompProfileTypeRuntimeDefault should return empty",
		annotationValue: "docker/default",
		seccompField:    &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
		expectedErr:     nil,
	}, {
		description:     "localhost/test.json annotation and SeccompProfileTypeLocalhost with correct profile should return empty",
		annotationValue: "localhost/test.json",
		seccompField:    &core.SeccompProfile{Type: core.SeccompProfileTypeLocalhost, LocalhostProfile: utilpointer.String("test.json")},
		expectedErr:     nil,
	}, {
		description:     "localhost/test.json annotation and SeccompProfileTypeLocalhost without profile should error",
		annotationValue: "localhost/test.json",
		seccompField:    &core.SeccompProfile{Type: core.SeccompProfileTypeLocalhost},
		fldPath:         rootFld,
		expectedErr:     field.Forbidden(rootFld.Child("localhostProfile"), "seccomp profile in annotation and field must match"),
	}, {
		description:     "localhost/test.json annotation and SeccompProfileTypeLocalhost with different profile should error",
		annotationValue: "localhost/test.json",
		seccompField:    &core.SeccompProfile{Type: core.SeccompProfileTypeLocalhost, LocalhostProfile: utilpointer.String("different.json")},
		fldPath:         rootFld,
		expectedErr:     field.Forbidden(rootFld.Child("localhostProfile"), "seccomp profile in annotation and field must match"),
	}, {
		description:     "localhost/test.json annotation and SeccompProfileTypeUnconfined with different profile should error",
		annotationValue: "localhost/test.json",
		seccompField:    &core.SeccompProfile{Type: core.SeccompProfileTypeUnconfined},
		fldPath:         rootFld,
		expectedErr:     field.Forbidden(rootFld.Child("type"), "seccomp type in annotation and field must match"),
	}, {
		description:     "localhost/test.json annotation and SeccompProfileTypeRuntimeDefault with different profile should error",
		annotationValue: "localhost/test.json",
		seccompField:    &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
		fldPath:         rootFld,
		expectedErr:     field.Forbidden(rootFld.Child("type"), "seccomp type in annotation and field must match"),
	},
	}

	for i, test := range tests {
		err := validateSeccompAnnotationsAndFieldsMatch(test.annotationValue, test.seccompField, test.fldPath)
		assert.Equal(t, test.expectedErr, err, "TestCase[%d]: %s", i, test.description)
	}
}

func TestValidatePodTemplateSpecSeccomp(t *testing.T) {
	rootFld := field.NewPath("template")
	tests := []struct {
		description string
		spec        *core.PodTemplateSpec
		fldPath     *field.Path
		expectedErr field.ErrorList
	}{{
		description: "seccomp field and container annotation must match",
		fldPath:     rootFld,
		expectedErr: field.ErrorList{
			field.Forbidden(
				rootFld.Child("spec").Child("containers").Index(1).Child("securityContext").Child("seccompProfile").Child("type"),
				"seccomp type in annotation and field must match"),
		},
		spec: &core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"container.seccomp.security.alpha.kubernetes.io/test2": "unconfined",
				},
			},
			Spec: podtest.MakePodSpec(
				podtest.SetContainers(
					podtest.MakeContainer("test1"),
					podtest.MakeContainer("test2",
						podtest.SetContainerSecurityContext(core.SecurityContext{
							SeccompProfile: &core.SeccompProfile{
								Type: core.SeccompProfileTypeRuntimeDefault,
							},
						}))),
			),
		},
	}, {
		description: "seccomp field and pod annotation must match",
		fldPath:     rootFld,
		expectedErr: field.ErrorList{
			field.Forbidden(
				rootFld.Child("spec").Child("securityContext").Child("seccompProfile").Child("type"),
				"seccomp type in annotation and field must match"),
		},
		spec: &core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"seccomp.security.alpha.kubernetes.io/pod": "runtime/default",
				},
			},
			Spec: podtest.MakePodSpec(
				podtest.SetSecurityContext(&core.PodSecurityContext{
					SeccompProfile: &core.SeccompProfile{
						Type: core.SeccompProfileTypeUnconfined,
					},
				}),
			),
		},
	}, {
		description: "init seccomp field and container annotation must match",
		fldPath:     rootFld,
		expectedErr: field.ErrorList{
			field.Forbidden(
				rootFld.Child("spec").Child("initContainers").Index(0).Child("securityContext").Child("seccompProfile").Child("type"),
				"seccomp type in annotation and field must match"),
		},
		spec: &core.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"container.seccomp.security.alpha.kubernetes.io/init-test": "unconfined",
				},
			},
			Spec: podtest.MakePodSpec(
				podtest.SetInitContainers(podtest.MakeContainer("init-test",
					podtest.SetContainerSecurityContext(core.SecurityContext{
						SeccompProfile: &core.SeccompProfile{
							Type: core.SeccompProfileTypeRuntimeDefault,
						},
					}))),
			),
		},
	},
	}

	for i, test := range tests {
		err := ValidatePodTemplateSpec(test.spec, rootFld, PodValidationOptions{})
		assert.Equal(t, test.expectedErr, err, "TestCase[%d]: %s", i, test.description)
	}
}

func TestValidateResourceRequirements(t *testing.T) {
	path := field.NewPath("resources")
	// TODO(ndixita): refactor the tests to check the expected errors are equal to
	// got errors.
	tests := []struct {
		name         string
		requirements core.ResourceRequirements
		validateFn   func(requirements *core.ResourceRequirements,
			podClaimNames sets.Set[string], fldPath *field.Path,
			opts PodValidationOptions) field.ErrorList
	}{{
		name: "limits and requests of hugepage resource are equal",
		requirements: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceCPU: resource.MustParse("10"),
				core.ResourceName(core.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("2Mi"),
			},
			Requests: core.ResourceList{
				core.ResourceCPU: resource.MustParse("10"),
				core.ResourceName(core.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("2Mi"),
			},
		},
		validateFn: ValidateContainerResourceRequirements,
	}, {
		name: "limits and requests of memory resource are equal",
		requirements: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceMemory: resource.MustParse("2Mi"),
			},
			Requests: core.ResourceList{
				core.ResourceMemory: resource.MustParse("2Mi"),
			},
		},
		validateFn: ValidateContainerResourceRequirements,
	}, {
		name: "limits and requests of cpu resource are equal",
		requirements: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceCPU: resource.MustParse("10"),
			},
			Requests: core.ResourceList{
				core.ResourceCPU: resource.MustParse("10"),
			},
		},
		validateFn: ValidateContainerResourceRequirements,
	},
		{
			name: "limits and requests of memory resource are equal",
			requirements: core.ResourceRequirements{
				Limits: core.ResourceList{
					core.ResourceMemory: resource.MustParse("2Mi"),
				},
				Requests: core.ResourceList{
					core.ResourceMemory: resource.MustParse("2Mi"),
				},
			},
			validateFn: validatePodResourceRequirements,
		}, {
			name: "limits and requests of cpu resource are equal",
			requirements: core.ResourceRequirements{
				Limits: core.ResourceList{
					core.ResourceCPU: resource.MustParse("10"),
				},
				Requests: core.ResourceList{
					core.ResourceCPU: resource.MustParse("10"),
				},
			},
			validateFn: validatePodResourceRequirements,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if errs := tc.validateFn(&tc.requirements, nil, path, PodValidationOptions{}); len(errs) != 0 {
				t.Errorf("unexpected errors: %v", errs)
			}
		})
	}

	errTests := []struct {
		name         string
		requirements core.ResourceRequirements
		validateFn   func(requirements *core.ResourceRequirements,
			podClaimNames sets.Set[string], fldPath *field.Path,
			opts PodValidationOptions) field.ErrorList
	}{{
		name: "hugepage resource without cpu or memory",
		requirements: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceName(core.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("2Mi"),
			},
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("2Mi"),
			},
		},
		validateFn: ValidateContainerResourceRequirements,
	}, {
		name: "pod resource with hugepages",
		requirements: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceName(core.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("2Mi"),
			},
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("2Mi"),
			},
		},
		validateFn: validatePodResourceRequirements,
	}, {
		name: "pod resource with ephemeral-storage",
		requirements: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceName(core.ResourceEphemeralStorage): resource.MustParse("2Mi"),
			},
			Requests: core.ResourceList{
				core.ResourceName(core.ResourceEphemeralStorage + "2Mi"): resource.MustParse("2Mi"),
			},
		},
		validateFn: validatePodResourceRequirements,
	}, {
		name: "pod resource with unsupported prefixed resources",
		requirements: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceName("kubernetesio/" + core.ResourceCPU): resource.MustParse("2"),
			},
			Requests: core.ResourceList{
				core.ResourceName("kubernetesio/" + core.ResourceMemory): resource.MustParse("2"),
			},
		},
		validateFn: validatePodResourceRequirements,
	}, {
		name: "pod resource with unsupported native resources",
		requirements: core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceName("kubernetes.io/" + strings.Repeat("a", 63)): resource.MustParse("2"),
			},
			Requests: core.ResourceList{
				core.ResourceName("kubernetes.io/" + strings.Repeat("a", 63)): resource.MustParse("2"),
			},
		},
		validateFn: validatePodResourceRequirements,
	},
		{
			name: "pod resource with unsupported empty native resource name",
			requirements: core.ResourceRequirements{
				Limits: core.ResourceList{
					core.ResourceName("kubernetes.io/"): resource.MustParse("2"),
				},
				Requests: core.ResourceList{
					core.ResourceName("kubernetes.io"): resource.MustParse("2"),
				},
			},
			validateFn: validatePodResourceRequirements,
		},
	}

	for _, tc := range errTests {
		t.Run(tc.name, func(t *testing.T) {
			if errs := tc.validateFn(&tc.requirements, nil, path, PodValidationOptions{}); len(errs) == 0 {
				t.Error("expected errors")
			}
		})
	}
}

func TestValidateNonSpecialIP(t *testing.T) {
	fp := field.NewPath("ip")

	// Valid values.
	for _, tc := range []struct {
		desc string
		ip   string
	}{
		{"ipv4", "10.1.2.3"},
		{"ipv4 class E", "244.1.2.3"},
		{"ipv6", "2000::1"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			errs := ValidateNonSpecialIP(tc.ip, fp)
			if len(errs) != 0 {
				t.Errorf("ValidateNonSpecialIP(%q, ...) = %v; want nil", tc.ip, errs)
			}
		})
	}
	// Invalid cases
	for _, tc := range []struct {
		desc string
		ip   string
	}{
		{"ipv4 unspecified", "0.0.0.0"},
		{"ipv6 unspecified", "::0"},
		{"ipv4 localhost", "127.0.0.0"},
		{"ipv4 localhost", "127.255.255.255"},
		{"ipv6 localhost", "::1"},
		{"ipv6 link local", "fe80::"},
		{"ipv6 local multicast", "ff02::"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			errs := ValidateNonSpecialIP(tc.ip, fp)
			if len(errs) == 0 {
				t.Errorf("ValidateNonSpecialIP(%q, ...) = nil; want non-nil (errors)", tc.ip)
			}
		})
	}
}

func TestValidateHostUsers(t *testing.T) {
	falseVar := false
	trueVar := true

	cases := []struct {
		name    string
		success bool
		spec    *core.PodSpec
	}{{
		name:    "empty",
		success: true,
		spec:    &core.PodSpec{},
	}, {
		name:    "hostUsers unset",
		success: true,
		spec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{},
		},
	}, {
		name:    "hostUsers=false",
		success: true,
		spec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostUsers: &falseVar,
			},
		},
	}, {
		name:    "hostUsers=true",
		success: true,
		spec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostUsers: &trueVar,
			},
		},
	}, {
		name:    "hostUsers=false & volumes",
		success: true,
		spec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostUsers: &falseVar,
			},
			Volumes: []core.Volume{{
				Name: "configmap",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{Name: "configmap"},
					},
				},
			}, {
				Name: "secret",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName: "secret",
					},
				},
			}, {
				Name: "downward-api",
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{},
				},
			}, {
				Name: "proj",
				VolumeSource: core.VolumeSource{
					Projected: &core.ProjectedVolumeSource{},
				},
			}, {
				Name: "empty-dir",
				VolumeSource: core.VolumeSource{
					EmptyDir: &core.EmptyDirVolumeSource{},
				},
			}},
		},
	}, {
		name:    "hostUsers=false - stateful volume",
		success: true,
		spec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostUsers: &falseVar,
			},
			Volumes: []core.Volume{{
				Name: "host-path",
				VolumeSource: core.VolumeSource{
					HostPath: &core.HostPathVolumeSource{},
				},
			}},
		},
	}, {
		name:    "hostUsers=true - unsupported volume",
		success: true,
		spec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostUsers: &trueVar,
			},
			Volumes: []core.Volume{{
				Name: "host-path",
				VolumeSource: core.VolumeSource{
					HostPath: &core.HostPathVolumeSource{},
				},
			}},
		},
	}, {
		name:    "hostUsers=false & HostNetwork",
		success: false,
		spec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostUsers:   &falseVar,
				HostNetwork: true,
			},
		},
	}, {
		name:    "hostUsers=false & HostPID",
		success: false,
		spec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostUsers: &falseVar,
				HostPID:   true,
			},
		},
	}, {
		name:    "hostUsers=false & HostIPC",
		success: false,
		spec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostUsers: &falseVar,
				HostIPC:   true,
			},
		},
	},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fPath := field.NewPath("spec")

			allErrs := validateHostUsers(tc.spec, fPath)
			if !tc.success && len(allErrs) == 0 {
				t.Errorf("Unexpected success")
			}
			if tc.success && len(allErrs) != 0 {
				t.Errorf("Unexpected error(s): %v", allErrs)
			}
		})
	}
}

func TestValidateWindowsHostProcessPod(t *testing.T) {
	const containerName = "container"
	falseVar := false
	trueVar := true

	testCases := []struct {
		name            string
		expectError     bool
		allowPrivileged bool
		podSpec         *core.PodSpec
	}{{
		name:            "Spec with feature enabled, pod-wide HostProcess=true, and HostNetwork unset should not validate",
		expectError:     true,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &trueVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
			}},
		},
	}, {
		name:            "Spec with feature enabled, pod-wide HostProcess=ture, and HostNetwork set should validate",
		expectError:     false,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &trueVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
			}},
		},
	}, {
		name:            "Spec with feature enabled, pod-wide HostProcess=ture, HostNetwork set, and containers setting HostProcess=true should validate",
		expectError:     false,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &trueVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
			InitContainers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
		},
	}, {
		name:            "Spec with feature enabled, pod-wide HostProcess=nil, HostNetwork set, and all containers setting HostProcess=true should validate",
		expectError:     false,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
			InitContainers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
		},
	}, {
		name:            "Pods with feature enabled, some containers setting HostProcess=true, and others setting HostProcess=false should not validate",
		expectError:     true,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
			InitContainers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &falseVar,
					},
				},
			}},
		},
	}, {
		name:            "Spec with feature enabled, some containers setting HostProcess=true, and other leaving HostProcess unset should not validate",
		expectError:     true,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
			InitContainers: []core.Container{{
				Name: containerName,
			}},
		},
	}, {
		name:            "Spec with feature enabled, pod-wide HostProcess=true, some containers setting HostProcess=true, and init containers setting HostProcess=false should not validate",
		expectError:     true,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &trueVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
			InitContainers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &falseVar,
					},
				},
			}},
		},
	}, {
		name:            "Spec with feature enabled, pod-wide HostProcess=true, some containers setting HostProcess=true, and others setting HostProcess=false should not validate",
		expectError:     true,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &trueVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}, {
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &falseVar,
					},
				},
			}},
		},
	}, {
		name:            "Spec with feature enabled, pod-wide HostProcess=true, some containers setting HostProcess=true, and others leaving HostProcess=nil should validate",
		expectError:     false,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &trueVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
			InitContainers: []core.Container{{
				Name: containerName,
			}},
		},
	}, {
		name:            "Spec with feature enabled, pod-wide HostProcess=false, some contaienrs setting HostProccess=true should not validate",
		expectError:     true,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &falseVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
			InitContainers: []core.Container{{
				Name: containerName,
			}},
		},
	}, {
		name:            "Pod's HostProcess set to true but all containers override to false should not validate",
		expectError:     true,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &trueVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &falseVar,
					},
				},
			}},
		},
	}, {
		name:            "Valid HostProcess pod should spec should not validate if allowPrivileged is not set",
		expectError:     true,
		allowPrivileged: false,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
			},
			Containers: []core.Container{{
				Name: containerName,
				SecurityContext: &core.SecurityContext{
					WindowsOptions: &core.WindowsSecurityContextOptions{
						HostProcess: &trueVar,
					},
				},
			}},
		},
	}, {
		name:            "Non-HostProcess ephemeral container in HostProcess pod should not validate",
		expectError:     true,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &trueVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
			}},
			EphemeralContainers: []core.EphemeralContainer{{
				EphemeralContainerCommon: core.EphemeralContainerCommon{
					SecurityContext: &core.SecurityContext{
						WindowsOptions: &core.WindowsSecurityContextOptions{
							HostProcess: &falseVar,
						},
					},
				},
			}},
		},
	}, {
		name:            "HostProcess ephemeral container in HostProcess pod should validate",
		expectError:     false,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			SecurityContext: &core.PodSecurityContext{
				HostNetwork: true,
				WindowsOptions: &core.WindowsSecurityContextOptions{
					HostProcess: &trueVar,
				},
			},
			Containers: []core.Container{{
				Name: containerName,
			}},
			EphemeralContainers: []core.EphemeralContainer{{
				EphemeralContainerCommon: core.EphemeralContainerCommon{},
			}},
		},
	}, {
		name:            "Non-HostProcess ephemeral container in Non-HostProcess pod should validate",
		expectError:     false,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			Containers: []core.Container{{
				Name: containerName,
			}},
			EphemeralContainers: []core.EphemeralContainer{{
				EphemeralContainerCommon: core.EphemeralContainerCommon{
					SecurityContext: &core.SecurityContext{
						WindowsOptions: &core.WindowsSecurityContextOptions{
							HostProcess: &falseVar,
						},
					},
				},
			}},
		},
	}, {
		name:            "HostProcess ephemeral container in Non-HostProcess pod should not validate",
		expectError:     true,
		allowPrivileged: true,
		podSpec: &core.PodSpec{
			Containers: []core.Container{{
				Name: containerName,
			}},
			EphemeralContainers: []core.EphemeralContainer{{
				EphemeralContainerCommon: core.EphemeralContainerCommon{
					SecurityContext: &core.SecurityContext{
						WindowsOptions: &core.WindowsSecurityContextOptions{
							HostProcess: &trueVar,
						},
					},
				},
			}},
		},
	},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			capabilities.ResetForTest()
			capabilities.Initialize(capabilities.Capabilities{
				AllowPrivileged: testCase.allowPrivileged,
			})

			errs := validateWindowsHostProcessPod(testCase.podSpec, field.NewPath("spec"))
			if testCase.expectError && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if !testCase.expectError && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

func TestValidateOS(t *testing.T) {
	testCases := []struct {
		name        string
		expectError bool
		podSpec     *core.PodSpec
	}{{
		name:        "no OS field, featuregate",
		expectError: false,
		podSpec:     &core.PodSpec{OS: nil},
	}, {
		name:        "empty OS field, featuregate",
		expectError: true,
		podSpec:     &core.PodSpec{OS: &core.PodOS{}},
	}, {
		name:        "OS field, featuregate, valid OS",
		expectError: false,
		podSpec:     &core.PodSpec{OS: &core.PodOS{Name: core.Linux}},
	}, {
		name:        "OS field, featuregate, valid OS",
		expectError: false,
		podSpec:     &core.PodSpec{OS: &core.PodOS{Name: core.Windows}},
	}, {
		name:        "OS field, featuregate, empty OS",
		expectError: true,
		podSpec:     &core.PodSpec{OS: &core.PodOS{Name: ""}},
	}, {
		name:        "OS field, featuregate, invalid OS",
		expectError: true,
		podSpec:     &core.PodSpec{OS: &core.PodOS{Name: "dummyOS"}},
	},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			errs := validateOS(testCase.podSpec, field.NewPath("spec"), PodValidationOptions{})
			if testCase.expectError && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if !testCase.expectError && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

func TestValidateAppArmorProfileFormat(t *testing.T) {
	tests := []struct {
		profile     string
		expectValid bool
	}{
		{"", true},
		{v1.DeprecatedAppArmorBetaProfileRuntimeDefault, true},
		{v1.DeprecatedAppArmorBetaProfileNameUnconfined, true},
		{"baz", false}, // Missing local prefix.
		{v1.DeprecatedAppArmorBetaProfileNamePrefix + "/usr/sbin/ntpd", true},
		{v1.DeprecatedAppArmorBetaProfileNamePrefix + "foo-bar", true},
	}

	for _, test := range tests {
		err := ValidateAppArmorProfileFormat(test.profile)
		if test.expectValid {
			assert.NoError(t, err, "Profile %s should be valid", test.profile)
		} else {
			assert.Errorf(t, err, "Profile %s should not be valid", test.profile)
		}
	}
}

func TestValidatePVSecretReference(t *testing.T) {
	rootFld := field.NewPath("name")
	type args struct {
		secretRef *core.SecretReference
		fldPath   *field.Path
	}
	tests := []struct {
		name          string
		args          args
		expectError   bool
		expectedError string
	}{{
		name:          "invalid secret ref name",
		args:          args{&core.SecretReference{Name: "$%^&*#", Namespace: "default"}, rootFld},
		expectError:   true,
		expectedError: "name.name: Invalid value: \"$%^&*#\": " + dnsSubdomainLabelErrMsg,
	}, {
		name:          "invalid secret ref namespace",
		args:          args{&core.SecretReference{Name: "valid", Namespace: "$%^&*#"}, rootFld},
		expectError:   true,
		expectedError: "name.namespace: Invalid value: \"$%^&*#\": " + dnsLabelErrMsg,
	}, {
		name:          "invalid secret: missing namespace",
		args:          args{&core.SecretReference{Name: "valid"}, rootFld},
		expectError:   true,
		expectedError: "name.namespace: Required value",
	}, {
		name:          "invalid secret : missing name",
		args:          args{&core.SecretReference{Namespace: "default"}, rootFld},
		expectError:   true,
		expectedError: "name.name: Required value",
	}, {
		name:          "valid secret",
		args:          args{&core.SecretReference{Name: "valid", Namespace: "default"}, rootFld},
		expectError:   false,
		expectedError: "",
	},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validatePVSecretReference(tt.args.secretRef, tt.args.fldPath)
			if tt.expectError && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if tt.expectError && len(errs) != 0 {
				str := errs[0].Error()
				if str != "" && !strings.Contains(str, tt.expectedError) {
					t.Errorf("%s: expected error detail either empty or %q, got %q", tt.name, tt.expectedError, str)
				}
			}
			if !tt.expectError && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

func TestValidateDynamicResourceAllocation(t *testing.T) {
	externalClaimName := "some-claim"
	externalClaimTemplateName := "some-claim-template"
	shortPodName := &metav1.ObjectMeta{
		Name: "some-pod",
	}
	requestName := "req-0"
	anotherRequestName := "req-1"
	goodClaimTemplate := podtest.MakePod("",
		podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim-template"}}}))),
		podtest.SetRestartPolicy(core.RestartPolicyAlways),
		podtest.SetResourceClaims(core.PodResourceClaim{
			Name:                      "my-claim-template",
			ResourceClaimTemplateName: &externalClaimTemplateName,
		}),
	)
	goodClaimReference := podtest.MakePod("",
		podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim-reference"}}}))),
		podtest.SetRestartPolicy(core.RestartPolicyAlways),
		podtest.SetResourceClaims(core.PodResourceClaim{
			Name:              "my-claim-reference",
			ResourceClaimName: &externalClaimName,
		}),
	)

	successCases := map[string]*core.Pod{
		"resource claim reference": goodClaimReference,
		"resource claim template":  goodClaimTemplate,
		"multiple claims": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim"}, {Name: "another-claim"}}}))),
			podtest.SetResourceClaims(
				core.PodResourceClaim{
					Name:              "my-claim",
					ResourceClaimName: &externalClaimName,
				},
				core.PodResourceClaim{
					Name:              "another-claim",
					ResourceClaimName: &externalClaimName,
				}),
		),
		"multiple claims with requests": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim", Request: requestName}, {Name: "another-claim", Request: requestName}}}))),
			podtest.SetResourceClaims(
				core.PodResourceClaim{
					Name:              "my-claim",
					ResourceClaimName: &externalClaimName,
				},
				core.PodResourceClaim{
					Name:              "another-claim",
					ResourceClaimName: &externalClaimName,
				}),
		),
		"single claim with requests": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim", Request: requestName}, {Name: "my-claim", Request: anotherRequestName}}}))),
			podtest.SetResourceClaims(
				core.PodResourceClaim{
					Name:              "my-claim",
					ResourceClaimName: &externalClaimName,
				}),
		),
		"init container": podtest.MakePod("",
			podtest.SetInitContainers(podtest.MakeContainer("ctr-init", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim"}}}))),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
	}
	for k, v := range successCases {
		t.Run(k, func(t *testing.T) {
			if errs := ValidatePodSpec(&v.Spec, shortPodName, field.NewPath("field"), PodValidationOptions{}); len(errs) != 0 {
				t.Errorf("expected success: %v", errs)
			}
		})
	}

	failureCases := map[string]*core.Pod{
		"pod claim name with prefix": podtest.MakePod("",
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "../my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"pod claim name with path": podtest.MakePod("",
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my/claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"pod claim name empty": podtest.MakePod("",
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"duplicate pod claim entries": podtest.MakePod("",
			podtest.SetResourceClaims(
				core.PodResourceClaim{
					Name:              "my-claim",
					ResourceClaimName: &externalClaimName,
				},
				core.PodResourceClaim{
					Name:              "my-claim",
					ResourceClaimName: &externalClaimName,
				}),
		),
		"resource claim source empty": podtest.MakePod("",
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name: "my-claim",
			}),
		),
		"resource claim reference and template": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim"}}}))),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:                      "my-claim",
				ResourceClaimName:         &externalClaimName,
				ResourceClaimTemplateName: &externalClaimTemplateName,
			}),
		),
		"claim not found": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "no-such-claim"}}}))),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"claim name empty": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: ""}}}))),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"pod claim name duplicates": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim"}, {Name: "my-claim"}}}))),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"pod claim name duplicates without and with request": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim"}, {Name: "my-claim", Request: "req-0"}}}))),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"pod claim name duplicates with and without request": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim", Request: "req-0"}, {Name: "my-claim"}}}))),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"pod claim name duplicates with requests": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim", Request: "req-0"}, {Name: "my-claim", Request: "req-0"}}}))),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"bad request name": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim", Request: "*$@%^"}}}))),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"no claims defined": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim"}}}))),
			podtest.SetRestartPolicy(core.RestartPolicyAlways),
		),
		"duplicate pod claim name": podtest.MakePod("",
			podtest.SetContainers(podtest.MakeContainer("ctr", podtest.SetContainerResources(core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim"}}}))),
			podtest.SetRestartPolicy(core.RestartPolicyAlways),
			podtest.SetResourceClaims(
				core.PodResourceClaim{
					Name:              "my-claim",
					ResourceClaimName: &externalClaimName,
				},
				core.PodResourceClaim{
					Name:              "my-claim",
					ResourceClaimName: &externalClaimName,
				}),
		),
		"ephemeral container don't support resource requirements": podtest.MakePod("",
			podtest.SetEphemeralContainers(core.EphemeralContainer{EphemeralContainerCommon: core.EphemeralContainerCommon{Name: "ctr-ephemeral", Image: "image", ImagePullPolicy: "IfNotPresent", TerminationMessagePolicy: "File", Resources: core.ResourceRequirements{Claims: []core.ResourceClaim{{Name: "my-claim"}}}}, TargetContainerName: "ctr"}),
			podtest.SetResourceClaims(core.PodResourceClaim{
				Name:              "my-claim",
				ResourceClaimName: &externalClaimName,
			}),
		),
		"invalid claim template name": func() *core.Pod {
			pod := goodClaimTemplate.DeepCopy()
			notLabel := ".foo_bar"
			pod.Spec.ResourceClaims[0].ResourceClaimTemplateName = &notLabel
			return pod
		}(),
		"invalid claim reference name": func() *core.Pod {
			pod := goodClaimReference.DeepCopy()
			notLabel := ".foo_bar"
			pod.Spec.ResourceClaims[0].ResourceClaimName = &notLabel
			return pod
		}(),
	}
	for k, v := range failureCases {
		if errs := ValidatePodSpec(&v.Spec, nil, field.NewPath("field"), PodValidationOptions{}); len(errs) == 0 {
			t.Errorf("expected failure for %q", k)
		}
	}
}

func TestValidateLoadBalancerStatus(t *testing.T) {
	ipModeVIP := core.LoadBalancerIPModeVIP
	ipModeProxy := core.LoadBalancerIPModeProxy
	ipModeDummy := core.LoadBalancerIPMode("dummy")

	testCases := []struct {
		name          string
		ipModeEnabled bool
		nonLBAllowed  bool
		tweakLBStatus func(s *core.LoadBalancerStatus)
		tweakSvcSpec  func(s *core.ServiceSpec)
		numErrs       int
	}{
		{
			name:         "type is not LB",
			nonLBAllowed: false,
			tweakSvcSpec: func(s *core.ServiceSpec) {
				s.Type = core.ServiceTypeClusterIP
			},
			tweakLBStatus: func(s *core.LoadBalancerStatus) {
				s.Ingress = []core.LoadBalancerIngress{{
					IP: "1.2.3.4",
				}}
			},
			numErrs: 1,
		}, {
			name:         "type is not LB. back-compat",
			nonLBAllowed: true,
			tweakSvcSpec: func(s *core.ServiceSpec) {
				s.Type = core.ServiceTypeClusterIP
			},
			tweakLBStatus: func(s *core.LoadBalancerStatus) {
				s.Ingress = []core.LoadBalancerIngress{{
					IP: "1.2.3.4",
				}}
			},
			numErrs: 0,
		}, {
			name:          "valid vip ipMode",
			ipModeEnabled: true,
			tweakLBStatus: func(s *core.LoadBalancerStatus) {
				s.Ingress = []core.LoadBalancerIngress{{
					IP:     "1.2.3.4",
					IPMode: &ipModeVIP,
				}}
			},
			numErrs: 0,
		}, {
			name:          "valid proxy ipMode",
			ipModeEnabled: true,
			tweakLBStatus: func(s *core.LoadBalancerStatus) {
				s.Ingress = []core.LoadBalancerIngress{{
					IP:     "1.2.3.4",
					IPMode: &ipModeProxy,
				}}
			},
			numErrs: 0,
		}, {
			name:          "invalid ipMode",
			ipModeEnabled: true,
			tweakLBStatus: func(s *core.LoadBalancerStatus) {
				s.Ingress = []core.LoadBalancerIngress{{
					IP:     "1.2.3.4",
					IPMode: &ipModeDummy,
				}}
			},
			numErrs: 1,
		}, {
			name:          "missing ipMode with LoadbalancerIPMode enabled",
			ipModeEnabled: true,
			tweakLBStatus: func(s *core.LoadBalancerStatus) {
				s.Ingress = []core.LoadBalancerIngress{{
					IP: "1.2.3.4",
				}}
			},
			numErrs: 1,
		}, {
			name:          "missing ipMode with LoadbalancerIPMode disabled",
			ipModeEnabled: false,
			tweakLBStatus: func(s *core.LoadBalancerStatus) {
				s.Ingress = []core.LoadBalancerIngress{{
					IP: "1.2.3.4",
				}}
			},
			numErrs: 0,
		}, {
			name:          "missing ip with ipMode present",
			ipModeEnabled: true,
			tweakLBStatus: func(s *core.LoadBalancerStatus) {
				s.Ingress = []core.LoadBalancerIngress{{
					IPMode: &ipModeProxy,
				}}
			},
			numErrs: 1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.ipModeEnabled {
				featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.31"))
			}
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.LoadBalancerIPMode, tc.ipModeEnabled)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AllowServiceLBStatusOnNonLB, tc.nonLBAllowed)
			status := core.LoadBalancerStatus{}
			tc.tweakLBStatus(&status)
			spec := core.ServiceSpec{Type: core.ServiceTypeLoadBalancer}
			if tc.tweakSvcSpec != nil {
				tc.tweakSvcSpec(&spec)
			}
			errs := ValidateLoadBalancerStatus(&status, field.NewPath("status"), &spec)
			if len(errs) != tc.numErrs {
				t.Errorf("Unexpected error list for case %q(expected:%v got %v) - Errors:\n %v", tc.name, tc.numErrs, len(errs), errs.ToAggregate())
			}
		})
	}
}

func TestValidateSleepAction(t *testing.T) {
	fldPath := field.NewPath("root")
	getInvalidStr := func(gracePeriod int64) string {
		return fmt.Sprintf("must be greater than 0 and less than terminationGracePeriodSeconds (%d). Enable AllowPodLifecycleSleepActionZeroValue feature gate for zero sleep.", gracePeriod)
	}

	getInvalidStrWithZeroValueEnabled := func(gracePeriod int64) string {
		return fmt.Sprintf("must be non-negative and less than terminationGracePeriodSeconds (%d)", gracePeriod)
	}

	testCases := []struct {
		name             string
		action           *core.SleepAction
		gracePeriod      int64
		zeroValueEnabled bool
		expectErr        field.ErrorList
	}{
		{
			name: "valid setting",
			action: &core.SleepAction{
				Seconds: 5,
			},
			gracePeriod:      30,
			zeroValueEnabled: false,
		},
		{
			name: "negative seconds",
			action: &core.SleepAction{
				Seconds: -1,
			},
			gracePeriod:      30,
			zeroValueEnabled: false,
			expectErr:        field.ErrorList{field.Invalid(fldPath, -1, getInvalidStr(30))},
		},
		{
			name: "longer than gracePeriod",
			action: &core.SleepAction{
				Seconds: 5,
			},
			gracePeriod:      3,
			zeroValueEnabled: false,
			expectErr:        field.ErrorList{field.Invalid(fldPath, 5, getInvalidStr(3))},
		},
		{
			name: "sleep duration of zero with zero value feature gate disabled",
			action: &core.SleepAction{
				Seconds: 0,
			},
			gracePeriod:      30,
			zeroValueEnabled: false,
			expectErr:        field.ErrorList{field.Invalid(fldPath, 0, getInvalidStr(30))},
		},
		{
			name: "sleep duration of zero with zero value feature gate enabled",
			action: &core.SleepAction{
				Seconds: 0,
			},
			gracePeriod:      30,
			zeroValueEnabled: true,
		},
		{
			name: "invalid sleep duration (negative value) with zero value disabled",
			action: &core.SleepAction{
				Seconds: -1,
			},
			gracePeriod:      30,
			zeroValueEnabled: false,
			expectErr:        field.ErrorList{field.Invalid(fldPath, -1, getInvalidStr(30))},
		},
		{
			name: "invalid sleep duration (negative value) with zero value enabled",
			action: &core.SleepAction{
				Seconds: -1,
			},
			gracePeriod:      30,
			zeroValueEnabled: true,
			expectErr:        field.ErrorList{field.Invalid(fldPath, -1, getInvalidStrWithZeroValueEnabled(30))},
		},
		{
			name: "zero grace period duration with zero value enabled",
			action: &core.SleepAction{
				Seconds: 0,
			},
			gracePeriod:      0,
			zeroValueEnabled: true,
		},
		{
			name: "nil grace period with zero value disabled",
			action: &core.SleepAction{
				Seconds: 5,
			},
			zeroValueEnabled: false,
			expectErr:        field.ErrorList{field.Invalid(fldPath, 5, getInvalidStr(0))},
		},
		{
			name: "nil grace period with zero value enabled",
			action: &core.SleepAction{
				Seconds: 0,
			},
			zeroValueEnabled: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateSleepAction(tc.action, &tc.gracePeriod, fldPath, PodValidationOptions{AllowPodLifecycleSleepActionZeroValue: tc.zeroValueEnabled})

			if len(tc.expectErr) > 0 && len(errs) == 0 {
				t.Errorf("Unexpected success")
			} else if len(tc.expectErr) == 0 && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			} else if len(tc.expectErr) > 0 {
				if tc.expectErr[0].Error() != errs[0].Error() {
					t.Errorf("Unexpected error(s): %v", errs)
				}
			}
		})
	}
}

// TODO: merge these test to TestValidatePodSpec after AllowRelaxedDNSSearchValidation feature graduates to Beta
func TestValidatePodDNSConfigWithRelaxedSearchDomain(t *testing.T) {
	testCases := []struct {
		name           string
		expectError    bool
		featureEnabled bool
		dnsConfig      *core.PodDNSConfig
	}{
		{
			name:           "beginswith underscore, contains underscore, featuregate enabled",
			expectError:    false,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"_sip._tcp.abc_d.example.com"}},
		},
		{
			name:           "contains underscore, featuregate enabled",
			expectError:    false,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"abc_d.example.com"}},
		},
		{
			name:           "is dot, featuregate enabled",
			expectError:    false,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"."}},
		},
		{
			name:           "two dots, featuregate enabled",
			expectError:    true,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{".."}},
		},
		{
			name:           "underscore and dot, featuregate enabled",
			expectError:    true,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"_."}},
		},
		{
			name:           "dash and dot, featuregate enabled",
			expectError:    true,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"-."}},
		},
		{
			name:           "two underscore and dot, featuregate enabled",
			expectError:    true,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"__."}},
		},
		{
			name:           "dot and two underscore, featuregate enabled",
			expectError:    true,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{".__"}},
		},
		{
			name:           "dot and underscore, featuregate enabled",
			expectError:    true,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"._"}},
		},
		{
			name:           "lot of underscores, featuregate enabled",
			expectError:    true,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"____________"}},
		},
		{
			name:           "a regular name, featuregate enabled",
			expectError:    false,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"example.com"}},
		},
		{
			name:           "unicode character, featuregate enabled",
			expectError:    true,
			featureEnabled: true,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"☃.example.com"}},
		},
		{
			name:           "begins with underscore, contains underscore, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"_sip._tcp.abc_d.example.com"}},
		},
		{
			name:           "contains underscore, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"abc_d.example.com"}},
		},
		{
			name:           "is dot, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"."}},
		},
		{
			name:           "two dots, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{".."}},
		},
		{
			name:           "underscore and dot, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"_."}},
		},
		{
			name:           "dash and dot, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"-."}},
		},
		{
			name:           "two underscore and dot, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"__."}},
		},
		{
			name:           "dot and two underscore, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{".__"}},
		},
		{
			name:           "dot and underscore, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"._"}},
		},
		{
			name:           "lot of underscores, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"____________"}},
		},
		{
			name:           "a regular name, featuregate disabled",
			expectError:    false,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"example.com"}},
		},
		{
			name:           "unicode character, featuregate disabled",
			expectError:    true,
			featureEnabled: false,
			dnsConfig:      &core.PodDNSConfig{Searches: []string{"☃.example.com"}},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			errs := validatePodDNSConfig(testCase.dnsConfig, nil, nil, PodValidationOptions{AllowRelaxedDNSSearchValidation: testCase.featureEnabled})
			if testCase.expectError && len(errs) == 0 {
				t.Errorf("Unexpected success")
			}
			if !testCase.expectError && len(errs) != 0 {
				t.Errorf("Unexpected error(s): %v", errs)
			}
		})
	}
}

// TODO: merge these test to TestValidatePodSpec after SupplementalGroupsPolicy feature graduates to Beta
func TestValidatePodSpecWithSupplementalGroupsPolicy(t *testing.T) {
	fldPath := field.NewPath("spec")
	badSupplementalGroupsPolicyEmpty := ptr.To(core.SupplementalGroupsPolicy(""))
	badSupplementalGroupsPolicyNotSupported := ptr.To(core.SupplementalGroupsPolicy("not-supported"))

	validatePodSpecTestCases := map[string]struct {
		securityContext *core.PodSecurityContext
		wantFieldErrors field.ErrorList
	}{
		"nil SecurityContext is valid": {
			securityContext: nil,
		},
		"nil SupplementalGroupsPolicy is valid": {
			securityContext: &core.PodSecurityContext{},
		},
		"SupplementalGroupsPolicyMerge is valid": {
			securityContext: &core.PodSecurityContext{
				SupplementalGroupsPolicy: ptr.To(core.SupplementalGroupsPolicyMerge),
			},
		},
		"SupplementalGroupsPolicyStrict is valid": {
			securityContext: &core.PodSecurityContext{
				SupplementalGroupsPolicy: ptr.To(core.SupplementalGroupsPolicyStrict),
			},
		},
		"empty SupplementalGroupsPolicy is invalid": {
			securityContext: &core.PodSecurityContext{
				SupplementalGroupsPolicy: badSupplementalGroupsPolicyEmpty,
			},
			wantFieldErrors: field.ErrorList{
				field.NotSupported(
					fldPath.Child("securityContext").Child("supplementalGroupsPolicy"),
					badSupplementalGroupsPolicyEmpty, sets.List(validSupplementalGroupsPolicies)),
			},
		},
		"not-supported SupplementalGroupsPolicy is invalid": {
			securityContext: &core.PodSecurityContext{
				SupplementalGroupsPolicy: badSupplementalGroupsPolicyNotSupported,
			},
			wantFieldErrors: field.ErrorList{
				field.NotSupported(
					fldPath.Child("securityContext").Child("supplementalGroupsPolicy"),
					badSupplementalGroupsPolicyNotSupported, sets.List(validSupplementalGroupsPolicies)),
			},
		},
	}
	for name, tt := range validatePodSpecTestCases {
		t.Run(name, func(t *testing.T) {
			podSpec := podtest.MakePodSpec(podtest.SetSecurityContext(tt.securityContext), podtest.SetContainers(podtest.MakeContainer("con")))

			if tt.wantFieldErrors == nil {
				tt.wantFieldErrors = field.ErrorList{}
			}
			errs := ValidatePodSpec(&podSpec, nil, fldPath, PodValidationOptions{})
			if diff := cmp.Diff(tt.wantFieldErrors, errs); diff != "" {
				t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
			}
		})
	}
}

// TODO: merge these testcases to TestValidateWindowsPodSecurityContext after SupplementalGroupsPolicy feature graduates to Beta
func TestValidateWindowsPodSecurityContextSupplementalGroupsPolicy(t *testing.T) {
	fldPath := field.NewPath("spec")

	testCases := map[string]struct {
		securityContext *core.PodSecurityContext
		wantFieldErrors field.ErrorList
	}{
		"nil SecurityContext is valid": {
			securityContext: nil,
		},
		"nil SupplementalGroupdPolicy is valid": {
			securityContext: &core.PodSecurityContext{},
		},
		"non-empty SupplementalGroupdPolicy is invalid": {
			securityContext: &core.PodSecurityContext{
				SupplementalGroupsPolicy: ptr.To(core.SupplementalGroupsPolicyMerge),
			},
			wantFieldErrors: field.ErrorList{
				field.Forbidden(
					fldPath.Child("securityContext").Child("supplementalGroupsPolicy"),
					"cannot be set for a windows pod"),
			},
		},
	}

	for name, tt := range testCases {
		t.Run(name, func(t *testing.T) {
			podSpec := podtest.MakePodSpec(podtest.SetSecurityContext(tt.securityContext), podtest.SetOS(core.Windows), podtest.SetContainers(podtest.MakeContainer("con")))
			if tt.wantFieldErrors == nil {
				tt.wantFieldErrors = field.ErrorList{}
			}
			errs := validateWindows(&podSpec, fldPath)
			if diff := cmp.Diff(tt.wantFieldErrors, errs); diff != "" {
				t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
			}
		})
	}
}

// TODO: merge these testcases to TestValidatePodStatusUpdate after SupplementalGroupsPolicy feature graduates to Beta
func TestValidatePodStatusUpdateWithSupplementalGroupsPolicy(t *testing.T) {
	badUID := int64(-1)
	badGID := int64(-1)

	containerTypes := map[string]func(podStatus *core.PodStatus, containerStatus []core.ContainerStatus){
		"container": func(podStatus *core.PodStatus, containerStatus []core.ContainerStatus) {
			podStatus.ContainerStatuses = containerStatus
		},
		"initContainer": func(podStatus *core.PodStatus, containerStatus []core.ContainerStatus) {
			podStatus.InitContainerStatuses = containerStatus
		},
		"ephemeralContainer": func(podStatus *core.PodStatus, containerStatus []core.ContainerStatus) {
			podStatus.EphemeralContainerStatuses = containerStatus
		},
	}

	testCases := map[string]struct {
		podOSes           []*core.PodOS
		containerStatuses []core.ContainerStatus
		wantFieldErrors   field.ErrorList
	}{
		"nil container user is valid": {
			podOSes:           []*core.PodOS{nil, {Name: core.Linux}},
			containerStatuses: []core.ContainerStatus{},
		},
		"empty container user is valid": {
			podOSes: []*core.PodOS{nil, {Name: core.Linux}},
			containerStatuses: []core.ContainerStatus{{
				User: &core.ContainerUser{},
			}},
		},
		"container user with valid ids": {
			podOSes: []*core.PodOS{nil, {Name: core.Linux}},
			containerStatuses: []core.ContainerStatus{{
				User: &core.ContainerUser{
					Linux: &core.LinuxContainerUser{},
				},
			}},
		},
		"container user with invalid ids": {
			podOSes: []*core.PodOS{nil, {Name: core.Linux}},
			containerStatuses: []core.ContainerStatus{{
				User: &core.ContainerUser{
					Linux: &core.LinuxContainerUser{
						UID:                badUID,
						GID:                badGID,
						SupplementalGroups: []int64{badGID},
					},
				},
			}},
			wantFieldErrors: field.ErrorList{
				field.Invalid(field.NewPath("[0].linux.uid"), badUID, "must be between 0 and 2147483647, inclusive"),
				field.Invalid(field.NewPath("[0].linux.gid"), badGID, "must be between 0 and 2147483647, inclusive"),
				field.Invalid(field.NewPath("[0].linux.supplementalGroups[0]"), badGID, "must be between 0 and 2147483647, inclusive"),
			},
		},
		"user.linux must not be set in windows": {
			podOSes: []*core.PodOS{{Name: core.Windows}},
			containerStatuses: []core.ContainerStatus{{
				User: &core.ContainerUser{
					Linux: &core.LinuxContainerUser{},
				},
			}},
			wantFieldErrors: field.ErrorList{
				field.Forbidden(field.NewPath("[0].linux"), "cannot be set for a windows pod"),
			},
		},
	}

	for name, tt := range testCases {
		for _, podOS := range tt.podOSes {
			for containerType, setContainerStatuses := range containerTypes {
				t.Run(fmt.Sprintf("[podOS=%v][containerType=%s] %s", podOS, containerType, name), func(t *testing.T) {
					oldPod := &core.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:            "foo",
							ResourceVersion: "1",
						},
						Spec: core.PodSpec{
							OS: podOS,
						},
						Status: core.PodStatus{},
					}
					newPod := &core.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:            "foo",
							ResourceVersion: "1",
						},
						Spec: core.PodSpec{
							OS: podOS,
						},
					}
					setContainerStatuses(&newPod.Status, tt.containerStatuses)
					var expectedFieldErrors field.ErrorList
					for _, err := range tt.wantFieldErrors {
						expectedField := fmt.Sprintf("%s%s", field.NewPath("status").Child(containerType+"Statuses"), err.Field)
						expectedFieldErrors = append(expectedFieldErrors, &field.Error{
							Type:     err.Type,
							Field:    expectedField,
							BadValue: err.BadValue,
							Detail:   err.Detail,
						})
					}
					errs := ValidatePodStatusUpdate(newPod, oldPod, PodValidationOptions{})
					if diff := cmp.Diff(expectedFieldErrors, errs); diff != "" {
						t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
					}
				})
			}
		}
	}
}
func TestValidateContainerStatusNoAllocatedResourcesStatus(t *testing.T) {
	containerStatuses := []core.ContainerStatus{
		{
			Name: "container-1",
		},
		{
			Name: "container-2",
			AllocatedResourcesStatus: []core.ResourceStatus{
				{
					Name:      "test.device/test",
					Resources: nil,
				},
			},
		},
		{
			Name: "container-3",
			AllocatedResourcesStatus: []core.ResourceStatus{
				{
					Name: "test.device/test",
					Resources: []core.ResourceHealth{
						{
							ResourceID: "resource-1",
							Health:     core.ResourceHealthStatusHealthy,
						},
					},
				},
			},
		},
	}

	fldPath := field.NewPath("spec", "containers")

	errs := validateContainerStatusNoAllocatedResourcesStatus(containerStatuses, fldPath)

	assert.Len(t, errs, 2)
	assert.Equal(t, "spec.containers[1].allocatedResourcesStatus", errs[0].Field)
	assert.Equal(t, "must not be specified in container status", errs[0].Detail)
	assert.Equal(t, "spec.containers[2].allocatedResourcesStatus", errs[1].Field)
	assert.Equal(t, "must not be specified in container status", errs[1].Detail)
}

func TestValidateContainerStatusAllocatedResourcesStatus(t *testing.T) {
	fldPath := field.NewPath("spec", "containers")

	testCases := map[string]struct {
		containers        []core.Container
		containerStatuses []core.ContainerStatus
		wantFieldErrors   field.ErrorList
	}{
		"basic correct status": {
			containers: []core.Container{
				{
					Name: "container-1",
					Resources: core.ResourceRequirements{
						Requests: core.ResourceList{
							"test.device/test": resource.MustParse("1"),
						},
					},
				},
			},
			containerStatuses: []core.ContainerStatus{
				{
					Name: "container-1",
					AllocatedResourcesStatus: []core.ResourceStatus{
						{
							Name: "test.device/test",
							Resources: []core.ResourceHealth{
								{
									ResourceID: "resource-1",
									Health:     core.ResourceHealthStatusHealthy,
								},
							},
						},
					},
				},
			},
			wantFieldErrors: field.ErrorList{},
		},
		"ignoring the missing container (see https://github.com/kubernetes/kubernetes/issues/124915)": {
			containers: []core.Container{
				{
					Name: "container-2",
					Resources: core.ResourceRequirements{
						Requests: core.ResourceList{
							"test.device/test": resource.MustParse("1"),
						},
					},
				},
			},
			containerStatuses: []core.ContainerStatus{
				{
					Name: "container-1",
					AllocatedResourcesStatus: []core.ResourceStatus{
						{
							Name: "test.device/test",
							Resources: []core.ResourceHealth{
								{
									ResourceID: "resource-1",
									Health:     core.ResourceHealthStatusHealthy,
								},
							},
						},
					},
				},
			},
			wantFieldErrors: field.ErrorList{},
		},
		"allow nil": {
			containers: []core.Container{
				{
					Name: "container-2",
					Resources: core.ResourceRequirements{
						Requests: core.ResourceList{
							"test.device/test": resource.MustParse("1"),
						},
					},
				},
			},
			containerStatuses: []core.ContainerStatus{
				{
					Name: "container-1",
				},
			},
			wantFieldErrors: field.ErrorList{},
		},
		"don't allow non-unique IDs": {
			containers: []core.Container{
				{
					Name: "container-2",
					Resources: core.ResourceRequirements{
						Requests: core.ResourceList{
							"test.device/test": resource.MustParse("1"),
						},
					},
				},
			},
			containerStatuses: []core.ContainerStatus{
				{
					Name: "container-1",
					AllocatedResourcesStatus: []core.ResourceStatus{
						{
							Name: "test.device/test",
							Resources: []core.ResourceHealth{
								{
									ResourceID: "resource-1",
									Health:     core.ResourceHealthStatusHealthy,
								},
								{
									ResourceID: "resource-1",
									Health:     core.ResourceHealthStatusUnhealthy,
								},
							},
						},
					},
				},
			},
			wantFieldErrors: field.ErrorList{
				field.Duplicate(fldPath.Index(0).Child("allocatedResourcesStatus").Index(0).Child("resources").Index(1).Child("resourceID"), core.ResourceID("resource-1")),
			},
		},

		"don't allow resources that are not in spec": {
			containers: []core.Container{
				{
					Name: "container-1",
					Resources: core.ResourceRequirements{
						Requests: core.ResourceList{
							"test.device/test": resource.MustParse("1"),
						},
					},
				},
			},
			containerStatuses: []core.ContainerStatus{
				{
					Name: "container-1",
					AllocatedResourcesStatus: []core.ResourceStatus{
						{
							Name: "test.device/test",
							Resources: []core.ResourceHealth{
								{
									ResourceID: "resource-1",
									Health:     core.ResourceHealthStatusHealthy,
								},
							},
						},
						{
							Name:      "test.device/test2",
							Resources: []core.ResourceHealth{},
						},
					},
				},
			},
			wantFieldErrors: field.ErrorList{
				field.Invalid(fldPath.Index(0).Child("allocatedResourcesStatus").Index(1).Child("name"), core.ResourceName("test.device/test2"), "must match one of the container's resource requests"),
			},
		},

		"allow claims and request that are in spec": {
			containers: []core.Container{
				{
					Name: "container-1",
					Resources: core.ResourceRequirements{
						Claims: []core.ResourceClaim{
							{
								Name:    "claim.name",
								Request: "request.name",
							},
						},
					},
				},
			},
			containerStatuses: []core.ContainerStatus{
				{
					Name: "container-1",
					AllocatedResourcesStatus: []core.ResourceStatus{
						{
							Name: "claim:claim.name/request.name",
							Resources: []core.ResourceHealth{
								{
									ResourceID: "driver/pool/device-name",
									Health:     core.ResourceHealthStatusHealthy,
								},
							},
						},
					},
				},
			},
			wantFieldErrors: field.ErrorList{},
		},

		"allow claims that are in spec without the request": {
			containers: []core.Container{
				{
					Name: "container-1",
					Resources: core.ResourceRequirements{
						Claims: []core.ResourceClaim{
							{
								Name: "claim.name",
							},
						},
					},
				},
			},
			containerStatuses: []core.ContainerStatus{
				{
					Name: "container-1",
					AllocatedResourcesStatus: []core.ResourceStatus{
						{
							Name: "claim:claim.name",
							Resources: []core.ResourceHealth{
								{
									ResourceID: "driver/pool/device-name",
									Health:     core.ResourceHealthStatusHealthy,
								},
							},
						},
					},
				},
			},
			wantFieldErrors: field.ErrorList{},
		},

		"don't allow claims that are not in spec": {
			containers: []core.Container{
				{
					Name: "container-1",
					Resources: core.ResourceRequirements{
						Claims: []core.ResourceClaim{
							{
								Name: "other-claim.name",
							},
						},
						Requests: core.ResourceList{
							"claim.name": resource.MustParse("1"),
						},
					},
				},
			},
			containerStatuses: []core.ContainerStatus{
				{
					Name: "container-1",
					AllocatedResourcesStatus: []core.ResourceStatus{
						{
							Name: "claim:claim.name",
							Resources: []core.ResourceHealth{
								{
									ResourceID: "driver/pool/device-name",
									Health:     core.ResourceHealthStatusHealthy,
								},
							},
						},
					},
				},
			},
			wantFieldErrors: field.ErrorList{
				field.Invalid(fldPath.Index(0).Child("allocatedResourcesStatus").Index(0).Child("name"), core.ResourceName("claim:claim.name"), "must match one of the container's resource claims in a format 'claim:<claimName>/<request>' or 'claim:<claimName>' if request is empty"),
			},
		},

		"don't allow health status outside the known values": {
			containers: []core.Container{
				{
					Name: "container-1",
					Resources: core.ResourceRequirements{
						Requests: core.ResourceList{
							"test.device/test": resource.MustParse("1"),
						},
					},
				},
			},
			containerStatuses: []core.ContainerStatus{
				{
					Name: "container-1",
					AllocatedResourcesStatus: []core.ResourceStatus{
						{
							Name: "test.device/test",
							Resources: []core.ResourceHealth{
								{
									ResourceID: "resource-1",
									Health:     "invalid-health-value",
								},
							},
						},
					},
				},
			},
			wantFieldErrors: field.ErrorList{
				field.NotSupported(fldPath.Index(0).Child("allocatedResourcesStatus").Index(0).Child("resources").Index(0).Child("health"), core.ResourceHealthStatus("invalid-health-value"), []string{"Healthy", "Unhealthy", "Unknown"}),
			},
		},
	}
	for name, tt := range testCases {
		t.Run(name, func(t *testing.T) {
			errs := validateContainerStatusAllocatedResourcesStatus(tt.containerStatuses, fldPath, tt.containers)
			if diff := cmp.Diff(tt.wantFieldErrors, errs); diff != "" {
				t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestValidateSELinuxChangePolicy(t *testing.T) {
	tests := []struct {
		name               string
		pod                *core.Pod
		allowOnlyRecursive bool
		wantErrs           field.ErrorList
	}{
		{
			name: "nil is valid",
			pod: podtest.MakePod("pod", podtest.SetSecurityContext(&core.PodSecurityContext{
				SELinuxChangePolicy: nil,
			})),
			allowOnlyRecursive: false,
			wantErrs:           nil,
		},
		{
			name: "Recursive is always valid",
			pod: podtest.MakePod("pod", podtest.SetSecurityContext(&core.PodSecurityContext{
				SELinuxChangePolicy: ptr.To(core.SELinuxChangePolicyRecursive),
			})),
			allowOnlyRecursive: false,
			wantErrs:           nil,
		},
		{
			name: "MountOption is not valid when AllowOnlyRecursiveSELinuxChangePolicy",
			pod: podtest.MakePod("pod", podtest.SetSecurityContext(&core.PodSecurityContext{
				SELinuxChangePolicy: ptr.To(core.SELinuxChangePolicyMountOption),
			})),
			allowOnlyRecursive: true,
			wantErrs: field.ErrorList{
				field.NotSupported(
					field.NewPath("spec", "securityContext", "seLinuxChangePolicy"),
					core.PodSELinuxChangePolicy("MountOption"),
					[]string{"Recursive"}),
			},
		},
		{
			name: "MountOption is valid when not AllowOnlyRecursiveSELinuxChangePolicy",
			pod: podtest.MakePod("pod", podtest.SetSecurityContext(&core.PodSecurityContext{
				SELinuxChangePolicy: ptr.To(core.SELinuxChangePolicyMountOption),
			})),
			allowOnlyRecursive: false,
			wantErrs:           nil,
		},
		{
			name: "invalid value",
			pod: podtest.MakePod("pod", podtest.SetSecurityContext(&core.PodSecurityContext{
				SELinuxChangePolicy: ptr.To(core.PodSELinuxChangePolicy("InvalidValue")),
			})),
			allowOnlyRecursive: false,
			wantErrs: field.ErrorList{
				field.NotSupported(field.NewPath("spec", "securityContext", "seLinuxChangePolicy"),
					core.PodSELinuxChangePolicy("InvalidValue"),
					[]string{"MountOption", "Recursive"}),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := PodValidationOptions{
				AllowOnlyRecursiveSELinuxChangePolicy: tc.allowOnlyRecursive,
			}
			errs := ValidatePodSpec(&tc.pod.Spec, &tc.pod.ObjectMeta, field.NewPath("spec"), opts)
			if len(errs) == 0 {
				errs = nil
			}
			if diff := cmp.Diff(tc.wantErrs, errs); diff != "" {
				t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
			}

		})
	}
}

func TestValidatePodResize(t *testing.T) {
	mkPod := func(req, lim core.ResourceList, tweaks ...podtest.Tweak) *core.Pod {
		return podtest.MakePod("pod", append(tweaks,
			podtest.SetContainers(
				podtest.MakeContainer(
					"container",
					podtest.SetContainerResources(
						core.ResourceRequirements{
							Requests: req,
							Limits:   lim,
						},
					),
				),
			),
		)...)
	}

	mkPodWithInitContainers := func(req, lim core.ResourceList, restartPolicy core.ContainerRestartPolicy, tweaks ...podtest.Tweak) *core.Pod {
		return podtest.MakePod("pod", append(tweaks,
			podtest.SetInitContainers(
				podtest.MakeContainer(
					"container",
					podtest.SetContainerResources(
						core.ResourceRequirements{
							Requests: req,
							Limits:   lim,
						},
					),
					podtest.SetContainerRestartPolicy(restartPolicy),
				),
			),
		)...)
	}

	tests := []struct {
		test string
		old  *core.Pod
		new  *core.Pod
		err  string
	}{
		{
			test: "pod-level resources with container cpu limit change",
			new: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			old: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("200m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			err: "pods with pod-level resources cannot be resized",
		}, {
			test: "pod-level resources with container memory limit change",
			new: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			old: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Limits: getResources("100m", "200Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			err: "pods with pod-level resources cannot be resized",
		},
		{
			test: "pod-level resources with container cpu request change",
			new: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "200Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			old: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("200m", "200Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			err: "pods with pod-level resources cannot be resized",
		}, {
			test: "pod-level resources with container memory request change",
			new: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			old: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "200Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			err: "pods with pod-level resources cannot be resized",
		},
		{
			test: "pod-level resources with pod-level memory limit change",
			new: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("200m", "200Mi", "", "")}),
			),
			old: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("200m", "100Mi", "", "")}),
			),
			err: "pods with pod-level resources cannot be resized",
		},
		{
			test: "pod-level resources with pod-level memory request change",
			new: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Requests: getResources("200m", "200Mi", "", "")}),
			),
			old: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Requests: getResources("200m", "100Mi", "", "")}),
			),
			err: "pods with pod-level resources cannot be resized",
		},
		{
			test: "pod-level resources with pod-level cpu limit change",
			new: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("200m", "200Mi", "", "")}),
			),
			old: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Limits: getResources("100m", "200Mi", "", "")}),
			),
			err: "pods with pod-level resources cannot be resized",
		},
		{
			test: "pod-level resources with pod-level cpu request change",
			new: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Requests: getResources("100m", "200Mi", "", "")}),
			),
			old: podtest.MakePod("pod",
				podtest.SetContainers(podtest.MakeContainer("container",
					podtest.SetContainerResources(core.ResourceRequirements{
						Requests: getResources("100m", "100Mi", "", ""),
					}))),
				podtest.SetPodResources(&core.ResourceRequirements{Requests: getResources("200m", "200Mi", "", "")}),
			),
			err: "pods with pod-level resources cannot be resized",
		},
		{
			test: "cpu limit change",
			old:  mkPod(core.ResourceList{}, getResources("100m", "0", "1Gi", "")),
			new:  mkPod(core.ResourceList{}, getResources("200m", "0", "1Gi", "")),
			err:  "",
		}, {
			test: "memory limit change",
			old:  mkPod(core.ResourceList{}, getResources("100m", "200Mi", "", "")),
			new:  mkPod(core.ResourceList{}, getResources("100m", "100Mi", "", "")),
			err:  "",
		}, {
			test: "storage limit change",
			old:  mkPod(core.ResourceList{}, getResources("100m", "100Mi", "2Gi", "")),
			new:  mkPod(core.ResourceList{}, getResources("100m", "100Mi", "1Gi", "")),
			err:  "spec: Forbidden: only cpu and memory resources are mutable",
		}, {
			test: "cpu request change",
			old:  mkPod(getResources("200m", "0", "", ""), core.ResourceList{}),
			new:  mkPod(getResources("100m", "0", "", ""), core.ResourceList{}),
			err:  "",
		}, {
			test: "memory request change",
			old:  mkPod(getResources("0", "100Mi", "", ""), core.ResourceList{}),
			new:  mkPod(getResources("0", "200Mi", "", ""), core.ResourceList{}),
			err:  "",
		}, {
			test: "storage request change",
			old:  mkPod(getResources("100m", "0", "1Gi", ""), core.ResourceList{}),
			new:  mkPod(getResources("100m", "0", "2Gi", ""), core.ResourceList{}),
			err:  "spec: Forbidden: only cpu and memory resources are mutable",
		}, {
			test: "Pod QoS unchanged, guaranteed -> guaranteed",
			old:  mkPod(getResources("100m", "100Mi", "1Gi", ""), getResources("100m", "100Mi", "1Gi", "")),
			new:  mkPod(getResources("200m", "400Mi", "1Gi", ""), getResources("200m", "400Mi", "1Gi", "")),
			err:  "",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable",
			old:  mkPod(getResources("200m", "200Mi", "1Gi", ""), getResources("400m", "400Mi", "2Gi", "")),
			new:  mkPod(getResources("100m", "100Mi", "1Gi", ""), getResources("200m", "200Mi", "2Gi", "")),
			err:  "",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, add limits",
			old:  mkPod(getResources("100m", "100Mi", "", ""), core.ResourceList{}),
			new:  mkPod(getResources("100m", "100Mi", "", ""), getResources("200m", "200Mi", "", "")),
			err:  "",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, add requests",
			old:  mkPod(core.ResourceList{}, getResources("200m", "500Mi", "1Gi", "")),
			new:  mkPod(getResources("300m", "", "", ""), getResources("400m", "500Mi", "1Gi", "")),
			err:  "",
		}, {
			test: "Pod QoS change, guaranteed -> burstable",
			old:  mkPod(getResources("100m", "100Mi", "", ""), getResources("100m", "100Mi", "", "")),
			new:  mkPod(getResources("100m", "100Mi", "", ""), getResources("200m", "200Mi", "", "")),
			err:  "Pod QOS Class may not change as a result of resizing",
		}, {
			test: "Pod QoS change, burstable -> guaranteed",
			old:  mkPod(getResources("100m", "100Mi", "", ""), core.ResourceList{}),
			new:  mkPod(getResources("100m", "100Mi", "", ""), getResources("100m", "100Mi", "", "")),
			err:  "Pod QOS Class may not change as a result of resizing",
		}, {
			test: "Pod QoS change, besteffort -> burstable",
			old:  mkPod(core.ResourceList{}, core.ResourceList{}),
			new:  mkPod(getResources("100m", "100Mi", "", ""), getResources("200m", "200Mi", "", "")),
			err:  "Pod QOS Class may not change as a result of resizing",
		}, {
			test: "Pod QoS change, burstable -> besteffort",
			old:  mkPod(getResources("100m", "100Mi", "", ""), getResources("200m", "200Mi", "", "")),
			new:  mkPod(core.ResourceList{}, core.ResourceList{}),
			err:  "Pod QOS Class may not change as a result of resizing",
		}, {
			test: "windows pod, no resource change",
			old:  mkPod(core.ResourceList{}, getResources("100m", "0", "1Gi", ""), podtest.SetOS(core.Windows)),
			new:  mkPod(core.ResourceList{}, getResources("100m", "0", "1Gi", ""), podtest.SetOS(core.Windows)),
			err:  "Forbidden: windows pods cannot be resized",
		}, {
			test: "windows pod, resource change",
			old:  mkPod(core.ResourceList{}, getResources("100m", "0", "1Gi", ""), podtest.SetOS(core.Windows)),
			new:  mkPod(core.ResourceList{}, getResources("200m", "0", "1Gi", ""), podtest.SetOS(core.Windows)),
			err:  "Forbidden: windows pods cannot be resized",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove cpu limit",
			old:  mkPod(core.ResourceList{}, getResources("100m", "100Mi", "", "")),
			new:  mkPod(core.ResourceList{}, getResources("", "100Mi", "", "")),
			err:  "spec.containers[0].resources.limits: Forbidden: resource limits cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove memory limit",
			old:  mkPod(core.ResourceList{}, getResources("100m", "100Mi", "", "")),
			new:  mkPod(core.ResourceList{}, getResources("100m", "", "", "")),
			err:  "spec.containers[0].resources.limits: Forbidden: resource limits cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove cpu request",
			old:  mkPod(getResources("100m", "100Mi", "", ""), core.ResourceList{}),
			new:  mkPod(getResources("", "100Mi", "", ""), core.ResourceList{}),
			err:  "spec.containers[0].resources.requests: Forbidden: resource requests cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove memory request",
			old:  mkPod(getResources("100m", "100Mi", "", ""), core.ResourceList{}),
			new:  mkPod(getResources("100m", "", "", ""), core.ResourceList{}),
			err:  "spec.containers[0].resources.requests: Forbidden: resource requests cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove cpu and memory limits",
			old:  mkPod(getResources("100m", "", "", ""), getResources("100m", "100Mi", "", "")),
			new:  mkPod(getResources("100m", "", "", ""), core.ResourceList{}),
			err:  "spec.containers[0].resources.limits: Forbidden: resource limits cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove cpu and memory requests",
			old:  mkPod(getResources("100m", "100Mi", "", ""), getResources("100m", "", "", "")),
			new:  mkPod(core.ResourceList{}, getResources("100m", "", "", "")),
			err:  "spec.containers[0].resources.requests: Forbidden: resource requests cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove cpu limit",
			old:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "100Mi", "", ""), core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(core.ResourceList{}, getResources("", "100Mi", "", ""), core.ContainerRestartPolicyAlways),
			err:  "spec.initContainers[0].resources.limits: Forbidden: resource limits cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove memory limit",
			old:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "100Mi", "", ""), core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "", "", ""), core.ContainerRestartPolicyAlways),
			err:  "spec.initContainers[0].resources.limits: Forbidden: resource limits cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove cpu request",
			old:  mkPodWithInitContainers(getResources("100m", "100Mi", "", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(getResources("", "100Mi", "", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			err:  "spec.initContainers[0].resources.requests: Forbidden: resource requests cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove memory request",
			old:  mkPodWithInitContainers(getResources("100m", "100Mi", "", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(getResources("100m", "", "", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			err:  "spec.initContainers[0].resources.requests: Forbidden: resource requests cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove cpu and memory limits",
			old:  mkPodWithInitContainers(getResources("100m", "", "", ""), getResources("100m", "100Mi", "", ""), core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(getResources("100m", "", "", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			err:  "spec.initContainers[0].resources.limits: Forbidden: resource limits cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove cpu and memory requests",
			old:  mkPodWithInitContainers(getResources("100m", "100Mi", "", ""), getResources("100m", "", "", ""), core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "", "", ""), core.ContainerRestartPolicyAlways),
			err:  "spec.initContainers[0].resources.requests: Forbidden: resource requests cannot be removed",
		}, {
			test: "Pod QoS unchanged, burstable -> burstable, remove cpu and memory requests",
			old:  mkPodWithInitContainers(getResources("100m", "100Mi", "", ""), getResources("100m", "", "", ""), ""),
			new:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "", "", ""), ""),
			err:  "spec: Forbidden: resources for non-sidecar init containers are immutable",
		}, {
			test: "Pod with nil Resource field in Status",
			old: mkPod(core.ResourceList{}, getResources("100m", "0", "1Gi", ""), podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					Name:        "main",
					Ready:       true,
					Started:     proto.Bool(true),
					Resources:   nil,
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			})),
			new: mkPod(core.ResourceList{}, getResources("200m", "0", "1Gi", "")),
			err: "Forbidden: Pod running on node without support for resize",
		},
		{
			test: "Pod with non-nil Resources field in Status",
			old: mkPod(core.ResourceList{}, getResources("100m", "0", "1Gi", ""), podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					Name:        "main",
					Ready:       true,
					Started:     proto.Bool(true),
					Resources:   &core.ResourceRequirements{},
					State: core.ContainerState{
						Running: &core.ContainerStateRunning{
							StartedAt: metav1.NewTime(time.Now()),
						},
					},
				}},
			})),
			new: mkPod(core.ResourceList{}, getResources("200m", "0", "1Gi", "")),
			err: "",
		},
		{
			test: "Pod without running containers",
			old: mkPod(core.ResourceList{}, getResources("100m", "0", "1Gi", ""), podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{},
			})),
			new: mkPod(core.ResourceList{}, getResources("200m", "0", "1Gi", "")),
			err: "",
		},
		{
			test: "Pod with containers which are not running yet",
			old: mkPod(core.ResourceList{}, getResources("100m", "0", "1Gi", ""), podtest.SetStatus(core.PodStatus{
				ContainerStatuses: []core.ContainerStatus{{
					ContainerID: "docker://numbers",
					Image:       "nginx:alpine",
					Name:        "main",
					Ready:       true,
					Started:     proto.Bool(true),
					Resources:   &core.ResourceRequirements{},
					State: core.ContainerState{
						Waiting: &core.ContainerStateWaiting{
							Reason: "PodInitializing",
						},
					},
				}},
			})),
			new: mkPod(core.ResourceList{}, getResources("200m", "0", "1Gi", "")),
			err: "",
		}, {
			test: "cpu limit change for sidecar containers",
			old:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "0", "1Gi", ""), core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(core.ResourceList{}, getResources("200m", "0", "1Gi", ""), core.ContainerRestartPolicyAlways),
			err:  "",
		}, {
			test: "memory limit change for sidecar containers",
			old:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "200Mi", "", ""), core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "100Mi", "", ""), core.ContainerRestartPolicyAlways),
			err:  "",
		}, {
			test: "storage limit change for sidecar containers",
			old:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "100Mi", "2Gi", ""), core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(core.ResourceList{}, getResources("100m", "100Mi", "1Gi", ""), core.ContainerRestartPolicyAlways),
			err:  "spec: Forbidden: only cpu and memory resources for sidecar containers are mutable",
		}, {
			test: "cpu request change for sidecar containers",
			old:  mkPodWithInitContainers(getResources("200m", "0", "", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(getResources("100m", "0", "", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			err:  "",
		}, {
			test: "memory request change for sidecar containers",
			old:  mkPodWithInitContainers(getResources("0", "100Mi", "", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(getResources("0", "200Mi", "", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			err:  "",
		}, {
			test: "storage request change for sidecar containers",
			old:  mkPodWithInitContainers(getResources("100m", "0", "1Gi", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			new:  mkPodWithInitContainers(getResources("100m", "0", "2Gi", ""), core.ResourceList{}, core.ContainerRestartPolicyAlways),
			err:  "spec: Forbidden: only cpu and memory resources for sidecar containers are mutable",
		},
	}

	for _, test := range tests {
		test.new.ObjectMeta.ResourceVersion = "1"
		test.old.ObjectMeta.ResourceVersion = "1"

		// set required fields if old and new match and have no opinion on the value
		if test.new.Name == "" && test.old.Name == "" {
			test.new.Name = "name"
			test.old.Name = "name"
		}
		if test.new.Namespace == "" && test.old.Namespace == "" {
			test.new.Namespace = "namespace"
			test.old.Namespace = "namespace"
		}
		if test.new.Spec.Containers == nil && test.old.Spec.Containers == nil {
			test.new.Spec.Containers = []core.Container{{Name: "autoadded", Image: "image", TerminationMessagePolicy: "File", ImagePullPolicy: "Always"}}
			test.old.Spec.Containers = []core.Container{{Name: "autoadded", Image: "image", TerminationMessagePolicy: "File", ImagePullPolicy: "Always"}}
		}
		if len(test.new.Spec.DNSPolicy) == 0 && len(test.old.Spec.DNSPolicy) == 0 {
			test.new.Spec.DNSPolicy = core.DNSClusterFirst
			test.old.Spec.DNSPolicy = core.DNSClusterFirst
		}
		if len(test.new.Spec.RestartPolicy) == 0 && len(test.old.Spec.RestartPolicy) == 0 {
			test.new.Spec.RestartPolicy = "Always"
			test.old.Spec.RestartPolicy = "Always"
		}

		errs := ValidatePodResize(test.new, test.old, PodValidationOptions{})
		if test.err == "" {
			if len(errs) != 0 {
				t.Errorf("unexpected invalid: %s (%+v)\nA: %+v\nB: %+v", test.test, errs, test.new, test.old)
			}
		} else {
			if len(errs) == 0 {
				t.Errorf("unexpected valid: %s\nA: %+v\nB: %+v", test.test, test.new, test.old)
			} else if actualErr := errs.ToAggregate().Error(); !strings.Contains(actualErr, test.err) {
				t.Errorf("unexpected error message: %s\nExpected error: %s\nActual error: %s", test.test, test.err, actualErr)
			}
		}
	}
}
