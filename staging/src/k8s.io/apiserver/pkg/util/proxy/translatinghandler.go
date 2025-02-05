/*
Copyright 2023 The Kubernetes Authors.

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

package proxy

import (
	"net/http"

	"k8s.io/klog/v2"
)

// translatingHandler wraps the delegate handler, implementing the
// http.Handler interface. The delegate handles all requests unless
// the request satisfies the passed "shouldTranslate" function
// (currently only for WebSocket/V5 request), in which case the translator
// handles the request.
type translatingHandler struct {
	delegate        http.Handler
	translator      http.Handler
	shouldTranslate func(*http.Request) bool
}

func NewTranslatingHandler(delegate http.Handler, translator http.Handler, shouldTranslate func(*http.Request) bool) http.Handler {
	return &translatingHandler{
		delegate:        delegate,
		translator:      translator,
		shouldTranslate: shouldTranslate,
	}
}

func (t *translatingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if t.shouldTranslate(req) {
		klog.FromContext(req.Context()).V(4).Info("Request handled by translator proxy")
		t.translator.ServeHTTP(w, req)
		return
	}
	t.delegate.ServeHTTP(w, req)
}
