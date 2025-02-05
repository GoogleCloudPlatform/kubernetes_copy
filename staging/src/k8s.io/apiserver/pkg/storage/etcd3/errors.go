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

package etcd3

import (
	goerrors "errors"
	"net/http"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/storage"

	etcdrpc "go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

func interpretWatchError(err error) error {
	switch {
	case err == etcdrpc.ErrCompacted:
		return errors.NewResourceExpired("The resourceVersion for the provided watch is too old.")
	}

	var corruptobjDeletedErr *corruptObjectDeletedError
	if goerrors.As(err, &corruptobjDeletedErr) {
		return &errors.StatusError{
			ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    http.StatusInternalServerError,
				Reason:  metav1.StatusReasonStoreReadError,
				Message: corruptobjDeletedErr.Error(),
			},
		}
	}

	return err
}

const (
	expired         string = "The resourceVersion for the provided list is too old."
	continueExpired string = "The provided continue parameter is too old " +
		"to display a consistent list result. You can start a new list without " +
		"the continue parameter."
	inconsistentContinue string = "The provided continue parameter is too old " +
		"to display a consistent list result. You can start a new list without " +
		"the continue parameter, or use the continue token in this response to " +
		"retrieve the remainder of the results. Continuing with the provided " +
		"token results in an inconsistent list - objects that were created, " +
		"modified, or deleted between the time the first chunk was returned " +
		"and now may show up in the list."
)

func interpretListError(err error, paging bool, continueKey, keyPrefix string) error {
	switch {
	case err == etcdrpc.ErrCompacted:
		if paging {
			return handleCompactedErrorForPaging(continueKey, keyPrefix)
		}
		return errors.NewResourceExpired(expired)
	}
	return err
}

func handleCompactedErrorForPaging(continueKey, keyPrefix string) error {
	// continueToken.ResourceVersion=-1 means that the apiserver can
	// continue the list at the latest resource version. We don't use rv=0
	// for this purpose to distinguish from a bad token that has empty rv.
	newToken, err := storage.EncodeContinue(continueKey, keyPrefix, -1)
	if err != nil {
		utilruntime.HandleError(err)
		return errors.NewResourceExpired(continueExpired)
	}
	statusError := errors.NewResourceExpired(inconsistentContinue)
	statusError.ErrStatus.ListMeta.Continue = newToken
	return statusError
}
