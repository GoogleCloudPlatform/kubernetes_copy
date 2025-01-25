/*
Copyright 2021 The Kubernetes Authors.

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
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	flowcontrol "k8s.io/api/flowcontrol/v1"
	utilfc "k8s.io/apiserver/pkg/util/flowcontrol"
	fqtesting "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/testing"
	"k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/clock"
	testclocks "k8s.io/utils/clock/testing"
)

/*
fightTest configures a test of how API Priority and Fairness config

	controllers fight when they disagree on how to set FlowSchemaStatus.
	In particular, they set the condition that indicates integrity of
	the reference to the PriorityLevelConfiguration.  The scenario tested is
	two teams of controllers, where the controllers in one team set the
	condition normally and the controllers in the other team set the condition
	to the opposite value.

	This is a behavioral test: it instantiates these controllers and runs them
	almost normally.  The test aims to run the controllers for a little under
	2 minutes.  The test takes clock readings to get upper and lower bounds on
	how long each controller ran, and calculates consequent bounds on the number
	of writes that should happen to each FlowSchemaStatus.  The test creates
	an informer to observe the writes.  The calculated lower bound on the
	number of writes is very lax, assuming only that one write can be done
	every 10 seconds.
*/
type fightTest struct {
	t              *testing.T
	ctx            context.Context
	cancel         context.CancelFunc
	loopbackConfig *rest.Config
	teamSize       int
	now            time.Time
	clk            *testclocks.FakeClock
	ctlrs          map[bool][]utilfc.Interface

	countsMutex sync.Mutex

	// writeCounts maps FlowSchema.Name to number of writes
	writeCounts map[string]int
}

func newFightTest(t *testing.T, loopbackConfig *rest.Config, teamSize int) *fightTest {
	now := time.Now()
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	ft := &fightTest{
		t:              t,
		ctx:            ctx,
		cancel:         cancel,
		loopbackConfig: loopbackConfig,
		teamSize:       teamSize,
		now:            now,
		clk:            testclocks.NewFakeClock(now),
		ctlrs: map[bool][]utilfc.Interface{
			false: make([]utilfc.Interface, teamSize),
			true:  make([]utilfc.Interface, teamSize)},
		writeCounts: map[string]int{},
	}
	return ft
}

func (ft *fightTest) createMainInformer() {
	myConfig := rest.CopyConfig(ft.loopbackConfig)
	myConfig = rest.AddUserAgent(myConfig, "audience")
	myClientset := clientset.NewForConfigOrDie(myConfig)
	informerFactory := informers.NewSharedInformerFactory(myClientset, 0)
	inf := informerFactory.Flowcontrol().V1().FlowSchemas().Informer()
	inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			fs := obj.(*flowcontrol.FlowSchema)
			ft.countWrite(fs)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			fs := newObj.(*flowcontrol.FlowSchema)
			ft.countWrite(fs)
		},
	})
	go inf.Run(ft.ctx.Done())
	if !cache.WaitForCacheSync(ft.ctx.Done(), inf.HasSynced) {
		ft.t.Errorf("Failed to sync main informer cache")
	}
}

func (ft *fightTest) countWrite(fs *flowcontrol.FlowSchema) {
	ft.countsMutex.Lock()
	defer ft.countsMutex.Unlock()
	ft.writeCounts[fs.Name]++
}

func (ft *fightTest) createController(invert bool, i int) {
	fieldMgr := fmt.Sprintf("testController%d%v", i, invert)
	myConfig := rest.CopyConfig(ft.loopbackConfig)
	myConfig = rest.AddUserAgent(myConfig, fieldMgr)
	myClientset := clientset.NewForConfigOrDie(myConfig)
	fcIfc := myClientset.FlowcontrolV1()
	informerFactory := informers.NewSharedInformerFactory(myClientset, 0)
	foundToDangling := func(found bool) bool { return !found }
	if invert {
		foundToDangling = func(found bool) bool { return found }
	}
	ctlr := utilfc.NewTestable(utilfc.TestableConfig{
		Name:                   fieldMgr,
		FoundToDangling:        foundToDangling,
		Clock:                  clock.RealClock{},
		AsFieldManager:         fieldMgr,
		InformerFactory:        informerFactory,
		FlowcontrolClient:      fcIfc,
		ServerConcurrencyLimit: 200, // server concurrency limit
		ReqsGaugeVec:           metrics.PriorityLevelConcurrencyGaugeVec,
		ExecSeatsGaugeVec:      metrics.PriorityLevelExecutionSeatsGaugeVec,
		QueueSetFactory:        fqtesting.NewNoRestraintFactory(),
	})
	ft.ctlrs[invert][i] = ctlr
	informerFactory.Start(ft.ctx.Done())
	if err := ctlr.Start(ft.ctx); err != nil {
		ft.t.Fatalf("error starting controller: %v", err)
	}
}

func (ft *fightTest) evaluate(tBeforeCreate, tAfterCreate time.Time) {
	tBeforeLock := time.Now()
	ft.countsMutex.Lock()
	defer ft.countsMutex.Unlock()
	tAfterLock := time.Now()
	minFightSecs := tBeforeLock.Sub(tAfterCreate).Seconds()
	maxFightSecs := tAfterLock.Sub(tBeforeCreate).Seconds()
	minTotalWrites := int(minFightSecs / 10)
	maxWritesPerWriter := 6 * int(math.Ceil(maxFightSecs/60))
	maxTotalWrites := (1 + ft.teamSize*2) * maxWritesPerWriter
	for flowSchemaName, writeCount := range ft.writeCounts {
		if writeCount < minTotalWrites {
			ft.t.Errorf("There were a total of %d writes to FlowSchema %s but there should have been at least %d from %s to %s", writeCount, flowSchemaName, minTotalWrites, tAfterCreate, tBeforeLock)
		} else if writeCount > maxTotalWrites {
			ft.t.Errorf("There were a total of %d writes to FlowSchema %s but there should have been no more than %d from %s to %s", writeCount, flowSchemaName, maxTotalWrites, tBeforeCreate, tAfterLock)
		} else {
			ft.t.Logf("There were a total of %d writes to FlowSchema %s over %v, %v seconds", writeCount, flowSchemaName, minFightSecs, maxFightSecs)
		}
	}
}
func TestConfigConsumerFight(t *testing.T) {
	_, kubeConfig, closeFn := setup(t, 100, 100)
	defer closeFn()
	const teamSize = 3
	ft := newFightTest(t, kubeConfig, teamSize)
	tBeforeCreate := time.Now()
	ft.createMainInformer()
	ft.foreach(ft.createController)
	tAfterCreate := time.Now()
	time.Sleep(110 * time.Second)
	ft.evaluate(tBeforeCreate, tAfterCreate)
	ft.cancel()
}

func (ft *fightTest) foreach(visit func(invert bool, i int)) {
	for i := 0; i < ft.teamSize; i++ {
		// The order of the following enumeration is not deterministic,
		// and that is good.
		invert := rand.Intn(2) == 0
		visit(invert, i)
		visit(!invert, i)
	}
}
