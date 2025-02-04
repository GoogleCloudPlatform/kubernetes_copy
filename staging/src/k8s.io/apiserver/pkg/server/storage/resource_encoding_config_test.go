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

package storage

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	runtimetesting "k8s.io/apimachinery/pkg/runtime/testing"
	"k8s.io/apimachinery/pkg/test"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	basecompatibility "k8s.io/component-base/compatibility"
)

func TestEmulatedStorageVersion(t *testing.T) {
	cases := []struct {
		name             string
		scheme           *runtime.Scheme
		binaryVersion    schema.GroupVersion
		effectiveVersion basecompatibility.EffectiveVersion
		want             schema.GroupVersion
	}{
		{
			name:             "pick compatible",
			scheme:           AlphaBetaScheme(utilversion.MustParse("1.31"), utilversion.MustParse("1.32")),
			binaryVersion:    v1beta1,
			effectiveVersion: basecompatibility.NewEffectiveVersionFromString("1.32", "", ""),
			want:             v1alpha1,
		},
		{
			name:             "alpha has been replaced, pick binary version",
			scheme:           AlphaReplacedBetaScheme(utilversion.MustParse("1.31"), utilversion.MustParse("1.32")),
			binaryVersion:    v1beta1,
			effectiveVersion: basecompatibility.NewEffectiveVersionFromString("1.32", "", ""),
			want:             v1beta1,
		},
	}

	for _, tc := range cases {
		test.TestScheme()
		t.Run(tc.name, func(t *testing.T) {
			found, err := emulatedStorageVersion(tc.binaryVersion, &CronJob{}, tc.effectiveVersion, tc.scheme)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if found != tc.want {
				t.Errorf("got %v; want %v", found, tc.want)
			}
		})
	}
}

var internalGV = schema.GroupVersion{Group: "workload.example.com", Version: runtime.APIVersionInternal}
var v1alpha1 = schema.GroupVersion{Group: "workload.example.com", Version: "v1alpha1"}
var v1beta1 = schema.GroupVersion{Group: "workload.example.com", Version: "v1beta1"}

type CronJobWithReplacement struct {
	introduced *utilversion.Version
	A          string `json:"A,omitempty"`
	B          int    `json:"B,omitempty"`
}

func (*CronJobWithReplacement) GetObjectKind() schema.ObjectKind { panic("not implemented") }
func (*CronJobWithReplacement) DeepCopyObject() runtime.Object {
	panic("not implemented")
}

func (in *CronJobWithReplacement) APILifecycleIntroduced() (major, minor int) {
	if in.introduced == nil {
		return 0, 0
	}
	return int(in.introduced.Major()), int(in.introduced.Minor())
}

func (in *CronJobWithReplacement) APILifecycleReplacement() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
}

type CronJob struct {
	introduced *utilversion.Version
	A          string `json:"A,omitempty"`
	B          int    `json:"B,omitempty"`
}

func (*CronJob) GetObjectKind() schema.ObjectKind { panic("not implemented") }
func (*CronJob) DeepCopyObject() runtime.Object {
	panic("not implemented")
}

func (in *CronJob) APILifecycleIntroduced() (major, minor int) {
	if in.introduced == nil {
		return 0, 0
	}
	return int(in.introduced.Major()), int(in.introduced.Minor())
}

func AlphaBetaScheme(alphaVersion, betaVersion *utilversion.Version) *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypes(internalGV, &CronJob{})
	s.AddKnownTypes(v1alpha1, &CronJob{introduced: alphaVersion})
	s.AddKnownTypes(v1beta1, &CronJob{introduced: betaVersion})
	s.AddKnownTypeWithName(internalGV.WithKind("CronJob"), &CronJob{})
	s.AddKnownTypeWithName(v1alpha1.WithKind("CronJob"), &CronJob{introduced: alphaVersion})
	s.AddKnownTypeWithName(v1beta1.WithKind("CronJob"), &CronJob{introduced: betaVersion})
	utilruntime.Must(runtimetesting.RegisterConversions(s))
	return s
}

func AlphaReplacedBetaScheme(alphaVersion, betaVersion *utilversion.Version) *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypes(internalGV, &CronJob{})
	s.AddKnownTypes(v1alpha1, &CronJobWithReplacement{introduced: alphaVersion})
	s.AddKnownTypes(v1beta1, &CronJob{introduced: betaVersion})
	s.AddKnownTypeWithName(internalGV.WithKind("CronJob"), &CronJob{})
	s.AddKnownTypeWithName(v1alpha1.WithKind("CronJob"), &CronJobWithReplacement{introduced: alphaVersion})
	s.AddKnownTypeWithName(v1beta1.WithKind("CronJob"), &CronJob{introduced: betaVersion})
	utilruntime.Must(runtimetesting.RegisterConversions(s))
	return s
}
