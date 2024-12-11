// Copyright 2019 The etcd Authors
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

// Package traceutil implements tracing utilities using "context".
package traceutil

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"go.uber.org/zap"
)

// TraceKey is used as a key of context for Trace.
type TraceKey struct{}

// StartTimeKey is used as a key of context for start time of operation.
type StartTimeKey struct{}

// Field is a kv pair to record additional details of the trace.
type Field struct {
	Key   string
	Value any
}

func (f *Field) format() string {
	return fmt.Sprintf("%s:%v; ", f.Key, f.Value)
}

func writeFields(fields []Field) string {
	if len(fields) == 0 {
		return ""
	}
	var buf strings.Builder
	buf.WriteString("{")
	for _, f := range fields {
		buf.WriteString(f.format())
	}
	buf.WriteString("}")
	return buf.String()
}

type Trace struct {
	operation    string
	lg           *zap.Logger
	fields       []Field
	startTime    time.Time
	steps        []step
	stepDisabled bool
	isEmpty      bool
}

type step struct {
	time            time.Time
	msg             string
	fields          []Field
	isSubTraceStart bool
	isSubTraceEnd   bool
}

func New(op string, lg *zap.Logger, fields ...Field) *Trace {
	return &Trace{operation: op, lg: lg, startTime: time.Now(), fields: fields}
}

// TODO returns a non-nil, empty Trace
func TODO() *Trace {
	return &Trace{isEmpty: true}
}

func Get(ctx context.Context) *Trace {
	if trace, ok := ctx.Value(TraceKey{}).(*Trace); ok && trace != nil {
		return trace
	}
	return TODO()
}

func (t *Trace) GetStartTime() time.Time {
	return t.startTime
}

func (t *Trace) SetStartTime(time time.Time) {
	t.startTime = time
}

func (t *Trace) InsertStep(at int, time time.Time, msg string, fields ...Field) {
	newStep := step{time: time, msg: msg, fields: fields}
	if at < len(t.steps) {
		t.steps = append(t.steps[:at+1], t.steps[at:]...)
		t.steps[at] = newStep
	} else {
		t.steps = append(t.steps, newStep)
	}
}

// StartSubTrace adds step to trace as a start sign of sublevel trace
// All steps in the subtrace will log out the input fields of this function
func (t *Trace) StartSubTrace(fields ...Field) {
	t.steps = append(t.steps, step{fields: fields, isSubTraceStart: true})
}

// StopSubTrace adds step to trace as a end sign of sublevel trace
// All steps in the subtrace will log out the input fields of this function
func (t *Trace) StopSubTrace(fields ...Field) {
	t.steps = append(t.steps, step{fields: fields, isSubTraceEnd: true})
}

// Step adds step to trace
func (t *Trace) Step(msg string, fields ...Field) {
	if !t.stepDisabled {
		t.steps = append(t.steps, step{time: time.Now(), msg: msg, fields: fields})
	}
}

// StepWithFunction will measure the input function as a single step
func (t *Trace) StepWithFunction(f func(), msg string, fields ...Field) {
	t.disableStep()
	f()
	t.enableStep()
	t.Step(msg, fields...)
}

func (t *Trace) AddField(fields ...Field) {
	for _, f := range fields {
		if !t.updateFieldIfExist(f) {
			t.fields = append(t.fields, f)
		}
	}
}

func (t *Trace) IsEmpty() bool {
	return t.isEmpty
}

// Log dumps all steps in the Trace
func (t *Trace) Log() {
	t.LogWithStepThreshold(0)
}

// LogIfLong dumps logs if the duration is longer than threshold
func (t *Trace) LogIfLong(threshold time.Duration) {
	if time.Since(t.startTime) > threshold {
		stepThreshold := threshold / time.Duration(len(t.steps)+1)
		t.LogWithStepThreshold(stepThreshold)
	}
}

// LogAllStepsIfLong dumps all logs if the duration is longer than threshold
func (t *Trace) LogAllStepsIfLong(threshold time.Duration) {
	if time.Since(t.startTime) > threshold {
		t.LogWithStepThreshold(0)
	}
}

// LogWithStepThreshold only dumps step whose duration is longer than step threshold
func (t *Trace) LogWithStepThreshold(threshold time.Duration) {
	msg, fs := t.logInfo(threshold)
	if t.lg != nil {
		t.lg.Info(msg, fs...)
	}
}

func (t *Trace) logInfo(threshold time.Duration) (string, []zap.Field) {
	endTime := time.Now()
	totalDuration := endTime.Sub(t.startTime)
	traceNum := rand.Int31()
	msg := fmt.Sprintf("trace[%d] %s", traceNum, t.operation)

	var steps []string
	lastStepTime := t.startTime
	for i := 0; i < len(t.steps); i++ {
		tstep := t.steps[i]
		// add subtrace common fields which defined at the beginning to each sub-steps
		if tstep.isSubTraceStart {
			for j := i + 1; j < len(t.steps) && !t.steps[j].isSubTraceEnd; j++ {
				t.steps[j].fields = append(tstep.fields, t.steps[j].fields...)
			}
			continue
		}
		// add subtrace common fields which defined at the end to each sub-steps
		if tstep.isSubTraceEnd {
			for j := i - 1; j >= 0 && !t.steps[j].isSubTraceStart; j-- {
				t.steps[j].fields = append(tstep.fields, t.steps[j].fields...)
			}
			continue
		}
	}
	for i := 0; i < len(t.steps); i++ {
		tstep := t.steps[i]
		if tstep.isSubTraceStart || tstep.isSubTraceEnd {
			continue
		}
		stepDuration := tstep.time.Sub(lastStepTime)
		if stepDuration > threshold {
			steps = append(steps, fmt.Sprintf("trace[%d] '%v' %s (duration: %v)",
				traceNum, tstep.msg, writeFields(tstep.fields), stepDuration))
		}
		lastStepTime = tstep.time
	}

	fs := []zap.Field{zap.String("detail", writeFields(t.fields)),
		zap.Duration("duration", totalDuration),
		zap.Time("start", t.startTime),
		zap.Time("end", endTime),
		zap.Strings("steps", steps),
		zap.Int("step_count", len(steps))}
	return msg, fs
}

func (t *Trace) updateFieldIfExist(f Field) bool {
	for i, v := range t.fields {
		if v.Key == f.Key {
			t.fields[i].Value = f.Value
			return true
		}
	}
	return false
}

// disableStep sets the flag to prevent the trace from adding steps
func (t *Trace) disableStep() {
	t.stepDisabled = true
}

// enableStep re-enable the trace to add steps
func (t *Trace) enableStep() {
	t.stepDisabled = false
}
