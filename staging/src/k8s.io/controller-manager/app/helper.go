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

package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// WaitForAPIServer waits for the API Server's /readyz endpoint to report "ok" with timeout.
func WaitForAPIServer(client clientset.Interface, timeout time.Duration) error {
	var lastErr error

	err := wait.PollImmediate(time.Second, timeout, func() (bool, error) {
		readyStatus := 0
		result := client.Discovery().RESTClient().Get().AbsPath("/readyz").Do(context.TODO()).StatusCode(&readyStatus)
		if result.Error() != nil {
			lastErr = fmt.Errorf("failed to get apiserver /readyz status: %w", result.Error())
			return false, nil
		}
		if readyStatus != http.StatusOK {
			content, _ := result.Raw()
			lastErr = fmt.Errorf("APIServer isn't ready: %v", string(content))
			klog.Warningf("APIServer isn't ready yet: %v. Waiting a little while.", string(content))
			return false, nil
		}

		return true, nil
	})

	if err != nil {
		return fmt.Errorf("%v: %v", err, lastErr)
	}

	return nil
}

// IsControllerEnabled check if a specified controller enabled or not.
func IsControllerEnabled(name string, disabledByDefaultControllers sets.String, controllers []string) bool {
	hasStar := false
	for _, ctrl := range controllers {
		if ctrl == name {
			return true
		}
		if ctrl == "-"+name {
			return false
		}
		if ctrl == "*" {
			hasStar = true
		}
	}
	// if we get here, there was no explicit choice
	if !hasStar {
		// nothing on by default
		return false
	}

	return !disabledByDefaultControllers.Has(name)
}
