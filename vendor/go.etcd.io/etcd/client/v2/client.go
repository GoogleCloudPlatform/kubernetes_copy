// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.etcd.io/etcd/api/v3/version"
)

var (
	ErrNoEndpoints           = errors.New("client: no endpoints available")
	ErrTooManyRedirects      = errors.New("client: too many redirects")
	ErrClusterUnavailable    = errors.New("client: etcd cluster is unavailable or misconfigured")
	ErrNoLeaderEndpoint      = errors.New("client: no leader endpoint available")
	errTooManyRedirectChecks = errors.New("client: too many redirect checks")

	// oneShotCtxValue is set on a context using WithValue(&oneShotValue) so
	// that Do() will not retry a request
	oneShotCtxValue interface{}
)

var DefaultRequestTimeout = 5 * time.Second

var DefaultTransport CancelableTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	TLSHandshakeTimeout: 10 * time.Second,
}

type EndpointSelectionMode int

const (
	// EndpointSelectionRandom is the default value of the 'SelectionMode'.
	// As the name implies, the client object will pick a node from the members
	// of the cluster in a random fashion. If the cluster has three members, A, B,
	// and C, the client picks any node from its three members as its request
	// destination.
	EndpointSelectionRandom EndpointSelectionMode = iota

	// If 'SelectionMode' is set to 'EndpointSelectionPrioritizeLeader',
	// requests are sent directly to the cluster leader. This reduces
	// forwarding roundtrips compared to making requests to etcd followers
	// who then forward them to the cluster leader. In the event of a leader
	// failure, however, clients configured this way cannot prioritize among
	// the remaining etcd followers. Therefore, when a client sets 'SelectionMode'
	// to 'EndpointSelectionPrioritizeLeader', it must use 'client.AutoSync()' to
	// maintain its knowledge of current cluster state.
	//
	// This mode should be used with Client.AutoSync().
	EndpointSelectionPrioritizeLeader
)

type Config struct {
	// Endpoints defines a set of URLs (schemes, hosts and ports only)
	// that can be used to communicate with a logical etcd cluster. For
	// example, a three-node cluster could be provided like so:
	//
	// 	Endpoints: []string{
	//		"http://node1.example.com:2379",
	//		"http://node2.example.com:2379",
	//		"http://node3.example.com:2379",
	//	}
	//
	// If multiple endpoints are provided, the Client will attempt to
	// use them all in the event that one or more of them are unusable.
	//
	// If Client.Sync is ever called, the Client may cache an alternate
	// set of endpoints to continue operation.
	Endpoints []string

	// Transport is used by the Client to drive HTTP requests. If not
	// provided, DefaultTransport will be used.
	Transport CancelableTransport

	// CheckRedirect specifies the policy for handling HTTP redirects.
	// If CheckRedirect is not nil, the Client calls it before
	// following an HTTP redirect. The sole argument is the number of
	// requests that have already been made. If CheckRedirect returns
	// an error, Client.Do will not make any further requests and return
	// the error back it to the caller.
	//
	// If CheckRedirect is nil, the Client uses its default policy,
	// which is to stop after 10 consecutive requests.
	CheckRedirect CheckRedirectFunc

	// Username specifies the user credential to add as an authorization header
	Username string

	// Password is the password for the specified user to add as an authorization header
	// to the request.
	Password string

	// HeaderTimeoutPerRequest specifies the time limit to wait for response
	// header in a single request made by the Client. The timeout includes
	// connection time, any redirects, and header wait time.
	//
	// For non-watch GET request, server returns the response body immediately.
	// For PUT/POST/DELETE request, server will attempt to commit request
	// before responding, which is expected to take `100ms + 2 * RTT`.
	// For watch request, server returns the header immediately to notify Client
	// watch start. But if server is behind some kind of proxy, the response
	// header may be cached at proxy, and Client cannot rely on this behavior.
	//
	// Especially, wait request will ignore this timeout.
	//
	// One API call may send multiple requests to different etcd servers until it
	// succeeds. Use context of the API to specify the overall timeout.
	//
	// A HeaderTimeoutPerRequest of zero means no timeout.
	HeaderTimeoutPerRequest time.Duration

	// SelectionMode is an EndpointSelectionMode enum that specifies the
	// policy for choosing the etcd cluster node to which requests are sent.
	SelectionMode EndpointSelectionMode
}

func (cfg *Config) transport() CancelableTransport {
	if cfg.Transport == nil {
		return DefaultTransport
	}
	return cfg.Transport
}

func (cfg *Config) checkRedirect() CheckRedirectFunc {
	if cfg.CheckRedirect == nil {
		return DefaultCheckRedirect
	}
	return cfg.CheckRedirect
}

