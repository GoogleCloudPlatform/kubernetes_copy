/*
Copyright 2021 The Kubernetes Authors.

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

package ensurer

import (
	"context"
	"reflect"
	"testing"

	flowcontrolv1 "k8s.io/api/flowcontrol/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/apis/flowcontrol/bootstrap"
	"k8s.io/client-go/kubernetes/fake"
	flowcontrollisters "k8s.io/client-go/listers/flowcontrol/v1"
	toolscache "k8s.io/client-go/tools/cache"
	flowcontrolapisv1 "k8s.io/kubernetes/pkg/apis/flowcontrol/v1"
	"k8s.io/utils/ptr"

	"github.com/google/go-cmp/cmp"
)

func TestEnsurePriorityLevel(t *testing.T) {
	validExemptPL := func() *flowcontrolv1.PriorityLevelConfiguration {
		copy := bootstrap.MandatoryPriorityLevelConfigurationExempt.DeepCopy()
		copy.Annotations[flowcontrolv1.AutoUpdateAnnotationKey] = "true"
		copy.Spec.Exempt.NominalConcurrencyShares = ptr.To[int32](10)
		copy.Spec.Exempt.LendablePercent = ptr.To[int32](50)
		return copy
	}()

	tests := []struct {
		name      string
		strategy  func() EnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration]
		current   *flowcontrolv1.PriorityLevelConfiguration
		bootstrap *flowcontrolv1.PriorityLevelConfiguration
		expected  *flowcontrolv1.PriorityLevelConfiguration
	}{
		// for suggested configurations
		{
			name:      "suggested priority level configuration does not exist - the object should always be re-created",
			strategy:  NewSuggestedEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: newPLConfiguration("pl1").WithLimited(10).Object(),
			current:   nil,
			expected:  newPLConfiguration("pl1").WithLimited(10).Object(),
		},
		{
			name:      "suggested priority level configuration exists, auto update is enabled, NominalConcurrencyShares does not match - current object should be updated",
			strategy:  NewSuggestedEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: newPLConfiguration("pl1").WithLimited(20).Object(),
			current:   newPLConfiguration("pl1").WithAutoUpdateAnnotation("true").WithLimited(10).Object(),
			expected:  newPLConfiguration("pl1").WithAutoUpdateAnnotation("true").WithLimited(20).Object(),
		},
		{
			name:      "suggested priority level configuration exists, auto update is enabled, Weight does not match - current object should be updated",
			strategy:  NewSuggestedEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: newPLConfiguration("pl1").WithLimited(20).Object(),
			current:   newPLConfiguration("pl1").WithAutoUpdateAnnotation("true").WithLimited(20).WithWeight(13).Object(),
			expected:  newPLConfiguration("pl1").WithAutoUpdateAnnotation("true").WithLimited(20).Object(),
		},
		{
			name:      "suggested priority level configuration exists, auto update is disabled, NominalConcurrencyShares does not match - current object should not be updated",
			strategy:  NewSuggestedEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: newPLConfiguration("pl1").WithLimited(20).Object(),
			current:   newPLConfiguration("pl1").WithAutoUpdateAnnotation("false").WithLimited(10).Object(),
			expected:  newPLConfiguration("pl1").WithAutoUpdateAnnotation("false").WithLimited(10).Object(),
		},
		{
			name:      "suggested priority level configuration exists, auto update is disabled, Weight does not match - current object should not be updated",
			strategy:  NewSuggestedEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: newPLConfiguration("pl1").WithLimited(20).WithWeight(13).Object(),
			current:   newPLConfiguration("pl1").WithAutoUpdateAnnotation("false").WithLimited(20).Object(),
			expected:  newPLConfiguration("pl1").WithAutoUpdateAnnotation("false").WithLimited(20).Object(),
		},

		// for mandatory configurations
		{
			name:      "mandatory priority level configuration does not exist - new object should be created",
			strategy:  NewMandatoryEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: newPLConfiguration("pl1").WithLimited(10).WithAutoUpdateAnnotation("true").Object(),
			current:   nil,
			expected:  newPLConfiguration("pl1").WithLimited(10).WithAutoUpdateAnnotation("true").Object(),
		},
		{
			name:      "mandatory priority level configuration exists, annotation is missing - annotation is added",
			strategy:  NewMandatoryEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: newPLConfiguration("pl1").WithLimited(20).Object(),
			current:   newPLConfiguration("pl1").WithLimited(20).Object(),
			expected:  newPLConfiguration("pl1").WithAutoUpdateAnnotation("true").WithLimited(20).Object(),
		},
		{
			name:      "mandatory priority level configuration exists, auto update is disabled, NominalConcurrencyShares does not match - current object should be updated",
			strategy:  NewMandatoryEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: newPLConfiguration("pl1").WithLimited(20).Object(),
			current:   newPLConfiguration("pl1").WithAutoUpdateAnnotation("false").WithLimited(10).Object(),
			expected:  newPLConfiguration("pl1").WithAutoUpdateAnnotation("true").WithLimited(20).Object(),
		},
		{
			name:      "mandatory priority level configuration exists, auto update is disabled, Weight does not match - current object should be updated",
			strategy:  NewMandatoryEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: newPLConfiguration("pl1").WithLimited(20).Object(),
			current:   newPLConfiguration("pl1").WithAutoUpdateAnnotation("false").WithLimited(20).WithWeight(13).Object(),
			expected:  newPLConfiguration("pl1").WithAutoUpdateAnnotation("true").WithLimited(20).Object(),
		},
		{
			name:     "admin changes the Exempt field of the exempt priority level configuration",
			strategy: NewMandatoryEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration],
			bootstrap: func() *flowcontrolv1.PriorityLevelConfiguration {
				return bootstrap.MandatoryPriorityLevelConfigurationExempt
			}(),
			current:  validExemptPL,
			expected: validExemptPL,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset().FlowcontrolV1().PriorityLevelConfigurations()
			indexer := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{})
			if test.current != nil {
				client.Create(context.TODO(), test.current, metav1.CreateOptions{})
				indexer.Add(test.current)
			}

			ops := NewPriorityLevelConfigurationOps(client, flowcontrollisters.NewPriorityLevelConfigurationLister(indexer))
			boots := []*flowcontrolv1.PriorityLevelConfiguration{test.bootstrap}
			strategy := test.strategy()

			err := EnsureConfigurations(context.Background(), ops, boots, strategy)
			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}

			plGot, err := client.Get(context.TODO(), test.bootstrap.Name, metav1.GetOptions{})
			switch {
			case test.expected == nil:
				if !apierrors.IsNotFound(err) {
					t.Fatalf("Expected GET to return an %q error, but got: %v", metav1.StatusReasonNotFound, err)
				}
			case err != nil:
				t.Fatalf("Expected GET to return no error, but got: %v", err)
			}

			if !reflect.DeepEqual(test.expected, plGot) {
				t.Errorf("PriorityLevelConfiguration does not match - diff: %s", cmp.Diff(test.expected, plGot))
			}
		})
	}
}

func TestSuggestedPLEnsureStrategy_ShouldUpdate(t *testing.T) {
	tests := []struct {
		name              string
		current           *flowcontrolv1.PriorityLevelConfiguration
		bootstrap         *flowcontrolv1.PriorityLevelConfiguration
		newObjectExpected *flowcontrolv1.PriorityLevelConfiguration
	}{
		{
			name:              "auto update is enabled, first generation, spec does not match - spec update expected",
			current:           newPLConfiguration("foo").WithAutoUpdateAnnotation("true").WithGeneration(1).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(10).Object(),
			newObjectExpected: newPLConfiguration("foo").WithAutoUpdateAnnotation("true").WithGeneration(1).WithLimited(10).Object(),
		},
		{
			name:              "auto update is enabled, first generation, spec matches - no update expected",
			current:           newPLConfiguration("foo").WithAutoUpdateAnnotation("true").WithGeneration(1).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithGeneration(1).WithLimited(5).Object(),
			newObjectExpected: nil,
		},
		{
			name:              "auto update is enabled, second generation, spec does not match - spec update expected",
			current:           newPLConfiguration("foo").WithAutoUpdateAnnotation("true").WithGeneration(2).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(10).Object(),
			newObjectExpected: newPLConfiguration("foo").WithAutoUpdateAnnotation("true").WithGeneration(2).WithLimited(10).Object(),
		},
		{
			name:              "auto update is enabled, second generation, spec matches - no update expected",
			current:           newPLConfiguration("foo").WithAutoUpdateAnnotation("true").WithGeneration(2).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(5).Object(),
			newObjectExpected: nil,
		},
		{
			name:              "auto update is disabled, first generation, spec does not match - no update expected",
			current:           newPLConfiguration("foo").WithAutoUpdateAnnotation("false").WithGeneration(1).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(10).Object(),
			newObjectExpected: nil,
		},
		{
			name:              "auto update is disabled, first generation, spec matches - no update expected",
			current:           newPLConfiguration("foo").WithAutoUpdateAnnotation("false").WithGeneration(1).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(5).Object(),
			newObjectExpected: nil,
		},
		{
			name:              "auto update is disabled, second generation, spec does not match - no update expected",
			current:           newPLConfiguration("foo").WithAutoUpdateAnnotation("false").WithGeneration(2).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(10).Object(),
			newObjectExpected: nil,
		},
		{
			name:              "auto update is disabled, second generation, spec matches - no update expected",
			current:           newPLConfiguration("foo").WithAutoUpdateAnnotation("false").WithGeneration(2).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(5).Object(),
			newObjectExpected: nil,
		},
		{
			name:              "annotation is missing, first generation, spec does not match - both annotation and spec update expected",
			current:           newPLConfiguration("foo").WithGeneration(1).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(10).Object(),
			newObjectExpected: newPLConfiguration("foo").WithAutoUpdateAnnotation("true").WithGeneration(1).WithLimited(10).Object(),
		},
		{
			name:              "annotation is missing, first generation, spec matches - annotation update is expected",
			current:           newPLConfiguration("foo").WithGeneration(1).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(5).Object(),
			newObjectExpected: newPLConfiguration("foo").WithAutoUpdateAnnotation("true").WithGeneration(1).WithLimited(5).Object(),
		},
		{
			name:              "annotation is missing, second generation, spec does not match - annotation update is expected",
			current:           newPLConfiguration("foo").WithGeneration(2).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(10).Object(),
			newObjectExpected: newPLConfiguration("foo").WithAutoUpdateAnnotation("false").WithGeneration(2).WithLimited(5).Object(),
		},
		{
			name:              "annotation is missing, second generation, spec matches - annotation update is expected",
			current:           newPLConfiguration("foo").WithGeneration(2).WithLimited(5).Object(),
			bootstrap:         newPLConfiguration("foo").WithLimited(5).Object(),
			newObjectExpected: newPLConfiguration("foo").WithAutoUpdateAnnotation("false").WithGeneration(2).WithLimited(5).Object(),
		},
	}

	ops := NewPriorityLevelConfigurationOps(nil, nil)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			strategy := NewSuggestedEnsureStrategy[*flowcontrolv1.PriorityLevelConfiguration]()
			updatableGot, updateGot, err := strategy.ReviseIfNeeded(ops, test.current, test.bootstrap)
			if err != nil {
				t.Errorf("Expected no error, but got: %v", err)
			}
			if test.newObjectExpected == nil {
				if updatableGot != nil {
					t.Errorf("Expected a nil object, but got: %#v", updatableGot)
				}
				if updateGot {
					t.Errorf("Expected update=%t but got: %t", false, updateGot)
				}
				return
			}

			if !updateGot {
				t.Errorf("Expected update=%t but got: %t", true, updateGot)
			}
			if !reflect.DeepEqual(test.newObjectExpected, updatableGot) {
				t.Errorf("Expected the object to be updated to match - diff: %s", cmp.Diff(test.newObjectExpected, updatableGot))
			}
		})
	}
}

func TestPriorityLevelSpecChanged(t *testing.T) {
	pl1 := &flowcontrolv1.PriorityLevelConfiguration{
		Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
			Type: flowcontrolv1.PriorityLevelEnablementLimited,
			Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
				LimitResponse: flowcontrolv1.LimitResponse{
					Type: flowcontrolv1.LimitResponseTypeReject,
				},
			},
		},
	}
	pl2 := &flowcontrolv1.PriorityLevelConfiguration{
		Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
			Type: flowcontrolv1.PriorityLevelEnablementLimited,
			Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(1)),
			},
		},
	}
	pl1Defaulted := &flowcontrolv1.PriorityLevelConfiguration{
		Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
			Type: flowcontrolv1.PriorityLevelEnablementLimited,
			Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(flowcontrolapisv1.PriorityLevelConfigurationDefaultNominalConcurrencyShares),
				LendablePercent:          ptr.To[int32](0),
				LimitResponse: flowcontrolv1.LimitResponse{
					Type: flowcontrolv1.LimitResponseTypeReject,
				},
				Weight: ptr.To(flowcontrolapisv1.LimitedPriorityLevelConfigurationDefaultWeight),
			},
		},
	}
	ple1 := &flowcontrolv1.PriorityLevelConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "exempt"},
		Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
			Type: flowcontrolv1.PriorityLevelEnablementExempt,
			Exempt: &flowcontrolv1.ExemptPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To[int32](42),
				LendablePercent:          ptr.To[int32](33),
			},
		},
	}
	ple2 := &flowcontrolv1.PriorityLevelConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "exempt"},
		Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
			Type: flowcontrolv1.PriorityLevelEnablementExempt,
			Exempt: &flowcontrolv1.ExemptPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To[int32](24),
				LendablePercent:          ptr.To[int32](86),
			},
		},
	}
	pleWrong := &flowcontrolv1.PriorityLevelConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "exempt"},
		Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
			Type: flowcontrolv1.PriorityLevelEnablementLimited,
			Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(1)),
			},
		},
	}
	pleInvalid := &flowcontrolv1.PriorityLevelConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "exempt"},
		Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
			Type: "widget",
		},
	}
	testCases := []struct {
		name        string
		expected    *flowcontrolv1.PriorityLevelConfiguration
		actual      *flowcontrolv1.PriorityLevelConfiguration
		specChanged bool
	}{
		{
			name:        "identical priority-level should work",
			expected:    bootstrap.MandatoryPriorityLevelConfigurationCatchAll,
			actual:      bootstrap.MandatoryPriorityLevelConfigurationCatchAll,
			specChanged: false,
		},
		{
			name:        "defaulted priority-level should work",
			expected:    pl1,
			actual:      pl1Defaulted,
			specChanged: false,
		},
		{
			name:        "non-defaulted priority-level has wrong spec",
			expected:    pl1,
			actual:      pl2,
			specChanged: true,
		},
		{
			name:        "tweaked exempt config",
			expected:    ple1,
			actual:      ple2,
			specChanged: false,
		},
		{
			name:        "exempt with wrong tag",
			expected:    ple1,
			actual:      pleWrong,
			specChanged: true,
		},
		{
			name:        "exempt with invalid tag",
			expected:    ple1,
			actual:      pleInvalid,
			specChanged: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			w := !plcSpecEqualish(testCase.expected, testCase.actual)
			if testCase.specChanged != w {
				t.Errorf("Expected priorityLevelSpecChanged to return %t, but got: %t - diff: %s", testCase.specChanged, w,
					cmp.Diff(testCase.expected, testCase.actual))
			}
		})
	}
}

func TestRemovePriorityLevelConfiguration(t *testing.T) {
	tests := []struct {
		name           string
		current        *flowcontrolv1.PriorityLevelConfiguration
		bootstrapName  string
		removeExpected bool
	}{
		{
			name:          "no priority level configuration objects exist",
			bootstrapName: "pl1",
			current:       nil,
		},
		{
			name:           "priority level configuration not wanted, auto update is enabled",
			bootstrapName:  "pl0",
			current:        newPLConfiguration("pl1").WithAutoUpdateAnnotation("true").Object(),
			removeExpected: true,
		},
		{
			name:           "priority level configuration not wanted, auto update is disabled",
			bootstrapName:  "pl0",
			current:        newPLConfiguration("pl1").WithAutoUpdateAnnotation("false").Object(),
			removeExpected: false,
		},
		{
			name:           "priority level configuration not wanted, the auto-update annotation is malformed",
			bootstrapName:  "pl0",
			current:        newPLConfiguration("pl1").WithAutoUpdateAnnotation("invalid").Object(),
			removeExpected: false,
		},
		{
			name:           "priority level configuration wanted, auto update is enabled",
			bootstrapName:  "pl1",
			current:        newPLConfiguration("pl1").WithAutoUpdateAnnotation("true").Object(),
			removeExpected: false,
		},
		{
			name:           "priority level configuration wanted, auto update is disabled",
			bootstrapName:  "pl1",
			current:        newPLConfiguration("pl1").WithAutoUpdateAnnotation("false").Object(),
			removeExpected: false,
		},
		{
			name:           "priority level configuration wanted, the auto-update annotation is malformed",
			bootstrapName:  "pl1",
			current:        newPLConfiguration("pl1").WithAutoUpdateAnnotation("invalid").Object(),
			removeExpected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset().FlowcontrolV1().PriorityLevelConfigurations()
			indexer := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{})
			if test.current != nil {
				client.Create(context.TODO(), test.current, metav1.CreateOptions{})
				indexer.Add(test.current)
			}

			boot := newPLConfiguration(test.bootstrapName).Object()
			boots := []*flowcontrolv1.PriorityLevelConfiguration{boot}
			ops := NewPriorityLevelConfigurationOps(client, flowcontrollisters.NewPriorityLevelConfigurationLister(indexer))
			err := RemoveUnwantedObjects(context.Background(), ops, boots)
			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}

			if test.current == nil {
				return
			}
			_, err = client.Get(context.TODO(), test.current.Name, metav1.GetOptions{})
			switch {
			case test.removeExpected:
				if !apierrors.IsNotFound(err) {
					t.Errorf("Expected error: %q, but got: %v", metav1.StatusReasonNotFound, err)
				}
			default:
				if err != nil {
					t.Errorf("Expected no error, but got: %v", err)
				}
			}
		})
	}
}

type plBuilder struct {
	object *flowcontrolv1.PriorityLevelConfiguration
}

func newPLConfiguration(name string) *plBuilder {
	return &plBuilder{
		object: &flowcontrolv1.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		},
	}
}

func (b *plBuilder) Object() *flowcontrolv1.PriorityLevelConfiguration {
	return b.object
}

func (b *plBuilder) WithGeneration(value int64) *plBuilder {
	b.object.SetGeneration(value)
	return b
}

func (b *plBuilder) WithAutoUpdateAnnotation(value string) *plBuilder {
	setAnnotation(b.object, value)
	return b
}

func (b *plBuilder) WithLimited(nominalConcurrencyShares int32) *plBuilder {
	b.object.Spec.Type = flowcontrolv1.PriorityLevelEnablementLimited
	b.object.Spec.Limited = &flowcontrolv1.LimitedPriorityLevelConfiguration{
		NominalConcurrencyShares: ptr.To(nominalConcurrencyShares),
		LendablePercent:          ptr.To[int32](0),
		LimitResponse: flowcontrolv1.LimitResponse{
			Type: flowcontrolv1.LimitResponseTypeReject,
		},
		Weight: ptr.To(flowcontrolapisv1.LimitedPriorityLevelConfigurationDefaultWeight),
	}
	return b
}

func (b *plBuilder) WithWeight(weight int32) *plBuilder {
	b.object.Spec.Limited.Weight = ptr.To(weight)
	return b
}

// must be called after WithLimited
func (b *plBuilder) WithQueuing(queues, handSize, queueLengthLimit int32) *plBuilder {
	limited := b.object.Spec.Limited
	if limited == nil {
		return b
	}

	limited.LimitResponse.Type = flowcontrolv1.LimitResponseTypeQueue
	limited.LimitResponse.Queuing = &flowcontrolv1.QueuingConfiguration{
		Queues:           queues,
		HandSize:         handSize,
		QueueLengthLimit: queueLengthLimit,
	}

	return b
}
