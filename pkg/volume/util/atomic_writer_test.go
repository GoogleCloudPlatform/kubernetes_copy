//go:build linux
// +build linux

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

package util

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	utiltesting "k8s.io/client-go/util/testing"
)

func TestNewAtomicWriter(t *testing.T) {
	targetDir, err := utiltesting.MkTmpdir("atomic-write")
	if err != nil {
		t.Fatalf("unexpected error creating tmp dir: %v", err)
	}
	defer os.RemoveAll(targetDir)

	_, err = NewAtomicWriter(targetDir, "-test-")
	if err != nil {
		t.Fatalf("unexpected error creating writer for existing target dir: %v", err)
	}

	nonExistentDir, err := utiltesting.MkTmpdir("atomic-write")
	if err != nil {
		t.Fatalf("unexpected error creating tmp dir: %v", err)
	}
	err = os.Remove(nonExistentDir)
	if err != nil {
		t.Fatalf("unexpected error ensuring dir %v does not exist: %v", nonExistentDir, err)
	}

	_, err = NewAtomicWriter(nonExistentDir, "-test-")
	if err == nil {
		t.Fatalf("unexpected success creating writer for nonexistent target dir: %v", err)
	}
}

func TestValidatePath(t *testing.T) {
	maxPath := strings.Repeat("a", maxPathLength+1)
	maxFile := strings.Repeat("a", maxFileNameLength+1)

	cases := []struct {
		name  string
		path  string
		valid bool
	}{
		{
			name:  "valid 1",
			path:  "i/am/well/behaved.txt",
			valid: true,
		},
		{
			name:  "valid 2",
			path:  "keepyourheaddownandfollowtherules.txt",
			valid: true,
		},
		{
			name:  "max path length",
			path:  maxPath,
			valid: false,
		},
		{
			name:  "max file length",
			path:  maxFile,
			valid: false,
		},
		{
			name:  "absolute failure",
			path:  "/dev/null",
			valid: false,
		},
		{
			name:  "reserved path",
			path:  "..sneaky.txt",
			valid: false,
		},
		{
			name:  "contains doubledot 1",
			path:  "hello/there/../../../../../../etc/passwd",
			valid: false,
		},
		{
			name:  "contains doubledot 2",
			path:  "hello/../etc/somethingbad",
			valid: false,
		},
		{
			name:  "empty",
			path:  "",
			valid: false,
		},
	}

	for _, tc := range cases {
		err := validatePath(tc.path)
		if tc.valid && err != nil {
			t.Errorf("%v: unexpected failure: %v", tc.name, err)
			continue
		}

		if !tc.valid && err == nil {
			t.Errorf("%v: unexpected success", tc.name)
		}
	}
}

