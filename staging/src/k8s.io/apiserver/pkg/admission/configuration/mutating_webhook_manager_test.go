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

package configuration

import (
	"context"
	"reflect"
	"testing"
	"time"

	"k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission/plugin/webhook"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGetMutatingWebhookConfig(t *testing.T) {
	// Build a test client that the admission plugin can use to look up the MutatingWebhookConfiguration
	client := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	stop := make(chan struct{})
	defer close(stop)

	manager := NewMutatingWebhookConfigurationManager(informerFactory).(*mutatingWebhookConfigurationManager)
	informerFactory.Start(stop)
	informerFactory.WaitForCacheSync(stop)

	// no configurations
	if configurations := manager.Webhooks(); len(configurations) != 0 {
		t.Errorf("expected empty webhooks, but got %v", configurations)
	}

	webhookConfiguration := &v1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "webhook1"},
		Webhooks:   []v1.MutatingWebhook{{Name: "webhook1.1"}},
	}

	client.
		AdmissionregistrationV1().
		MutatingWebhookConfigurations().
		Create(context.TODO(), webhookConfiguration, metav1.CreateOptions{})

	// Wait up to 10s for the notification to be delivered.
	// (on my system this takes < 2ms)
	startTime := time.Now()
	configurations := manager.Webhooks()
	for len(configurations) == 0 {
		if time.Since(startTime) > 10*time.Second {
			break
		}
		time.Sleep(time.Millisecond)
		configurations = manager.Webhooks()
	}

	// verify presence
	if len(configurations) == 0 {
		t.Errorf("expected non empty webhooks")
	}
	for i := range configurations {
		h, ok := configurations[i].GetMutatingWebhook()
		if !ok {
			t.Errorf("Expected mutating webhook")
			continue
		}
		if !reflect.DeepEqual(h, &webhookConfiguration.Webhooks[i]) {
			t.Errorf("Expected\n%#v\ngot\n%#v", &webhookConfiguration.Webhooks[i], h)
		}
	}
}

// mockCreateMutatingWebhookAccessor is a struct used to compute how many times
// the function webhook.NewMutatingWebhookAccessor is being called when refreshing
// webhookAccessors.
//
// NOTE: Maybe there some testing help that we can import and reuse instead.
type mockCreateMutatingWebhookAccessor struct {
	numberOfCalls int
}

func (mock *mockCreateMutatingWebhookAccessor) calledNTimes() int { return mock.numberOfCalls }
func (mock *mockCreateMutatingWebhookAccessor) resetCounter()     { mock.numberOfCalls = 0 }
func (mock *mockCreateMutatingWebhookAccessor) incrementCounter() { mock.numberOfCalls++ }

func (mock *mockCreateMutatingWebhookAccessor) fn(uid string, configurationName string, h *v1.MutatingWebhook) webhook.WebhookAccessor {
	mock.incrementCounter()
	return webhook.NewMutatingWebhookAccessor(uid, configurationName, h)
}

func mutatingConfigurationTotalWebhooks(configurations []*v1.MutatingWebhookConfiguration) int {
	total := 0
	for _, configuration := range configurations {
		total += len(configuration.Webhooks)
	}
	return total
}

