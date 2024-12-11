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

// This file defines the http endpoints for etcd health checks.
// The endpoints include /livez, /readyz and /health.

package etcdhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/server/v3/auth"
	"go.etcd.io/etcd/server/v3/config"
	"go.etcd.io/raft/v3"
)

const (
	PathHealth                 = "/health"
	PathProxyHealth            = "/proxy/health"
	HealthStatusSuccess string = "success"
	HealthStatusError   string = "error"
	checkTypeLivez             = "livez"
	checkTypeReadyz            = "readyz"
	checkTypeHealth            = "health"
)

type ServerHealth interface {
	Alarms() []*pb.AlarmMember
	Leader() types.ID
	Range(context.Context, *pb.RangeRequest) (*pb.RangeResponse, error)
	Config() config.ServerConfig
	AuthStore() auth.AuthStore
}

// HandleHealth registers metrics and health handlers. it checks health by using v3 range request
// and its corresponding timeout.
func HandleHealth(lg *zap.Logger, mux *http.ServeMux, srv ServerHealth) {
	mux.Handle(PathHealth, NewHealthHandler(lg, func(ctx context.Context, excludedAlarms StringSet, serializable bool) Health {
		if h := checkAlarms(lg, srv, excludedAlarms); h.Health != "true" {
			return h
		}
		if h := checkLeader(lg, srv, serializable); h.Health != "true" {
			return h
		}
		return checkAPI(ctx, lg, srv, serializable)
	}))

	installLivezEndpoints(lg, mux, srv)
	installReadyzEndpoints(lg, mux, srv)
}

// NewHealthHandler handles '/health' requests.
func NewHealthHandler(lg *zap.Logger, hfunc func(ctx context.Context, excludedAlarms StringSet, Serializable bool) Health) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			lg.Warn("/health error", zap.Int("status-code", http.StatusMethodNotAllowed))
			return
		}
		excludedAlarms := getQuerySet(r, "exclude")
		// Passing the query parameter "serializable=true" ensures that the
		// health of the local etcd is checked vs the health of the cluster.
		// This is useful for probes attempting to validate the liveness of
		// the etcd process vs readiness of the cluster to serve requests.
		serializableFlag := getSerializableFlag(r)
		h := hfunc(r.Context(), excludedAlarms, serializableFlag)
		defer func() {
			if h.Health == "true" {
				healthSuccess.Inc()
			} else {
				healthFailed.Inc()
			}
		}()
		d, _ := json.Marshal(h)
		if h.Health != "true" {
			http.Error(w, string(d), http.StatusServiceUnavailable)
			lg.Warn("/health error", zap.String("output", string(d)), zap.Int("status-code", http.StatusServiceUnavailable))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(d)
		lg.Debug("/health OK", zap.Int("status-code", http.StatusOK))
	}
}

var (
	healthSuccess = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "health_success",
		Help:      "The total number of successful health checks",
	})
	healthFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "health_failures",
		Help:      "The total number of failed health checks",
	})
	healthCheckGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "healthcheck",
		Help:      "The result of each kind of healthcheck.",
	},
		[]string{"type", "name"},
	)
	healthCheckCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "healthchecks_total",
		Help:      "The total number of each kind of healthcheck.",
	},
		[]string{"type", "name", "status"},
	)
)

func init() {
	prometheus.MustRegister(healthSuccess)
	prometheus.MustRegister(healthFailed)
	prometheus.MustRegister(healthCheckGauge)
	prometheus.MustRegister(healthCheckCounter)
}

// Health defines etcd server health status.
// TODO: remove manual parsing in etcdctl cluster-health
type Health struct {
	Health string `json:"health"`
	Reason string `json:"reason"`
}

// HealthStatus is used in new /readyz or /livez health checks instead of the Health struct.
type HealthStatus struct {
	Reason string `json:"reason"`
	Status string `json:"status"`
}

func getQuerySet(r *http.Request, query string) StringSet {
	querySet := make(map[string]struct{})
	qs, found := r.URL.Query()[query]
	if found {
		for _, q := range qs {
			if len(q) == 0 {
				continue
			}
			querySet[q] = struct{}{}
		}
	}
	return querySet
}

func getSerializableFlag(r *http.Request) bool {
	return r.URL.Query().Get("serializable") == "true"
}

// TODO: etcdserver.ErrNoLeader in health API

