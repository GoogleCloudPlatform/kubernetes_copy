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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp" //nolint:depguard
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	fcboot "k8s.io/apiserver/pkg/apis/flowcontrol/bootstrap"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/util/apihelpers"
	fq "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing"
	fcfmt "k8s.io/apiserver/pkg/util/flowcontrol/format"
	"k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	fcrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	flowcontrol "k8s.io/api/flowcontrol/v1"
	flowcontrolapplyconfiguration "k8s.io/client-go/applyconfigurations/flowcontrol/v1"
	flowcontrolclient "k8s.io/client-go/kubernetes/typed/flowcontrol/v1"
	flowcontrollister "k8s.io/client-go/listers/flowcontrol/v1"
)

const timeFmt = "2006-01-02T15:04:05.999"

const (
	// priorityLevelMaxSeatsPercent is the percentage of the nominalCL used as max seats allocatable from work estimator
	priorityLevelMaxSeatsPercent = float64(0.15)
)

// This file contains a simple local (to the apiserver) controller
// that digests API Priority and Fairness config objects (FlowSchema
// and PriorityLevelConfiguration) into the data structure that the
// filter uses.  At this first level of development this controller
// takes the simplest possible approach: whenever notified of any
// change to any config object, or when any priority level that is
// undesired becomes completely unused, all the config objects are
// read and processed as a whole.

const (
	// Borrowing among priority levels will be accomplished by periodically
	// adjusting the current concurrency limits (CurrentCLs);
	// borrowingAdjustmentPeriod is that period.
	borrowingAdjustmentPeriod = 10 * time.Second

	// The input to the seat borrowing is smoothed seat demand figures.
	// This constant controls the decay rate of that smoothing,
	// as described in the comment on the `seatDemandStats` field of `priorityLevelState`.
	// The particular number appearing here has the property that half-life
	// of that decay is 5 minutes.
	// This is a very preliminary guess at a good value and is likely to be tweaked
	// once we get some experience with borrowing.
	seatDemandSmoothingCoefficient = 0.977
)

// The funcs in this package follow the naming convention that the suffix
// "Locked" means the relevant mutex must be locked at the start of each
// call and will be locked upon return.  For a configController, the
// suffix "ReadLocked" stipulates a read lock while just "Locked"
// stipulates a full lock.  Absence of either suffix means that either
// (a) the lock must NOT be held at call time and will not be held
// upon return or (b) locking is irrelevant.

// StartFunction begins the process of handling a request.  If the
// request gets queued then this function uses the given hashValue as
// the source of entropy as it shuffle-shards the request into a
// queue.  The descr1 and descr2 values play no role in the logic but
// appear in log messages.  This method does not return until the
// queuing, if any, for this request is done.  If `execute` is false
// then `afterExecution` is irrelevant and the request should be
// rejected.  Otherwise the request should be executed and
// `afterExecution` must be called exactly once.
type StartFunction func(ctx context.Context, hashValue uint64) (execute bool, afterExecution func())

// RequestDigest holds necessary info from request for flow-control
type RequestDigest struct {
	RequestInfo *request.RequestInfo
	User        user.Info
}

// `*configController` maintains eventual consistency with the API
// objects that configure API Priority and Fairness, and provides a
// procedural interface to the configured behavior.  The methods of
// this type and cfgMeal follow the convention that the suffix
// "Locked" means that the caller must hold the configController lock.
type configController struct {
	name              string // varies in tests of fighting controllers
	clock             clock.PassiveClock
	queueSetFactory   fq.QueueSetFactory
	reqsGaugeVec      metrics.RatioedGaugeVec
	execSeatsGaugeVec metrics.RatioedGaugeVec

	// How this controller appears in an ObjectMeta ManagedFieldsEntry.Manager
	asFieldManager string

	// Given a boolean indicating whether a FlowSchema's referenced
	// PriorityLevelConfig exists, return a boolean indicating whether
	// the reference is dangling
	foundToDangling func(bool) bool

	// configQueue holds `(interface{})(0)` when the configuration
	// objects need to be reprocessed.
	configQueue workqueue.TypedRateLimitingInterface[int]

	plLister         flowcontrollister.PriorityLevelConfigurationLister
	plInformerSynced cache.InformerSynced

	fsLister         flowcontrollister.FlowSchemaLister
	fsInformerSynced cache.InformerSynced

	flowcontrolClient flowcontrolclient.FlowcontrolV1Interface

	// serverConcurrencyLimit is the limit on the server's total
	// number of non-exempt requests being served at once.  This comes
	// from server configuration.
	serverConcurrencyLimit int

	// watchTracker implements the necessary WatchTracker interface.
	WatchTracker

	// MaxSeatsTracker tracks the maximum seats that should be allocatable from the
	// work estimator for a given priority level. This controller does not enforce
	// any limits on max seats stored in this tracker, it is up to the work estimator
	// to set lower/upper limits on max seats (currently min=1, max=10).
	MaxSeatsTracker

	// the most recent update attempts, ordered by increasing age.
	// Consumer trims to keep only the last minute's worth of entries.
	// The controller uses this to limit itself to at most six updates
	// to a given FlowSchema in any minute.
	// This may only be accessed from the one and only worker goroutine.
	mostRecentUpdates []updateAttempt

	// This must be locked while accessing the later fields.
	// A lock for writing is needed
	// for writing to any of the following:
	// - the flowSchemas field
	// - the slice held in the flowSchemas field
	// - the priorityLevelStates field
	// - the map held in the priorityLevelStates field
	// - any field of a priorityLevelState held in that map
	lock sync.RWMutex

	// flowSchemas holds the flow schema objects, sorted by increasing
	// numerical (decreasing logical) matching precedence.  Every
	// FlowSchema in this slice is immutable.
	flowSchemas apihelpers.FlowSchemaSequence

	// priorityLevelStates maps the PriorityLevelConfiguration object
	// name to the state for that level.  Every name referenced from a
	// member of `flowSchemas` has an entry here.
	priorityLevelStates map[string]*priorityLevelState

	// nominalCLSum is the sum of the nominalCL fields in the priorityLevelState records.
	// This can exceed serverConcurrencyLimit because of the deliberate rounding up
	// in the computation of the nominalCL values.
	// This is tracked because it is an input to the allocation adjustment algorithm.
	nominalCLSum int
}

type updateAttempt struct {
	timeUpdated  time.Time
	updatedItems sets.String // FlowSchema names
}

