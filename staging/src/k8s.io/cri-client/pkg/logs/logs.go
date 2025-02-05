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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "k8s.io/api/core/v1"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"

	remote "k8s.io/cri-client/pkg"
	"k8s.io/cri-client/pkg/internal"
)

// Notice that the current CRI logs implementation doesn't handle
// log rotation.
// * It will not retrieve logs in rotated log file.
// * If log rotation happens when following the log:
//   * If the rotation is using create mode, we'll still follow the old file.
//   * If the rotation is using copytruncate, we'll be reading at the original position and get nothing.

const (
	// RFC3339NanoFixed is the fixed width version of time.RFC3339Nano.
	RFC3339NanoFixed = "2006-01-02T15:04:05.000000000Z07:00"
	// RFC3339NanoLenient is the variable width RFC3339 time format for lenient parsing of strings into timestamps.
	RFC3339NanoLenient = "2006-01-02T15:04:05.999999999Z07:00"

	// timeFormatOut is the format for writing timestamps to output.
	timeFormatOut = RFC3339NanoFixed
	// timeFormatIn is the format for parsing timestamps from other logs.
	timeFormatIn = RFC3339NanoLenient

	// logForceCheckPeriod is the period to check for a new read
	logForceCheckPeriod = 1 * time.Second
)

var (
	// eol is the end-of-line sign in the log.
	eol = []byte{'\n'}
	// delimiter is the delimiter for timestamp and stream type in log line.
	delimiter = []byte{' '}
	// tagDelimiter is the delimiter for log tags.
	tagDelimiter = []byte(runtimeapi.LogTagDelimiter)
)

// logMessage is the CRI internal log type.
type logMessage struct {
	timestamp time.Time
	stream    runtimeapi.LogStreamType
	log       []byte
}

// reset resets the log to nil.
func (l *logMessage) reset() {
	l.timestamp = time.Time{}
	l.stream = ""
	l.log = nil
}

// LogOptions is the CRI internal type of all log options.
type LogOptions struct {
	tail      int64
	bytes     int64
	since     time.Time
	follow    bool
	timestamp bool
}

// NewLogOptions convert the v1.PodLogOptions to CRI internal LogOptions.
func NewLogOptions(apiOpts *v1.PodLogOptions, now time.Time) *LogOptions {
	opts := &LogOptions{
		tail:      -1, // -1 by default which means read all logs.
		bytes:     -1, // -1 by default which means read all logs.
		follow:    apiOpts.Follow,
		timestamp: apiOpts.Timestamps,
	}
	if apiOpts.TailLines != nil {
		opts.tail = *apiOpts.TailLines
	}
	if apiOpts.LimitBytes != nil {
		opts.bytes = *apiOpts.LimitBytes
	}
	if apiOpts.SinceSeconds != nil {
		opts.since = now.Add(-time.Duration(*apiOpts.SinceSeconds) * time.Second)
	}
	if apiOpts.SinceTime != nil && apiOpts.SinceTime.After(opts.since) {
		opts.since = apiOpts.SinceTime.Time
	}
	return opts
}

// parseFunc is a function parsing one log line to the internal log type.
// Notice that the caller must make sure logMessage is not nil.
type parseFunc func([]byte, *logMessage) error

var parseFuncs = []parseFunc{
	parseCRILog,        // CRI log format parse function
	parseDockerJSONLog, // Docker JSON log format parse function
}

// parseCRILog parses logs in CRI log format. CRI Log format example:
//
//	2016-10-06T00:17:09.669794202Z stdout P log content 1
//	2016-10-06T00:17:09.669794203Z stderr F log content 2
func parseCRILog(log []byte, msg *logMessage) error {
	var err error
	// Parse timestamp
	idx := bytes.Index(log, delimiter)
	if idx < 0 {
		return fmt.Errorf("timestamp is not found")
	}
	msg.timestamp, err = time.Parse(timeFormatIn, string(log[:idx]))
	if err != nil {
		return fmt.Errorf("unexpected timestamp format %q: %v", timeFormatIn, err)
	}

	// Parse stream type
	log = log[idx+1:]
	idx = bytes.Index(log, delimiter)
	if idx < 0 {
		return fmt.Errorf("stream type is not found")
	}
	msg.stream = runtimeapi.LogStreamType(log[:idx])
	if msg.stream != runtimeapi.Stdout && msg.stream != runtimeapi.Stderr {
		return fmt.Errorf("unexpected stream type %q", msg.stream)
	}

	// Parse log tag
	log = log[idx+1:]
	idx = bytes.Index(log, delimiter)
	if idx < 0 {
		return fmt.Errorf("log tag is not found")
	}
	// Keep this forward compatible.
	tags := bytes.Split(log[:idx], tagDelimiter)
	partial := runtimeapi.LogTag(tags[0]) == runtimeapi.LogTagPartial
	// Trim the tailing new line if this is a partial line.
	if partial && len(log) > 0 && log[len(log)-1] == '\n' {
		log = log[:len(log)-1]
	}

	// Get log content
	msg.log = log[idx+1:]

	return nil
}

