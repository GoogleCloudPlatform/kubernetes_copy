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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/pointer"

	_ "k8s.io/kubernetes/pkg/apis/batch/install"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/controller"
)

var (
	shortDead  int64 = 10
	mediumDead int64 = 2 * 60 * 60
	longDead   int64 = 1000000
	noDead     int64 = -12345

	errorSchedule = "obvious error schedule"
	// schedule is hourly on the hour
	onTheHour = "0 * * * ?"
	everyHour = "@every 1h"

	errorTimeZone = "bad timezone"
	newYork       = "America/New_York"
)

// returns a cronJob with some fields filled in.
func cronJob() batchv1.CronJob {
	return batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "mycronjob",
			Namespace:         "snazzycats",
			UID:               types.UID("1a2b3c"),
			CreationTimestamp: metav1.Time{Time: justBeforeTheHour()},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:          "* * * * ?",
			ConcurrencyPolicy: "Allow",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"a": "b"},
					Annotations: map[string]string{"x": "y"},
				},
				Spec: jobSpec(),
			},
		},
	}
}

func jobSpec() batchv1.JobSpec {
	one := int32(1)
	return batchv1.JobSpec{
		Parallelism: &one,
		Completions: &one,
		Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"foo": "bar",
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{Image: "foo/bar"},
				},
			},
		},
	}
}

func justASecondBeforeTheHour() time.Time {
	T1, err := time.Parse(time.RFC3339, "2016-05-19T09:59:59Z")
	if err != nil {
		panic("test setup error")
	}
	return T1
}

func justAfterThePriorHour() time.Time {
	T1, err := time.Parse(time.RFC3339, "2016-05-19T09:01:00Z")
	if err != nil {
		panic("test setup error")
	}
	return T1
}

func justBeforeThePriorHour() time.Time {
	T1, err := time.Parse(time.RFC3339, "2016-05-19T08:59:00Z")
	if err != nil {
		panic("test setup error")
	}
	return T1
}

func justAfterTheHour() *time.Time {
	T1, err := time.Parse(time.RFC3339, "2016-05-19T10:01:00Z")
	if err != nil {
		panic("test setup error")
	}
	return &T1
}

func justAfterTheHourInZone(tz string) time.Time {
	location, err := time.LoadLocation(tz)
	if err != nil {
		panic("tz error: " + err.Error())
	}

	T1, err := time.ParseInLocation(time.RFC3339, "2016-05-19T10:01:00Z", location)
	if err != nil {
		panic("test setup error: " + err.Error())
	}
	return T1
}

func justBeforeTheHour() time.Time {
	T1, err := time.Parse(time.RFC3339, "2016-05-19T09:59:00Z")
	if err != nil {
		panic("test setup error")
	}
	return T1
}

func justBeforeTheNextHour() time.Time {
	T1, err := time.Parse(time.RFC3339, "2016-05-19T10:59:00Z")
	if err != nil {
		panic("test setup error")
	}
	return T1
}

func weekAfterTheHour() time.Time {
	T1, err := time.Parse(time.RFC3339, "2016-05-26T10:00:00Z")
	if err != nil {
		panic("test setup error")
	}
	return T1
}

