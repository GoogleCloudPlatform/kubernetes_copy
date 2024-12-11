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

package schedule

import (
	"context"
	"sync"

	"go.uber.org/zap"

	"go.etcd.io/etcd/client/pkg/v3/verify"
)

type Job interface {
	Name() string
	Do(context.Context)
}

type job struct {
	name string
	do   func(context.Context)
}

func (j job) Name() string {
	return j.name
}

func (j job) Do(ctx context.Context) {
	j.do(ctx)
}

func NewJob(name string, do func(ctx context.Context)) Job {
	return job{
		name: name,
		do:   do,
	}
}

// Scheduler can schedule jobs.
type Scheduler interface {
	// Schedule asks the scheduler to schedule a job defined by the given func.
	// Schedule to a stopped scheduler might panic.
	Schedule(j Job)

	// Pending returns number of pending jobs
	Pending() int

	// Scheduled returns the number of scheduled jobs (excluding pending jobs)
	Scheduled() int

	// Finished returns the number of finished jobs
	Finished() int

	// WaitFinish waits until at least n job are finished and all pending jobs are finished.
	WaitFinish(n int)

	// Stop stops the scheduler.
	Stop()
}

type fifo struct {
	mu sync.Mutex

	resume    chan struct{}
	scheduled int
	finished  int
	pendings  []Job

	ctx    context.Context
	cancel context.CancelFunc

	finishCond *sync.Cond
	donec      chan struct{}
	lg         *zap.Logger
}

// NewFIFOScheduler returns a Scheduler that schedules jobs in FIFO
// order sequentially
func NewFIFOScheduler(lg *zap.Logger) Scheduler {
	verify.Assert(lg != nil, "the logger should not be nil")

	f := &fifo{
		resume: make(chan struct{}, 1),
		donec:  make(chan struct{}, 1),
		lg:     lg,
	}
	f.finishCond = sync.NewCond(&f.mu)
	f.ctx, f.cancel = context.WithCancel(context.Background())
	go f.run()
	return f
}

// Schedule schedules a job that will be ran in FIFO order sequentially.
func (f *fifo) Schedule(j Job) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.cancel == nil {
		panic("schedule: schedule to stopped scheduler")
	}

	if len(f.pendings) == 0 {
		select {
		case f.resume <- struct{}{}:
		default:
		}
	}
	f.pendings = append(f.pendings, j)
}

func (f *fifo) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pendings)
}

func (f *fifo) Scheduled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scheduled
}

func (f *fifo) Finished() int {
	f.finishCond.L.Lock()
	defer f.finishCond.L.Unlock()
	return f.finished
}

func (f *fifo) WaitFinish(n int) {
	f.finishCond.L.Lock()
	for f.finished < n || len(f.pendings) != 0 {
		f.finishCond.Wait()
	}
	f.finishCond.L.Unlock()
}

// Stop stops the scheduler and cancels all pending jobs.
func (f *fifo) Stop() {
	f.mu.Lock()
	f.cancel()
	f.cancel = nil
	f.mu.Unlock()
	<-f.donec
}

func (f *fifo) run() {
	defer func() {
		close(f.donec)
		close(f.resume)
	}()

	for {
		var todo Job
		f.mu.Lock()
		if len(f.pendings) != 0 {
			f.scheduled++
			todo = f.pendings[0]
		}
		f.mu.Unlock()
		if todo == nil {
			select {
			case <-f.resume:
			case <-f.ctx.Done():
				f.mu.Lock()
				pendings := f.pendings
				f.pendings = nil
				f.mu.Unlock()
				// clean up pending jobs
				for _, todo := range pendings {
					f.executeJob(todo, true)
				}
				return
			}
		} else {
			f.executeJob(todo, false)
		}
	}
}

func (f *fifo) executeJob(todo Job, updatedFinishedStats bool) {
	defer func() {
		if !updatedFinishedStats {
			f.finishCond.L.Lock()
			f.finished++
			f.pendings = f.pendings[1:]
			f.finishCond.Broadcast()
			f.finishCond.L.Unlock()
		}
		if err := recover(); err != nil {
			f.lg.Panic("execute job failed", zap.String("job", todo.Name()), zap.Any("panic", err))
		}
	}()

	todo.Do(f.ctx)
}