func TestPathsToRemove(t *testing.T) {
	cases := []struct {
		name     string
		payload1 map[string]FileProjection
		payload2 map[string]FileProjection
		expected sets.Set[string]
	}{
		{
			name: "simple",
			payload1: map[string]FileProjection{
				"foo.txt": {Mode: 0644, Data: []byte("foo")},
				"bar.txt": {Mode: 0644, Data: []byte("bar")},
			},
			payload2: map[string]FileProjection{
				"foo.txt": {Mode: 0644, Data: []byte("foo")},
			},
			expected: sets.New[string]("bar.txt"),
		},
		{
			name: "simple 2",
			payload1: map[string]FileProjection{
				"foo.txt":     {Mode: 0644, Data: []byte("foo")},
				"zip/bar.txt": {Mode: 0644, Data: []byte("zip/b}ar")},
			},
			payload2: map[string]FileProjection{
				"foo.txt": {Mode: 0644, Data: []byte("foo")},
			},
			expected: sets.New[string]("zip/bar.txt", "zip"),
		},
		{
			name: "subdirs 1",
			payload1: map[string]FileProjection{
				"foo.txt":         {Mode: 0644, Data: []byte("foo")},
				"zip/zap/bar.txt": {Mode: 0644, Data: []byte("zip/bar")},
			},
			payload2: map[string]FileProjection{
				"foo.txt": {Mode: 0644, Data: []byte("foo")},
			},
			expected: sets.New[string]("zip/zap/bar.txt", "zip", "zip/zap"),
		},
		{
			name: "subdirs 2",
			payload1: map[string]FileProjection{
				"foo.txt":             {Mode: 0644, Data: []byte("foo")},
				"zip/1/2/3/4/bar.txt": {Mode: 0644, Data: []byte("zip/b}ar")},
			},
			payload2: map[string]FileProjection{
				"foo.txt": {Mode: 0644, Data: []byte("foo")},
			},
			expected: sets.New[string]("zip/1/2/3/4/bar.txt", "zip", "zip/1", "zip/1/2", "zip/1/2/3", "zip/1/2/3/4"),
		},
		{
			name: "subdirs 3",
			payload1: map[string]FileProjection{
				"foo.txt":             {Mode: 0644, Data: []byte("foo")},
				"zip/1/2/3/4/bar.txt": {Mode: 0644, Data: []byte("zip/b}ar")},
				"zap/a/b/c/bar.txt":   {Mode: 0644, Data: []byte("zap/bar")},
			},
			payload2: map[string]FileProjection{
				"foo.txt": {Mode: 0644, Data: []byte("foo")},
			},
			expected: sets.New[string]("zip/1/2/3/4/bar.txt", "zip", "zip/1", "zip/1/2", "zip/1/2/3", "zip/1/2/3/4", "zap", "zap/a", "zap/a/b", "zap/a/b/c", "zap/a/b/c/bar.txt"),
		},
		{
			name: "subdirs 4",
			payload1: map[string]FileProjection{
				"foo.txt":             {Mode: 0644, Data: []byte("foo")},
				"zap/1/2/3/4/bar.txt": {Mode: 0644, Data: []byte("zip/bar")},
				"zap/1/2/c/bar.txt":   {Mode: 0644, Data: []byte("zap/bar")},
				"zap/1/2/magic.txt":   {Mode: 0644, Data: []byte("indigo")},
			},
			payload2: map[string]FileProjection{
				"foo.txt":           {Mode: 0644, Data: []byte("foo")},
				"zap/1/2/magic.txt": {Mode: 0644, Data: []byte("indigo")},
			},
			expected: sets.New[string]("zap/1/2/3/4/bar.txt", "zap/1/2/3", "zap/1/2/3/4", "zap/1/2/3/4/bar.txt", "zap/1/2/c", "zap/1/2/c/bar.txt"),
		},
		{
			name: "subdirs 5",
			payload1: map[string]FileProjection{
				"foo.txt":             {Mode: 0644, Data: []byte("foo")},
				"zap/1/2/3/4/bar.txt": {Mode: 0644, Data: []byte("zip/bar")},
				"zap/1/2/c/bar.txt":   {Mode: 0644, Data: []byte("zap/bar")},
			},
			payload2: map[string]FileProjection{
				"foo.txt":           {Mode: 0644, Data: []byte("foo")},
				"zap/1/2/magic.txt": {Mode: 0644, Data: []byte("indigo")},
			},
			expected: sets.New[string]("zap/1/2/3/4/bar.txt", "zap/1/2/3", "zap/1/2/3/4", "zap/1/2/3/4/bar.txt", "zap/1/2/c", "zap/1/2/c/bar.txt"),
		},
	}

	for _, tc := range cases {
		targetDir, err := utiltesting.MkTmpdir("atomic-write")
		if err != nil {
			t.Errorf("%v: unexpected error creating tmp dir: %v", tc.name, err)
			continue
		}
		defer os.RemoveAll(targetDir)

		writer := &AtomicWriter{targetDir: targetDir, logContext: "-test-"}
		err = writer.Write(tc.payload1, nil)
		if err != nil {
			t.Errorf("%v: unexpected error writing: %v", tc.name, err)
			continue
		}

		dataDirPath := filepath.Join(targetDir, dataDirName)
		oldTsDir, err := os.Readlink(dataDirPath)
		if err != nil && os.IsNotExist(err) {
			t.Errorf("Data symlink does not exist: %v", dataDirPath)
			continue
		} else if err != nil {
			t.Errorf("Unable to read symlink %v: %v", dataDirPath, err)
			continue
		}

		actual, _, err := writer.pathsToRemove(tc.payload2, filepath.Join(targetDir, oldTsDir))
		if err != nil {
			t.Errorf("%v: unexpected error determining paths to remove: %v", tc.name, err)
			continue
		}

		if e, a := tc.expected, actual; !e.Equal(a) {
			t.Errorf("%v: unexpected paths to remove:\nexpected: %v\n     got: %v", tc.name, e, a)
		}
	}
}

