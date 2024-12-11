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
	"errors"
	"fmt"
	"io"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

type (
	DefragmentResponse pb.DefragmentResponse
	AlarmResponse      pb.AlarmResponse
	AlarmMember        pb.AlarmMember
	StatusResponse     pb.StatusResponse
	HashKVResponse     pb.HashKVResponse
	MoveLeaderResponse pb.MoveLeaderResponse
	DowngradeResponse  pb.DowngradeResponse

	DowngradeAction pb.DowngradeRequest_DowngradeAction
)

const (
	DowngradeValidate = DowngradeAction(pb.DowngradeRequest_VALIDATE)
	DowngradeEnable   = DowngradeAction(pb.DowngradeRequest_ENABLE)
	DowngradeCancel   = DowngradeAction(pb.DowngradeRequest_CANCEL)
)

type Maintenance interface {
	// AlarmList gets all active alarms.
	AlarmList(ctx context.Context) (*AlarmResponse, error)

	// AlarmDisarm disarms a given alarm.
	AlarmDisarm(ctx context.Context, m *AlarmMember) (*AlarmResponse, error)

	// Defragment releases wasted space from internal fragmentation on a given etcd member.
	// Defragment is only needed when deleting a large number of keys and want to reclaim
	// the resources.
	// Defragment is an expensive operation. User should avoid defragmenting multiple members
	// at the same time.
	// To defragment multiple members in the cluster, user need to call defragment multiple
	// times with different endpoints.
	Defragment(ctx context.Context, endpoint string) (*DefragmentResponse, error)

	// Status gets the status of the endpoint.
	Status(ctx context.Context, endpoint string) (*StatusResponse, error)

	// HashKV returns a hash of the KV state at the time of the RPC.
	// If revision is zero, the hash is computed on all keys. If the revision
	// is non-zero, the hash is computed on all keys at or below the given revision.
	HashKV(ctx context.Context, endpoint string, rev int64) (*HashKVResponse, error)

	// SnapshotWithVersion returns a reader for a point-in-time snapshot and version of etcd that created it.
	// If the context "ctx" is canceled or timed out, reading from returned
	// "io.ReadCloser" would error out (e.g. context.Canceled, context.DeadlineExceeded).
	SnapshotWithVersion(ctx context.Context) (*SnapshotResponse, error)

	// Snapshot provides a reader for a point-in-time snapshot of etcd.
	// If the context "ctx" is canceled or timed out, reading from returned
	// "io.ReadCloser" would error out (e.g. context.Canceled, context.DeadlineExceeded).
	// Deprecated: use SnapshotWithVersion instead.
	Snapshot(ctx context.Context) (io.ReadCloser, error)

	// MoveLeader requests current leader to transfer its leadership to the transferee.
	// Request must be made to the leader.
	MoveLeader(ctx context.Context, transfereeID uint64) (*MoveLeaderResponse, error)

	// Downgrade requests downgrades, verifies feasibility or cancels downgrade
	// on the cluster version.
	// Supported since etcd 3.5.
	Downgrade(ctx context.Context, action DowngradeAction, version string) (*DowngradeResponse, error)
}

// SnapshotResponse is aggregated response from the snapshot stream.
// Consumer is responsible for closing steam by calling .Snapshot.Close()
type SnapshotResponse struct {
	// Header is the first header in the snapshot stream, has the current key-value store information
	// and indicates the point in time of the snapshot.
	Header *pb.ResponseHeader
	// Snapshot exposes ReaderCloser interface for data stored in the Blob field in the snapshot stream.
	Snapshot io.ReadCloser
	// Version is the local version of server that created the snapshot.
	// In cluster with binaries with different version, each cluster can return different result.
	// Informs which etcd server version should be used when restoring the snapshot.
	// Supported on etcd >= v3.6.
	Version string
}

type maintenance struct {
	lg       *zap.Logger
	dial     func(endpoint string) (pb.MaintenanceClient, func(), error)
	remote   pb.MaintenanceClient
	callOpts []grpc.CallOption
}