// priorityLevelState holds the state specific to a priority level.
type priorityLevelState struct {
	// the API object or prototype prescribing this level.  Nothing
	// reached through this pointer is mutable.
	pl *flowcontrol.PriorityLevelConfiguration

	// qsCompleter holds the QueueSetCompleter derived from `pl`
	// and `queues`.
	qsCompleter fq.QueueSetCompleter

	// The QueueSet for this priority level.
	// Never nil.
	queues fq.QueueSet

	// quiescing==true indicates that this priority level should be
	// removed when its queues have all drained.
	quiescing bool

	// number of goroutines between Controller::Match and calling the
	// returned StartFunction
	numPending int

	// Observers tracking number of requests waiting, executing
	reqsGaugePair metrics.RatioedGaugePair

	// Observer of number of seats occupied throughout execution
	execSeatsObs metrics.RatioedGauge

	// Integrator of seat demand, reset every CurrentCL adjustment period
	seatDemandIntegrator fq.Integrator

	// Gauge of seat demand / nominalCL
	seatDemandRatioedGauge metrics.RatioedGauge

	// seatDemandStats is derived from periodically examining the seatDemandIntegrator.
	// The average, standard deviation, and high watermark come directly from the integrator.
	// envelope = avg + stdDev.
	// Periodically smoothed gets replaced with `max(envelope, A*smoothed + (1-A)*envelope)`,
	// where A is seatDemandSmoothingCoefficient.
	seatDemandStats seatDemandStats

	// nominalCL is the nominal concurrency limit configured in the PriorityLevelConfiguration
	nominalCL int

	// minCL is the nominal limit less the lendable amount
	minCL int

	//maxCL is the nominal limit plus the amount that may be borrowed
	maxCL int

	// currentCL is the dynamically derived concurrency limit to impose for now
	currentCL int
}

type seatDemandStats struct {
	avg           float64
	stdDev        float64
	highWatermark int
	smoothed      float64
}

func (stats *seatDemandStats) update(obs fq.IntegratorResults) {
	stats.highWatermark = int(math.Round(obs.Max))
	if obs.Duration <= 0 {
		return
	}
	if math.IsNaN(obs.Deviation) {
		obs.Deviation = 0
	}
	stats.avg = obs.Average
	stats.stdDev = obs.Deviation
	envelope := obs.Average + obs.Deviation
	stats.smoothed = math.Max(envelope, seatDemandSmoothingCoefficient*stats.smoothed+(1-seatDemandSmoothingCoefficient)*envelope)
}

// NewTestableController is extra flexible to facilitate testing
func newTestableController(config TestableConfig) *configController {
	cfgCtlr := &configController{
		name:                   config.Name,
		clock:                  config.Clock,
		queueSetFactory:        config.QueueSetFactory,
		reqsGaugeVec:           config.ReqsGaugeVec,
		execSeatsGaugeVec:      config.ExecSeatsGaugeVec,
		asFieldManager:         config.AsFieldManager,
		foundToDangling:        config.FoundToDangling,
		serverConcurrencyLimit: config.ServerConcurrencyLimit,
		flowcontrolClient:      config.FlowcontrolClient,
		priorityLevelStates:    make(map[string]*priorityLevelState),
		WatchTracker:           NewWatchTracker(),
		MaxSeatsTracker:        NewMaxSeatsTracker(),
	}
	klog.V(2).Infof("NewTestableController %q with serverConcurrencyLimit=%d, name=%s, asFieldManager=%q", cfgCtlr.name, cfgCtlr.serverConcurrencyLimit, cfgCtlr.name, cfgCtlr.asFieldManager)
	// Start with longish delay because conflicts will be between
	// different processes, so take some time to go away.
	cfgCtlr.configQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemExponentialFailureRateLimiter[int](200*time.Millisecond, 8*time.Hour),
		workqueue.TypedRateLimitingQueueConfig[int]{Name: "priority_and_fairness_config_queue"},
	)
	// ensure the data structure reflects the mandatory config
	cfgCtlr.lockAndDigestConfigObjects(nil, nil)
	fci := config.InformerFactory.Flowcontrol().V1()
	pli := fci.PriorityLevelConfigurations()
	fsi := fci.FlowSchemas()
	cfgCtlr.plLister = pli.Lister()
	cfgCtlr.plInformerSynced = pli.Informer().HasSynced
	cfgCtlr.fsLister = fsi.Lister()
	cfgCtlr.fsInformerSynced = fsi.Informer().HasSynced
	pli.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pl := obj.(*flowcontrol.PriorityLevelConfiguration)
			klog.V(7).Infof("Triggered API priority and fairness config reloading in %s due to creation of PLC %s", cfgCtlr.name, pl.Name)
			cfgCtlr.configQueue.Add(0)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newPL := newObj.(*flowcontrol.PriorityLevelConfiguration)
			oldPL := oldObj.(*flowcontrol.PriorityLevelConfiguration)
			if !apiequality.Semantic.DeepEqual(oldPL.Spec, newPL.Spec) {
				klog.V(7).Infof("Triggered API priority and fairness config reloading in %s due to spec update of PLC %s", cfgCtlr.name, newPL.Name)
				cfgCtlr.configQueue.Add(0)
			} else {
				klog.V(7).Infof("No trigger API priority and fairness config reloading in %s due to spec non-change of PLC %s", cfgCtlr.name, newPL.Name)
			}
		},
		DeleteFunc: func(obj interface{}) {
			name, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			klog.V(7).Infof("Triggered API priority and fairness config reloading in %s due to deletion of PLC %s", cfgCtlr.name, name)
			cfgCtlr.configQueue.Add(0)

		}})
	fsi.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			fs := obj.(*flowcontrol.FlowSchema)
			klog.V(7).Infof("Triggered API priority and fairness config reloading in %s due to creation of FS %s", cfgCtlr.name, fs.Name)
			cfgCtlr.configQueue.Add(0)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newFS := newObj.(*flowcontrol.FlowSchema)
			oldFS := oldObj.(*flowcontrol.FlowSchema)
			// Changes to either Spec or Status are relevant.  The
			// concern is that we might, in some future release, want
			// different behavior than is implemented now. One of the
			// hardest questions is how does an operator roll out the
			// new release in a cluster with multiple kube-apiservers
			// --- in a way that works no matter what servers crash
			// and restart when. If this handler reacts only to
			// changes in Spec then we have a scenario in which the
			// rollout leaves the old Status in place. The scenario
			// ends with this subsequence: deploy the last new server
			// before deleting the last old server, and in between
			// those two operations the last old server crashes and
			// recovers. The chosen solution is making this controller
			// insist on maintaining the particular state that it
			// establishes.
			if !(apiequality.Semantic.DeepEqual(oldFS.Spec, newFS.Spec) &&
				apiequality.Semantic.DeepEqual(oldFS.Status, newFS.Status)) {
				klog.V(7).Infof("Triggered API priority and fairness config reloading in %s due to spec and/or status update of FS %s", cfgCtlr.name, newFS.Name)
				cfgCtlr.configQueue.Add(0)
			} else {
				klog.V(7).Infof("No trigger of API priority and fairness config reloading in %s due to spec and status non-change of FS %s", cfgCtlr.name, newFS.Name)
			}
		},
		DeleteFunc: func(obj interface{}) {
			name, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			klog.V(7).Infof("Triggered API priority and fairness config reloading in %s due to deletion of FS %s", cfgCtlr.name, name)
			cfgCtlr.configQueue.Add(0)

		}})
	return cfgCtlr
}