func TestWriteOnce(t *testing.T) {
	// $1 if you can tell me what this binary is
	encodedMysteryBinary := `f0VMRgIBAQAAAAAAAAAAAAIAPgABAAAAeABAAAAAAABAAAAAAAAAAAAAAAAAAAAAAAAAAEAAOAAB
AAAAAAAAAAEAAAAFAAAAAAAAAAAAAAAAAEAAAAAAAAAAQAAAAAAAfQAAAAAAAAB9AAAAAAAAAAAA
IAAAAAAAsDyZDwU=`

	mysteryBinaryBytes := make([]byte, base64.StdEncoding.DecodedLen(len(encodedMysteryBinary)))
	numBytes, err := base64.StdEncoding.Decode(mysteryBinaryBytes, []byte(encodedMysteryBinary))
	if err != nil {
		t.Fatalf("Unexpected error decoding binary payload: %v", err)
	}

	if numBytes != 125 {
		t.Fatalf("Unexpected decoded binary size: expected 125, got %v", numBytes)
	}

	cases := []struct {
		name    string
		payload map[string]FileProjection
		success bool
	}{
		{
			name: "invalid payload 1",
			payload: map[string]FileProjection{
				"foo":        {Mode: 0644, Data: []byte("foo")},
				"..bar":      {Mode: 0644, Data: []byte("bar")},
				"binary.bin": {Mode: 0644, Data: mysteryBinaryBytes},
			},
			success: false,
		},
		{
			name: "invalid payload 2",
			payload: map[string]FileProjection{
				"foo/../bar": {Mode: 0644, Data: []byte("foo")},
			},
			success: false,
		},
		{
			name: "basic 1",
			payload: map[string]FileProjection{
				"foo": {Mode: 0644, Data: []byte("foo")},
				"bar": {Mode: 0644, Data: []byte("bar")},
			},
			success: true,
		},
		{
			name: "basic 2",
			payload: map[string]FileProjection{
				"binary.bin":  {Mode: 0644, Data: mysteryBinaryBytes},
				".binary.bin": {Mode: 0644, Data: mysteryBinaryBytes},
			},
			success: true,
		},
		{
			name: "basic mode 1",
			payload: map[string]FileProjection{
				"foo": {Mode: 0777, Data: []byte("foo")},
				"bar": {Mode: 0400, Data: []byte("bar")},
			},
			success: true,
		},
		{
			name: "dotfiles",
			payload: map[string]FileProjection{
				"foo":           {Mode: 0644, Data: []byte("foo")},
				"bar":           {Mode: 0644, Data: []byte("bar")},
				".dotfile":      {Mode: 0644, Data: []byte("dotfile")},
				".dotfile.file": {Mode: 0644, Data: []byte("dotfile.file")},
			},
			success: true,
		},
		{
			name: "dotfiles mode",
			payload: map[string]FileProjection{
				"foo":           {Mode: 0407, Data: []byte("foo")},
				"bar":           {Mode: 0440, Data: []byte("bar")},
				".dotfile":      {Mode: 0777, Data: []byte("dotfile")},
				".dotfile.file": {Mode: 0666, Data: []byte("dotfile.file")},
			},
			success: true,
		},
		{
			name: "subdirectories 1",
			payload: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo/bar")},
				"bar/zab.txt": {Mode: 0644, Data: []byte("bar/zab.txt")},
			},
			success: true,
		},
		{
			name: "subdirectories mode 1",
			payload: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0400, Data: []byte("foo/bar")},
				"bar/zab.txt": {Mode: 0644, Data: []byte("bar/zab.txt")},
			},
			success: true,
		},
		{
			name: "subdirectories 2",
			payload: map[string]FileProjection{
				"foo//bar.txt":      {Mode: 0644, Data: []byte("foo//bar")},
				"bar///bar/zab.txt": {Mode: 0644, Data: []byte("bar/../bar/zab.txt")},
			},
			success: true,
		},
		{
			name: "subdirectories 3",
			payload: map[string]FileProjection{
				"foo/bar.txt":      {Mode: 0644, Data: []byte("foo/bar")},
				"bar/zab.txt":      {Mode: 0644, Data: []byte("bar/zab.txt")},
				"foo/blaz/bar.txt": {Mode: 0644, Data: []byte("foo/blaz/bar")},
				"bar/zib/zab.txt":  {Mode: 0644, Data: []byte("bar/zib/zab.txt")},
			},
			success: true,
		},
		{
			name: "kitchen sink",
			payload: map[string]FileProjection{
				"foo.log":                           {Mode: 0644, Data: []byte("foo")},
				"bar.zap":                           {Mode: 0644, Data: []byte("bar")},
				".dotfile":                          {Mode: 0644, Data: []byte("dotfile")},
				"foo/bar.txt":                       {Mode: 0644, Data: []byte("foo/bar")},
				"bar/zab.txt":                       {Mode: 0644, Data: []byte("bar/zab.txt")},
				"foo/blaz/bar.txt":                  {Mode: 0644, Data: []byte("foo/blaz/bar")},
				"bar/zib/zab.txt":                   {Mode: 0400, Data: []byte("bar/zib/zab.txt")},
				"1/2/3/4/5/6/7/8/9/10/.dotfile.lib": {Mode: 0777, Data: []byte("1-2-3-dotfile")},
			},
			success: true,
		},
	}

	for _, tc := range cases {
		targetDir, err := utiltesting.MkTmpdir("atomic-write")
		if err != nil {
			t.Errorf("%v: unexpected error creating tmp dir: %v", tc.name, err)
			continue
		}
		defer os.RemoveAll(targetDir)

		writer := &AtomicWriter{targetDir: targetDir, logContext: "-test-"}
		err = writer.Write(tc.payload, nil)
		if err != nil && tc.success {
			t.Errorf("%v: unexpected error writing payload: %v", tc.name, err)
			continue
		} else if err == nil && !tc.success {
			t.Errorf("%v: unexpected success", tc.name)
			continue
		} else if err != nil {
			continue
		}

		checkVolumeContents(targetDir, tc.name, tc.payload, t)
	}
}