func NewMaintenance(c *Client) Maintenance {
	api := &maintenance{
		lg: c.lg,
		dial: func(endpoint string) (pb.MaintenanceClient, func(), error) {
			conn, err := c.Dial(endpoint)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to dial endpoint %s with maintenance client: %w", endpoint, err)
			}

			cancel := func() { conn.Close() }
			return RetryMaintenanceClient(c, conn), cancel, nil
		},
		remote: RetryMaintenanceClient(c, c.conn),
	}
	if c != nil {
		api.callOpts = c.callOpts
	}
	return api
}

func NewMaintenanceFromMaintenanceClient(remote pb.MaintenanceClient, c *Client) Maintenance {
	api := &maintenance{
		dial: func(string) (pb.MaintenanceClient, func(), error) {
			return remote, func() {}, nil
		},
		remote: remote,
	}
	if c != nil {
		api.callOpts = c.callOpts
		api.lg = c.lg
	}
	return api
}

func (m *maintenance) AlarmList(ctx context.Context) (*AlarmResponse, error) {
	req := &pb.AlarmRequest{
		Action:   pb.AlarmRequest_GET,
		MemberID: 0,                 // all
		Alarm:    pb.AlarmType_NONE, // all
	}
	resp, err := m.remote.Alarm(ctx, req, m.callOpts...)
	if err == nil {
		return (*AlarmResponse)(resp), nil
	}
	return nil, ContextError(ctx, err)
}

func (m *maintenance) AlarmDisarm(ctx context.Context, am *AlarmMember) (*AlarmResponse, error) {
	req := &pb.AlarmRequest{
		Action:   pb.AlarmRequest_DEACTIVATE,
		MemberID: am.MemberID,
		Alarm:    am.Alarm,
	}

	if req.MemberID == 0 && req.Alarm == pb.AlarmType_NONE {
		ar, err := m.AlarmList(ctx)
		if err != nil {
			return nil, ContextError(ctx, err)
		}
		ret := AlarmResponse{}
		for _, am := range ar.Alarms {
			dresp, derr := m.AlarmDisarm(ctx, (*AlarmMember)(am))
			if derr != nil {
				return nil, ContextError(ctx, derr)
			}
			ret.Alarms = append(ret.Alarms, dresp.Alarms...)
		}
		return &ret, nil
	}

	resp, err := m.remote.Alarm(ctx, req, m.callOpts...)
	if err == nil {
		return (*AlarmResponse)(resp), nil
	}
	return nil, ContextError(ctx, err)
}

func (m *maintenance) Defragment(ctx context.Context, endpoint string) (*DefragmentResponse, error) {
	remote, cancel, err := m.dial(endpoint)
	if err != nil {
		return nil, ContextError(ctx, err)
	}
	defer cancel()
	resp, err := remote.Defragment(ctx, &pb.DefragmentRequest{}, m.callOpts...)
	if err != nil {
		return nil, ContextError(ctx, err)
	}
	return (*DefragmentResponse)(resp), nil
}

func (m *maintenance) Status(ctx context.Context, endpoint string) (*StatusResponse, error) {
	remote, cancel, err := m.dial(endpoint)
	if err != nil {
		return nil, ContextError(ctx, err)
	}
	defer cancel()
	resp, err := remote.Status(ctx, &pb.StatusRequest{}, m.callOpts...)
	if err != nil {
		return nil, ContextError(ctx, err)
	}
	return (*StatusResponse)(resp), nil
}

func (m *maintenance) HashKV(ctx context.Context, endpoint string, rev int64) (*HashKVResponse, error) {
	remote, cancel, err := m.dial(endpoint)
	if err != nil {
		return nil, ContextError(ctx, err)
	}
	defer cancel()
	resp, err := remote.HashKV(ctx, &pb.HashKVRequest{Revision: rev}, m.callOpts...)
	if err != nil {
		return nil, ContextError(ctx, err)
	}
	return (*HashKVResponse)(resp), nil
}

