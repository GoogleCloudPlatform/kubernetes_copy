// Copyright 2021 The etcd Authors
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

package wal

import (
	"fmt"
	"strings"

	"github.com/coreos/go-semver/semver"
	"github.com/golang/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/pkg/v3/pbutil"
	"go.etcd.io/raft/v3/raftpb"
)

// ReadWALVersion reads remaining entries from opened WAL and returns struct
// that implements schema.WAL interface.
func ReadWALVersion(w *WAL) (*walVersion, error) {
	_, _, ents, err := w.ReadAll()
	if err != nil {
		return nil, err
	}
	return &walVersion{entries: ents}, nil
}

type walVersion struct {
	entries []raftpb.Entry
}

// MinimalEtcdVersion returns minimal etcd able to interpret entries from  WAL log,
func (w *walVersion) MinimalEtcdVersion() *semver.Version {
	return MinimalEtcdVersion(w.entries)
}

// MinimalEtcdVersion returns minimal etcd able to interpret entries from  WAL log,
// determined by looking at entries since the last snapshot and returning the highest
// etcd version annotation from used messages, fields, enums and their values.
func MinimalEtcdVersion(ents []raftpb.Entry) *semver.Version {
	var maxVer *semver.Version
	for _, ent := range ents {
		err := visitEntry(ent, func(path protoreflect.FullName, ver *semver.Version) error {
			maxVer = maxVersion(maxVer, ver)
			return nil
		})
		if err != nil {
			panic(err)
		}
	}
	return maxVer
}

type Visitor func(path protoreflect.FullName, ver *semver.Version) error

// VisitFileDescriptor calls visitor on each field and enum value with etcd version read from proto definition.
// If field/enum value is not annotated, visitor will be called with nil.
// Upon encountering invalid annotation, will immediately exit with error.
func VisitFileDescriptor(file protoreflect.FileDescriptor, visitor Visitor) error {
	msgs := file.Messages()
	for i := 0; i < msgs.Len(); i++ {
		err := visitMessageDescriptor(msgs.Get(i), visitor)
		if err != nil {
			return err
		}
	}
	enums := file.Enums()
	for i := 0; i < enums.Len(); i++ {
		err := visitEnumDescriptor(enums.Get(i), visitor)
		if err != nil {
			return err
		}
	}
	return nil
}

func visitEntry(ent raftpb.Entry, visitor Visitor) error {
	err := visitMessage(proto.MessageReflect(&ent), visitor)
	if err != nil {
		return err
	}
	return visitEntryData(ent.Type, ent.Data, visitor)
}

func visitEntryData(entryType raftpb.EntryType, data []byte, visitor Visitor) error {
	var msg protoreflect.Message
	switch entryType {
	case raftpb.EntryNormal:
		var raftReq etcdserverpb.InternalRaftRequest
		if err := pbutil.Unmarshaler(&raftReq).Unmarshal(data); err != nil {
			// try V2 Request
			var r etcdserverpb.Request
			if pbutil.Unmarshaler(&r).Unmarshal(data) != nil {
				// return original error
				return err
			}
			msg = proto.MessageReflect(&r)
			break
		}
		msg = proto.MessageReflect(&raftReq)
		if raftReq.ClusterVersionSet != nil {
			ver, err := semver.NewVersion(raftReq.ClusterVersionSet.Ver)
			if err != nil {
				return err
			}
			err = visitor(msg.Descriptor().FullName(), ver)
			if err != nil {
				return err
			}
		}
	case raftpb.EntryConfChange:
		var confChange raftpb.ConfChange
		err := pbutil.Unmarshaler(&confChange).Unmarshal(data)
		if err != nil {
			return nil
		}
		msg = proto.MessageReflect(&confChange)
		return visitor(msg.Descriptor().FullName(), &version.V3_0)
	case raftpb.EntryConfChangeV2:
		var confChange raftpb.ConfChangeV2
		err := pbutil.Unmarshaler(&confChange).Unmarshal(data)
		if err != nil {
			return nil
		}
		msg = proto.MessageReflect(&confChange)
		return visitor(msg.Descriptor().FullName(), &version.V3_4)
	default:
		panic("unhandled")
	}
	return visitMessage(msg, visitor)
}