// jsonLog is a log message, typically a single entry from a given log stream.
// since the data structure is originally from docker, we should be careful to
// with any changes to jsonLog
type jsonLog struct {
	// Log is the log message
	Log string `json:"log,omitempty"`
	// Stream is the log source
	Stream string `json:"stream,omitempty"`
	// Created is the created timestamp of log
	Created time.Time `json:"time"`
}

// parseDockerJSONLog parses logs in Docker JSON log format. Docker JSON log format
// example:
//
//	{"log":"content 1","stream":"stdout","time":"2016-10-20T18:39:20.57606443Z"}
//	{"log":"content 2","stream":"stderr","time":"2016-10-20T18:39:20.57606444Z"}
func parseDockerJSONLog(log []byte, msg *logMessage) error {
	var l = &jsonLog{}

	// TODO: JSON decoding is fairly expensive, we should evaluate this.
	if err := json.Unmarshal(log, l); err != nil {
		return fmt.Errorf("failed with %v to unmarshal log %q", err, l)
	}
	msg.timestamp = l.Created
	msg.stream = runtimeapi.LogStreamType(l.Stream)
	msg.log = []byte(l.Log)
	return nil
}

// getParseFunc returns proper parse function based on the sample log line passed in.
func getParseFunc(log []byte) (parseFunc, error) {
	for _, p := range parseFuncs {
		if err := p(log, &logMessage{}); err == nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("unsupported log format: %q", log)
}

// logWriter controls the writing into the stream based on the log options.
type logWriter struct {
	stdout io.Writer
	stderr io.Writer
	opts   *LogOptions
	remain int64
}

// errMaximumWrite is returned when all bytes have been written.
var errMaximumWrite = errors.New("maximum write")

// errShortWrite is returned when the message is not fully written.
var errShortWrite = errors.New("short write")

func newLogWriter(stdout io.Writer, stderr io.Writer, opts *LogOptions) *logWriter {
	w := &logWriter{
		stdout: stdout,
		stderr: stderr,
		opts:   opts,
		remain: math.MaxInt64, // initialize it as infinity
	}
	if opts.bytes >= 0 {
		w.remain = opts.bytes
	}
	return w
}

// writeLogs writes logs into stdout, stderr.
func (w *logWriter) write(msg *logMessage, addPrefix bool) error {
	if msg.timestamp.Before(w.opts.since) {
		// Skip the line because it's older than since
		return nil
	}
	line := msg.log
	if w.opts.timestamp && addPrefix {
		prefix := append([]byte(msg.timestamp.Format(timeFormatOut)), delimiter[0])
		line = append(prefix, line...)
	}
	// If the line is longer than the remaining bytes, cut it.
	if int64(len(line)) > w.remain {
		line = line[:w.remain]
	}
	// Get the proper stream to write to.
	var stream io.Writer
	switch msg.stream {
	case runtimeapi.Stdout:
		stream = w.stdout
	case runtimeapi.Stderr:
		stream = w.stderr
	default:
		return fmt.Errorf("unexpected stream type %q", msg.stream)
	}
	// Since v1.32, either w.stdout or w.stderr may be nil if the stream is not requested.
	// In such case, we should neither count the bytes nor write to the stream.
	if stream == nil {
		return nil
	}
	n, err := stream.Write(line)
	w.remain -= int64(n)
	if err != nil {
		return err
	}
	// If the line has not been fully written, return errShortWrite
	if n < len(line) {
		return errShortWrite
	}
	// If there are no more bytes left, return errMaximumWrite
	if w.remain <= 0 {
		return errMaximumWrite
	}
	return nil
}

// ReadLogs read the container log and redirect into stdout and stderr.
// Note that containerID is only needed when following the log, or else
// just pass in empty string "".
func ReadLogs(ctx context.Context, logger *klog.Logger, path, containerID string, opts *LogOptions, runtimeService internalapi.RuntimeService, stdout, stderr io.Writer) error {
	// fsnotify has different behavior for symlinks in different platform,
	// for example it follows symlink on Linux, but not on Windows,
	// so we explicitly resolve symlinks before reading the logs.
	// There shouldn't be security issue because the container log
	// path is owned by kubelet and the container runtime.
	evaluated, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("failed to try resolving symlinks in path %q: %v", path, err)
	}
	path = evaluated
	f, err := openFileShareDelete(path)
	if err != nil {
		return fmt.Errorf("failed to open log file %q: %v", path, err)
	}
	defer func() {
		//nolint:errcheck
		f.Close()
	}()

	// Search start point based on tail line.
	start, err := findTailLineStartIndex(f, opts.tail)
	if err != nil {
		return fmt.Errorf("failed to tail %d lines of log file %q: %v", opts.tail, path, err)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return fmt.Errorf("failed to seek %d in log file %q: %v", start, path, err)
	}

	limitedMode := (opts.tail >= 0) && (!opts.follow)
	limitedNum := opts.tail
	// Start parsing the logs.
	r := bufio.NewReader(f)
	// Do not create watcher here because it is not needed if `Follow` is false.
	var watcher *fsnotify.Watcher
	var parse parseFunc
	var stop bool
	isNewLine := true
	found := true
	writer := newLogWriter(stdout, stderr, opts)
	msg := &logMessage{}
	baseName := filepath.Base(path)
	dir := filepath.Dir(path)
	var de *dedupWriteEvents
	for {
		if stop || (limitedMode && limitedNum == 0) {
			internal.Log(logger, 2, "Finished parsing log file", "path", path)
			return nil
		}
		l, err := r.ReadBytes(eol[0])
		if err != nil {
			if err != io.EOF { // This is an real error
				return fmt.Errorf("failed to read log file %q: %v", path, err)
			}
			if opts.follow {
				// The container is not running, we got to the end of the log.
				if !found {
					return nil
				}
				// Reset seek so that if this is an incomplete line,
				// it will be read again.
				if _, err := f.Seek(-int64(len(l)), io.SeekCurrent); err != nil {
					return fmt.Errorf("failed to reset seek in log file %q: %v", path, err)
				}
				if watcher == nil {
					// Initialize the watcher if it has not been initialized yet.
					if watcher, err = fsnotify.NewWatcher(); err != nil {
						return fmt.Errorf("failed to create fsnotify watcher: %v", err)
					}
					if err := watcher.Add(dir); err != nil {
						return fmt.Errorf("failed to watch directory %q: %w", dir, err)
					}

					de = newDedupWriteEvents(watcher, baseName, logger)
					de.dedupEvents(ctx)
					defer de.Cleanup()
					//nolint:errcheck
					defer watcher.Close()
					// If we just created the watcher, try again to read as we might have missed
					// the event.
					continue
				}
				var recreated bool
				// Wait until the next log change.
				found, recreated, err = de.waitLogs(ctx, containerID, runtimeService)
				if err != nil {
					return err
				}
				if recreated {
					newF, err := openFileShareDelete(path)
					if err != nil {
						if os.IsNotExist(err) {
							continue
						}
						return fmt.Errorf("failed to open log file %q: %v", path, err)
					}
					f.Close()
					f = newF
					r = bufio.NewReader(f)
				}
				// If the container exited consume data until the next EOF
				continue
			}
			// Should stop after writing the remaining content.
			stop = true
			if len(l) == 0 {
				continue
			}
			internal.Log(logger, 0, "Incomplete line in log file", "path", path, "line", l)
		}
		if parse == nil {
			// Initialize the log parsing function.
			parse, err = getParseFunc(l)
			if err != nil {
				return fmt.Errorf("failed to get parse function: %v", err)
			}
		}
		// Parse the log line.
		msg.reset()
		if err := parse(l, msg); err != nil {
			internal.LogErr(logger, err, "Failed when parsing line in log file", "path", path, "line", l)
			continue
		}
		// Write the log line into the stream.
		if err := writer.write(msg, isNewLine); err != nil {
			if err == errMaximumWrite {
				internal.Log(logger, 2, "Finished parsing log file, hit bytes limit", "path", path, "limit", opts.bytes)
				return nil
			}
			internal.LogErr(logger, err, "Failed when writing line to log file", "path", path, "line", msg)
			return err
		}
		if limitedMode {
			limitedNum--
		}
		if len(msg.log) > 0 {
			isNewLine = msg.log[len(msg.log)-1] == eol[0]
		} else {
			isNewLine = true
		}
	}
}