func TestControllerV2SyncCronJob(t *testing.T) {
	// Check expectations on deadline parameters
	if shortDead/60/60 >= 1 {
		t.Errorf("shortDead should be less than one hour")
	}

	if mediumDead/60/60 < 1 || mediumDead/60/60 >= 24 {
		t.Errorf("mediumDead should be between one hour and one day")
	}

	if longDead/60/60/24 < 10 {
		t.Errorf("longDead should be at least ten days")
	}

	testCases := map[string]struct {
		// cj spec
		concurrencyPolicy          batchv1.ConcurrencyPolicy
		suspend                    bool
		schedule                   string
		timeZone                   *string
		deadline                   int64
		successfulJobsHistoryLimit *int32

		// cj status
		ranPreviously bool
		stillActive   bool

		// environment
		cronjobCreationTime time.Time
		jobCreationTime     time.Time
		lastScheduleTime    time.Time
		now                 time.Time
		jobCreateError      error
		jobGetErr           error

		// expectations
		expectCreate               bool
		expectDelete               bool
		expectCompleted            bool
		expectActive               int
		expectedWarnings           int
		expectErr                  bool
		expectRequeueAfter         bool
		expectedRequeueDuration    time.Duration
		expectUpdateStatus         bool
		jobStillNotFoundInLister   bool
		jobPresentInCJActiveStatus bool
	}{
		"never ran, not valid schedule, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   errorSchedule,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectedWarnings:           1,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, not valid schedule, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   errorSchedule,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectedWarnings:           1,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, not valid schedule, R": {
			concurrencyPolicy:          "Forbid",
			schedule:                   errorSchedule,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectedWarnings:           1,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, not valid time zone": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			timeZone:                   &errorTimeZone,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectedWarnings:           1,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, not time, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true},
		"never ran, not time, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, not time, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, not time in zone": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			timeZone:                   &newYork,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, is time, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, is time, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, is time, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, is time in zone, but time zone disabled": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			timeZone:                   &newYork,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justAfterTheHourInZone(newYork),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, is time in zone": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			timeZone:                   &newYork,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justAfterTheHourInZone(newYork),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, is time in zone, but TZ is also set in schedule": {
			concurrencyPolicy:          "Allow",
			schedule:                   "TZ=UTC " + onTheHour,
			timeZone:                   &newYork,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justAfterTheHourInZone(newYork),
			expectCreate:               true,
			expectedWarnings:           1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, is time, suspended": {
			concurrencyPolicy:          "Allow",
			suspend:                    true,
			schedule:                   onTheHour,
			deadline:                   noDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			jobPresentInCJActiveStatus: true,
		},
		"never ran, is time, past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   shortDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justAfterTheHour().Add(time.Minute * time.Duration(shortDead+1)),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute - time.Minute*time.Duration(shortDead+1) + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"never ran, is time, not past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   longDead,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},

		"prev ran but done, not time, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, not time, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, not time, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, is time, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, is time, create job failed, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			jobCreateError:             errors.NewAlreadyExists(schema.GroupResource{Resource: "job", Group: "batch"}, ""),
			expectErr:                  false,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, is time, job not present in CJ active status, create job failed, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			jobCreateError:             errors.NewAlreadyExists(schema.GroupResource{Resource: "job", Group: "batch"}, ""),
			expectErr:                  false,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: false,
		},
		"prev ran but done, is time, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, is time, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, is time, suspended": {
			concurrencyPolicy:          "Allow",
			suspend:                    true,
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, is time, past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   shortDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, is time, not past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   longDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},

		"still active, not time, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"still active, not time, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"still active, not time, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        justBeforeTheHour(),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"still active, is time, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               2,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"still active, is time, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"still active, is time, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectDelete:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"still active, is time, get job failed, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			jobGetErr:                  errors.NewBadRequest("request is invalid"),
			expectActive:               1,
			expectedWarnings:           1,
			jobPresentInCJActiveStatus: true,
		},
		"still active, is time, suspended": {
			concurrencyPolicy:          "Allow",
			suspend:                    true,
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectActive:               1,
			jobPresentInCJActiveStatus: true,
		},
		"still active, is time, past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   shortDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"still active, is time, not past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   longDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               2,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},

		// Controller should fail to schedule these, as there are too many missed starting times
		// and either no deadline or a too long deadline.
		"prev ran but done, long overdue, not past deadline, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   longDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectedWarnings:           1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, long overdue, not past deadline, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   longDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectedWarnings:           1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, long overdue, not past deadline, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   longDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectedWarnings:           1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, long overdue, no deadline, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectedWarnings:           1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, long overdue, no deadline, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectedWarnings:           1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, long overdue, no deadline, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectedWarnings:           1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},

		"prev ran but done, long overdue, past medium deadline, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   mediumDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, long overdue, past short deadline, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   shortDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},

		"prev ran but done, long overdue, past medium deadline, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   mediumDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, long overdue, past short deadline, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   shortDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},

		"prev ran but done, long overdue, past medium deadline, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   mediumDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"prev ran but done, long overdue, past short deadline, F": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   shortDead,
			ranPreviously:              true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},

		// Tests for time skews
		// the controller sees job is created, takes no actions
		"this ran but done, time drifted back, F": {
			concurrencyPolicy:       "Forbid",
			schedule:                onTheHour,
			deadline:                noDead,
			ranPreviously:           true,
			jobCreationTime:         *justAfterTheHour(),
			now:                     justBeforeTheHour(),
			jobCreateError:          errors.NewAlreadyExists(schema.GroupResource{Resource: "jobs", Group: "batch"}, ""),
			expectRequeueAfter:      true,
			expectedRequeueDuration: 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:      true,
		},

		// Tests for slow job lister
		"this started but went missing, not past deadline, A": {
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   longDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            topOfTheHour().Add(time.Millisecond * 100),
			now:                        justAfterTheHour().Add(time.Millisecond * 100),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute - time.Millisecond*100 + nextScheduleDelta,
			jobStillNotFoundInLister:   true,
			jobPresentInCJActiveStatus: true,
		},
		"this started but went missing, not past deadline, f": {
			concurrencyPolicy:          "Forbid",
			schedule:                   onTheHour,
			deadline:                   longDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            topOfTheHour().Add(time.Millisecond * 100),
			now:                        justAfterTheHour().Add(time.Millisecond * 100),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute - time.Millisecond*100 + nextScheduleDelta,
			jobStillNotFoundInLister:   true,
			jobPresentInCJActiveStatus: true,
		},
		"this started but went missing, not past deadline, R": {
			concurrencyPolicy:          "Replace",
			schedule:                   onTheHour,
			deadline:                   longDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            topOfTheHour().Add(time.Millisecond * 100),
			now:                        justAfterTheHour().Add(time.Millisecond * 100),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute - time.Millisecond*100 + nextScheduleDelta,
			jobStillNotFoundInLister:   true,
			jobPresentInCJActiveStatus: true,
		},

		// Tests for slow cronjob list
		"this started but is not present in cronjob active list, not past deadline, A": {
			concurrencyPolicy:       "Allow",
			schedule:                onTheHour,
			deadline:                longDead,
			ranPreviously:           true,
			stillActive:             true,
			jobCreationTime:         topOfTheHour().Add(time.Millisecond * 100),
			now:                     justAfterTheHour().Add(time.Millisecond * 100),
			expectActive:            1,
			expectRequeueAfter:      true,
			expectedRequeueDuration: 1*time.Hour - 1*time.Minute - time.Millisecond*100 + nextScheduleDelta,
		},
		"this started but is not present in cronjob active list, not past deadline, f": {
			concurrencyPolicy:       "Forbid",
			schedule:                onTheHour,
			deadline:                longDead,
			ranPreviously:           true,
			stillActive:             true,
			jobCreationTime:         topOfTheHour().Add(time.Millisecond * 100),
			now:                     justAfterTheHour().Add(time.Millisecond * 100),
			expectActive:            1,
			expectRequeueAfter:      true,
			expectedRequeueDuration: 1*time.Hour - 1*time.Minute - time.Millisecond*100 + nextScheduleDelta,
		},
		"this started but is not present in cronjob active list, not past deadline, R": {
			concurrencyPolicy:       "Replace",
			schedule:                onTheHour,
			deadline:                longDead,
			ranPreviously:           true,
			stillActive:             true,
			jobCreationTime:         topOfTheHour().Add(time.Millisecond * 100),
			now:                     justAfterTheHour().Add(time.Millisecond * 100),
			expectActive:            1,
			expectRequeueAfter:      true,
			expectedRequeueDuration: 1*time.Hour - 1*time.Minute - time.Millisecond*100 + nextScheduleDelta,
		},

		// Tests for @every-style schedule
		"with @every schedule, never ran, not time": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   noDead,
			cronjobCreationTime:        justBeforeTheHour(),
			jobCreationTime:            justBeforeTheHour(),
			now:                        *topOfTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"with @every schedule, never ran, is time": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   noDead,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeThePriorHour(),
			now:                        justBeforeTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
			expectCreate:               true,
			expectActive:               1,
			expectUpdateStatus:         true,
		},
		"with @every schedule, never ran, is time, past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   shortDead,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeThePriorHour(),
			now:                        justBeforeTheHour().Add(time.Second * time.Duration(shortDead+1)),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - time.Second*time.Duration(shortDead+1) + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"with @every schedule, never ran, is time, not past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   longDead,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeThePriorHour(),
			now:                        justBeforeTheHour().Add(time.Second * time.Duration(shortDead-1)),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - time.Second*time.Duration(shortDead-1) + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"with @every schedule, prev ran but done, not time": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   noDead,
			ranPreviously:              true,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeThePriorHour(),
			lastScheduleTime:           justBeforeTheHour(),
			now:                        *topOfTheHour(),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"with @every schedule, prev ran but done, is time": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   noDead,
			ranPreviously:              true,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeThePriorHour(),
			lastScheduleTime:           justBeforeTheHour(),
			now:                        topOfTheHour().Add(1 * time.Hour),
			expectCreate:               true,
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"with @every schedule, prev ran but done, is time, past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   shortDead,
			ranPreviously:              true,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeThePriorHour(),
			lastScheduleTime:           justBeforeTheHour(),
			now:                        justBeforeTheNextHour().Add(time.Second * time.Duration(shortDead+1)),
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - time.Second*time.Duration(shortDead+1) + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		// This test will fail: the logic around StartingDeadlineSecond in getNextScheduleTime messes up
		// the time that calculating schedule.Next(earliestTime) is based on. While this works perfectly
		// well for classic cron scheduled, with @every X, schedule.Next(earliestTime) just returns the time
		// offset by X relative to the earliestTime.
		// "with @every schedule, prev ran but done, is time, not past deadline": {
		// 	concurrencyPolicy:          "Allow",
		// 	schedule:                   everyHour,
		// 	deadline:                   shortDead,
		// 	ranPreviously:              true,
		// 	cronjobCreationTime:        justBeforeThePriorHour(),
		// 	jobCreationTime:            justBeforeThePriorHour(),
		// 	lastScheduleTime:           justBeforeTheHour(),
		// 	now:                        justBeforeTheNextHour().Add(time.Second * time.Duration(shortDead-1)),
		// 	expectCreate:               true,
		// 	expectActive:               1,
		// 	expectRequeueAfter:         true,
		// 	expectedRequeueDuration:    1*time.Hour - time.Second*time.Duration(shortDead-1) + nextScheduleDelta,
		// 	expectUpdateStatus:         true,
		// 	jobPresentInCJActiveStatus: true,
		// },
		"with @every schedule, still active, not time": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeTheHour(),
			lastScheduleTime:           justBeforeTheHour(),
			now:                        *topOfTheHour(),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 1*time.Minute + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"with @every schedule, still active, is time": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeThePriorHour(),
			lastScheduleTime:           justBeforeThePriorHour(),
			now:                        *justAfterTheHour(),
			expectCreate:               true,
			expectActive:               2,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - 2*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"with @every schedule, still active, is time, past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   shortDead,
			ranPreviously:              true,
			stillActive:                true,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeTheHour(),
			lastScheduleTime:           justBeforeTheHour(),
			now:                        justBeforeTheNextHour().Add(time.Second * time.Duration(shortDead+1)),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - time.Second*time.Duration(shortDead+1) + nextScheduleDelta,
			jobPresentInCJActiveStatus: true,
		},
		"with @every schedule, still active, is time, not past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   longDead,
			ranPreviously:              true,
			stillActive:                true,
			cronjobCreationTime:        justBeforeThePriorHour(),
			jobCreationTime:            justBeforeTheHour(),
			lastScheduleTime:           justBeforeTheHour(),
			now:                        justBeforeTheNextHour().Add(time.Second * time.Duration(shortDead-1)),
			expectCreate:               true,
			expectActive:               2,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - time.Second*time.Duration(shortDead-1) + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
		},
		"with @every schedule, prev ran but done, long overdue, no deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   noDead,
			ranPreviously:              true,
			cronjobCreationTime:        justAfterThePriorHour(),
			lastScheduleTime:           *justAfterTheHour(),
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour(),
			expectCreate:               true,
			expectActive:               1,
			expectedWarnings:           1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Minute + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"with @every schedule, prev ran but done, long overdue, past deadline": {
			concurrencyPolicy:          "Allow",
			schedule:                   everyHour,
			deadline:                   shortDead,
			ranPreviously:              true,
			cronjobCreationTime:        justAfterThePriorHour(),
			lastScheduleTime:           *justAfterTheHour(),
			jobCreationTime:            justAfterThePriorHour(),
			now:                        weekAfterTheHour().Add(1 * time.Minute).Add(time.Second * time.Duration(shortDead+1)),
			expectActive:               1,
			expectRequeueAfter:         true,
			expectedRequeueDuration:    1*time.Hour - time.Second*time.Duration(shortDead+1) + nextScheduleDelta,
			expectUpdateStatus:         true,
			jobPresentInCJActiveStatus: true,
			expectCompleted:            true,
		},
		"do nothing if the namespace is terminating": {
			jobCreateError: &errors.StatusError{ErrStatus: metav1.Status{Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{
				{
					Type:    v1.NamespaceTerminatingCause,
					Message: fmt.Sprintf("namespace %s is being terminated", metav1.NamespaceDefault),
					Field:   "metadata.namespace",
				}}}}},
			concurrencyPolicy:          "Allow",
			schedule:                   onTheHour,
			deadline:                   noDead,
			ranPreviously:              true,
			stillActive:                true,
			jobCreationTime:            justAfterThePriorHour(),
			now:                        *justAfterTheHour(),
			expectActive:               0,
			expectRequeueAfter:         false,
			expectUpdateStatus:         false,
			expectErr:                  true,
			jobPresentInCJActiveStatus: false,
		},
		"set lastsuccessfultime if successfulJobHistoryLimit is zero": {
			successfulJobsHistoryLimit: pointer.Int32(0),
			ranPreviously:              true,
			schedule:                   onTheHour,
			expectUpdateStatus:         true,
			expectCompleted:            true,
			jobPresentInCJActiveStatus: true,
		},
		"set lastsuccessfultime if successfulJobHistoryLimit is ten": {
			successfulJobsHistoryLimit: pointer.Int32(10),
			ranPreviously:              true,
			schedule:                   onTheHour,
			expectUpdateStatus:         true,
			expectCompleted:            true,
			jobPresentInCJActiveStatus: true,
		},
		"set lastsuccessfultime if successfulJobHistoryLimit is nil": {
			ranPreviously:              true,
			schedule:                   onTheHour,
			expectUpdateStatus:         true,
			expectCompleted:            true,
			jobPresentInCJActiveStatus: true,
		},
	}
	for name, tc := range testCases {
		name := name
		tc := tc

		t.Run(name, func(t *testing.T) {
			cj := cronJob()
			cj.Spec.ConcurrencyPolicy = tc.concurrencyPolicy
			cj.Spec.Suspend = &tc.suspend
			cj.Spec.Schedule = tc.schedule
			cj.Spec.TimeZone = tc.timeZone
			cj.Spec.SuccessfulJobsHistoryLimit = tc.successfulJobsHistoryLimit
			if tc.deadline != noDead {
				cj.Spec.StartingDeadlineSeconds = &tc.deadline
			}

			var (
				job *batchv1.Job
				err error
			)
			js := []*batchv1.Job{}
			realCJ := cj.DeepCopy()
			if tc.ranPreviously {
				cj.ObjectMeta.CreationTimestamp = metav1.Time{Time: justBeforeThePriorHour()}
				if !tc.cronjobCreationTime.IsZero() {
					cj.ObjectMeta.CreationTimestamp = metav1.Time{Time: tc.cronjobCreationTime}
				}
				cj.Status.LastScheduleTime = &metav1.Time{Time: justAfterThePriorHour()}
				if !tc.lastScheduleTime.IsZero() {
					cj.Status.LastScheduleTime = &metav1.Time{Time: tc.lastScheduleTime}
				}
				job, err = getJobFromTemplate2(&cj, tc.jobCreationTime)
				if err != nil {
					t.Fatalf("%s: unexpected error creating a job from template: %v", name, err)
				}
				job.UID = "1234"
				job.Namespace = cj.Namespace

				ref, err := getRef(job)
				if err != nil {
					t.Fatalf("%s: unexpected error getting the job object reference: %v", name, err)
				}
				if tc.jobPresentInCJActiveStatus {
					cj.Status.Active = []v1.ObjectReference{*ref}
				}

				if tc.stillActive {
					realCJ.Status.Active = []v1.ObjectReference{*ref}
					if !tc.jobStillNotFoundInLister {
						js = append(js, job)
					}
				} else {
					job.Status.CompletionTime = &metav1.Time{Time: job.ObjectMeta.CreationTimestamp.Add(time.Second * 10)}
					job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
						Type:   batchv1.JobComplete,
						Status: v1.ConditionTrue,
					})
					if !tc.jobStillNotFoundInLister {
						js = append(js, job)
					}
				}
			} else {
				cj.ObjectMeta.CreationTimestamp = metav1.Time{Time: justBeforeTheHour()}
				if !tc.cronjobCreationTime.IsZero() {
					cj.ObjectMeta.CreationTimestamp = metav1.Time{Time: tc.cronjobCreationTime}
				}
				if tc.stillActive {
					t.Errorf("%s: test setup error: this case makes no sense", name)
				}
			}

			jc := &fakeJobControl{Job: job, CreateErr: tc.jobCreateError, Err: tc.jobGetErr}
			cjc := &fakeCJControl{CronJob: realCJ}
			recorder := record.NewFakeRecorder(10)

			jm := ControllerV2{
				jobControl:     jc,
				cronJobControl: cjc,
				recorder:       recorder,
				now: func() time.Time {
					return tc.now
				},
			}
			cjCopy := cj.DeepCopy()
			requeueAfter, updateStatus, err := jm.syncCronJob(context.TODO(), cjCopy, js)
			if tc.expectErr && err == nil {
				t.Errorf("%s: expected error got none with requeueAfter time: %#v", name, requeueAfter)
			}
			if tc.expectRequeueAfter {
				if !reflect.DeepEqual(requeueAfter, &tc.expectedRequeueDuration) {
					t.Errorf("%s: expected requeueAfter: %+v, got requeueAfter time: %+v", name, tc.expectedRequeueDuration, requeueAfter)
				}
			}
			if updateStatus != tc.expectUpdateStatus {
				t.Errorf("%s: expected updateStatus: %t, actually: %t", name, tc.expectUpdateStatus, updateStatus)
			}
			expectedCreates := 0
			if tc.expectCreate {
				expectedCreates = 1
			}
			if tc.ranPreviously && !tc.stillActive {
				completionTime := tc.jobCreationTime.Add(10 * time.Second)
				if cjCopy.Status.LastSuccessfulTime == nil || !cjCopy.Status.LastSuccessfulTime.Time.Equal(completionTime) {
					t.Errorf("cj.status.lastSuccessfulTime: %s expected, got %#v", completionTime, cj.Status.LastSuccessfulTime)
				}
			}
			if len(jc.Jobs) != expectedCreates {
				t.Errorf("%s: expected %d job started, actually %v", name, expectedCreates, len(jc.Jobs))
			}
			for i := range jc.Jobs {
				job := &jc.Jobs[i]
				controllerRef := metav1.GetControllerOf(job)
				if controllerRef == nil {
					t.Errorf("%s: expected job to have ControllerRef: %#v", name, job)
				} else {
					if got, want := controllerRef.APIVersion, "batch/v1"; got != want {
						t.Errorf("%s: controllerRef.APIVersion = %q, want %q", name, got, want)
					}
					if got, want := controllerRef.Kind, "CronJob"; got != want {
						t.Errorf("%s: controllerRef.Kind = %q, want %q", name, got, want)
					}
					if got, want := controllerRef.Name, cj.Name; got != want {
						t.Errorf("%s: controllerRef.Name = %q, want %q", name, got, want)
					}
					if got, want := controllerRef.UID, cj.UID; got != want {
						t.Errorf("%s: controllerRef.UID = %q, want %q", name, got, want)
					}
					if controllerRef.Controller == nil || *controllerRef.Controller != true {
						t.Errorf("%s: controllerRef.Controller is not set to true", name)
					}
				}
			}

			expectedDeletes := 0
			if tc.expectDelete {
				expectedDeletes = 1
			}
			if len(jc.DeleteJobName) != expectedDeletes {
				t.Errorf("%s: expected %d job deleted, actually %v", name, expectedDeletes, len(jc.DeleteJobName))
			}

			// Status update happens once when ranging through job list, and another one if create jobs.
			expectUpdates := 1
			expectedEvents := 0
			if tc.expectCreate {
				expectedEvents++
				expectUpdates++
			}
			if tc.expectDelete {
				expectedEvents++
			}
			if tc.expectCompleted {
				expectedEvents++
			}
			if name == "still active, is time, F" {
				// this is the only test case where we would raise an event for not scheduling
				expectedEvents++
			}
			expectedEvents += tc.expectedWarnings

			if len(recorder.Events) != expectedEvents {
				t.Errorf("%s: expected %d event, actually %v", name, expectedEvents, len(recorder.Events))
			}

			numWarnings := 0
			for i := 1; i <= len(recorder.Events); i++ {
				e := <-recorder.Events
				if strings.HasPrefix(e, v1.EventTypeWarning) {
					numWarnings++
				}
			}
			if numWarnings != tc.expectedWarnings {
				t.Errorf("%s: expected %d warnings, actually %v", name, tc.expectedWarnings, numWarnings)
			}

			if len(cjc.Updates) == expectUpdates && tc.expectActive != len(cjc.Updates[expectUpdates-1].Status.Active) {
				t.Errorf("%s: expected Active size %d, got %d", name, tc.expectActive, len(cjc.Updates[expectUpdates-1].Status.Active))
			}

			if &cj == cjCopy {
				t.Errorf("syncCronJob is not creating a copy of the original cronjob")
			}
		})
	}

}