func (m *maintenance) SnapshotWithVersion(ctx context.Context) (*SnapshotResponse, error) {
	ss, err := m.remote.Snapshot(ctx, &pb.SnapshotRequest{}, append(m.callOpts, withMax(defaultStreamMaxRetries))...)
	if err != nil {
		return nil, ContextError(ctx, err)
	}

	m.lg.Info("opened snapshot stream; downloading")
	pr, pw := io.Pipe()

	resp, err := ss.Recv()
	if err != nil {
		m.logAndCloseWithError(err, pw)
		return nil, err
	}
	go func() {
		// Saving response is blocking
		err := m.save(resp, pw)
		if err != nil {
			m.logAndCloseWithError(err, pw)
			return
		}
		for {
			sresp, err := ss.Recv()
			if err != nil {
				m.logAndCloseWithError(err, pw)
				return
			}

			err = m.save(sresp, pw)
			if err != nil {
				m.logAndCloseWithError(err, pw)
				return
			}
		}
	}()

	return &SnapshotResponse{
		Header:   resp.GetHeader(),
		Snapshot: &snapshotReadCloser{ctx: ctx, ReadCloser: pr},
		Version:  resp.GetVersion(),
	}, nil
}

func (m *maintenance) Snapshot(ctx context.Context) (io.ReadCloser, error) {
	ss, err := m.remote.Snapshot(ctx, &pb.SnapshotRequest{}, append(m.callOpts, withMax(defaultStreamMaxRetries))...)
	if err != nil {
		return nil, ContextError(ctx, err)
	}

	m.lg.Info("opened snapshot stream; downloading")
	pr, pw := io.Pipe()

	go func() {
		for {
			resp, err := ss.Recv()
			if err != nil {
				m.logAndCloseWithError(err, pw)
				return
			}
			err = m.save(resp, pw)
			if err != nil {
				m.logAndCloseWithError(err, pw)
				return
			}
		}
	}()
	return &snapshotReadCloser{ctx: ctx, ReadCloser: pr}, nil
}

func (m *maintenance) logAndCloseWithError(err error, pw *io.PipeWriter) {
	switch {
	case errors.Is(err, io.EOF):
		m.lg.Info("completed snapshot read; closing")
	default:
		m.lg.Warn("failed to receive from snapshot stream; closing", zap.Error(err))
	}
	pw.CloseWithError(err)
}

func (m *maintenance) save(resp *pb.SnapshotResponse, pw *io.PipeWriter) error {
	// can "resp == nil && err == nil"
	// before we receive snapshot SHA digest?
	// No, server sends EOF with an empty response
	// after it sends SHA digest at the end

	if _, werr := pw.Write(resp.Blob); werr != nil {
		return werr
	}
	return nil
}

type snapshotReadCloser struct {
	ctx context.Context
	io.ReadCloser
}

func (rc *snapshotReadCloser) Read(p []byte) (n int, err error) {
	n, err = rc.ReadCloser.Read(p)
	return n, ContextError(rc.ctx, err)
}

func (m *maintenance) MoveLeader(ctx context.Context, transfereeID uint64) (*MoveLeaderResponse, error) {
	resp, err := m.remote.MoveLeader(ctx, &pb.MoveLeaderRequest{TargetID: transfereeID}, m.callOpts...)
	return (*MoveLeaderResponse)(resp), ContextError(ctx, err)
}

func (m *maintenance) Downgrade(ctx context.Context, action DowngradeAction, version string) (*DowngradeResponse, error) {
	var actionType pb.DowngradeRequest_DowngradeAction
	switch action {
	case DowngradeValidate:
		actionType = pb.DowngradeRequest_VALIDATE
	case DowngradeEnable:
		actionType = pb.DowngradeRequest_ENABLE
	case DowngradeCancel:
		actionType = pb.DowngradeRequest_CANCEL
	default:
		return nil, errors.New("etcdclient: unknown downgrade action")
	}
	resp, err := m.remote.Downgrade(ctx, &pb.DowngradeRequest{Action: actionType, Version: version}, m.callOpts...)
	return (*DowngradeResponse)(resp), ContextError(ctx, err)
}