func (cfgCtlr *configController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()

	// Let the config worker stop when we are done
	defer cfgCtlr.configQueue.ShutDown()

	klog.Info("Starting API Priority and Fairness config controller")
	if ok := cache.WaitForCacheSync(stopCh, cfgCtlr.plInformerSynced, cfgCtlr.fsInformerSynced); !ok {
		return fmt.Errorf("Never achieved initial sync")
	}

	klog.Info("Running API Priority and Fairness config worker")
	go wait.Until(cfgCtlr.runWorker, time.Second, stopCh)

	klog.Info("Running API Priority and Fairness periodic rebalancing process")
	go wait.Until(cfgCtlr.updateBorrowing, borrowingAdjustmentPeriod, stopCh)

	<-stopCh
	klog.Info("Shutting down API Priority and Fairness config worker")
	return nil
}

func (cfgCtlr *configController) updateBorrowing() {
	cfgCtlr.lock.Lock()
	defer cfgCtlr.lock.Unlock()
	cfgCtlr.updateBorrowingLocked(true, cfgCtlr.priorityLevelStates)
}

func (cfgCtlr *configController) updateBorrowingLocked(setCompleters bool, plStates map[string]*priorityLevelState) {
	items := make([]allocProblemItem, 0, len(plStates))
	nonExemptPLNames := make([]string, 0, len(plStates))
	idxOfNonExempt := map[string]int{} // items index of non-exempt classes
	cclOfExempt := map[string]int{}    // minCurrentCL of exempt classes
	var minCLSum, minCurrentCLSum int  // sums over non-exempt classes
	remainingServerCL := cfgCtlr.nominalCLSum
	for plName, plState := range plStates {
		obs := plState.seatDemandIntegrator.Reset()
		plState.seatDemandStats.update(obs)
		var minCurrentCL int
		if plState.pl.Spec.Type == flowcontrol.PriorityLevelEnablementExempt {
			minCurrentCL = max(plState.minCL, plState.seatDemandStats.highWatermark)
			cclOfExempt[plName] = minCurrentCL
			remainingServerCL -= minCurrentCL
		} else {
			// Lower bound on this priority level's adjusted concurrency limit is the lesser of:
			// - its seat demand high watermark over the last adjustment period, and
			// - its configured concurrency limit.
			// BUT: we do not want this to be lower than the lower bound from configuration.
			// See KEP-1040 for a more detailed explanation.
			minCurrentCL = max(plState.minCL, min(plState.nominalCL, plState.seatDemandStats.highWatermark))
			idxOfNonExempt[plName] = len(items)
			nonExemptPLNames = append(nonExemptPLNames, plName)
			items = append(items, allocProblemItem{
				lowerBound: float64(minCurrentCL),
				upperBound: float64(plState.maxCL),
				target:     math.Max(float64(minCurrentCL), plState.seatDemandStats.smoothed),
			})
			minCLSum += plState.minCL
			minCurrentCLSum += minCurrentCL
		}
	}
	if len(items) == 0 && cfgCtlr.nominalCLSum > 0 {
		klog.ErrorS(nil, "Impossible: no priority levels", "plStates", cfgCtlr.priorityLevelStates)
		return
	}
	var allocs []float64
	var shareFrac, fairFrac float64
	var err error
	if remainingServerCL <= minCLSum {
		metrics.SetFairFrac(0)
	} else if remainingServerCL <= minCurrentCLSum {
		shareFrac = float64(remainingServerCL-minCLSum) / float64(minCurrentCLSum-minCLSum)
		metrics.SetFairFrac(0)
	} else {
		allocs, fairFrac, err = computeConcurrencyAllocation(cfgCtlr.nominalCLSum, items)
		if err != nil {
			klog.ErrorS(err, "Unable to derive new concurrency limits", "plNames", nonExemptPLNames, "items", items)
			allocs = make([]float64, len(items))
			for idx, plName := range nonExemptPLNames {
				plState := plStates[plName]
				allocs[idx] = float64(plState.currentCL)
			}
		}
		metrics.SetFairFrac(float64(fairFrac))
	}
	for plName, plState := range plStates {
		idx, isNonExempt := idxOfNonExempt[plName]
		if setCompleters {
			qsCompleter, err := queueSetCompleterForPL(cfgCtlr.queueSetFactory, plState.queues,
				plState.pl, plState.reqsGaugePair, plState.execSeatsObs,
				metrics.NewUnionGauge(plState.seatDemandIntegrator, plState.seatDemandRatioedGauge))
			if err != nil {
				klog.ErrorS(err, "Inconceivable!  Configuration error in existing priority level", "pl", plState.pl)
				continue
			}
			plState.qsCompleter = qsCompleter
		}
		var currentCL int
		if !isNonExempt {
			currentCL = cclOfExempt[plName]
		} else if remainingServerCL <= minCLSum {
			currentCL = plState.minCL
		} else if remainingServerCL <= minCurrentCLSum {
			minCurrentCL := max(plState.minCL, min(plState.nominalCL, plState.seatDemandStats.highWatermark))
			currentCL = plState.minCL + int(math.Round(float64(minCurrentCL-plState.minCL)*shareFrac))
		} else {
			currentCL = int(math.Round(float64(allocs[idx])))
		}
		relChange := relDiff(float64(currentCL), float64(plState.currentCL))
		plState.currentCL = currentCL
		metrics.NotePriorityLevelConcurrencyAdjustment(plState.pl.Name, float64(plState.seatDemandStats.highWatermark), plState.seatDemandStats.avg, plState.seatDemandStats.stdDev, plState.seatDemandStats.smoothed, float64(items[idx].target), currentCL)
		logLevel := klog.Level(4)
		if relChange >= 0.05 {
			logLevel = 2
		}
		var concurrencyDenominator int
		if currentCL > 0 {
			concurrencyDenominator = currentCL
		} else {
			concurrencyDenominator = int(math.Max(1, math.Round(float64(cfgCtlr.serverConcurrencyLimit)/10)))
		}
		plState.seatDemandRatioedGauge.SetDenominator(float64(concurrencyDenominator))
		klog.V(logLevel).InfoS("Update CurrentCL", "plName", plName, "seatDemandHighWatermark", plState.seatDemandStats.highWatermark, "seatDemandAvg", plState.seatDemandStats.avg, "seatDemandStdev", plState.seatDemandStats.stdDev, "seatDemandSmoothed", plState.seatDemandStats.smoothed, "fairFrac", fairFrac, "currentCL", currentCL, "concurrencyDenominator", concurrencyDenominator, "backstop", err != nil)
		plState.queues = plState.qsCompleter.Complete(fq.DispatchingConfig{ConcurrencyLimit: currentCL, ConcurrencyDenominator: concurrencyDenominator})
	}
}

// runWorker is the logic of the one and only worker goroutine.  We
// limit the number to one in order to obviate explicit
// synchronization around access to `cfgCtlr.mostRecentUpdates`.
func (cfgCtlr *configController) runWorker() {
	for cfgCtlr.processNextWorkItem() {
	}
}