func checkAlarms(lg *zap.Logger, srv ServerHealth, excludedAlarms StringSet) Health {
	h := Health{Health: "true"}

	for _, v := range srv.Alarms() {
		alarmName := v.Alarm.String()
		if _, found := excludedAlarms[alarmName]; found {
			lg.Debug("/health excluded alarm", zap.String("alarm", v.String()))
			continue
		}

		h.Health = "false"
		switch v.Alarm {
		case pb.AlarmType_NOSPACE:
			h.Reason = "ALARM NOSPACE"
		case pb.AlarmType_CORRUPT:
			h.Reason = "ALARM CORRUPT"
		default:
			h.Reason = "ALARM UNKNOWN"
		}
		lg.Warn("serving /health false due to an alarm", zap.String("alarm", v.String()))
		return h
	}

	return h
}

func checkLeader(lg *zap.Logger, srv ServerHealth, serializable bool) Health {
	h := Health{Health: "true"}
	if !serializable && (uint64(srv.Leader()) == raft.None) {
		h.Health = "false"
		h.Reason = "RAFT NO LEADER"
		lg.Warn("serving /health false; no leader")
	}
	return h
}

func checkAPI(ctx context.Context, lg *zap.Logger, srv ServerHealth, serializable bool) Health {
	h := Health{Health: "true"}
	cfg := srv.Config()
	ctx = srv.AuthStore().WithRoot(ctx)
	cctx, cancel := context.WithTimeout(ctx, cfg.ReqTimeout())
	_, err := srv.Range(cctx, &pb.RangeRequest{KeysOnly: true, Limit: 1, Serializable: serializable})
	cancel()
	if err != nil {
		h.Health = "false"
		h.Reason = fmt.Sprintf("RANGE ERROR:%s", err)
		lg.Warn("serving /health false; Range fails", zap.Error(err))
		return h
	}
	lg.Debug("serving /health true")
	return h
}

type HealthCheck func(ctx context.Context) error

type CheckRegistry struct {
	checkType string
	checks    map[string]HealthCheck
}

func installLivezEndpoints(lg *zap.Logger, mux *http.ServeMux, server ServerHealth) {
	reg := CheckRegistry{checkType: checkTypeLivez, checks: make(map[string]HealthCheck)}
	reg.Register("serializable_read", readCheck(server, true /* serializable */))
	reg.InstallHTTPEndpoints(lg, mux)
}

func installReadyzEndpoints(lg *zap.Logger, mux *http.ServeMux, server ServerHealth) {
	reg := CheckRegistry{checkType: checkTypeReadyz, checks: make(map[string]HealthCheck)}
	reg.Register("data_corruption", activeAlarmCheck(server, pb.AlarmType_CORRUPT))
	// serializable_read checks if local read is ok.
	// linearizable_read checks if there is consensus in the cluster.
	// Having both serializable_read and linearizable_read helps isolate the cause of problems if there is a read failure.
	reg.Register("serializable_read", readCheck(server, true))
	// linearizable_read check would be replaced by read_index check in 3.6
	reg.Register("linearizable_read", readCheck(server, false))
	reg.InstallHTTPEndpoints(lg, mux)
}

func (reg *CheckRegistry) Register(name string, check HealthCheck) {
	reg.checks[name] = check
}

func (reg *CheckRegistry) RootPath() string {
	return "/" + reg.checkType
}

// InstallHttpEndpoints installs the http handlers for the health checks.
//
// Deprecated: Please use (*CheckRegistry) InstallHTTPEndpoints instead.
//
//revive:disable-next-line:var-naming
func (reg *CheckRegistry) InstallHttpEndpoints(lg *zap.Logger, mux *http.ServeMux) {
	reg.InstallHTTPEndpoints(lg, mux)
}

func (reg *CheckRegistry) InstallHTTPEndpoints(lg *zap.Logger, mux *http.ServeMux) {
	checkNames := make([]string, 0, len(reg.checks))
	for k := range reg.checks {
		checkNames = append(checkNames, k)
	}

	// installs the http handler for the root path.
	reg.installRootHTTPEndpoint(lg, mux, checkNames...)
	for _, checkName := range checkNames {
		// installs the http handler for the individual check sub path.
		subpath := path.Join(reg.RootPath(), checkName)
		check := checkName
		mux.Handle(subpath, newHealthHandler(subpath, lg, func(r *http.Request) HealthStatus {
			return reg.runHealthChecks(r.Context(), check)
		}))
	}
}

