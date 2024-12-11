// Copyright 2016 The etcd Authors
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

package leasehttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/pkg/v3/httputil"
	"go.etcd.io/etcd/server/v3/lease"
	"go.etcd.io/etcd/server/v3/lease/leasepb"
)

var (
	LeasePrefix         = "/leases"
	LeaseInternalPrefix = "/leases/internal"
	applyTimeout        = time.Second
	ErrLeaseHTTPTimeout = errors.New("waiting for node to catch up its applied index has timed out")
)

// NewHandler returns an http Handler for lease renewals
func NewHandler(l lease.Lessor, waitch func() <-chan struct{}) http.Handler {
	return &leaseHandler{l, waitch}
}

type leaseHandler struct {
	l      lease.Lessor
	waitch func() <-chan struct{}
}

func (h *leaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "error reading body", http.StatusBadRequest)
		return
	}

	var v []byte
	switch r.URL.Path {
	case LeasePrefix:
		lreq := pb.LeaseKeepAliveRequest{}
		if uerr := lreq.Unmarshal(b); uerr != nil {
			http.Error(w, "error unmarshalling request", http.StatusBadRequest)
			return
		}
		select {
		case <-h.waitch():
		case <-time.After(applyTimeout):
			http.Error(w, ErrLeaseHTTPTimeout.Error(), http.StatusRequestTimeout)
			return
		}
		ttl, rerr := h.l.Renew(lease.LeaseID(lreq.ID))
		if rerr != nil {
			if errors.Is(rerr, lease.ErrLeaseNotFound) {
				http.Error(w, rerr.Error(), http.StatusNotFound)
				return
			}

			http.Error(w, rerr.Error(), http.StatusBadRequest)
			return
		}
		// TODO: fill out ResponseHeader
		resp := &pb.LeaseKeepAliveResponse{ID: lreq.ID, TTL: ttl}
		v, err = resp.Marshal()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

	case LeaseInternalPrefix:
		lreq := leasepb.LeaseInternalRequest{}
		if lerr := lreq.Unmarshal(b); lerr != nil {
			http.Error(w, "error unmarshalling request", http.StatusBadRequest)
			return
		}
		select {
		case <-h.waitch():
		case <-time.After(applyTimeout):
			http.Error(w, ErrLeaseHTTPTimeout.Error(), http.StatusRequestTimeout)
			return
		}

		// gofail: var beforeLookupWhenForwardLeaseTimeToLive struct{}

		l := h.l.Lookup(lease.LeaseID(lreq.LeaseTimeToLiveRequest.ID))
		if l == nil {
			http.Error(w, lease.ErrLeaseNotFound.Error(), http.StatusNotFound)
			return
		}
		// TODO: fill out ResponseHeader
		resp := &leasepb.LeaseInternalResponse{
			LeaseTimeToLiveResponse: &pb.LeaseTimeToLiveResponse{
				Header:     &pb.ResponseHeader{},
				ID:         lreq.LeaseTimeToLiveRequest.ID,
				TTL:        int64(l.Remaining().Seconds()),
				GrantedTTL: l.TTL(),
			},
		}
		if lreq.LeaseTimeToLiveRequest.Keys {
			ks := l.Keys()
			kbs := make([][]byte, len(ks))
			for i := range ks {
				kbs[i] = []byte(ks[i])
			}
			resp.LeaseTimeToLiveResponse.Keys = kbs
		}

		// The leasor could be demoted if leader changed during lookup.
		// We should return error to force retry instead of returning
		// incorrect remaining TTL.
		if l.Demoted() {
			http.Error(w, lease.ErrNotPrimary.Error(), http.StatusInternalServerError)
			return
		}

		v, err = resp.Marshal()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

	default:
		http.Error(w, fmt.Sprintf("unknown request path %q", r.URL.Path), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/protobuf")
	w.Write(v)
}

// RenewHTTP renews a lease at a given primary server.
// TODO: Batch request in future?
func RenewHTTP(ctx context.Context, id lease.LeaseID, url string, rt http.RoundTripper) (int64, error) {
	// will post lreq protobuf to leader
	lreq, err := (&pb.LeaseKeepAliveRequest{ID: int64(id)}).Marshal()
	if err != nil {
		return -1, err
	}

	cc := &http.Client{
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(lreq))
	if err != nil {
		return -1, err
	}
	req.Header.Set("Content-Type", "application/protobuf")
	req.Cancel = ctx.Done()

	resp, err := cc.Do(req)
	if err != nil {
		return -1, err
	}
	b, err := readResponse(resp)
	if err != nil {
		return -1, err
	}

	if resp.StatusCode == http.StatusRequestTimeout {
		return -1, ErrLeaseHTTPTimeout
	}

	if resp.StatusCode == http.StatusNotFound {
		return -1, lease.ErrLeaseNotFound
	}

	if resp.StatusCode != http.StatusOK {
		return -1, fmt.Errorf("lease: unknown error(%s)", b)
	}

	lresp := &pb.LeaseKeepAliveResponse{}
	if err := lresp.Unmarshal(b); err != nil {
		return -1, fmt.Errorf(`lease: %w. data = "%s"`, err, b)
	}
	if lresp.ID != int64(id) {
		return -1, fmt.Errorf("lease: renew id mismatch")
	}
	return lresp.TTL, nil
}

// TimeToLiveHTTP retrieves lease information of the given lease ID.
func TimeToLiveHTTP(ctx context.Context, id lease.LeaseID, keys bool, url string, rt http.RoundTripper) (*leasepb.LeaseInternalResponse, error) {
	// will post lreq protobuf to leader
	lreq, err := (&leasepb.LeaseInternalRequest{
		LeaseTimeToLiveRequest: &pb.LeaseTimeToLiveRequest{
			ID:   int64(id),
			Keys: keys,
		},
	}).Marshal()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(lreq))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/protobuf")

	req = req.WithContext(ctx)

	cc := &http.Client{
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var b []byte
	// buffer errc channel so that errc don't block inside the go routinue
	resp, err := cc.Do(req)
	if err != nil {
		return nil, err
	}
	b, err = readResponse(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusRequestTimeout {
		return nil, ErrLeaseHTTPTimeout
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, lease.ErrLeaseNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lease: unknown error(%s)", string(b))
	}

	lresp := &leasepb.LeaseInternalResponse{}
	if err := lresp.Unmarshal(b); err != nil {
		return nil, fmt.Errorf(`lease: %w. data = "%s"`, err, string(b))
	}
	if lresp.LeaseTimeToLiveResponse.ID != int64(id) {
		return nil, fmt.Errorf("lease: TTL id mismatch")
	}
	return lresp, nil
}

func readResponse(resp *http.Response) (b []byte, err error) {
	b, err = io.ReadAll(resp.Body)
	httputil.GracefulClose(resp)
	return
}