type fakeQueue struct {
	workqueue.TypedRateLimitingInterface[string]
	delay time.Duration
	key   interface{}
}

func (f *fakeQueue) AddAfter(key string, delay time.Duration) {
	f.delay = delay
	f.key = key
}

// this test will take around 61 seconds to complete
func TestControllerV2UpdateCronJob(t *testing.T) {
	tests := []struct {
		name          string
		oldCronJob    *batchv1.CronJob
		newCronJob    *batchv1.CronJob
		expectedDelay time.Duration
	}{
		{
			name: "spec.template changed",
			oldCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "b"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
			},
			newCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "foo"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
			},
			expectedDelay: 0 * time.Second,
		},
		{
			name: "spec.schedule changed",
			oldCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "30 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "b"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
				Status: batchv1.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: justBeforeTheHour()},
				},
			},
			newCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/1 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "foo"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
				Status: batchv1.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: justBeforeTheHour()},
				},
			},
			expectedDelay: 1*time.Second + nextScheduleDelta,
		},
		{
			name: "spec.schedule with @every changed - cadence decrease",
			oldCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "@every 1m",
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "b"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
				Status: batchv1.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: justBeforeTheHour()},
				},
			},
			newCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "@every 3m",
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "foo"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
				Status: batchv1.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: justBeforeTheHour()},
				},
			},
			expectedDelay: 2*time.Minute + 1*time.Second + nextScheduleDelta,
		},
		{
			name: "spec.schedule with @every changed - cadence increase",
			oldCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "@every 3m",
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "b"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
				Status: batchv1.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: justBeforeTheHour()},
				},
			},
			newCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "@every 1m",
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "foo"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
				Status: batchv1.CronJobStatus{
					LastScheduleTime: &metav1.Time{Time: justBeforeTheHour()},
				},
			},
			expectedDelay: 1*time.Second + nextScheduleDelta,
		},
		{
			name: "spec.timeZone not changed",
			oldCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					TimeZone: &newYork,
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "b"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
			},
			newCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					TimeZone: &newYork,
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "foo"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
			},
			expectedDelay: 0 * time.Second,
		},
		{
			name: "spec.timeZone changed",
			oldCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					TimeZone: &newYork,
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "b"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
			},
			newCronJob: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					TimeZone: nil,
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"a": "foo"},
							Annotations: map[string]string{"x": "y"},
						},
						Spec: jobSpec(),
					},
				},
			},
			expectedDelay: 0 * time.Second,
		},

		// TODO: Add more test cases for updating scheduling.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			kubeClient := fake.NewSimpleClientset()
			sharedInformers := informers.NewSharedInformerFactory(kubeClient, controller.NoResyncPeriodFunc())
			jm, err := NewControllerV2(ctx, sharedInformers.Batch().V1().Jobs(), sharedInformers.Batch().V1().CronJobs(), kubeClient)
			if err != nil {
				t.Errorf("unexpected error %v", err)
				return
			}
			jm.now = justASecondBeforeTheHour
			queue := &fakeQueue{TypedRateLimitingInterface: workqueue.NewTypedRateLimitingQueueWithConfig(
				workqueue.DefaultTypedControllerRateLimiter[string](),
				workqueue.TypedRateLimitingQueueConfig[string]{
					Name: "test-update-cronjob",
				},
			)}
			jm.queue = queue
			jm.jobControl = &fakeJobControl{}
			jm.cronJobControl = &fakeCJControl{}
			jm.recorder = record.NewFakeRecorder(10)

			jm.updateCronJob(logger, tt.oldCronJob, tt.newCronJob)
			if queue.delay.Seconds() != tt.expectedDelay.Seconds() {
				t.Errorf("Expected delay %#v got %#v", tt.expectedDelay.Seconds(), queue.delay.Seconds())
			}
		})
	}
}

