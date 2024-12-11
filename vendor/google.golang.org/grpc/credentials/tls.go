/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package credentials

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"

	"google.golang.org/grpc/grpclog"
	credinternal "google.golang.org/grpc/internal/credentials"
	"google.golang.org/grpc/internal/envconfig"
)

var logger = grpclog.Component("credentials")

// TLSInfo contains the auth information for a TLS authenticated connection.
// It implements the AuthInfo interface.
type TLSInfo struct {
	State tls.ConnectionState
	CommonAuthInfo
	// This API is experimental.
	SPIFFEID *url.URL
}

// AuthType returns the type of TLSInfo as a string.
func (t TLSInfo) AuthType() string {
	return "tls"
}

// cipherSuiteLookup returns the string version of a TLS cipher suite ID.
func cipherSuiteLookup(cipherSuiteID uint16) string {
	for _, s := range tls.CipherSuites() {
		if s.ID == cipherSuiteID {
			return s.Name
		}
	}
	for _, s := range tls.InsecureCipherSuites() {
		if s.ID == cipherSuiteID {
			return s.Name
		}
	}
	return fmt.Sprintf("unknown ID: %v", cipherSuiteID)
}

// GetSecurityValue returns security info requested by channelz.
func (t TLSInfo) GetSecurityValue() ChannelzSecurityValue {
	v := &TLSChannelzSecurityValue{
		StandardName: cipherSuiteLookup(t.State.CipherSuite),
	}
	// Currently there's no way to get LocalCertificate info from tls package.
	if len(t.State.PeerCertificates) > 0 {
		v.RemoteCertificate = t.State.PeerCertificates[0].Raw
	}
	return v
}

// tlsCreds is the credentials required for authenticating a connection using TLS.
type tlsCreds struct {
	// TLS configuration
	config *tls.Config
}

func (c tlsCreds) Info() ProtocolInfo {
	return ProtocolInfo{
		SecurityProtocol: "tls",
		SecurityVersion:  "1.2",
		ServerName:       c.config.ServerName,
	}
}

func (c *tlsCreds) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (_ net.Conn, _ AuthInfo, err error) {
	// use local cfg to avoid clobbering ServerName if using multiple endpoints
	cfg := credinternal.CloneTLSConfig(c.config)
	if cfg.ServerName == "" {
		serverName, _, err := net.SplitHostPort(authority)
		if err != nil {
			// If the authority had no host port or if the authority cannot be parsed, use it as-is.
			serverName = authority
		}
		cfg.ServerName = serverName
	}
	conn := tls.Client(rawConn, cfg)
	errChannel := make(chan error, 1)
	go func() {
		errChannel <- conn.Handshake()
		close(errChannel)
	}()
	select {
	case err := <-errChannel:
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
	case <-ctx.Done():
		conn.Close()
		return nil, nil, ctx.Err()
	}

	// The negotiated protocol can be either of the following:
	// 1. h2: When the server supports ALPN. Only HTTP/2 can be negotiated since
	//    it is the only protocol advertised by the client during the handshake.
	//    The tls library ensures that the server chooses a protocol advertised
	//    by the client.
	// 2. "" (empty string): If the server doesn't support ALPN. ALPN is a requirement
	//    for using HTTP/2 over TLS. We can terminate the connection immediately.
	np := conn.ConnectionState().NegotiatedProtocol
	if np == "" {
		if envconfig.EnforceALPNEnabled {
			conn.Close()
			return nil, nil, fmt.Errorf("credentials: cannot check peer: missing selected ALPN property")
		}
		logger.Warningf("Allowing TLS connection to server %q with ALPN disabled. TLS connections to servers with ALPN disabled will be disallowed in future grpc-go releases", cfg.ServerName)
	}
	tlsInfo := TLSInfo{
		State: conn.ConnectionState(),
		CommonAuthInfo: CommonAuthInfo{
			SecurityLevel: PrivacyAndIntegrity,
		},
	}
	id := credinternal.SPIFFEIDFromState(conn.ConnectionState())
	if id != nil {
		tlsInfo.SPIFFEID = id
	}
	return credinternal.WrapSyscallConn(rawConn, conn), tlsInfo, nil
}

