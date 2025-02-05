/*
Copyright 2019 The Kubernetes Authors.

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

package queueset

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"

	"k8s.io/apiserver/pkg/authentication/user"
	genericrequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/util/flowcontrol/counter"
	"k8s.io/apiserver/pkg/util/flowcontrol/debug"
	fq "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing"
	"k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/promise"
	test "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/testing"
	testeventclock "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/testing/eventclock"
	testpromise "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/testing/promise"
	"k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	fcrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/klog/v2"
)

// fairAlloc computes the max-min fair allocation of the given
// capacity to the given demands (which slice is not side-effected).
func fairAlloc(demands []float64, capacity float64) []float64 {
	count := len(demands)
	indices := make([]int, count)
	for i := 0; i < count; i++ {
		indices[i] = i
	}
	sort.Slice(indices, func(i, j int) bool { return demands[indices[i]] < demands[indices[j]] })
	alloc := make([]float64, count)
	var next int
	var prevAlloc float64
	for ; next < count; next++ {
		// `capacity` is how much remains assuming that
		// all unvisited items get `prevAlloc`.
		idx := indices[next]
		demand := demands[idx]
		if demand <= 0 {
			continue
		}
		// `fullCapacityBite` is how much more capacity would be used
		// if this and all following items get as much as this one
		// is demanding.
		fullCapacityBite := float64(count-next) * (demand - prevAlloc)
		if fullCapacityBite > capacity {
			break
		}
		prevAlloc = demand
		alloc[idx] = demand
		capacity -= fullCapacityBite
	}
	for j := next; j < count; j++ {
		alloc[indices[j]] = prevAlloc + capacity/float64(count-next)
	}
	return alloc
}

func TestFairAlloc(t *testing.T) {
	if e, a := []float64{0, 0}, fairAlloc([]float64{0, 0}, 42); !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#+v, got #%+v", e, a)
	}
	if e, a := []float64{42, 0}, fairAlloc([]float64{47, 0}, 42); !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#+v, got #%+v", e, a)
	}
	if e, a := []float64{1, 41}, fairAlloc([]float64{1, 47}, 42); !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#+v, got #%+v", e, a)
	}
	if e, a := []float64{3, 5, 5, 1}, fairAlloc([]float64{3, 7, 9, 1}, 14); !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#+v, got #%+v", e, a)
	}
	if e, a := []float64{1, 9, 7, 3}, fairAlloc([]float64{1, 9, 7, 3}, 21); !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#+v, got #%+v", e, a)
	}
}

type uniformClient struct {
	hash     uint64
	nThreads int
	nCalls   int
	// duration for a simulated synchronous call
	execDuration time.Duration
	// duration for simulated "other work".  This can be negative,
	// causing a request to be launched a certain amount of time
	// before the previous one finishes.
	thinkDuration time.Duration
	// padDuration is additional time during which this request occupies its seats.
	// This comes at the end of execution, after the reply has been released toward
	// the client.
	// The evaluation code below does not take this into account.
	// In cases where `padDuration` makes a difference,
	// set the `expectedAverages` field of `uniformScenario`.
	padDuration time.Duration
	// When true indicates that only half the specified number of
	// threads should run during the first half of the evaluation
	// period
	split bool
	// initialSeats is the number of seats this request occupies in the first phase of execution
	initialSeats uint64
	// finalSeats is the number occupied during the second phase of execution
	finalSeats uint64
}

func newUniformClient(hash uint64, nThreads, nCalls int, execDuration, thinkDuration time.Duration) uniformClient {
	return uniformClient{
		hash:          hash,
		nThreads:      nThreads,
		nCalls:        nCalls,
		execDuration:  execDuration,
		thinkDuration: thinkDuration,
		initialSeats:  1,
		finalSeats:    1,
	}
}

func (uc uniformClient) setSplit() uniformClient {
	uc.split = true
	return uc
}

func (uc uniformClient) setInitWidth(seats uint64) uniformClient {
	uc.initialSeats = seats
	return uc
}

func (uc uniformClient) pad(finalSeats int, duration time.Duration) uniformClient {
	uc.finalSeats = uint64(finalSeats)
	uc.padDuration = duration
	return uc
}

// uniformScenario describes a scenario based on the given set of uniform clients.
// Each uniform client specifies a number of threads, each of which alternates between thinking
// and making a synchronous request through the QueueSet.
// The test measures how much concurrency each client got, on average, over
// the initial evalDuration and tests to see whether they all got about the fair amount.
// Each client needs to be demanding enough to use more than its fair share,
// or overall care needs to be taken about timing so that scheduling details
// do not cause any client to actually request a significantly smaller share
// than it theoretically should.
// expectFair indicate whether the QueueSet is expected to be
// fair in the respective halves of a split scenario;
// in a non-split scenario this is a singleton with one expectation.
// expectAllRequests indicates whether all requests are expected to get dispatched.
// expectedAverages, if provided, replaces the normal calculation of expected results.
type uniformScenario struct {
	name                                     string
	qs                                       fq.QueueSet
	clients                                  []uniformClient
	concurrencyLimit                         int
	evalDuration                             time.Duration
	expectedFair                             []bool
	expectedFairnessMargin                   []float64
	expectAllRequests                        bool
	evalInqueueMetrics, evalExecutingMetrics bool
	rejectReason                             string
	clk                                      *testeventclock.Fake
	counter                                  counter.GoRoutineCounter
	expectedAverages                         []float64
	expectedEpochAdvances                    int
	seatDemandIntegratorSubject              fq.Integrator
	dontDump                                 bool
}

func (us uniformScenario) exercise(t *testing.T) {
	uss := uniformScenarioState{
		t:                         t,
		uniformScenario:           us,
		startTime:                 us.clk.Now(),
		execSeatsIntegrators:      make([]fq.Integrator, len(us.clients)),
		seatDemandIntegratorCheck: fq.NewNamedIntegrator(us.clk, us.name+"-seatDemandCheck"),
		executions:                make([]int32, len(us.clients)),
		rejects:                   make([]int32, len(us.clients)),
	}
	for _, uc := range us.clients {
		uss.doSplit = uss.doSplit || uc.split
	}
	uss.exercise()
}

type uniformScenarioState struct {
	t *testing.T
	uniformScenario
	startTime                                                                              time.Time
	doSplit                                                                                bool
	execSeatsIntegrators                                                                   []fq.Integrator
	seatDemandIntegratorCheck                                                              fq.Integrator
	failedCount                                                                            uint64
	expectedInqueueReqs, expectedInqueueSeats, expectedExecuting, expectedConcurrencyInUse string
	executions, rejects                                                                    []int32
}

func (uss *uniformScenarioState) exercise() {
	uss.t.Logf("%s: Start %s, doSplit=%v, clk=%p, grc=%p", uss.startTime.Format(nsTimeFmt), uss.name, uss.doSplit, uss.clk, uss.counter)
	if uss.evalInqueueMetrics || uss.evalExecutingMetrics {
		metrics.Reset()
	}
	for i, uc := range uss.clients {
		uss.execSeatsIntegrators[i] = fq.NewNamedIntegrator(uss.clk, fmt.Sprintf("%s client %d execSeats", uss.name, i))
		fsName := fmt.Sprintf("client%d", i)
		uss.expectedInqueueReqs = uss.expectedInqueueReqs + fmt.Sprintf(`				apiserver_flowcontrol_current_inqueue_requests{flow_schema=%q,priority_level=%q} 0%s`, fsName, uss.name, "\n")
		uss.expectedInqueueSeats = uss.expectedInqueueSeats + fmt.Sprintf(`				apiserver_flowcontrol_current_inqueue_seats{flow_schema=%q,priority_level=%q} 0%s`, fsName, uss.name, "\n")
		for j := 0; j < uc.nThreads; j++ {
			ust := uniformScenarioThread{
				uss:                 uss,
				i:                   i,
				j:                   j,
				nCalls:              uc.nCalls,
				uc:                  uc,
				execSeatsIntegrator: uss.execSeatsIntegrators[i],
				fsName:              fsName,
			}
			ust.start()
		}
	}
	if uss.doSplit {
		uss.evalTo(uss.startTime.Add(uss.evalDuration/2), false, uss.expectedFair[0], uss.expectedFairnessMargin[0])
	}
	uss.evalTo(uss.startTime.Add(uss.evalDuration), true, uss.expectedFair[len(uss.expectedFair)-1], uss.expectedFairnessMargin[len(uss.expectedFairnessMargin)-1])
	uss.clk.Run(nil)
	uss.finalReview()
}

type uniformScenarioThread struct {
	uss                 *uniformScenarioState
	i, j                int
	nCalls              int
	uc                  uniformClient
	execSeatsIntegrator fq.Integrator
	fsName              string
}

func (ust *uniformScenarioThread) start() {
	initialDelay := time.Duration(90*ust.j + 20*ust.i)
	if ust.uc.split && ust.j >= ust.uc.nThreads/2 {
		initialDelay += ust.uss.evalDuration / 2
		ust.nCalls = ust.nCalls / 2
	}
	ust.uss.clk.EventAfterDuration(ust.genCallK(0), initialDelay)
}

// generates an EventFunc that does call k
func (ust *uniformScenarioThread) genCallK(k int) func(time.Time) {
	return func(time.Time) {
		ust.callK(k)
	}
}

func (ust *uniformScenarioThread) callK(k int) {
	if k >= ust.nCalls {
		return
	}
	maxWidth := float64(uint64max(ust.uc.initialSeats, ust.uc.finalSeats))
	ust.uss.seatDemandIntegratorCheck.Add(maxWidth)
	returnSeatDemand := func(time.Time) { ust.uss.seatDemandIntegratorCheck.Add(-maxWidth) }
	ctx := context.Background()
	username := fmt.Sprintf("%d:%d:%d", ust.i, ust.j, k)
	ctx = genericrequest.WithUser(ctx, &user.DefaultInfo{Name: username})
	req, idle := ust.uss.qs.StartRequest(ctx, &fcrequest.WorkEstimate{InitialSeats: ust.uc.initialSeats, FinalSeats: ust.uc.finalSeats, AdditionalLatency: ust.uc.padDuration}, ust.uc.hash, "", ust.fsName, ust.uss.name, []int{ust.i, ust.j, k}, nil)
	ust.uss.t.Logf("%s: %d, %d, %d got req=%p, idle=%v", ust.uss.clk.Now().Format(nsTimeFmt), ust.i, ust.j, k, req, idle)
	if req == nil {
		atomic.AddUint64(&ust.uss.failedCount, 1)
		atomic.AddInt32(&ust.uss.rejects[ust.i], 1)
		returnSeatDemand(ust.uss.clk.Now())
		return
	}
	if idle {
		ust.uss.t.Error("got request but QueueSet reported idle")
	}
	if (!ust.uss.dontDump) && k%100 == 0 {
		insistRequestFromUser(ust.uss.t, ust.uss.qs, username)
	}
	var executed bool
	var returnTime time.Time
	idle2 := req.Finish(func() {
		executed = true
		execStart := ust.uss.clk.Now()
		atomic.AddInt32(&ust.uss.executions[ust.i], 1)
		ust.execSeatsIntegrator.Add(float64(ust.uc.initialSeats))
		ust.uss.t.Logf("%s: %d, %d, %d executing; width1=%d", execStart.Format(nsTimeFmt), ust.i, ust.j, k, ust.uc.initialSeats)
		ust.uss.clk.EventAfterDuration(ust.genCallK(k+1), ust.uc.execDuration+ust.uc.thinkDuration)
		ust.uss.clk.Sleep(ust.uc.execDuration)
		ust.execSeatsIntegrator.Add(-float64(ust.uc.initialSeats))
		ust.uss.clk.EventAfterDuration(returnSeatDemand, ust.uc.padDuration)
		returnTime = ust.uss.clk.Now()
	})
	now := ust.uss.clk.Now()
	ust.uss.t.Logf("%s: %d, %d, %d got executed=%v, idle2=%v", now.Format(nsTimeFmt), ust.i, ust.j, k, executed, idle2)
	if !executed {
		atomic.AddUint64(&ust.uss.failedCount, 1)
		atomic.AddInt32(&ust.uss.rejects[ust.i], 1)
		returnSeatDemand(ust.uss.clk.Now())
	} else if now != returnTime {
		ust.uss.t.Errorf("%s: %d, %d, %d returnTime=%s", now.Format(nsTimeFmt), ust.i, ust.j, k, returnTime.Format(nsTimeFmt))
	}
}

func insistRequestFromUser(t *testing.T, qs fq.QueueSet, username string) {
	qsd := qs.Dump(true)
	goodRequest := func(rd debug.RequestDump) bool {
		return rd.UserName == username
	}
	goodSliceOfRequests := SliceMapReduce(goodRequest, or)
	if goodSliceOfRequests(qsd.QueuelessExecutingRequests) {
		t.Logf("Found user %s among queueless requests", username)
		return
	}
	goodQueueDump := func(qd debug.QueueDump) bool {
		return goodSliceOfRequests(qd.Requests) || goodSliceOfRequests(qd.RequestsExecuting)
	}
	if SliceMapReduce(goodQueueDump, or)(qsd.Queues) {
		t.Logf("Found user %s among queued requests", username)
		return
	}
	t.Errorf("Failed to find request from user %s", username)
}

func (uss *uniformScenarioState) evalTo(lim time.Time, last, expectFair bool, margin float64) {
	uss.clk.Run(&lim)
	uss.clk.SetTime(lim)
	if uss.doSplit && !last {
		uss.t.Logf("%s: End of first half of scenario %q", uss.clk.Now().Format(nsTimeFmt), uss.name)
	} else {
		uss.t.Logf("%s: End of scenario %q", uss.clk.Now().Format(nsTimeFmt), uss.name)
	}
	demands := make([]float64, len(uss.clients))
	averages := make([]float64, len(uss.clients))
	for i, uc := range uss.clients {
		nThreads := uc.nThreads
		if uc.split && !last {
			nThreads = nThreads / 2
		}
		sep := uc.thinkDuration
		demands[i] = float64(nThreads) * float64(uc.initialSeats) * float64(uc.execDuration) / float64(sep+uc.execDuration)
		averages[i] = uss.execSeatsIntegrators[i].Reset().Average
	}
	fairAverages := uss.expectedAverages
	if fairAverages == nil {
		fairAverages = fairAlloc(demands, float64(uss.concurrencyLimit))
	}
	for i := range uss.clients {
		expectedAverage := fairAverages[i]
		var gotFair bool
		if expectedAverage > 0 {
			relDiff := (averages[i] - expectedAverage) / expectedAverage
			gotFair = math.Abs(relDiff) <= margin
		} else {
			gotFair = math.Abs(averages[i]) <= margin
		}

		if gotFair != expectFair {
			uss.t.Errorf("%s client %d last=%v expectFair=%v margin=%v got an Average of %v but the expected average was %v", uss.name, i, last, expectFair, margin, averages[i], expectedAverage)
		} else {
			uss.t.Logf("%s client %d last=%v expectFair=%v margin=%v got an Average of %v and the expected average was %v", uss.name, i, last, expectFair, margin, averages[i], expectedAverage)
		}
	}
	if uss.seatDemandIntegratorSubject != nil {
		checkResults := uss.seatDemandIntegratorCheck.GetResults()
		subjectResults := uss.seatDemandIntegratorSubject.GetResults()
		if float64close(subjectResults.Duration, checkResults.Duration) {
			uss.t.Logf("%s last=%v got duration of %v and expected %v", uss.name, last, subjectResults.Duration, checkResults.Duration)
		} else {
			uss.t.Errorf("%s last=%v got duration of %v but expected %v", uss.name, last, subjectResults.Duration, checkResults.Duration)
		}
		if got, expected := float64NaNTo0(subjectResults.Average), float64NaNTo0(checkResults.Average); float64close(got, expected) {
			uss.t.Logf("%s last=%v got SeatDemand average of %v and expected %v", uss.name, last, got, expected)
		} else {
			uss.t.Errorf("%s last=%v got SeatDemand average of %v but expected %v", uss.name, last, got, expected)
		}
		if got, expected := float64NaNTo0(subjectResults.Deviation), float64NaNTo0(checkResults.Deviation); float64close(got, expected) {
			uss.t.Logf("%s last=%v got SeatDemand standard deviation of %v and expected %v", uss.name, last, got, expected)
		} else {
			uss.t.Errorf("%s last=%v got SeatDemand standard deviation of %v but expected %v", uss.name, last, got, expected)
		}
	}
}

func (uss *uniformScenarioState) finalReview() {
	if uss.expectAllRequests && uss.failedCount > 0 {
		uss.t.Errorf("Expected all requests to be successful but got %v failed requests", uss.failedCount)
	} else if !uss.expectAllRequests && uss.failedCount == 0 {
		uss.t.Errorf("Expected failed requests but all requests succeeded")
	}
	if uss.evalInqueueMetrics {
		e := `
				# HELP apiserver_flowcontrol_current_inqueue_requests [BETA] Number of requests currently pending in queues of the API Priority and Fairness subsystem
				# TYPE apiserver_flowcontrol_current_inqueue_requests gauge
` + uss.expectedInqueueReqs
		err := metrics.GatherAndCompare(e, "apiserver_flowcontrol_current_inqueue_requests")
		if err != nil {
			uss.t.Error(err)
		} else {
			uss.t.Log("Success with" + e)
		}

		e = `
				# HELP apiserver_flowcontrol_current_inqueue_seats [ALPHA] Number of seats currently pending in queues of the API Priority and Fairness subsystem
				# TYPE apiserver_flowcontrol_current_inqueue_seats gauge
` + uss.expectedInqueueSeats
		err = metrics.GatherAndCompare(e, "apiserver_flowcontrol_current_inqueue_seats")
		if err != nil {
			uss.t.Error(err)
		} else {
			uss.t.Log("Success with" + e)
		}
	}
	expectedRejects := ""
	for i := range uss.clients {
		fsName := fmt.Sprintf("client%d", i)
		if atomic.LoadInt32(&uss.executions[i]) > 0 {
			uss.expectedExecuting = uss.expectedExecuting + fmt.Sprintf(`				apiserver_flowcontrol_current_executing_requests{flow_schema=%q,priority_level=%q} 0%s`, fsName, uss.name, "\n")
			uss.expectedConcurrencyInUse = uss.expectedConcurrencyInUse + fmt.Sprintf(`				apiserver_flowcontrol_request_concurrency_in_use{flow_schema=%q,priority_level=%q} 0%s`, fsName, uss.name, "\n")
		}
		if atomic.LoadInt32(&uss.rejects[i]) > 0 {
			expectedRejects = expectedRejects + fmt.Sprintf(`				apiserver_flowcontrol_rejected_requests_total{flow_schema=%q,priority_level=%q,reason=%q} %d%s`, fsName, uss.name, uss.rejectReason, uss.rejects[i], "\n")
		}
	}
	if uss.evalExecutingMetrics && len(uss.expectedExecuting) > 0 {
		e := `
				# HELP apiserver_flowcontrol_current_executing_requests [BETA] Number of requests in initial (for a WATCH) or any (for a non-WATCH) execution stage in the API Priority and Fairness subsystem
				# TYPE apiserver_flowcontrol_current_executing_requests gauge
` + uss.expectedExecuting
		err := metrics.GatherAndCompare(e, "apiserver_flowcontrol_current_executing_requests")
		if err != nil {
			uss.t.Error(err)
		} else {
			uss.t.Log("Success with" + e)
		}
	}
	if uss.evalExecutingMetrics && len(uss.expectedConcurrencyInUse) > 0 {
		e := `
				# HELP apiserver_flowcontrol_request_concurrency_in_use [ALPHA] Concurrency (number of seats) occupied by the currently executing (initial stage for a WATCH, any stage otherwise) requests in the API Priority and Fairness subsystem
				# TYPE apiserver_flowcontrol_request_concurrency_in_use gauge
` + uss.expectedConcurrencyInUse
		err := metrics.GatherAndCompare(e, "apiserver_flowcontrol_request_concurrency_in_use")
		if err != nil {
			uss.t.Error(err)
		} else {
			uss.t.Log("Success with" + e)
		}
	}
	if uss.evalExecutingMetrics && len(expectedRejects) > 0 {
		e := `
				# HELP apiserver_flowcontrol_rejected_requests_total [BETA] Number of requests rejected by API Priority and Fairness subsystem
				# TYPE apiserver_flowcontrol_rejected_requests_total counter
` + expectedRejects
		err := metrics.GatherAndCompare(e, "apiserver_flowcontrol_rejected_requests_total")
		if err != nil {
			uss.t.Error(err)
		} else {
			uss.t.Log("Success with" + e)
		}
	}
	e := ""
	if uss.expectedEpochAdvances > 0 {
		e = fmt.Sprintf(`        # HELP apiserver_flowcontrol_epoch_advance_total [ALPHA] Number of times the queueset's progress meter jumped backward
        # TYPE apiserver_flowcontrol_epoch_advance_total counter
        apiserver_flowcontrol_epoch_advance_total{priority_level=%q,success=%q} %d%s`, uss.name, "true", uss.expectedEpochAdvances, "\n")
	}
	err := metrics.GatherAndCompare(e, "apiserver_flowcontrol_epoch_advance_total")
	if err != nil {
		uss.t.Error(err)
	} else {
		uss.t.Logf("Success with apiserver_flowcontrol_epoch_advance_total = %d", uss.expectedEpochAdvances)
	}
}

func TestMain(m *testing.M) {
	klog.InitFlags(nil)
	os.Exit(m.Run())
}

// TestNoRestraint tests whether the no-restraint factory gives every client what it asks for
// even though that is unfair.
// Expects fairness when there is no competition, unfairness when there is competition.
func TestNoRestraint(t *testing.T) {
	metrics.Register()
	testCases := []struct {
		concurrency int
		margin      float64
		fair        bool
		name        string
	}{
		{concurrency: 10, margin: 0.001, fair: true, name: "no-competition"},
		{concurrency: 2, margin: 0.25, fair: false, name: "with-competition"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Now()
			clk, counter := testeventclock.NewFake(now, 0, nil)
			nrc, err := test.NewNoRestraintFactory().BeginConstruction(fq.QueuingConfig{}, newGaugePair(clk), newExecSeatsGauge(clk), fq.NewNamedIntegrator(clk, "TestNoRestraint"))
			if err != nil {
				t.Fatal(err)
			}
			nr := nrc.Complete(fq.DispatchingConfig{})
			uniformScenario{name: "NoRestraint/" + testCase.name,
				qs: nr,
				clients: []uniformClient{
					newUniformClient(1001001001, 5, 15, time.Second, time.Second),
					newUniformClient(2002002002, 2, 15, time.Second, time.Second/2),
				},
				concurrencyLimit:       testCase.concurrency,
				evalDuration:           time.Second * 18,
				expectedFair:           []bool{testCase.fair},
				expectedFairnessMargin: []float64{testCase.margin},
				expectAllRequests:      true,
				clk:                    clk,
				counter:                counter,
				dontDump:               true,
			}.exercise(t)
		})
	}
}

func TestBaseline(t *testing.T) {
	metrics.Register()
	now := time.Now()

	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "TestBaseline",
		DesiredNumQueues: 9,
		QueueLengthLimit: 8,
		HandSize:         3,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, "seatDemandSubject")
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 1)

	uniformScenario{name: qCfg.Name,
		qs: qs,
		clients: []uniformClient{
			newUniformClient(1001001001, 1, 21, time.Second, 0),
		},
		concurrencyLimit:            1,
		evalDuration:                time.Second * 20,
		expectedFair:                []bool{true},
		expectedFairnessMargin:      []float64{0},
		expectAllRequests:           true,
		evalInqueueMetrics:          true,
		evalExecutingMetrics:        true,
		clk:                         clk,
		counter:                     counter,
		seatDemandIntegratorSubject: seatDemandIntegratorSubject,
	}.exercise(t)
}

func TestExampt(t *testing.T) {
	metrics.Register()
	for concurrencyLimit := 0; concurrencyLimit <= 2; concurrencyLimit += 2 {
		t.Run(fmt.Sprintf("concurrency=%d", concurrencyLimit), func(t *testing.T) {
			now := time.Now()
			clk, counter := testeventclock.NewFake(now, 0, nil)
			qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
			qCfg := fq.QueuingConfig{
				Name:             "TestBaseline",
				DesiredNumQueues: -1,
				QueueLengthLimit: 2,
				HandSize:         3,
			}
			seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, "seatDemandSubject")
			qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
			if err != nil {
				t.Fatal(err)
			}
			qs := qsComplete(qsc, concurrencyLimit)
			uniformScenario{name: qCfg.Name,
				qs: qs,
				clients: []uniformClient{
					newUniformClient(1001001001, 5, 20, time.Second, time.Second).setInitWidth(3),
				},
				concurrencyLimit:            1,
				evalDuration:                time.Second * 40,
				expectedFair:                []bool{true}, // "fair" is a bit odd-sounding here, but it "expectFair" here means expect `expectedAverages`
				expectedAverages:            []float64{7.5},
				expectedFairnessMargin:      []float64{0.00000001},
				expectAllRequests:           true,
				evalInqueueMetrics:          false,
				evalExecutingMetrics:        true,
				clk:                         clk,
				counter:                     counter,
				seatDemandIntegratorSubject: seatDemandIntegratorSubject,
			}.exercise(t)
		})
	}
}

func TestSeparations(t *testing.T) {
	flts := func(avgs ...float64) []float64 { return avgs }
	for _, seps := range []struct {
		think, pad                 time.Duration
		finalSeats, conc, nClients int
		exp                        []float64 // override expected results
	}{
		{think: time.Second, pad: 0, finalSeats: 1, conc: 1, nClients: 1},
		{think: time.Second, pad: 0, finalSeats: 1, conc: 2, nClients: 1},
		{think: time.Second, pad: 0, finalSeats: 2, conc: 2, nClients: 1},
		{think: time.Second, pad: 0, finalSeats: 1, conc: 1, nClients: 2},
		{think: time.Second, pad: 0, finalSeats: 1, conc: 2, nClients: 2},
		{think: time.Second, pad: 0, finalSeats: 2, conc: 2, nClients: 2},
		{think: 0, pad: time.Second, finalSeats: 1, conc: 1, nClients: 1, exp: flts(0.5)},
		{think: 0, pad: time.Second, finalSeats: 1, conc: 2, nClients: 1},
		{think: 0, pad: time.Second, finalSeats: 2, conc: 2, nClients: 1, exp: flts(0.5)},
		{think: 0, pad: time.Second, finalSeats: 1, conc: 1, nClients: 2, exp: flts(0.25, 0.25)},
		{think: 0, pad: time.Second, finalSeats: 1, conc: 2, nClients: 2, exp: flts(0.5, 0.5)},
		{think: 0, pad: time.Second, finalSeats: 2, conc: 2, nClients: 2, exp: flts(0.25, 0.25)},
		{think: time.Second, pad: time.Second / 2, finalSeats: 1, conc: 1, nClients: 1},
		{think: time.Second, pad: time.Second / 2, finalSeats: 1, conc: 2, nClients: 1},
		{think: time.Second, pad: time.Second / 2, finalSeats: 2, conc: 2, nClients: 1},
		{think: time.Second, pad: time.Second / 2, finalSeats: 1, conc: 1, nClients: 2, exp: flts(1.0/3, 1.0/3)},
		{think: time.Second, pad: time.Second / 2, finalSeats: 1, conc: 2, nClients: 2},
		{think: time.Second, pad: time.Second / 2, finalSeats: 2, conc: 2, nClients: 2, exp: flts(1.0/3, 1.0/3)},
		{think: time.Second / 2, pad: time.Second, finalSeats: 1, conc: 1, nClients: 1, exp: flts(0.5)},
		{think: time.Second / 2, pad: time.Second, finalSeats: 1, conc: 2, nClients: 1},
		{think: time.Second / 2, pad: time.Second, finalSeats: 2, conc: 2, nClients: 1, exp: flts(0.5)},
		{think: time.Second / 2, pad: time.Second, finalSeats: 1, conc: 1, nClients: 2, exp: flts(0.25, 0.25)},
		{think: time.Second / 2, pad: time.Second, finalSeats: 1, conc: 2, nClients: 2, exp: flts(0.5, 0.5)},
		{think: time.Second / 2, pad: time.Second, finalSeats: 2, conc: 2, nClients: 2, exp: flts(0.25, 0.25)},
	} {
		caseName := fmt.Sprintf("think=%v,finalSeats=%d,pad=%v,nClients=%d,conc=%d", seps.think, seps.finalSeats, seps.pad, seps.nClients, seps.conc)
		t.Run(caseName, func(t *testing.T) {
			metrics.Register()
			now := time.Now()

			clk, counter := testeventclock.NewFake(now, 0, nil)
			qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
			qCfg := fq.QueuingConfig{
				Name:             "TestSeparations/" + caseName,
				DesiredNumQueues: 9,
				QueueLengthLimit: 8,
				HandSize:         3,
			}
			seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, caseName+" seatDemandSubject")
			qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
			if err != nil {
				t.Fatal(err)
			}
			qs := qsComplete(qsc, seps.conc)
			uniformScenario{name: qCfg.Name,
				qs: qs,
				clients: []uniformClient{
					newUniformClient(1001001001, 1, 25, time.Second, seps.think).pad(seps.finalSeats, seps.pad),
					newUniformClient(2002002002, 1, 25, time.Second, seps.think).pad(seps.finalSeats, seps.pad),
				}[:seps.nClients],
				concurrencyLimit:            seps.conc,
				evalDuration:                time.Second * 24, // multiple of every period involved, so that margin can be 0 below
				expectedFair:                []bool{true},
				expectedFairnessMargin:      []float64{0},
				expectAllRequests:           true,
				evalInqueueMetrics:          true,
				evalExecutingMetrics:        true,
				clk:                         clk,
				counter:                     counter,
				expectedAverages:            seps.exp,
				seatDemandIntegratorSubject: seatDemandIntegratorSubject,
			}.exercise(t)
		})
	}
}

func TestUniformFlowsHandSize1(t *testing.T) {
	metrics.Register()
	now := time.Now()

	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "TestUniformFlowsHandSize1",
		DesiredNumQueues: 9,
		QueueLengthLimit: 8,
		HandSize:         1,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, "seatDemandSubject")
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 4)

	uniformScenario{name: qCfg.Name,
		qs: qs,
		clients: []uniformClient{
			newUniformClient(1001001001, 8, 20, time.Second, time.Second-1),
			newUniformClient(2002002002, 8, 20, time.Second, time.Second-1),
		},
		concurrencyLimit:            4,
		evalDuration:                time.Second * 50,
		expectedFair:                []bool{true},
		expectedFairnessMargin:      []float64{0.01},
		expectAllRequests:           true,
		evalInqueueMetrics:          true,
		evalExecutingMetrics:        true,
		clk:                         clk,
		counter:                     counter,
		seatDemandIntegratorSubject: seatDemandIntegratorSubject,
	}.exercise(t)
}

func TestUniformFlowsHandSize3(t *testing.T) {
	metrics.Register()
	now := time.Now()

	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "TestUniformFlowsHandSize3",
		DesiredNumQueues: 8,
		QueueLengthLimit: 16,
		HandSize:         3,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, qCfg.Name)
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 4)
	uniformScenario{name: qCfg.Name,
		qs: qs,
		clients: []uniformClient{
			newUniformClient(400900100100, 8, 30, time.Second, time.Second-1),
			newUniformClient(300900200200, 8, 30, time.Second, time.Second-1),
		},
		concurrencyLimit:            4,
		evalDuration:                time.Second * 60,
		expectedFair:                []bool{true},
		expectedFairnessMargin:      []float64{0.03},
		expectAllRequests:           true,
		evalInqueueMetrics:          true,
		evalExecutingMetrics:        true,
		clk:                         clk,
		counter:                     counter,
		seatDemandIntegratorSubject: seatDemandIntegratorSubject,
	}.exercise(t)
}

func TestDifferentFlowsExpectEqual(t *testing.T) {
	metrics.Register()
	now := time.Now()

	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "DiffFlowsExpectEqual",
		DesiredNumQueues: 9,
		QueueLengthLimit: 8,
		HandSize:         1,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, qCfg.Name)
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 4)

	uniformScenario{name: qCfg.Name,
		qs: qs,
		clients: []uniformClient{
			newUniformClient(1001001001, 8, 20, time.Second, time.Second),
			newUniformClient(2002002002, 7, 30, time.Second, time.Second/2),
		},
		concurrencyLimit:            4,
		evalDuration:                time.Second * 40,
		expectedFair:                []bool{true},
		expectedFairnessMargin:      []float64{0.01},
		expectAllRequests:           true,
		evalInqueueMetrics:          true,
		evalExecutingMetrics:        true,
		clk:                         clk,
		counter:                     counter,
		seatDemandIntegratorSubject: seatDemandIntegratorSubject,
	}.exercise(t)
}

// TestSeatSecondsRollover checks that there is not a problem with SeatSeconds overflow.
func TestSeatSecondsRollover(t *testing.T) {
	metrics.Register()
	now := time.Now()

	const Quarter = 91 * 24 * time.Hour

	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "TestSeatSecondsRollover",
		DesiredNumQueues: 9,
		QueueLengthLimit: 8,
		HandSize:         1,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, qCfg.Name)
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 2000)

	uniformScenario{name: qCfg.Name,
		qs: qs,
		clients: []uniformClient{
			newUniformClient(1001001001, 8, 20, Quarter, Quarter).setInitWidth(500),
			newUniformClient(2002002002, 7, 30, Quarter, Quarter/2).setInitWidth(500),
		},
		concurrencyLimit:            2000,
		evalDuration:                Quarter * 40,
		expectedFair:                []bool{true},
		expectedFairnessMargin:      []float64{0.01},
		expectAllRequests:           true,
		evalInqueueMetrics:          true,
		evalExecutingMetrics:        true,
		clk:                         clk,
		counter:                     counter,
		expectedEpochAdvances:       8,
		seatDemandIntegratorSubject: seatDemandIntegratorSubject,
	}.exercise(t)
}

func TestDifferentFlowsExpectUnequal(t *testing.T) {
	metrics.Register()
	now := time.Now()

	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "DiffFlowsExpectUnequal",
		DesiredNumQueues: 9,
		QueueLengthLimit: 6,
		HandSize:         1,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, qCfg.Name)
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 3)

	uniformScenario{name: qCfg.Name,
		qs: qs,
		clients: []uniformClient{
			newUniformClient(1001001001, 4, 20, time.Second, time.Second-1),
			newUniformClient(2002002002, 2, 20, time.Second, time.Second-1),
		},
		concurrencyLimit:            3,
		evalDuration:                time.Second * 20,
		expectedFair:                []bool{true},
		expectedFairnessMargin:      []float64{0.01},
		expectAllRequests:           true,
		evalInqueueMetrics:          true,
		evalExecutingMetrics:        true,
		clk:                         clk,
		counter:                     counter,
		seatDemandIntegratorSubject: seatDemandIntegratorSubject,
	}.exercise(t)
}

func TestDifferentWidths(t *testing.T) {
	metrics.Register()
	now := time.Now()

	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "TestDifferentWidths",
		DesiredNumQueues: 64,
		QueueLengthLimit: 13,
		HandSize:         7,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, qCfg.Name)
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 6)
	uniformScenario{name: qCfg.Name,
		qs: qs,
		clients: []uniformClient{
			newUniformClient(10010010010010, 13, 10, time.Second, time.Second-1),
			newUniformClient(20020020020020, 7, 10, time.Second, time.Second-1).setInitWidth(2),
		},
		concurrencyLimit:            6,
		evalDuration:                time.Second * 20,
		expectedFair:                []bool{true},
		expectedFairnessMargin:      []float64{0.155},
		expectAllRequests:           true,
		evalInqueueMetrics:          true,
		evalExecutingMetrics:        true,
		clk:                         clk,
		counter:                     counter,
		seatDemandIntegratorSubject: seatDemandIntegratorSubject,
	}.exercise(t)
}

func TestTooWide(t *testing.T) {
	metrics.Register()
	now := time.Now()

	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "TestTooWide",
		DesiredNumQueues: 64,
		QueueLengthLimit: 35,
		HandSize:         7,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, qCfg.Name)
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 6)
	uniformScenario{name: qCfg.Name,
		qs: qs,
		clients: []uniformClient{
			newUniformClient(40040040040040, 15, 21, time.Second, time.Second-1).setInitWidth(2),
			newUniformClient(50050050050050, 15, 21, time.Second, time.Second-1).setInitWidth(2),
			newUniformClient(60060060060060, 15, 21, time.Second, time.Second-1).setInitWidth(2),
			newUniformClient(70070070070070, 15, 21, time.Second, time.Second-1).setInitWidth(2),
			newUniformClient(90090090090090, 15, 21, time.Second, time.Second-1).setInitWidth(7),
		},
		concurrencyLimit:            6,
		evalDuration:                time.Second * 225,
		expectedFair:                []bool{true},
		expectedFairnessMargin:      []float64{0.33},
		expectAllRequests:           true,
		evalInqueueMetrics:          true,
		evalExecutingMetrics:        true,
		clk:                         clk,
		counter:                     counter,
		seatDemandIntegratorSubject: seatDemandIntegratorSubject,
	}.exercise(t)
}

// TestWindup exercises a scenario with the windup problem.
// That is, a flow that can not use all the seats that it is allocated
// for a while.  During that time, the queues that serve that flow
// advance their `virtualStart` (that is, R(next dispatch in virtual world))
// more slowly than the other queues (which are using more seats than they
// are allocated).  The implementation has a hack that addresses part of
// this imbalance but not all of it.  In this test, flow 1 can not use all
// of its allocation during the first half, and *can* (and does) use all of
// its allocation and more during the second half.
// Thus we expect the fair (not equal) result
// in the first half and an unfair result in the second half.
// This func has two test cases, bounding the amount of unfairness
// in the second half.
func TestWindup(t *testing.T) {
	metrics.Register()
	testCases := []struct {
		margin2     float64
		expectFair2 bool
		name        string
	}{
		{margin2: 0.26, expectFair2: true, name: "upper-bound"},
		{margin2: 0.1, expectFair2: false, name: "lower-bound"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Now()
			clk, counter := testeventclock.NewFake(now, 0, nil)
			qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
			qCfg := fq.QueuingConfig{
				Name:             "TestWindup/" + testCase.name,
				DesiredNumQueues: 9,
				QueueLengthLimit: 6,
				HandSize:         1,
			}
			seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, qCfg.Name)
			qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
			if err != nil {
				t.Fatal(err)
			}
			qs := qsComplete(qsc, 3)

			uniformScenario{name: qCfg.Name, qs: qs,
				clients: []uniformClient{
					newUniformClient(1001001001, 2, 40, time.Second, -1),
					newUniformClient(2002002002, 2, 40, time.Second, -1).setSplit(),
				},
				concurrencyLimit:            3,
				evalDuration:                time.Second * 40,
				expectedFair:                []bool{true, testCase.expectFair2},
				expectedFairnessMargin:      []float64{0.01, testCase.margin2},
				expectAllRequests:           true,
				evalInqueueMetrics:          true,
				evalExecutingMetrics:        true,
				clk:                         clk,
				counter:                     counter,
				seatDemandIntegratorSubject: seatDemandIntegratorSubject,
			}.exercise(t)
		})
	}
}

func TestDifferentFlowsWithoutQueuing(t *testing.T) {
	metrics.Register()
	now := time.Now()

	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "TestDifferentFlowsWithoutQueuing",
		DesiredNumQueues: 0,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, "seatDemandSubject")
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 4)

	uniformScenario{name: qCfg.Name,
		qs: qs,
		clients: []uniformClient{
			newUniformClient(1001001001, 6, 10, time.Second, 57*time.Millisecond),
			newUniformClient(2002002002, 4, 15, time.Second, 750*time.Millisecond),
		},
		concurrencyLimit:            4,
		evalDuration:                time.Second * 13,
		expectedFair:                []bool{false},
		expectedFairnessMargin:      []float64{0.20},
		evalExecutingMetrics:        true,
		rejectReason:                "concurrency-limit",
		clk:                         clk,
		counter:                     counter,
		seatDemandIntegratorSubject: seatDemandIntegratorSubject,
	}.exercise(t)
}

// TestContextCancel tests cancellation of a request's context.
// The outline is:
//  1. Use a concurrency limit of 1.
//  2. Start request 1.
//  3. Use a fake clock for the following logic, to insulate from scheduler noise.
//  4. The exec fn of request 1 starts request 2, which should wait
//     in its queue.
//  5. The exec fn of request 1 also forks a goroutine that waits 1 second
//     and then cancels the context of request 2.
//  6. The exec fn of request 1, if StartRequest 2 returns a req2 (which is the normal case),
//     calls `req2.Finish`, which is expected to return after the context cancel.
//  7. The queueset interface allows StartRequest 2 to return `nil` in this situation,
//     if the scheduler gets the cancel done before StartRequest finishes;
//     the test handles this without regard to whether the implementation will ever do that.
//  8. Check that the above took exactly 1 second.
func TestContextCancel(t *testing.T) {
	metrics.Register()
	metrics.Reset()
	now := time.Now()
	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "TestContextCancel",
		DesiredNumQueues: 11,
		QueueLengthLimit: 11,
		HandSize:         1,
	}
	seatDemandIntegratorSubject := fq.NewNamedIntegrator(clk, qCfg.Name)
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), seatDemandIntegratorSubject)
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 1)
	counter.Add(1) // account for main activity of the goroutine running this test
	ctx1 := context.Background()
	pZero := func() *int32 { var zero int32; return &zero }
	// counts of calls to the QueueNoteFns
	queueNoteCounts := map[int]map[bool]*int32{
		1: {false: pZero(), true: pZero()},
		2: {false: pZero(), true: pZero()},
	}
	queueNoteFn := func(fn int) func(inQueue bool) {
		return func(inQueue bool) { atomic.AddInt32(queueNoteCounts[fn][inQueue], 1) }
	}
	fatalErrs := []string{}
	var errsLock sync.Mutex
	expectQNCount := func(fn int, inQueue bool, expect int32) {
		if a := atomic.LoadInt32(queueNoteCounts[fn][inQueue]); a != expect {
			errsLock.Lock()
			defer errsLock.Unlock()
			fatalErrs = append(fatalErrs, fmt.Sprintf("Got %d calls to queueNoteFn%d(%v), expected %d", a, fn, inQueue, expect))
		}
	}
	expectQNCounts := func(fn int, expectF, expectT int32) {
		expectQNCount(fn, false, expectF)
		expectQNCount(fn, true, expectT)
	}
	req1, _ := qs.StartRequest(ctx1, &fcrequest.WorkEstimate{InitialSeats: 1}, 1, "", "fs1", "test", "one", queueNoteFn(1))
	if req1 == nil {
		t.Error("Request rejected")
		return
	}
	expectQNCounts(1, 1, 1)
	var executed1, idle1 bool
	counter.Add(1) // account for the following goroutine
	go func() {
		defer counter.Add(-1) // account completion of this goroutine
		idle1 = req1.Finish(func() {
			executed1 = true
			ctx2, cancel2 := context.WithCancel(context.Background())
			tBefore := clk.Now()
			counter.Add(1) // account for the following goroutine
			go func() {
				defer counter.Add(-1) // account completion of this goroutine
				clk.Sleep(time.Second)
				expectQNCounts(2, 0, 1)
				// account for unblocking the goroutine that waits on cancellation
				counter.Add(1)
				cancel2()
			}()
			req2, idle2a := qs.StartRequest(ctx2, &fcrequest.WorkEstimate{InitialSeats: 1}, 2, "", "fs2", "test", "two", queueNoteFn(2))
			if idle2a {
				t.Error("2nd StartRequest returned idle")
			}
			if req2 != nil {
				idle2b := req2.Finish(func() {
					t.Error("Executing req2")
				})
				if idle2b {
					t.Error("2nd Finish returned idle")
				}
				expectQNCounts(2, 1, 1)
			}
			tAfter := clk.Now()
			dt := tAfter.Sub(tBefore)
			if dt != time.Second {
				t.Errorf("Unexpected: dt=%d", dt)
			}
		})
	}()
	counter.Add(-1) // completion of main activity of goroutine running this test
	clk.Run(nil)
	errsLock.Lock()
	defer errsLock.Unlock()
	if len(fatalErrs) > 0 {
		t.Error(strings.Join(fatalErrs, "; "))
	}
	if !executed1 {
		t.Errorf("Unexpected: executed1=%v", executed1)
	}
	if !idle1 {
		t.Error("Not idle at the end")
	}
}

func countingPromiseFactoryFactory(activeCounter counter.GoRoutineCounter) promiseFactoryFactory {
	return func(qs *queueSet) promiseFactory {
		return func(initial interface{}, doneCtx context.Context, doneVal interface{}) promise.WriteOnce {
			return testpromise.NewCountingWriteOnce(activeCounter, &qs.lock, initial, doneCtx.Done(), doneVal)
		}
	}
}

func TestTotalRequestsExecutingWithPanic(t *testing.T) {
	metrics.Register()
	metrics.Reset()
	now := time.Now()
	clk, counter := testeventclock.NewFake(now, 0, nil)
	qsf := newTestableQueueSetFactory(clk, countingPromiseFactoryFactory(counter))
	qCfg := fq.QueuingConfig{
		Name:             "TestTotalRequestsExecutingWithPanic",
		DesiredNumQueues: 0,
	}
	qsc, err := qsf.BeginConstruction(qCfg, newGaugePair(clk), newExecSeatsGauge(clk), fq.NewNamedIntegrator(clk, qCfg.Name))
	if err != nil {
		t.Fatal(err)
	}
	qs := qsComplete(qsc, 1)
	counter.Add(1) // account for the goroutine running this test

	queue, ok := qs.(*queueSet)
	if !ok {
		t.Fatalf("expected a QueueSet of type: %T but got: %T", &queueSet{}, qs)
	}
	if queue.totRequestsExecuting != 0 {
		t.Fatalf("precondition: expected total requests currently executing of the QueueSet to be 0, but got: %d", queue.totRequestsExecuting)
	}
	if queue.dCfg.ConcurrencyLimit != 1 {
		t.Fatalf("precondition: expected concurrency limit of the QueueSet to be 1, but got: %d", queue.dCfg.ConcurrencyLimit)
	}

	ctx := context.Background()
	req, _ := qs.StartRequest(ctx, &fcrequest.WorkEstimate{InitialSeats: 1}, 1, "", "fs", "test", "one", func(inQueue bool) {})
	if req == nil {
		t.Fatal("expected a Request object from StartRequest, but got nil")
	}

	panicErrExpected := errors.New("apiserver panic'd")
	var panicErrGot interface{}
	func() {
		defer func() {
			panicErrGot = recover()
		}()

		req.Finish(func() {
			// verify that total requests executing goes up by 1 since the request is executing.
			if queue.totRequestsExecuting != 1 {
				t.Fatalf("expected total requests currently executing of the QueueSet to be 1, but got: %d", queue.totRequestsExecuting)
			}

			panic(panicErrExpected)
		})
	}()

	// verify that the panic was from us (above)
	if panicErrExpected != panicErrGot {
		t.Errorf("expected panic error: %#v, but got: %#v", panicErrExpected, panicErrGot)
	}
	if queue.totRequestsExecuting != 0 {
		t.Errorf("expected total requests currently executing of the QueueSet to be 0, but got: %d", queue.totRequestsExecuting)
	}
}

func TestFindDispatchQueueLocked(t *testing.T) {
	const G = 3 * time.Millisecond
	qs0 := &queueSet{estimatedServiceDuration: G}
	tests := []struct {
		name                    string
		robinIndex              int
		concurrencyLimit        int
		totSeatsInUse           int
		queues                  []*queue
		attempts                int
		beforeSelectQueueLocked func(attempt int, qs *queueSet)
		minQueueIndexExpected   []int
		robinIndexExpected      []int
	}{
		{
			name:             "width1=1, seats are available, the queue with less virtual start time wins",
			concurrencyLimit: 1,
			totSeatsInUse:    0,
			robinIndex:       -1,
			queues: []*queue{
				{
					nextDispatchR: fcrequest.SeatsTimesDuration(1, 200*time.Second),
					requestsWaiting: newFIFO(
						&request{workEstimate: qs0.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 1})},
					),
					requestsExecuting: sets.New[*request](),
				},
				{
					nextDispatchR: fcrequest.SeatsTimesDuration(1, 100*time.Second),
					requestsWaiting: newFIFO(
						&request{workEstimate: qs0.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 1})},
					),
				},
			},
			attempts:              1,
			minQueueIndexExpected: []int{1},
			robinIndexExpected:    []int{1},
		},
		{
			name:             "width1=1, all seats are occupied, no queue is picked",
			concurrencyLimit: 1,
			totSeatsInUse:    1,
			robinIndex:       -1,
			queues: []*queue{
				{
					nextDispatchR: fcrequest.SeatsTimesDuration(1, 200*time.Second),
					requestsWaiting: newFIFO(
						&request{workEstimate: qs0.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 1})},
					),
					requestsExecuting: sets.New[*request](),
				},
			},
			attempts:              1,
			minQueueIndexExpected: []int{-1},
			robinIndexExpected:    []int{0},
		},
		{
			name:             "width1 > 1, seats are available for request with the least finish R, queue is picked",
			concurrencyLimit: 50,
			totSeatsInUse:    25,
			robinIndex:       -1,
			queues: []*queue{
				{
					nextDispatchR: fcrequest.SeatsTimesDuration(1, 200*time.Second),
					requestsWaiting: newFIFO(
						&request{workEstimate: qs0.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 50})},
					),
					requestsExecuting: sets.New[*request](),
				},
				{
					nextDispatchR: fcrequest.SeatsTimesDuration(1, 100*time.Second),
					requestsWaiting: newFIFO(
						&request{workEstimate: qs0.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 25})},
					),
					requestsExecuting: sets.New[*request](),
				},
			},
			attempts:              1,
			minQueueIndexExpected: []int{1},
			robinIndexExpected:    []int{1},
		},
		{
			name:             "width1 > 1, seats are not available for request with the least finish R, queue is not picked",
			concurrencyLimit: 50,
			totSeatsInUse:    26,
			robinIndex:       -1,
			queues: []*queue{
				{
					nextDispatchR: fcrequest.SeatsTimesDuration(1, 200*time.Second),
					requestsWaiting: newFIFO(
						&request{workEstimate: qs0.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 10})},
					),
					requestsExecuting: sets.New[*request](),
				},
				{
					nextDispatchR: fcrequest.SeatsTimesDuration(1, 100*time.Second),
					requestsWaiting: newFIFO(
						&request{workEstimate: qs0.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 25})},
					),
					requestsExecuting: sets.New[*request](),
				},
			},
			attempts:              3,
			minQueueIndexExpected: []int{-1, -1, -1},
			robinIndexExpected:    []int{1, 1, 1},
		},
		{
			name:             "width1 > 1, seats become available before 3rd attempt, queue is picked",
			concurrencyLimit: 50,
			totSeatsInUse:    26,
			robinIndex:       -1,
			queues: []*queue{
				{
					nextDispatchR: fcrequest.SeatsTimesDuration(1, 200*time.Second),
					requestsWaiting: newFIFO(
						&request{workEstimate: qs0.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 10})},
					),
					requestsExecuting: sets.New[*request](),
				},
				{
					nextDispatchR: fcrequest.SeatsTimesDuration(1, 100*time.Second),
					requestsWaiting: newFIFO(
						&request{workEstimate: qs0.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 25})},
					),
					requestsExecuting: sets.New[*request](),
				},
			},
			beforeSelectQueueLocked: func(attempt int, qs *queueSet) {
				if attempt == 3 {
					qs.totSeatsInUse = 25
				}
			},
			attempts:              3,
			minQueueIndexExpected: []int{-1, -1, 1},
			robinIndexExpected:    []int{1, 1, 1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			qs := &queueSet{
				estimatedServiceDuration: G,
				seatDemandIntegrator:     fq.NewNamedIntegrator(clock.RealClock{}, "seatDemandSubject"),
				robinIndex:               test.robinIndex,
				totSeatsInUse:            test.totSeatsInUse,
				qCfg:                     fq.QueuingConfig{Name: "TestSelectQueueLocked/" + test.name},
				dCfg: fq.DispatchingConfig{
					ConcurrencyLimit: test.concurrencyLimit,
				},
				queues: test.queues,
			}

			t.Logf("QS: robin index=%d, seats in use=%d limit=%d", qs.robinIndex, qs.totSeatsInUse, qs.dCfg.ConcurrencyLimit)

			for i := 0; i < test.attempts; i++ {
				attempt := i + 1
				if test.beforeSelectQueueLocked != nil {
					test.beforeSelectQueueLocked(attempt, qs)
				}

				var minQueueExpected *queue
				if queueIdx := test.minQueueIndexExpected[i]; queueIdx >= 0 {
					minQueueExpected = test.queues[queueIdx]
				}

				minQueueGot, reqGot := qs.findDispatchQueueToBoundLocked()
				if minQueueExpected != minQueueGot {
					t.Errorf("Expected queue: %#v, but got: %#v", minQueueExpected, minQueueGot)
				}

				robinIndexExpected := test.robinIndexExpected[i]
				if robinIndexExpected != qs.robinIndex {
					t.Errorf("Expected robin index: %d for attempt: %d, but got: %d", robinIndexExpected, attempt, qs.robinIndex)
				}

				if (reqGot == nil) != (minQueueGot == nil) {
					t.Errorf("reqGot=%p but minQueueGot=%p", reqGot, minQueueGot)
				}
			}
		})
	}
}

func TestFinishRequestLocked(t *testing.T) {
	tests := []struct {
		name         string
		workEstimate fcrequest.WorkEstimate
	}{
		{
			name: "request has additional latency",
			workEstimate: fcrequest.WorkEstimate{
				InitialSeats:      1,
				FinalSeats:        10,
				AdditionalLatency: time.Minute,
			},
		},
		{
			name: "request has no additional latency",
			workEstimate: fcrequest.WorkEstimate{
				InitialSeats: 10,
			},
		},
	}

	metrics.Register()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metrics.Reset()

			now := time.Now()
			clk, _ := testeventclock.NewFake(now, 0, nil)
			qs := &queueSet{
				clock:                    clk,
				estimatedServiceDuration: time.Second,
				reqsGaugePair:            newGaugePair(clk),
				execSeatsGauge:           newExecSeatsGauge(clk),
				seatDemandIntegrator:     fq.NewNamedIntegrator(clk, "seatDemandSubject"),
			}
			queue := &queue{
				requestsWaiting:   newRequestFIFO(),
				requestsExecuting: sets.New[*request](),
			}
			r := &request{
				qs:           qs,
				queue:        queue,
				workEstimate: qs.completeWorkEstimate(&test.workEstimate),
			}
			rOther := &request{qs: qs, queue: queue}

			qs.totRequestsExecuting = 111
			qs.totSeatsInUse = 222
			queue.requestsExecuting = sets.New(r, rOther)
			queue.seatsInUse = 22

			var (
				queuesetTotalRequestsExecutingExpected = qs.totRequestsExecuting - 1
				queuesetTotalSeatsInUseExpected        = qs.totSeatsInUse - test.workEstimate.MaxSeats()
				queueRequestsExecutingExpected         = sets.New(rOther)
				queueSeatsInUseExpected                = queue.seatsInUse - test.workEstimate.MaxSeats()
			)

			qs.finishRequestLocked(r)

			// as soon as AdditionalLatency elapses we expect the seats to be released
			clk.SetTime(now.Add(test.workEstimate.AdditionalLatency))

			if queuesetTotalRequestsExecutingExpected != qs.totRequestsExecuting {
				t.Errorf("Expected total requests executing: %d, but got: %d", queuesetTotalRequestsExecutingExpected, qs.totRequestsExecuting)
			}
			if queuesetTotalSeatsInUseExpected != qs.totSeatsInUse {
				t.Errorf("Expected total seats in use: %d, but got: %d", queuesetTotalSeatsInUseExpected, qs.totSeatsInUse)
			}
			if !queueRequestsExecutingExpected.Equal(queue.requestsExecuting) {
				t.Errorf("Expected requests executing for queue: %v, but got: %v", queueRequestsExecutingExpected, queue.requestsExecuting)
			}
			if queueSeatsInUseExpected != queue.seatsInUse {
				t.Errorf("Expected seats in use for queue: %d, but got: %d", queueSeatsInUseExpected, queue.seatsInUse)
			}
		})
	}
}

func TestRequestSeats(t *testing.T) {
	qs := &queueSet{estimatedServiceDuration: time.Second}
	tests := []struct {
		name     string
		request  *request
		expected int
	}{
		{
			name:     "",
			request:  &request{workEstimate: qs.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 3, FinalSeats: 3})},
			expected: 3,
		},
		{
			name:     "",
			request:  &request{workEstimate: qs.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 1, FinalSeats: 3})},
			expected: 3,
		},
		{
			name:     "",
			request:  &request{workEstimate: qs.completeWorkEstimate(&fcrequest.WorkEstimate{InitialSeats: 3, FinalSeats: 1})},
			expected: 3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			seatsGot := test.request.MaxSeats()
			if test.expected != seatsGot {
				t.Errorf("Expected seats: %d, got %d", test.expected, seatsGot)
			}
		})
	}
}

func TestRequestWork(t *testing.T) {
	qs := &queueSet{estimatedServiceDuration: 2 * time.Second}
	request := &request{
		workEstimate: qs.completeWorkEstimate(&fcrequest.WorkEstimate{
			InitialSeats:      3,
			FinalSeats:        50,
			AdditionalLatency: 70 * time.Second,
		}),
	}

	got := request.totalWork()
	want := fcrequest.SeatsTimesDuration(3, 2*time.Second) + fcrequest.SeatsTimesDuration(50, 70*time.Second)
	if want != got {
		t.Errorf("Expected totalWork: %v, but got: %v", want, got)
	}
}

func newFIFO(requests ...*request) fifo {
	l := newRequestFIFO()
	for i := range requests {
		requests[i].removeFromQueueLocked = l.Enqueue(requests[i])
	}
	return l
}

func newGaugePair(clk clock.PassiveClock) metrics.RatioedGaugePair {
	return metrics.RatioedGaugeVecPhasedElementPair(metrics.PriorityLevelConcurrencyGaugeVec, 1, 1, []string{"test"})
}

func newExecSeatsGauge(clk clock.PassiveClock) metrics.RatioedGauge {
	return metrics.PriorityLevelExecutionSeatsGaugeVec.NewForLabelValuesSafe(0, 1, []string{"test"})
}

func float64close(x, y float64) bool {
	x0 := float64NaNTo0(x)
	y0 := float64NaNTo0(y)
	diff := math.Abs(x0 - y0)
	den := math.Max(math.Abs(x0), math.Abs(y0))
	return den == 0 || diff/den < 1e-10
}

func uint64max(a, b uint64) uint64 {
	if b > a {
		return b
	}
	return a
}

func float64NaNTo0(x float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	return x
}

func qsComplete(qsc fq.QueueSetCompleter, concurrencyLimit int) fq.QueueSet {
	concurrencyDenominator := concurrencyLimit
	if concurrencyDenominator <= 0 {
		concurrencyDenominator = 1
	}
	return qsc.Complete(fq.DispatchingConfig{ConcurrencyLimit: concurrencyLimit, ConcurrencyDenominator: concurrencyDenominator})
}