func TestControllerV2GetJobsToBeReconciled(t *testing.T) {
	trueRef := true
	tests := []struct {
		name     string
		cronJob  *batchv1.CronJob
		jobs     []runtime.Object
		expected []*batchv1.Job
	}{
		{
			name:    "test getting jobs in namespace without controller reference",
			cronJob: &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "foo-ns", Name: "fooer"}},
			jobs: []runtime.Object{
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "foo-ns"}},
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "foo-ns"}},
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo2", Namespace: "foo-ns"}},
			},
			expected: []*batchv1.Job{},
		},
		{
			name:    "test getting jobs in namespace with a controller reference",
			cronJob: &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "foo-ns", Name: "fooer"}},
			jobs: []runtime.Object{
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "foo-ns"}},
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "foo-ns",
					OwnerReferences: []metav1.OwnerReference{{Name: "fooer", Controller: &trueRef}}}},
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo2", Namespace: "foo-ns"}},
			},
			expected: []*batchv1.Job{
				{ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "foo-ns",
					OwnerReferences: []metav1.OwnerReference{{Name: "fooer", Controller: &trueRef}}}},
			},
		},
		{
			name:    "test getting jobs in other namespaces",
			cronJob: &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "foo-ns", Name: "fooer"}},
			jobs: []runtime.Object{
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar-ns"}},
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "bar-ns"}},
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo2", Namespace: "bar-ns"}},
			},
			expected: []*batchv1.Job{},
		},
		{
			name: "test getting jobs whose labels do not match job template",
			cronJob: &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Namespace: "foo-ns", Name: "fooer"},
				Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"key": "value"}},
				}},
			},
			jobs: []runtime.Object{
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
					Namespace:       "foo-ns",
					Name:            "foo-fooer-owner-ref",
					Labels:          map[string]string{"key": "different-value"},
					OwnerReferences: []metav1.OwnerReference{{Name: "fooer", Controller: &trueRef}}},
				},
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
					Namespace:       "foo-ns",
					Name:            "foo-other-owner-ref",
					Labels:          map[string]string{"key": "different-value"},
					OwnerReferences: []metav1.OwnerReference{{Name: "another-cronjob", Controller: &trueRef}}},
				},
			},
			expected: []*batchv1.Job{{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       "foo-ns",
					Name:            "foo-fooer-owner-ref",
					Labels:          map[string]string{"key": "different-value"},
					OwnerReferences: []metav1.OwnerReference{{Name: "fooer", Controller: &trueRef}}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			kubeClient := fake.NewSimpleClientset()
			sharedInformers := informers.NewSharedInformerFactory(kubeClient, controller.NoResyncPeriodFunc())
			for _, job := range tt.jobs {
				sharedInformers.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
			}
			jm, err := NewControllerV2(ctx, sharedInformers.Batch().V1().Jobs(), sharedInformers.Batch().V1().CronJobs(), kubeClient)
			if err != nil {
				t.Errorf("unexpected error %v", err)
				return
			}

			actual, err := jm.getJobsToBeReconciled(tt.cronJob)
			if err != nil {
				t.Errorf("unexpected error %v", err)
				return
			}
			if !reflect.DeepEqual(actual, tt.expected) {
				t.Errorf("\nExpected %#v,\nbut got %#v", tt.expected, actual)
			}
		})
	}
}