// CancelableTransport mimics net/http.Transport, but requires that
// the object also support request cancellation.
type CancelableTransport interface {
	http.RoundTripper
	CancelRequest(req *http.Request)
}

type CheckRedirectFunc func(via int) error

// DefaultCheckRedirect follows up to 10 redirects, but no more.
var DefaultCheckRedirect CheckRedirectFunc = func(via int) error {
	if via > 10 {
		return ErrTooManyRedirects
	}
	return nil
}

type Client interface {
	// Sync updates the internal cache of the etcd cluster's membership.
	Sync(context.Context) error

	// AutoSync periodically calls Sync() every given interval.
	// The recommended sync interval is 10 seconds to 1 minute, which does
	// not bring too much overhead to server and makes client catch up the
	// cluster change in time.
	//
	// The example to use it:
	//
	//  for {
	//      err := client.AutoSync(ctx, 10*time.Second)
	//      if err == context.DeadlineExceeded || err == context.Canceled {
	//          break
	//      }
	//      log.Print(err)
	//  }
	AutoSync(context.Context, time.Duration) error

	// Endpoints returns a copy of the current set of API endpoints used
	// by Client to resolve HTTP requests. If Sync has ever been called,
	// this may differ from the initial Endpoints provided in the Config.
	Endpoints() []string

	// SetEndpoints sets the set of API endpoints used by Client to resolve
	// HTTP requests. If the given endpoints are not valid, an error will be
	// returned
	SetEndpoints(eps []string) error

	// GetVersion retrieves the current etcd server and cluster version
	GetVersion(ctx context.Context) (*version.Versions, error)

	httpClient
}

func New(cfg Config) (Client, error) {
	c := &httpClusterClient{
		clientFactory: newHTTPClientFactory(cfg.transport(), cfg.checkRedirect(), cfg.HeaderTimeoutPerRequest),
		rand:          rand.New(rand.NewSource(int64(time.Now().Nanosecond()))),
		selectionMode: cfg.SelectionMode,
	}
	if cfg.Username != "" {
		c.credentials = &credentials{
			username: cfg.Username,
			password: cfg.Password,
		}
	}
	if err := c.SetEndpoints(cfg.Endpoints); err != nil {
		return nil, err
	}
	return c, nil
}

type httpClient interface {
	Do(context.Context, httpAction) (*http.Response, []byte, error)
}

func newHTTPClientFactory(tr CancelableTransport, cr CheckRedirectFunc, headerTimeout time.Duration) httpClientFactory {
	return func(ep url.URL) httpClient {
		return &redirectFollowingHTTPClient{
			checkRedirect: cr,
			client: &simpleHTTPClient{
				transport:     tr,
				endpoint:      ep,
				headerTimeout: headerTimeout,
			},
		}
	}
}

type credentials struct {
	username string
	password string
}

type httpClientFactory func(url.URL) httpClient

type httpAction interface {
	HTTPRequest(url.URL) *http.Request
}

type httpClusterClient struct {
	clientFactory httpClientFactory
	endpoints     []url.URL
	pinned        int
	credentials   *credentials
	sync.RWMutex
	rand          *rand.Rand
	selectionMode EndpointSelectionMode
}

func (c *httpClusterClient) getLeaderEndpoint(ctx context.Context, eps []url.URL) (string, error) {
	ceps := make([]url.URL, len(eps))
	copy(ceps, eps)

	// To perform a lookup on the new endpoint list without using the current
	// client, we'll copy it
	clientCopy := &httpClusterClient{
		clientFactory: c.clientFactory,
		credentials:   c.credentials,
		rand:          c.rand,

		pinned:    0,
		endpoints: ceps,
	}

	mAPI := NewMembersAPI(clientCopy)
	leader, err := mAPI.Leader(ctx)
	if err != nil {
		return "", err
	}
	if len(leader.ClientURLs) == 0 {
		return "", ErrNoLeaderEndpoint
	}

	return leader.ClientURLs[0], nil // TODO: how to handle multiple client URLs?
}

func (c *httpClusterClient) parseEndpoints(eps []string) ([]url.URL, error) {
	if len(eps) == 0 {
		return []url.URL{}, ErrNoEndpoints
	}

	neps := make([]url.URL, len(eps))
	for i, ep := range eps {
		u, err := url.Parse(ep)
		if err != nil {
			return []url.URL{}, err
		}
		neps[i] = *u
	}
	return neps, nil
}

func (c *httpClusterClient) SetEndpoints(eps []string) error {
	neps, err := c.parseEndpoints(eps)
	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	c.endpoints = shuffleEndpoints(c.rand, neps)
	// We're not doing anything for PrioritizeLeader here. This is
	// due to not having a context meaning we can't call getLeaderEndpoint
	// However, if you're using PrioritizeLeader, you've already been told
	// to regularly call sync, where we do have a ctx, and can figure the
	// leader. PrioritizeLeader is also quite a loose guarantee, so deal
	// with it
	c.pinned = 0

	return nil
}

