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

// Package srv looks up DNS SRV records.
package srv

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"go.etcd.io/etcd/client/pkg/v3/types"
)

var (
	// indirection for testing
	lookupSRV      = net.LookupSRV // net.DefaultResolver.LookupSRV when ctxs don't conflict
	resolveTCPAddr = net.ResolveTCPAddr
)

// GetCluster gets the cluster information via DNS discovery.
// Also sees each entry as a separate instance.
func GetCluster(serviceScheme, service, name, dns string, apurls types.URLs) ([]string, error) {
	tcp2ap := make(map[string]url.URL)

	// First, resolve the apurls
	for _, url := range apurls {
		tcpAddr, err := resolveTCPAddr("tcp", url.Host)
		if err != nil {
			return nil, err
		}
		tcp2ap[tcpAddr.String()] = url
	}

	var (
		tempName    int
		stringParts []string
	)
	updateNodeMap := func(service, scheme string) error {
		_, addrs, err := lookupSRV(service, "tcp", dns)
		if err != nil {
			return err
		}
		for _, srv := range addrs {
			port := fmt.Sprintf("%d", srv.Port)
			host := net.JoinHostPort(srv.Target, port)
			tcpAddr, terr := resolveTCPAddr("tcp", host)
			if terr != nil {
				err = terr
				continue
			}
			n := ""
			url, ok := tcp2ap[tcpAddr.String()]
			if ok {
				n = name
			}
			if n == "" {
				n = fmt.Sprintf("%d", tempName)
				tempName++
			}
			// SRV records have a trailing dot but URL shouldn't.
			shortHost := strings.TrimSuffix(srv.Target, ".")
			urlHost := net.JoinHostPort(shortHost, port)
			if ok && url.Scheme != scheme {
				err = fmt.Errorf("bootstrap at %s from DNS for %s has scheme mismatch with expected peer %s", scheme+"://"+urlHost, service, url.String())
			} else {
				stringParts = append(stringParts, fmt.Sprintf("%s=%s://%s", n, scheme, urlHost))
			}
		}
		if len(stringParts) == 0 {
			return err
		}
		return nil
	}

	err := updateNodeMap(service, serviceScheme)
	if err != nil {
		return nil, fmt.Errorf("error querying DNS SRV records for _%s %w", service, err)
	}
	return stringParts, nil
}

type SRVClients struct {
	Endpoints []string
	SRVs      []*net.SRV
}

// GetClient looks up the client endpoints for a service and domain.
func GetClient(service, domain string, serviceName string) (*SRVClients, error) {
	var (
		urls []*url.URL
		srvs []*net.SRV
	)

	updateURLs := func(service, scheme string) error {
		_, addrs, err := lookupSRV(service, "tcp", domain)
		if err != nil {
			return err
		}
		for _, srv := range addrs {
			urls = append(urls, &url.URL{
				Scheme: scheme,
				Host:   net.JoinHostPort(srv.Target, fmt.Sprintf("%d", srv.Port)),
			})
		}
		srvs = append(srvs, addrs...)
		return nil
	}

	errHTTPS := updateURLs(GetSRVService(service, serviceName, "https"), "https")
	errHTTP := updateURLs(GetSRVService(service, serviceName, "http"), "http")

	if errHTTPS != nil && errHTTP != nil {
		return nil, fmt.Errorf("dns lookup errors: %w and %w", errHTTPS, errHTTP)
	}

	endpoints := make([]string, len(urls))
	for i := range urls {
		endpoints[i] = urls[i].String()
	}
	return &SRVClients{Endpoints: endpoints, SRVs: srvs}, nil
}

// GetSRVService generates a SRV service including an optional suffix.
func GetSRVService(service, serviceName string, scheme string) (SRVService string) {
	if scheme == "https" {
		service = fmt.Sprintf("%s-ssl", service)
	}

	if serviceName != "" {
		return fmt.Sprintf("%s-%s", service, serviceName)
	}
	return service
}
