/*
Copyright 2016 The Kubernetes Authors.

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

package cache

import (
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	concurrencyLevel = 5
)

func BenchmarkListener(b *testing.B) {
	var notification addNotification

	var swg sync.WaitGroup
	swg.Add(b.N)
	b.SetParallelism(concurrencyLevel)
	// Preallocate enough space so that benchmark does not run out of it
	pl := newProcessListener(klog.Background(), "test", &ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			swg.Done()
		},
	}, 0, 0, time.Now(), 1024*1024, func() bool { return true })
	var wg wait.Group
	defer wg.Wait()       // Wait for .run and .pop to stop
	defer close(pl.addCh) // Tell .run and .pop to stop
	wg.Start(pl.run)
	wg.Start(pl.pop)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			pl.add(notification)
		}
	})
	swg.Wait() // Block until all notifications have been received
	b.StopTimer()
}