func TestUpdate(t *testing.T) {
	cases := []struct {
		name        string
		first       map[string]FileProjection
		next        map[string]FileProjection
		shouldWrite bool
	}{
		{
			name: "update",
			first: map[string]FileProjection{
				"foo": {Mode: 0644, Data: []byte("foo")},
				"bar": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo": {Mode: 0644, Data: []byte("foo2")},
				"bar": {Mode: 0640, Data: []byte("bar2")},
			},
			shouldWrite: true,
		},
		{
			name: "no update",
			first: map[string]FileProjection{
				"foo": {Mode: 0644, Data: []byte("foo")},
				"bar": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo": {Mode: 0644, Data: []byte("foo")},
				"bar": {Mode: 0644, Data: []byte("bar")},
			},
			shouldWrite: false,
		},
		{
			name: "no update 2",
			first: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
				"bar/zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
				"bar/zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			shouldWrite: false,
		},
		{
			name: "add 1",
			first: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
				"bar/zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
				"bar/zab.txt": {Mode: 0644, Data: []byte("bar")},
				"blu/zip.txt": {Mode: 0644, Data: []byte("zip")},
			},
			shouldWrite: true,
		},
		{
			name: "add 2",
			first: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
				"bar/zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo/bar.txt":             {Mode: 0644, Data: []byte("foo")},
				"bar/zab.txt":             {Mode: 0644, Data: []byte("bar")},
				"blu/two/2/3/4/5/zip.txt": {Mode: 0644, Data: []byte("zip")},
			},
			shouldWrite: true,
		},
		{
			name: "add 3",
			first: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
				"bar/zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo/bar.txt":         {Mode: 0644, Data: []byte("foo")},
				"bar/zab.txt":         {Mode: 0644, Data: []byte("bar")},
				"bar/2/3/4/5/zip.txt": {Mode: 0644, Data: []byte("zip")},
			},
			shouldWrite: true,
		},
		{
			name: "delete 1",
			first: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
				"bar/zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
			},
			shouldWrite: true,
		},
		{
			name: "delete 2",
			first: map[string]FileProjection{
				"foo/bar.txt":       {Mode: 0644, Data: []byte("foo")},
				"bar/1/2/3/zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
			},
			shouldWrite: true,
		},
		{
			name: "delete 3",
			first: map[string]FileProjection{
				"foo/bar.txt":       {Mode: 0644, Data: []byte("foo")},
				"bar/1/2/sip.txt":   {Mode: 0644, Data: []byte("sip")},
				"bar/1/2/3/zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo/bar.txt":     {Mode: 0644, Data: []byte("foo")},
				"bar/1/2/sip.txt": {Mode: 0644, Data: []byte("sip")},
			},
			shouldWrite: true,
		},
		{
			name: "delete 4",
			first: map[string]FileProjection{
				"foo/bar.txt":            {Mode: 0644, Data: []byte("foo")},
				"bar/1/2/sip.txt":        {Mode: 0644, Data: []byte("sip")},
				"bar/1/2/3/4/5/6zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			next: map[string]FileProjection{
				"foo/bar.txt":     {Mode: 0644, Data: []byte("foo")},
				"bar/1/2/sip.txt": {Mode: 0644, Data: []byte("sip")},
			},
			shouldWrite: true,
		},
		{
			name: "delete all",
			first: map[string]FileProjection{
				"foo/bar.txt":            {Mode: 0644, Data: []byte("foo")},
				"bar/1/2/sip.txt":        {Mode: 0644, Data: []byte("sip")},
				"bar/1/2/3/4/5/6zab.txt": {Mode: 0644, Data: []byte("bar")},
			},
			next:        map[string]FileProjection{},
			shouldWrite: true,
		},
		{
			name: "add and delete 1",
			first: map[string]FileProjection{
				"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
			},
			next: map[string]FileProjection{
				"bar/baz.txt": {Mode: 0644, Data: []byte("baz")},
			},
			shouldWrite: true,
		},
	}

	for _, tc := range cases {
		targetDir, err := utiltesting.MkTmpdir("atomic-write")
		if err != nil {
			t.Errorf("%v: unexpected error creating tmp dir: %v", tc.name, err)
			continue
		}
		defer os.RemoveAll(targetDir)

		writer := &AtomicWriter{targetDir: targetDir, logContext: "-test-"}

		err = writer.Write(tc.first, nil)
		if err != nil {
			t.Errorf("%v: unexpected error writing: %v", tc.name, err)
			continue
		}

		checkVolumeContents(targetDir, tc.name, tc.first, t)
		if !tc.shouldWrite {
			continue
		}

		err = writer.Write(tc.next, nil)
		if err != nil {
			if tc.shouldWrite {
				t.Errorf("%v: unexpected error writing: %v", tc.name, err)
				continue
			}
		} else if !tc.shouldWrite {
			t.Errorf("%v: unexpected success", tc.name)
			continue
		}

		checkVolumeContents(targetDir, tc.name, tc.next, t)
	}
}