func TestControllerV2CleanupFinishedJobs(t *testing.T) {
	tests := []struct {
		name                string
		now                 time.Time
		cronJob             *batchv1.CronJob
		finishedJobs        []*batchv1.Job
		jobCreateError      error
		expectedDeletedJobs []string
	}{
		{
			name: "jobs are still deleted when a cronjob can't create jobs due to jobs quota being reached (avoiding a deadlock)",
			now:  *justAfterTheHour(),
			cronJob: &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Namespace: "foo-ns", Name: "fooer"},
				Spec: batchv1.CronJobSpec{
					Schedule:                   onTheHour,
					SuccessfulJobsHistoryLimit: pointer.Int32(1),
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"key": "value"}},
					},
				},
				Status: batchv1.CronJobStatus{LastScheduleTime: &metav1.Time{Time: justAfterThePriorHour()}},
			},
			finishedJobs: []*batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "foo-ns",
						Name:            "finished-job-started-hour-ago",
						OwnerReferences: []metav1.OwnerReference{{Name: "fooer", Controller: pointer.Bool(true)}},
					},
					Status: batchv1.JobStatus{StartTime: &metav1.Time{Time: justBeforeThePriorHour()}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "foo-ns",
						Name:            "finished-job-started-minute-ago",
						OwnerReferences: []metav1.OwnerReference{{Name: "fooer", Controller: pointer.Bool(true)}},
					},
					Status: batchv1.JobStatus{StartTime: &metav1.Time{Time: justBeforeTheHour()}},
				},
			},
			jobCreateError:      errors.NewInternalError(fmt.Errorf("quota for # of jobs reached")),
			expectedDeletedJobs: []string{"finished-job-started-hour-ago"},
		},
		{
			name: "jobs are not deleted if history limit not reached",
			now:  justBeforeTheHour(),
			cronJob: &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Namespace: "foo-ns", Name: "fooer"},
				Spec: batchv1.CronJobSpec{
					Schedule:                   onTheHour,
					SuccessfulJobsHistoryLimit: pointer.Int32(2),
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"key": "value"}},
					},
				},
				Status: batchv1.CronJobStatus{LastScheduleTime: &metav1.Time{Time: justAfterThePriorHour()}},
			},
			finishedJobs: []*batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "foo-ns",
						Name:            "finished-job-started-hour-ago",
						OwnerReferences: []metav1.OwnerReference{{Name: "fooer", Controller: pointer.Bool(true)}},
					},
					Status: batchv1.JobStatus{StartTime: &metav1.Time{Time: justBeforeThePriorHour()}},
				},
			},
			jobCreateError:      nil,
			expectedDeletedJobs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)

			for _, job := range tt.finishedJobs {
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: v1.ConditionTrue}}
			}

			client := fake.NewSimpleClientset()

			informerFactory := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())
			_ = informerFactory.Batch().V1().CronJobs().Informer().GetIndexer().Add(tt.cronJob)
			for _, job := range tt.finishedJobs {
				_ = informerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
			}

			jm, err := NewControllerV2(ctx, informerFactory.Batch().V1().Jobs(), informerFactory.Batch().V1().CronJobs(), client)
			if err != nil {
				t.Errorf("unexpected error %v", err)
				return
			}
			jobControl := &fakeJobControl{CreateErr: tt.jobCreateError}
			jm.jobControl = jobControl
			jm.now = func() time.Time {
				return tt.now
			}

			jm.enqueueController(tt.cronJob)
			jm.processNextWorkItem(ctx)

			if len(tt.expectedDeletedJobs) != len(jobControl.DeleteJobName) {
				t.Fatalf("expected '%v' jobs to be deleted, instead deleted '%s'", tt.expectedDeletedJobs, jobControl.DeleteJobName)
			}
			sort.Strings(jobControl.DeleteJobName)
			sort.Strings(tt.expectedDeletedJobs)
			for i, deletedJob := range jobControl.DeleteJobName {
				if deletedJob != tt.expectedDeletedJobs[i] {
					t.Fatalf("expected '%v' jobs to be deleted, instead deleted '%s'", tt.expectedDeletedJobs, jobControl.DeleteJobName)
				}
			}
		})
	}
}

