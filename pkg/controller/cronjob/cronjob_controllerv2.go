/*
Copyright 2020 The Kubernetes Authors.

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

package cronjob

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	batchv1informers "k8s.io/client-go/informers/batch/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	batchv1listers "k8s.io/client-go/listers/batch/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/cronjob/metrics"
	jobutil "k8s.io/kubernetes/pkg/controller/job/util"
	"k8s.io/utils/ptr"
)

var (
	// controllerKind contains the schema.GroupVersionKind for this controller type.
	controllerKind = batchv1.SchemeGroupVersion.WithKind("CronJob")

	nextScheduleDelta = 100 * time.Millisecond
)

// ControllerV2 is a controller for CronJobs.
// Refactored Cronjob controller that uses DelayingQueue and informers
type ControllerV2 struct {
	queue workqueue.TypedRateLimitingInterface[string]

	kubeClient  clientset.Interface
	recorder    record.EventRecorder
	broadcaster record.EventBroadcaster

	jobControl     jobControlInterface
	cronJobControl cjControlInterface

	jobLister     batchv1listers.JobLister
	cronJobLister batchv1listers.CronJobLister

	jobListerSynced     cache.InformerSynced
	cronJobListerSynced cache.InformerSynced

	// now is a function that returns current time, done to facilitate unit tests
	now func() time.Time
}

// NewControllerV2 creates and initializes a new Controller.
func NewControllerV2(ctx context.Context, jobInformer batchv1informers.JobInformer, cronJobsInformer batchv1informers.CronJobInformer, kubeClient clientset.Interface) (*ControllerV2, error) {
	logger := klog.FromContext(ctx)
	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))

	jm := &ControllerV2{
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "cronjob",
			},
		),
		kubeClient:  kubeClient,
		broadcaster: eventBroadcaster,
		recorder:    eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "cronjob-controller"}),

		jobControl:     realJobControl{KubeClient: kubeClient},
		cronJobControl: &realCJControl{KubeClient: kubeClient},

		jobLister:     jobInformer.Lister(),
		cronJobLister: cronJobsInformer.Lister(),

		jobListerSynced:     jobInformer.Informer().HasSynced,
		cronJobListerSynced: cronJobsInformer.Informer().HasSynced,
		now:                 time.Now,
	}

	jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    jm.addJob,
		UpdateFunc: jm.updateJob,
		DeleteFunc: jm.deleteJob,
	})

	cronJobsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			jm.enqueueController(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			jm.updateCronJob(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			jm.enqueueController(obj)
		},
	})

	metrics.Register()

	return jm, nil
}

// Run starts the main goroutine responsible for watching and syncing jobs.
func (jm *ControllerV2) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()

	// Start event processing pipeline.
	jm.broadcaster.StartStructuredLogging(3)
	jm.broadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: jm.kubeClient.CoreV1().Events("")})
	defer jm.broadcaster.Shutdown()

	defer jm.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting cronjob controller v2")
	defer logger.Info("Shutting down cronjob controller v2")

	if !cache.WaitForNamedCacheSync("cronjob", ctx.Done(), jm.jobListerSynced, jm.cronJobListerSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, jm.worker, time.Second)
	}

	<-ctx.Done()
}

func (jm *ControllerV2) worker(ctx context.Context) {
	for jm.processNextWorkItem(ctx) {
	}
}

func (jm *ControllerV2) processNextWorkItem(ctx context.Context) bool {
	key, quit := jm.queue.Get()
	if quit {
		return false
	}
	defer jm.queue.Done(key)

	requeueAfter, err := jm.sync(ctx, key)
	switch {
	case err != nil:
		utilruntime.HandleError(fmt.Errorf("error syncing CronJobController %v, requeuing: %w", key, err))
		jm.queue.AddRateLimited(key)
	case requeueAfter != nil:
		jm.queue.Forget(key)
		jm.queue.AddAfter(key, *requeueAfter)
	}
	return true
}

func (jm *ControllerV2) sync(ctx context.Context, cronJobKey string) (*time.Duration, error) {
	ns, name, err := cache.SplitMetaNamespaceKey(cronJobKey)
	if err != nil {
		return nil, err
	}
	logger := klog.FromContext(ctx)
	cronJob, err := jm.cronJobLister.CronJobs(ns).Get(name)
	switch {
	case errors.IsNotFound(err):
		// may be cronjob is deleted, don't need to requeue this key
		logger.V(4).Info("CronJob not found, may be it is deleted", "cronjob", klog.KObj(cronJob), "err", err)
		return nil, nil
	case err != nil:
		// for other transient apiserver error requeue with exponential backoff
		return nil, err
	}

	jobsToBeReconciled, err := jm.getJobsToBeReconciled(cronJob)
	if err != nil {
		return nil, err
	}

	// cronJobCopy is used to combine all the updates to a
	// CronJob object and perform an actual update only once.
	cronJobCopy := cronJob.DeepCopy()

	updateStatusAfterCleanup := jm.cleanupFinishedJobs(ctx, cronJobCopy, jobsToBeReconciled)

	requeueAfter, updateStatusAfterSync, syncErr := jm.syncCronJob(ctx, cronJobCopy, jobsToBeReconciled)
	if syncErr != nil {
		logger.V(2).Info("Error reconciling cronjob", "cronjob", klog.KObj(cronJob), "err", syncErr)
	}

	// Update the CronJob if needed
	if updateStatusAfterCleanup || updateStatusAfterSync {
		if _, err := jm.cronJobControl.UpdateStatus(ctx, cronJobCopy); err != nil {
			logger.V(2).Info("Unable to update status for cronjob", "cronjob", klog.KObj(cronJob), "resourceVersion", cronJob.ResourceVersion, "err", err)
			return nil, err
		}
	}

	if requeueAfter != nil {
		logger.V(4).Info("Re-queuing cronjob", "cronjob", klog.KObj(cronJob), "requeueAfter", requeueAfter)
		return requeueAfter, nil
	}
	// this marks the key done, currently only happens when the cronjob is suspended or spec has invalid schedule format
	return nil, syncErr
}

// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (jm *ControllerV2) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *batchv1.CronJob {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != controllerKind.Kind {
		return nil
	}
	cronJob, err := jm.cronJobLister.CronJobs(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if cronJob.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return cronJob
}

func (jm *ControllerV2) getJobsToBeReconciled(cronJob *batchv1.CronJob) ([]*batchv1.Job, error) {
	// list all jobs: there may be jobs with labels that don't match the template anymore,
	// but that still have a ControllerRef to the given cronjob
	jobList, err := jm.jobLister.Jobs(cronJob.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	jobsToBeReconciled := []*batchv1.Job{}

	for _, job := range jobList {
		// If it has a ControllerRef, that's all that matters.
		if controllerRef := metav1.GetControllerOf(job); controllerRef != nil && controllerRef.Name == cronJob.Name {
			// this job is needs to be reconciled
			jobsToBeReconciled = append(jobsToBeReconciled, job)
		}
	}
	return jobsToBeReconciled, nil
}

// When a job is created, enqueue the controller that manages it and update it's expectations.
func (jm *ControllerV2) addJob(obj interface{}) {
	job := obj.(*batchv1.Job)
	if job.DeletionTimestamp != nil {
		// on a restart of the controller, it's possible a new job shows up in a state that
		// is already pending deletion. Prevent the job from being a creation observation.
		jm.deleteJob(job)
		return
	}

	// If it has a ControllerRef, that's all that matters.
	if controllerRef := metav1.GetControllerOf(job); controllerRef != nil {
		cronJob := jm.resolveControllerRef(job.Namespace, controllerRef)
		if cronJob == nil {
			return
		}
		jm.enqueueController(cronJob)
		return
	}
}

// updateJob figures out what CronJob(s) manage a Job when the Job
// is updated and wake them up. If the anything of the Job have changed, we need to
// awaken both the old and new CronJob. old and cur must be *batchv1.Job
// types.
func (jm *ControllerV2) updateJob(old, cur interface{}) {
	curJob := cur.(*batchv1.Job)
	oldJob := old.(*batchv1.Job)
	if curJob.ResourceVersion == oldJob.ResourceVersion {
		// Periodic resync will send update events for all known jobs.
		// Two different versions of the same jobs will always have different RVs.
		return
	}

	curControllerRef := metav1.GetControllerOf(curJob)
	oldControllerRef := metav1.GetControllerOf(oldJob)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// The ControllerRef was changed. Sync the old controller, if any.
		if cronJob := jm.resolveControllerRef(oldJob.Namespace, oldControllerRef); cronJob != nil {
			jm.enqueueController(cronJob)
		}
	}

	// If it has a ControllerRef, that's all that matters.
	if curControllerRef != nil {
		cronJob := jm.resolveControllerRef(curJob.Namespace, curControllerRef)
		if cronJob == nil {
			return
		}
		jm.enqueueController(cronJob)
		return
	}
}

func (jm *ControllerV2) deleteJob(obj interface{}) {
	job, ok := obj.(*batchv1.Job)

	// When a delete is dropped, the relist will notice a job in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		job, ok = tombstone.Obj.(*batchv1.Job)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Job %#v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(job)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	cronJob := jm.resolveControllerRef(job.Namespace, controllerRef)
	if cronJob == nil {
		return
	}
	jm.enqueueController(cronJob)
}

func (jm *ControllerV2) enqueueController(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	jm.queue.Add(key)
}

func (jm *ControllerV2) enqueueControllerAfter(obj interface{}, t time.Duration) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	jm.queue.AddAfter(key, t)
}

// updateCronJob re-queues the CronJob for next scheduled time if there is a
// change in spec.schedule otherwise it re-queues it now
func (jm *ControllerV2) updateCronJob(logger klog.Logger, old interface{}, curr interface{}) {
	oldCJ, okOld := old.(*batchv1.CronJob)
	newCJ, okNew := curr.(*batchv1.CronJob)

	if !okOld || !okNew {
		// typecasting of one failed, handle this better, may be log entry
		return
	}
	// if the change in schedule results in next requeue having to be sooner than it already was,
	// it will be handled here by the queue. If the next requeue is further than previous schedule,
	// the sync loop will essentially be a no-op for the already queued key with old schedule.
	if oldCJ.Spec.Schedule != newCJ.Spec.Schedule || !ptr.Equal(oldCJ.Spec.TimeZone, newCJ.Spec.TimeZone) {
		// schedule changed, change the requeue time, pass nil recorder so that syncCronJob will output any warnings
		sched, err := cron.ParseStandard(formatSchedule(newCJ, nil))
		if err != nil {
			// this is likely a user error in defining the spec value
			// we should log the error and not reconcile this cronjob until an update to spec
			logger.V(2).Info("Unparsable schedule for cronjob", "cronjob", klog.KObj(newCJ), "schedule", newCJ.Spec.Schedule, "err", err)
			jm.recorder.Eventf(newCJ, corev1.EventTypeWarning, "UnParseableCronJobSchedule", "unparsable schedule for cronjob: %s", newCJ.Spec.Schedule)
			return
		}
		now := jm.now()
		t := nextScheduleTimeDuration(newCJ, now, sched)

		jm.enqueueControllerAfter(curr, *t)
		return
	}

	// other parameters changed, requeue this now and if this gets triggered
	// within deadline, sync loop will work on the CJ otherwise updates will be handled
	// during the next schedule
	// TODO: need to handle the change of spec.JobTemplate.metadata.labels explicitly
	//   to cleanup jobs with old labels
	jm.enqueueController(curr)
}

// syncCronJob reconciles a CronJob with a list of any Jobs that it created.
// All known jobs created by "cronJob" should be included in "jobs".
// The current time is passed in to facilitate testing.
// It returns a bool to indicate an update to api-server is needed
func (jm *ControllerV2) syncCronJob(
	ctx context.Context,
	cronJob *batchv1.CronJob,
	jobs []*batchv1.Job) (*time.Duration, bool, error) {

	now := jm.now()
	updateStatus := false

	childrenJobs := make(map[types.UID]bool)
	for _, j := range jobs {
		childrenJobs[j.ObjectMeta.UID] = true
		found := inActiveList(cronJob, j.ObjectMeta.UID)
		if !found && !jobutil.IsJobFinished(j) {
			cjCopy, err := jm.cronJobControl.GetCronJob(ctx, cronJob.Namespace, cronJob.Name)
			if err != nil {
				return nil, updateStatus, err
			}
			if inActiveList(cjCopy, j.ObjectMeta.UID) {
				cronJob = cjCopy
				continue
			}
			jm.recorder.Eventf(cronJob, corev1.EventTypeWarning, "UnexpectedJob", "Saw a job that the controller did not create or forgot: %s", j.Name)
			// We found an unfinished job that has us as the parent, but it is not in our Active list.
			// This could happen if we crashed right after creating the Job and before updating the status,
			// or if our jobs list is newer than our cj status after a relist, or if someone intentionally created
			// a job that they wanted us to adopt.
		} else if jobutil.IsJobFinished(j) {
			if found {
				_, condition := jobutil.FinishedCondition(j)
				deleteFromActiveList(cronJob, j.ObjectMeta.UID)
				jm.recorder.Eventf(cronJob, corev1.EventTypeNormal, "SawCompletedJob", "Saw completed job: %s, condition: %v", j.Name, condition)
				updateStatus = true
			}
			if jobutil.IsJobSucceeded(j) {
				// a job does not have to be in active list, as long as it has completed successfully, we will process the timestamp
				if cronJob.Status.LastSuccessfulTime == nil {
					cronJob.Status.LastSuccessfulTime = j.Status.CompletionTime
					updateStatus = true
				}
				if j.Status.CompletionTime != nil && j.Status.CompletionTime.After(cronJob.Status.LastSuccessfulTime.Time) {
					cronJob.Status.LastSuccessfulTime = j.Status.CompletionTime
					updateStatus = true
				}
			}

		}
	}

	// Remove any job reference from the active list if the corresponding job does not exist any more.
	// Otherwise, the cronjob may be stuck in active mode forever even though there is no matching
	// job running.
	for _, j := range cronJob.Status.Active {
		_, found := childrenJobs[j.UID]
		if found {
			continue
		}
		// Explicitly try to get the job from api-server to avoid a slow watch not able to update
		// the job lister on time, giving an unwanted miss
		_, err := jm.jobControl.GetJob(j.Namespace, j.Name)
		switch {
		case errors.IsNotFound(err):
			// The job is actually missing, delete from active list and schedule a new one if within
			// deadline
			jm.recorder.Eventf(cronJob, corev1.EventTypeNormal, "MissingJob", "Active job went missing: %v", j.Name)
			deleteFromActiveList(cronJob, j.UID)
			updateStatus = true
		case err != nil:
			return nil, updateStatus, err
		}
		// the job is missing in the lister but found in api-server
	}

	if cronJob.DeletionTimestamp != nil {
		// The CronJob is being deleted.
		// Don't do anything other than updating status.
		return nil, updateStatus, nil
	}

	logger := klog.FromContext(ctx)
	if cronJob.Spec.TimeZone != nil {
		timeZone := ptr.Deref(cronJob.Spec.TimeZone, "")
		if _, err := time.LoadLocation(timeZone); err != nil {
			logger.V(4).Info("Not starting job because timeZone is invalid", "cronjob", klog.KObj(cronJob), "timeZone", timeZone, "err", err)
			jm.recorder.Eventf(cronJob, corev1.EventTypeWarning, "UnknownTimeZone", "invalid timeZone: %q: %s", timeZone, err)
			return nil, updateStatus, nil
		}
	}

	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		logger.V(4).Info("Not starting job because the cron is suspended", "cronjob", klog.KObj(cronJob))
		return nil, updateStatus, nil
	}

	sched, err := cron.ParseStandard(formatSchedule(cronJob, jm.recorder))
	if err != nil {
		// this is likely a user error in defining the spec value
		// we should log the error and not reconcile this cronjob until an update to spec
		logger.V(2).Info("Unparsable schedule", "cronjob", klog.KObj(cronJob), "schedule", cronJob.Spec.Schedule, "err", err)
		jm.recorder.Eventf(cronJob, corev1.EventTypeWarning, "UnparsableSchedule", "unparsable schedule: %q : %s", cronJob.Spec.Schedule, err)
		return nil, updateStatus, nil
	}

	scheduledTime, err := nextScheduleTime(logger, cronJob, now, sched, jm.recorder)
	if err != nil {
		// this is likely a user error in defining the spec value
		// we should log the error and not reconcile this cronjob until an update to spec
		logger.V(2).Info("Invalid schedule", "cronjob", klog.KObj(cronJob), "schedule", cronJob.Spec.Schedule, "err", err)
		jm.recorder.Eventf(cronJob, corev1.EventTypeWarning, "InvalidSchedule", "invalid schedule: %s : %s", cronJob.Spec.Schedule, err)
		return nil, updateStatus, nil
	}
	if scheduledTime == nil {
		// no unmet start time, return cj,.
		// The only time this should happen is if queue is filled after restart.
		// Otherwise, the queue is always suppose to trigger sync function at the time of
		// the scheduled time, that will give atleast 1 unmet time schedule
		logger.V(4).Info("No unmet start times", "cronjob", klog.KObj(cronJob))
		t := nextScheduleTimeDuration(cronJob, now, sched)
		return t, updateStatus, nil
	}

	tooLate := false
	if cronJob.Spec.StartingDeadlineSeconds != nil {
		tooLate = scheduledTime.Add(time.Second * time.Duration(*cronJob.Spec.StartingDeadlineSeconds)).Before(now)
	}
	if tooLate {
		logger.V(4).Info("Missed starting window", "cronjob", klog.KObj(cronJob))
		jm.recorder.Eventf(cronJob, corev1.EventTypeWarning, "MissSchedule", "Missed scheduled time to start a job: %s", scheduledTime.UTC().Format(time.RFC1123Z))

		// TODO: Since we don't set LastScheduleTime when not scheduling, we are going to keep noticing
		// the miss every cycle.  In order to avoid sending multiple events, and to avoid processing
		// the cj again and again, we could set a Status.LastMissedTime when we notice a miss.
		// Then, when we call getRecentUnmetScheduleTimes, we can take max(creationTimestamp,
		// Status.LastScheduleTime, Status.LastMissedTime), and then so we won't generate
		// and event the next time we process it, and also so the user looking at the status
		// can see easily that there was a missed execution.
		t := nextScheduleTimeDuration(cronJob, now, sched)
		return t, updateStatus, nil
	}
	if inActiveListByName(cronJob, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getJobName(cronJob, *scheduledTime),
			Namespace: cronJob.Namespace,
		}}) || cronJob.Status.LastScheduleTime.Equal(&metav1.Time{Time: *scheduledTime}) {
		logger.V(4).Info("Not starting job because the scheduled time is already processed", "cronjob", klog.KObj(cronJob), "schedule", scheduledTime)
		t := nextScheduleTimeDuration(cronJob, now, sched)
		return t, updateStatus, nil
	}
	if cronJob.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent && len(cronJob.Status.Active) > 0 {
		// Regardless which source of information we use for the set of active jobs,
		// there is some risk that we won't see an active job when there is one.
		// (because we haven't seen the status update to the SJ or the created pod).
		// So it is theoretically possible to have concurrency with Forbid.
		// As long the as the invocations are "far enough apart in time", this usually won't happen.
		//
		// TODO: for Forbid, we could use the same name for every execution, as a lock.
		// With replace, we could use a name that is deterministic per execution time.
		// But that would mean that you could not inspect prior successes or failures of Forbid jobs.
		logger.V(4).Info("Not starting job because prior execution is still running and concurrency policy is Forbid", "cronjob", klog.KObj(cronJob))
		jm.recorder.Eventf(cronJob, corev1.EventTypeNormal, "JobAlreadyActive", "Not starting job because prior execution is running and concurrency policy is Forbid")
		t := nextScheduleTimeDuration(cronJob, now, sched)
		return t, updateStatus, nil
	}
	if cronJob.Spec.ConcurrencyPolicy == batchv1.ReplaceConcurrent {
		for _, j := range cronJob.Status.Active {
			logger.V(4).Info("Deleting job that was still running at next scheduled start time", "job", klog.KRef(j.Namespace, j.Name))
			job, err := jm.jobControl.GetJob(j.Namespace, j.Name)
			if err != nil {
				jm.recorder.Eventf(cronJob, corev1.EventTypeWarning, "FailedGet", "Get job: %v", err)
				return nil, updateStatus, err
			}
			if !deleteJob(logger, cronJob, job, jm.jobControl, jm.recorder) {
				return nil, updateStatus, fmt.Errorf("could not replace job %s/%s", job.Namespace, job.Name)
			}
			updateStatus = true
		}
	}

	jobAlreadyExists := false
	jobReq, err := getJobFromTemplate2(cronJob, *scheduledTime)
	if err != nil {
		logger.Error(err, "Unable to make Job from template", "cronjob", klog.KObj(cronJob))
		return nil, updateStatus, err
	}
	jobResp, err := jm.jobControl.CreateJob(cronJob.Namespace, jobReq)
	switch {
	case errors.HasStatusCause(err, corev1.NamespaceTerminatingCause):
		// if the namespace is being terminated, we don't have to do
		// anything because any creation will fail
		return nil, updateStatus, err
	case errors.IsAlreadyExists(err):
		// If the job is created by other actor, assume it has updated the cronjob status accordingly.
		// However, if the job was created by cronjob controller, this means we've previously created the job
		// but failed to update the active list in the status, in which case we should reattempt to add the job
		// into the active list and update the status.
		jobAlreadyExists = true
		job, err := jm.jobControl.GetJob(jobReq.GetNamespace(), jobReq.GetName())
		if err != nil {
			return nil, updateStatus, err
		}
		jobResp = job

		// check that this job is owned by cronjob controller, otherwise do nothing and assume external controller
		// is updating the status.
		if !metav1.IsControlledBy(job, cronJob) {
			return nil, updateStatus, nil
		}

		// Recheck if the job is missing from the active list before attempting to update the status again.
		found := inActiveList(cronJob, job.ObjectMeta.UID)
		if found {
			return nil, updateStatus, nil
		}
	case err != nil:
		// default error handling
		jm.recorder.Eventf(cronJob, corev1.EventTypeWarning, "FailedCreate", "Error creating job: %v", err)
		return nil, updateStatus, err
	}

	if jobAlreadyExists {
		logger.Info("Job already exists", "cronjob", klog.KObj(cronJob), "job", klog.KObj(jobReq))
	} else {
		metrics.CronJobCreationSkew.Observe(jobResp.ObjectMeta.GetCreationTimestamp().Sub(*scheduledTime).Seconds())
		logger.V(4).Info("Created Job", "job", klog.KObj(jobResp), "cronjob", klog.KObj(cronJob))
		jm.recorder.Eventf(cronJob, corev1.EventTypeNormal, "SuccessfulCreate", "Created job %v", jobResp.Name)
	}

	// ------------------------------------------------------------------ //

	// If this process restarts at this point (after posting a job, but
	// before updating the status), then we might try to start the job on
	// the next time.  Actually, if we re-list the SJs and Jobs on the next
	// iteration of syncAll, we might not see our own status update, and
	// then post one again.  So, we need to use the job name as a lock to
	// prevent us from making the job twice (name the job with hash of its
	// scheduled time).

	// Add the just-started job to the status list.
	jobRef, err := getRef(jobResp)
	if err != nil {
		logger.V(2).Info("Unable to make object reference", "cronjob", klog.KObj(cronJob), "err", err)
		return nil, updateStatus, fmt.Errorf("unable to make object reference for job for %s", klog.KObj(cronJob))
	}
	cronJob.Status.Active = append(cronJob.Status.Active, *jobRef)
	cronJob.Status.LastScheduleTime = &metav1.Time{Time: *scheduledTime}
	updateStatus = true

	t := nextScheduleTimeDuration(cronJob, now, sched)
	return t, updateStatus, nil
}

func getJobName(cj *batchv1.CronJob, scheduledTime time.Time) string {
	return fmt.Sprintf("%s-%d", cj.Name, getTimeHashInMinutes(scheduledTime))
}

// cleanupFinishedJobs cleanups finished jobs created by a CronJob
// It returns a bool to indicate an update to api-server is needed
func (jm *ControllerV2) cleanupFinishedJobs(ctx context.Context, cj *batchv1.CronJob, js []*batchv1.Job) bool {
	// If neither limits are active, there is no need to do anything.
	if cj.Spec.FailedJobsHistoryLimit == nil && cj.Spec.SuccessfulJobsHistoryLimit == nil {
		return false
	}

	updateStatus := false
	failedJobs := []*batchv1.Job{}
	successfulJobs := []*batchv1.Job{}

	for _, job := range js {
		isFinished, finishedStatus := jm.getFinishedStatus(job)
		if isFinished && finishedStatus == batchv1.JobComplete {
			successfulJobs = append(successfulJobs, job)
		} else if isFinished && finishedStatus == batchv1.JobFailed {
			failedJobs = append(failedJobs, job)
		}
	}

	if cj.Spec.SuccessfulJobsHistoryLimit != nil &&
		jm.removeOldestJobs(ctx, cj,
			successfulJobs,
			*cj.Spec.SuccessfulJobsHistoryLimit) {
		updateStatus = true
	}

	if cj.Spec.FailedJobsHistoryLimit != nil &&
		jm.removeOldestJobs(ctx, cj,
			failedJobs,
			*cj.Spec.FailedJobsHistoryLimit) {
		updateStatus = true
	}

	return updateStatus
}

func (jm *ControllerV2) getFinishedStatus(j *batchv1.Job) (bool, batchv1.JobConditionType) {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true, c.Type
		}
	}
	return false, ""
}

// removeOldestJobs removes the oldest jobs from a list of jobs
func (jm *ControllerV2) removeOldestJobs(ctx context.Context, cj *batchv1.CronJob, js []*batchv1.Job, maxJobs int32) bool {
	updateStatus := false
	numToDelete := len(js) - int(maxJobs)
	if numToDelete <= 0 {
		return updateStatus
	}
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Cleaning up jobs from CronJob list", "deletejobnum", numToDelete, "jobnum", len(js), "cronjob", klog.KObj(cj))

	sort.Sort(byJobStartTime(js))
	for i := 0; i < numToDelete; i++ {
		logger.V(4).Info("Removing job from CronJob list", "job", js[i].Name, "cronjob", klog.KObj(cj))
		if deleteJob(logger, cj, js[i], jm.jobControl, jm.recorder) {
			updateStatus = true
		}
	}
	return updateStatus
}

// deleteJob reaps a job, deleting the job, the pods and the reference in the active list
func deleteJob(logger klog.Logger, cj *batchv1.CronJob, job *batchv1.Job, jc jobControlInterface, recorder record.EventRecorder) bool {
	// delete the job itself...
	if err := jc.DeleteJob(job.Namespace, job.Name); err != nil {
		recorder.Eventf(cj, corev1.EventTypeWarning, "FailedDelete", "Deleted job: %v", err)
		logger.Error(err, "Error deleting job from cronjob", "job", klog.KObj(job), "cronjob", klog.KObj(cj))
		return false
	}
	// ... and its reference from active list
	deleteFromActiveList(cj, job.ObjectMeta.UID)
	recorder.Eventf(cj, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted job %v", job.Name)

	return true
}

func getRef(object runtime.Object) (*corev1.ObjectReference, error) {
	return ref.GetReference(scheme.Scheme, object)
}

func formatSchedule(cj *batchv1.CronJob, recorder record.EventRecorder) string {
	if strings.Contains(cj.Spec.Schedule, "TZ") {
		if recorder != nil {
			recorder.Eventf(cj, corev1.EventTypeWarning, "UnsupportedSchedule", "CRON_TZ or TZ used in schedule %q is not officially supported, see https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/ for more details", cj.Spec.Schedule)
		}

		return cj.Spec.Schedule
	}

	if cj.Spec.TimeZone != nil {
		if _, err := time.LoadLocation(*cj.Spec.TimeZone); err != nil {
			return cj.Spec.Schedule
		}

		return fmt.Sprintf("TZ=%s %s", *cj.Spec.TimeZone, cj.Spec.Schedule)
	}

	return cj.Spec.Schedule
}
