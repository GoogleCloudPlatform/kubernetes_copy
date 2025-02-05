/*
Copyright 2017 The Kubernetes Authors.

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

package logs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utiltesting "k8s.io/client-go/util/testing"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	apitesting "k8s.io/cri-api/pkg/apis/testing"
	"k8s.io/utils/ptr"
)

func TestLogOptions(t *testing.T) {
	var (
		line         = int64(8)
		bytes        = int64(64)
		timestamp    = metav1.Now()
		sinceseconds = int64(10)
	)
	for c, test := range []struct {
		apiOpts *v1.PodLogOptions
		expect  *LogOptions
	}{
		{ // empty options
			apiOpts: &v1.PodLogOptions{},
			expect:  &LogOptions{tail: -1, bytes: -1},
		},
		{ // test tail lines
			apiOpts: &v1.PodLogOptions{TailLines: &line},
			expect:  &LogOptions{tail: line, bytes: -1},
		},
		{ // test limit bytes
			apiOpts: &v1.PodLogOptions{LimitBytes: &bytes},
			expect:  &LogOptions{tail: -1, bytes: bytes},
		},
		{ // test since timestamp
			apiOpts: &v1.PodLogOptions{SinceTime: &timestamp},
			expect:  &LogOptions{tail: -1, bytes: -1, since: timestamp.Time},
		},
		{ // test since seconds
			apiOpts: &v1.PodLogOptions{SinceSeconds: &sinceseconds},
			expect:  &LogOptions{tail: -1, bytes: -1, since: timestamp.Add(-10 * time.Second)},
		},
	} {
		t.Logf("TestCase #%d: %+v", c, test)
		opts := NewLogOptions(test.apiOpts, timestamp.Time)
		assert.Equal(t, test.expect, opts)
	}
}

func TestReadLogs(t *testing.T) {
	file, err := os.CreateTemp("", "TestFollowLogs")
	if err != nil {
		t.Fatalf("unable to create temp file")
	}
	defer utiltesting.CloseAndRemove(t, file)
	file.WriteString(`{"log":"line1\n","stream":"stdout","time":"2020-09-27T11:18:01.00000000Z"}` + "\n")
	file.WriteString(`{"log":"line2\n","stream":"stdout","time":"2020-09-27T11:18:02.00000000Z"}` + "\n")
	file.WriteString(`{"log":"line3\n","stream":"stdout","time":"2020-09-27T11:18:03.00000000Z"}` + "\n")

	testCases := []struct {
		name          string
		podLogOptions v1.PodLogOptions
		expected      string
	}{
		{
			name:          "default pod log options should output all lines",
			podLogOptions: v1.PodLogOptions{},
			expected:      "line1\nline2\nline3\n",
		},
		{
			name: "using TailLines 2 should output last 2 lines",
			podLogOptions: v1.PodLogOptions{
				TailLines: ptr.To[int64](2),
			},
			expected: "line2\nline3\n",
		},
		{
			name: "using TailLines 4 should output all lines when the log has less than 4 lines",
			podLogOptions: v1.PodLogOptions{
				TailLines: ptr.To[int64](4),
			},
			expected: "line1\nline2\nline3\n",
		},
		{
			name: "using TailLines 0 should output nothing",
			podLogOptions: v1.PodLogOptions{
				TailLines: ptr.To[int64](0),
			},
			expected: "",
		},
		{
			name: "using LimitBytes 9 should output first 9 bytes",
			podLogOptions: v1.PodLogOptions{
				LimitBytes: ptr.To[int64](9),
			},
			expected: "line1\nlin",
		},
		{
			name: "using LimitBytes 100 should output all bytes when the log has less than 100 bytes",
			podLogOptions: v1.PodLogOptions{
				LimitBytes: ptr.To[int64](100),
			},
			expected: "line1\nline2\nline3\n",
		},
		{
			name: "using LimitBytes 0 should output nothing",
			podLogOptions: v1.PodLogOptions{
				LimitBytes: ptr.To[int64](0),
			},
			expected: "",
		},
		{
			name: "using SinceTime should output lines with a time on or after the specified time",
			podLogOptions: v1.PodLogOptions{
				SinceTime: &metav1.Time{Time: time.Date(2020, time.Month(9), 27, 11, 18, 02, 0, time.UTC)},
			},
			expected: "line2\nline3\n",
		},
		{
			name: "using SinceTime now should output nothing",
			podLogOptions: v1.PodLogOptions{
				SinceTime: &metav1.Time{Time: time.Now()},
			},
			expected: "",
		},
		{
			name: "using follow should output all log lines",
			podLogOptions: v1.PodLogOptions{
				Follow: true,
			},
			expected: "line1\nline2\nline3\n",
		},
		{
			name: "using follow combined with TailLines 2 should output the last 2 lines",
			podLogOptions: v1.PodLogOptions{
				Follow:    true,
				TailLines: ptr.To[int64](2),
			},
			expected: "line2\nline3\n",
		},
		{
			name: "using follow combined with SinceTime should output lines with a time on or after the specified time",
			podLogOptions: v1.PodLogOptions{
				Follow:    true,
				SinceTime: &metav1.Time{Time: time.Date(2020, time.Month(9), 27, 11, 18, 02, 0, time.UTC)},
			},
			expected: "line2\nline3\n",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			containerID := "fake-container-id"
			fakeRuntimeService := &apitesting.FakeRuntimeService{
				Containers: map[string]*apitesting.FakeContainer{
					containerID: {
						ContainerStatus: runtimeapi.ContainerStatus{
							State: runtimeapi.ContainerState_CONTAINER_RUNNING,
						},
					},
				},
			}
			// If follow is specified, mark the container as exited or else ReadLogs will run indefinitely
			if tc.podLogOptions.Follow {
				fakeRuntimeService.Containers[containerID].State = runtimeapi.ContainerState_CONTAINER_EXITED
			}
			opts := NewLogOptions(&tc.podLogOptions, time.Now())
			stdoutBuf := bytes.NewBuffer(nil)
			stderrBuf := bytes.NewBuffer(nil)
			err = ReadLogs(context.TODO(), nil, file.Name(), containerID, opts, fakeRuntimeService, stdoutBuf, stderrBuf)

			if err != nil {
				t.Fatal(err.Error())
			}
			if stderrBuf.Len() > 0 {
				t.Fatalf("Stderr: %v", stderrBuf.String())
			}
			if actual := stdoutBuf.String(); tc.expected != actual {
				t.Fatalf("Actual output does not match expected.\nActual:  %v\nExpected: %v\n", actual, tc.expected)
			}
		})
	}
}

func TestReadRotatedLog(t *testing.T) {
	tmpDir := t.TempDir()
	file, err := os.CreateTemp(tmpDir, "logfile")
	if err != nil {
		assert.NoErrorf(t, err, "unable to create temp file")
	}
	stdoutBuf := &bytes.Buffer{}
	stderrBuf := &bytes.Buffer{}
	containerID := "fake-container-id"
	fakeRuntimeService := &apitesting.FakeRuntimeService{
		Containers: map[string]*apitesting.FakeContainer{
			containerID: {
				ContainerStatus: runtimeapi.ContainerStatus{
					State: runtimeapi.ContainerState_CONTAINER_RUNNING,
				},
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Start to follow the container's log.
	fileName := file.Name()
	go func(ctx context.Context) {
		podLogOptions := v1.PodLogOptions{
			Follow: true,
		}
		opts := NewLogOptions(&podLogOptions, time.Now())
		_ = ReadLogs(ctx, nil, fileName, containerID, opts, fakeRuntimeService, stdoutBuf, stderrBuf)
	}(ctx)

	// log in stdout
	expectedStdout := "line0\nline2\nline4\nline6\nline8\n"
	// log in stderr
	expectedStderr := "line1\nline3\nline5\nline7\nline9\n"

	dir := filepath.Dir(file.Name())
	baseName := filepath.Base(file.Name())

	// Write 10 lines to log file.
	// Let ReadLogs start.
	time.Sleep(50 * time.Millisecond)

	for line := 0; line < 10; line++ {
		// Write the first three lines to log file
		now := time.Now().Format(RFC3339NanoLenient)
		if line%2 == 0 {
			file.WriteString(fmt.Sprintf(
				`{"log":"line%d\n","stream":"stdout","time":"%s"}`+"\n", line, now))
		} else {
			file.WriteString(fmt.Sprintf(
				`{"log":"line%d\n","stream":"stderr","time":"%s"}`+"\n", line, now))
		}
		time.Sleep(1 * time.Millisecond)

		if line == 5 {
			file.Close()
			// Pretend to rotate the log.
			rotatedName := fmt.Sprintf("%s.%s", baseName, time.Now().Format("220060102-150405"))
			rotatedName = filepath.Join(dir, rotatedName)
			if err := os.Rename(filepath.Join(dir, baseName), rotatedName); err != nil {
				assert.NoErrorf(t, err, "failed to rotate log %q to %q", file.Name(), rotatedName)
				return
			}

			newF := filepath.Join(dir, baseName)
			if file, err = os.Create(newF); err != nil {
				assert.NoError(t, err, "unable to create new log file")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Finished writing into the file, close it, so we can delete it later.
	err = file.Close()
	assert.NoErrorf(t, err, "could not close file.")

	time.Sleep(1 * time.Second)
	// Make the function ReadLogs end.
	fakeRuntimeService.Lock()
	fakeRuntimeService.Containers[containerID].State = runtimeapi.ContainerState_CONTAINER_EXITED
	fakeRuntimeService.Unlock()

	assert.Equal(t, expectedStdout, stdoutBuf.String())
	assert.Equal(t, expectedStderr, stderrBuf.String())
}

func TestParseLog(t *testing.T) {
	timestamp, err := time.Parse(timeFormatIn, "2016-10-20T18:39:20.57606443Z")
	assert.NoError(t, err)
	msg := &logMessage{}
	for c, test := range []struct {
		line string
		msg  *logMessage
		err  bool
	}{
		{ // Docker log format stdout
			line: `{"log":"docker stdout test log","stream":"stdout","time":"2016-10-20T18:39:20.57606443Z"}` + "\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stdout,
				log:       []byte("docker stdout test log"),
			},
		},
		{ // Docker log format stderr
			line: `{"log":"docker stderr test log","stream":"stderr","time":"2016-10-20T18:39:20.57606443Z"}` + "\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stderr,
				log:       []byte("docker stderr test log"),
			},
		},
		{ // CRI log format stdout
			line: "2016-10-20T18:39:20.57606443Z stdout F cri stdout test log\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stdout,
				log:       []byte("cri stdout test log\n"),
			},
		},
		{ // CRI log format stderr
			line: "2016-10-20T18:39:20.57606443Z stderr F cri stderr test log\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stderr,
				log:       []byte("cri stderr test log\n"),
			},
		},
		{ // Unsupported Log format
			line: "unsupported log format test log\n",
			msg:  &logMessage{},
			err:  true,
		},
		{ // Partial CRI log line
			line: "2016-10-20T18:39:20.57606443Z stdout P cri stdout partial test log\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stdout,
				log:       []byte("cri stdout partial test log"),
			},
		},
		{ // Partial CRI log line with multiple log tags.
			line: "2016-10-20T18:39:20.57606443Z stdout P:TAG1:TAG2 cri stdout partial test log\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stdout,
				log:       []byte("cri stdout partial test log"),
			},
		},
	} {
		t.Logf("TestCase #%d: %+v", c, test)
		parse, err := getParseFunc([]byte(test.line))
		if test.err {
			assert.Error(t, err)
			continue
		}
		assert.NoError(t, err)
		err = parse([]byte(test.line), msg)
		assert.NoError(t, err)
		assert.Equal(t, test.msg, msg)
	}
}

func TestWriteLogs(t *testing.T) {
	timestamp := time.Unix(1234, 43210)
	log := "abcdefg\n"

	for c, test := range []struct {
		stream       runtimeapi.LogStreamType
		since        time.Time
		timestamp    bool
		expectStdout string
		expectStderr string
	}{
		{ // stderr log
			stream:       runtimeapi.Stderr,
			expectStderr: log,
		},
		{ // stdout log
			stream:       runtimeapi.Stdout,
			expectStdout: log,
		},
		{ // since is after timestamp
			stream: runtimeapi.Stdout,
			since:  timestamp.Add(1 * time.Second),
		},
		{ // timestamp enabled
			stream:       runtimeapi.Stderr,
			timestamp:    true,
			expectStderr: timestamp.Format(timeFormatOut) + " " + log,
		},
	} {
		t.Logf("TestCase #%d: %+v", c, test)
		msg := &logMessage{
			timestamp: timestamp,
			stream:    test.stream,
			log:       []byte(log),
		}
		stdoutBuf := bytes.NewBuffer(nil)
		stderrBuf := bytes.NewBuffer(nil)
		w := newLogWriter(stdoutBuf, stderrBuf, &LogOptions{since: test.since, timestamp: test.timestamp, bytes: -1})
		err := w.write(msg, true)
		assert.NoError(t, err)
		assert.Equal(t, test.expectStdout, stdoutBuf.String())
		assert.Equal(t, test.expectStderr, stderrBuf.String())
	}
}

func TestWriteLogsWithBytesLimit(t *testing.T) {
	timestamp := time.Unix(1234, 4321)
	timestampStr := timestamp.Format(timeFormatOut)
	log := "abcdefg\n"

	for c, test := range []struct {
		stdoutLines  int
		stderrLines  int
		bytes        int
		timestamp    bool
		expectStdout string
		expectStderr string
	}{
		{ // limit bytes less than one line
			stdoutLines:  3,
			bytes:        3,
			expectStdout: "abc",
		},
		{ // limit bytes across lines
			stdoutLines:  3,
			bytes:        len(log) + 3,
			expectStdout: "abcdefg\nabc",
		},
		{ // limit bytes more than all lines
			stdoutLines:  3,
			bytes:        3 * len(log),
			expectStdout: "abcdefg\nabcdefg\nabcdefg\n",
		},
		{ // limit bytes for stderr
			stderrLines:  3,
			bytes:        len(log) + 3,
			expectStderr: "abcdefg\nabc",
		},
		{ // limit bytes for both stdout and stderr, stdout first.
			stdoutLines:  1,
			stderrLines:  2,
			bytes:        len(log) + 3,
			expectStdout: "abcdefg\n",
			expectStderr: "abc",
		},
		{ // limit bytes with timestamp
			stdoutLines:  3,
			timestamp:    true,
			bytes:        len(timestampStr) + 1 + len(log) + 2,
			expectStdout: timestampStr + " " + log + timestampStr[:2],
		},
	} {
		t.Logf("TestCase #%d: %+v", c, test)
		msg := &logMessage{
			timestamp: timestamp,
			log:       []byte(log),
		}
		stdoutBuf := bytes.NewBuffer(nil)
		stderrBuf := bytes.NewBuffer(nil)
		w := newLogWriter(stdoutBuf, stderrBuf, &LogOptions{timestamp: test.timestamp, bytes: int64(test.bytes)})
		for i := 0; i < test.stdoutLines; i++ {
			msg.stream = runtimeapi.Stdout
			if err := w.write(msg, true); err != nil {
				assert.EqualError(t, err, errMaximumWrite.Error())
			}
		}
		for i := 0; i < test.stderrLines; i++ {
			msg.stream = runtimeapi.Stderr
			if err := w.write(msg, true); err != nil {
				assert.EqualError(t, err, errMaximumWrite.Error())
			}
		}
		assert.Equal(t, test.expectStdout, stdoutBuf.String())
		assert.Equal(t, test.expectStderr, stderrBuf.String())
	}
}

func TestReadLogsLimitsWithTimestamps(t *testing.T) {
	logLineFmt := "2022-10-29T16:10:22.592603036-05:00 stdout P %v\n"
	logLineNewLine := "2022-10-29T16:10:22.592603036-05:00 stdout F \n"

	tmpfile, err := os.CreateTemp("", "log.*.txt")
	assert.NoError(t, err)

	count := 10000

	for i := 0; i < count; i++ {
		tmpfile.WriteString(fmt.Sprintf(logLineFmt, i))
	}
	tmpfile.WriteString(logLineNewLine)

	for i := 0; i < count; i++ {
		tmpfile.WriteString(fmt.Sprintf(logLineFmt, i))
	}
	tmpfile.WriteString(logLineNewLine)

	// two lines are in the buffer

	defer os.Remove(tmpfile.Name()) // clean up

	assert.NoError(t, err)
	tmpfile.Close()

	var buf bytes.Buffer
	w := io.MultiWriter(&buf)

	err = ReadLogs(context.Background(), nil, tmpfile.Name(), "", &LogOptions{tail: -1, bytes: -1, timestamp: true}, nil, w, w)
	assert.NoError(t, err)

	lineCount := 0
	scanner := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for scanner.Scan() {
		lineCount++

		// Split the line
		ts, logline, _ := bytes.Cut(scanner.Bytes(), []byte(" "))

		// Verification
		//   1. The timestamp should exist
		//   2. The last item in the log should be 9999
		_, err = time.Parse(time.RFC3339, string(ts))
		assert.NoError(t, err, "timestamp not found")
		assert.True(t, bytes.HasSuffix(logline, []byte("9999")), "is the complete log found")
	}

	assert.Equal(t, 2, lineCount, "should have two lines")
}

func TestOnlyStdoutStream(t *testing.T) {
	timestamp := time.Unix(1234, 43210)
	msgs := []*logMessage{
		{
			timestamp: timestamp,
			stream:    runtimeapi.Stdout,
			log:       []byte("out1\n"),
		},
		{
			timestamp: timestamp,
			stream:    runtimeapi.Stderr,
			log:       []byte("err1\n"),
		},
		{
			timestamp: timestamp,
			stream:    runtimeapi.Stdout,
			log:       []byte("out2\n"),
		},
	}

	testCases := map[string]struct {
		limitBytes int64

		expectedStdout string
	}{
		"all stdout logs": {
			limitBytes: -1,

			expectedStdout: "out1\nout2\n",
		},
		"the first 7 bytes from stdout": {
			limitBytes: 7,

			expectedStdout: "out1\nou",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			stdoutBuf := bytes.NewBuffer(nil)
			w := newLogWriter(stdoutBuf, nil, &LogOptions{
				bytes: tc.limitBytes,
			})
			for _, msg := range msgs {
				err := w.write(msg, false)
				if errors.Is(err, errMaximumWrite) {
					continue
				}
				require.NoError(t, err)
			}
			assert.EqualValues(t, tc.expectedStdout, stdoutBuf.String())
		})
	}
}

func TestDedupEvents(t *testing.T) {
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{
			name: "events without error",
			input: []interface{}{fsnotify.Write, fsnotify.Write, fsnotify.Write, fsnotify.Create,
				fsnotify.Write, fsnotify.Write, fsnotify.Write, fsnotify.Write, fsnotify.Write, fsnotify.Write, fsnotify.Create,
				fsnotify.Write, fsnotify.Write, fsnotify.Create},
			expected: []interface{}{fsnotify.Write, fsnotify.Write, fsnotify.Create,
				fsnotify.Write, fsnotify.Create,
				fsnotify.Write, fsnotify.Create},
		},
		{
			name:     "event with error",
			input:    []interface{}{fsnotify.Write, fsnotify.Write, fsnotify.Write, fsnotify.Create, fsnotify.ErrEventOverflow, fsnotify.ErrEventOverflow},
			expected: []interface{}{fsnotify.Write, fsnotify.Write, fsnotify.Create},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			w := &fsnotify.Watcher{Events: make(chan fsnotify.Event), Errors: make(chan error)}
			dedup := newDedupWriteEvents(w, "0.log", nil)
			dedup.dedupEvents(context.Background())

			go func() {
				for _, val := range tc.input {
					switch v := val.(type) {
					case error:
						dedup.w.Errors <- v
					case fsnotify.Op:
						ev := fsnotify.Event{
							Op:   v,
							Name: "0.log",
						}
						dedup.w.Events <- ev
					}
				}
				close(dedup.w.Events)
			}()

			time.Sleep(10 * time.Millisecond)

			received := make([]interface{}, 0, 10)
			for op := range dedup.events {
				// Mimic read logs
				time.Sleep(10 * time.Millisecond)
				received = append(received, op.Event.Op)
			}
			assert.Equal(t, tc.expected, received)
		})
	}
}
