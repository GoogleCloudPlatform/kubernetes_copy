/*
Copyright 2022 The Kubernetes Authors.

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

package nodeshutdown

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	filesystem "k8s.io/kubernetes/pkg/util/filesystem"
)

type storage interface {
	Store(data interface{}) (err error)
	Load(data interface{}) (err error)
}

type localStorage struct {
	Path string
}

func (l localStorage) Store(data interface{}) (err error) {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return atomicWrite(l.Path, b, 0644)
}

func (l localStorage) Load(data interface{}) (err error) {
	b, err := os.ReadFile(l.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(b, data)
}

func timestamp(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}

type state struct {
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
}

// atomicWrite atomically writes data to a file specified by filename.
func atomicWrite(filename string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(filename), ".tmp-"+filepath.Base(filename))
	if err != nil {
		return err
	}
	err = filesystem.Chmod(f.Name(), perm)
	if err != nil {
		f.Close()
		return err
	}
	n, err := f.Write(data)
	if err != nil {
		f.Close()
		return err
	}
	if n < len(data) {
		f.Close()
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filename)
}