func (c *httpClusterClient) Do(ctx context.Context, act httpAction) (*http.Response, []byte, error) {
	action := act
	c.RLock()
	leps := len(c.endpoints)
	eps := make([]url.URL, leps)
	n := copy(eps, c.endpoints)
	pinned := c.pinned

	if c.credentials != nil {
		action = &authedAction{
			act:         act,
			credentials: *c.credentials,
		}
	}
	c.RUnlock()

	if leps == 0 {
		return nil, nil, ErrNoEndpoints
	}

	if leps != n {
		return nil, nil, errors.New("unable to pick endpoint: copy failed")
	}

	var resp *http.Response
	var body []byte
	var err error
	cerr := &ClusterError{}
	isOneShot := ctx.Value(&oneShotCtxValue) != nil

	for i := pinned; i < leps+pinned; i++ {
		k := i % leps
		hc := c.clientFactory(eps[k])
		resp, body, err = hc.Do(ctx, action)
		if err != nil {
			cerr.Errors = append(cerr.Errors, err)
			if err == ctx.Err() {
				return nil, nil, ctx.Err()
			}
			if err == context.Canceled || err == context.DeadlineExceeded {
				return nil, nil, err
			}
		} else if resp.StatusCode/100 == 5 {
			switch resp.StatusCode {
			case http.StatusInternalServerError, http.StatusServiceUnavailable:
				// TODO: make sure this is a no leader response
				cerr.Errors = append(cerr.Errors, fmt.Errorf("client: etcd member %s has no leader", eps[k].String()))
			default:
				cerr.Errors = append(cerr.Errors, fmt.Errorf("client: etcd member %s returns server error [%s]", eps[k].String(), http.StatusText(resp.StatusCode)))
			}
			err = cerr.Errors[0]
		}
		if err != nil {
			if !isOneShot {
				continue
			}
			c.Lock()
			c.pinned = (k + 1) % leps
			c.Unlock()
			return nil, nil, err
		}
		if k != pinned {
			c.Lock()
			c.pinned = k
			c.Unlock()
		}
		return resp, body, nil
	}

	return nil, nil, cerr
}

func (c *httpClusterClient) Endpoints() []string {
	c.RLock()
	defer c.RUnlock()

	eps := make([]string, len(c.endpoints))
	for i, ep := range c.endpoints {
		eps[i] = ep.String()
	}

	return eps
}

func (c *httpClusterClient) Sync(ctx context.Context) error {
	mAPI := NewMembersAPI(c)
	ms, err := mAPI.List(ctx)
	if err != nil {
		return err
	}

	var eps []string
	for _, m := range ms {
		eps = append(eps, m.ClientURLs...)
	}

	neps, err := c.parseEndpoints(eps)
	if err != nil {
		return err
	}

	npin := 0

	switch c.selectionMode {
	case EndpointSelectionRandom:
		c.RLock()
		eq := endpointsEqual(c.endpoints, neps)
		c.RUnlock()

		if eq {
			return nil
		}
		// When items in the endpoint list changes, we choose a new pin
		neps = shuffleEndpoints(c.rand, neps)
	case EndpointSelectionPrioritizeLeader:
		nle, err := c.getLeaderEndpoint(ctx, neps)
		if err != nil {
			return ErrNoLeaderEndpoint
		}

		for i, n := range neps {
			if n.String() == nle {
				npin = i
				break
			}
		}
	default:
		return fmt.Errorf("invalid endpoint selection mode: %d", c.selectionMode)
	}

	c.Lock()
	defer c.Unlock()
	c.endpoints = neps
	c.pinned = npin

	return nil
}

func (c *httpClusterClient) AutoSync(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		err := c.Sync(ctx)
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *httpClusterClient) GetVersion(ctx context.Context) (*version.Versions, error) {
	act := &getAction{Prefix: "/version"}

	resp, body, err := c.Do(ctx, act)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if len(body) == 0 {
			return nil, ErrEmptyBody
		}
		var vresp version.Versions
		if err := json.Unmarshal(body, &vresp); err != nil {
			return nil, ErrInvalidJSON
		}
		return &vresp, nil
	default:
		var etcdErr Error
		if err := json.Unmarshal(body, &etcdErr); err != nil {
			return nil, ErrInvalidJSON
		}
		return nil, etcdErr
	}
}

type roundTripResponse struct {
	resp *http.Response
	err  error
}

type simpleHTTPClient struct {
	transport     CancelableTransport
	endpoint      url.URL
	headerTimeout time.Duration
}

// ErrNoRequest indicates that the HTTPRequest object could not be found
// or was nil.  No processing could continue.
var ErrNoRequest = errors.New("no HTTPRequest was available")

