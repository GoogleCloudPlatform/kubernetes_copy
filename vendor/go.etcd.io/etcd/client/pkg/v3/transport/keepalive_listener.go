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

package transport

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"
)

// NewKeepAliveListener returns a listener that listens on the given address.
// Be careful when wrap around KeepAliveListener with another Listener if TLSInfo is not nil.
// Some pkgs (like go/http) might expect Listener to return TLSConn type to start TLS handshake.
// http://tldp.org/HOWTO/TCP-Keepalive-HOWTO/overview.html
//
// Note(ahrtr):
// only `net.TCPConn` supports `SetKeepAlive` and `SetKeepAlivePeriod`
// by default, so if you want to wrap multiple layers of net.Listener,
// the `keepaliveListener` should be the one which is closest to the
// original `net.Listener` implementation, namely `TCPListener`.
func NewKeepAliveListener(l net.Listener, scheme string, tlscfg *tls.Config) (net.Listener, error) {
	kal := &keepaliveListener{
		Listener: l,
	}

	if scheme == "https" {
		if tlscfg == nil {
			return nil, errors.New("cannot listen on TLS for given listener: KeyFile and CertFile are not presented")
		}
		return newTLSKeepaliveListener(kal, tlscfg), nil
	}

	return kal, nil
}

type keepaliveListener struct{ net.Listener }

func (kln *keepaliveListener) Accept() (net.Conn, error) {
	c, err := kln.Listener.Accept()
	if err != nil {
		return nil, err
	}

	kac, err := createKeepaliveConn(c)
	if err != nil {
		return nil, fmt.Errorf("create keepalive connection failed, %w", err)
	}
	// detection time: tcp_keepalive_time + tcp_keepalive_probes + tcp_keepalive_intvl
	// default on linux:  30 + 8 * 30
	// default on osx:    30 + 8 * 75
	if err := kac.SetKeepAlive(true); err != nil {
		return nil, fmt.Errorf("SetKeepAlive failed, %w", err)
	}
	if err := kac.SetKeepAlivePeriod(30 * time.Second); err != nil {
		return nil, fmt.Errorf("SetKeepAlivePeriod failed, %w", err)
	}
	return kac, nil
}

func createKeepaliveConn(c net.Conn) (*keepAliveConn, error) {
	tcpc, ok := c.(*net.TCPConn)
	if !ok {
		return nil, ErrNotTCP
	}
	return &keepAliveConn{tcpc}, nil
}

type keepAliveConn struct {
	*net.TCPConn
}

// SetKeepAlive sets keepalive
func (l *keepAliveConn) SetKeepAlive(doKeepAlive bool) error {
	return l.TCPConn.SetKeepAlive(doKeepAlive)
}

// A tlsKeepaliveListener implements a network listener (net.Listener) for TLS connections.
type tlsKeepaliveListener struct {
	net.Listener
	config *tls.Config
}

// Accept waits for and returns the next incoming TLS connection.
// The returned connection c is a *tls.Conn.
func (l *tlsKeepaliveListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	c = tls.Server(c, l.config)
	return c, nil
}

// newTLSKeepaliveListener creates a Listener which accepts connections from an inner
// Listener and wraps each connection with Server.
// The configuration config must be non-nil and must have
// at least one certificate.
func newTLSKeepaliveListener(inner net.Listener, config *tls.Config) net.Listener {
	l := &tlsKeepaliveListener{}
	l.Listener = inner
	l.config = config
	return l
}