// TestControllerV2JobAlreadyExistsButNotInActiveStatus validates that an already created job that was not added to the status
// of a CronJob initially will be added back on the next sync. Previously, if we failed to update the status after creating a job,
// cronjob controller would retry continuously because it would attempt to create a job that already exists.
func TestControllerV2JobAlreadyExistsButNotInActiveStatus(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)

	cj := cronJob()
	cj.Spec.ConcurrencyPolicy = "Forbid"
	cj.Spec.Schedule = everyHour
	cj.Status.LastScheduleTime = &metav1.Time{Time: justBeforeThePriorHour()}
	cj.Status.Active = []v1.ObjectReference{}
	cjCopy := cj.DeepCopy()

	job, err := getJobFromTemplate2(&cj, justAfterThePriorHour())
	if err != nil {
		t.Fatalf("Unexpected error creating a job from template: %v", err)
	}
	job.UID = "1234"
	job.Namespace = cj.Namespace

	client := fake.NewSimpleClientset(cjCopy, job)
	informerFactory := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())
	_ = informerFactory.Batch().V1().CronJobs().Informer().GetIndexer().Add(cjCopy)

	jm, err := NewControllerV2(ctx, informerFactory.Batch().V1().Jobs(), informerFactory.Batch().V1().CronJobs(), client)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	jobControl := &fakeJobControl{Job: job, CreateErr: errors.NewAlreadyExists(schema.GroupResource{Resource: "job", Group: "batch"}, "")}
	jm.jobControl = jobControl
	cronJobControl := &fakeCJControl{}
	jm.cronJobControl = cronJobControl
	jm.now = justBeforeTheHour

	jm.enqueueController(cjCopy)
	jm.processNextWorkItem(ctx)

	if len(cronJobControl.Updates) != 1 {
		t.Fatalf("Unexpected updates to cronjob, got: %d, expected 1", len(cronJobControl.Updates))
	}
	if len(cronJobControl.Updates[0].Status.Active) != 1 {
		t.Errorf("Unexpected active jobs count, got: %d, expected 1", len(cronJobControl.Updates[0].Status.Active))
	}

	expectedActiveRef, err := getRef(job)
	if err != nil {
		t.Fatalf("Error getting expected job ref: %v", err)
	}
	if !reflect.DeepEqual(cronJobControl.Updates[0].Status.Active[0], *expectedActiveRef) {
		t.Errorf("Unexpected job reference in cronjob active list, got: %v, expected: %v", cronJobControl.Updates[0].Status.Active[0], expectedActiveRef)
	}
}