// processNextWorkItem works on one entry from the work queue.
// Only invoke this in the one and only worker goroutine.
func (cfgCtlr *configController) processNextWorkItem() bool {
	obj, shutdown := cfgCtlr.configQueue.Get()
	if shutdown {
		return false
	}

	func(obj int) {
		defer cfgCtlr.configQueue.Done(obj)
		specificDelay, err := cfgCtlr.syncOne()
		switch {
		case err != nil:
			klog.Error(err)
			cfgCtlr.configQueue.AddRateLimited(obj)
		case specificDelay > 0:
			cfgCtlr.configQueue.AddAfter(obj, specificDelay)
		default:
			cfgCtlr.configQueue.Forget(obj)
		}
	}(obj)

	return true
}

// syncOne does one full synchronization.  It reads all the API
// objects that configure API Priority and Fairness and updates the
// local configController accordingly.
// Only invoke this in the one and only worker goroutine
func (cfgCtlr *configController) syncOne() (specificDelay time.Duration, err error) {
	klog.V(5).Infof("%s syncOne at %s", cfgCtlr.name, cfgCtlr.clock.Now().Format(timeFmt))
	all := labels.Everything()
	newPLs, err := cfgCtlr.plLister.List(all)
	if err != nil {
		return 0, fmt.Errorf("unable to list PriorityLevelConfiguration objects: %w", err)
	}
	newFSs, err := cfgCtlr.fsLister.List(all)
	if err != nil {
		return 0, fmt.Errorf("unable to list FlowSchema objects: %w", err)
	}
	return cfgCtlr.digestConfigObjects(newPLs, newFSs)
}

// cfgMeal is the data involved in the process of digesting the API
// objects that configure API Priority and Fairness.  All the config
// objects are digested together, because this is the simplest way to
// cope with the various dependencies between objects.  The process of
// digestion is done in four passes over config objects --- three
// passes over PriorityLevelConfigurations and one pass over the
// FlowSchemas --- with the work divided among the passes according to
// those dependencies.
type cfgMeal struct {
	cfgCtlr *configController

	newPLStates map[string]*priorityLevelState

	// The sum of the concurrency shares of the priority levels in the
	// new configuration
	shareSum float64

	// These keep track of which mandatory priority level config
	// objects have been digested
	haveExemptPL, haveCatchAllPL bool

	// Buffered FlowSchema status updates to do.  Do them when the
	// lock is not held, to avoid a deadlock due to such a request
	// provoking a call into this controller while the lock held
	// waiting on that request to complete.
	fsStatusUpdates []fsStatusUpdate

	maxWaitingRequests, maxExecutingRequests int
}

// A buffered set of status updates for FlowSchemas
type fsStatusUpdate struct {
	flowSchema *flowcontrol.FlowSchema
	condition  flowcontrol.FlowSchemaCondition
	oldValue   flowcontrol.FlowSchemaCondition
}

// digestConfigObjects is given all the API objects that configure
// cfgCtlr and writes its consequent new configState.
// Only invoke this in the one and only worker goroutine
func (cfgCtlr *configController) digestConfigObjects(newPLs []*flowcontrol.PriorityLevelConfiguration, newFSs []*flowcontrol.FlowSchema) (time.Duration, error) {
	fsStatusUpdates := cfgCtlr.lockAndDigestConfigObjects(newPLs, newFSs)
	var errs []error
	currResult := updateAttempt{
		timeUpdated:  cfgCtlr.clock.Now(),
		updatedItems: sets.String{},
	}
	var suggestedDelay time.Duration
	for _, fsu := range fsStatusUpdates {
		// if we should skip this name, indicate we will need a delay, but continue with other entries
		if cfgCtlr.shouldDelayUpdate(fsu.flowSchema.Name) {
			if suggestedDelay == 0 {
				suggestedDelay = time.Duration(30+rand.Intn(45)) * time.Second
			}
			continue
		}

		// if we are going to issue an update, be sure we track every name we update so we know if we update it too often.
		currResult.updatedItems.Insert(fsu.flowSchema.Name)
		if klogV := klog.V(4); klogV.Enabled() {
			klogV.Infof("%s writing Condition %s to FlowSchema %s, which had ResourceVersion=%s, because its previous value was %s, diff: %s",
				cfgCtlr.name, fsu.condition, fsu.flowSchema.Name, fsu.flowSchema.ResourceVersion, fcfmt.Fmt(fsu.oldValue), cmp.Diff(fsu.oldValue, fsu.condition))
		}

		if err := apply(cfgCtlr.flowcontrolClient.FlowSchemas(), fsu, cfgCtlr.asFieldManager); err != nil {
			if apierrors.IsNotFound(err) {
				// This object has been deleted.  A notification is coming
				// and nothing more needs to be done here.
				klog.V(5).Infof("%s at %s: attempted update of concurrently deleted FlowSchema %s; nothing more needs to be done", cfgCtlr.name, cfgCtlr.clock.Now().Format(timeFmt), fsu.flowSchema.Name)
			} else {
				errs = append(errs, fmt.Errorf("failed to set a status.condition for FlowSchema %s: %w", fsu.flowSchema.Name, err))
			}
		}
	}
	cfgCtlr.addUpdateResult(currResult)

	return suggestedDelay, utilerrors.NewAggregate(errs)
}

func apply(client flowcontrolclient.FlowSchemaInterface, fsu fsStatusUpdate, asFieldManager string) error {
	applyOptions := metav1.ApplyOptions{FieldManager: asFieldManager, Force: true}

	// the condition field in fsStatusUpdate holds the new condition we want to update.
	// TODO: this will break when we have multiple conditions for a flowschema
	_, err := client.ApplyStatus(context.TODO(), toFlowSchemaApplyConfiguration(fsu), applyOptions)
	return err
}

func toFlowSchemaApplyConfiguration(fsUpdate fsStatusUpdate) *flowcontrolapplyconfiguration.FlowSchemaApplyConfiguration {
	condition := flowcontrolapplyconfiguration.FlowSchemaCondition().
		WithType(fsUpdate.condition.Type).
		WithStatus(fsUpdate.condition.Status).
		WithReason(fsUpdate.condition.Reason).
		WithLastTransitionTime(fsUpdate.condition.LastTransitionTime).
		WithMessage(fsUpdate.condition.Message)

	return flowcontrolapplyconfiguration.FlowSchema(fsUpdate.flowSchema.Name).
		WithStatus(flowcontrolapplyconfiguration.FlowSchemaStatus().
			WithConditions(condition),
		)
}

// shouldDelayUpdate checks to see if a flowschema has been updated too often and returns true if a delay is needed.
// Only invoke this in the one and only worker goroutine
func (cfgCtlr *configController) shouldDelayUpdate(flowSchemaName string) bool {
	numUpdatesInPastMinute := 0
	oneMinuteAgo := cfgCtlr.clock.Now().Add(-1 * time.Minute)
	for idx, update := range cfgCtlr.mostRecentUpdates {
		if oneMinuteAgo.After(update.timeUpdated) {
			// this and the remaining items are no longer relevant
			cfgCtlr.mostRecentUpdates = cfgCtlr.mostRecentUpdates[:idx]
			return false
		}
		if update.updatedItems.Has(flowSchemaName) {
			numUpdatesInPastMinute++
			if numUpdatesInPastMinute > 5 {
				return true
			}
		}
	}
	return false
}

