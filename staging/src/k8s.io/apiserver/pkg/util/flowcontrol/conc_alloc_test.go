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

package flowcontrol

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

// floating-point imprecision
const fpSlack = 1e-10

const MaxWeight = 1000

// TestConcAlloc tests computeConcurrencyAllocation with a bunch of randomly generated cases.
func TestConcAlloc(t *testing.T) {
	rands := rand.New(rand.NewSource(1234567890))
	for i := 0; i < 10000; i++ {
		test1ConcAlloc(t, rands)
	}
}

func test1ConcAlloc(t *testing.T, rands *rand.Rand) {
	probLen := ([]int{0, 1, 2, 3, 4, 6, 9})[rands.Intn(7)]
	classes := make([]allocProblemItem, probLen)
	var lowSum, highSum float64
	var requiredSum int
	var requiredSumF float64
	style := "empty"
	if probLen > 0 {
		switch rands.Intn(20) {
		case 0:
			style = "bound from below"
			requiredSum = rands.Intn(probLen * 3)
			requiredSumF = float64(requiredSum)
			partition64(rands, probLen, requiredSumF, func(j int, x float64) {
				classes[j].lowerBound = x
				classes[j].target = x + 2*rands.Float64()
				classes[j].upperBound = x + 3*rands.Float64()
				classes[j].weight = float64(1 + rands.Intn(MaxWeight))
				lowSum += classes[j].lowerBound
				highSum += classes[j].upperBound
			})
		case 1:
			style = "bound from above"
			requiredSum = rands.Intn(probLen*3) + 1
			requiredSumF = float64(requiredSum)
			partition64(rands, probLen, requiredSumF, func(j int, x float64) {
				classes[j].upperBound = x
				classes[j].lowerBound = x * math.Max(0, 1.25*rands.Float64()-1)
				classes[j].target = classes[j].lowerBound + rands.Float64()
				classes[j].weight = float64(1 + rands.Intn(MaxWeight))
				lowSum += classes[j].lowerBound
				highSum += classes[j].upperBound
			})
		default:
			style = "not-set-by-bounds"
			for j := 0; j < probLen; j++ {
				x := math.Max(0, rands.Float64()*5-1)
				classes[j].lowerBound = x
				classes[j].target = x + 2*rands.Float64()
				classes[j].upperBound = x + 3*rands.Float64()
				classes[j].weight = float64(1 + rands.Intn(MaxWeight))
				lowSum += classes[j].lowerBound
				highSum += classes[j].upperBound
			}
			requiredSumF = math.Round(float64(lowSum + (highSum-lowSum)*rands.Float64()))
			requiredSum = int(requiredSumF)
		}
	}
	for rands.Float64() < 0.25 {
		// Add a class with a target of zero
		classes = append(classes, allocProblemItem{target: 0, allocWeight: allocWeight{weight: float64(1 + rands.Intn(MaxWeight))}, upperBound: rands.Float64() + 0.00001})
		highSum += classes[probLen].upperBound
		if probLen > 1 {
			m := rands.Intn(probLen)
			classes[m], classes[probLen] = classes[probLen], classes[m]
		}
		probLen = len(classes)
	}
	allocs, delta, err := computeConcurrencyAllocation(requiredSum, classes)
	var actualSumF float64
	for _, item := range allocs {
		actualSumF += item
	}
	expectErr := lowSum-requiredSumF > fpSlack || requiredSumF-highSum > fpSlack
	if err != nil {
		if expectErr {
			t.Logf("Success for requiredSum=%v, %s classes=%#+v expected error and got %#+v", requiredSum, style, classes, err)
			return
		}
		t.Fatalf("Fail for requiredSum=%v, %s classes=%#+v got unexpected error %#+v", requiredSum, style, classes, err)
	}
	if expectErr {
		t.Fatalf("Fail for from requiredSum=%v, %s classes=%#+v: expected error but got solution %v, %v instead", requiredSum, style, classes, allocs, delta)
	}
	var failed bool
	rd := f64RelDiff(requiredSumF, actualSumF)
	if rd > fpSlack {
		failed = true
		t.Errorf("Fail for requiredSum=%v, %s classes=%#+v got solution %v, %v which has sum %v", requiredSum, style, classes, allocs, delta, actualSumF)
	}
	for idx, item := range classes {
		target := math.Max(item.target, MinTarget)
		alloc := item.proportionForDelta(delta) * target
		if alloc <= item.lowerBound {
			if allocs[idx] != item.lowerBound {
				failed = true
				t.Errorf("Fail for requiredSum=%v, %s classes=%#+v got solution %v, %v in which item %d should be its lower bound but is not", requiredSum, style, classes, allocs, delta, idx)
			}
		} else if alloc >= item.upperBound {
			if allocs[idx] != item.upperBound {
				failed = true
				t.Errorf("Fail for requiredSum=%v, %s classes=%#+v got solution %v, %v in which item %d should be its upper bound but is not", requiredSum, style, classes, allocs, delta, idx)
			}
		} else if f64RelDiff(alloc, allocs[idx]) > fpSlack {
			failed = true
			t.Errorf("Fail for requiredSum=%v, %s classes=%#+v got solution %v, %v in which item %d got alloc %v should be %v (which is proportional to its target) but is not", requiredSum, style, classes, allocs, delta, idx, allocs[idx], alloc)
		}
	}
	if failed {
		t.FailNow()
	}
	t.Logf("Success for requiredSum=%v, %s classes=%#+v got solution %v, %v", requiredSum, style, classes, allocs, delta)
}

// partition64 calls consume n times, passing ints [0,n) and floats that sum to x
func partition64(rands *rand.Rand, n int, x float64, consume func(int, float64)) {
	if n <= 0 {
		return
	}
	divs := make([]float64, n-1)
	for idx := range divs {
		divs[idx] = float64(rands.Float64())
	}
	sort.Float64s(divs)
	var last float64
	for idx, div := range divs {
		div32 := float64(div)
		delta := div32 - last
		consume(idx, delta*x)
		last = div32
	}
	consume(n-1, (1-last)*x)
}

func f64RelDiff(a, b float64) float64 {
	den := math.Max(math.Abs(a), math.Abs(b))
	if den == 0 {
		return 0
	}
	return math.Abs(a-b) / den
}
