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

package flowcontrol

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	flowcontrol "k8s.io/api/flowcontrol/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	fcboot "k8s.io/apiserver/pkg/apis/flowcontrol/bootstrap"
	"k8s.io/apiserver/pkg/util/flowcontrol/debug"
	fq "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing"
	fcfmt "k8s.io/apiserver/pkg/util/flowcontrol/format"
	"k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	fcrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	fcclient "k8s.io/client-go/kubernetes/typed/flowcontrol/v1"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
)

// Some tests print a lot of debug logs which slows down tests considerably,
// causing them to even timeout.
var testDebugLogs = false

func TestMain(m *testing.M) {
	klog.InitFlags(nil)
	os.Exit(m.Run())
}

var mandPLs = func() map[string]*flowcontrol.PriorityLevelConfiguration {
	ans := make(map[string]*flowcontrol.PriorityLevelConfiguration)
	for _, mand := range fcboot.MandatoryPriorityLevelConfigurations {
		ans[mand.Name] = mand
	}
	return ans
}()

// in general usage, the boolean returned may be inaccurate by the time the caller examines it.
func (cfgCtlr *configController) hasPriorityLevelState(plName string) bool {
	cfgCtlr.lock.Lock()
	defer cfgCtlr.lock.Unlock()
	return cfgCtlr.priorityLevelStates[plName] != nil
}

type ctlrTestState struct {
	t               *testing.T
	cfgCtlr         *configController
	fcIfc           fcclient.FlowcontrolV1Interface
	existingPLs     map[string]*flowcontrol.PriorityLevelConfiguration
	existingFSs     map[string]*flowcontrol.FlowSchema
	heldRequestsMap map[string][]heldRequest
	requestWG       sync.WaitGroup
	lock            sync.Mutex
	queues          map[string]*ctlrTestQueueSet
}

type heldRequest struct {
	rd       RequestDigest
	finishCh chan struct{}
}

var _ fq.QueueSetFactory = (*ctlrTestState)(nil)

type ctlrTestQueueSetCompleter struct {
	cts *ctlrTestState
	cqs *ctlrTestQueueSet
	qc  fq.QueuingConfig
}

type ctlrTestQueueSet struct {
	cts         *ctlrTestState
	qc          fq.QueuingConfig
	dc          fq.DispatchingConfig
	countActive int
}

type ctlrTestRequest struct {
	cqs            *ctlrTestQueueSet
	qsName         string
	descr1, descr2 interface{}
}

func (cts *ctlrTestState) BeginConstruction(qc fq.QueuingConfig, rip metrics.RatioedGaugePair, eso metrics.RatioedGauge, sdi metrics.Gauge) (fq.QueueSetCompleter, error) {
	return ctlrTestQueueSetCompleter{cts, nil, qc}, nil
}

func (cqs *ctlrTestQueueSet) BeginConfigChange(qc fq.QueuingConfig) (fq.QueueSetCompleter, error) {
	return ctlrTestQueueSetCompleter{cqs.cts, cqs, qc}, nil
}

func (cqs *ctlrTestQueueSet) Dump(bool) debug.QueueSetDump {
	return debug.QueueSetDump{}
}

func (cqc ctlrTestQueueSetCompleter) Complete(dc fq.DispatchingConfig) fq.QueueSet {
	cqc.cts.lock.Lock()
	defer cqc.cts.lock.Unlock()
	qs := cqc.cqs
	if qs == nil {
		qs = &ctlrTestQueueSet{cts: cqc.cts, qc: cqc.qc, dc: dc}
		cqc.cts.queues[cqc.qc.Name] = qs
	} else {
		qs.qc, qs.dc = cqc.qc, dc
	}
	return qs
}

func (cqs *ctlrTestQueueSet) IsIdle() bool {
	cqs.cts.lock.Lock()
	defer cqs.cts.lock.Unlock()
	klog.V(7).Infof("For %p QS %s, countActive==%d", cqs, cqs.qc.Name, cqs.countActive)
	return cqs.countActive == 0
}

func (cqs *ctlrTestQueueSet) StartRequest(ctx context.Context, width *fcrequest.WorkEstimate, hashValue uint64, flowDistinguisher, fsName string, descr1, descr2 interface{}, queueNoteFn fq.QueueNoteFn) (req fq.Request, idle bool) {
	cqs.cts.lock.Lock()
	defer cqs.cts.lock.Unlock()
	cqs.countActive++
	if testDebugLogs {
		cqs.cts.t.Logf("Queued %q %#+v %#+v for %p QS=%s, countActive:=%d", fsName, descr1, descr2, cqs, cqs.qc.Name, cqs.countActive)
	}
	return &ctlrTestRequest{cqs, cqs.qc.Name, descr1, descr2}, false
}

