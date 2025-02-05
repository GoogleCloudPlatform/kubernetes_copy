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

package logrotation

import (
	"fmt"
	"io"
	"time"
)

type RotationStub struct{}

func OpenStub(filePath string, enableFlush bool, maxSize int64, maxAge time.Duration) (io.WriteCloser, error) {
	w := &RotationStub{}
	fmt.Printf("filePath: %s, enableFlush: %t, maxSize: %d, maxAge: %s", filePath, enableFlush, maxSize, maxAge)
	return w, nil
}

// write func to satisfy io.writer interface
func (w *RotationStub) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w *RotationStub) Close() error {
	return nil
}
