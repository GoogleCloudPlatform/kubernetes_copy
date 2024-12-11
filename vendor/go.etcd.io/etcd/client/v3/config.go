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

package clientv3

import (
	"context"
	"crypto/tls"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"go.etcd.io/etcd/client/pkg/v3/transport"
)

type Config struct {
	// Endpoints is a list of URLs.
	Endpoints []string `json:"endpoints"`

	// AutoSyncInterval is the interval to update endpoints with its latest members.
	// 0 disables auto-sync. By default auto-sync is disabled.
	AutoSyncInterval time.Duration `json:"auto-sync-interval"`

	// DialTimeout is the timeout for failing to establish a connection.
	DialTimeout time.Duration `json:"dial-timeout"`

	// DialKeepAliveTime is the time after which client pings the server to see if
	// transport is alive.
	DialKeepAliveTime time.Duration `json:"dial-keep-alive-time"`

	// DialKeepAliveTimeout is the time that the client waits for a response for the
	// keep-alive probe. If the response is not received in this time, the connection is closed.
	DialKeepAliveTimeout time.Duration `json:"dial-keep-alive-timeout"`

	// MaxCallSendMsgSize is the client-side request send limit in bytes.
	// If 0, it defaults to 2.0 MiB (2 * 1024 * 1024).
	// Make sure that "MaxCallSendMsgSize" < server-side default send/recv limit.
	// ("--max-request-bytes" flag to etcd or "embed.Config.MaxRequestBytes").
	MaxCallSendMsgSize int

	// MaxCallRecvMsgSize is the client-side response receive limit.
	// If 0, it defaults to "math.MaxInt32", because range response can
	// easily exceed request send limits.
	// Make sure that "MaxCallRecvMsgSize" >= server-side default send/recv limit.
	// ("--max-recv-bytes" flag to etcd).
	MaxCallRecvMsgSize int

	// TLS holds the client secure credentials, if any.
	TLS *tls.Config

	// Username is a user name for authentication.
	Username string `json:"username"`

	// Password is a password for authentication.
	Password string `json:"password"`

	// RejectOldCluster when set will refuse to create a client against an outdated cluster.
	RejectOldCluster bool `json:"reject-old-cluster"`

	// DialOptions is a list of dial options for the grpc client (e.g., for interceptors).
	// For example, pass "grpc.WithBlock()" to block until the underlying connection is up.
	// Without this, Dial returns immediately and connecting the server happens in background.
	DialOptions []grpc.DialOption

	// Context is the default client context; it can be used to cancel grpc dial out and
	// other operations that do not have an explicit context.
	Context context.Context

	// Logger sets client-side logger.
	// If nil, fallback to building LogConfig.
	Logger *zap.Logger

	// LogConfig configures client-side logger.
	// If nil, use the default logger.
	// TODO: configure gRPC logger
	LogConfig *zap.Config

	// PermitWithoutStream when set will allow client to send keepalive pings to server without any active streams(RPCs).
	PermitWithoutStream bool `json:"permit-without-stream"`

	// MaxUnaryRetries is the maximum number of retries for unary RPCs.
	MaxUnaryRetries uint `json:"max-unary-retries"`

	// BackoffWaitBetween is the wait time before retrying an RPC.
	BackoffWaitBetween time.Duration `json:"backoff-wait-between"`

	// BackoffJitterFraction is the jitter fraction to randomize backoff wait time.
	BackoffJitterFraction float64 `json:"backoff-jitter-fraction"`

	// TODO: support custom balancer picker
}

// ConfigSpec is the configuration from users, which comes from command-line flags,
// environment variables or config file. It is a fully declarative configuration,
// and can be serialized & deserialized to/from JSON.
type ConfigSpec struct {
	Endpoints          []string      `json:"endpoints"`
	RequestTimeout     time.Duration `json:"request-timeout"`
	DialTimeout        time.Duration `json:"dial-timeout"`
	KeepAliveTime      time.Duration `json:"keepalive-time"`
	KeepAliveTimeout   time.Duration `json:"keepalive-timeout"`
	MaxCallSendMsgSize int           `json:"max-request-bytes"`
	MaxCallRecvMsgSize int           `json:"max-recv-bytes"`
	Secure             *SecureConfig `json:"secure"`
	Auth               *AuthConfig   `json:"auth"`
}

type SecureConfig struct {
	Cert       string `json:"cert"`
	Key        string `json:"key"`
	Cacert     string `json:"cacert"`
	ServerName string `json:"server-name"`

	InsecureTransport  bool `json:"insecure-transport"`
	InsecureSkipVerify bool `json:"insecure-skip-tls-verify"`
}

type AuthConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (cs *ConfigSpec) Clone() *ConfigSpec {
	if cs == nil {
		return nil
	}

	clone := *cs

	if len(cs.Endpoints) > 0 {
		clone.Endpoints = make([]string, len(cs.Endpoints))
		copy(clone.Endpoints, cs.Endpoints)
	}

	if cs.Secure != nil {
		clone.Secure = &SecureConfig{}
		*clone.Secure = *cs.Secure
	}
	if cs.Auth != nil {
		clone.Auth = &AuthConfig{}
		*clone.Auth = *cs.Auth
	}

	return &clone
}

func (cfg AuthConfig) Empty() bool {
	return cfg.Username == "" && cfg.Password == ""
}

// NewClientConfig creates a Config based on the provided ConfigSpec.
func NewClientConfig(confSpec *ConfigSpec, lg *zap.Logger) (*Config, error) {
	tlsCfg, err := newTLSConfig(confSpec.Secure, lg)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Endpoints:            confSpec.Endpoints,
		DialTimeout:          confSpec.DialTimeout,
		DialKeepAliveTime:    confSpec.KeepAliveTime,
		DialKeepAliveTimeout: confSpec.KeepAliveTimeout,
		MaxCallSendMsgSize:   confSpec.MaxCallSendMsgSize,
		MaxCallRecvMsgSize:   confSpec.MaxCallRecvMsgSize,
		TLS:                  tlsCfg,
	}

	if confSpec.Auth != nil {
		cfg.Username = confSpec.Auth.Username
		cfg.Password = confSpec.Auth.Password
	}

	return cfg, nil
}

func newTLSConfig(scfg *SecureConfig, lg *zap.Logger) (*tls.Config, error) {
	var (
		tlsCfg *tls.Config
		err    error
	)

	if scfg == nil {
		return nil, nil
	}

	if scfg.Cert != "" || scfg.Key != "" || scfg.Cacert != "" || scfg.ServerName != "" {
		cfgtls := &transport.TLSInfo{
			CertFile:      scfg.Cert,
			KeyFile:       scfg.Key,
			TrustedCAFile: scfg.Cacert,
			ServerName:    scfg.ServerName,
			Logger:        lg,
		}
		if tlsCfg, err = cfgtls.ClientConfig(); err != nil {
			return nil, err
		}
	}

	// If key/cert is not given but user wants secure connection, we
	// should still setup an empty tls configuration for gRPC to setup
	// secure connection.
	if tlsCfg == nil && !scfg.InsecureTransport {
		tlsCfg = &tls.Config{}
	}

	// If the user wants to skip TLS verification then we should set
	// the InsecureSkipVerify flag in tls configuration.
	if scfg.InsecureSkipVerify {
		if tlsCfg == nil {
			tlsCfg = &tls.Config{}
		}
		tlsCfg.InsecureSkipVerify = scfg.InsecureSkipVerify
	}

	return tlsCfg, nil
}