func TestMultipleUpdates(t *testing.T) {
	cases := []struct {
		name     string
		payloads []map[string]FileProjection
	}{
		{
			name: "update 1",
			payloads: []map[string]FileProjection{
				{
					"foo": {Mode: 0644, Data: []byte("foo")},
					"bar": {Mode: 0644, Data: []byte("bar")},
				},
				{
					"foo": {Mode: 0400, Data: []byte("foo2")},
					"bar": {Mode: 0400, Data: []byte("bar2")},
				},
				{
					"foo": {Mode: 0600, Data: []byte("foo3")},
					"bar": {Mode: 0600, Data: []byte("bar3")},
				},
			},
		},
		{
			name: "update 2",
			payloads: []map[string]FileProjection{
				{
					"foo/bar.txt": {Mode: 0644, Data: []byte("foo/bar")},
					"bar/zab.txt": {Mode: 0644, Data: []byte("bar/zab.txt")},
				},
				{
					"foo/bar.txt": {Mode: 0644, Data: []byte("foo/bar2")},
					"bar/zab.txt": {Mode: 0400, Data: []byte("bar/zab.txt2")},
				},
			},
		},
		{
			name: "clear sentinel",
			payloads: []map[string]FileProjection{
				{
					"foo": {Mode: 0644, Data: []byte("foo")},
					"bar": {Mode: 0644, Data: []byte("bar")},
				},
				{
					"foo": {Mode: 0644, Data: []byte("foo2")},
					"bar": {Mode: 0644, Data: []byte("bar2")},
				},
				{
					"foo": {Mode: 0644, Data: []byte("foo3")},
					"bar": {Mode: 0644, Data: []byte("bar3")},
				},
				{
					"foo": {Mode: 0644, Data: []byte("foo4")},
					"bar": {Mode: 0644, Data: []byte("bar4")},
				},
			},
		},
		{
			name: "subdirectories 2",
			payloads: []map[string]FileProjection{
				{
					"foo/bar.txt":      {Mode: 0644, Data: []byte("foo/bar")},
					"bar/zab.txt":      {Mode: 0644, Data: []byte("bar/zab.txt")},
					"foo/blaz/bar.txt": {Mode: 0644, Data: []byte("foo/blaz/bar")},
					"bar/zib/zab.txt":  {Mode: 0644, Data: []byte("bar/zib/zab.txt")},
				},
				{
					"foo/bar.txt":      {Mode: 0644, Data: []byte("foo/bar2")},
					"bar/zab.txt":      {Mode: 0644, Data: []byte("bar/zab.txt2")},
					"foo/blaz/bar.txt": {Mode: 0644, Data: []byte("foo/blaz/bar2")},
					"bar/zib/zab.txt":  {Mode: 0644, Data: []byte("bar/zib/zab.txt2")},
				},
			},
		},
		{
			name: "add 1",
			payloads: []map[string]FileProjection{
				{
					"foo/bar.txt":            {Mode: 0644, Data: []byte("foo/bar")},
					"bar//zab.txt":           {Mode: 0644, Data: []byte("bar/zab.txt")},
					"foo/blaz/bar.txt":       {Mode: 0644, Data: []byte("foo/blaz/bar")},
					"bar/zib////zib/zab.txt": {Mode: 0644, Data: []byte("bar/zib/zab.txt")},
				},
				{
					"foo/bar.txt":      {Mode: 0644, Data: []byte("foo/bar2")},
					"bar/zab.txt":      {Mode: 0644, Data: []byte("bar/zab.txt2")},
					"foo/blaz/bar.txt": {Mode: 0644, Data: []byte("foo/blaz/bar2")},
					"bar/zib/zab.txt":  {Mode: 0644, Data: []byte("bar/zib/zab.txt2")},
					"add/new/keys.txt": {Mode: 0644, Data: []byte("addNewKeys")},
				},
			},
		},
		{
			name: "add 2",
			payloads: []map[string]FileProjection{
				{
					"foo/bar.txt":      {Mode: 0644, Data: []byte("foo/bar2")},
					"bar/zab.txt":      {Mode: 0644, Data: []byte("bar/zab.txt2")},
					"foo/blaz/bar.txt": {Mode: 0644, Data: []byte("foo/blaz/bar2")},
					"bar/zib/zab.txt":  {Mode: 0644, Data: []byte("bar/zib/zab.txt2")},
					"add/new/keys.txt": {Mode: 0644, Data: []byte("addNewKeys")},
				},
				{
					"foo/bar.txt":       {Mode: 0644, Data: []byte("foo/bar2")},
					"bar/zab.txt":       {Mode: 0644, Data: []byte("bar/zab.txt2")},
					"foo/blaz/bar.txt":  {Mode: 0644, Data: []byte("foo/blaz/bar2")},
					"bar/zib/zab.txt":   {Mode: 0644, Data: []byte("bar/zib/zab.txt2")},
					"add/new/keys.txt":  {Mode: 0644, Data: []byte("addNewKeys")},
					"add/new/keys2.txt": {Mode: 0644, Data: []byte("addNewKeys2")},
					"add/new/keys3.txt": {Mode: 0644, Data: []byte("addNewKeys3")},
				},
			},
		},
		{
			name: "remove 1",
			payloads: []map[string]FileProjection{
				{
					"foo/bar.txt":         {Mode: 0644, Data: []byte("foo/bar")},
					"bar//zab.txt":        {Mode: 0644, Data: []byte("bar/zab.txt")},
					"foo/blaz/bar.txt":    {Mode: 0644, Data: []byte("foo/blaz/bar")},
					"zip/zap/zup/fop.txt": {Mode: 0644, Data: []byte("zip/zap/zup/fop.txt")},
				},
				{
					"foo/bar.txt": {Mode: 0644, Data: []byte("foo/bar2")},
					"bar/zab.txt": {Mode: 0644, Data: []byte("bar/zab.txt2")},
				},
				{
					"foo/bar.txt": {Mode: 0644, Data: []byte("foo/bar")},
				},
			},
		},
	}

	for _, tc := range cases {
		targetDir, err := utiltesting.MkTmpdir("atomic-write")
		if err != nil {
			t.Errorf("%v: unexpected error creating tmp dir: %v", tc.name, err)
			continue
		}
		defer os.RemoveAll(targetDir)

		writer := &AtomicWriter{targetDir: targetDir, logContext: "-test-"}

		for _, payload := range tc.payloads {
			writer.Write(payload, nil)

			checkVolumeContents(targetDir, tc.name, payload, t)
		}
	}
}