// TestControllerV2JobAlreadyExistsButDifferentOwner validates that an already created job
// not owned by the cronjob controller is ignored.
func TestControllerV2JobAlreadyExistsButDifferentOwner(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)

	cj := cronJob()
	cj.Spec.ConcurrencyPolicy = "Forbid"
	cj.Spec.Schedule = everyHour
	cj.Status.LastScheduleTime = &metav1.Time{Time: justBeforeThePriorHour()}
	cj.Status.Active = []v1.ObjectReference{}
	cjCopy := cj.DeepCopy()

	job, err := getJobFromTemplate2(&cj, justAfterThePriorHour())
	if err != nil {
		t.Fatalf("Unexpected error creating a job from template: %v", err)
	}
	job.UID = "1234"
	job.Namespace = cj.Namespace

	// remove owners for this test since we are testing that jobs not belonging to cronjob
	// controller are safely ignored
	job.OwnerReferences = []metav1.OwnerReference{}

	client := fake.NewSimpleClientset(cjCopy, job)
	informerFactory := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())
	_ = informerFactory.Batch().V1().CronJobs().Informer().GetIndexer().Add(cjCopy)

	jm, err := NewControllerV2(ctx, informerFactory.Batch().V1().Jobs(), informerFactory.Batch().V1().CronJobs(), client)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	jobControl := &fakeJobControl{Job: job, CreateErr: errors.NewAlreadyExists(schema.GroupResource{Resource: "job", Group: "batch"}, "")}
	jm.jobControl = jobControl
	cronJobControl := &fakeCJControl{}
	jm.cronJobControl = cronJobControl
	jm.now = justBeforeTheHour

	jm.enqueueController(cjCopy)
	jm.processNextWorkItem(ctx)

	if len(cronJobControl.Updates) != 0 {
		t.Fatalf("Unexpected updates to cronjob, got: %d, expected 0", len(cronJobControl.Updates))
	}
}
