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

package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/util/interrupt"
)

// ConditionalWait hold information to check an API status condition
type ConditionalWait struct {
	conditionName   string
	conditionStatus string
	// errOut is written to if an error occurs
	errOut io.Writer
}

// IsConditionMet is a conditionfunc for waiting on an API condition to be met
func (w ConditionalWait) IsConditionMet(ctx context.Context, info *resource.Info, o *WaitOptions) (runtime.Object, bool, error) {
	return getObjAndCheckCondition(ctx, info, o, w.isConditionMet, w.checkCondition)
}

func (w ConditionalWait) checkCondition(obj *unstructured.Unstructured) (bool, error) {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	for _, conditionUncast := range conditions {
		condition := conditionUncast.(map[string]interface{})
		name, found, err := unstructured.NestedString(condition, "type")
		if !found || err != nil || !strings.EqualFold(name, w.conditionName) {
			continue
		}
		status, found, err := unstructured.NestedString(condition, "status")
		if !found || err != nil {
			continue
		}
		generation, found, _ := unstructured.NestedInt64(obj.Object, "metadata", "generation")
		if found {
			observedGeneration, found := getObservedGeneration(obj, condition)
			if found && observedGeneration < generation {
				klog.V(2).Info("observedGeneration < generation, meaning the controller has yet to process the new changes resulting in a stale condition")
				return false, nil
			}
		}
		return strings.EqualFold(status, w.conditionStatus), nil
	}

	return false, nil
}

func (w ConditionalWait) isConditionMet(event watch.Event) (bool, error) {
	if event.Type == watch.Error {
		// keep waiting in the event we see an error - we expect the watch to be closed by
		// the server
		err := apierrors.FromObject(event.Object)
		fmt.Fprintf(w.errOut, "error: An error occurred while waiting for the condition to be satisfied: %v", err)
		return false, nil
	}
	if event.Type == watch.Deleted {
		// this will chain back out, result in another get and an return false back up the chain
		return false, nil
	}
	obj := event.Object.(*unstructured.Unstructured)
	return w.checkCondition(obj)
}

type isCondMetFunc func(event watch.Event) (bool, error)
type checkCondFunc func(obj *unstructured.Unstructured) (bool, error)

// getObjAndCheckCondition will make a List query to the API server to get the object and check if the condition is met using check function.
// If the condition is not met, it will make a Watch query to the server and pass in the condMet function
func getObjAndCheckCondition(ctx context.Context, info *resource.Info, o *WaitOptions, condMet isCondMetFunc, check checkCondFunc) (runtime.Object, bool, error) {
	if len(info.Name) == 0 {
		return info.Object, false, fmt.Errorf("resource name must be provided")
	}

	endTime := time.Now().Add(o.Timeout)
	timeout := time.Until(endTime)
	errWaitTimeoutWithName := extendErrWaitTimeout(wait.ErrWaitTimeout, info) // nolint:staticcheck // SA1019
	if o.Timeout == 0 {
		// If timeout is zero we will fetch the object(s) once only and check
		gottenObj, initObjGetErr := o.DynamicClient.Resource(info.Mapping.Resource).Namespace(info.Namespace).Get(context.Background(), info.Name, metav1.GetOptions{})
		if initObjGetErr != nil {
			return nil, false, initObjGetErr
		}
		if gottenObj == nil {
			return nil, false, fmt.Errorf("condition not met for %s", info.ObjectName())
		}
		conditionCheck, err := check(gottenObj)
		if err != nil {
			return gottenObj, false, err
		}
		if !conditionCheck {
			return gottenObj, false, fmt.Errorf("condition not met for %s", info.ObjectName())
		}
		return gottenObj, true, nil
	}
	if timeout < 0 {
		// we're out of time
		return info.Object, false, errWaitTimeoutWithName
	}

	mapping := info.ResourceMapping() // used to pass back meaningful errors if object disappears
	fieldSelector := fields.OneTermEqualSelector("metadata.name", info.Name).String()
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = fieldSelector
			return o.DynamicClient.Resource(info.Mapping.Resource).Namespace(info.Namespace).List(context.TODO(), options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = fieldSelector
			return o.DynamicClient.Resource(info.Mapping.Resource).Namespace(info.Namespace).Watch(context.TODO(), options)
		},
	}

	// this function is used to refresh the cache to prevent timeout waits on resources that have disappeared
	preconditionFunc := func(store cache.Store) (bool, error) {
		_, exists, err := store.Get(&metav1.ObjectMeta{Namespace: info.Namespace, Name: info.Name})
		if err != nil {
			return true, err
		}
		if !exists {
			return true, apierrors.NewNotFound(mapping.Resource.GroupResource(), info.Name)
		}

		return false, nil
	}

	intrCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var result runtime.Object
	intr := interrupt.New(nil, cancel)
	err := intr.Run(func() error {
		ev, err := watchtools.UntilWithSync(intrCtx, lw, &unstructured.Unstructured{}, preconditionFunc, watchtools.ConditionFunc(condMet))
		if ev != nil {
			result = ev.Object
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return errWaitTimeoutWithName
		}
		return err
	})
	if err != nil {
		if errors.Is(err, wait.ErrWaitTimeout) { // nolint:staticcheck // SA1019
			return result, false, errWaitTimeoutWithName
		}
		return result, false, err
	}

	return result, true, nil
}

func extendErrWaitTimeout(err error, info *resource.Info) error {
	return fmt.Errorf("%s on %s/%s", err.Error(), info.Mapping.Resource.Resource, info.Name)
}

func getObservedGeneration(obj *unstructured.Unstructured, condition map[string]interface{}) (int64, bool) {
	conditionObservedGeneration, found, _ := unstructured.NestedInt64(condition, "observedGeneration")
	if found {
		return conditionObservedGeneration, true
	}
	statusObservedGeneration, found, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	return statusObservedGeneration, found
}
