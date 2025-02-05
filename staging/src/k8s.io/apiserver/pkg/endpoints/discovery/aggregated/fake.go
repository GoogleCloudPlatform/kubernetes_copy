/*
Copyright 2022 The Kubernetes Authors.

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

package aggregated

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/emicklei/go-restful/v3"
	"github.com/google/go-cmp/cmp" //nolint:depguard
	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

type FakeResourceManager interface {
	ResourceManager
	Expect() ResourceManager

	HasExpectedNumberActions() bool
	Validate() error
	WaitForActions(ctx context.Context, timeout time.Duration) error
}

func NewFakeResourceManager() FakeResourceManager {
	return &fakeResourceManager{}
}

// a resource manager with helper functions for checking the actions
// match expected. For Use in tests
type fakeResourceManager struct {
	recorderResourceManager
	expect recorderResourceManager
}

// a resource manager which instead of managing a discovery document,
// simply records the calls to its interface functions for testing
type recorderResourceManager struct {
	lock    sync.RWMutex
	Actions []recorderResourceManagerAction
}

var _ ResourceManager = &fakeResourceManager{}
var _ ResourceManager = &recorderResourceManager{}

// Storage type for a call to the resource manager
type recorderResourceManagerAction struct {
	Type    string
	Group   string
	Version string
	Value   interface{}
}

func (f *fakeResourceManager) Expect() ResourceManager {
	return &f.expect
}

func (f *fakeResourceManager) HasExpectedNumberActions() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	f.expect.lock.RLock()
	defer f.expect.lock.RUnlock()

	return len(f.Actions) >= len(f.expect.Actions)
}

func (f *fakeResourceManager) Validate() error {
	f.lock.RLock()
	defer f.lock.RUnlock()

	f.expect.lock.RLock()
	defer f.expect.lock.RUnlock()

	if !reflect.DeepEqual(f.expect.Actions, f.Actions) {
		return errors.New(cmp.Diff(f.expect.Actions, f.Actions))
	}
	return nil
}

func (f *fakeResourceManager) WaitForActions(ctx context.Context, timeout time.Duration) error {
	err := wait.PollImmediateWithContext(
		ctx,
		100*time.Millisecond, // try every 100ms
		timeout,              // timeout after timeout
		func(ctx context.Context) (done bool, err error) {
			if f.HasExpectedNumberActions() {
				return true, f.Validate()
			}
			return false, nil
		})
	return err
}

func (f *recorderResourceManager) SetGroupVersionPriority(gv metav1.GroupVersion, grouppriority, versionpriority int) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.Actions = append(f.Actions, recorderResourceManagerAction{
		Type:    "SetGroupVersionPriority",
		Group:   gv.Group,
		Version: gv.Version,
		Value:   versionpriority,
	})
}

func (f *recorderResourceManager) AddGroupVersion(groupName string, value apidiscoveryv2.APIVersionDiscovery) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.Actions = append(f.Actions, recorderResourceManagerAction{
		Type:  "AddGroupVersion",
		Group: groupName,
		Value: value,
	})
}
func (f *recorderResourceManager) RemoveGroup(groupName string) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.Actions = append(f.Actions, recorderResourceManagerAction{
		Type:  "RemoveGroup",
		Group: groupName,
	})

}
func (f *recorderResourceManager) RemoveGroupVersion(gv metav1.GroupVersion) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.Actions = append(f.Actions, recorderResourceManagerAction{
		Type:    "RemoveGroupVersion",
		Group:   gv.Group,
		Version: gv.Version,
	})

}
func (f *recorderResourceManager) SetGroups(values []apidiscoveryv2.APIGroupDiscovery) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.Actions = append(f.Actions, recorderResourceManagerAction{
		Type:  "SetGroups",
		Value: values,
	})
}
func (f *recorderResourceManager) WebService() *restful.WebService {
	panic("unimplemented")
}

func (f *recorderResourceManager) ServeHTTP(http.ResponseWriter, *http.Request) {
	panic("unimplemented")
}

func (f *recorderResourceManager) WithSource(source Source) ResourceManager {
	panic("unimplemented")
}