func checkVolumeContents(targetDir, tcName string, payload map[string]FileProjection, t *testing.T) {
	dataDirPath := filepath.Join(targetDir, dataDirName)
	// use filepath.Walk to reconstruct the payload, then deep equal
	observedPayload := make(map[string]FileProjection)
	visitor := func(path string, info os.FileInfo, _ error) error {
		if info.IsDir() {
			return nil
		}

		relativePath := strings.TrimPrefix(path, dataDirPath)
		relativePath = strings.TrimPrefix(relativePath, "/")
		if strings.HasPrefix(relativePath, "..") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			return err
		}
		mode := int32(fileInfo.Mode())

		observedPayload[relativePath] = FileProjection{Data: content, Mode: mode}

		return nil
	}

	d, err := os.ReadDir(targetDir)
	if err != nil {
		t.Errorf("Unable to read dir %v: %v", targetDir, err)
		return
	}
	for _, info := range d {
		if strings.HasPrefix(info.Name(), "..") {
			continue
		}
		if info.Type()&os.ModeSymlink != 0 {
			p := filepath.Join(targetDir, info.Name())
			actual, err := os.Readlink(p)
			if err != nil {
				t.Errorf("Unable to read symlink %v: %v", p, err)
				continue
			}
			if err := filepath.Walk(filepath.Join(targetDir, actual), visitor); err != nil {
				t.Errorf("%v: unexpected error walking directory: %v", tcName, err)
			}
		}
	}

	cleanPathPayload := make(map[string]FileProjection, len(payload))
	for k, v := range payload {
		cleanPathPayload[filepath.Clean(k)] = v
	}

	if !reflect.DeepEqual(cleanPathPayload, observedPayload) {
		t.Errorf("%v: payload and observed payload do not match.", tcName)
	}
}