func visitMessageDescriptor(md protoreflect.MessageDescriptor, visitor Visitor) error {
	err := visitDescriptor(md, visitor)
	if err != nil {
		return err
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		err = visitDescriptor(fd, visitor)
		if err != nil {
			return err
		}
	}

	enums := md.Enums()
	for i := 0; i < enums.Len(); i++ {
		err = visitEnumDescriptor(enums.Get(i), visitor)
		if err != nil {
			return err
		}
	}
	return err
}

func visitMessage(m protoreflect.Message, visitor Visitor) error {
	md := m.Descriptor()
	err := visitDescriptor(md, visitor)
	if err != nil {
		return err
	}
	m.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		fd := md.Fields().Get(field.Index())
		err = visitDescriptor(fd, visitor)
		if err != nil {
			return false
		}

		switch m := value.Interface().(type) {
		case protoreflect.Message:
			err = visitMessage(m, visitor)
		case protoreflect.EnumNumber:
			err = visitEnumNumber(fd.Enum(), m, visitor)
		}
		if err != nil {
			return false
		}
		return true
	})
	return err
}

func visitEnumDescriptor(enum protoreflect.EnumDescriptor, visitor Visitor) error {
	err := visitDescriptor(enum, visitor)
	if err != nil {
		return err
	}
	fields := enum.Values()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		err = visitDescriptor(fd, visitor)
		if err != nil {
			return err
		}
	}
	return err
}

func visitEnumNumber(enum protoreflect.EnumDescriptor, number protoreflect.EnumNumber, visitor Visitor) error {
	err := visitDescriptor(enum, visitor)
	if err != nil {
		return err
	}
	intNumber := int(number)
	fields := enum.Values()
	if intNumber >= fields.Len() || intNumber < 0 {
		return fmt.Errorf("could not visit EnumNumber [%d]", intNumber)
	}
	return visitEnumValue(fields.Get(intNumber), visitor)
}

func visitEnumValue(enum protoreflect.EnumValueDescriptor, visitor Visitor) error {
	valueOpts := enum.Options().(*descriptorpb.EnumValueOptions)
	if valueOpts != nil {
		ver, _ := etcdVersionFromOptionsString(valueOpts.String())
		err := visitor(enum.FullName(), ver)
		if err != nil {
			return err
		}
	}
	return nil
}

func visitDescriptor(md protoreflect.Descriptor, visitor Visitor) error {
	opts, ok := md.Options().(fmt.Stringer)
	if !ok {
		return nil
	}
	ver, err := etcdVersionFromOptionsString(opts.String())
	if err != nil {
		return fmt.Errorf("%s: %w", md.FullName(), err)
	}
	return visitor(md.FullName(), ver)
}

func maxVersion(a *semver.Version, b *semver.Version) *semver.Version {
	if a != nil && (b == nil || b.LessThan(*a)) {
		return a
	}
	return b
}

func etcdVersionFromOptionsString(opts string) (*semver.Version, error) {
	// TODO: Use proto.GetExtention when gogo/protobuf is usable with protoreflect
	msgs := []string{"[versionpb.etcd_version_msg]:", "[versionpb.etcd_version_field]:", "[versionpb.etcd_version_enum]:", "[versionpb.etcd_version_enum_value]:"}
	var end, index int
	for _, msg := range msgs {
		index = strings.Index(opts, msg)
		end = index + len(msg)
		if index != -1 {
			break
		}
	}
	if index == -1 {
		return nil, nil
	}
	var verStr string
	_, err := fmt.Sscanf(opts[end:], "%q", &verStr)
	if err != nil {
		return nil, err
	}
	if strings.Count(verStr, ".") == 1 {
		verStr = verStr + ".0"
	}
	ver, err := semver.NewVersion(verStr)
	if err != nil {
		return nil, err
	}
	return ver, nil
}