func (c *simpleHTTPClient) Do(ctx context.Context, act httpAction) (*http.Response, []byte, error) {
	req := act.HTTPRequest(c.endpoint)
	if req == nil {
		return nil, nil, ErrNoRequest
	}

	if err := printcURL(req); err != nil {
		return nil, nil, err
	}

	isWait := false
	if req.URL != nil {
		ws := req.URL.Query().Get("wait")
		if len(ws) != 0 {
			var err error
			isWait, err = strconv.ParseBool(ws)
			if err != nil {
				return nil, nil, fmt.Errorf("wrong wait value %s (%v for %+v)", ws, err, req)
			}
		}
	}

	var hctx context.Context
	var hcancel context.CancelFunc
	if !isWait && c.headerTimeout > 0 {
		hctx, hcancel = context.WithTimeout(ctx, c.headerTimeout)
	} else {
		hctx, hcancel = context.WithCancel(ctx)
	}
	defer hcancel()

	reqcancel := requestCanceler(c.transport, req)

	rtchan := make(chan roundTripResponse, 1)
	go func() {
		resp, err := c.transport.RoundTrip(req)
		rtchan <- roundTripResponse{resp: resp, err: err}
		close(rtchan)
	}()

	var resp *http.Response
	var err error

	select {
	case rtresp := <-rtchan:
		resp, err = rtresp.resp, rtresp.err
	case <-hctx.Done():
		// cancel and wait for request to actually exit before continuing
		reqcancel()
		rtresp := <-rtchan
		resp = rtresp.resp
		switch {
		case ctx.Err() != nil:
			err = ctx.Err()
		case hctx.Err() != nil:
			err = fmt.Errorf("client: endpoint %s exceeded header timeout", c.endpoint.String())
		default:
			panic("failed to get error from context")
		}
	}

	// always check for resp nil-ness to deal with possible
	// race conditions between channels above
	defer func() {
		if resp != nil {
			resp.Body.Close()
		}
	}()

	if err != nil {
		return nil, nil, err
	}

	var body []byte
	done := make(chan struct{})
	go func() {
		body, err = io.ReadAll(resp.Body)
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		if resp != nil {
			resp.Body.Close()
		}
		<-done
		return nil, nil, ctx.Err()
	case <-done:
	}

	return resp, body, err
}

type authedAction struct {
	act         httpAction
	credentials credentials
}

func (a *authedAction) HTTPRequest(url url.URL) *http.Request {
	r := a.act.HTTPRequest(url)
	r.SetBasicAuth(a.credentials.username, a.credentials.password)
	return r
}

type redirectFollowingHTTPClient struct {
	client        httpClient
	checkRedirect CheckRedirectFunc
}

func (r *redirectFollowingHTTPClient) Do(ctx context.Context, act httpAction) (*http.Response, []byte, error) {
	next := act
	for i := 0; i < 100; i++ {
		if i > 0 {
			if err := r.checkRedirect(i); err != nil {
				return nil, nil, err
			}
		}
		resp, body, err := r.client.Do(ctx, next)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode/100 == 3 {
			hdr := resp.Header.Get("Location")
			if hdr == "" {
				return nil, nil, fmt.Errorf("location header not set")
			}
			loc, err := url.Parse(hdr)
			if err != nil {
				return nil, nil, fmt.Errorf("location header not valid URL: %s", hdr)
			}
			next = &redirectedHTTPAction{
				action:   act,
				location: *loc,
			}
			continue
		}
		return resp, body, nil
	}

	return nil, nil, errTooManyRedirectChecks
}

type redirectedHTTPAction struct {
	action   httpAction
	location url.URL
}

func (r *redirectedHTTPAction) HTTPRequest(ep url.URL) *http.Request {
	orig := r.action.HTTPRequest(ep)
	orig.URL = &r.location
	return orig
}

func shuffleEndpoints(r *rand.Rand, eps []url.URL) []url.URL {
	// copied from Go 1.9<= rand.Rand.Perm
	n := len(eps)
	p := make([]int, n)
	for i := 0; i < n; i++ {
		j := r.Intn(i + 1)
		p[i] = p[j]
		p[j] = i
	}
	neps := make([]url.URL, n)
	for i, k := range p {
		neps[i] = eps[k]
	}
	return neps
}

func endpointsEqual(left, right []url.URL) bool {
	if len(left) != len(right) {
		return false
	}

	sLeft := make([]string, len(left))
	sRight := make([]string, len(right))
	for i, l := range left {
		sLeft[i] = l.String()
	}
	for i, r := range right {
		sRight[i] = r.String()
	}

	sort.Strings(sLeft)
	sort.Strings(sRight)
	for i := range sLeft {
		if sLeft[i] != sRight[i] {
			return false
		}
	}
	return true
}
