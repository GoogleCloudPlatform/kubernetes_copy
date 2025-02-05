/*
Copyright 2022 The Kubernetes Authors.

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

package bootstrap

import (
	"testing"

	flowcontrol "k8s.io/api/flowcontrol/v1"
)

func TestBootstrapPriorityLevelConfigurationWithBorrowing(t *testing.T) {
	tests := []struct {
		name                    string
		nominalSharesExpected   int32
		lendablePercentexpected int32
	}{
		{
			name:                    "leader-election",
			nominalSharesExpected:   40,
			lendablePercentexpected: 0,
		},
		{
			name:                    "node-high",
			nominalSharesExpected:   40,
			lendablePercentexpected: 25,
		},
		{
			name:                    "system",
			nominalSharesExpected:   30,
			lendablePercentexpected: 33,
		},
		{
			name:                    "workload-high",
			nominalSharesExpected:   40,
			lendablePercentexpected: 50,
		},
		{
			name:                    "workload-low",
			nominalSharesExpected:   100,
			lendablePercentexpected: 90,
		},
		{
			name:                    "global-default",
			nominalSharesExpected:   20,
			lendablePercentexpected: 50,
		},
		{
			name:                    "catch-all",
			nominalSharesExpected:   5,
			lendablePercentexpected: 0,
		},
	}

	bootstrapPLs := func() map[string]*flowcontrol.PriorityLevelConfiguration {
		list := make([]*flowcontrol.PriorityLevelConfiguration, 0)
		list = append(list, MandatoryPriorityLevelConfigurations...)
		list = append(list, SuggestedPriorityLevelConfigurations...)

		m := map[string]*flowcontrol.PriorityLevelConfiguration{}
		for i := range list {
			m[list[i].Name] = list[i]
		}
		return m
	}()

	for _, test := range tests {
		bootstrapPL := bootstrapPLs[test.name]
		if bootstrapPL == nil {
			t.Errorf("Expected bootstrap PriorityLevelConfiguration %q, but not found in bootstrap configuration", test.name)
			continue
		}
		delete(bootstrapPLs, test.name)

		if bootstrapPL.Spec.Type != flowcontrol.PriorityLevelEnablementLimited {
			t.Errorf("bootstrap PriorityLevelConfiguration %q is not %q", test.name, flowcontrol.PriorityLevelEnablementLimited)
			continue
		}
		if test.nominalSharesExpected != *bootstrapPL.Spec.Limited.NominalConcurrencyShares {
			t.Errorf("bootstrap PriorityLevelConfiguration %q: expected NominalConcurrencyShares: %d, but got: %d", test.name, test.nominalSharesExpected, bootstrapPL.Spec.Limited.NominalConcurrencyShares)
		}
		if test.lendablePercentexpected != *bootstrapPL.Spec.Limited.LendablePercent {
			t.Errorf("bootstrap PriorityLevelConfiguration %q: expected NominalConcurrencyShares: %d, but got: %d", test.name, test.lendablePercentexpected, bootstrapPL.Spec.Limited.LendablePercent)
		}
		if bootstrapPL.Spec.Limited.BorrowingLimitPercent != nil {
			t.Errorf("bootstrap PriorityLevelConfiguration %q: expected BorrowingLimitPercent to be nil, but got: %d", test.name, *bootstrapPL.Spec.Limited.BorrowingLimitPercent)
		}
	}

	if len(bootstrapPLs) != 0 {
		names := make([]string, 0)
		for name, bpl := range bootstrapPLs {
			if bpl.Spec.Type == flowcontrol.PriorityLevelEnablementExempt {
				t.Logf("bootstrap PriorityLevelConfiguration %q is of %q type, skipped", name, flowcontrol.PriorityLevelConfigurationNameExempt)
				continue
			}
			names = append(names, name)
		}

		if len(names) != 0 {
			t.Errorf("bootstrap PriorityLevelConfiguration objects not accounted by this test: %v", names)
		}
	}
	exemptPL := MandatoryPriorityLevelConfigurationExempt
	if exemptPL.Spec.Exempt.NominalConcurrencyShares != nil && *exemptPL.Spec.Exempt.NominalConcurrencyShares != 0 {
		t.Errorf("Expected exempt priority level to have NominalConcurrencyShares==0 but got %d instead", *exemptPL.Spec.Exempt.NominalConcurrencyShares)
	}
	if exemptPL.Spec.Exempt.LendablePercent != nil && *exemptPL.Spec.Exempt.LendablePercent != 0 {
		t.Errorf("Expected exempt priority level to have LendablePercent==0 but got %d instead", *exemptPL.Spec.Exempt.LendablePercent)
	}
}
