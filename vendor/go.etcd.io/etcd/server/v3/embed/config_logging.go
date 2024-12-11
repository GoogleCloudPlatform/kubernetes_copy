// Copyright 2018 The etcd Authors
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

package embed

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zapgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
	"gopkg.in/natefinch/lumberjack.v2"

	"go.etcd.io/etcd/client/pkg/v3/logutil"
)

// GetLogger returns the logger.
func (cfg *Config) GetLogger() *zap.Logger {
	cfg.loggerMu.RLock()
	l := cfg.logger
	cfg.loggerMu.RUnlock()
	return l
}

// setupLogging initializes etcd logging.
// Must be called after flag parsing or finishing configuring embed.Config.
func (cfg *Config) setupLogging() error {
	switch cfg.Logger {
	case "capnslog": // removed in v3.5
		return fmt.Errorf("--logger=capnslog is removed in v3.5")

	case "zap":
		if len(cfg.LogOutputs) == 0 {
			cfg.LogOutputs = []string{DefaultLogOutput}
		}
		if len(cfg.LogOutputs) > 1 {
			for _, v := range cfg.LogOutputs {
				if v == DefaultLogOutput {
					return fmt.Errorf("multi logoutput for %q is not supported yet", DefaultLogOutput)
				}
			}
		}
		if cfg.EnableLogRotation {
			if err := setupLogRotation(cfg.LogOutputs, cfg.LogRotationConfigJSON); err != nil {
				return err
			}
		}

		outputPaths, errOutputPaths := make([]string, 0), make([]string, 0)
		isJournal := false
		for _, v := range cfg.LogOutputs {
			switch v {
			case DefaultLogOutput:
				outputPaths = append(outputPaths, StdErrLogOutput)
				errOutputPaths = append(errOutputPaths, StdErrLogOutput)

			case JournalLogOutput:
				isJournal = true

			case StdErrLogOutput:
				outputPaths = append(outputPaths, StdErrLogOutput)
				errOutputPaths = append(errOutputPaths, StdErrLogOutput)

			case StdOutLogOutput:
				outputPaths = append(outputPaths, StdOutLogOutput)
				errOutputPaths = append(errOutputPaths, StdOutLogOutput)

			default:
				var path string
				if cfg.EnableLogRotation {
					// append rotate scheme to logs managed by lumberjack log rotation
					if v[0:1] == "/" {
						path = fmt.Sprintf("rotate:/%%2F%s", v[1:])
					} else {
						path = fmt.Sprintf("rotate:/%s", v)
					}
				} else {
					path = v
				}
				outputPaths = append(outputPaths, path)
				errOutputPaths = append(errOutputPaths, path)
			}
		}

		if !isJournal {
			copied := logutil.DefaultZapLoggerConfig
			copied.OutputPaths = outputPaths
			copied.ErrorOutputPaths = errOutputPaths
			copied = logutil.MergeOutputPaths(copied)
			copied.Level = zap.NewAtomicLevelAt(logutil.ConvertToZapLevel(cfg.LogLevel))
			encoding, err := logutil.ConvertToZapFormat(cfg.LogFormat)
			if err != nil {
				return err
			}
			copied.Encoding = encoding
			if cfg.ZapLoggerBuilder == nil {
				lg, err := copied.Build()
				if err != nil {
					return err
				}
				cfg.ZapLoggerBuilder = NewZapLoggerBuilder(lg)
			}
		} else {
			if len(cfg.LogOutputs) > 1 {
				for _, v := range cfg.LogOutputs {
					if v != DefaultLogOutput {
						return fmt.Errorf("running with systemd/journal but other '--log-outputs' values (%q) are configured with 'default'; override 'default' value with something else", cfg.LogOutputs)
					}
				}
			}

			// use stderr as fallback
			syncer, lerr := getJournalWriteSyncer()
			if lerr != nil {
				return lerr
			}

			lvl := zap.NewAtomicLevelAt(logutil.ConvertToZapLevel(cfg.LogLevel))

			var encoder zapcore.Encoder
			encoding, err := logutil.ConvertToZapFormat(cfg.LogFormat)
			if err != nil {
				return err
			}

			if encoding == logutil.ConsoleLogFormat {
				encoder = zapcore.NewConsoleEncoder(logutil.DefaultZapLoggerConfig.EncoderConfig)
			} else {
				encoder = zapcore.NewJSONEncoder(logutil.DefaultZapLoggerConfig.EncoderConfig)
			}

			// WARN: do not change field names in encoder config
			// journald logging writer assumes field names of "level" and "caller"
			cr := zapcore.NewCore(
				encoder,
				syncer,
				lvl,
			)
			if cfg.ZapLoggerBuilder == nil {
				cfg.ZapLoggerBuilder = NewZapLoggerBuilder(zap.New(cr, zap.AddCaller(), zap.ErrorOutput(syncer)))
			}
		}

		err := cfg.ZapLoggerBuilder(cfg)
		if err != nil {
			return err
		}

		logTLSHandshakeFailureFunc := func(msg string) func(conn *tls.Conn, err error) {
			return func(conn *tls.Conn, err error) {
				state := conn.ConnectionState()
				remoteAddr := conn.RemoteAddr().String()
				serverName := state.ServerName
				if len(state.PeerCertificates) > 0 {
					cert := state.PeerCertificates[0]
					ips := make([]string, len(cert.IPAddresses))
					for i := range cert.IPAddresses {
						ips[i] = cert.IPAddresses[i].String()
					}
					cfg.logger.Warn(
						msg,
						zap.String("remote-addr", remoteAddr),
						zap.String("server-name", serverName),
						zap.Strings("ip-addresses", ips),
						zap.Strings("dns-names", cert.DNSNames),
						zap.Error(err),
					)
				} else {
					cfg.logger.Warn(
						msg,
						zap.String("remote-addr", remoteAddr),
						zap.String("server-name", serverName),
						zap.Error(err),
					)
				}
			}
		}

		cfg.ClientTLSInfo.HandshakeFailure = logTLSHandshakeFailureFunc("rejected connection on client endpoint")
		cfg.PeerTLSInfo.HandshakeFailure = logTLSHandshakeFailureFunc("rejected connection on peer endpoint")

	default:
		return fmt.Errorf("unknown logger option %q", cfg.Logger)
	}

	return nil
}

