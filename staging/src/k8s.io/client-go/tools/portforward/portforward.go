/*
Copyright 2015 The Kubernetes Authors.

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

package portforward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"
)

// PortForwardProtocolV1Name is the subprotocol used for port forwarding.
// TODO move to API machinery and re-unify with kubelet/server/portfoward
const PortForwardProtocolV1Name = "portforward.k8s.io"

var (
	// error returned whenever we lost connection to a pod
	ErrLostConnectionToPod = errors.New("lost connection to pod")

	// set of error we're expecting during port-forwarding
	networkClosedError = "use of closed network connection"
)

// PortForwarder knows how to listen for local connections and forward them to
// a remote pod via an upgraded HTTP request.
type PortForwarder struct {
	addresses []listenAddress
	ports     []ForwardedPort
	stopChan  <-chan struct{}

	logger        klog.Logger
	dialer        httpstream.Dialer
	streamConn    httpstream.Connection
	listeners     []io.Closer
	Ready         chan struct{}
	requestIDLock sync.Mutex
	requestID     int
	out           io.Writer
	errOut        io.Writer
}

// ForwardedPort contains a Local:Remote port pairing.
type ForwardedPort struct {
	Local  uint16
	Remote uint16
}

/*
valid port specifications:

5000
- forwards from localhost:5000 to pod:5000

8888:5000
- forwards from localhost:8888 to pod:5000

0:5000
:5000
  - selects a random available local port,
    forwards from localhost:<random port> to pod:5000
*/
func parsePorts(ports []string) ([]ForwardedPort, error) {
	var forwards []ForwardedPort
	for _, portString := range ports {
		parts := strings.Split(portString, ":")
		var localString, remoteString string
		if len(parts) == 1 {
			localString = parts[0]
			remoteString = parts[0]
		} else if len(parts) == 2 {
			localString = parts[0]
			if localString == "" {
				// support :5000
				localString = "0"
			}
			remoteString = parts[1]
		} else {
			return nil, fmt.Errorf("invalid port format '%s'", portString)
		}

		localPort, err := strconv.ParseUint(localString, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("error parsing local port '%s': %s", localString, err)
		}

		remotePort, err := strconv.ParseUint(remoteString, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("error parsing remote port '%s': %s", remoteString, err)
		}
		if remotePort == 0 {
			return nil, fmt.Errorf("remote port must be > 0")
		}

		forwards = append(forwards, ForwardedPort{uint16(localPort), uint16(remotePort)})
	}

	return forwards, nil
}

type listenAddress struct {
	address     string
	protocol    string
	failureMode string
}

func parseAddresses(addressesToParse []string) ([]listenAddress, error) {
	var addresses []listenAddress
	parsed := make(map[string]listenAddress)
	for _, address := range addressesToParse {
		if address == "localhost" {
			if _, exists := parsed["127.0.0.1"]; !exists {
				ip := listenAddress{address: "127.0.0.1", protocol: "tcp4", failureMode: "all"}
				parsed[ip.address] = ip
			}
			if _, exists := parsed["::1"]; !exists {
				ip := listenAddress{address: "::1", protocol: "tcp6", failureMode: "all"}
				parsed[ip.address] = ip
			}
		} else if netutils.ParseIPSloppy(address).To4() != nil {
			parsed[address] = listenAddress{address: address, protocol: "tcp4", failureMode: "any"}
		} else if netutils.ParseIPSloppy(address) != nil {
			parsed[address] = listenAddress{address: address, protocol: "tcp6", failureMode: "any"}
		} else {
			return nil, fmt.Errorf("%s is not a valid IP", address)
		}
	}
	addresses = make([]listenAddress, len(parsed))
	id := 0
	for _, v := range parsed {
		addresses[id] = v
		id++
	}
	// Sort addresses before returning to get a stable order
	sort.Slice(addresses, func(i, j int) bool { return addresses[i].address < addresses[j].address })

	return addresses, nil
}

// New creates a new PortForwarder with localhost listen addresses.
func New(dialer httpstream.Dialer, ports []string, stopChan <-chan struct{}, readyChan chan struct{}, out, errOut io.Writer) (*PortForwarder, error) {
	return NewOnAddresses(dialer, []string{"localhost"}, ports, stopChan, readyChan, out, errOut)
}

// NewOnAddresses creates a new PortForwarder with custom listen addresses.
//
// Contextual logging: NewOnAddressesWithContext should be used instead of NewOnAddresses in code which supports contextual logging.
func NewOnAddresses(dialer httpstream.Dialer, addresses []string, ports []string, stopChan <-chan struct{}, readyChan chan struct{}, out, errOut io.Writer) (*PortForwarder, error) {
	return NewOnAddressesWithContext(wait.ContextForChannel(stopChan), dialer, addresses, ports, readyChan, out, errOut)
}

