/*
Copyright 2014 The Kubernetes Authors.

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
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
)

// atomsToAttrs states which attributes of which tags require URL substitution.
// Sources: http://www.w3.org/TR/REC-html40/index/attributes.html
//
//	http://www.w3.org/html/wg/drafts/html/master/index.html#attributes-1
var atomsToAttrs = map[atom.Atom]sets.String{
	atom.A:          sets.NewString("href"),
	atom.Applet:     sets.NewString("codebase"),
	atom.Area:       sets.NewString("href"),
	atom.Audio:      sets.NewString("src"),
	atom.Base:       sets.NewString("href"),
	atom.Blockquote: sets.NewString("cite"),
	atom.Body:       sets.NewString("background"),
	atom.Button:     sets.NewString("formaction"),
	atom.Command:    sets.NewString("icon"),
	atom.Del:        sets.NewString("cite"),
	atom.Embed:      sets.NewString("src"),
	atom.Form:       sets.NewString("action"),
	atom.Frame:      sets.NewString("longdesc", "src"),
	atom.Head:       sets.NewString("profile"),
	atom.Html:       sets.NewString("manifest"),
	atom.Iframe:     sets.NewString("longdesc", "src"),
	atom.Img:        sets.NewString("longdesc", "src", "usemap"),
	atom.Input:      sets.NewString("src", "usemap", "formaction"),
	atom.Ins:        sets.NewString("cite"),
	atom.Link:       sets.NewString("href"),
	atom.Object:     sets.NewString("classid", "codebase", "data", "usemap"),
	atom.Q:          sets.NewString("cite"),
	atom.Script:     sets.NewString("src"),
	atom.Source:     sets.NewString("src"),
	atom.Video:      sets.NewString("poster", "src"),

	// TODO: css URLs hidden in style elements.
}

// Transport is a transport for text/html content that replaces URLs in html
// content with the prefix of the proxy server
type Transport struct {
	Scheme      string
	Host        string
	PathPrepend string

	http.RoundTripper
}

// RoundTrip implements the http.RoundTripper interface
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Add reverse proxy headers.
	forwardedURI := path.Join(t.PathPrepend, req.URL.EscapedPath())
	if strings.HasSuffix(req.URL.Path, "/") {
		forwardedURI = forwardedURI + "/"
	}
	req.Header.Set("X-Forwarded-Uri", forwardedURI)
	if len(t.Host) > 0 {
		req.Header.Set("X-Forwarded-Host", t.Host)
	}
	if len(t.Scheme) > 0 {
		req.Header.Set("X-Forwarded-Proto", t.Scheme)
	}

	rt := t.RoundTripper
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := rt.RoundTrip(req)

	if err != nil {
		return nil, errors.NewServiceUnavailable(fmt.Sprintf("error trying to reach service: %v", err))
	}

	if redirect := resp.Header.Get("Location"); redirect != "" {
		targetURL, err := url.Parse(redirect)
		if err != nil {
			return nil, errors.NewInternalError(fmt.Errorf("error trying to parse Location header: %v", err))
		}
		resp.Header.Set("Location", t.rewriteURL(targetURL, req.URL, req.Host))
		return resp, nil
	}

	cType := resp.Header.Get("Content-Type")
	cType = strings.TrimSpace(strings.SplitN(cType, ";", 2)[0])
	if cType != "text/html" {
		// Do nothing, simply pass through
		return resp, nil
	}

	return t.rewriteResponse(req, resp)
}

var _ = net.RoundTripperWrapper(&Transport{})

func (rt *Transport) WrappedRoundTripper() http.RoundTripper {
	return rt.RoundTripper
}

// rewriteURL rewrites a single URL to go through the proxy, if the URL refers
// to the same host as sourceURL, which is the page on which the target URL
// occurred, or if the URL matches the sourceRequestHost.
func (t *Transport) rewriteURL(url *url.URL, sourceURL *url.URL, sourceRequestHost string) string {
	// Example:
	//      When API server processes a proxy request to a service (e.g. /api/v1/namespace/foo/service/bar/proxy/),
	//      the sourceURL.Host (i.e. req.URL.Host) is the endpoint IP address of the service. The
	//      sourceRequestHost (i.e. req.Host) is the Host header that specifies the host on which the
	//      URL is sought, which can be different from sourceURL.Host. For example, if user sends the
	//      request through "kubectl proxy" locally (i.e. localhost:8001/api/v1/namespace/foo/service/bar/proxy/),
	//      sourceRequestHost is "localhost:8001".
	//
	//      If the service's response URL contains non-empty host, and url.Host is equal to either sourceURL.Host
	//      or sourceRequestHost, we should not consider the returned URL to be a completely different host.
	//      It's the API server's responsibility to rewrite a same-host-and-absolute-path URL and append the
	//      necessary URL prefix (i.e. /api/v1/namespace/foo/service/bar/proxy/).
	isDifferentHost := url.Host != "" && url.Host != sourceURL.Host && url.Host != sourceRequestHost
	isRelative := !strings.HasPrefix(url.Path, "/")
	if isDifferentHost || isRelative {
		return url.String()
	}

	// Do not rewrite scheme and host if the Transport has empty scheme and host
	// when targetURL already contains the sourceRequestHost
	if !(url.Host == sourceRequestHost && t.Scheme == "" && t.Host == "") {
		url.Scheme = t.Scheme
		url.Host = t.Host
	}

	origPath := url.Path
	// Do not rewrite URL if the sourceURL already contains the necessary prefix.
	if strings.HasPrefix(url.Path, t.PathPrepend) {
		return url.String()
	}
	url.Path = path.Join(t.PathPrepend, url.Path)
	if strings.HasSuffix(origPath, "/") {
		// Add back the trailing slash, which was stripped by path.Join().
		url.Path += "/"
	}

	return url.String()
}

// rewriteHTML scans the HTML for tags with url-valued attributes, and updates
// those values with the urlRewriter function. The updated HTML is output to the
// writer.
func rewriteHTML(reader io.Reader, writer io.Writer, urlRewriter func(*url.URL) string) error {
	// Note: This assumes the content is UTF-8.
	tokenizer := html.NewTokenizer(reader)

	var err error
	for err == nil {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			err = tokenizer.Err()
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if urlAttrs, ok := atomsToAttrs[token.DataAtom]; ok {
				for i, attr := range token.Attr {
					if urlAttrs.Has(attr.Key) {
						url, err := url.Parse(attr.Val)
						if err != nil {
							// Do not rewrite the URL if it isn't valid.  It is intended not
							// to error here to prevent the inability to understand the
							// content of the body to cause a fatal error.
							continue
						}
						token.Attr[i].Val = urlRewriter(url)
					}
				}
			}
			_, err = writer.Write([]byte(token.String()))
		default:
			_, err = writer.Write(tokenizer.Raw())
		}
	}
	if err != io.EOF {
		return err
	}
	return nil
}

// rewriteResponse modifies an HTML response by updating absolute links referring
// to the original host to instead refer to the proxy transport.
func (t *Transport) rewriteResponse(req *http.Request, resp *http.Response) (*http.Response, error) {
	origBody := resp.Body
	defer origBody.Close()

	newContent := &bytes.Buffer{}
	var reader io.Reader = origBody
	var writer io.Writer = newContent
	encoding := resp.Header.Get("Content-Encoding")
	switch encoding {
	case "gzip":
		var err error
		reader, err = gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("errorf making gzip reader: %v", err)
		}
		gzw := gzip.NewWriter(writer)
		defer gzw.Close()
		writer = gzw
	case "deflate":
		var err error
		reader = flate.NewReader(reader)
		flw, err := flate.NewWriter(writer, flate.BestCompression)
		if err != nil {
			return nil, fmt.Errorf("errorf making flate writer: %v", err)
		}
		defer func() {
			flw.Close()
			flw.Flush()
		}()
		writer = flw
	case "":
		// This is fine
	default:
		// Some encoding we don't understand-- don't try to parse this
		klog.FromContext(req.Context()).Error(nil, "Proxy encountered unknown encoding for text/html, can't understand this so not fixing links", "encoding", encoding)
		return resp, nil
	}

	urlRewriter := func(targetUrl *url.URL) string {
		return t.rewriteURL(targetUrl, req.URL, req.Host)
	}
	err := rewriteHTML(reader, writer, urlRewriter)
	if err != nil {
		klog.FromContext(req.Context()).Error(err, "Failed to rewrite URLs")
		return resp, err
	}

	resp.Body = io.NopCloser(newContent)
	// Update header node with new content-length
	// TODO: Remove any hash/signature headers here?
	resp.Header.Del("Content-Length")
	resp.ContentLength = int64(newContent.Len())

	return resp, err
}
