/*
Copyright 2016 The Kubernetes Authors.

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
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

// Utilities for dealing with Jobs and CronJobs and time.

type missedSchedulesType int

const (
	noneMissed missedSchedulesType = iota
	fewMissed
	manyMissed
)

func (e missedSchedulesType) String() string {
	switch e {
	case noneMissed:
		return "none"
	case fewMissed:
		return "few"
	case manyMissed:
		return "many"
	default:
		return fmt.Sprintf("unknown(%d)", int(e))
	}
}

// inActiveList checks if cronjob's .status.active has a job with the same UID.
func inActiveList(cj *batchv1.CronJob, uid types.UID) bool {
	for _, j := range cj.Status.Active {
		if j.UID == uid {
			return true
		}
	}
	return false
}

// inActiveListByName checks if cronjob's status.active has a job with the same
// name and namespace.
func inActiveListByName(cj *batchv1.CronJob, job *batchv1.Job) bool {
	for _, j := range cj.Status.Active {
		if j.Name == job.Name && j.Namespace == job.Namespace {
			return true
		}
	}
	return false
}

func deleteFromActiveList(cj *batchv1.CronJob, uid types.UID) {
	if cj == nil {
		return
	}
	// TODO: @alpatel the memory footprint can may be reduced here by
	//  cj.Status.Active = append(cj.Status.Active[:indexToRemove], cj.Status.Active[indexToRemove:]...)
	newActive := []corev1.ObjectReference{}
	for _, j := range cj.Status.Active {
		if j.UID != uid {
			newActive = append(newActive, j)
		}
	}
	cj.Status.Active = newActive
}

// mostRecentScheduleTime returns:
//   - the last schedule time or CronJob's creation time,
//   - the most recent time a Job should be created or nil, if that's after now,
//   - value indicating either none missed schedules, a few missed or many missed
//   - error in an edge case where the schedule specification is grammatically correct,
//     but logically doesn't make sense (31st day for months with only 30 days, for example).
func mostRecentScheduleTime(cj *batchv1.CronJob, now time.Time, schedule cron.Schedule, includeStartingDeadlineSeconds bool) (time.Time, *time.Time, missedSchedulesType, error) {
	earliestTime := cj.ObjectMeta.CreationTimestamp.Time
	missedSchedules := noneMissed
	if cj.Status.LastScheduleTime != nil {
		earliestTime = cj.Status.LastScheduleTime.Time
	}
	if includeStartingDeadlineSeconds && cj.Spec.StartingDeadlineSeconds != nil {
		// controller is not going to schedule anything below this point
		schedulingDeadline := now.Add(-time.Second * time.Duration(*cj.Spec.StartingDeadlineSeconds))

		if schedulingDeadline.After(earliestTime) {
			earliestTime = schedulingDeadline
		}
	}

	t1 := schedule.Next(earliestTime)
	t2 := schedule.Next(t1)

	if now.Before(t1) {
		return earliestTime, nil, missedSchedules, nil
	}
	if now.Before(t2) {
		return earliestTime, &t1, missedSchedules, nil
	}

	// It is possible for cron.ParseStandard("59 23 31 2 *") to return an invalid schedule
	// minute - 59, hour - 23, dom - 31, month - 2, and dow is optional, clearly 31 is invalid
	// In this case the timeBetweenTwoSchedules will be 0, and we error out the invalid schedule
	timeBetweenTwoSchedules := int64(t2.Sub(t1).Round(time.Second).Seconds())
	if timeBetweenTwoSchedules < 1 {
		return earliestTime, nil, missedSchedules, fmt.Errorf("time difference between two schedules is less than 1 second")
	}
	// this logic used for calculating number of missed schedules does a rough
	// approximation, by calculating a diff between two schedules (t1 and t2),
	// and counting how many of these will fit in between last schedule and now
	timeElapsed := int64(now.Sub(t1).Seconds())
	numberOfMissedSchedules := (timeElapsed / timeBetweenTwoSchedules) + 1

	var mostRecentTime time.Time
	// to get the most recent time accurate for regular schedules and the ones
	// specified with @every form, we first need to calculate the potential earliest
	// time by multiplying the initial number of missed schedules by its interval,
	// this is critical to ensure @every starts at the correct time, this explains
	// the numberOfMissedSchedules-1, the additional -1 serves there to go back
	// in time one more time unit, and let the cron library calculate a proper
	// schedule, for case where the schedule is not consistent, for example
	// something like  30 6-16/4 * * 1-5
	potentialEarliest := t1.Add(time.Duration((numberOfMissedSchedules-1-1)*timeBetweenTwoSchedules) * time.Second)
	for t := schedule.Next(potentialEarliest); !t.After(now); t = schedule.Next(t) {
		mostRecentTime = t
	}

	// An object might miss several starts. For example, if
	// controller gets wedged on friday at 5:01pm when everyone has
	// gone home, and someone comes in on tuesday AM and discovers
	// the problem and restarts the controller, then all the hourly
	// jobs, more than 80 of them for one hourly cronJob, should
	// all start running with no further intervention (if the cronJob
	// allows concurrency and late starts).
	//
	// However, if there is a bug somewhere, or incorrect clock
	// on controller's server or apiservers (for setting creationTimestamp)
	// then there could be so many missed start times (it could be off
	// by decades or more), that it would eat up all the CPU and memory
	// of this controller. In that case, we want to not try to list
	// all the missed start times.
	//
	// I've somewhat arbitrarily picked 100, as more than 80,
	// but less than "lots".
	switch {
	case numberOfMissedSchedules > 100:
		missedSchedules = manyMissed
	// inform about few missed, still
	case numberOfMissedSchedules > 0:
		missedSchedules = fewMissed
	}

	if mostRecentTime.IsZero() {
		return earliestTime, nil, missedSchedules, nil
	}
	return earliestTime, &mostRecentTime, missedSchedules, nil
}

// nextScheduleTimeDuration returns the time duration to requeue based on
// the schedule and last schedule time. It adds a 100ms padding to the next requeue to account
// for Network Time Protocol(NTP) time skews. If the time drifts the adjustment, which in most
// realistic cases should be around 100s, the job will still be executed without missing
// the schedule.
func nextScheduleTimeDuration(cj *batchv1.CronJob, now time.Time, schedule cron.Schedule) *time.Duration {
	earliestTime, mostRecentTime, missedSchedules, err := mostRecentScheduleTime(cj, now, schedule, false)
	if err != nil {
		// we still have to requeue at some point, so aim for the next scheduling slot from now
		mostRecentTime = &now
	} else if mostRecentTime == nil {
		if missedSchedules == noneMissed {
			// no missed schedules since earliestTime
			mostRecentTime = &earliestTime
		} else {
			// if there are missed schedules since earliestTime, always use now
			mostRecentTime = &now
		}
	}

	t := schedule.Next(*mostRecentTime).Add(nextScheduleDelta).Sub(now)
	return &t
}

// nextScheduleTime returns the time.Time of the next schedule after the last scheduled
// and before now, or nil if no unmet schedule times, and an error.
// If there are too many (>100) unstarted times, it will also record a warning.
func nextScheduleTime(logger klog.Logger, cj *batchv1.CronJob, now time.Time, schedule cron.Schedule, recorder record.EventRecorder) (*time.Time, error) {
	_, mostRecentTime, missedSchedules, err := mostRecentScheduleTime(cj, now, schedule, true)

	if mostRecentTime == nil || mostRecentTime.After(now) {
		return nil, err
	}

	if missedSchedules == manyMissed {
		recorder.Event(cj, corev1.EventTypeWarning, "TooManyMissedTimes", "too many missed start times. Set or decrease .spec.startingDeadlineSeconds or check clock skew")
		logger.Info("too many missed times", "cronjob", klog.KObj(cj))
	}

	return mostRecentTime, err
}

func copyLabels(template *batchv1.JobTemplateSpec) labels.Set {
	l := make(labels.Set)
	for k, v := range template.Labels {
		l[k] = v
	}
	return l
}

func copyAnnotations(template *batchv1.JobTemplateSpec) labels.Set {
	a := make(labels.Set)
	for k, v := range template.Annotations {
		a[k] = v
	}
	return a
}

// getJobFromTemplate2 makes a Job from a CronJob. It converts the unix time into minutes from
// epoch time and concatenates that to the job name, because the cronjob_controller v2 has the lowest
// granularity of 1 minute for scheduling job.
func getJobFromTemplate2(cj *batchv1.CronJob, scheduledTime time.Time) (*batchv1.Job, error) {
	labels := copyLabels(&cj.Spec.JobTemplate)
	annotations := copyAnnotations(&cj.Spec.JobTemplate)
	// We want job names for a given nominal start time to have a deterministic name to avoid the same job being created twice
	name := getJobName(cj, scheduledTime)

	if utilfeature.DefaultFeatureGate.Enabled(features.CronJobsScheduledAnnotation) {

		timeZoneLocation, err := time.LoadLocation(ptr.Deref(cj.Spec.TimeZone, ""))
		if err != nil {
			return nil, err
		}
		// Append job creation timestamp to the cronJob annotations. The time will be in RFC3339 form.
		annotations[batchv1.CronJobScheduledTimestampAnnotation] = scheduledTime.In(timeZoneLocation).Format(time.RFC3339)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Labels:            labels,
			Annotations:       annotations,
			Name:              name,
			CreationTimestamp: metav1.Time{Time: scheduledTime},
			OwnerReferences:   []metav1.OwnerReference{*metav1.NewControllerRef(cj, controllerKind)},
		},
	}
	cj.Spec.JobTemplate.Spec.DeepCopyInto(&job.Spec)
	return job, nil
}

// getTimeHashInMinutes returns Unix Epoch Time in minutes
func getTimeHashInMinutes(scheduledTime time.Time) int64 {
	return scheduledTime.Unix() / 60
}

// byJobStartTime sorts a list of jobs by start timestamp, using their names as a tie breaker.
type byJobStartTime []*batchv1.Job

func (o byJobStartTime) Len() int      { return len(o) }
func (o byJobStartTime) Swap(i, j int) { o[i], o[j] = o[j], o[i] }

func (o byJobStartTime) Less(i, j int) bool {
	if o[i].Status.StartTime == nil && o[j].Status.StartTime != nil {
		return false
	}
	if o[i].Status.StartTime != nil && o[j].Status.StartTime == nil {
		return true
	}
	if o[i].Status.StartTime.Equal(o[j].Status.StartTime) {
		return o[i].Name < o[j].Name
	}
	return o[i].Status.StartTime.Before(o[j].Status.StartTime)
}
