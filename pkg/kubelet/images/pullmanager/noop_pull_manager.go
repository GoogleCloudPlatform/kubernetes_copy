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

package pullmanager

import (
	"time"

	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
)

var _ ImagePullManager = &NoopImagePullManager{}

type NoopImagePullManager struct{}

func (m *NoopImagePullManager) RecordPullIntent(_ string) error { return nil }
func (m *NoopImagePullManager) RecordImagePulled(_, _ string, _ *kubeletconfiginternal.ImagePullCredentials) {
}
func (m *NoopImagePullManager) RecordImagePullFailed(image string) {}
func (m *NoopImagePullManager) MustAttemptImagePull(_, _ string, _ []kubeletconfiginternal.ImagePullSecret) bool {
	return false
}
func (m *NoopImagePullManager) PruneUnknownRecords(_ []string, _ time.Time) {}