func (c *tlsCreds) ServerHandshake(rawConn net.Conn) (net.Conn, AuthInfo, error) {
	conn := tls.Server(rawConn, c.config)
	if err := conn.Handshake(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	cs := conn.ConnectionState()
	// The negotiated application protocol can be empty only if the client doesn't
	// support ALPN. In such cases, we can close the connection since ALPN is required
	// for using HTTP/2 over TLS.
	if cs.NegotiatedProtocol == "" {
		if envconfig.EnforceALPNEnabled {
			conn.Close()
			return nil, nil, fmt.Errorf("credentials: cannot check peer: missing selected ALPN property")
		} else if logger.V(2) {
			logger.Info("Allowing TLS connection from client with ALPN disabled. TLS connections with ALPN disabled will be disallowed in future grpc-go releases")
		}
	}
	tlsInfo := TLSInfo{
		State: cs,
		CommonAuthInfo: CommonAuthInfo{
			SecurityLevel: PrivacyAndIntegrity,
		},
	}
	id := credinternal.SPIFFEIDFromState(conn.ConnectionState())
	if id != nil {
		tlsInfo.SPIFFEID = id
	}
	return credinternal.WrapSyscallConn(rawConn, conn), tlsInfo, nil
}

func (c *tlsCreds) Clone() TransportCredentials {
	return NewTLS(c.config)
}

func (c *tlsCreds) OverrideServerName(serverNameOverride string) error {
	c.config.ServerName = serverNameOverride
	return nil
}

// The following cipher suites are forbidden for use with HTTP/2 by
// https://datatracker.ietf.org/doc/html/rfc7540#appendix-A
var tls12ForbiddenCipherSuites = map[uint16]struct{}{
	tls.TLS_RSA_WITH_AES_128_CBC_SHA:         {},
	tls.TLS_RSA_WITH_AES_256_CBC_SHA:         {},
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256:      {},
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384:      {},
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA: {},
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA: {},
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA:   {},
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA:   {},
}

// NewTLS uses c to construct a TransportCredentials based on TLS.
func NewTLS(c *tls.Config) TransportCredentials {
	config := applyDefaults(c)
	if config.GetConfigForClient != nil {
		oldFn := config.GetConfigForClient
		config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			cfgForClient, err := oldFn(hello)
			if err != nil || cfgForClient == nil {
				return cfgForClient, err
			}
			return applyDefaults(cfgForClient), nil
		}
	}
	return &tlsCreds{config: config}
}

func applyDefaults(c *tls.Config) *tls.Config {
	config := credinternal.CloneTLSConfig(c)
	config.NextProtos = credinternal.AppendH2ToNextProtos(config.NextProtos)
	// If the user did not configure a MinVersion and did not configure a
	// MaxVersion < 1.2, use MinVersion=1.2, which is required by
	// https://datatracker.ietf.org/doc/html/rfc7540#section-9.2
	if config.MinVersion == 0 && (config.MaxVersion == 0 || config.MaxVersion >= tls.VersionTLS12) {
		config.MinVersion = tls.VersionTLS12
	}
	// If the user did not configure CipherSuites, use all "secure" cipher
	// suites reported by the TLS package, but remove some explicitly forbidden
	// by https://datatracker.ietf.org/doc/html/rfc7540#appendix-A
	if config.CipherSuites == nil {
		for _, cs := range tls.CipherSuites() {
			if _, ok := tls12ForbiddenCipherSuites[cs.ID]; !ok {
				config.CipherSuites = append(config.CipherSuites, cs.ID)
			}
		}
	}
	return config
}

// NewClientTLSFromCert constructs TLS credentials from the provided root
// certificate authority certificate(s) to validate server connections. If
// certificates to establish the identity of the client need to be included in
// the credentials (eg: for mTLS), use NewTLS instead, where a complete
// tls.Config can be specified.
// serverNameOverride is for testing only. If set to a non empty string,
// it will override the virtual host name of authority (e.g. :authority header
// field) in requests.
func NewClientTLSFromCert(cp *x509.CertPool, serverNameOverride string) TransportCredentials {
	return NewTLS(&tls.Config{ServerName: serverNameOverride, RootCAs: cp})
}

// NewClientTLSFromFile constructs TLS credentials from the provided root
// certificate authority certificate file(s) to validate server connections. If
// certificates to establish the identity of the client need to be included in
// the credentials (eg: for mTLS), use NewTLS instead, where a complete
// tls.Config can be specified.
// serverNameOverride is for testing only. If set to a non empty string,
// it will override the virtual host name of authority (e.g. :authority header
// field) in requests.
func NewClientTLSFromFile(certFile, serverNameOverride string) (TransportCredentials, error) {
	b, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("credentials: failed to append certificates")
	}
	return NewTLS(&tls.Config{ServerName: serverNameOverride, RootCAs: cp}), nil
}

// NewServerTLSFromCert constructs TLS credentials from the input certificate for server.
func NewServerTLSFromCert(cert *tls.Certificate) TransportCredentials {
	return NewTLS(&tls.Config{Certificates: []tls.Certificate{*cert}})
}

// NewServerTLSFromFile constructs TLS credentials from the input certificate file and key
// file for server.
func NewServerTLSFromFile(certFile, keyFile string) (TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}}), nil
}

// TLSChannelzSecurityValue defines the struct that TLS protocol should return
// from GetSecurityValue(), containing security info like cipher and certificate used.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type TLSChannelzSecurityValue struct {
	ChannelzSecurityValue
	StandardName      string
	LocalCertificate  []byte
	RemoteCertificate []byte
}
