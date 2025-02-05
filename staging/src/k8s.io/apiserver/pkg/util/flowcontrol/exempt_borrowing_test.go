/*
Copyright 2024 The Kubernetes Authors.

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
	"testing"
	"time"

	fcboot "k8s.io/apiserver/pkg/apis/flowcontrol/bootstrap"
	fqs "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/queueset"
	testeventclock "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/testing/eventclock"
	"k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"

	flowcontrol "k8s.io/api/flowcontrol/v1"
)

func TestUpdateBorrowing(t *testing.T) {
	startTime := time.Now()
	clk, _ := testeventclock.NewFake(startTime, 0, nil)
	plcExempt := fcboot.MandatoryPriorityLevelConfigurationExempt
	plcHigh := fcboot.SuggestedPriorityLevelConfigurationWorkloadHigh
	plcMid := fcboot.SuggestedPriorityLevelConfigurationWorkloadLow
	plcLow := fcboot.MandatoryPriorityLevelConfigurationCatchAll
	plcs := []*flowcontrol.PriorityLevelConfiguration{plcHigh, plcExempt, plcMid, plcLow}
	fses := []*flowcontrol.FlowSchema{}
	k8sClient := clientsetfake.NewSimpleClientset(plcLow, plcExempt, plcHigh, plcMid)
	informerFactory := informers.NewSharedInformerFactory(k8sClient, 0)
	flowcontrolClient := k8sClient.FlowcontrolV1()
	serverCL := int(*plcHigh.Spec.Limited.NominalConcurrencyShares+
		*plcMid.Spec.Limited.NominalConcurrencyShares+
		*plcLow.Spec.Limited.NominalConcurrencyShares) * 6
	config := TestableConfig{
		Name:                   "test",
		Clock:                  clk,
		AsFieldManager:         "testfm",
		FoundToDangling:        func(found bool) bool { return !found },
		InformerFactory:        informerFactory,
		FlowcontrolClient:      flowcontrolClient,
		ServerConcurrencyLimit: serverCL,
		ReqsGaugeVec:           metrics.PriorityLevelConcurrencyGaugeVec,
		ExecSeatsGaugeVec:      metrics.PriorityLevelExecutionSeatsGaugeVec,
		QueueSetFactory:        fqs.NewQueueSetFactory(clk),
	}
	ctlr := newTestableController(config)
	_ = ctlr.lockAndDigestConfigObjects(plcs, fses)
	if ctlr.nominalCLSum != serverCL {
		t.Fatalf("Unexpected rounding: nominalCLSum=%d", ctlr.nominalCLSum)
	}
	stateExempt := ctlr.priorityLevelStates[plcExempt.Name]
	stateHigh := ctlr.priorityLevelStates[plcHigh.Name]
	stateMid := ctlr.priorityLevelStates[plcMid.Name]
	stateLow := ctlr.priorityLevelStates[plcLow.Name]

	// Scenario 1: everybody wants more than ServerConcurrencyLimit.
	// Test the case of exempt borrowing so much that less than minCL
	// is available to each non-exempt.
	stateExempt.seatDemandIntegrator.Set(float64(serverCL + 100))
	stateHigh.seatDemandIntegrator.Set(float64(serverCL + 100))
	stateMid.seatDemandIntegrator.Set(float64(serverCL + 100))
	stateLow.seatDemandIntegrator.Set(float64(serverCL + 100))
	clk.SetTime(startTime.Add(borrowingAdjustmentPeriod))
	ctlr.updateBorrowing()
	if expected, actual := serverCL+100, stateExempt.currentCL; expected != actual {
		t.Errorf("Scenario 1: expected %d, got %d for exempt", expected, actual)
	} else {
		t.Logf("Scenario 1: expected and got %d for exempt", expected)
	}
	if expected, actual := stateHigh.minCL, stateHigh.currentCL; expected != actual {
		t.Errorf("Scenario 1: expected %d, got %d for hi", expected, actual)
	} else {
		t.Logf("Scenario 1: expected and got %d for hi", expected)
	}
	if expected, actual := stateMid.minCL, stateMid.currentCL; expected != actual {
		t.Errorf("Scenario 1: expected %d, got %d for mid", expected, actual)
	} else {
		t.Logf("Scenario 1: expected and got %d for mid", expected)
	}
	if expected, actual := stateLow.minCL, stateLow.currentCL; expected != actual {
		t.Errorf("Scenario 1: expected %d, got %d for lo", expected, actual)
	} else {
		t.Logf("Scenario 1: expected and got %d for lo", expected)
	}

	// Scenario 2: non-exempt want more than serverCL but get halfway between minCL and minCurrentCL.
	expectedHigh := (stateHigh.nominalCL + stateHigh.minCL) / 2
	expectedMid := (stateMid.nominalCL + stateMid.minCL) / 2
	expectedLow := (stateLow.nominalCL + stateLow.minCL) / 2
	expectedExempt := serverCL - (expectedHigh + expectedMid + expectedLow)
	stateExempt.seatDemandIntegrator.Set(float64(expectedExempt))
	clk.SetTime(startTime.Add(2 * borrowingAdjustmentPeriod))
	ctlr.updateBorrowing()
	clk.SetTime(startTime.Add(3 * borrowingAdjustmentPeriod))
	ctlr.updateBorrowing()
	if expected, actual := expectedExempt, stateExempt.currentCL; expected != actual {
		t.Errorf("Scenario 2: expected %d, got %d for exempt", expected, actual)
	} else {
		t.Logf("Scenario 2: expected and got %d for exempt", expected)
	}
	if expected, actual := expectedHigh, stateHigh.currentCL; expected != actual {
		t.Errorf("Scenario 2: expected %d, got %d for hi", expected, actual)
	} else {
		t.Logf("Scenario 2: expected and got %d for hi", expected)
	}
	if expected, actual := expectedMid, stateMid.currentCL; expected != actual {
		t.Errorf("Scenario 2: expected %d, got %d for mid", expected, actual)
	} else {
		t.Logf("Scenario 2: expected and got %d for mid", expected)
	}
	if expected, actual := expectedLow, stateLow.currentCL; expected != actual {
		t.Errorf("Scenario 2: expected %d, got %d for lo", expected, actual)
	} else {
		t.Logf("Scenario 2: expected and got %d for lo", expected)
	}

	// Scenario 3: only mid is willing to lend, and exempt borrows all of that.
	// Test the case of regular borrowing.
	expectedHigh = stateHigh.nominalCL
	expectedMid = stateMid.minCL
	expectedLow = stateLow.nominalCL
	expectedExempt = serverCL - (expectedHigh + expectedMid + expectedLow)
	stateExempt.seatDemandIntegrator.Set(float64(expectedExempt))
	stateMid.seatDemandIntegrator.Set(float64(1))
	clk.SetTime(startTime.Add(4 * borrowingAdjustmentPeriod))
	ctlr.updateBorrowing()
	clk.SetTime(startTime.Add(5 * borrowingAdjustmentPeriod))
	ctlr.updateBorrowing()
	if expected, actual := expectedExempt, stateExempt.currentCL; expected != actual {
		t.Errorf("Scenario 3: expected %d, got %d for exempt", expected, actual)
	} else {
		t.Logf("Scenario 3: expected and got %d for exempt", expected)
	}
	if expected, actual := expectedHigh, stateHigh.currentCL; expected != actual {
		t.Errorf("Scenario 3: expected %d, got %d for hi", expected, actual)
	} else {
		t.Logf("Scenario 3: expected and got %d for hi", expected)
	}
	if expected, actual := expectedMid, stateMid.currentCL; expected != actual {
		t.Errorf("Scenario 3: expected %d, got %d for mid", expected, actual)
	} else {
		t.Logf("Scenario 3: expected and got %d for mid", expected)
	}
	if expected, actual := expectedLow, stateLow.currentCL; expected != actual {
		t.Errorf("Scenario 3: expected %d, got %d for lo", expected, actual)
	} else {
		t.Logf("Scenario 3: expected and got %d for lo", expected)
	}

}

func TestMaxCL(t *testing.T) {
	startTime := time.Now()
	clk, _ := testeventclock.NewFake(startTime, 0, nil)
	plcExempt := fcboot.MandatoryPriorityLevelConfigurationExempt
	plcHigh := fcboot.SuggestedPriorityLevelConfigurationWorkloadHigh
	plcMid := fcboot.SuggestedPriorityLevelConfigurationWorkloadLow
	plcLow := fcboot.MandatoryPriorityLevelConfigurationCatchAll
	plcs := []*flowcontrol.PriorityLevelConfiguration{plcHigh, plcExempt, plcMid, plcLow}
	fses := []*flowcontrol.FlowSchema{}
	k8sClient := clientsetfake.NewSimpleClientset(plcLow, plcExempt, plcHigh, plcMid)
	informerFactory := informers.NewSharedInformerFactory(k8sClient, 0)
	flowcontrolClient := k8sClient.FlowcontrolV1()
	serverCL := int(*plcHigh.Spec.Limited.NominalConcurrencyShares+
		*plcMid.Spec.Limited.NominalConcurrencyShares+
		*plcLow.Spec.Limited.NominalConcurrencyShares) * 6
	config := TestableConfig{
		Name:                   "test",
		Clock:                  clk,
		AsFieldManager:         "testfm",
		FoundToDangling:        func(found bool) bool { return !found },
		InformerFactory:        informerFactory,
		FlowcontrolClient:      flowcontrolClient,
		ServerConcurrencyLimit: serverCL,
		ReqsGaugeVec:           metrics.PriorityLevelConcurrencyGaugeVec,
		ExecSeatsGaugeVec:      metrics.PriorityLevelExecutionSeatsGaugeVec,
		QueueSetFactory:        fqs.NewQueueSetFactory(clk),
	}
	ctlr := newTestableController(config)
	_ = ctlr.lockAndDigestConfigObjects(plcs, fses)
	if ctlr.nominalCLSum != serverCL {
		t.Fatalf("Unexpected rounding: nominalCLSum=%d", ctlr.nominalCLSum)
	}
	stateExempt := ctlr.priorityLevelStates[plcExempt.Name]
	stateHigh := ctlr.priorityLevelStates[plcHigh.Name]
	stateMid := ctlr.priorityLevelStates[plcMid.Name]
	stateLow := ctlr.priorityLevelStates[plcLow.Name]

	expectedExempt := serverCL
	expectedHigh := stateHigh.nominalCL + int(float64(stateMid.nominalCL)*float64(*stateMid.pl.Spec.Limited.LendablePercent)/100) + int(float64(stateLow.nominalCL)*float64(*stateLow.pl.Spec.Limited.LendablePercent)/100)
	expectedMid := int(float64(stateHigh.nominalCL)*float64(*stateHigh.pl.Spec.Limited.LendablePercent)/100) + stateMid.nominalCL + int(float64(stateLow.nominalCL)*float64(*stateLow.pl.Spec.Limited.LendablePercent)/100)
	expectedLow := int(float64(stateHigh.nominalCL)*float64(*stateHigh.pl.Spec.Limited.LendablePercent)/100) + int(float64(stateMid.nominalCL)*float64(*stateMid.pl.Spec.Limited.LendablePercent)/100) + stateLow.nominalCL
	if expected, actual := expectedExempt, stateExempt.maxCL; expected != actual {
		t.Errorf("MaxCL: expected %d, got %d for exempt", expected, actual)
	} else {
		t.Logf("MaxCL: expected and got %d for exempt", expected)
	}
	if expected, actual := expectedHigh, stateHigh.maxCL; expected != actual {
		t.Errorf("MaxCL: expected %d, got %d for hi", expected, actual)
	} else {
		t.Logf("MaxCL: expected and got %d for hi", expected)
	}
	if expected, actual := expectedMid, stateMid.maxCL; expected != actual {
		t.Errorf("MaxCL: expected %d, got %d for mid", expected, actual)
	} else {
		t.Logf("MaxCL: expected and got %d for mid", expected)
	}
	if expected, actual := expectedLow, stateLow.maxCL; expected != actual {
		t.Errorf("MaxCL: expected %d, got %d for lo", expected, actual)
	} else {
		t.Logf("MaxCL: expected and got %d for lo", expected)
	}
}
