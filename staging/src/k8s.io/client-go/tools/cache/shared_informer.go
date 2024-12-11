/*
Copyright 2015 The Kubernetes Authors.

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

package cache

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/naming"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache/synctrack"
	"k8s.io/utils/buffer"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"

	"k8s.io/klog/v2"

	clientgofeaturegate "k8s.io/client-go/features"
)

// SharedInformer provides eventually consistent linkage of its
// clients to the authoritative state of a given collection of
// objects.  An object is identified by its API group, kind/resource,
// namespace (if any), and name; the `ObjectMeta.UID` is not part of
// an object's ID as far as this contract is concerned.  One
// SharedInformer provides linkage to objects of a particular API
// group and kind/resource.  The linked object collection of a
// SharedInformer may be further restricted to one namespace (if
// applicable) and/or by label selector and/or field selector.
//
// The authoritative state of an object is what apiservers provide
// access to, and an object goes through a strict sequence of states.
// An object state is either (1) present with a ResourceVersion and
// other appropriate content or (2) "absent".
//
// A SharedInformer maintains a local cache --- exposed by GetStore(),
// by GetIndexer() in the case of an indexed informer, and possibly by
// machinery involved in creating and/or accessing the informer --- of
// the state of each relevant object.  This cache is eventually
// consistent with the authoritative state.  This means that, unless
// prevented by persistent communication problems, if ever a
// particular object ID X is authoritatively associated with a state S
// then for every SharedInformer I whose collection includes (X, S)
// eventually either (1) I's cache associates X with S or a later
// state of X, (2) I is stopped, or (3) the authoritative state
// service for X terminates.  To be formally complete, we say that the
// absent state meets any restriction by label selector or field
// selector.
//
// For a given informer and relevant object ID X, the sequence of
// states that appears in the informer's cache is a subsequence of the
// states authoritatively associated with X.  That is, some states
// might never appear in the cache but ordering among the appearing
// states is correct.  Note, however, that there is no promise about
// ordering between states seen for different objects.
//
// The local cache starts out empty, and gets populated and updated
// during `Run()`.
//
// As a simple example, if a collection of objects is henceforth
// unchanging, a SharedInformer is created that links to that
// collection, and that SharedInformer is `Run()` then that
// SharedInformer's cache eventually holds an exact copy of that
// collection (unless it is stopped too soon, the authoritative state
// service ends, or communication problems between the two
// persistently thwart achievement).
//
// As another simple example, if the local cache ever holds a
// non-absent state for some object ID and the object is eventually
// removed from the authoritative state then eventually the object is
// removed from the local cache (unless the SharedInformer is stopped
// too soon, the authoritative state service ends, or communication
// problems persistently thwart the desired result).
//
// The keys in the Store are of the form namespace/name for namespaced
// objects, and are simply the name for non-namespaced objects.
// Clients can use `MetaNamespaceKeyFunc(obj)` to extract the key for
// a given object, and `SplitMetaNamespaceKey(key)` to split a key
// into its constituent parts.
//
// Every query against the local cache is answered entirely from one
// snapshot of the cache's state.  Thus, the result of a `List` call
// will not contain two entries with the same namespace and name.
//
// A client is identified here by a ResourceEventHandler.  For every
// update to the SharedInformer's local cache and for every client
// added before `Run()`, eventually either the SharedInformer is
// stopped or the client is notified of the update.  A client added
// after `Run()` starts gets a startup batch of notifications of
// additions of the objects existing in the cache at the time that
// client was added; also, for every update to the SharedInformer's
// local cache after that client was added, eventually either the
// SharedInformer is stopped or that client is notified of that
// update.  Client notifications happen after the corresponding cache
// update and, in the case of a SharedIndexInformer, after the
// corresponding index updates.  It is possible that additional cache
// and index updates happen before such a prescribed notification.
// For a given SharedInformer and client, the notifications are
// delivered sequentially.  For a given SharedInformer, client, and
// object ID, the notifications are delivered in order.  Because
// `ObjectMeta.UID` has no role in identifying objects, it is possible
// that when (1) object O1 with ID (e.g. namespace and name) X and
// `ObjectMeta.UID` U1 in the SharedInformer's local cache is deleted
// and later (2) another object O2 with ID X and ObjectMeta.UID U2 is
// created the informer's clients are not notified of (1) and (2) but
// rather are notified only of an update from O1 to O2. Clients that
// need to detect such cases might do so by comparing the `ObjectMeta.UID`
// field of the old and the new object in the code that handles update
// notifications (i.e. `OnUpdate` method of ResourceEventHandler).
//
// A client must process each notification promptly; a SharedInformer
// is not engineered to deal well with a large backlog of
// notifications to deliver.  Lengthy processing should be passed off
// to something else, for example through a
// `client-go/util/workqueue`.
//
// A delete notification exposes the last locally known non-absent
// state, except that its ResourceVersion is replaced with a
// ResourceVersion in which the object is actually absent.
type SharedInformer interface {
	// AddEventHandler adds an event handler to the shared informer using
	// the shared informer's resync period.  Events to a single handler are
	// delivered sequentially, but there is no coordination between
	// different handlers.
	// It returns a registration handle for the handler that can be used to
	// remove the handler again, or to tell if the handler is synced (has
	// seen every item in the initial list).
	//
	// Contextual logging: AddEventHandlerWithOptions together with a logger in the options should be used instead of AddEventHandler in code which supports contextual logging.
	AddEventHandler(handler ResourceEventHandler) (ResourceEventHandlerRegistration, error)
	// AddEventHandlerWithResyncPeriod adds an event handler to the
	// shared informer with the requested resync period; zero means
	// this handler does not care about resyncs.  The resync operation
	// consists of delivering to the handler an update notification
	// for every object in the informer's local cache; it does not add
	// any interactions with the authoritative storage.  Some
	// informers do no resyncs at all, not even for handlers added
	// with a non-zero resyncPeriod.  For an informer that does
	// resyncs, and for each handler that requests resyncs, that
	// informer develops a nominal resync period that is no shorter
	// than the requested period but may be longer.  The actual time
	// between any two resyncs may be longer than the nominal period
	// because the implementation takes time to do work and there may
	// be competing load and scheduling noise.
	// It returns a registration handle for the handler that can be used to remove
	// the handler again and an error if the handler cannot be added.
	//
	// Contextual logging: AddEventHandlerWithOptions together with a logger in the options should be used instead of AddEventHandlerWithResyncPeriod in code which supports contextual logging.
	AddEventHandlerWithResyncPeriod(handler ResourceEventHandler, resyncPeriod time.Duration) (ResourceEventHandlerRegistration, error)
	// AddEventHandlerWithOptions is a variant of AddEventHandlerWithResyncPeriod where
	// all optional parameters are passed in a struct.
	AddEventHandlerWithOptions(handler ResourceEventHandler, options HandlerOptions) (ResourceEventHandlerRegistration, error)
	// RemoveEventHandler removes a formerly added event handler given by
	// its registration handle.
	// This function is guaranteed to be idempotent, and thread-safe.
	RemoveEventHandler(handle ResourceEventHandlerRegistration) error
	// GetStore returns the informer's local cache as a Store.
	GetStore() Store
	// GetController is deprecated, it does nothing useful
	GetController() Controller
	// Run starts and runs the shared informer, returning after it stops.
	// The informer will be stopped when stopCh is closed.
	//
	// Contextual logging: RunWithContext should be used instead of Run in code which uses contextual logging.
	Run(stopCh <-chan struct{})
	// RunWithContext starts and runs the shared informer, returning after it stops.
	// The informer will be stopped when the context is canceled.
	RunWithContext(ctx context.Context)
	// HasSynced returns true if the shared informer's store has been
	// informed by at least one full LIST of the authoritative state
	// of the informer's object collection.  This is unrelated to "resync".
	//
	// Note that this doesn't tell you if an individual handler is synced!!
	// For that, please call HasSynced on the handle returned by
	// AddEventHandler.
	HasSynced() bool
	// LastSyncResourceVersion is the resource version observed when last synced with the underlying
	// store. The value returned is not synchronized with access to the underlying store and is not
	// thread-safe.
	LastSyncResourceVersion() string

	// The WatchErrorHandler is called whenever ListAndWatch drops the
	// connection with an error. After calling this handler, the informer
	// will backoff and retry.
	//
	// The default implementation looks at the error type and tries to log
	// the error message at an appropriate level.
	//
	// There's only one handler, so if you call this multiple times, last one
	// wins; calling after the informer has been started returns an error.
	//
	// The handler is intended for visibility, not to e.g. pause the consumers.
	// The handler should return quickly - any expensive processing should be
	// offloaded.
	//
	// Contextual logging: SetWatchErrorHandlerWithContext should be used instead of SetWatchErrorHandler in code which supports contextual logging.
	SetWatchErrorHandler(handler WatchErrorHandler) error

	// SetWatchErrorHandlerWithContext is a variant of SetWatchErrorHandler where
	// the handler is passed an additional context parameter.
	SetWatchErrorHandlerWithContext(handler WatchErrorHandlerWithContext) error

	// The TransformFunc is called for each object which is about to be stored.
	//
	// This function is intended for you to take the opportunity to
	// remove, transform, or normalize fields. One use case is to strip unused
	// metadata fields out of objects to save on RAM cost.
	//
	// Must be set before starting the informer.
	//
	// Please see the comment on TransformFunc for more details.
	SetTransform(handler TransformFunc) error

	// IsStopped reports whether the informer has already been stopped.
	// Adding event handlers to already stopped informers is not possible.
	// An informer already stopped will never be started again.
	IsStopped() bool
}

// Opaque interface representing the registration of ResourceEventHandler for
// a SharedInformer. Must be supplied back to the same SharedInformer's
// `RemoveEventHandler` to unregister the handlers.
//
// Also used to tell if the handler is synced (has had all items in the initial
// list delivered).
type ResourceEventHandlerRegistration interface {
	// HasSynced reports if both the parent has synced and all pre-sync
	// events have been delivered.
	HasSynced() bool
}

// Optional configuration options for [SharedInformer.AddEventHandlerWithOptions].
// May be left empty.
type HandlerOptions struct {
	// Logger overrides the default klog.Background() logger.
	Logger *klog.Logger

	// ResyncPeriod requests a certain resync period from an informer. Zero
	// means the handler does not care about resyncs. Not all informers do
	// resyncs, even if requested. See
	// [SharedInformer.AddEventHandlerWithResyncPeriod] for details.
	//
	// If nil, the default resync period of the shared informer is used.
	ResyncPeriod *time.Duration
}

// SharedIndexInformer provides add and get Indexers ability based on SharedInformer.
type SharedIndexInformer interface {
	SharedInformer
	// AddIndexers add indexers to the informer before it starts.
	AddIndexers(indexers Indexers) error
	GetIndexer() Indexer
}

// NewSharedInformer creates a new instance for the ListerWatcher. See NewSharedIndexInformerWithOptions for full details.
func NewSharedInformer(lw ListerWatcher, exampleObject runtime.Object, defaultEventHandlerResyncPeriod time.Duration) SharedInformer {
	return NewSharedIndexInformer(lw, exampleObject, defaultEventHandlerResyncPeriod, Indexers{})
}

// NewSharedIndexInformer creates a new instance for the ListerWatcher and specified Indexers. See
// NewSharedIndexInformerWithOptions for full details.
func NewSharedIndexInformer(lw ListerWatcher, exampleObject runtime.Object, defaultEventHandlerResyncPeriod time.Duration, indexers Indexers) SharedIndexInformer {
	return NewSharedIndexInformerWithOptions(
		lw,
		exampleObject,
		SharedIndexInformerOptions{
			ResyncPeriod: defaultEventHandlerResyncPeriod,
			Indexers:     indexers,
		},
	)
}

// NewSharedIndexInformerWithOptions creates a new instance for the ListerWatcher.
// The created informer will not do resyncs if options.ResyncPeriod is zero.  Otherwise: for each
// handler that with a non-zero requested resync period, whether added
// before or after the informer starts, the nominal resync period is
// the requested resync period rounded up to a multiple of the
// informer's resync checking period.  Such an informer's resync
// checking period is established when the informer starts running,
// and is the maximum of (a) the minimum of the resync periods
// requested before the informer starts and the
// options.ResyncPeriod given here and (b) the constant
// `minimumResyncPeriod` defined in this file.
func NewSharedIndexInformerWithOptions(lw ListerWatcher, exampleObject runtime.Object, options SharedIndexInformerOptions) SharedIndexInformer {
	realClock := &clock.RealClock{}

	informerName := naming.GetNameFromCallsite(internalPackages...)
	metricName := makeValidPromethusMetricName(fmt.Sprintf("informer_%s_objectType_%s", informerName, reflect.TypeOf(exampleObject).String()))

	return &sharedIndexInformer{
		name:                            informerName,
		metrics:                         newInformerMetrics(metricName),
		indexer:                         NewIndexer(DeletionHandlingMetaNamespaceKeyFunc, options.Indexers),
		processor:                       &sharedProcessor{clock: realClock},
		listerWatcher:                   lw,
		objectType:                      exampleObject,
		objectDescription:               options.ObjectDescription,
		resyncCheckPeriod:               options.ResyncPeriod,
		defaultEventHandlerResyncPeriod: options.ResyncPeriod,
		clock:                           realClock,
		cacheMutationDetector:           NewCacheMutationDetector(fmt.Sprintf("%T", exampleObject)),
	}
}

// SharedIndexInformerOptions configures a sharedIndexInformer.
type SharedIndexInformerOptions struct {
	// ResyncPeriod is the default event handler resync period and resync check
	// period. If unset/unspecified, these are defaulted to 0 (do not resync).
	ResyncPeriod time.Duration

	// Indexers is the sharedIndexInformer's indexers. If unset/unspecified, no indexers are configured.
	Indexers Indexers

	// ObjectDescription is the sharedIndexInformer's object description. This is passed through to the
	// underlying Reflector's type description.
	ObjectDescription string
}

// InformerSynced is a function that can be used to determine if an informer has synced.  This is useful for determining if caches have synced.
type InformerSynced func() bool

const (
	// syncedPollPeriod controls how often you look at the status of your sync funcs
	syncedPollPeriod = 100 * time.Millisecond

	// initialBufferSize is the initial number of event notifications that can be buffered.
	initialBufferSize = 1024
)

// WaitForNamedCacheSync is a wrapper around WaitForCacheSync that generates log messages
// indicating that the caller identified by name is waiting for syncs, followed by
// either a successful or failed sync.
//
// Contextual logging: WaitForNamedCacheSyncWithContext should be used instead of WaitForNamedCacheSync in code which supports contextual logging.
func WaitForNamedCacheSync(controllerName string, stopCh <-chan struct{}, cacheSyncs ...InformerSynced) bool {
	klog.Background().Info("Waiting for caches to sync", "controller", controllerName)

	if !WaitForCacheSync(stopCh, cacheSyncs...) {
		utilruntime.HandleErrorWithContext(context.Background(), nil, "Unable to sync caches", "controller", controllerName)
		return false
	}

	klog.Background().Info("Caches are synced", "controller", controllerName)
	return true
}

// WaitForNamedCacheSyncWithContext is a wrapper around WaitForCacheSyncWithContext that generates log messages
// indicating that the caller is waiting for syncs, followed by either a successful or failed sync.
//
// Contextual logging can be used to identify the caller in those log messages. The log level is zero,
// the same as in [WaitForNamedCacheSync]. If this is too verbose, then store a logger with an increased
// threshold in the context:
//
//	WaitForNamedCacheSyncWithContext(klog.NewContext(ctx, logger.V(5)), ...)
func WaitForNamedCacheSyncWithContext(ctx context.Context, cacheSyncs ...InformerSynced) bool {
	logger := klog.FromContext(ctx)
	logger.Info("Waiting for caches to sync")

	if !WaitForCacheSync(ctx.Done(), cacheSyncs...) {
		utilruntime.HandleErrorWithContext(ctx, nil, "Unable to sync caches")
		return false
	}

	logger.Info("Caches are synced")
	return true
}

// WaitForCacheSync waits for caches to populate.  It returns true if it was successful, false
// if the controller should shutdown
// callers should prefer WaitForNamedCacheSync()
func WaitForCacheSync(stopCh <-chan struct{}, cacheSyncs ...InformerSynced) bool {
	err := wait.PollImmediateUntil(syncedPollPeriod,
		func() (bool, error) {
			for _, syncFunc := range cacheSyncs {
				if !syncFunc() {
					return false, nil
				}
			}
			return true, nil
		},
		stopCh)
	if err != nil {
		return false
	}

	return true
}

// `*sharedIndexInformer` implements SharedIndexInformer and has three
// main components.  One is an indexed local cache, `indexer Indexer`.
// The second main component is a Controller that pulls
// objects/notifications using the ListerWatcher and pushes them into
// a DeltaFIFO --- whose knownObjects is the informer's local cache
// --- while concurrently Popping Deltas values from that fifo and
// processing them with `sharedIndexInformer::HandleDeltas`.  Each
// invocation of HandleDeltas, which is done with the fifo's lock
// held, processes each Delta in turn.  For each Delta this both
// updates the local cache and stuffs the relevant notification into
// the sharedProcessor.  The third main component is that
// sharedProcessor, which is responsible for relaying those
// notifications to each of the informer's clients.
type sharedIndexInformer struct {
	name string

	indexer    Indexer
	controller Controller

	processor             *sharedProcessor
	cacheMutationDetector MutationDetector

	listerWatcher ListerWatcher

	// objectType is an example object of the type this informer is expected to handle. If set, an event
	// with an object with a mismatching type is dropped instead of being delivered to listeners.
	objectType runtime.Object

	// objectDescription is the description of this informer's objects. This typically defaults to
	objectDescription string

	// resyncCheckPeriod is how often we want the reflector's resync timer to fire so it can call
	// shouldResync to check if any of our listeners need a resync.
	resyncCheckPeriod time.Duration
	// defaultEventHandlerResyncPeriod is the default resync period for any handlers added via
	// AddEventHandler (i.e. they don't specify one and just want to use the shared informer's default
	// value).
	defaultEventHandlerResyncPeriod time.Duration
	// clock allows for testability
	clock clock.Clock

	started, stopped bool
	startedLock      sync.Mutex

	// blockDeltas gives a way to stop all event distribution so that a late event handler
	// can safely join the shared informer.
	blockDeltas sync.Mutex

	// Called whenever the ListAndWatch drops the connection with an error.
	watchErrorHandler WatchErrorHandlerWithContext

	transform TransformFunc

	// metrics tracks basic metric information about the informer.
	metrics *informerMetrics
}

// dummyController hides the fact that a SharedInformer is different from a dedicated one
// where a caller can `Run`.  The run method is disconnected in this case, because higher
// level logic will decide when to start the SharedInformer and related controller.
// Because returning information back is always asynchronous, the legacy callers shouldn't
// notice any change in behavior.
type dummyController struct {
	informer *sharedIndexInformer
}

func (v *dummyController) RunWithContext(context.Context) {
}

func (v *dummyController) Run(stopCh <-chan struct{}) {
}

func (v *dummyController) HasSynced() bool {
	return v.informer.HasSynced()
}

func (v *dummyController) LastSyncResourceVersion() string {
	if clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.InformerResourceVersion) {
		return v.informer.LastSyncResourceVersion()
	}

	return ""
}

type updateNotification struct {
	oldObj interface{}
	newObj interface{}
}

type addNotification struct {
	newObj          interface{}
	isInInitialList bool
}

type deleteNotification struct {
	oldObj interface{}
}

func (s *sharedIndexInformer) SetWatchErrorHandler(handler WatchErrorHandler) error {
	return s.SetWatchErrorHandlerWithContext(func(_ context.Context, r *Reflector, err error) {
		handler(r, err)
	})
}

func (s *sharedIndexInformer) SetWatchErrorHandlerWithContext(handler WatchErrorHandlerWithContext) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.started {
		return fmt.Errorf("informer has already started")
	}

	s.watchErrorHandler = handler
	return nil
}

func (s *sharedIndexInformer) SetTransform(handler TransformFunc) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.started {
		return fmt.Errorf("informer has already started")
	}

	s.transform = handler
	return nil
}

func (s *sharedIndexInformer) Run(stopCh <-chan struct{}) {
	s.RunWithContext(wait.ContextForChannel(stopCh))
}

func (s *sharedIndexInformer) RunWithContext(ctx context.Context) {
	defer utilruntime.HandleCrashWithContext(ctx)
	logger := klog.FromContext(ctx)

	if s.HasStarted() {
		logger.Info("Warning: the sharedIndexInformer has started, run more than once is not allowed")
		return
	}

	func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()

		var fifo Queue
		if clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.InOrderInformers) {
			fifo = NewRealFIFO(MetaNamespaceKeyFunc, s.indexer, s.transform)
		} else {
			fifo = NewDeltaFIFOWithOptions(DeltaFIFOOptions{
				KnownObjects:          s.indexer,
				EmitDeltaTypeReplaced: true,
				Transformer:           s.transform,
				Metrics:               s.metrics,
			})
		}

		cfg := &Config{
			Queue:             fifo,
			ListerWatcher:     s.listerWatcher,
			ObjectType:        s.objectType,
			ObjectDescription: s.objectDescription,
			FullResyncPeriod:  s.resyncCheckPeriod,
			ShouldResync:      s.processor.shouldResync,

			Process:                      s.HandleDeltas,
			WatchErrorHandlerWithContext: s.watchErrorHandler,
		}

		s.controller = New(cfg)
		s.controller.(*controller).clock = s.clock
		s.started = true
	}()

	// Separate stop context because Processor should be stopped strictly after controller.
	// Cancelation in the parent context is ignored and all values are passed on,
	// including - but not limited to - a logger.
	processorStopCtx, stopProcessor := context.WithCancelCause(context.WithoutCancel(ctx))
	var wg wait.Group
	defer wg.Wait()                                         // Wait for Processor to stop
	defer stopProcessor(errors.New("informer is stopping")) // Tell Processor to stop
	// TODO: extend the MutationDetector interface so that it optionally
	// has a RunWithContext method that we can use here.
	wg.StartWithChannel(processorStopCtx.Done(), s.cacheMutationDetector.Run)
	wg.StartWithContext(processorStopCtx, s.processor.run)

	defer func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()
		s.stopped = true // Don't want any new listeners
	}()
	s.controller.RunWithContext(ctx)
}

func (s *sharedIndexInformer) HasStarted() bool {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()
	return s.started
}

func (s *sharedIndexInformer) HasSynced() bool {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.controller == nil {
		return false
	}
	return s.controller.HasSynced()
}

func (s *sharedIndexInformer) LastSyncResourceVersion() string {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.controller == nil {
		return ""
	}
	return s.controller.LastSyncResourceVersion()
}

func (s *sharedIndexInformer) GetStore() Store {
	return s.indexer
}

func (s *sharedIndexInformer) GetIndexer() Indexer {
	return s.indexer
}

func (s *sharedIndexInformer) AddIndexers(indexers Indexers) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.stopped {
		return fmt.Errorf("indexer was not added because it has stopped already")
	}

	return s.indexer.AddIndexers(indexers)
}

func (s *sharedIndexInformer) GetController() Controller {
	return &dummyController{informer: s}
}

func (s *sharedIndexInformer) AddEventHandler(handler ResourceEventHandler) (ResourceEventHandlerRegistration, error) {
	return s.AddEventHandlerWithOptions(handler, HandlerOptions{})
}

func determineResyncPeriod(logger klog.Logger, desired, check time.Duration) time.Duration {
	if desired == 0 {
		return desired
	}
	if check == 0 {
		logger.Info("Warning: the specified resyncPeriod is invalid because this shared informer doesn't support resyncing", "desired", desired)
		return 0
	}
	if desired < check {
		logger.Info("Warning: the specified resyncPeriod is being increased to the minimum resyncCheckPeriod", "desired", desired, "resyncCheckPeriod", check)
		return check
	}
	return desired
}

const minimumResyncPeriod = 1 * time.Second

func (s *sharedIndexInformer) AddEventHandlerWithResyncPeriod(handler ResourceEventHandler, resyncPeriod time.Duration) (ResourceEventHandlerRegistration, error) {
	return s.AddEventHandlerWithOptions(handler, HandlerOptions{ResyncPeriod: &resyncPeriod})
}

func (s *sharedIndexInformer) AddEventHandlerWithOptions(handler ResourceEventHandler, options HandlerOptions) (ResourceEventHandlerRegistration, error) {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.stopped {
		return nil, fmt.Errorf("handler %v was not added to shared informer because it has stopped already", handler)
	}

	logger := ptr.Deref(options.Logger, klog.Background())
	resyncPeriod := ptr.Deref(options.ResyncPeriod, s.defaultEventHandlerResyncPeriod)
	if resyncPeriod > 0 {
		if resyncPeriod < minimumResyncPeriod {
			logger.Info("Warning: resync period is too small. Changing it to the minimum allowed value", "resyncPeriod", resyncPeriod, "minimumResyncPeriod", minimumResyncPeriod)
			resyncPeriod = minimumResyncPeriod
		}

		if resyncPeriod < s.resyncCheckPeriod {
			if s.started {
				logger.Info("Warning: resync period is smaller than resync check period and the informer has already started. Changing it to the resync check period", "resyncPeriod", resyncPeriod, "resyncCheckPeriod", s.resyncCheckPeriod)

				resyncPeriod = s.resyncCheckPeriod
			} else {
				// if the event handler's resyncPeriod is smaller than the current resyncCheckPeriod, update
				// resyncCheckPeriod to match resyncPeriod and adjust the resync periods of all the listeners
				// accordingly
				s.resyncCheckPeriod = resyncPeriod
				s.processor.resyncCheckPeriodChanged(logger, resyncPeriod)
			}
		}
	}

	listener := newProcessListener(logger, s.name, handler, resyncPeriod, determineResyncPeriod(logger, resyncPeriod, s.resyncCheckPeriod), s.clock.Now(), initialBufferSize, s.HasSynced)

	if !s.started {
		return s.processor.addListener(listener), nil
	}

	// in order to safely join, we have to
	// 1. stop sending add/update/delete notifications
	// 2. do a list against the store
	// 3. send synthetic "Add" events to the new handler
	// 4. unblock
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()

	handle := s.processor.addListener(listener)
	for _, item := range s.indexer.List() {
		// Note that we enqueue these notifications with the lock held
		// and before returning the handle. That means there is never a
		// chance for anyone to call the handle's HasSynced method in a
		// state when it would falsely return true (i.e., when the
		// shared informer is synced but it has not observed an Add
		// with isInitialList being true, nor when the thread
		// processing notifications somehow goes faster than this
		// thread adding them and the counter is temporarily zero).
		listener.add(addNotification{newObj: item, isInInitialList: true})
	}
	return handle, nil
}

func (s *sharedIndexInformer) HandleDeltas(obj interface{}, isInInitialList bool) error {
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()

	if deltas, ok := obj.(Deltas); ok {
		return processDeltas(s, s.indexer, deltas, isInInitialList)
	}
	return errors.New("object given as Process argument is not Deltas")
}

// Conforms to ResourceEventHandler
func (s *sharedIndexInformer) OnAdd(obj interface{}, isInInitialList bool) {
	// Invocation of this function is locked under s.blockDeltas, so it is
	// save to distribute the notification
	s.cacheMutationDetector.AddObject(obj)
	s.processor.distribute(addNotification{newObj: obj, isInInitialList: isInInitialList}, false)
}

// Conforms to ResourceEventHandler
func (s *sharedIndexInformer) OnUpdate(old, new interface{}) {
	isSync := false

	// If is a Sync event, isSync should be true
	// If is a Replaced event, isSync is true if resource version is unchanged.
	// If RV is unchanged: this is a Sync/Replaced event, so isSync is true

	if accessor, err := meta.Accessor(new); err == nil {
		if oldAccessor, err := meta.Accessor(old); err == nil {
			// Events that didn't change resourceVersion are treated as resync events
			// and only propagated to listeners that requested resync
			isSync = accessor.GetResourceVersion() == oldAccessor.GetResourceVersion()
		}
	}

	// Invocation of this function is locked under s.blockDeltas, so it is
	// save to distribute the notification
	s.cacheMutationDetector.AddObject(new)
	s.processor.distribute(updateNotification{oldObj: old, newObj: new}, isSync)
}

// Conforms to ResourceEventHandler
func (s *sharedIndexInformer) OnDelete(old interface{}) {
	// Invocation of this function is locked under s.blockDeltas, so it is
	// save to distribute the notification
	s.processor.distribute(deleteNotification{oldObj: old}, false)
}

// IsStopped reports whether the informer has already been stopped
func (s *sharedIndexInformer) IsStopped() bool {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()
	return s.stopped
}

func (s *sharedIndexInformer) RemoveEventHandler(handle ResourceEventHandlerRegistration) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	// in order to safely remove, we have to
	// 1. stop sending add/update/delete notifications
	// 2. remove and stop listener
	// 3. unblock
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()
	return s.processor.removeListener(handle)
}

// sharedProcessor has a collection of processorListener and can
// distribute a notification object to its listeners.  There are two
// kinds of distribute operations.  The sync distributions go to a
// subset of the listeners that (a) is recomputed in the occasional
// calls to shouldResync and (b) every listener is initially put in.
// The non-sync distributions go to every listener.
type sharedProcessor struct {
	listenersStarted bool
	listenersLock    sync.RWMutex
	// Map from listeners to whether or not they are currently syncing
	listeners map[*processorListener]bool
	clock     clock.Clock
	wg        wait.Group
}

func (p *sharedProcessor) getListener(registration ResourceEventHandlerRegistration) *processorListener {
	p.listenersLock.RLock()
	defer p.listenersLock.RUnlock()

	if p.listeners == nil {
		return nil
	}

	if result, ok := registration.(*processorListener); ok {
		if _, exists := p.listeners[result]; exists {
			return result
		}
	}

	return nil
}

func (p *sharedProcessor) addListener(listener *processorListener) ResourceEventHandlerRegistration {
	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()

	if p.listeners == nil {
		p.listeners = make(map[*processorListener]bool)
	}

	p.listeners[listener] = true

	if p.listenersStarted {
		p.wg.Start(listener.run)
		p.wg.Start(listener.pop)
	}

	return listener
}

func (p *sharedProcessor) removeListener(handle ResourceEventHandlerRegistration) error {
	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()

	listener, ok := handle.(*processorListener)
	if !ok {
		return fmt.Errorf("invalid key type %t", handle)
	} else if p.listeners == nil {
		// No listeners are registered, do nothing
		return nil
	} else if _, exists := p.listeners[listener]; !exists {
		// Listener is not registered, just do nothing
		return nil
	}

	delete(p.listeners, listener)

	if p.listenersStarted {
		close(listener.addCh)
	}

	return nil
}

func (p *sharedProcessor) distribute(obj interface{}, sync bool) {
	p.listenersLock.RLock()
	defer p.listenersLock.RUnlock()

	for listener, isSyncing := range p.listeners {
		switch {
		case !sync:
			// non-sync messages are delivered to every listener
			listener.add(obj)
		case isSyncing:
			// sync messages are delivered to every syncing listener
			listener.add(obj)
		default:
			// skipping a sync obj for a non-syncing listener
		}
	}
}

func (p *sharedProcessor) run(ctx context.Context) {
	func() {
		p.listenersLock.RLock()
		defer p.listenersLock.RUnlock()
		for listener := range p.listeners {
			p.wg.Start(listener.run)
			p.wg.Start(listener.pop)
		}
		p.listenersStarted = true
	}()
	<-ctx.Done()

	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()
	for listener := range p.listeners {
		close(listener.addCh) // Tell .pop() to stop. .pop() will tell .run() to stop
	}

	// Wipe out list of listeners since they are now closed
	// (processorListener cannot be re-used)
	p.listeners = nil

	// Reset to false since no listeners are running
	p.listenersStarted = false

	p.wg.Wait() // Wait for all .pop() and .run() to stop
}

// shouldResync queries every listener to determine if any of them need a resync, based on each
// listener's resyncPeriod.
func (p *sharedProcessor) shouldResync() bool {
	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()

	resyncNeeded := false
	now := p.clock.Now()
	for listener := range p.listeners {
		// need to loop through all the listeners to see if they need to resync so we can prepare any
		// listeners that are going to be resyncing.
		shouldResync := listener.shouldResync(now)
		p.listeners[listener] = shouldResync

		if shouldResync {
			resyncNeeded = true
			listener.determineNextResync(now)
		}
	}
	return resyncNeeded
}

func (p *sharedProcessor) resyncCheckPeriodChanged(logger klog.Logger, resyncCheckPeriod time.Duration) {
	p.listenersLock.RLock()
	defer p.listenersLock.RUnlock()

	for listener := range p.listeners {
		resyncPeriod := determineResyncPeriod(
			logger, listener.requestedResyncPeriod, resyncCheckPeriod)
		listener.setResyncPeriod(resyncPeriod)
	}
}

// processorListener relays notifications from a sharedProcessor to
// one ResourceEventHandler --- using two goroutines, two unbuffered
// channels, and an unbounded ring buffer.  The `add(notification)`
// function sends the given notification to `addCh`.  One goroutine
// runs `pop()`, which pumps notifications from `addCh` to `nextCh`
// using storage in the ring buffer while `nextCh` is not keeping up.
// Another goroutine runs `run()`, which receives notifications from
// `nextCh` and synchronously invokes the appropriate handler method.
//
// processorListener also keeps track of the adjusted requested resync
// period of the listener.
type processorListener struct {
	name   string
	logger klog.Logger

	nextCh chan interface{}
	addCh  chan interface{}

	handler ResourceEventHandler

	syncTracker *synctrack.SingleFileTracker

	// pendingNotifications is an unbounded ring buffer that holds all notifications not yet distributed.
	// There is one per listener, but a failing/stalled listener will have infinite pendingNotifications
	// added until we OOM.
	// TODO: This is no worse than before, since reflectors were backed by unbounded DeltaFIFOs, but
	// we should try to do something better.
	pendingNotifications buffer.RingGrowing

	// requestedResyncPeriod is how frequently the listener wants a
	// full resync from the shared informer, but modified by two
	// adjustments.  One is imposing a lower bound,
	// `minimumResyncPeriod`.  The other is another lower bound, the
	// sharedIndexInformer's `resyncCheckPeriod`, that is imposed (a) only
	// in AddEventHandlerWithResyncPeriod invocations made after the
	// sharedIndexInformer starts and (b) only if the informer does
	// resyncs at all.
	requestedResyncPeriod time.Duration
	// resyncPeriod is the threshold that will be used in the logic
	// for this listener.  This value differs from
	// requestedResyncPeriod only when the sharedIndexInformer does
	// not do resyncs, in which case the value here is zero.  The
	// actual time between resyncs depends on when the
	// sharedProcessor's `shouldResync` function is invoked and when
	// the sharedIndexInformer processes `Sync` type Delta objects.
	resyncPeriod time.Duration
	// nextResync is the earliest time the listener should get a full resync
	nextResync time.Time
	// resyncLock guards access to resyncPeriod and nextResync
	resyncLock sync.Mutex

	// metrics tracks basic metric information about the listener.
	metrics *eventHandlerMetrics
}

// HasSynced returns true if the source informer has synced, and all
// corresponding events have been delivered.
func (p *processorListener) HasSynced() bool {
	return p.syncTracker.HasSynced()
}

func newProcessListener(logger klog.Logger, name string, handler ResourceEventHandler, requestedResyncPeriod, resyncPeriod time.Duration, now time.Time, bufferSize int, hasSynced func() bool) *processorListener {
	listenerName := makeValidPromethusMetricName(fmt.Sprintf("listener_%s_%T", name, handler))

	metrics := &eventHandlerMetrics{
		numberOfPendingNotifications: sharedInformerMetricsFactory.getMetricsProvider().NewPendingNotificationsTotalMetric(listenerName),
		sizeOfRingGrowing:            sharedInformerMetricsFactory.getMetricsProvider().NewRingGrowingCapacityMetric(listenerName),
		processDuration:              sharedInformerMetricsFactory.getMetricsProvider().NewEventProcessingDurationMetric(listenerName),
	}

	ret := &processorListener{
		logger:                logger,
		metrics:               metrics,
		name:                  listenerName,
		nextCh:                make(chan interface{}),
		addCh:                 make(chan interface{}),
		handler:               handler,
		syncTracker:           &synctrack.SingleFileTracker{UpstreamHasSynced: hasSynced},
		pendingNotifications:  *buffer.NewRingGrowing(bufferSize),
		requestedResyncPeriod: requestedResyncPeriod,
		resyncPeriod:          resyncPeriod,
	}

	ret.determineNextResync(now)

	return ret
}

func (p *processorListener) add(notification interface{}) {
	if a, ok := notification.(addNotification); ok && a.isInInitialList {
		p.syncTracker.Start()
	}
	p.addCh <- notification
}

func (p *processorListener) pop() {
	defer utilruntime.HandleCrashWithLogger(p.logger)
	defer close(p.nextCh) // Tell .run() to stop

	var nextCh chan<- interface{}
	var notification interface{}
	for {
		select {
		case nextCh <- notification:
			// Notification dispatched
			var ok bool
			notification, ok = p.pendingNotifications.ReadOne()
			if !ok { // Nothing to pop
				nextCh = nil // Disable this select case
			}
		case notificationToAdd, ok := <-p.addCh:
			if !ok {
				return
			}
			if notification == nil { // No notification to pop (and pendingNotifications is empty)
				// Optimize the case - skip adding to pendingNotifications
				notification = notificationToAdd
				nextCh = p.nextCh
			} else { // There is already a notification waiting to be dispatched
				p.pendingNotifications.WriteOne(notificationToAdd)
			}
		}
	}
}

func (p *processorListener) run() {
	// this call blocks until the channel is closed.  When a panic happens during the notification
	// we will catch it, **the offending item will be skipped!**, and after a short delay (one second)
	// the next notification will be attempted. This is usually better than the alternative of never
	// delivering again.
	//
	// This only applies if utilruntime is configured to not panic, which is not the default.
	sleepAfterCrash := false
	metricsUpdateCounter := 0
	lastMetricsUpdate := time.Now()
	const (
		metricsUpdateInterval = 5 * time.Second
		metricsUpdateBatch    = 100
	)

	for next := range p.nextCh {
		if sleepAfterCrash {
			// Sleep before processing the next item.
			time.Sleep(time.Second)
		}
		startTime := time.Now()
		func() {
			// Gets reset below, but only if we get that far.
			sleepAfterCrash = true
			defer utilruntime.HandleCrashWithLogger(p.logger)

			switch notification := next.(type) {
			case updateNotification:
				p.handler.OnUpdate(notification.oldObj, notification.newObj)
			case addNotification:
				p.handler.OnAdd(notification.newObj, notification.isInInitialList)
				if notification.isInInitialList {
					p.syncTracker.Finished()
				}
			case deleteNotification:
				p.handler.OnDelete(notification.oldObj)
			default:
				utilruntime.HandleErrorWithLogger(p.logger, nil, "unrecognized notification", "notificationType", fmt.Sprintf("%T", next))
			}
			sleepAfterCrash = false

			// Note: There is a data race here when multiple goroutines read/write the queue concurrently.
			// However, since these are just metrics and some inaccuracy is acceptable,
			// we choose not to add locks to avoid the performance overhead.
			// This is an intentional trade-off between accuracy and performance.
			//
			// TODO: This requires implementing Len() and Capacity() for ring growing
			metricsUpdateCounter++
			if metricsUpdateCounter >= metricsUpdateBatch || time.Since(lastMetricsUpdate) >= metricsUpdateInterval {
				p.metrics.processDuration.Observe(time.Since(startTime).Seconds())
				// p.metrics.numberOfPendingNotifications.Set(p.pendingNotifications.Len())
				// p.metrics.sizeOfRingGrowing.Set(float64(p.pendingNotifications.Capacity()))

				metricsUpdateCounter = 0
				lastMetricsUpdate = time.Now()
			}
		}()
	}
}

// shouldResync deterimines if the listener needs a resync. If the listener's resyncPeriod is 0,
// this always returns false.
func (p *processorListener) shouldResync(now time.Time) bool {
	p.resyncLock.Lock()
	defer p.resyncLock.Unlock()

	if p.resyncPeriod == 0 {
		return false
	}

	return now.After(p.nextResync) || now.Equal(p.nextResync)
}

func (p *processorListener) determineNextResync(now time.Time) {
	p.resyncLock.Lock()
	defer p.resyncLock.Unlock()

	p.nextResync = now.Add(p.resyncPeriod)
}

func (p *processorListener) setResyncPeriod(resyncPeriod time.Duration) {
	p.resyncLock.Lock()
	defer p.resyncLock.Unlock()

	p.resyncPeriod = resyncPeriod
}
