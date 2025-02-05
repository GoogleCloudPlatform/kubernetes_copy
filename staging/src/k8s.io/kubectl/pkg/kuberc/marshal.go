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

package kuberc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"k8s.io/kubectl/pkg/config"
)

// decodePreference iterates over the yamls in kuberc file to find the supported kuberc version.
// Once it finds, it returns the compatible kuberc object as well as accumulated errors during the iteration.
func decodePreference(kubercFile string) (*config.Preference, error) {
	kubercBytes, err := os.ReadFile(kubercFile)
	if err != nil {
		return nil, err
	}

	attemptedItems := 0
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewBuffer(kubercBytes)))
	for {
		doc, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			// no more entries, expected when we reach the end of the file
			break
		}
		if readErr != nil {
			// other errors are fatal
			return nil, readErr
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			// empty item, ignore
			continue
		}
		// remember we attempted
		attemptedItems++
		pref, gvk, strictDecodeErr := strictCodecs.UniversalDecoder().Decode(doc, nil, nil)
		if strictDecodeErr != nil {
			var lenientDecodeErr error
			pref, gvk, lenientDecodeErr = lenientCodecs.UniversalDecoder().Decode(doc, nil, nil)
			if lenientDecodeErr != nil {
				// both strict and lenient failed
				// verbose log the error with the most information about this item and continue
				klog.V(5).Infof("kuberc strict decoding failed %v", strictDecodeErr)
				continue
			}
		}

		// check expected GVK, if bad, verbose log and continue
		expectedGK := schema.GroupKind{
			Group: config.SchemeGroupVersion.Group,
			Kind:  "Preference",
		}
		if gvk.GroupKind() != expectedGK {
			klog.V(5).Infof("unexpected GroupVersionKind for kuberc: %v", gvk)
			continue
		}

		// check expected go type, if bad, verbose log and continue
		preferences, ok := pref.(*config.Preference)
		if !ok {
			klog.V(5).Infof("invalid preference format in kuberc")
			continue
		}

		// we have a usable preferences to return
		klog.V(5).Infof("successfully decoded kuberc %s", kubercFile)
		return preferences, strictDecodeErr

	}
	if attemptedItems > 0 {
		return nil, fmt.Errorf("no valid preferences found in %s, use --v=5 to see details", kubercFile)
	}
	// empty doc
	klog.V(5).Infof("empty kuberc file %s", kubercFile)
	return nil, nil
}
