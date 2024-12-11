// Copyright 2017 The etcd Authors
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
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
)

// tlsListener overrides a TLS listener so it will reject client
// certificates with insufficient SAN credentials or CRL revoked
// certificates.
type tlsListener struct {
	net.Listener
	connc            chan net.Conn
	donec            chan struct{}
	err              error
	handshakeFailure func(*tls.Conn, error)
	check            tlsCheckFunc
}

type tlsCheckFunc func(context.Context, *tls.Conn) error

// NewTLSListener handshakes TLS connections and performs optional CRL checking.
func NewTLSListener(l net.Listener, tlsinfo *TLSInfo) (net.Listener, error) {
	check := func(context.Context, *tls.Conn) error { return nil }
	return newTLSListener(l, tlsinfo, check)
}

func newTLSListener(l net.Listener, tlsinfo *TLSInfo, check tlsCheckFunc) (net.Listener, error) {
	if tlsinfo == nil || tlsinfo.Empty() {
		l.Close()
		return nil, fmt.Errorf("cannot listen on TLS for %s: KeyFile and CertFile are not presented", l.Addr().String())
	}
	tlscfg, err := tlsinfo.ServerConfig()
	if err != nil {
		return nil, err
	}

	hf := tlsinfo.HandshakeFailure
	if hf == nil {
		hf = func(*tls.Conn, error) {}
	}

	if len(tlsinfo.CRLFile) > 0 {
		prevCheck := check
		check = func(ctx context.Context, tlsConn *tls.Conn) error {
			if err := prevCheck(ctx, tlsConn); err != nil {
				return err
			}
			st := tlsConn.ConnectionState()
			if certs := st.PeerCertificates; len(certs) > 0 {
				return checkCRL(tlsinfo.CRLFile, certs)
			}
			return nil
		}
	}

	tlsl := &tlsListener{
		Listener:         tls.NewListener(l, tlscfg),
		connc:            make(chan net.Conn),
		donec:            make(chan struct{}),
		handshakeFailure: hf,
		check:            check,
	}
	go tlsl.acceptLoop()
	return tlsl, nil
}

func (l *tlsListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connc:
		return conn, nil
	case <-l.donec:
		return nil, l.err
	}
}

func checkSAN(ctx context.Context, tlsConn *tls.Conn) error {
	st := tlsConn.ConnectionState()
	if certs := st.PeerCertificates; len(certs) > 0 {
		addr := tlsConn.RemoteAddr().String()
		return checkCertSAN(ctx, certs[0], addr)
	}
	return nil
}

// acceptLoop launches each TLS handshake in a separate goroutine
// to prevent a hanging TLS connection from blocking other connections.
func (l *tlsListener) acceptLoop() {
	var wg sync.WaitGroup
	var pendingMu sync.Mutex

	pending := make(map[net.Conn]struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		pendingMu.Lock()
		for c := range pending {
			c.Close()
		}
		pendingMu.Unlock()
		wg.Wait()
		close(l.donec)
	}()

	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			l.err = err
			return
		}

		pendingMu.Lock()
		pending[conn] = struct{}{}
		pendingMu.Unlock()

		wg.Add(1)
		go func() {
			defer func() {
				if conn != nil {
					conn.Close()
				}
				wg.Done()
			}()

			tlsConn := conn.(*tls.Conn)
			herr := tlsConn.Handshake()
			pendingMu.Lock()
			delete(pending, conn)
			pendingMu.Unlock()

			if herr != nil {
				l.handshakeFailure(tlsConn, herr)
				return
			}
			if err := l.check(ctx, tlsConn); err != nil {
				l.handshakeFailure(tlsConn, err)
				return
			}

			select {
			case l.connc <- tlsConn:
				conn = nil
			case <-ctx.Done():
			}
		}()
	}
}

func checkCRL(crlPath string, cert []*x509.Certificate) error {
	// TODO: cache
	crlBytes, err := os.ReadFile(crlPath)
	if err != nil {
		return err
	}
	certList, err := x509.ParseRevocationList(crlBytes)
	if err != nil {
		return err
	}
	revokedSerials := make(map[string]struct{})
	for _, rc := range certList.RevokedCertificateEntries {
		revokedSerials[string(rc.SerialNumber.Bytes())] = struct{}{}
	}
	for _, c := range cert {
		serial := string(c.SerialNumber.Bytes())
		if _, ok := revokedSerials[serial]; ok {
			return fmt.Errorf("transport: certificate serial %x revoked", serial)
		}
	}
	return nil
}

func checkCertSAN(ctx context.Context, cert *x509.Certificate, remoteAddr string) error {
	if len(cert.IPAddresses) == 0 && len(cert.DNSNames) == 0 {
		return nil
	}
	h, _, herr := net.SplitHostPort(remoteAddr)
	if herr != nil {
		return herr
	}
	if len(cert.IPAddresses) > 0 {
		cerr := cert.VerifyHostname(h)
		if cerr == nil {
			return nil
		}
		if len(cert.DNSNames) == 0 {
			return cerr
		}
	}
	if len(cert.DNSNames) > 0 {
		ok, err := isHostInDNS(ctx, h, cert.DNSNames)
		if ok {
			return nil
		}
		errStr := ""
		if err != nil {
			errStr = " (" + err.Error() + ")"
		}
		return fmt.Errorf("tls: %q does not match any of DNSNames %q"+errStr, h, cert.DNSNames)
	}
	return nil
}

func isHostInDNS(ctx context.Context, host string, dnsNames []string) (ok bool, err error) {
	// reverse lookup
	var names []string
	var wildcards []string
	for _, dns := range dnsNames {
		if strings.HasPrefix(dns, "*.") {
			wildcards = append(wildcards, dns[1:])
		} else {
			names = append(names, dns)
		}
	}
	lnames, lerr := net.DefaultResolver.LookupAddr(ctx, host)
	for _, name := range lnames {
		// strip trailing '.' from PTR record
		if name[len(name)-1] == '.' {
			name = name[:len(name)-1]
		}
		for _, wc := range wildcards {
			if strings.HasSuffix(name, wc) {
				return true, nil
			}
		}
		for _, n := range names {
			if n == name {
				return true, nil
			}
		}
	}
	err = lerr

	// forward lookup
	for _, dns := range names {
		addrs, lerr := net.DefaultResolver.LookupHost(ctx, dns)
		if lerr != nil {
			err = lerr
			continue
		}
		for _, addr := range addrs {
			if addr == host {
				return true, nil
			}
		}
	}
	return false, err
}

func (l *tlsListener) Close() error {
	err := l.Listener.Close()
	<-l.donec
	return err
}