// addUpdateResult adds the result. It isn't a ring buffer because
// this is small and rate limited.
// Only invoke this in the one and only worker goroutine
func (cfgCtlr *configController) addUpdateResult(result updateAttempt) {
	cfgCtlr.mostRecentUpdates = append([]updateAttempt{result}, cfgCtlr.mostRecentUpdates...)
}

func (cfgCtlr *configController) lockAndDigestConfigObjects(newPLs []*flowcontrol.PriorityLevelConfiguration, newFSs []*flowcontrol.FlowSchema) []fsStatusUpdate {
	cfgCtlr.lock.Lock()
	defer cfgCtlr.lock.Unlock()
	meal := cfgMeal{
		cfgCtlr:     cfgCtlr,
		newPLStates: make(map[string]*priorityLevelState),
	}

	meal.digestNewPLsLocked(newPLs)
	meal.digestFlowSchemasLocked(newFSs)
	meal.processOldPLsLocked()

	// Supply missing mandatory PriorityLevelConfiguration objects
	if !meal.haveExemptPL {
		meal.imaginePL(fcboot.MandatoryPriorityLevelConfigurationExempt)
	}
	if !meal.haveCatchAllPL {
		meal.imaginePL(fcboot.MandatoryPriorityLevelConfigurationCatchAll)
	}

	meal.finishQueueSetReconfigsLocked()

	// The new config has been constructed
	cfgCtlr.priorityLevelStates = meal.newPLStates
	klog.V(5).InfoS("Switched to new API Priority and Fairness configuration", "maxWaitingRequests", meal.maxWaitingRequests, "maxExecutinRequests", meal.maxExecutingRequests)

	metrics.GetWaitingReadonlyConcurrency().SetDenominator(float64(meal.maxWaitingRequests))
	metrics.GetWaitingMutatingConcurrency().SetDenominator(float64(meal.maxWaitingRequests))
	metrics.GetExecutingReadonlyConcurrency().SetDenominator(float64(meal.maxExecutingRequests))
	metrics.GetExecutingMutatingConcurrency().SetDenominator(float64(meal.maxExecutingRequests))

	return meal.fsStatusUpdates
}

// Digest the new set of PriorityLevelConfiguration objects.
// Pretend broken ones do not exist.
func (meal *cfgMeal) digestNewPLsLocked(newPLs []*flowcontrol.PriorityLevelConfiguration) {
	for _, pl := range newPLs {
		state := meal.cfgCtlr.priorityLevelStates[pl.Name]
		if state == nil {
			labelValues := []string{pl.Name}
			state = &priorityLevelState{
				reqsGaugePair:          metrics.RatioedGaugeVecPhasedElementPair(meal.cfgCtlr.reqsGaugeVec, 1, 1, labelValues),
				execSeatsObs:           meal.cfgCtlr.execSeatsGaugeVec.NewForLabelValuesSafe(0, 1, labelValues),
				seatDemandIntegrator:   fq.NewNamedIntegrator(meal.cfgCtlr.clock, pl.Name),
				seatDemandRatioedGauge: metrics.ApiserverSeatDemands.NewForLabelValuesSafe(0, 1, []string{pl.Name}),
			}
		}
		qsCompleter, err := queueSetCompleterForPL(meal.cfgCtlr.queueSetFactory, state.queues,
			pl, state.reqsGaugePair, state.execSeatsObs,
			metrics.NewUnionGauge(state.seatDemandIntegrator, state.seatDemandRatioedGauge))
		if err != nil {
			klog.Warningf("Ignoring PriorityLevelConfiguration object %s because its spec (%s) is broken: %s", pl.Name, fcfmt.Fmt(pl.Spec), err)
			continue
		}
		meal.newPLStates[pl.Name] = state
		state.pl = pl
		state.qsCompleter = qsCompleter
		if state.quiescing { // it was undesired, but no longer
			klog.V(3).Infof("Priority level %q was undesired and has become desired again", pl.Name)
			state.quiescing = false
		}
		nominalConcurrencyShares, _, _ := plSpecCommons(state.pl)
		meal.shareSum += float64(*nominalConcurrencyShares)
		meal.haveExemptPL = meal.haveExemptPL || pl.Name == flowcontrol.PriorityLevelConfigurationNameExempt
		meal.haveCatchAllPL = meal.haveCatchAllPL || pl.Name == flowcontrol.PriorityLevelConfigurationNameCatchAll
	}
}

// Digest the given FlowSchema objects.  Ones that reference a missing
// or broken priority level are not to be passed on to the filter for
// use.  We do this before holding over old priority levels so that
// requests stop going to those levels and FlowSchemaStatus values
// reflect this.  This function also adds any missing mandatory
// FlowSchema objects.  The given objects must all have distinct
// names.
func (meal *cfgMeal) digestFlowSchemasLocked(newFSs []*flowcontrol.FlowSchema) {
	fsSeq := make(apihelpers.FlowSchemaSequence, 0, len(newFSs))
	fsMap := make(map[string]*flowcontrol.FlowSchema, len(newFSs))
	var haveExemptFS, haveCatchAllFS bool
	for i, fs := range newFSs {
		otherFS := fsMap[fs.Name]
		if otherFS != nil {
			// This client is forbidden to do this.
			panic(fmt.Sprintf("Given two FlowSchema objects with the same name: %s and %s", fcfmt.Fmt(otherFS), fcfmt.Fmt(fs)))
		}
		fsMap[fs.Name] = fs
		_, goodPriorityRef := meal.newPLStates[fs.Spec.PriorityLevelConfiguration.Name]

		// Ensure the object's status reflects whether its priority
		// level reference is broken.
		//
		// TODO: consider not even trying if server is not handling
		// requests yet.
		meal.presyncFlowSchemaStatus(fs, meal.cfgCtlr.foundToDangling(goodPriorityRef), fs.Spec.PriorityLevelConfiguration.Name)

		if !goodPriorityRef {
			klog.V(6).Infof("Ignoring FlowSchema %s because of bad priority level reference %q", fs.Name, fs.Spec.PriorityLevelConfiguration.Name)
			continue
		}
		fsSeq = append(fsSeq, newFSs[i])
		haveExemptFS = haveExemptFS || fs.Name == flowcontrol.FlowSchemaNameExempt
		haveCatchAllFS = haveCatchAllFS || fs.Name == flowcontrol.FlowSchemaNameCatchAll
	}
	// sort into the order to be used for matching
	sort.Sort(fsSeq)

	// Supply missing mandatory FlowSchemas, in correct position
	if !haveExemptFS {
		fsSeq = append(apihelpers.FlowSchemaSequence{fcboot.MandatoryFlowSchemaExempt}, fsSeq...)
	}
	if !haveCatchAllFS {
		fsSeq = append(fsSeq, fcboot.MandatoryFlowSchemaCatchAll)
	}

	meal.cfgCtlr.flowSchemas = fsSeq
	klogV := klog.V(5)
	if klogV.Enabled() {
		for _, fs := range fsSeq {
			klogV.Infof("Using FlowSchema %s", fcfmt.Fmt(fs))
		}
	}
}