func (ctr *ctlrTestRequest) Finish(execute func()) bool {
	execute()
	ctr.cqs.cts.lock.Lock()
	defer ctr.cqs.cts.lock.Unlock()
	ctr.cqs.countActive--
	if testDebugLogs {
		ctr.cqs.cts.t.Logf("Finished %#+v %#+v for %p QS=%s, countActive:=%d", ctr.descr1, ctr.descr2, ctr.cqs, ctr.cqs.qc.Name, ctr.cqs.countActive)
	}
	return ctr.cqs.countActive == 0
}

func (cts *ctlrTestState) getQueueSetNames() sets.String {
	cts.lock.Lock()
	defer cts.lock.Unlock()
	return sets.StringKeySet(cts.queues)
}

func (cts *ctlrTestState) getNonIdleQueueSetNames() sets.String {
	cts.lock.Lock()
	defer cts.lock.Unlock()
	ans := sets.NewString()
	for name, qs := range cts.queues {
		if qs.countActive > 0 {
			ans.Insert(name)
		}
	}
	return ans
}

func (cts *ctlrTestState) hasNonIdleQueueSet(name string) bool {
	cts.lock.Lock()
	defer cts.lock.Unlock()
	qs := cts.queues[name]
	return qs != nil && qs.countActive > 0
}

func (cts *ctlrTestState) addHeldRequest(plName string, rd RequestDigest, finishCh chan struct{}) {
	cts.lock.Lock()
	defer cts.lock.Unlock()
	hrs := cts.heldRequestsMap[plName]
	hrs = append(hrs, heldRequest{rd, finishCh})
	cts.heldRequestsMap[plName] = hrs
	if testDebugLogs {
		cts.t.Logf("Holding %#+v for %s, count:=%d", rd, plName, len(hrs))
	}
}

func (cts *ctlrTestState) popHeldRequest() (plName string, hr *heldRequest, nCount int) {
	cts.lock.Lock()
	defer cts.lock.Unlock()
	var hrs []heldRequest
	for {
		for plName, hrs = range cts.heldRequestsMap {
			goto GotOne
		}
		return "", nil, 0
	GotOne:
		if nhr := len(hrs); nhr > 0 {
			hrv := hrs[nhr-1]
			hrs = hrs[:nhr-1]
			hr = &hrv
		}
		if len(hrs) == 0 {
			delete(cts.heldRequestsMap, plName)
		} else {
			cts.heldRequestsMap[plName] = hrs
		}
		if hr != nil {
			nCount = len(hrs)
			return
		}
	}
}

var mandQueueSetNames = func() sets.String {
	mandQueueSetNames := sets.NewString()
	for _, mpl := range fcboot.MandatoryPriorityLevelConfigurations {
		mandQueueSetNames.Insert(mpl.Name)
	}
	return mandQueueSetNames
}()