func TestGetMutatingWebhookConfigSmartReload(t *testing.T) {
	type args struct {
		createWebhookConfigurations []*v1.MutatingWebhookConfiguration
		updateWebhookConfigurations []*v1.MutatingWebhookConfiguration
	}
	tests := []struct {
		name              string
		args              args
		numberOfCreations int
		// number of refreshes are number of times we recreated a webhook accessor
		// instead of pulling from the cache.
		numberOfRefreshes             int
		finalNumberOfWebhookAccessors int
	}{
		{
			name: "no creations and no updates",
			args: args{
				nil,
				nil,
			},
			numberOfCreations:             0,
			numberOfRefreshes:             0,
			finalNumberOfWebhookAccessors: 0,
		},
		{
			name: "create configurations and no updates",
			args: args{
				[]*v1.MutatingWebhookConfiguration{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook1"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook1.1"}, {Name: "webhook1.2"}},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook2"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook2.1"}, {Name: "webhook2.2"}},
					},
				},
				nil,
			},
			numberOfCreations:             4,
			numberOfRefreshes:             0,
			finalNumberOfWebhookAccessors: 4,
		},
		{
			name: "create configurations and update some of them",
			args: args{
				[]*v1.MutatingWebhookConfiguration{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook3"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook3.1"}},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook4"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook4.1"}},
					},
				},
				[]*v1.MutatingWebhookConfiguration{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook4"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook4.1-updated"}, {Name: "webhook4.2"}},
					},
				},
			},
			numberOfCreations:             2,
			numberOfRefreshes:             2,
			finalNumberOfWebhookAccessors: 3,
		},
		{
			name: "create configuration and update moar of them",
			args: args{
				[]*v1.MutatingWebhookConfiguration{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook5"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook5.1"}, {Name: "webhook5.2"}},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook6"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook6.1"}},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook7"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook7.1"}},
					},
				},
				[]*v1.MutatingWebhookConfiguration{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook6"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook6.1-updated"}},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "webhook7"},
						Webhooks:   []v1.MutatingWebhook{{Name: "webhook7.1-updated"}, {Name: "webhook7.2"}},
					},
				},
			},
			numberOfCreations:             4,
			numberOfRefreshes:             3,
			finalNumberOfWebhookAccessors: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			stop := make(chan struct{})
			defer close(stop)
			manager := NewMutatingWebhookConfigurationManager(informerFactory)
			managerStructPtr := manager.(*mutatingWebhookConfigurationManager)
			fakeWebhookAccessorCreator := &mockCreateMutatingWebhookAccessor{}
			managerStructPtr.createMutatingWebhookAccessor = fakeWebhookAccessorCreator.fn
			informerFactory.Start(stop)
			informerFactory.WaitForCacheSync(stop)

			// Create webhooks
			for _, configurations := range tt.args.createWebhookConfigurations {
				client.
					AdmissionregistrationV1().
					MutatingWebhookConfigurations().
					Create(context.TODO(), configurations, metav1.CreateOptions{})
			}
			// TODO use channels to wait for manager.createMutatingWebhookAccessor
			// to be called instead of using time.Sleep
			time.Sleep(1 * time.Second)
			webhooks := manager.Webhooks()
			if mutatingConfigurationTotalWebhooks(tt.args.createWebhookConfigurations) != len(webhooks) {
				t.Errorf("Expected number of webhooks %d received %d",
					mutatingConfigurationTotalWebhooks(tt.args.createWebhookConfigurations),
					len(webhooks),
				)
			}
			// assert creations
			if tt.numberOfCreations != fakeWebhookAccessorCreator.calledNTimes() {
				t.Errorf(
					"Expected number of creations %d received %d",
					tt.numberOfCreations, fakeWebhookAccessorCreator.calledNTimes(),
				)
			}

			// reset mock counter
			fakeWebhookAccessorCreator.resetCounter()

			// Update webhooks
			for _, configurations := range tt.args.updateWebhookConfigurations {
				client.
					AdmissionregistrationV1().
					MutatingWebhookConfigurations().
					Update(context.TODO(), configurations, metav1.UpdateOptions{})
			}
			// TODO use channels to wait for manager.createMutatingWebhookAccessor
			// to be called instead of using time.Sleep
			time.Sleep(1 * time.Second)
			webhooks = manager.Webhooks()
			if tt.finalNumberOfWebhookAccessors != len(webhooks) {
				t.Errorf("Expected final number of webhooks %d received %d",
					tt.finalNumberOfWebhookAccessors,
					len(webhooks),
				)
			}

			// assert updates
			if tt.numberOfRefreshes != fakeWebhookAccessorCreator.calledNTimes() {
				t.Errorf(
					"Expected number of refreshes %d received %d",
					tt.numberOfRefreshes, fakeWebhookAccessorCreator.calledNTimes(),
				)
			}
			// reset mock counter for the next test cases
			fakeWebhookAccessorCreator.resetCounter()
		})
	}
}