// NewZapLoggerBuilder generates a zap logger builder that sets given logger
// for embedded etcd.
func NewZapLoggerBuilder(lg *zap.Logger) func(*Config) error {
	return func(cfg *Config) error {
		cfg.loggerMu.Lock()
		defer cfg.loggerMu.Unlock()
		cfg.logger = lg
		return nil
	}
}

// NewZapCoreLoggerBuilder - is a deprecated setter for the logger.
// Deprecated: Use simpler NewZapLoggerBuilder. To be removed in etcd-3.6.
func NewZapCoreLoggerBuilder(lg *zap.Logger, _ zapcore.Core, _ zapcore.WriteSyncer) func(*Config) error {
	return NewZapLoggerBuilder(lg)
}

// SetupGlobalLoggers configures 'global' loggers (grpc, zapGlobal) based on the cfg.
//
// The method is not executed by embed server by default (since 3.5) to
// enable setups where grpc/zap.Global logging is configured independently
// or spans separate lifecycle (like in tests).
func (cfg *Config) SetupGlobalLoggers() {
	lg := cfg.GetLogger()
	if lg != nil {
		if cfg.LogLevel == "debug" {
			grpc.EnableTracing = true
			grpclog.SetLoggerV2(zapgrpc.NewLogger(lg))
		} else {
			grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, os.Stderr, os.Stderr))
		}
		zap.ReplaceGlobals(lg)
	}
}

type logRotationConfig struct {
	*lumberjack.Logger
}

// Sync implements zap.Sink
func (logRotationConfig) Sync() error { return nil }

// setupLogRotation initializes log rotation for a single file path target.
func setupLogRotation(logOutputs []string, logRotateConfigJSON string) error {
	var logRotationCfg logRotationConfig
	outputFilePaths := 0
	for _, v := range logOutputs {
		switch v {
		case DefaultLogOutput, StdErrLogOutput, StdOutLogOutput:
			continue
		default:
			outputFilePaths++
		}
	}
	// log rotation requires file target
	if len(logOutputs) == 1 && outputFilePaths == 0 {
		return ErrLogRotationInvalidLogOutput
	}
	// support max 1 file target for log rotation
	if outputFilePaths > 1 {
		return ErrLogRotationInvalidLogOutput
	}

	if err := json.Unmarshal([]byte(logRotateConfigJSON), &logRotationCfg); err != nil {
		var unmarshalTypeError *json.UnmarshalTypeError
		var syntaxError *json.SyntaxError
		switch {
		case errors.As(err, &syntaxError):
			return fmt.Errorf("improperly formatted log rotation config: %w", err)
		case errors.As(err, &unmarshalTypeError):
			return fmt.Errorf("invalid log rotation config: %w", err)
		default:
			return fmt.Errorf("fail to unmarshal log rotation config: %w", err)
		}
	}
	zap.RegisterSink("rotate", func(u *url.URL) (zap.Sink, error) {
		logRotationCfg.Filename = u.Path[1:]
		return &logRotationCfg, nil
	})
	return nil
}