func TestConfigConsumer(t *testing.T) {
	rngOuter := rand.New(rand.NewSource(1234567890123456789))
	for i := 1; i <= 10; i++ {
		rng := rand.New(rand.NewSource(int64(rngOuter.Uint64())))
		t.Run(fmt.Sprintf("trial%d:", i), func(t *testing.T) {
			clientset := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(clientset, 0)
			flowcontrolClient := clientset.FlowcontrolV1()
			cts := &ctlrTestState{t: t,
				fcIfc:           flowcontrolClient,
				existingFSs:     map[string]*flowcontrol.FlowSchema{},
				existingPLs:     map[string]*flowcontrol.PriorityLevelConfiguration{},
				heldRequestsMap: map[string][]heldRequest{},
				queues:          map[string]*ctlrTestQueueSet{},
			}
			ctlr := newTestableController(TestableConfig{
				Name:                   "Controller",
				Clock:                  clock.RealClock{},
				AsFieldManager:         ConfigConsumerAsFieldManager,
				FoundToDangling:        func(found bool) bool { return !found },
				InformerFactory:        informerFactory,
				FlowcontrolClient:      flowcontrolClient,
				ServerConcurrencyLimit: 100, // server concurrency limit
				ReqsGaugeVec:           metrics.PriorityLevelConcurrencyGaugeVec,
				ExecSeatsGaugeVec:      metrics.PriorityLevelExecutionSeatsGaugeVec,
				QueueSetFactory:        cts,
			})
			cts.cfgCtlr = ctlr
			persistingPLNames := sets.NewString()
			trialStep := fmt.Sprintf("trial%d-0", i)
			_, _, desiredPLNames, newBadPLNames := genPLs(rng, trialStep, persistingPLNames, 0)
			_, _, newFTRs, newCatchAlls := genFSs(t, rng, trialStep, desiredPLNames, newBadPLNames, 0)
			for j := 0; ; {
				if testDebugLogs {
					t.Logf("For %s, desiredPLNames=%#+v", trialStep, desiredPLNames)
					t.Logf("For %s, newFTRs=%#+v", trialStep, newFTRs)
				}
				// Check that the latest digestion did the right thing
				nextPLNames := sets.NewString()
				for oldPLName := range persistingPLNames {
					if mandPLs[oldPLName] != nil || cts.hasNonIdleQueueSet(oldPLName) {
						nextPLNames.Insert(oldPLName)
					}
				}
				persistingPLNames = nextPLNames.Union(desiredPLNames)
				expectedQueueSetNames := persistingPLNames.Union(mandQueueSetNames)
				allQueueSetNames := cts.getQueueSetNames()
				missingQueueSetNames := expectedQueueSetNames.Difference(allQueueSetNames)
				if len(missingQueueSetNames) > 0 {
					t.Errorf("Fail: missing QueueSets %v", missingQueueSetNames)
				}
				nonIdleQueueSetNames := cts.getNonIdleQueueSetNames()
				extraQueueSetNames := nonIdleQueueSetNames.Difference(expectedQueueSetNames)
				if len(extraQueueSetNames) > 0 {
					t.Errorf("Fail: unexpected QueueSets %v", extraQueueSetNames)
				}
				for plName, hr, nCount := cts.popHeldRequest(); hr != nil; plName, hr, nCount = cts.popHeldRequest() {
					desired := desiredPLNames.Has(plName) || mandPLs[plName] != nil
					if testDebugLogs {
						t.Logf("Releasing held request %#+v, desired=%v, plName=%s, count:=%d", hr.rd, desired, plName, nCount)
					}
					close(hr.finishCh)
				}
				cts.requestWG.Wait()
				for _, ftr := range newFTRs {
					checkNewFS(cts, rng, trialStep, ftr, newCatchAlls)
				}

				j++
				if j > 20 {
					break
				}

				// Calculate expected survivors

				// Now create a new config and digest it
				trialStep = fmt.Sprintf("trial%d-%d", i, j)
				var newPLs []*flowcontrol.PriorityLevelConfiguration
				var newFSs []*flowcontrol.FlowSchema
				newPLs, _, desiredPLNames, newBadPLNames = genPLs(rng, trialStep, persistingPLNames, 1+rng.Intn(4))
				newFSs, _, newFTRs, newCatchAlls = genFSs(t, rng, trialStep, desiredPLNames, newBadPLNames, 1+rng.Intn(6))

				if testDebugLogs {
					for _, newPL := range newPLs {
						t.Logf("For %s, digesting newPL=%s", trialStep, fcfmt.Fmt(newPL))
					}
					for _, newFS := range newFSs {
						t.Logf("For %s, digesting newFS=%s", trialStep, fcfmt.Fmt(newFS))
					}
				}
				_ = ctlr.lockAndDigestConfigObjects(newPLs, newFSs)
			}
			for plName, hr, nCount := cts.popHeldRequest(); hr != nil; plName, hr, nCount = cts.popHeldRequest() {
				if testDebugLogs {
					desired := desiredPLNames.Has(plName) || mandPLs[plName] != nil
					t.Logf("Releasing held request %#+v, desired=%v, plName=%s, count:=%d", hr.rd, desired, plName, nCount)
				}
				close(hr.finishCh)
			}
			cts.requestWG.Wait()
		})
	}
}