// NewOnAddressesWithContext creates a new PortForwarder with custom listen addresses.
func NewOnAddressesWithContext(ctx context.Context, dialer httpstream.Dialer, addresses []string, ports []string, readyChan chan struct{}, out, errOut io.Writer) (*PortForwarder, error) {
	if len(addresses) == 0 {
		return nil, errors.New("you must specify at least 1 address")
	}
	parsedAddresses, err := parseAddresses(addresses)
	if err != nil {
		return nil, err
	}
	if len(ports) == 0 {
		return nil, errors.New("you must specify at least 1 port")
	}
	parsedPorts, err := parsePorts(ports)
	if err != nil {
		return nil, err
	}
	return &PortForwarder{
		logger:    klog.FromContext(ctx),
		dialer:    dialer,
		addresses: parsedAddresses,
		ports:     parsedPorts,
		stopChan:  ctx.Done(),
		Ready:     readyChan,
		out:       out,
		errOut:    errOut,
	}, nil
}

// ForwardPorts formats and executes a port forwarding request. The connection will remain
// open until stopChan is closed.
func (pf *PortForwarder) ForwardPorts() error {
	defer pf.Close()

	var err error
	var protocol string
	pf.streamConn, protocol, err = pf.dialer.Dial(PortForwardProtocolV1Name)
	if err != nil {
		return fmt.Errorf("error upgrading connection: %s", err)
	}
	defer pf.streamConn.Close()
	if protocol != PortForwardProtocolV1Name {
		return fmt.Errorf("unable to negotiate protocol: client supports %q, server returned %q", PortForwardProtocolV1Name, protocol)
	}

	return pf.forward()
}

// forward dials the remote host specific in req, upgrades the request, starts
// listeners for each port specified in ports, and forwards local connections
// to the remote host via streams.
func (pf *PortForwarder) forward() error {
	var err error

	listenSuccess := false
	for i := range pf.ports {
		port := &pf.ports[i]
		err = pf.listenOnPort(port)
		switch {
		case err == nil:
			listenSuccess = true
		default:
			if pf.errOut != nil {
				fmt.Fprintf(pf.errOut, "Unable to listen on port %d: %v\n", port.Local, err)
			}
		}
	}

	if !listenSuccess {
		return fmt.Errorf("unable to listen on any of the requested ports: %v", pf.ports)
	}

	if pf.Ready != nil {
		close(pf.Ready)
	}

	// wait for interrupt or conn closure
	select {
	case <-pf.stopChan:
	case <-pf.streamConn.CloseChan():
		return ErrLostConnectionToPod
	}

	return nil
}

// listenOnPort delegates listener creation and waits for connections on requested bind addresses.
// An error is raised based on address groups (default and localhost) and their failure modes
func (pf *PortForwarder) listenOnPort(port *ForwardedPort) error {
	var errors []error
	failCounters := make(map[string]int, 2)
	successCounters := make(map[string]int, 2)
	for _, addr := range pf.addresses {
		err := pf.listenOnPortAndAddress(port, addr.protocol, addr.address)
		if err != nil {
			errors = append(errors, err)
			failCounters[addr.failureMode]++
		} else {
			successCounters[addr.failureMode]++
		}
	}
	if successCounters["all"] == 0 && failCounters["all"] > 0 {
		return fmt.Errorf("%s: %v", "Listeners failed to create with the following errors", errors)
	}
	if failCounters["any"] > 0 {
		return fmt.Errorf("%s: %v", "Listeners failed to create with the following errors", errors)
	}
	return nil
}

// listenOnPortAndAddress delegates listener creation and waits for new connections
// in the background f
func (pf *PortForwarder) listenOnPortAndAddress(port *ForwardedPort, protocol string, address string) error {
	listener, err := pf.getListener(protocol, address, port)
	if err != nil {
		return err
	}
	pf.listeners = append(pf.listeners, listener)
	go pf.waitForConnection(listener, *port)
	return nil
}

// getListener creates a listener on the interface targeted by the given hostname on the given port with
// the given protocol. protocol is in net.Listen style which basically admits values like tcp, tcp4, tcp6
func (pf *PortForwarder) getListener(protocol string, hostname string, port *ForwardedPort) (net.Listener, error) {
	listener, err := net.Listen(protocol, net.JoinHostPort(hostname, strconv.Itoa(int(port.Local))))
	if err != nil {
		return nil, fmt.Errorf("unable to create listener: Error %s", err)
	}
	listenerAddress := listener.Addr().String()
	host, localPort, _ := net.SplitHostPort(listenerAddress)
	localPortUInt, err := strconv.ParseUint(localPort, 10, 16)

	if err != nil {
		fmt.Fprintf(pf.out, "Failed to forward from %s:%d -> %d\n", hostname, localPortUInt, port.Remote)
		return nil, fmt.Errorf("error parsing local port: %s from %s (%s)", err, listenerAddress, host)
	}
	port.Local = uint16(localPortUInt)
	if pf.out != nil {
		fmt.Fprintf(pf.out, "Forwarding from %s -> %d\n", net.JoinHostPort(hostname, strconv.Itoa(int(localPortUInt))), port.Remote)
	}

	return listener, nil
}