// Consider all the priority levels in the previous configuration.
// Keep the ones that are in the new config, supply mandatory
// behavior, or are still busy; for the rest: drop it if it has no
// queues, otherwise start the quiescing process if that has not
// already been started.
func (meal *cfgMeal) processOldPLsLocked() {
	for plName, plState := range meal.cfgCtlr.priorityLevelStates {
		if meal.newPLStates[plName] != nil {
			// Still desired and already updated
			continue
		}
		if plName == flowcontrol.PriorityLevelConfigurationNameExempt && !meal.haveExemptPL || plName == flowcontrol.PriorityLevelConfigurationNameCatchAll && !meal.haveCatchAllPL {
			// BTW, we know the Spec has not changed what is says about queuing because the
			// mandatory objects have immutable Specs as far as queuing is concerned.
			klog.V(3).Infof("Retaining mandatory priority level %q despite lack of API object", plName)
		} else {
			if plState.numPending == 0 && plState.queues.IsIdle() {
				// The QueueSet is done
				// draining and no use is coming from another
				// goroutine
				klog.V(3).Infof("Removing undesired priority level %q, Type=%v", plName, plState.pl.Spec.Type)
				meal.cfgCtlr.MaxSeatsTracker.ForgetPriorityLevel(plName)
				continue
			}
			if !plState.quiescing {
				klog.V(3).Infof("Priority level %q became undesired", plName)
				plState.quiescing = true
			}
		}
		var err error
		plState.qsCompleter, err = queueSetCompleterForPL(meal.cfgCtlr.queueSetFactory, plState.queues,
			plState.pl, plState.reqsGaugePair, plState.execSeatsObs,
			metrics.NewUnionGauge(plState.seatDemandIntegrator, plState.seatDemandRatioedGauge))
		if err != nil {
			// This can not happen because queueSetCompleterForPL already approved this config
			panic(fmt.Sprintf("%s from name=%q spec=%s", err, plName, fcfmt.Fmt(plState.pl.Spec)))
		}
		// We deliberately include the lingering priority levels
		// here so that their queues get some concurrency and they
		// continue to drain.  During this interim a lingering
		// priority level continues to get a concurrency
		// allocation determined by all the share values in the
		// regular way.
		nominalConcurrencyShares, _, _ := plSpecCommons(plState.pl)
		meal.shareSum += float64(*nominalConcurrencyShares)
		meal.haveExemptPL = meal.haveExemptPL || plName == flowcontrol.PriorityLevelConfigurationNameExempt
		meal.haveCatchAllPL = meal.haveCatchAllPL || plName == flowcontrol.PriorityLevelConfigurationNameCatchAll
		meal.newPLStates[plName] = plState
	}
}

// For all the priority levels of the new config, divide up the
// server's total concurrency limit among them and create/update their
// QueueSets.
func (meal *cfgMeal) finishQueueSetReconfigsLocked() {
	for plName, plState := range meal.newPLStates {
		nominalConcurrencyShares, lendablePercent, borrowingLimitPercent := plSpecCommons(plState.pl)
		// The use of math.Ceil here means that the results might sum
		// to a little more than serverConcurrencyLimit but the
		// difference will be negligible.
		concurrencyLimit := int(math.Ceil(float64(meal.cfgCtlr.serverConcurrencyLimit) * float64(*nominalConcurrencyShares) / meal.shareSum))
		var lendableCL, borrowingCL int
		if lendablePercent != nil {
			lendableCL = int(math.Round(float64(concurrencyLimit) * float64(*lendablePercent) / 100))
		}
		if borrowingLimitPercent != nil {
			borrowingCL = int(math.Round(float64(concurrencyLimit) * float64(*borrowingLimitPercent) / 100))
		} else {
			borrowingCL = meal.cfgCtlr.serverConcurrencyLimit
		}

		metrics.SetPriorityLevelConfiguration(plName, concurrencyLimit, concurrencyLimit-lendableCL, concurrencyLimit+borrowingCL)
		cfgChanged := plState.nominalCL != concurrencyLimit || plState.minCL != concurrencyLimit-lendableCL || plState.maxCL != concurrencyLimit+borrowingCL
		plState.nominalCL = concurrencyLimit
		plState.minCL = concurrencyLimit - lendableCL
		plState.maxCL = concurrencyLimit + borrowingCL
		meal.maxExecutingRequests += concurrencyLimit
		if limited := plState.pl.Spec.Limited; limited != nil {
			if qCfg := limited.LimitResponse.Queuing; qCfg != nil {
				meal.maxWaitingRequests += int(qCfg.Queues * qCfg.QueueLengthLimit)

				// Max seats allocatable from work estimator is calculated as MAX(1, MIN(0.15 * nominalCL, nominalCL/handSize)).
				// This is to keep max seats relative to total available concurrency with a minimum value of 1.
				// 15% of nominal concurrency was chosen since it preserved the previous max seats of 10 for default priority levels
				// when using apiserver's default total server concurrency of 600 (--max-requests-inflight=400, --max-mutating-requests-inflight=200).
				// This ensures that clusters with relatively high inflight requests will continue to use a max seats of 10
				// while clusters with lower inflight requests will use max seats no greater than nominalCL/handSize.
				// Calculated max seats can return arbitrarily high values but work estimator currently limits max seats at 10.
				handSize := plState.pl.Spec.Limited.LimitResponse.Queuing.HandSize
				maxSeats := uint64(math.Max(1, math.Min(math.Ceil(float64(concurrencyLimit)*priorityLevelMaxSeatsPercent), float64(int32(concurrencyLimit)/handSize))))
				meal.cfgCtlr.MaxSeatsTracker.SetMaxSeats(plName, maxSeats)
			}
		}
		if plState.queues == nil {
			initialCL := concurrencyLimit - lendableCL/2
			klog.V(2).Infof("Introducing queues for priority level %q: config=%s, nominalCL=%d, lendableCL=%d, borrowingCL=%d, currentCL=%d, quiescing=%v (shares=%v, shareSum=%v)", plName, fcfmt.Fmt(plState.pl.Spec), concurrencyLimit, lendableCL, borrowingCL, initialCL, plState.quiescing, nominalConcurrencyShares, meal.shareSum)
			plState.seatDemandStats = seatDemandStats{}
			plState.currentCL = initialCL
		} else {
			logLevel := klog.Level(5)
			if cfgChanged {
				logLevel = 2
			}
			klog.V(logLevel).Infof("Retaining queues for priority level %q: config=%s, nominalCL=%d, lendableCL=%d, borrowingCL=%d, currentCL=%d, quiescing=%v, numPending=%d (shares=%v, shareSum=%v)", plName, fcfmt.Fmt(plState.pl.Spec), concurrencyLimit, lendableCL, borrowingCL, plState.currentCL, plState.quiescing, plState.numPending, nominalConcurrencyShares, meal.shareSum)
		}
	}
	meal.cfgCtlr.nominalCLSum = meal.maxExecutingRequests
	meal.cfgCtlr.updateBorrowingLocked(false, meal.newPLStates)
}

