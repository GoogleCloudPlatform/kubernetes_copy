/*
Copyright 2016 The Kubernetes Authors.

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

package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/endpoints/metrics"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
)

const (
	defaultKeepAlivePeriod = 3 * time.Minute
)

// tlsConfig produces the tls.Config to serve with.
func (s *SecureServingInfo) tlsConfig(stopCh <-chan struct{}) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		// Can't use SSLv3 because of POODLE and BEAST
		// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
		// Can't use TLSv1.1 because of RC4 cipher usage
		MinVersion: tls.VersionTLS12,
		// enable HTTP2 for go's 1.7 HTTP Server
		NextProtos: []string{"h2", "http/1.1"},
	}

	// these are static aspects of the tls.Config
	if s.DisableHTTP2 {
		klog.Info("Forcing use of http/1.1 only")
		tlsConfig.NextProtos = []string{"http/1.1"}
	}
	if s.MinTLSVersion > 0 {
		tlsConfig.MinVersion = s.MinTLSVersion
	}
	if len(s.CipherSuites) > 0 {
		tlsConfig.CipherSuites = s.CipherSuites
		insecureCiphers := flag.InsecureTLSCiphers()
		for i := 0; i < len(s.CipherSuites); i++ {
			for cipherName, cipherID := range insecureCiphers {
				if s.CipherSuites[i] == cipherID {
					klog.Warningf("Use of insecure cipher '%s' detected.", cipherName)
				}
			}
		}
	}

	if s.ClientCA != nil {
		// Populate PeerCertificates in requests, but don't reject connections without certificates
		// This allows certificates to be validated by authenticators, while still allowing other auth types
		tlsConfig.ClientAuth = tls.RequestClientCert
	}

	if s.ClientCA != nil || s.Cert != nil || len(s.SNICerts) > 0 {
		dynamicCertificateController := dynamiccertificates.NewDynamicServingCertificateController(
			tlsConfig,
			s.ClientCA,
			s.Cert,
			s.SNICerts,
			nil, // TODO see how to plumb an event recorder down in here. For now this results in simply klog messages.
		)

		if s.ClientCA != nil {
			s.ClientCA.AddListener(dynamicCertificateController)
		}
		if s.Cert != nil {
			s.Cert.AddListener(dynamicCertificateController)
		}
		// generate a context from stopCh. This is to avoid modifying files which are relying on apiserver
		// TODO: See if we can pass ctx to the current method
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-stopCh:
				cancel() // stopCh closed, so cancel our context
			case <-ctx.Done():
			}
		}()
		// start controllers if possible
		if controller, ok := s.ClientCA.(dynamiccertificates.ControllerRunner); ok {
			// runonce to try to prime data.  If this fails, it's ok because we fail closed.
			// Files are required to be populated already, so this is for convenience.
			if err := controller.RunOnce(ctx); err != nil {
				klog.Warningf("Initial population of client CA failed: %v", err)
			}

			go controller.Run(ctx, 1)
		}
		if controller, ok := s.Cert.(dynamiccertificates.ControllerRunner); ok {
			// runonce to try to prime data.  If this fails, it's ok because we fail closed.
			// Files are required to be populated already, so this is for convenience.
			if err := controller.RunOnce(ctx); err != nil {
				klog.Warningf("Initial population of default serving certificate failed: %v", err)
			}

			go controller.Run(ctx, 1)
		}
		for _, sniCert := range s.SNICerts {
			sniCert.AddListener(dynamicCertificateController)
			if controller, ok := sniCert.(dynamiccertificates.ControllerRunner); ok {
				// runonce to try to prime data.  If this fails, it's ok because we fail closed.
				// Files are required to be populated already, so this is for convenience.
				if err := controller.RunOnce(ctx); err != nil {
					klog.Warningf("Initial population of SNI serving certificate failed: %v", err)
				}

				go controller.Run(ctx, 1)
			}
		}

		// runonce to try to prime data.  If this fails, it's ok because we fail closed.
		// Files are required to be populated already, so this is for convenience.
		if err := dynamicCertificateController.RunOnce(ctx); err != nil {
			klog.Warningf("Initial population of dynamic certificates failed: %v", err)
		}
		go dynamicCertificateController.Run(ctx, 1, stopCh)

		tlsConfig.GetConfigForClient = dynamicCertificateController.GetConfigForClient
	}

	return tlsConfig, nil
}

// Serve runs the secure http server. It fails only if certificates cannot be loaded or the initial listen call fails.
// The actual server loop (stoppable by closing stopCh) runs in a go routine, i.e. Serve does not block.
// It returns a stoppedCh that is closed when all non-hijacked active requests have been processed.
// It returns a listenerStoppedCh that is closed when the underlying http Server has stopped listening.
func (s *SecureServingInfo) Serve(handler http.Handler, shutdownTimeout time.Duration, stopCh <-chan struct{}) (<-chan struct{}, <-chan struct{}, error) {
	if s.Listener == nil {
		return nil, nil, fmt.Errorf("listener must not be nil")
	}

	tlsConfig, err := s.tlsConfig(stopCh)
	if err != nil {
		return nil, nil, err
	}

	secureServer := &http.Server{
		Addr:           s.Listener.Addr().String(),
		Handler:        handler,
		MaxHeaderBytes: 1 << 20,
		TLSConfig:      tlsConfig,

		IdleTimeout:       90 * time.Second, // matches http.DefaultTransport keep-alive timeout
		ReadHeaderTimeout: 32 * time.Second, // just shy of requestTimeoutUpperBound
	}

	if !s.DisableHTTP2 {
		// At least 99% of serialized resources in surveyed clusters were smaller than 256kb.
		// This should be big enough to accommodate most API POST requests in a single frame,
		// and small enough to allow a per connection buffer of this size multiplied by `MaxConcurrentStreams`.
		const resourceBody99Percentile = 256 * 1024

		http2Options := &http2.Server{
			IdleTimeout: 90 * time.Second, // matches http.DefaultTransport keep-alive timeout
			// shrink the per-stream buffer and max framesize from the 1MB default while still accommodating most API POST requests in a single frame
			MaxUploadBufferPerStream: resourceBody99Percentile,
			MaxReadFrameSize:         resourceBody99Percentile,
		}

		// use the overridden concurrent streams setting or make the default of 250 explicit so we can size MaxUploadBufferPerConnection appropriately
		if s.HTTP2MaxStreamsPerConnection > 0 {
			http2Options.MaxConcurrentStreams = uint32(s.HTTP2MaxStreamsPerConnection)
		} else {
			// match http2.initialMaxConcurrentStreams used by clients
			// this makes it so that a malicious client can only open 400 streams before we forcibly close the connection
			// https://github.com/golang/net/commit/b225e7ca6dde1ef5a5ae5ce922861bda011cfabd
			http2Options.MaxConcurrentStreams = 100
		}

		// increase the connection buffer size from the 1MB default to handle the specified number of concurrent streams
		http2Options.MaxUploadBufferPerConnection = http2Options.MaxUploadBufferPerStream * int32(http2Options.MaxConcurrentStreams)
		// apply settings to the server
		if err := http2.ConfigureServer(secureServer, http2Options); err != nil {
			return nil, nil, fmt.Errorf("error configuring http2: %v", err)
		}
	}

	// use tlsHandshakeErrorWriter to handle messages of tls handshake error
	tlsErrorWriter := &tlsHandshakeErrorWriter{os.Stderr}
	tlsErrorLogger := log.New(tlsErrorWriter, "", 0)
	secureServer.ErrorLog = tlsErrorLogger

	klog.Infof("Serving securely on %s", secureServer.Addr)
	return RunServer(secureServer, s.Listener, shutdownTimeout, stopCh)
}

// RunServer spawns a go-routine continuously serving until the stopCh is
// closed.
// It returns a stoppedCh that is closed when all non-hijacked active requests
// have been processed.
// This function does not block
// TODO: make private when insecure serving is gone from the kube-apiserver
func RunServer(
	server *http.Server,
	ln net.Listener,
	shutDownTimeout time.Duration,
	stopCh <-chan struct{},
) (<-chan struct{}, <-chan struct{}, error) {
	if ln == nil {
		return nil, nil, fmt.Errorf("listener must not be nil")
	}

	// Shutdown server gracefully.
	serverShutdownCh, listenerStoppedCh := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(serverShutdownCh)
		<-stopCh
		ctx, cancel := context.WithTimeout(context.Background(), shutDownTimeout)
		defer cancel()
		err := server.Shutdown(ctx)
		if err != nil {
			klog.Errorf("Failed to shutdown server: %v", err)
		}
	}()

	go func() {
		defer utilruntime.HandleCrash()
		defer close(listenerStoppedCh)

		var listener net.Listener
		listener = tcpKeepAliveListener{ln}
		if server.TLSConfig != nil {
			listener = tls.NewListener(listener, server.TLSConfig)
		}

		err := server.Serve(listener)

		msg := fmt.Sprintf("Stopped listening on %s", ln.Addr().String())
		select {
		case <-stopCh:
			klog.Info(msg)
		default:
			panic(fmt.Sprintf("%s due to error: %v", msg, err))
		}
	}()

	return serverShutdownCh, listenerStoppedCh, nil
}

// tcpKeepAliveListener sets TCP keep-alive timeouts on accepted
// connections. It's used by ListenAndServe and ListenAndServeTLS so
// dead TCP connections (e.g. closing laptop mid-download) eventually
// go away.
//
// Copied from Go 1.7.2 net/http/server.go
type tcpKeepAliveListener struct {
	net.Listener
}

func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	c, err := ln.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(defaultKeepAlivePeriod)
	}
	return c, nil
}

// tlsHandshakeErrorWriter writes TLS handshake errors to klog with
// trace level - V(5), to avoid flooding of tls handshake errors.
type tlsHandshakeErrorWriter struct {
	out io.Writer
}

const tlsHandshakeErrorPrefix = "http: TLS handshake error"

func (w *tlsHandshakeErrorWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), tlsHandshakeErrorPrefix) {
		klog.V(5).Info(string(p))
		metrics.TLSHandshakeErrors.Inc()
		return len(p), nil
	}

	// for non tls handshake error, log it as usual
	return w.out.Write(p)
}