func TestValidatePayload(t *testing.T) {
	maxPath := strings.Repeat("a", maxPathLength+1)

	cases := []struct {
		name     string
		payload  map[string]FileProjection
		expected sets.Set[string]
		valid    bool
	}{
		{
			name: "valid payload",
			payload: map[string]FileProjection{
				"foo": {},
				"bar": {},
			},
			valid:    true,
			expected: sets.New[string]("foo", "bar"),
		},
		{
			name: "payload with path length > 4096 is invalid",
			payload: map[string]FileProjection{
				maxPath: {},
			},
			valid: false,
		},
		{
			name: "payload with absolute path is invalid",
			payload: map[string]FileProjection{
				"/dev/null": {},
			},
			valid: false,
		},
		{
			name: "payload with reserved path is invalid",
			payload: map[string]FileProjection{
				"..sneaky.txt": {},
			},
			valid: false,
		},
		{
			name: "payload with doubledot path is invalid",
			payload: map[string]FileProjection{
				"foo/../etc/password": {},
			},
			valid: false,
		},
		{
			name: "payload with empty path is invalid",
			payload: map[string]FileProjection{
				"": {},
			},
			valid: false,
		},
		{
			name: "payload with unclean path should be cleaned",
			payload: map[string]FileProjection{
				"foo////bar": {},
			},
			valid:    true,
			expected: sets.New[string]("foo/bar"),
		},
	}
	getPayloadPaths := func(payload map[string]FileProjection) sets.Set[string] {
		paths := sets.New[string]()
		for path := range payload {
			paths.Insert(path)
		}
		return paths
	}

	for _, tc := range cases {
		real, err := validatePayload(tc.payload)
		if !tc.valid && err == nil {
			t.Errorf("%v: unexpected success", tc.name)
		}

		if tc.valid {
			if err != nil {
				t.Errorf("%v: unexpected failure: %v", tc.name, err)
				continue
			}

			realPaths := getPayloadPaths(real)
			if !realPaths.Equal(tc.expected) {
				t.Errorf("%v: unexpected payload paths: %v is not equal to %v", tc.name, realPaths, tc.expected)
			}
		}

	}
}

func TestCreateUserVisibleFiles(t *testing.T) {
	cases := []struct {
		name     string
		payload  map[string]FileProjection
		expected map[string]string
	}{
		{
			name: "simple path",
			payload: map[string]FileProjection{
				"foo": {},
				"bar": {},
			},
			expected: map[string]string{
				"foo": "..data/foo",
				"bar": "..data/bar",
			},
		},
		{
			name: "simple nested path",
			payload: map[string]FileProjection{
				"foo/bar":     {},
				"foo/bar/txt": {},
				"bar/txt":     {},
			},
			expected: map[string]string{
				"foo": "..data/foo",
				"bar": "..data/bar",
			},
		},
		{
			name: "unclean nested path",
			payload: map[string]FileProjection{
				"./bar":     {},
				"foo///bar": {},
			},
			expected: map[string]string{
				"bar": "..data/bar",
				"foo": "..data/foo",
			},
		},
	}

	for _, tc := range cases {
		targetDir, err := utiltesting.MkTmpdir("atomic-write")
		if err != nil {
			t.Errorf("%v: unexpected error creating tmp dir: %v", tc.name, err)
			continue
		}
		defer os.RemoveAll(targetDir)

		dataDirPath := filepath.Join(targetDir, dataDirName)
		err = os.MkdirAll(dataDirPath, 0755)
		if err != nil {
			t.Fatalf("%v: unexpected error creating data path: %v", tc.name, err)
		}

		writer := &AtomicWriter{targetDir: targetDir, logContext: "-test-"}
		payload, err := validatePayload(tc.payload)
		if err != nil {
			t.Fatalf("%v: unexpected error validating payload: %v", tc.name, err)
		}
		err = writer.createUserVisibleFiles(payload)
		if err != nil {
			t.Fatalf("%v: unexpected error creating visible files: %v", tc.name, err)
		}

		for subpath, expectedDest := range tc.expected {
			visiblePath := filepath.Join(targetDir, subpath)
			destination, err := os.Readlink(visiblePath)
			if err != nil && os.IsNotExist(err) {
				t.Fatalf("%v: visible symlink does not exist: %v", tc.name, visiblePath)
			} else if err != nil {
				t.Fatalf("%v: unable to read symlink %v: %v", tc.name, dataDirPath, err)
			}

			if expectedDest != destination {
				t.Fatalf("%v: symlink destination %q not same with expected data dir %q", tc.name, destination, expectedDest)
			}
		}
	}
}