func TestAPFControllerWithGracefulShutdown(t *testing.T) {
	const plName = "test-ps"
	fs := &flowcontrol.FlowSchema{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-fs",
		},
		Spec: flowcontrol.FlowSchemaSpec{
			MatchingPrecedence: 100,
			PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
				Name: plName,
			},
			DistinguisherMethod: &flowcontrol.FlowDistinguisherMethod{
				Type: flowcontrol.FlowDistinguisherMethodByUserType,
			},
		},
	}

	pl := &flowcontrol.PriorityLevelConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: plName,
		},
		Spec: flowcontrol.PriorityLevelConfigurationSpec{
			Type: flowcontrol.PriorityLevelEnablementLimited,
			Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
				NominalConcurrencyShares: ptr.To(int32(10)),
				LimitResponse: flowcontrol.LimitResponse{
					Type: flowcontrol.LimitResponseTypeReject,
				},
			},
		},
	}

	clientset := clientsetfake.NewSimpleClientset(fs, pl)
	informerFactory := informers.NewSharedInformerFactory(clientset, time.Second)
	flowcontrolClient := clientset.FlowcontrolV1()
	cts := &ctlrTestState{t: t,
		fcIfc:           flowcontrolClient,
		existingFSs:     map[string]*flowcontrol.FlowSchema{},
		existingPLs:     map[string]*flowcontrol.PriorityLevelConfiguration{},
		heldRequestsMap: map[string][]heldRequest{},
		queues:          map[string]*ctlrTestQueueSet{},
	}
	controller := newTestableController(TestableConfig{
		Name:                   "Controller",
		Clock:                  clock.RealClock{},
		AsFieldManager:         ConfigConsumerAsFieldManager,
		FoundToDangling:        func(found bool) bool { return !found },
		InformerFactory:        informerFactory,
		FlowcontrolClient:      flowcontrolClient,
		ServerConcurrencyLimit: 100,
		ReqsGaugeVec:           metrics.PriorityLevelConcurrencyGaugeVec,
		ExecSeatsGaugeVec:      metrics.PriorityLevelExecutionSeatsGaugeVec,
		QueueSetFactory:        cts,
	})

	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	informerFactory.Start(ctx.Done())

	status := informerFactory.WaitForCacheSync(ctx.Done())
	if names := unsynced(status); len(names) > 0 {
		t.Fatalf("WaitForCacheSync did not successfully complete, resources=%#v", names)
	}

	if err := controller.Start(ctx); err != nil {
		t.Fatalf("error starting controller: %v", err)
	}

	// ensure that the controller has run its first loop.
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		return controller.hasPriorityLevelState(plName), nil
	})
	if err != nil {
		t.Errorf("expected the controller to reconcile the priority level configuration object: %s, error: %s", plName, err)
	}
}

func unsynced(status map[reflect.Type]bool) []string {
	names := make([]string, 0)

	for objType, synced := range status {
		if !synced {
			names = append(names, objType.Name())
		}
	}

	return names
}

func checkNewFS(cts *ctlrTestState, rng *rand.Rand, trialName string, ftr *fsTestingRecord, catchAlls map[bool]*flowcontrol.FlowSchema) {
	t := cts.t
	ctlr := cts.cfgCtlr
	fs := ftr.fs
	expectedPLName := fs.Spec.PriorityLevelConfiguration.Name
	ctx := context.Background()
	// Use this to make sure all these requests have started executing
	// before the next reconfiguration
	var startWG sync.WaitGroup
	for matches, digests1 := range ftr.digests {
		for isResource, digests2 := range digests1 {
			for _, rd := range digests2 {
				finishCh := make(chan struct{})
				rdu := uniqify(rd)
				cts.requestWG.Add(1)
				startWG.Add(1)
				go func(matches, isResource bool, rdu RequestDigest) {
					expectedMatch := matches && ftr.wellFormed && (fsPrecedes(fs, catchAlls[isResource]) || fs.Name == catchAlls[isResource].Name)
					ctlr.Handle(ctx, rdu, func(matchFS *flowcontrol.FlowSchema, matchPL *flowcontrol.PriorityLevelConfiguration, _ string) {
						matchIsExempt := matchPL.Spec.Type == flowcontrol.PriorityLevelEnablementExempt
						if testDebugLogs {
							t.Logf("Considering FlowSchema %s, expectedMatch=%v, isResource=%v: Handle(%#+v) => note(fs=%s, pl=%s, isExempt=%v)", fs.Name, expectedMatch, isResource, rdu, matchFS.Name, matchPL.Name, matchIsExempt)
						}
						if a := matchFS.Name == fs.Name; expectedMatch != a {
							t.Errorf("Fail at %s/%s: rd=%#+v, expectedMatch=%v, actualMatch=%v, matchFSName=%q, catchAlls=%#+v", trialName, fs.Name, rdu, expectedMatch, a, matchFS.Name, catchAlls)
						}
						if matchFS.Name == fs.Name {
							if fs.Spec.PriorityLevelConfiguration.Name != matchPL.Name {
								t.Errorf("Fail at %s/%s: expected=%v, actual=%v", trialName, fs.Name, fs.Spec.PriorityLevelConfiguration.Name, matchPL.Name)
							}
						}
					}, func() fcrequest.WorkEstimate {
						return fcrequest.WorkEstimate{InitialSeats: 1}
					}, func(inQueue bool) {
					}, func() {
						startWG.Done()
						_ = <-finishCh
					})
					cts.requestWG.Done()
				}(matches, isResource, rdu)
				if rng.Float32() < 0.8 {
					if testDebugLogs {
						t.Logf("Immediate request %#+v, plName=%s", rdu, expectedPLName)
					}
					close(finishCh)
				} else {
					cts.addHeldRequest(expectedPLName, rdu, finishCh)
				}
			}
		}
	}
	startWG.Wait()
}

