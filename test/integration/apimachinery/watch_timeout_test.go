/*
Copyright 2020 The Kubernetes Authors.

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

package apimachinery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	kubectlproxy "k8s.io/kubectl/pkg/proxy"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

type extractRT struct {
	http.Header
}

func (rt *extractRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.Header = req.Header
	return &http.Response{}, nil
}

// headersForConfig extracts any http client logic necessary for the provided
// config.
func headersForConfig(c *restclient.Config, url *url.URL) (http.Header, error) {
	extract := &extractRT{}
	rt, err := restclient.HTTPWrappersForConfig(c, extract)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return nil, err
	}
	if _, err := rt.RoundTrip(request); err != nil {
		return nil, err
	}
	return extract.Header, nil
}

// websocketConfig constructs a websocket config to the provided URL, using the client
// config, with the specified protocols.
func websocketConfig(url *url.URL, config *restclient.Config, protocols []string) (*websocket.Config, error) {
	tlsConfig, err := restclient.TLSConfigFor(config)
	if err != nil {
		return nil, fmt.Errorf("Failed to create tls config: %v", err)
	}
	if url.Scheme == "https" {
		url.Scheme = "wss"
	} else {
		url.Scheme = "ws"
	}
	headers, err := headersForConfig(config, url)
	if err != nil {
		return nil, fmt.Errorf("Failed to load http headers: %v", err)
	}
	cfg, err := websocket.NewConfig(url.String(), "http://localhost")
	if err != nil {
		return nil, fmt.Errorf("Failed to create websocket config: %v", err)
	}
	cfg.Header = headers
	cfg.TlsConfig = tlsConfig
	cfg.Protocol = protocols
	return cfg, err
}

func TestWebsocketWatchClientTimeout(t *testing.T) {
	// server setup
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	defer server.TearDownFn()

	// object setup
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}},
		},
	}
	configmap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
	}
	clientset, err := kubernetes.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientset.CoreV1().Services("default").Create(context.TODO(), service, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := clientset.CoreV1().ConfigMaps("default").Create(context.TODO(), configmap, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name         string
		path         string
		timeout      time.Duration
		expectResult string
	}{
		{
			name:         "configmaps",
			path:         "/api/v1/configmaps?watch=true&timeoutSeconds=5",
			timeout:      10 * time.Second,
			expectResult: `"name":"test"`,
		},
		{
			name:         "services",
			path:         "/api/v1/services?watch=true&timeoutSeconds=5",
			timeout:      10 * time.Second,
			expectResult: `"name":"test"`,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			url, err := url.Parse(server.ClientConfig.Host + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			wsc, err := websocketConfig(url, server.ClientConfig, nil)
			if err != nil {
				t.Fatal(err)
			}

			wsConn, err := websocket.DialConfig(wsc)
			if err != nil {
				t.Fatal(err)
			}
			defer wsConn.Close()

			resultCh := make(chan string)
			go func() {
				defer close(resultCh)
				buf := &bytes.Buffer{}
				for {
					var msg []byte
					if err := websocket.Message.Receive(wsConn, &msg); err != nil {
						if err == io.EOF {
							resultCh <- buf.String()
							return
						}
						if !t.Failed() {
							// if we didn't already fail, treat this as an error
							t.Errorf("Failed to read completely from websocket %v", err)
						}
						return
					}
					if len(msg) == 0 {
						t.Logf("zero-length message")
						continue
					}
					t.Logf("Read %v %v", len(msg), string(msg))
					buf.Write(msg)
				}
			}()

			select {
			case resultString := <-resultCh:
				if !strings.Contains(resultString, tc.expectResult) {
					t.Fatalf("Unexpected result:\n%s", resultString)
				}
			case <-time.After(tc.timeout):
				t.Fatalf("hit timeout before connection closed")
			}
		})
	}
}

func TestWatchClientTimeoutXXX(t *testing.T) {
	// server setup
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	defer server.TearDownFn()

	t.Run("direct", func(t *testing.T) {
		t.Logf("client at %s", server.ClientConfig.Host)
		testWatchClientTimeouts(t, restclient.CopyConfig(server.ClientConfig))
	})

	t.Run("reverse proxy", func(t *testing.T) {
		u, _ := url.Parse(server.ClientConfig.Host)
		proxy := httputil.NewSingleHostReverseProxy(u)
		proxy.FlushInterval = -1

		transport, err := restclient.TransportFor(server.ClientConfig)
		if err != nil {
			t.Fatal(err)
		}
		proxy.Transport = transport

		proxyServer := httptest.NewServer(proxy)
		defer proxyServer.Close()

		t.Logf("client to %s, backend at %s", proxyServer.URL, server.ClientConfig.Host)
		testWatchClientTimeouts(t, &restclient.Config{Host: proxyServer.URL})
	})

	t.Run("kubectl proxy", func(t *testing.T) {
		kubectlProxyServer, err := kubectlproxy.NewServer("", "/", "/static/", nil, server.ClientConfig, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		kubectlProxyListener, err := kubectlProxyServer.Listen("", 0)
		if err != nil {
			t.Fatal(err)
		}
		defer kubectlProxyListener.Close()
		go kubectlProxyServer.ServeOnListener(kubectlProxyListener)

		t.Logf("client to %s, backend at %s", kubectlProxyListener.Addr().String(), server.ClientConfig.Host)
		testWatchClientTimeouts(t, &restclient.Config{Host: "http://" + kubectlProxyListener.Addr().String()})
	})
}

func testWatchClientTimeouts(t *testing.T, config *restclient.Config) {
	t.Run("timeout", func(t *testing.T) {
		testWatchClientTimeout(t, config, time.Second, 0)
	})
	t.Run("timeoutSeconds", func(t *testing.T) {
		testWatchClientTimeout(t, config, 0, time.Second)
	})
	t.Run("timeout+timeoutSeconds", func(t *testing.T) {
		testWatchClientTimeout(t, config, time.Second, time.Second)
	})
}

func testWatchClientTimeout(t *testing.T, config *restclient.Config, timeout, timeoutSeconds time.Duration) {
	config.Timeout = timeout
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	listCount := 0
	watchCount := 0
	stopCh := make(chan struct{})
	listWatch := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			t.Logf("listing (version=%s continue=%s)", options.ResourceVersion, options.Continue)
			listCount++
			if listCount > 1 {
				t.Errorf("listed more than once")
				close(stopCh)
			}
			return client.CoreV1().ConfigMaps(metav1.NamespaceAll).List(context.TODO(), options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			t.Logf("watching (version=%s)", options.ResourceVersion)
			if timeoutSeconds != 0 {
				timeout := int64(timeoutSeconds / time.Second)
				options.TimeoutSeconds = &timeout
			}
			watchCount++
			if watchCount > 1 {
				// success, restarted watch
				close(stopCh)
			}
			return client.CoreV1().ConfigMaps(metav1.NamespaceAll).Watch(context.TODO(), options)
		},
	}
	_, informer := cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: listWatch,
		ObjectType:    &corev1.ConfigMap{},
		ResyncPeriod:  30 * time.Minute,
		Handler:       cache.ResourceEventHandlerFuncs{},
		Indexers:      cache.Indexers{},
	})
	informer.Run(stopCh)
	select {
	case <-stopCh:
	case <-time.After(time.Minute):
		t.Fatal("timeout")
	}
}