func isContainerRunning(ctx context.Context, logger *klog.Logger, id string, r internalapi.RuntimeService) (bool, error) {
	resp, err := r.ContainerStatus(ctx, id, false)
	if err != nil {
		// Assume that the container is still running when the runtime is
		// unavailable. Most runtimes support that containers can be in running
		// state even if their CRI server is not available right now.
		code := status.Code(err)
		if code == codes.Unavailable {
			return true, nil
		} else if code == codes.NotFound {
			internal.Log(logger, 5, "Container not found", "containerId", id)
			return false, nil
		}

		return false, err
	}
	status := resp.GetStatus()
	if status == nil {
		return false, remote.ErrContainerStatusNil
	}
	// Only keep following container log when it is running.
	if status.State != runtimeapi.ContainerState_CONTAINER_RUNNING {
		internal.Log(logger, 5, "Container is not running", "containerId", id, "state", status.State)
		// Do not return error because it's normal that the container stops
		// during waiting.
		return false, nil
	}
	return true, nil
}

// waitLogs wait for the next log write. It returns two booleans and an error. The first boolean
// indicates whether a new log is found; the second boolean if the log file was recreated;
// the error is error happens during waiting new logs.
func (de *dedupWriteEvents) waitLogs(ctx context.Context, id string, runtimeService internalapi.RuntimeService) (bool, bool, error) {
	// no need to wait if the pod is not running
	if running, err := isContainerRunning(ctx, de.logger, id, runtimeService); !running {
		return false, false, err
	}

	for {
		select {
		case <-ctx.Done():
			return false, false, fmt.Errorf("context cancelled")
		case e, ok := <-de.events:
			if !ok {
				return false, false, nil
			} else {
				if e.Err != nil {
					return false, false, e.Err
				}
				return true, e.Event.Op == fsnotify.Create, nil
			}

		case <-time.After(logForceCheckPeriod):
			return true, false, nil
		}
	}
}