func (reg *CheckRegistry) runHealthChecks(ctx context.Context, checkNames ...string) HealthStatus {
	h := HealthStatus{Status: HealthStatusSuccess}
	var individualCheckOutput bytes.Buffer
	for _, checkName := range checkNames {
		check, found := reg.checks[checkName]
		if !found {
			panic(fmt.Errorf("Health check: %s not registered", checkName))
		}
		if err := check(ctx); err != nil {
			fmt.Fprintf(&individualCheckOutput, "[-]%s failed: %v\n", checkName, err)
			h.Status = HealthStatusError
			recordMetrics(reg.checkType, checkName, HealthStatusError)
		} else {
			fmt.Fprintf(&individualCheckOutput, "[+]%s ok\n", checkName)
			recordMetrics(reg.checkType, checkName, HealthStatusSuccess)
		}
	}
	h.Reason = individualCheckOutput.String()
	return h
}

// installRootHTTPEndpoint installs the http handler for the root path.
func (reg *CheckRegistry) installRootHTTPEndpoint(lg *zap.Logger, mux *http.ServeMux, checks ...string) {
	hfunc := func(r *http.Request) HealthStatus {
		// extracts the health check names to be excludeList from the query param
		excluded := getQuerySet(r, "exclude")

		filteredCheckNames := filterCheckList(lg, listToStringSet(checks), excluded)
		h := reg.runHealthChecks(r.Context(), filteredCheckNames...)
		return h
	}
	mux.Handle(reg.RootPath(), newHealthHandler(reg.RootPath(), lg, hfunc))
}

// newHealthHandler generates a http HandlerFunc for a health check function hfunc.
func newHealthHandler(path string, lg *zap.Logger, hfunc func(*http.Request) HealthStatus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			lg.Warn("Health request error", zap.String("path", path), zap.Int("status-code", http.StatusMethodNotAllowed))
			return
		}
		h := hfunc(r)
		// Always returns detailed reason for failed checks.
		if h.Status == HealthStatusError {
			http.Error(w, h.Reason, http.StatusServiceUnavailable)
			lg.Error("Health check error", zap.String("path", path), zap.String("reason", h.Reason), zap.Int("status-code", http.StatusServiceUnavailable))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Only writes detailed reason for verbose requests.
		if _, found := r.URL.Query()["verbose"]; found {
			fmt.Fprint(w, h.Reason)
		}
		fmt.Fprint(w, "ok\n")
		lg.Debug("Health check OK", zap.String("path", path), zap.String("reason", h.Reason), zap.Int("status-code", http.StatusOK))
	}
}

func filterCheckList(lg *zap.Logger, checks StringSet, excluded StringSet) []string {
	filteredList := []string{}
	for chk := range checks {
		if _, found := excluded[chk]; found {
			delete(excluded, chk)
			continue
		}
		filteredList = append(filteredList, chk)
	}
	if len(excluded) > 0 {
		// For version compatibility, excluding non-exist checks would not fail the request.
		lg.Warn("some health checks cannot be excluded", zap.String("missing-health-checks", formatQuoted(excluded.List()...)))
	}
	return filteredList
}

// formatQuoted returns a formatted string of the health check names,
// preserving the order passed in.
func formatQuoted(names ...string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return strings.Join(quoted, ",")
}

type StringSet map[string]struct{}

func (s StringSet) List() []string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	return keys
}

func listToStringSet(list []string) StringSet {
	set := make(map[string]struct{})
	for _, s := range list {
		set[s] = struct{}{}
	}
	return set
}

func recordMetrics(checkType, name string, status string) {
	val := 0.0
	if status == HealthStatusSuccess {
		val = 1.0
	}
	healthCheckGauge.With(prometheus.Labels{
		"type": checkType,
		"name": name,
	}).Set(val)
	healthCheckCounter.With(prometheus.Labels{
		"type":   checkType,
		"name":   name,
		"status": status,
	}).Inc()
}

// activeAlarmCheck checks if a specific alarm type is active in the server.
func activeAlarmCheck(srv ServerHealth, at pb.AlarmType) func(context.Context) error {
	return func(ctx context.Context) error {
		as := srv.Alarms()
		for _, v := range as {
			if v.Alarm == at {
				return fmt.Errorf("alarm activated: %s", at.String())
			}
		}
		return nil
	}
}

func readCheck(srv ServerHealth, serializable bool) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		ctx = srv.AuthStore().WithRoot(ctx)
		_, err := srv.Range(ctx, &pb.RangeRequest{KeysOnly: true, Limit: 1, Serializable: serializable})
		return err
	}
}