// waitForConnection waits for new connections to listener and handles them in
// the background.
func (pf *PortForwarder) waitForConnection(listener net.Listener, port ForwardedPort) {
	for {
		select {
		case <-pf.streamConn.CloseChan():
			return
		default:
			conn, err := listener.Accept()
			if err != nil {
				// TODO consider using something like https://github.com/hydrogen18/stoppableListener?
				if !strings.Contains(strings.ToLower(err.Error()), networkClosedError) {
					runtime.HandleErrorWithLogger(pf.logger, err, "Error accepting connection", "localPort", port.Local)
				}
				return
			}
			go pf.handleConnection(conn, port)
		}
	}
}

func (pf *PortForwarder) nextRequestID() int {
	pf.requestIDLock.Lock()
	defer pf.requestIDLock.Unlock()
	id := pf.requestID
	pf.requestID++
	return id
}

// handleConnection copies data between the local connection and the stream to
// the remote server.
func (pf *PortForwarder) handleConnection(conn net.Conn, port ForwardedPort) {
	defer conn.Close()

	if pf.out != nil {
		fmt.Fprintf(pf.out, "Handling connection for %d\n", port.Local)
	}

	requestID := pf.nextRequestID()

	// create error stream
	headers := http.Header{}
	headers.Set(v1.StreamType, v1.StreamTypeError)
	headers.Set(v1.PortHeader, fmt.Sprintf("%d", port.Remote))
	headers.Set(v1.PortForwardRequestIDHeader, strconv.Itoa(requestID))
	errorStream, err := pf.streamConn.CreateStream(headers)
	if err != nil {
		runtime.HandleErrorWithLogger(pf.logger, err, "Error creating error stream", "localPort", port.Local, "remotePort", port.Remote)
		return
	}
	// we're not writing to this stream
	errorStream.Close()
	defer pf.streamConn.RemoveStreams(errorStream)

	type readAllResult struct {
		message []byte
		err     error
	}
	errorChan := make(chan readAllResult)
	go func() {
		message, err := io.ReadAll(errorStream)
		errorChan <- readAllResult{
			message: message,
			err:     err,
		}
		close(errorChan)
	}()

	// create data stream
	headers.Set(v1.StreamType, v1.StreamTypeData)
	dataStream, err := pf.streamConn.CreateStream(headers)
	if err != nil {
		runtime.HandleErrorWithLogger(pf.logger, err, "Error creating forwarding stream", "localPort", port.Local, "remotePort", port.Remote)
		return
	}
	defer pf.streamConn.RemoveStreams(dataStream)

	localError := make(chan struct{})
	remoteDone := make(chan struct{})

	go func() {
		// Copy from the remote side to the local port.
		if _, err := io.Copy(conn, dataStream); err != nil && !strings.Contains(strings.ToLower(err.Error()), networkClosedError) {
			runtime.HandleErrorWithLogger(pf.logger, err, "Error copying from remote stream to local connection", "localPort", port.Local, "remotePort", port.Remote)
		}

		// inform the select below that the remote copy is done
		close(remoteDone)
	}()

	go func() {
		// inform server we're not sending any more data after copy unblocks
		defer dataStream.Close()

		// Copy from the local port to the remote side.
		if _, err := io.Copy(dataStream, conn); err != nil && !strings.Contains(strings.ToLower(err.Error()), networkClosedError) {
			runtime.HandleErrorWithLogger(pf.logger, err, "Error copying from local connection to remote stream", "localPort", port.Local, "remotePort", port.Remote)
			// break out of the select below without waiting for the other copy to finish
			close(localError)
		}
	}()

	// wait for either a local->remote error or for copying from remote->local to finish
	select {
	case <-remoteDone:
	case <-localError:
	}

	// reset dataStream to discard any unsent data, preventing port forwarding from being blocked.
	// we must reset dataStream before waiting on errorChan, otherwise,
	// the blocking data will affect errorStream and cause <-errorChan to block indefinitely.
	_ = dataStream.Reset()

	// always expect something on errorChan (it may be empty)
	errResult := <-errorChan
	switch {
	case errResult.err != nil:
		runtime.HandleErrorWithLogger(pf.logger, errResult.err, "Error reading from error stream", "localPort", port.Local, "remotePort", port.Remote)
		pf.streamConn.Close()
	case len(errResult.message) > 0:
		runtime.HandleErrorWithLogger(pf.logger, errors.New(string(errResult.message)), "An error occurred forwarding", "localPort", port.Local, "remotePort", port.Remote)
		pf.streamConn.Close()
	}
}

// Close stops all listeners of PortForwarder.
func (pf *PortForwarder) Close() {
	// stop all listeners
	for _, l := range pf.listeners {
		if err := l.Close(); err != nil {
			runtime.HandleErrorWithLogger(pf.logger, err, "Error closing listener")
		}
	}
}

// GetPorts will return the ports that were forwarded; this can be used to
// retrieve the locally-bound port in cases where the input was port 0. This
// function will signal an error if the Ready channel is nil or if the
// listeners are not ready yet; this function will succeed after the Ready
// channel has been closed.
func (pf *PortForwarder) GetPorts() ([]ForwardedPort, error) {
	if pf.Ready == nil {
		return nil, fmt.Errorf("no Ready channel provided")
	}
	select {
	case <-pf.Ready:
		return pf.ports, nil
	default:
		return nil, fmt.Errorf("listeners not ready")
	}
}