func newDedupWriteEvents(w *fsnotify.Watcher, logfileName string, logger *klog.Logger) *dedupWriteEvents {
	return &dedupWriteEvents{
		w:           w,
		logger:      logger,
		logFileName: logfileName,
		events:      make(chan fileEvent, 4),
	}
}

type dedupWriteEvents struct {
	w           *fsnotify.Watcher
	logger      *klog.Logger
	logFileName string
	events      chan fileEvent
}

type fileEvent struct {
	Event fsnotify.Event
	Err   error
}

func (de *dedupWriteEvents) Cleanup() {
	// clean all events in channel
	for range de.events {
	}
}

// dedupEvents deduplicate Write events.
func (de *dedupWriteEvents) dedupEvents(ctx context.Context) {
	go func() {
		waitDuration := 150 * time.Millisecond
		ticker := time.NewTicker(waitDuration)

		var lastAdd time.Time
		defer func() {
			close(de.events)
			ticker.Stop()
			<-ticker.C
		}()

		errRetry := 2
		for {
			select {
			case <-ctx.Done():
				return

			case err := <-de.w.Errors:
				internal.LogErr(de.logger, err, "Received fsnotify watch error, retrying unless no more retries left", "retries", errRetry)
				if errRetry == 0 {
					de.events <- fileEvent{Err: err}
					return
				}
				errRetry--
			case <-ticker.C:
				errRetry = 2

			case e, ok := <-de.w.Events:
				if !ok { // Channel was closed (i.e. Watcher.Close() was called).
					return
				}

				if filepath.Base(e.Name) != de.logFileName {
					continue
				}

				switch e.Op {
				case fsnotify.Write:
					// If there is one Write event in the channel, we can discard this one.
					// If there is one Create event in the channel, we will reopen the log file, and read from the
					// start. It's OK to discard the new Write event.
					if len(de.events) > 0 ||
						time.Since(lastAdd) < waitDuration {
						continue
					}
					lastAdd = time.Now()
				case fsnotify.Create:
					// Always add a write event before create event. In case lost some log lines.
					de.events <- fileEvent{
						Event: fsnotify.Event{
							Name: e.Name,
							Op:   fsnotify.Write,
						},
					}
					lastAdd = time.Now()
				default:
					continue
				}

				de.events <- fileEvent{Event: e}
			}
		}
	}()
}