// queueSetCompleterForPL returns an appropriate QueueSetCompleter for the
// given priority level configuration.  Returns nil and an error if the given
// object is malformed in a way that is a problem for this package.
func queueSetCompleterForPL(qsf fq.QueueSetFactory, queues fq.QueueSet, pl *flowcontrol.PriorityLevelConfiguration, reqsIntPair metrics.RatioedGaugePair, execSeatsObs metrics.RatioedGauge, seatDemandGauge metrics.Gauge) (fq.QueueSetCompleter, error) {
	if (pl.Spec.Type == flowcontrol.PriorityLevelEnablementLimited) != (pl.Spec.Limited != nil) {
		return nil, errors.New("broken union structure at the top, for Limited")
	}
	if (pl.Spec.Type == flowcontrol.PriorityLevelEnablementExempt) != (pl.Spec.Exempt != nil) {
		return nil, errors.New("broken union structure at the top, for Exempt")
	}
	if (pl.Spec.Type == flowcontrol.PriorityLevelEnablementExempt) != (pl.Name == flowcontrol.PriorityLevelConfigurationNameExempt) {
		// This package does not attempt to cope with a priority level dynamically switching between exempt and not.
		return nil, errors.New("non-alignment between name and type")
	}
	qcQS := fq.QueuingConfig{Name: pl.Name}
	if pl.Spec.Limited != nil {
		if (pl.Spec.Limited.LimitResponse.Type == flowcontrol.LimitResponseTypeReject) != (pl.Spec.Limited.LimitResponse.Queuing == nil) {
			return nil, errors.New("broken union structure for limit response")
		}
		qcAPI := pl.Spec.Limited.LimitResponse.Queuing
		if qcAPI != nil {
			qcQS = fq.QueuingConfig{Name: pl.Name,
				DesiredNumQueues: int(qcAPI.Queues),
				QueueLengthLimit: int(qcAPI.QueueLengthLimit),
				HandSize:         int(qcAPI.HandSize),
			}
		}
	} else {
		qcQS = fq.QueuingConfig{Name: pl.Name, DesiredNumQueues: -1}
	}
	var qsc fq.QueueSetCompleter
	var err error
	if queues != nil {
		qsc, err = queues.BeginConfigChange(qcQS)
	} else {
		qsc, err = qsf.BeginConstruction(qcQS, reqsIntPair, execSeatsObs, seatDemandGauge)
	}
	if err != nil {
		err = fmt.Errorf("priority level %q has QueuingConfiguration %#+v, which is invalid: %w", pl.Name, qcQS, err)
	}
	return qsc, err
}

func (meal *cfgMeal) presyncFlowSchemaStatus(fs *flowcontrol.FlowSchema, isDangling bool, plName string) {
	danglingCondition := apihelpers.GetFlowSchemaConditionByType(fs, flowcontrol.FlowSchemaConditionDangling)
	if danglingCondition == nil {
		danglingCondition = &flowcontrol.FlowSchemaCondition{
			Type: flowcontrol.FlowSchemaConditionDangling,
		}
	}
	desiredStatus := flowcontrol.ConditionFalse
	var desiredReason, desiredMessage string
	if isDangling {
		desiredStatus = flowcontrol.ConditionTrue
		desiredReason = "NotFound"
		desiredMessage = fmt.Sprintf("This FlowSchema references the PriorityLevelConfiguration object named %q but there is no such object", plName)
	} else {
		desiredReason = "Found"
		desiredMessage = fmt.Sprintf("This FlowSchema references the PriorityLevelConfiguration object named %q and it exists", plName)
	}
	if danglingCondition.Status == desiredStatus && danglingCondition.Reason == desiredReason && danglingCondition.Message == desiredMessage {
		return
	}
	now := meal.cfgCtlr.clock.Now()
	meal.fsStatusUpdates = append(meal.fsStatusUpdates, fsStatusUpdate{
		flowSchema: fs,
		condition: flowcontrol.FlowSchemaCondition{
			Type:               flowcontrol.FlowSchemaConditionDangling,
			Status:             desiredStatus,
			LastTransitionTime: metav1.NewTime(now),
			Reason:             desiredReason,
			Message:            desiredMessage,
		},
		oldValue: *danglingCondition})
}

// imaginePL adds a priority level based on one of the mandatory ones
// that does not actually exist (right now) as a real API object.
func (meal *cfgMeal) imaginePL(proto *flowcontrol.PriorityLevelConfiguration) {
	klog.V(3).Infof("No %s PriorityLevelConfiguration found, imagining one", proto.Name)
	labelValues := []string{proto.Name}
	reqsGaugePair := metrics.RatioedGaugeVecPhasedElementPair(meal.cfgCtlr.reqsGaugeVec, 1, 1, labelValues)
	execSeatsObs := meal.cfgCtlr.execSeatsGaugeVec.NewForLabelValuesSafe(0, 1, labelValues)
	seatDemandIntegrator := fq.NewNamedIntegrator(meal.cfgCtlr.clock, proto.Name)
	seatDemandRatioedGauge := metrics.ApiserverSeatDemands.NewForLabelValuesSafe(0, 1, []string{proto.Name})
	qsCompleter, err := queueSetCompleterForPL(meal.cfgCtlr.queueSetFactory, nil, proto, reqsGaugePair,
		execSeatsObs, metrics.NewUnionGauge(seatDemandIntegrator, seatDemandRatioedGauge))
	if err != nil {
		// This can not happen because proto is one of the mandatory
		// objects and these are not erroneous
		panic(err)
	}
	meal.newPLStates[proto.Name] = &priorityLevelState{
		pl:                     proto,
		qsCompleter:            qsCompleter,
		reqsGaugePair:          reqsGaugePair,
		execSeatsObs:           execSeatsObs,
		seatDemandIntegrator:   seatDemandIntegrator,
		seatDemandRatioedGauge: seatDemandRatioedGauge,
	}
	nominalConcurrencyShares, _, _ := plSpecCommons(proto)
	meal.shareSum += float64(*nominalConcurrencyShares)
}

