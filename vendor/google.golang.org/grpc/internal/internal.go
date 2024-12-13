/*
 * Copyright 2016 gRPC authors.
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

// Package internal contains gRPC-internal code, to avoid polluting
// the godoc of the top-level grpc package.  It must not import any grpc
// symbols to avoid circular dependencies.
package internal

import (
	"context"
	"time"

	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/serviceconfig"
)

var (
	// WithHealthCheckFunc is set by dialoptions.go
	WithHealthCheckFunc any // func (HealthChecker) DialOption
	// HealthCheckFunc is used to provide client-side LB channel health checking
	HealthCheckFunc HealthChecker
	// BalancerUnregister is exported by package balancer to unregister a balancer.
	BalancerUnregister func(name string)
	// KeepaliveMinPingTime is the minimum ping interval.  This must be 10s by
	// default, but tests may wish to set it lower for convenience.
	KeepaliveMinPingTime = 10 * time.Second
	// KeepaliveMinServerPingTime is the minimum ping interval for servers.
	// This must be 1s by default, but tests may wish to set it lower for
	// convenience.
	KeepaliveMinServerPingTime = time.Second
	// ParseServiceConfig parses a JSON representation of the service config.
	ParseServiceConfig any // func(string) *serviceconfig.ParseResult
	// EqualServiceConfigForTesting is for testing service config generation and
	// parsing. Both a and b should be returned by ParseServiceConfig.
	// This function compares the config without rawJSON stripped, in case the
	// there's difference in white space.
	EqualServiceConfigForTesting func(a, b serviceconfig.Config) bool
	// GetCertificateProviderBuilder returns the registered builder for the
	// given name. This is set by package certprovider for use from xDS
	// bootstrap code while parsing certificate provider configs in the
	// bootstrap file.
	GetCertificateProviderBuilder any // func(string) certprovider.Builder
	// GetXDSHandshakeInfoForTesting returns a pointer to the xds.HandshakeInfo
	// stored in the passed in attributes. This is set by
	// credentials/xds/xds.go.
	GetXDSHandshakeInfoForTesting any // func (*attributes.Attributes) *unsafe.Pointer
	// GetServerCredentials returns the transport credentials configured on a
	// gRPC server. An xDS-enabled server needs to know what type of credentials
	// is configured on the underlying gRPC server. This is set by server.go.
	GetServerCredentials any // func (*grpc.Server) credentials.TransportCredentials
	// CanonicalString returns the canonical string of the code defined here:
	// https://github.com/grpc/grpc/blob/master/doc/statuscodes.md.
	//
	// This is used in the 1.0 release of gcp/observability, and thus must not be
	// deleted or changed.
	CanonicalString any // func (codes.Code) string
	// IsRegisteredMethod returns whether the passed in method is registered as
	// a method on the server.
	IsRegisteredMethod any // func(*grpc.Server, string) bool
	// ServerFromContext returns the server from the context.
	ServerFromContext any // func(context.Context) *grpc.Server
	// AddGlobalServerOptions adds an array of ServerOption that will be
	// effective globally for newly created servers. The priority will be: 1.
	// user-provided; 2. this method; 3. default values.
	//
	// This is used in the 1.0 release of gcp/observability, and thus must not be
	// deleted or changed.
	AddGlobalServerOptions any // func(opt ...ServerOption)
	// ClearGlobalServerOptions clears the array of extra ServerOption. This
	// method is useful in testing and benchmarking.
	//
	// This is used in the 1.0 release of gcp/observability, and thus must not be
	// deleted or changed.
	ClearGlobalServerOptions func()
	// AddGlobalDialOptions adds an array of DialOption that will be effective
	// globally for newly created client channels. The priority will be: 1.
	// user-provided; 2. this method; 3. default values.
	//
	// This is used in the 1.0 release of gcp/observability, and thus must not be
	// deleted or changed.
	AddGlobalDialOptions any // func(opt ...DialOption)
	// DisableGlobalDialOptions returns a DialOption that prevents the
	// ClientConn from applying the global DialOptions (set via
	// AddGlobalDialOptions).
	//
	// This is used in the 1.0 release of gcp/observability, and thus must not be
	// deleted or changed.
	DisableGlobalDialOptions any // func() grpc.DialOption
	// ClearGlobalDialOptions clears the array of extra DialOption. This
	// method is useful in testing and benchmarking.
	//
	// This is used in the 1.0 release of gcp/observability, and thus must not be
	// deleted or changed.
	ClearGlobalDialOptions func()

	// AddGlobalPerTargetDialOptions adds a PerTargetDialOption that will be
	// configured for newly created ClientConns.
	AddGlobalPerTargetDialOptions any // func (opt any)
	// ClearGlobalPerTargetDialOptions clears the slice of global late apply
	// dial options.
	ClearGlobalPerTargetDialOptions func()

	// JoinDialOptions combines the dial options passed as arguments into a
	// single dial option.
	JoinDialOptions any // func(...grpc.DialOption) grpc.DialOption
	// JoinServerOptions combines the server options passed as arguments into a
	// single server option.
	JoinServerOptions any // func(...grpc.ServerOption) grpc.ServerOption

	// WithBinaryLogger returns a DialOption that specifies the binary logger
	// for a ClientConn.
	//
	// This is used in the 1.0 release of gcp/observability, and thus must not be
	// deleted or changed.
	WithBinaryLogger any // func(binarylog.Logger) grpc.DialOption
	// BinaryLogger returns a ServerOption that can set the binary logger for a
	// server.
	//
	// This is used in the 1.0 release of gcp/observability, and thus must not be
	// deleted or changed.
	BinaryLogger any // func(binarylog.Logger) grpc.ServerOption

	// SubscribeToConnectivityStateChanges adds a grpcsync.Subscriber to a
	// provided grpc.ClientConn.
	SubscribeToConnectivityStateChanges any // func(*grpc.ClientConn, grpcsync.Subscriber)

	// NewXDSResolverWithConfigForTesting creates a new xds resolver builder using
	// the provided xds bootstrap config instead of the global configuration from
	// the supported environment variables.  The resolver.Builder is meant to be
	// used in conjunction with the grpc.WithResolvers DialOption.
	//
	// Testing Only
	//
	// This function should ONLY be used for testing and may not work with some
	// other features, including the CSDS service.
	NewXDSResolverWithConfigForTesting any // func([]byte) (resolver.Builder, error)

	// RegisterRLSClusterSpecifierPluginForTesting registers the RLS Cluster
	// Specifier Plugin for testing purposes, regardless of the XDSRLS environment
	// variable.
	//
	// TODO: Remove this function once the RLS env var is removed.
	RegisterRLSClusterSpecifierPluginForTesting func()

	// UnregisterRLSClusterSpecifierPluginForTesting unregisters the RLS Cluster
	// Specifier Plugin for testing purposes. This is needed because there is no way
	// to unregister the RLS Cluster Specifier Plugin after registering it solely
	// for testing purposes using RegisterRLSClusterSpecifierPluginForTesting().
	//
	// TODO: Remove this function once the RLS env var is removed.
	UnregisterRLSClusterSpecifierPluginForTesting func()

	// RegisterRBACHTTPFilterForTesting registers the RBAC HTTP Filter for testing
	// purposes, regardless of the RBAC environment variable.
	//
	// TODO: Remove this function once the RBAC env var is removed.
	RegisterRBACHTTPFilterForTesting func()

	// UnregisterRBACHTTPFilterForTesting unregisters the RBAC HTTP Filter for
	// testing purposes. This is needed because there is no way to unregister the
	// HTTP Filter after registering it solely for testing purposes using
	// RegisterRBACHTTPFilterForTesting().
	//
	// TODO: Remove this function once the RBAC env var is removed.
	UnregisterRBACHTTPFilterForTesting func()

	// ORCAAllowAnyMinReportingInterval is for examples/orca use ONLY.
	ORCAAllowAnyMinReportingInterval any // func(so *orca.ServiceOptions)

	// GRPCResolverSchemeExtraMetadata determines when gRPC will add extra
	// metadata to RPCs.
	GRPCResolverSchemeExtraMetadata = "xds"

	// EnterIdleModeForTesting gets the ClientConn to enter IDLE mode.
	EnterIdleModeForTesting any // func(*grpc.ClientConn)

	// ExitIdleModeForTesting gets the ClientConn to exit IDLE mode.
	ExitIdleModeForTesting any // func(*grpc.ClientConn) error

	// ChannelzTurnOffForTesting disables the Channelz service for testing
	// purposes.
	ChannelzTurnOffForTesting func()

	// TriggerXDSResourceNotFoundForTesting causes the provided xDS Client to
	// invoke resource-not-found error for the given resource type and name.
	TriggerXDSResourceNotFoundForTesting any // func(xdsclient.XDSClient, xdsresource.Type, string) error

	// FromOutgoingContextRaw returns the un-merged, intermediary contents of
	// metadata.rawMD.
	FromOutgoingContextRaw any // func(context.Context) (metadata.MD, [][]string, bool)

	// UserSetDefaultScheme is set to true if the user has overridden the
	// default resolver scheme.
	UserSetDefaultScheme = false

	// ConnectedAddress returns the connected address for a SubConnState. The
	// address is only valid if the state is READY.
	ConnectedAddress any // func (scs SubConnState) resolver.Address

	// SetConnectedAddress sets the connected address for a SubConnState.
	SetConnectedAddress any // func(scs *SubConnState, addr resolver.Address)

	// SnapshotMetricRegistryForTesting snapshots the global data of the metric
	// registry. Returns a cleanup function that sets the metric registry to its
	// original state. Only called in testing functions.
	SnapshotMetricRegistryForTesting func() func()

	// SetDefaultBufferPoolForTesting updates the default buffer pool, for
	// testing purposes.
	SetDefaultBufferPoolForTesting any // func(mem.BufferPool)

	// SetBufferPoolingThresholdForTesting updates the buffer pooling threshold, for
	// testing purposes.
	SetBufferPoolingThresholdForTesting any // func(int)
)

// HealthChecker defines the signature of the client-side LB channel health
// checking function.
//
// The implementation is expected to create a health checking RPC stream by
// calling newStream(), watch for the health status of serviceName, and report
// its health back by calling setConnectivityState().
//
// The health checking protocol is defined at:
// https://github.com/grpc/grpc/blob/master/doc/health-checking.md
type HealthChecker func(ctx context.Context, newStream func(string) (any, error), setConnectivityState func(connectivity.State, error), serviceName string) error

const (
	// CredsBundleModeFallback switches GoogleDefaultCreds to fallback mode.
	CredsBundleModeFallback = "fallback"
	// CredsBundleModeBalancer switches GoogleDefaultCreds to grpclb balancer
	// mode.
	CredsBundleModeBalancer = "balancer"
	// CredsBundleModeBackendFromBalancer switches GoogleDefaultCreds to mode
	// that supports backend returned by grpclb balancer.
	CredsBundleModeBackendFromBalancer = "backend-from-balancer"
)

// RLSLoadBalancingPolicyName is the name of the RLS LB policy.
//
// It currently has an experimental suffix which would be removed once
// end-to-end testing of the policy is completed.
const RLSLoadBalancingPolicyName = "rls_experimental"