func TestSetPerms(t *testing.T) {
	targetDir, err := utiltesting.MkTmpdir("atomic-write")
	if err != nil {
		t.Fatalf("unexpected error creating tmp dir: %v", err)
	}
	defer os.RemoveAll(targetDir)

	// Test that setPerms() is called once and with valid timestamp directory.
	payload1 := map[string]FileProjection{
		"foo/bar.txt": {Mode: 0644, Data: []byte("foo")},
		"bar/zab.txt": {Mode: 0644, Data: []byte("bar")},
	}

	var setPermsCalled int
	writer := &AtomicWriter{targetDir: targetDir, logContext: "-test-"}
	err = writer.Write(payload1, func(subPath string) error {
		fileInfo, err := os.Stat(filepath.Join(targetDir, subPath))
		if err != nil {
			t.Fatalf("unexpected error getting file info: %v", err)
		}
		// Ensure that given timestamp directory really exists.
		if !fileInfo.IsDir() {
			t.Fatalf("subPath is not a directory: %v", subPath)
		}
		setPermsCalled++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error writing: %v", err)
	}
	if setPermsCalled != 1 {
		t.Fatalf("unexpected number of calls to setPerms: %v", setPermsCalled)
	}

	// Test that errors from setPerms() are propagated.
	payload2 := map[string]FileProjection{
		"foo/bar.txt": {Mode: 0644, Data: []byte("foo2")},
		"bar/zab.txt": {Mode: 0644, Data: []byte("bar2")},
	}

	err = writer.Write(payload2, func(_ string) error {
		return fmt.Errorf("error in setPerms")
	})
	if err == nil {
		t.Fatalf("expected error while writing but got nil")
	}
	if !strings.Contains(err.Error(), "error in setPerms") {
		t.Fatalf("unexpected error while writing: %v", err)
	}
}

func TestWriteAgainAfterUnexpectedExit(t *testing.T) {
	testCases := []struct {
		name       string
		payload    map[string]FileProjection
		simulateFn func(targetDir string, payload map[string]FileProjection) error
	}{
		{
			name: "process killed before creating user visible files",
			payload: map[string]FileProjection{
				"foo": {Mode: 0644, Data: []byte("foo")},
				"bar": {Mode: 0644, Data: []byte("bar")},
			},
			simulateFn: func(targetDir string, payload map[string]FileProjection) error {
				for filename := range payload {
					path := filepath.Join(targetDir, filename)
					if err := os.RemoveAll(path); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			targetDir, err := utiltesting.MkTmpdir("atomic-write")
			if err != nil {
				t.Fatalf("unexpected error creating tmp dir: %v", err)
			}
			defer func() {
				err := os.RemoveAll(targetDir)
				if err != nil {
					t.Errorf("%v: unexpected error removing tmp dir: %v", tc.name, err)
				}
			}()

			writer := &AtomicWriter{targetDir: targetDir, logContext: "-test-"}
			err = writer.Write(tc.payload, nil)
			if err != nil {
				t.Fatalf("unexpected error writing payload: %v", err)
			}

			err = tc.simulateFn(targetDir, tc.payload)
			if err != nil {
				t.Fatalf("failed to simulate the unexpected exit: %v", err)
			}

			err = writer.Write(tc.payload, nil)
			if err != nil {
				t.Fatalf("unexpected error writing payload again: %v", err)
			}
			checkVolumeContents(targetDir, tc.name, tc.payload, t)
		})
	}
}
