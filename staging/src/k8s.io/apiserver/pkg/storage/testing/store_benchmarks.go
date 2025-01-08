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

package testing

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apiserver/pkg/apis/example"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/storage"
)

type scope string

var (
	cluster   scope = "Cluster"
	node      scope = "Node"
	namespace scope = "Namespace"
)

func RunBenchmarkStoreListCreate(ctx context.Context, b *testing.B, store storage.Interface, match metav1.ResourceVersionMatch) {
	objectCount := atomic.Uint64{}
	pods := make([]*example.Pod, 0, b.N)
	for i := 0; i < b.N; i++ {
		name := rand.String(100)
		pods = append(pods, &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pod := pods[i]
		podOut := &example.Pod{}
		err := store.Create(ctx, ComputePodKey(pod), pod, podOut, 0)
		if err != nil {
			panic(fmt.Sprintf("Unexpected error %s", err))
		}
		listOut := &example.PodList{}
		err = store.GetList(ctx, "/pods", storage.ListOptions{
			Recursive:            true,
			ResourceVersion:      podOut.ResourceVersion,
			ResourceVersionMatch: match,
			Predicate: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.Everything(),
				Limit: 1,
			},
		}, listOut)
		if err != nil {
			panic(fmt.Sprintf("Unexpected error %s", err))
		}
		if len(listOut.Items) != 1 {
			b.Errorf("Expected to get 1 element, got %d", len(listOut.Items))
		}
		objectCount.Add(uint64(len(listOut.Items)))
	}
	b.ReportMetric(float64(objectCount.Load())/float64(b.N), "objects/op")
}

func RunBenchmarkStoreList(ctx context.Context, b *testing.B, store storage.Interface, data BenchmarkData, useIndex bool) {
	for _, rvm := range []metav1.ResourceVersionMatch{"Empty", metav1.ResourceVersionMatchExact, metav1.ResourceVersionMatchNotOlderThan} {
		b.Run(fmt.Sprintf("RV=%s", rvm), func(b *testing.B) {
			for _, scope := range []scope{cluster, node, namespace} {
				b.Run(fmt.Sprintf("Scope=%s", scope), func(b *testing.B) {
					var expectedElements int
					switch scope {
					case namespace:
						expectedElements = len(data.Pods) / len(data.NamespaceNames)
					case node:
						expectedElements = len(data.Pods) / len(data.NodeNames)
					case cluster:
						expectedElements = len(data.Pods)
					}
					limitOptions := []int64{0}
					switch {
					case expectedElements > 1000:
						limitOptions = append(limitOptions, 1000)
					case expectedElements > 100:
						limitOptions = append(limitOptions, 100)
					}
					for _, limit := range limitOptions {
						b.Run(fmt.Sprintf("Paginate=%v", limit), func(b *testing.B) {
							runBenchmarkStoreList(ctx, b, store, limit, rvm, scope, data, useIndex)
						})
					}
				})
			}
		})
	}
}

func runBenchmarkStoreList(ctx context.Context, b *testing.B, store storage.Interface, limit int64, match metav1.ResourceVersionMatch, scope scope, data BenchmarkData, useIndex bool) {
	wg := sync.WaitGroup{}
	objectCount := atomic.Uint64{}
	pageCount := atomic.Uint64{}
	for i := 0; i < b.N; i++ {
		wg.Add(1)
		resourceVersion := ""
		switch match {
		case metav1.ResourceVersionMatchExact, metav1.ResourceVersionMatchNotOlderThan:
			maxRevision := 1 + len(data.Pods)
			resourceVersion = fmt.Sprintf("%d", maxRevision-99+i%100)
		case "Empty":
			match = ""
		}
		go func(resourceVersion, nodeName, namespaceName string) {
			defer wg.Done()
			opts := storage.ListOptions{
				Recursive:            true,
				ResourceVersion:      resourceVersion,
				ResourceVersionMatch: match,
				Predicate: storage.SelectionPredicate{
					GetAttrs: podAttr,
					Label:    labels.Everything(),
					Field:    fields.Everything(),
					Limit:    limit,
				},
			}
			switch scope {
			case cluster:
				objects, pages := paginateList(ctx, store, "/pods/", opts)
				objectCount.Add(uint64(objects))
				pageCount.Add(uint64(pages))
			case node:
				if useIndex {
					opts.Predicate.GetAttrs = podAttr
					opts.Predicate.IndexFields = []string{"spec.nodeName"}
					opts.Predicate.Field = fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName})
				}
				objects, pages := paginateList(ctx, store, "/pods/", opts)
				objectCount.Add(uint64(objects))
				pageCount.Add(uint64(pages))
			case namespace:
				ctx := ctx
				if useIndex {
					opts.Predicate.IndexFields = []string{"metadata.namespace"}
					ctx = request.WithRequestInfo(ctx, &request.RequestInfo{Namespace: namespaceName})
				}
				objects, pages := paginateList(ctx, store, "/pods/"+namespaceName, opts)
				objectCount.Add(uint64(objects))
				pageCount.Add(uint64(pages))
			}
		}(resourceVersion, data.NodeNames[i%len(data.NodeNames)], data.NamespaceNames[i%len(data.NamespaceNames)])
	}
	wg.Wait()
	b.ReportMetric(float64(objectCount.Load())/float64(b.N), "objects/op")
	b.ReportMetric(float64(pageCount.Load())/float64(b.N), "pages/op")
}

func paginateList(ctx context.Context, store storage.Interface, key string, opts storage.ListOptions) (objectCount int, pageCount int) {
	listOut := &example.PodList{}
	err := store.GetList(ctx, key, opts, listOut)
	if err != nil {
		panic(fmt.Sprintf("Unexpected error %s", err))
	}
	opts.Predicate.Continue = listOut.Continue
	opts.ResourceVersion = ""
	opts.ResourceVersionMatch = ""
	pageCount += 1
	objectCount += len(listOut.Items)
	for opts.Predicate.Continue != "" {
		listOut := &example.PodList{}
		err := store.GetList(ctx, key, opts, listOut)
		if err != nil {
			panic(fmt.Sprintf("Unexpected error %s", err))
		}
		opts.Predicate.Continue = listOut.Continue
		pageCount += 1
		objectCount += len(listOut.Items)
	}
	return objectCount, pageCount
}

func podAttr(obj runtime.Object) (labels.Set, fields.Set, error) {
	pod := obj.(*example.Pod)
	return nil, fields.Set{
		"spec.nodeName":      pod.Spec.NodeName,
		"metadata.namespace": pod.Namespace,
	}, nil
}

func PrepareBenchchmarkData(namespaceCount, podPerNamespaceCount, nodeCount int) (data BenchmarkData) {
	data.NodeNames = make([]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		data.NodeNames[i] = rand.String(10)
	}
	data.NamespaceNames = make([]string, namespaceCount)
	for i := 0; i < namespaceCount; i++ {
		namespace := rand.String(10)
		data.NamespaceNames[i] = namespace
		for j := 0; j < podPerNamespaceCount; j++ {
			name := rand.String(10)
			data.Pods = append(data.Pods, &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Spec: example.PodSpec{NodeName: data.NodeNames[rand.Intn(nodeCount)]}})
		}
	}
	return data
}

type BenchmarkData struct {
	Pods           []*example.Pod
	NamespaceNames []string
	NodeNames      []string
}