func genPLs(rng *rand.Rand, trial string, oldPLNames sets.String, n int) (pls []*flowcontrol.PriorityLevelConfiguration, plMap map[string]*flowcontrol.PriorityLevelConfiguration, goodNames, badNames sets.String) {
	pls = make([]*flowcontrol.PriorityLevelConfiguration, 0, n)
	plMap = make(map[string]*flowcontrol.PriorityLevelConfiguration, n)
	goodNames = sets.NewString()
	badNames = sets.NewString(trial+"-nopl1", trial+"-nopl2")
	addGood := func(pl *flowcontrol.PriorityLevelConfiguration) {
		pls = append(pls, pl)
		plMap[pl.Name] = pl
		goodNames.Insert(pl.Name)
	}
	for i := 1; i <= n; i++ {
		pl := genPL(rng, fmt.Sprintf("%s-pl%d", trial, i))
		addGood(pl)
	}
	for oldPLName := range oldPLNames {
		if _, has := mandPLs[oldPLName]; has {
			continue
		}
		if rng.Float32() < 0.67 {
			pl := genPL(rng, oldPLName)
			addGood(pl)
		}
	}
	for _, pl := range mandPLs {
		if n > 0 && rng.Float32() < 0.5 && !(goodNames.Has(pl.Name) || badNames.Has(pl.Name)) {
			addGood(pl)
		}
	}
	return
}

func genFSs(t *testing.T, rng *rand.Rand, trial string, goodPLNames, badPLNames sets.String, n int) (newFSs []*flowcontrol.FlowSchema, newFSMap map[string]*flowcontrol.FlowSchema, newFTRs map[string]*fsTestingRecord, catchAlls map[bool]*flowcontrol.FlowSchema) {
	newFTRs = map[string]*fsTestingRecord{}
	catchAlls = map[bool]*flowcontrol.FlowSchema{
		false: fcboot.MandatoryFlowSchemaCatchAll,
		true:  fcboot.MandatoryFlowSchemaCatchAll}
	newFSMap = map[string]*flowcontrol.FlowSchema{}
	add := func(ftr *fsTestingRecord) {
		newFSs = append(newFSs, ftr.fs)
		newFSMap[ftr.fs.Name] = ftr.fs
		newFTRs[ftr.fs.Name] = ftr
		if ftr.wellFormed {
			if ftr.matchesAllNonResourceRequests && fsPrecedes(ftr.fs, catchAlls[false]) {
				catchAlls[false] = ftr.fs
			}
			if ftr.matchesAllResourceRequests && fsPrecedes(ftr.fs, catchAlls[true]) {
				catchAlls[true] = ftr.fs
			}
		}
		if testDebugLogs {
			t.Logf("For trial %s, adding wf=%v FlowSchema %s", trial, ftr.wellFormed, fcfmt.Fmt(ftr.fs))
		}
	}
	if n == 0 || rng.Float32() < 0.5 {
		add(mandFTRCatchAll)
	}
	for i := 1; i <= n; i++ {
		ftr := genFS(t, rng, fmt.Sprintf("%s-fs%d", trial, i), false, goodPLNames, badPLNames)
		add(ftr)
	}
	if n == 0 || rng.Float32() < 0.5 {
		add(mandFTRExempt)
	}
	return
}

func fsPrecedes(a, b *flowcontrol.FlowSchema) bool {
	if a.Spec.MatchingPrecedence < b.Spec.MatchingPrecedence {
		return true
	}
	if a.Spec.MatchingPrecedence == b.Spec.MatchingPrecedence {
		return a.Name < b.Name
	}
	return false
}