// startRequest classifies and, if appropriate, enqueues the request.
// Returns a nil Request if and only if the request is to be rejected.
// The returned bool indicates whether the request is exempt from
// limitation.  The startWaitingTime is when the request started
// waiting in its queue, or `Time{}` if this did not happen.
func (cfgCtlr *configController) startRequest(ctx context.Context, rd RequestDigest,
	noteFn func(fs *flowcontrol.FlowSchema, pl *flowcontrol.PriorityLevelConfiguration, flowDistinguisher string),
	workEstimator func() fcrequest.WorkEstimate,
	queueNoteFn fq.QueueNoteFn) (fs *flowcontrol.FlowSchema, pl *flowcontrol.PriorityLevelConfiguration, isExempt bool, req fq.Request, startWaitingTime time.Time) {
	klog.V(7).Infof("startRequest(%#+v)", rd)
	cfgCtlr.lock.RLock()
	defer cfgCtlr.lock.RUnlock()
	var selectedFlowSchema, catchAllFlowSchema *flowcontrol.FlowSchema
	for _, fs := range cfgCtlr.flowSchemas {
		if matchesFlowSchema(rd, fs) {
			selectedFlowSchema = fs
			break
		}
		if fs.Name == flowcontrol.FlowSchemaNameCatchAll {
			catchAllFlowSchema = fs
		}
	}
	if selectedFlowSchema == nil {
		// This should never happen. If the requestDigest's User is a part of
		// system:authenticated or system:unauthenticated, the catch-all flow
		// schema should match it. However, if that invariant somehow fails,
		// fallback to the catch-all flow schema anyway.
		if catchAllFlowSchema == nil {
			// This should absolutely never, ever happen! APF guarantees two
			// undeletable flow schemas at all times: an exempt flow schema and a
			// catch-all flow schema.
			panic(fmt.Sprintf("no fallback catch-all flow schema found for request %#+v and user %#+v", rd.RequestInfo, rd.User))
		}
		selectedFlowSchema = catchAllFlowSchema
		klog.Warningf("no match found for request %#+v and user %#+v; selecting catchAll=%s as fallback flow schema", rd.RequestInfo, rd.User, fcfmt.Fmt(selectedFlowSchema))
	}
	plName := selectedFlowSchema.Spec.PriorityLevelConfiguration.Name
	plState := cfgCtlr.priorityLevelStates[plName]
	var numQueues int32
	var hashValue uint64
	var flowDistinguisher string
	if plState.pl.Spec.Type != flowcontrol.PriorityLevelEnablementExempt {
		if plState.pl.Spec.Limited.LimitResponse.Type == flowcontrol.LimitResponseTypeQueue {
			numQueues = plState.pl.Spec.Limited.LimitResponse.Queuing.Queues
		}
		if numQueues > 1 {
			flowDistinguisher = computeFlowDistinguisher(rd, selectedFlowSchema.Spec.DistinguisherMethod)
			hashValue = hashFlowID(selectedFlowSchema.Name, flowDistinguisher)
		}
	}

	noteFn(selectedFlowSchema, plState.pl, flowDistinguisher)
	workEstimate := workEstimator()

	if plState.pl.Spec.Type != flowcontrol.PriorityLevelEnablementExempt {
		startWaitingTime = cfgCtlr.clock.Now()
	}
	klog.V(7).Infof("startRequest(%#+v) => fsName=%q, distMethod=%#+v, plName=%q, numQueues=%d", rd, selectedFlowSchema.Name, selectedFlowSchema.Spec.DistinguisherMethod, plName, numQueues)
	req, idle := plState.queues.StartRequest(ctx, &workEstimate, hashValue, flowDistinguisher, selectedFlowSchema.Name, rd.RequestInfo, rd.User, queueNoteFn)
	if idle {
		cfgCtlr.maybeReapReadLocked(plName, plState)
	}
	return selectedFlowSchema, plState.pl, plState.pl.Spec.Type == flowcontrol.PriorityLevelEnablementExempt, req, startWaitingTime
}

// maybeReap will remove the last internal traces of the named
// priority level if it has no more use.  Call this after getting a
// clue that the given priority level is undesired and idle.
func (cfgCtlr *configController) maybeReap(plName string) {
	cfgCtlr.lock.RLock()
	defer cfgCtlr.lock.RUnlock()
	plState := cfgCtlr.priorityLevelStates[plName]
	if plState == nil {
		klog.V(7).Infof("plName=%s, plState==nil", plName)
		return
	}
	useless := plState.quiescing && plState.numPending == 0 && plState.queues.IsIdle()
	klog.V(7).Infof("plState.quiescing=%v, plState.numPending=%d, useless=%v", plState.quiescing, plState.numPending, useless)
	if !useless {
		return
	}
	klog.V(3).Infof("Triggered API priority and fairness config reloading because priority level %s is undesired and idle", plName)
	cfgCtlr.configQueue.Add(0)
}

// maybeReapLocked requires the cfgCtlr's lock to already be held and
// will remove the last internal traces of the named priority level if
// it has no more use.  Call this if both (1) plState.queues is
// non-nil and reported being idle, and (2) cfgCtlr's lock has not
// been released since then.
func (cfgCtlr *configController) maybeReapReadLocked(plName string, plState *priorityLevelState) {
	if !(plState.quiescing && plState.numPending == 0) {
		return
	}
	klog.V(3).Infof("Triggered API priority and fairness config reloading because priority level %s is undesired and idle", plName)
	cfgCtlr.configQueue.Add(0)
}

// computeFlowDistinguisher extracts the flow distinguisher according to the given method
func computeFlowDistinguisher(rd RequestDigest, method *flowcontrol.FlowDistinguisherMethod) string {
	if method == nil {
		return ""
	}
	switch method.Type {
	case flowcontrol.FlowDistinguisherMethodByUserType:
		return rd.User.GetName()
	case flowcontrol.FlowDistinguisherMethodByNamespaceType:
		return rd.RequestInfo.Namespace
	default:
		// this line shall never reach
		panic("invalid flow-distinguisher method")
	}
}

func hashFlowID(fsName, fDistinguisher string) uint64 {
	hash := sha256.New()
	var sep = [1]byte{0}
	hash.Write([]byte(fsName))
	hash.Write(sep[:])
	hash.Write([]byte(fDistinguisher))
	var sum [32]byte
	hash.Sum(sum[:0])
	return binary.LittleEndian.Uint64(sum[:8])
}

func relDiff(x, y float64) float64 {
	diff := math.Abs(x - y)
	den := math.Max(math.Abs(x), math.Abs(y))
	if den == 0 {
		return 0
	}
	return diff / den
}

// plSpecCommons returns the (NominalConcurrencyShares, LendablePercent, BorrowingLimitPercent) of the given priority level config
func plSpecCommons(pl *flowcontrol.PriorityLevelConfiguration) (*int32, *int32, *int32) {
	if limiter := pl.Spec.Limited; limiter != nil {
		return limiter.NominalConcurrencyShares, limiter.LendablePercent, limiter.BorrowingLimitPercent
	}
	limiter := pl.Spec.Exempt
	var nominalConcurrencyShares int32
	if limiter.NominalConcurrencyShares != nil {
		nominalConcurrencyShares = *limiter.NominalConcurrencyShares
	}
	return &nominalConcurrencyShares, limiter.LendablePercent, nil
}
