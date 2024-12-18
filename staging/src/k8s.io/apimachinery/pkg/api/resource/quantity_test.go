/*
Copyright 2014 The Kubernetes Authors.

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

package resource

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/google/go-cmp/cmp"
	fuzz "github.com/google/gofuzz"
	"github.com/spf13/pflag"
	inf "gopkg.in/inf.v0"

	cbor "k8s.io/apimachinery/pkg/runtime/serializer/cbor/direct"
)

var (
	bigMostPositive = big.NewInt(mostPositive)
	bigMostNegative = big.NewInt(mostNegative)
)

func dec(i int64, exponent int) infDecAmount {
	// See the below test-- scale is the negative of an exponent.
	return infDecAmount{inf.NewDec(i, inf.Scale(-exponent))}
}

func bigDec(i *big.Int, exponent int) infDecAmount {
	// See the below test-- scale is the negative of an exponent.
	return infDecAmount{inf.NewDecBig(i, inf.Scale(-exponent))}
}

func decQuantity(i int64, exponent int, format Format) Quantity {
	return Quantity{d: dec(i, exponent), Format: format}
}

func bigDecQuantity(i *big.Int, exponent int, format Format) Quantity {
	return Quantity{d: bigDec(i, exponent), Format: format}
}

func intQuantity(i int64, exponent Scale, format Format) Quantity {
	return Quantity{i: int64Amount{value: i, scale: exponent}, Format: format}
}

func TestDec(t *testing.T) {
	table := []struct {
		got    infDecAmount
		expect string
	}{
		{dec(1, 0), "1"},
		{dec(1, 1), "10"},
		{dec(5, 2), "500"},
		{dec(8, 3), "8000"},
		{dec(2, 0), "2"},
		{dec(1, -1), "0.1"},
		{dec(3, -2), "0.03"},
		{dec(4, -3), "0.004"},
	}

	for _, item := range table {
		if e, a := item.expect, item.got.Dec.String(); e != a {
			t.Errorf("expected %v, got %v", e, a)
		}
	}
}

func TestBigDec(t *testing.T) {
	table := []struct {
		got    infDecAmount
		expect string
	}{
		{bigDec(big.NewInt(1), 0), "1"},
		{bigDec(big.NewInt(1), 1), "10"},
		{bigDec(big.NewInt(5), 2), "500"},
		{bigDec(big.NewInt(8), 3), "8000"},
		{bigDec(big.NewInt(2), 0), "2"},
		{bigDec(big.NewInt(1), -1), "0.1"},
		{bigDec(big.NewInt(3), -2), "0.03"},
		{bigDec(big.NewInt(4), -3), "0.004"},
		{bigDec(big.NewInt(0).Add(bigMostPositive, big.NewInt(1)), 0), "9223372036854775808"},
		{bigDec(big.NewInt(0).Add(bigMostPositive, big.NewInt(1)), 1), "92233720368547758080"},
		{bigDec(big.NewInt(0).Add(bigMostPositive, big.NewInt(1)), 2), "922337203685477580800"},
		{bigDec(big.NewInt(0).Add(bigMostPositive, big.NewInt(1)), -1), "922337203685477580.8"},
		{bigDec(big.NewInt(0).Add(bigMostPositive, big.NewInt(1)), -2), "92233720368547758.08"},
		{bigDec(big.NewInt(0).Sub(bigMostNegative, big.NewInt(1)), 0), "-9223372036854775809"},
		{bigDec(big.NewInt(0).Sub(bigMostNegative, big.NewInt(1)), 1), "-92233720368547758090"},
		{bigDec(big.NewInt(0).Sub(bigMostNegative, big.NewInt(1)), 2), "-922337203685477580900"},
		{bigDec(big.NewInt(0).Sub(bigMostNegative, big.NewInt(1)), -1), "-922337203685477580.9"},
		{bigDec(big.NewInt(0).Sub(bigMostNegative, big.NewInt(1)), -2), "-92233720368547758.09"},
	}

	for _, item := range table {
		if e, a := item.expect, item.got.Dec.String(); e != a {
			t.Errorf("expected %v, got %v", e, a)
		}
	}
}

// TestQuantityParseZero ensures that when a 0 quantity is passed, its string value is 0
func TestQuantityParseZero(t *testing.T) {
	zero := MustParse("0")
	if expected, actual := "0", zero.String(); expected != actual {
		t.Errorf("Expected %v, actual %v", expected, actual)
	}
}

// TestQuantityParseNonNumericPanic ensures that when a non-numeric string is parsed
// it panics
func TestQuantityParseNonNumericPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("MustParse did not panic")
		}
	}()
	_ = MustParse("Non-Numeric")
}

// TestQuantityAddZeroPreservesSuffix verifies that a suffix is preserved
// independent of the order of operations when adding a zero and non-zero val
func TestQuantityAddZeroPreservesSuffix(t *testing.T) {
	testValues := []string{"100m", "1Gi"}
	zero := MustParse("0")
	for _, testValue := range testValues {
		value := MustParse(testValue)
		v1 := value.DeepCopy()
		// ensure non-zero + zero = non-zero (suffix preserved)
		v1.Add(zero)
		// ensure zero + non-zero = non-zero (suffix preserved)
		v2 := zero.DeepCopy()
		v2.Add(value)

		if v1.String() != testValue {
			t.Errorf("Expected %v, actual %v", testValue, v1.String())
			continue
		}
		if v2.String() != testValue {
			t.Errorf("Expected %v, actual %v", testValue, v2.String())
		}
	}
}

// TestQuantitySubZeroPreservesSuffix verifies that a suffix is preserved
// independent of the order of operations when subtracting a zero and non-zero val
func TestQuantitySubZeroPreservesSuffix(t *testing.T) {
	testValues := []string{"100m", "1Gi"}
	zero := MustParse("0")
	for _, testValue := range testValues {
		value := MustParse(testValue)
		v1 := value.DeepCopy()
		// ensure non-zero - zero = non-zero (suffix preserved)
		v1.Sub(zero)
		// ensure we preserved the input value
		if v1.String() != testValue {
			t.Errorf("Expected %v, actual %v", testValue, v1.String())
		}

		// ensure zero - non-zero = -non-zero (suffix preserved)
		v2 := zero.DeepCopy()
		v2.Sub(value)
		negVal := value.DeepCopy()
		negVal.Neg()
		if v2.String() != negVal.String() {
			t.Errorf("Expected %v, actual %v", negVal.String(), v2.String())
		}
	}
}

// TestQuantityCanocicalizeZero verifies that you get 0 as canonical value if internal value is 0, and not 0<suffix>
func TestQuantityCanocicalizeZero(t *testing.T) {
	val := MustParse("1000m")
	val.i.Sub(int64Amount{value: 1})
	zero := Quantity{i: val.i, Format: DecimalSI}
	if expected, actual := "0", zero.String(); expected != actual {
		t.Errorf("Expected %v, actual %v", expected, actual)
	}
}

func TestQuantityCmp(t *testing.T) {
	// Test when d is nil
	table := []struct {
		x      string
		y      string
		expect int
	}{
		{"0", "0", 0},
		{"100m", "50m", 1},
		{"50m", "100m", -1},
		{"10000T", "100Gi", 1},
	}
	for _, testCase := range table {
		q1 := MustParse(testCase.x)
		q2 := MustParse(testCase.y)
		if result := q1.Cmp(q2); result != testCase.expect {
			t.Errorf("X: %v, Y: %v, Expected: %v, Actual: %v", testCase.x, testCase.y, testCase.expect, result)
		}
	}
	// Test when i is {0,0}
	table2 := []struct {
		x      *inf.Dec
		y      *inf.Dec
		expect int
	}{
		{dec(0, 0).Dec, dec(0, 0).Dec, 0},
		{nil, dec(0, 0).Dec, 0},
		{dec(0, 0).Dec, nil, 0},
		{nil, nil, 0},
		{nil, dec(10, 0).Dec, -1},
		{nil, dec(-10, 0).Dec, 1},
		{dec(10, 0).Dec, nil, 1},
		{dec(-10, 0).Dec, nil, -1},
	}
	for _, testCase := range table2 {
		q1 := Quantity{d: infDecAmount{testCase.x}, Format: DecimalSI}
		q2 := Quantity{d: infDecAmount{testCase.y}, Format: DecimalSI}
		if result := q1.Cmp(q2); result != testCase.expect {
			t.Errorf("X: %v, Y: %v, Expected: %v, Actual: %v", testCase.x, testCase.y, testCase.expect, result)
		}
	}
}

func TestParseQuantityString(t *testing.T) {
	table := []struct {
		input              string
		positive           bool
		value              string
		num, denom, suffix string
	}{
		{"0.025Ti", true, "0.025", "0", "025", "Ti"},
		{"1.025Ti", true, "1.025", "1", "025", "Ti"},
		{"-1.025Ti", false, "-1.025", "1", "025", "Ti"},
		{".", true, ".", "0", "", ""},
		{"-.", false, "-.", "0", "", ""},
		{"1E-3", true, "1", "1", "", "E-3"},
	}
	for _, test := range table {
		positive, value, num, denom, suffix, err := parseQuantityString(test.input)
		if err != nil {
			t.Errorf("%s: error: %v", test.input, err)
			continue
		}
		if positive != test.positive || value != test.value || num != test.num || denom != test.denom || suffix != test.suffix {
			t.Errorf("%s: unmatched: %t %q %q %q %q", test.input, positive, value, num, denom, suffix)
		}
	}
}

func TestQuantityParse(t *testing.T) {
	if _, err := ParseQuantity(""); err == nil {
		t.Errorf("expected empty string to return error")
	}

	table := []struct {
		input  string
		expect Quantity
	}{
		{"0", decQuantity(0, 0, DecimalSI)},
		{"0n", decQuantity(0, 0, DecimalSI)},
		{"0u", decQuantity(0, 0, DecimalSI)},
		{"0m", decQuantity(0, 0, DecimalSI)},
		{"0Ki", decQuantity(0, 0, BinarySI)},
		{"0k", decQuantity(0, 0, DecimalSI)},
		{"0Mi", decQuantity(0, 0, BinarySI)},
		{"0M", decQuantity(0, 0, DecimalSI)},
		{"0Gi", decQuantity(0, 0, BinarySI)},
		{"0G", decQuantity(0, 0, DecimalSI)},
		{"0Ti", decQuantity(0, 0, BinarySI)},
		{"0T", decQuantity(0, 0, DecimalSI)},

		// Quantity less numbers are allowed
		{"1", decQuantity(1, 0, DecimalSI)},

		// Binary suffixes
		{"1Ki", decQuantity(1024, 0, BinarySI)},
		{"8Ki", decQuantity(8*1024, 0, BinarySI)},
		{"7Mi", decQuantity(7*1024*1024, 0, BinarySI)},
		{"6Gi", decQuantity(6*1024*1024*1024, 0, BinarySI)},
		{"5Ti", decQuantity(5*1024*1024*1024*1024, 0, BinarySI)},
		{"4Pi", decQuantity(4*1024*1024*1024*1024*1024, 0, BinarySI)},
		{"3Ei", decQuantity(3*1024*1024*1024*1024*1024*1024, 0, BinarySI)},

		{"10Ti", decQuantity(10*1024*1024*1024*1024, 0, BinarySI)},
		{"100Ti", decQuantity(100*1024*1024*1024*1024, 0, BinarySI)},

		// Decimal suffixes
		{"5n", decQuantity(5, -9, DecimalSI)},
		{"4u", decQuantity(4, -6, DecimalSI)},
		{"3m", decQuantity(3, -3, DecimalSI)},
		{"9", decQuantity(9, 0, DecimalSI)},
		{"8k", decQuantity(8, 3, DecimalSI)},
		{"50k", decQuantity(5, 4, DecimalSI)},
		{"7M", decQuantity(7, 6, DecimalSI)},
		{"6G", decQuantity(6, 9, DecimalSI)},
		{"5T", decQuantity(5, 12, DecimalSI)},
		{"40T", decQuantity(4, 13, DecimalSI)},
		{"300T", decQuantity(3, 14, DecimalSI)},
		{"2P", decQuantity(2, 15, DecimalSI)},
		{"1E", decQuantity(1, 18, DecimalSI)},

		// Decimal exponents
		{"1E-3", decQuantity(1, -3, DecimalExponent)},
		{"1e3", decQuantity(1, 3, DecimalExponent)},
		{"1E6", decQuantity(1, 6, DecimalExponent)},
		{"1e9", decQuantity(1, 9, DecimalExponent)},
		{"1E12", decQuantity(1, 12, DecimalExponent)},
		{"1e15", decQuantity(1, 15, DecimalExponent)},
		{"1E18", decQuantity(1, 18, DecimalExponent)},

		// Nonstandard but still parsable
		{"1e14", decQuantity(1, 14, DecimalExponent)},
		{"1e13", decQuantity(1, 13, DecimalExponent)},
		{"1e3", decQuantity(1, 3, DecimalExponent)},
		{"100.035k", decQuantity(100035, 0, DecimalSI)},

		// Things that look like floating point
		{"0.001", decQuantity(1, -3, DecimalSI)},
		{"0.0005k", decQuantity(5, -1, DecimalSI)},
		{"0.005", decQuantity(5, -3, DecimalSI)},
		{"0.05", decQuantity(5, -2, DecimalSI)},
		{"0.5", decQuantity(5, -1, DecimalSI)},
		{"0.00050k", decQuantity(5, -1, DecimalSI)},
		{"0.00500", decQuantity(5, -3, DecimalSI)},
		{"0.05000", decQuantity(5, -2, DecimalSI)},
		{"0.50000", decQuantity(5, -1, DecimalSI)},
		{"0.5e0", decQuantity(5, -1, DecimalExponent)},
		{"0.5e-1", decQuantity(5, -2, DecimalExponent)},
		{"0.5e-2", decQuantity(5, -3, DecimalExponent)},
		{"0.5e0", decQuantity(5, -1, DecimalExponent)},
		{"10.035M", decQuantity(10035, 3, DecimalSI)},

		{"1.2e3", decQuantity(12, 2, DecimalExponent)},
		{"1.3E+6", decQuantity(13, 5, DecimalExponent)},
		{"1.40e9", decQuantity(14, 8, DecimalExponent)},
		{"1.53E12", decQuantity(153, 10, DecimalExponent)},
		{"1.6e15", decQuantity(16, 14, DecimalExponent)},
		{"1.7E18", decQuantity(17, 17, DecimalExponent)},

		{"9.01", decQuantity(901, -2, DecimalSI)},
		{"8.1k", decQuantity(81, 2, DecimalSI)},
		{"7.123456M", decQuantity(7123456, 0, DecimalSI)},
		{"6.987654321G", decQuantity(6987654321, 0, DecimalSI)},
		{"5.444T", decQuantity(5444, 9, DecimalSI)},
		{"40.1T", decQuantity(401, 11, DecimalSI)},
		{"300.2T", decQuantity(3002, 11, DecimalSI)},
		{"2.5P", decQuantity(25, 14, DecimalSI)},
		{"1.01E", decQuantity(101, 16, DecimalSI)},

		// Things that saturate/round
		{"3.001n", decQuantity(4, -9, DecimalSI)},
		{"1.1E-9", decQuantity(2, -9, DecimalExponent)},
		{"0.0000000001", decQuantity(1, -9, DecimalSI)},
		{"0.0000000005", decQuantity(1, -9, DecimalSI)},
		{"0.00000000050", decQuantity(1, -9, DecimalSI)},
		{"0.5e-9", decQuantity(1, -9, DecimalExponent)},
		{"0.9n", decQuantity(1, -9, DecimalSI)},
		{"0.00000012345", decQuantity(124, -9, DecimalSI)},
		{"0.00000012354", decQuantity(124, -9, DecimalSI)},
		{"9Ei", Quantity{d: maxAllowed, Format: BinarySI}},
		{"9223372036854775807Ki", Quantity{d: maxAllowed, Format: BinarySI}},
		{"12E", decQuantity(12, 18, DecimalSI)},

		// We'll accept fractional binary stuff, too.
		{"100.035Ki", decQuantity(10243584, -2, BinarySI)},
		{"0.5Mi", decQuantity(.5*1024*1024, 0, BinarySI)},
		{"0.05Gi", decQuantity(536870912, -1, BinarySI)},
		{"0.025Ti", decQuantity(274877906944, -1, BinarySI)},

		// Things written by trolls
		{"0.000000000001Ki", decQuantity(2, -9, DecimalSI)}, // rounds up, changes format
		{".001", decQuantity(1, -3, DecimalSI)},
		{".0001k", decQuantity(100, -3, DecimalSI)},
		{"1.", decQuantity(1, 0, DecimalSI)},
		{"1.G", decQuantity(1, 9, DecimalSI)},
	}

	for _, asDec := range []bool{false, true} {
		for _, item := range table {
			got, err := ParseQuantity(item.input)
			if err != nil {
				t.Errorf("%v: unexpected error: %v", item.input, err)
				continue
			}
			if asDec {
				got.AsDec()
			}

			if e, a := item.expect, got; e.Cmp(a) != 0 {
				t.Errorf("%v: expected %v, got %v", item.input, e.String(), a.String())
			}
			if e, a := item.expect.Format, got.Format; e != a {
				t.Errorf("%v: expected %#v, got %#v", item.input, e, a)
			}

			if asDec {
				if i, ok := got.AsInt64(); i != 0 || ok {
					t.Errorf("%v: expected inf.Dec to return false for AsInt64: %d", item.input, i)
				}
				continue
			}
			i, ok := item.expect.AsInt64()
			if !ok {
				continue
			}
			j, ok := got.AsInt64()
			if !ok {
				if got.d.Dec == nil && got.i.scale >= 0 {
					t.Errorf("%v: is an int64Amount, but can't return AsInt64: %v", item.input, got)
				}
				continue
			}
			if i != j {
				t.Errorf("%v: expected equivalent representation as int64: %d %d", item.input, i, j)
			}
		}

		for _, item := range table {
			got, err := ParseQuantity(item.input)
			if err != nil {
				t.Errorf("%v: unexpected error: %v", item.input, err)
				continue
			}

			if asDec {
				got.AsDec()
			}

			for _, format := range []Format{DecimalSI, BinarySI, DecimalExponent} {
				// ensure we are not simply checking pointer equality by creating a new inf.Dec
				var copied inf.Dec
				copied.Add(inf.NewDec(0, inf.Scale(0)), got.AsDec())
				q := NewDecimalQuantity(copied, format)
				if c := q.Cmp(got); c != 0 {
					t.Errorf("%v: round trip from decimal back to quantity is not comparable: %d: %#v vs %#v", item.input, c, got, q)
				}
			}

			// verify that we can decompose the input and get the same result by building up from the base.
			positive, _, num, denom, suffix, err := parseQuantityString(item.input)
			if err != nil {
				t.Errorf("%v: unexpected error: %v", item.input, err)
				continue
			}
			if got.Sign() >= 0 && !positive || got.Sign() < 0 && positive {
				t.Errorf("%v: positive was incorrect: %t", item.input, positive)
				continue
			}
			var value string
			if !positive {
				value = "-"
			}
			value += num
			if len(denom) > 0 {
				value += "." + denom
			}
			value += suffix
			if len(value) == 0 {
				t.Errorf("%v: did not parse correctly, %q %q %q", item.input, num, denom, suffix)
			}
			expected, err := ParseQuantity(value)
			if err != nil {
				t.Errorf("%v: unexpected error for %s: %v", item.input, value, err)
				continue
			}
			if expected.Cmp(got) != 0 {
				t.Errorf("%v: not the same as %s", item.input, value)
				continue
			}
		}

		// Try the negative version of everything
		desired := &inf.Dec{}
		expect := Quantity{d: infDecAmount{Dec: desired}}
		for _, item := range table {
			got, err := ParseQuantity("-" + strings.TrimLeftFunc(item.input, unicode.IsSpace))
			if err != nil {
				t.Errorf("-%v: unexpected error: %v", item.input, err)
				continue
			}
			if asDec {
				got.AsDec()
			}

			expected := item.expect
			desired.Neg(expected.AsDec())

			if e, a := expect, got; e.Cmp(a) != 0 {
				t.Errorf("%v: expected %s, got %s", item.input, e.String(), a.String())
			}
			if e, a := expected.Format, got.Format; e != a {
				t.Errorf("%v: expected %#v, got %#v", item.input, e, a)
			}
		}

		// Try everything with an explicit +
		for _, item := range table {
			got, err := ParseQuantity("+" + strings.TrimLeftFunc(item.input, unicode.IsSpace))
			if err != nil {
				t.Errorf("-%v: unexpected error: %v", item.input, err)
				continue
			}
			if asDec {
				got.AsDec()
			}

			if e, a := item.expect, got; e.Cmp(a) != 0 {
				t.Errorf("%v(%t): expected %s, got %s", item.input, asDec, e.String(), a.String())
			}
			if e, a := item.expect.Format, got.Format; e != a {
				t.Errorf("%v: expected %#v, got %#v", item.input, e, a)
			}
		}
	}

	invalid := []string{
		"1.1.M",
		"1+1.0M",
		"0.1mi",
		"0.1am",
		"aoeu",
		".5i",
		"1i",
		"-3.01i",
		"-3.01e-",

		// trailing whitespace is forbidden
		" 1",
		"1 ",
	}
	for _, item := range invalid {
		_, err := ParseQuantity(item)
		if err == nil {
			t.Errorf("%v parsed unexpectedly", item)
		}
	}
}

func TestQuantityRoundUp(t *testing.T) {
	table := []struct {
		in     string
		scale  Scale
		expect Quantity
		ok     bool
	}{
		{"9.01", -3, decQuantity(901, -2, DecimalSI), true},
		{"9.01", -2, decQuantity(901, -2, DecimalSI), true},
		{"9.01", -1, decQuantity(91, -1, DecimalSI), false},
		{"9.01", 0, decQuantity(10, 0, DecimalSI), false},
		{"9.01", 1, decQuantity(10, 0, DecimalSI), false},
		{"9.01", 2, decQuantity(100, 0, DecimalSI), false},

		{"-9.01", -3, decQuantity(-901, -2, DecimalSI), true},
		{"-9.01", -2, decQuantity(-901, -2, DecimalSI), true},
		{"-9.01", -1, decQuantity(-91, -1, DecimalSI), false},
		{"-9.01", 0, decQuantity(-10, 0, DecimalSI), false},
		{"-9.01", 1, decQuantity(-10, 0, DecimalSI), false},
		{"-9.01", 2, decQuantity(-100, 0, DecimalSI), false},
	}

	for _, asDec := range []bool{false, true} {
		for _, item := range table {
			got, err := ParseQuantity(item.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			expect := item.expect.DeepCopy()
			if asDec {
				got.AsDec()
			}
			if ok := got.RoundUp(item.scale); ok != item.ok {
				t.Errorf("%s(%d,%t): unexpected ok: %t", item.in, item.scale, asDec, ok)
			}
			if got.Cmp(expect) != 0 {
				t.Errorf("%s(%d,%t): unexpected round: %s vs %s", item.in, item.scale, asDec, got.String(), expect.String())
			}
		}
	}
}

func TestQuantityCmpInt64AndDec(t *testing.T) {
	table := []struct {
		a, b Quantity
		cmp  int
	}{
		{intQuantity(901, -2, DecimalSI), intQuantity(901, -2, DecimalSI), 0},
		{intQuantity(90, -1, DecimalSI), intQuantity(901, -2, DecimalSI), -1},
		{intQuantity(901, -2, DecimalSI), intQuantity(900, -2, DecimalSI), 1},
		{intQuantity(0, 0, DecimalSI), intQuantity(0, 0, DecimalSI), 0},
		{intQuantity(0, 1, DecimalSI), intQuantity(0, -1, DecimalSI), 0},
		{intQuantity(0, -1, DecimalSI), intQuantity(0, 1, DecimalSI), 0},
		{intQuantity(800, -3, DecimalSI), intQuantity(1, 0, DecimalSI), -1},
		{intQuantity(800, -3, DecimalSI), intQuantity(79, -2, DecimalSI), 1},

		{intQuantity(mostPositive, 0, DecimalSI), intQuantity(1, -1, DecimalSI), 1},
		{intQuantity(mostPositive, 1, DecimalSI), intQuantity(1, 0, DecimalSI), 1},
		{intQuantity(mostPositive, 1, DecimalSI), intQuantity(1, 1, DecimalSI), 1},
		{intQuantity(mostPositive, 1, DecimalSI), intQuantity(0, 1, DecimalSI), 1},
		{intQuantity(mostPositive, -16, DecimalSI), intQuantity(1, 3, DecimalSI), -1},

		{intQuantity(mostNegative, 0, DecimalSI), intQuantity(0, 0, DecimalSI), -1},
		{intQuantity(mostNegative, -18, DecimalSI), intQuantity(-1, 0, DecimalSI), -1},
		{intQuantity(mostNegative, -19, DecimalSI), intQuantity(-1, 0, DecimalSI), 1},

		{intQuantity(1*1000000*1000000*1000000, -17, DecimalSI), intQuantity(1, 1, DecimalSI), 0},
		{intQuantity(1*1000000*1000000*1000000, -17, DecimalSI), intQuantity(-10, 0, DecimalSI), 1},
		{intQuantity(-1*1000000*1000000*1000000, -17, DecimalSI), intQuantity(-10, 0, DecimalSI), 0},
		{intQuantity(1*1000000*1000000*1000000, -17, DecimalSI), intQuantity(1, 0, DecimalSI), 1},

		{intQuantity(1*1000000*1000000*1000000+1, -17, DecimalSI), intQuantity(1, 1, DecimalSI), 1},
		{intQuantity(1*1000000*1000000*1000000-1, -17, DecimalSI), intQuantity(1, 1, DecimalSI), -1},
	}

	for _, item := range table {
		if cmp := item.a.Cmp(item.b); cmp != item.cmp {
			t.Errorf("%#v: unexpected Cmp: %d", item, cmp)
		}
		if cmp := item.b.Cmp(item.a); cmp != -item.cmp {
			t.Errorf("%#v: unexpected inverted Cmp: %d", item, cmp)
		}
	}

	for _, item := range table {
		a, b := item.a.DeepCopy(), item.b.DeepCopy()
		a.AsDec()
		if cmp := a.Cmp(b); cmp != item.cmp {
			t.Errorf("%#v: unexpected Cmp: %d", item, cmp)
		}
		if cmp := b.Cmp(a); cmp != -item.cmp {
			t.Errorf("%#v: unexpected inverted Cmp: %d", item, cmp)
		}
	}

	for _, item := range table {
		a, b := item.a.DeepCopy(), item.b.DeepCopy()
		b.AsDec()
		if cmp := a.Cmp(b); cmp != item.cmp {
			t.Errorf("%#v: unexpected Cmp: %d", item, cmp)
		}
		if cmp := b.Cmp(a); cmp != -item.cmp {
			t.Errorf("%#v: unexpected inverted Cmp: %d", item, cmp)
		}
	}

	for _, item := range table {
		a, b := item.a.DeepCopy(), item.b.DeepCopy()
		a.AsDec()
		b.AsDec()
		if cmp := a.Cmp(b); cmp != item.cmp {
			t.Errorf("%#v: unexpected Cmp: %d", item, cmp)
		}
		if cmp := b.Cmp(a); cmp != -item.cmp {
			t.Errorf("%#v: unexpected inverted Cmp: %d", item, cmp)
		}
	}
}

func TestQuantityNeg(t *testing.T) {
	table := []struct {
		a   Quantity
		out string
	}{
		{intQuantity(901, -2, DecimalSI), "-9010m"},
		{decQuantity(901, -2, DecimalSI), "-9010m"},
	}

	for i, item := range table {
		out := item.a.DeepCopy()
		out.Neg()
		if out.Cmp(item.a) == 0 {
			t.Errorf("%d: negating an item should not mutate the source: %s", i, out.String())
		}
		if out.String() != item.out {
			t.Errorf("%d: negating did not equal exact value: %s", i, out.String())
		}
	}
}

func TestQuantityString(t *testing.T) {
	table := []struct {
		in        Quantity
		expect    string
		alternate string
	}{
		{decQuantity(1024*1024*1024, 0, BinarySI), "1Gi", "1024Mi"},
		{decQuantity(300*1024*1024, 0, BinarySI), "300Mi", "307200Ki"},
		{decQuantity(6*1024, 0, BinarySI), "6Ki", ""},
		{decQuantity(1001*1024*1024*1024, 0, BinarySI), "1001Gi", "1025024Mi"},
		{decQuantity(1024*1024*1024*1024, 0, BinarySI), "1Ti", "1024Gi"},
		{decQuantity(5, 0, BinarySI), "5", "5000m"},
		{decQuantity(500, -3, BinarySI), "500m", "0.5"},
		{decQuantity(1, 9, DecimalSI), "1G", "1000M"},
		{decQuantity(1000, 6, DecimalSI), "1G", "0.001T"},
		{decQuantity(1000000, 3, DecimalSI), "1G", ""},
		{decQuantity(1000000000, 0, DecimalSI), "1G", ""},
		{decQuantity(1, -3, DecimalSI), "1m", "1000u"},
		{decQuantity(80, -3, DecimalSI), "80m", ""},
		{decQuantity(1080, -3, DecimalSI), "1080m", "1.08"},
		{decQuantity(108, -2, DecimalSI), "1080m", "1080000000n"},
		{decQuantity(10800, -4, DecimalSI), "1080m", ""},
		{decQuantity(300, 6, DecimalSI), "300M", ""},
		{decQuantity(1, 12, DecimalSI), "1T", ""},
		{decQuantity(1234567, 6, DecimalSI), "1234567M", ""},
		{decQuantity(1234567, -3, BinarySI), "1234567m", ""},
		{decQuantity(3, 3, DecimalSI), "3k", ""},
		{decQuantity(1025, 0, BinarySI), "1025", ""},
		{decQuantity(0, 0, DecimalSI), "0", ""},
		{decQuantity(0, 0, BinarySI), "0", ""},
		{decQuantity(1, 9, DecimalExponent), "1e9", ".001e12"},
		{decQuantity(1, -3, DecimalExponent), "1e-3", "0.001e0"},
		{decQuantity(1, -9, DecimalExponent), "1e-9", "1000e-12"},
		{decQuantity(80, -3, DecimalExponent), "80e-3", ""},
		{decQuantity(300, 6, DecimalExponent), "300e6", ""},
		{decQuantity(1, 12, DecimalExponent), "1e12", ""},
		{decQuantity(1, 3, DecimalExponent), "1e3", ""},
		{decQuantity(3, 3, DecimalExponent), "3e3", ""},
		{decQuantity(3, 3, DecimalSI), "3k", ""},
		{decQuantity(0, 0, DecimalExponent), "0", "00"},
		{decQuantity(1, -9, DecimalSI), "1n", ""},
		{decQuantity(80, -9, DecimalSI), "80n", ""},
		{decQuantity(1080, -9, DecimalSI), "1080n", ""},
		{decQuantity(108, -8, DecimalSI), "1080n", ""},
		{decQuantity(10800, -10, DecimalSI), "1080n", ""},
		{decQuantity(1, -6, DecimalSI), "1u", ""},
		{decQuantity(80, -6, DecimalSI), "80u", ""},
		{decQuantity(1080, -6, DecimalSI), "1080u", ""},
	}
	for _, item := range table {
		got := item.in.String()
		if e, a := item.expect, got; e != a {
			t.Errorf("%#v: expected %v, got %v", item.in, e, a)
		}
		q, err := ParseQuantity(item.expect)
		if err != nil {
			t.Errorf("%#v: unexpected error: %v", item.expect, err)
		}
		if len(q.s) == 0 || q.s != item.expect {
			t.Errorf("%#v: did not copy canonical string on parse: %s", item.expect, q.s)
		}
		if len(item.alternate) == 0 {
			continue
		}
		q, err = ParseQuantity(item.alternate)
		if err != nil {
			t.Errorf("%#v: unexpected error: %v", item.expect, err)
			continue
		}
		if len(q.s) != 0 {
			t.Errorf("%#v: unexpected nested string: %v", item.expect, q.s)
		}
		if q.String() != item.expect {
			t.Errorf("%#v: unexpected alternate canonical: %v", item.expect, q.String())
		}
		if len(q.s) == 0 || q.s != item.expect {
			t.Errorf("%#v: did not set canonical string on ToString: %s", item.expect, q.s)
		}
	}
	desired := &inf.Dec{} // Avoid modifying the values in the table.
	for _, item := range table {
		if item.in.Cmp(Quantity{}) == 0 {
			// Don't expect it to print "-0" ever
			continue
		}
		q := item.in
		q.d = infDecAmount{desired.Neg(q.AsDec())}
		if e, a := "-"+item.expect, q.String(); e != a {
			t.Errorf("%#v: expected %v, got %v", item.in, e, a)
		}
	}
}

func TestQuantityParseEmit(t *testing.T) {
	table := []struct {
		in     string
		expect string
	}{
		{"1Ki", "1Ki"},
		{"1Mi", "1Mi"},
		{"1Gi", "1Gi"},
		{"1024Mi", "1Gi"},
		{"1000M", "1G"},
		{".001Ki", "1024m"},
		{".000001Ki", "1024u"},
		{".000000001Ki", "1024n"},
		{".000000000001Ki", "2n"},
	}

	for _, item := range table {
		q, err := ParseQuantity(item.in)
		if err != nil {
			t.Errorf("Couldn't parse %v", item.in)
			continue
		}
		if e, a := item.expect, q.String(); e != a {
			t.Errorf("%#v: expected %v, got %v", item.in, e, a)
		}
	}
	for _, item := range table {
		q, err := ParseQuantity("-" + item.in)
		if err != nil {
			t.Errorf("Couldn't parse %v", item.in)
			continue
		}
		if q.Cmp(Quantity{}) == 0 {
			continue
		}
		if e, a := "-"+item.expect, q.String(); e != a {
			t.Errorf("%#v: expected %v, got %v (%#v)", item.in, e, a, q.i)
		}
	}
}

var fuzzer = fuzz.New().Funcs(
	func(q *Quantity, c fuzz.Continue) {
		q.i = Zero
		if c.RandBool() {
			q.Format = BinarySI
			if c.RandBool() {
				dec := &inf.Dec{}
				q.d = infDecAmount{Dec: dec}
				dec.SetScale(0)
				dec.SetUnscaled(c.Int63())
				return
			}
			// Be sure to test cases like 1Mi
			dec := &inf.Dec{}
			q.d = infDecAmount{Dec: dec}
			dec.SetScale(0)
			dec.SetUnscaled(c.Int63n(1024) << uint(10*c.Intn(5)))
			return
		}
		if c.RandBool() {
			q.Format = DecimalSI
		} else {
			q.Format = DecimalExponent
		}
		if c.RandBool() {
			dec := &inf.Dec{}
			q.d = infDecAmount{Dec: dec}
			dec.SetScale(inf.Scale(c.Intn(4)))
			dec.SetUnscaled(c.Int63())
			return
		}
		// Be sure to test cases like 1M
		dec := &inf.Dec{}
		q.d = infDecAmount{Dec: dec}
		dec.SetScale(inf.Scale(3 - c.Intn(15)))
		dec.SetUnscaled(c.Int63n(1000))
	},
)

func TestQuantityDeepCopy(t *testing.T) {
	// Test when d is nil
	slice := []string{"0", "100m", "50m", "10000T"}
	for _, testCase := range slice {
		q := MustParse(testCase)
		if result := q.DeepCopy(); result != q {
			t.Errorf("Expected: %v, Actual: %v", q, result)
		}
	}
	table := []*inf.Dec{
		dec(0, 0).Dec,
		dec(10, 0).Dec,
		dec(-10, 0).Dec,
	}
	// Test when i is {0,0}
	for _, testCase := range table {
		q := Quantity{d: infDecAmount{testCase}, Format: DecimalSI}
		result := q.DeepCopy()
		if q.d.Cmp(result.AsDec()) != 0 {
			t.Errorf("Expected: %v, Actual: %v", q.String(), result.String())
		}
		result = Quantity{d: infDecAmount{dec(2, 0).Dec}, Format: DecimalSI}
		if q.d.Cmp(result.AsDec()) == 0 {
			t.Errorf("Modifying result has affected q")
		}
	}
}

func TestJSON(t *testing.T) {
	for i := 0; i < 500; i++ {
		q := &Quantity{}
		fuzzer.Fuzz(q)
		b, err := json.Marshal(q)
		if err != nil {
			t.Errorf("error encoding %v: %v", q, err)
			continue
		}
		q2 := &Quantity{}
		err = json.Unmarshal(b, q2)
		if err != nil {
			t.Logf("%d: %s", i, string(b))
			t.Errorf("%v: error decoding %v: %v", q, string(b), err)
		}
		if q2.Cmp(*q) != 0 {
			t.Errorf("Expected equal: %v, %v (json was '%v')", q, q2, string(b))
		}
	}
}

func TestJSONWhitespace(t *testing.T) {
	q := Quantity{}
	testCases := []struct {
		in     string
		expect string
	}{
		{`" 1"`, "1"},
		{`"1 "`, "1"},
		{`1`, "1"},
		{` 1`, "1"},
		{`1 `, "1"},
		{`10`, "10"},
		{`-1`, "-1"},
		{` -1`, "-1"},
	}
	for _, test := range testCases {
		if err := json.Unmarshal([]byte(test.in), &q); err != nil {
			t.Errorf("%q: %v", test.in, err)
		}
		if q.String() != test.expect {
			t.Errorf("unexpected string: %q", q.String())
		}
	}
}

func TestMilliNewSet(t *testing.T) {
	table := []struct {
		value  int64
		format Format
		expect string
		exact  bool
	}{
		{1, DecimalSI, "1m", true},
		{1000, DecimalSI, "1", true},
		{1234000, DecimalSI, "1234", true},
		{1024, BinarySI, "1024m", false}, // Format changes
		{1000000, "invalidFormatDefaultsToExponent", "1e3", true},
		{1024 * 1024, BinarySI, "1048576m", false}, // Format changes
	}

	for _, item := range table {
		q := NewMilliQuantity(item.value, item.format)
		if e, a := item.expect, q.String(); e != a {
			t.Errorf("Expected %v, got %v; %#v", e, a, q)
		}
		if !item.exact {
			continue
		}
		q2, err := ParseQuantity(q.String())
		if err != nil {
			t.Errorf("Round trip failed on %v", q)
		}
		if e, a := item.value, q2.MilliValue(); e != a {
			t.Errorf("Expected %v, got %v", e, a)
		}
	}

	for _, item := range table {
		q := NewQuantity(0, item.format)
		q.SetMilli(item.value)
		if e, a := item.expect, q.String(); e != a {
			t.Errorf("Set: Expected %v, got %v; %#v", e, a, q)
		}
	}
}

func TestNewSet(t *testing.T) {
	table := []struct {
		value  int64
		format Format
		expect string
	}{
		{1, DecimalSI, "1"},
		{1000, DecimalSI, "1k"},
		{1234000, DecimalSI, "1234k"},
		{1024, BinarySI, "1Ki"},
		{1000000, "invalidFormatDefaultsToExponent", "1e6"},
		{1024 * 1024, BinarySI, "1Mi"},
	}

	for _, asDec := range []bool{false, true} {
		for _, item := range table {
			q := NewQuantity(item.value, item.format)
			if asDec {
				q.ToDec()
			}
			if e, a := item.expect, q.String(); e != a {
				t.Errorf("Expected %v, got %v; %#v", e, a, q)
			}
			q2, err := ParseQuantity(q.String())
			if err != nil {
				t.Errorf("Round trip failed on %v", q)
			}
			if e, a := item.value, q2.Value(); e != a {
				t.Errorf("Expected %v, got %v", e, a)
			}
		}

		for _, item := range table {
			q := NewQuantity(0, item.format)
			q.Set(item.value)
			if asDec {
				q.ToDec()
			}
			if e, a := item.expect, q.String(); e != a {
				t.Errorf("Set: Expected %v, got %v; %#v", e, a, q)
			}
		}
	}
}

func TestNewScaledSet(t *testing.T) {
	table := []struct {
		value  int64
		scale  Scale
		expect string
	}{
		{1, Nano, "1n"},
		{1000, Nano, "1u"},
		{1, Micro, "1u"},
		{1000, Micro, "1m"},
		{1, Milli, "1m"},
		{1000, Milli, "1"},
		{1, 0, "1"},
		{0, Nano, "0"},
		{0, Micro, "0"},
		{0, Milli, "0"},
		{0, 0, "0"},
	}

	for _, item := range table {
		q := NewScaledQuantity(item.value, item.scale)
		if e, a := item.expect, q.String(); e != a {
			t.Errorf("Expected %v, got %v; %#v", e, a, q)
		}
		q2, err := ParseQuantity(q.String())
		if err != nil {
			t.Errorf("Round trip failed on %v", q)
		}
		if e, a := item.value, q2.ScaledValue(item.scale); e != a {
			t.Errorf("Expected %v, got %v", e, a)
		}
		q3 := NewQuantity(0, DecimalSI)
		q3.SetScaled(item.value, item.scale)
		if q.Cmp(*q3) != 0 {
			t.Errorf("Expected %v and %v to be equal", q, q3)
		}
	}
}

func TestScaledValue(t *testing.T) {
	table := []struct {
		fromScale Scale
		toScale   Scale
		expected  int64
	}{
		{Nano, Nano, 1},
		{Nano, Micro, 1},
		{Nano, Milli, 1},
		{Nano, 0, 1},
		{Micro, Nano, 1000},
		{Micro, Micro, 1},
		{Micro, Milli, 1},
		{Micro, 0, 1},
		{Milli, Nano, 1000 * 1000},
		{Milli, Micro, 1000},
		{Milli, Milli, 1},
		{Milli, 0, 1},
		{0, Nano, 1000 * 1000 * 1000},
		{0, Micro, 1000 * 1000},
		{0, Milli, 1000},
		{0, 0, 1},
		{2, -2, 100 * 100},
	}

	for _, item := range table {
		q := NewScaledQuantity(1, item.fromScale)
		if e, a := item.expected, q.ScaledValue(item.toScale); e != a {
			t.Errorf("%v to %v: Expected %v, got %v", item.fromScale, item.toScale, e, a)
		}
	}
}

func TestUninitializedNoCrash(t *testing.T) {
	var q Quantity

	q.Value()
	q.MilliValue()
	q.DeepCopy()
	_ = q.String()
	q.MarshalJSON()
}

func TestDeepCopy(t *testing.T) {
	q := NewQuantity(5, DecimalSI)
	c := q.DeepCopy()
	c.Set(6)
	if q.Value() == 6 {
		t.Errorf("Copy didn't")
	}
}

func TestSub(t *testing.T) {
	tests := []struct {
		a        Quantity
		b        Quantity
		expected Quantity
	}{
		{decQuantity(10, 0, DecimalSI), decQuantity(1, 1, DecimalSI), decQuantity(0, 0, DecimalSI)},
		{decQuantity(10, 0, DecimalSI), decQuantity(1, 0, BinarySI), decQuantity(9, 0, DecimalSI)},
		{decQuantity(10, 0, BinarySI), decQuantity(1, 0, DecimalSI), decQuantity(9, 0, BinarySI)},
		{Quantity{Format: DecimalSI}, decQuantity(50, 0, DecimalSI), decQuantity(-50, 0, DecimalSI)},
		{decQuantity(50, 0, DecimalSI), Quantity{Format: DecimalSI}, decQuantity(50, 0, DecimalSI)},
		{Quantity{Format: DecimalSI}, Quantity{Format: DecimalSI}, decQuantity(0, 0, DecimalSI)},
	}

	for i, test := range tests {
		test.a.Sub(test.b)
		if test.a.Cmp(test.expected) != 0 {
			t.Errorf("[%d] Expected %q, got %q", i, test.expected.String(), test.a.String())
		}
	}
}

func TestNeg(t *testing.T) {
	tests := []struct {
		a        Quantity
		b        Quantity
		expected Quantity
	}{
		{a: intQuantity(0, 0, DecimalSI), expected: intQuantity(0, 0, DecimalSI)},
		{a: Quantity{}, expected: Quantity{}},
		{a: intQuantity(10, 0, BinarySI), expected: intQuantity(-10, 0, BinarySI)},
		{a: intQuantity(-10, 0, BinarySI), expected: intQuantity(10, 0, BinarySI)},
		{a: decQuantity(0, 0, DecimalSI), expected: intQuantity(0, 0, DecimalSI)},
		{a: decQuantity(10, 0, BinarySI), expected: intQuantity(-10, 0, BinarySI)},
		{a: decQuantity(-10, 0, BinarySI), expected: intQuantity(10, 0, BinarySI)},
	}

	for i, test := range tests {
		a := test.a.DeepCopy()
		a.Neg()
		// ensure value is same
		if a.Cmp(test.expected) != 0 {
			t.Errorf("[%d] Expected %q, got %q", i, test.expected.String(), a.String())
		}
	}
}

func TestAdd(t *testing.T) {
	tests := []struct {
		a        Quantity
		b        Quantity
		expected Quantity
	}{
		{decQuantity(10, 0, DecimalSI), decQuantity(1, 1, DecimalSI), decQuantity(20, 0, DecimalSI)},
		{decQuantity(10, 0, DecimalSI), decQuantity(1, 0, BinarySI), decQuantity(11, 0, DecimalSI)},
		{decQuantity(10, 0, BinarySI), decQuantity(1, 0, DecimalSI), decQuantity(11, 0, BinarySI)},
		{Quantity{Format: DecimalSI}, decQuantity(50, 0, DecimalSI), decQuantity(50, 0, DecimalSI)},
		{decQuantity(50, 0, DecimalSI), Quantity{Format: DecimalSI}, decQuantity(50, 0, DecimalSI)},
		{Quantity{Format: DecimalSI}, Quantity{Format: DecimalSI}, decQuantity(0, 0, DecimalSI)},
	}

	for i, test := range tests {
		test.a.Add(test.b)
		if test.a.Cmp(test.expected) != 0 {
			t.Errorf("[%d] Expected %q, got %q", i, test.expected.String(), test.a.String())
		}
	}
}

func TestMul(t *testing.T) {
	tests := []struct {
		a        Quantity
		b        int64
		expected Quantity
		ok       bool
	}{
		{decQuantity(10, 0, DecimalSI), 10, decQuantity(100, 0, DecimalSI), true},
		{decQuantity(10, 0, DecimalSI), 1, decQuantity(10, 0, DecimalSI), true},
		{decQuantity(10, 0, BinarySI), 1, decQuantity(10, 0, BinarySI), true},
		{Quantity{Format: DecimalSI}, 50, decQuantity(0, 0, DecimalSI), true},
		{decQuantity(50, 0, DecimalSI), 0, decQuantity(0, 0, DecimalSI), true},
		{Quantity{Format: DecimalSI}, 0, decQuantity(0, 0, DecimalSI), true},

		{decQuantity(10, 0, DecimalSI), -10, decQuantity(-100, 0, DecimalSI), true},
		{decQuantity(-10, 0, DecimalSI), 1, decQuantity(-10, 0, DecimalSI), true},
		{decQuantity(10, 0, BinarySI), -1, decQuantity(-10, 0, BinarySI), true},
		{decQuantity(-50, 0, DecimalSI), 0, decQuantity(0, 0, DecimalSI), true},
		{decQuantity(-50, 0, DecimalSI), -50, decQuantity(2500, 0, DecimalSI), true},
		{Quantity{Format: DecimalSI}, -50, decQuantity(0, 0, DecimalSI), true},
		{decQuantity(mostPositive, 0, DecimalSI), 0, decQuantity(0, 1, DecimalSI), true},
		{decQuantity(mostPositive, 0, DecimalSI), 1, decQuantity(mostPositive, 0, DecimalSI), true},
		{decQuantity(mostPositive, 0, DecimalSI), -1, decQuantity(-mostPositive, 0, DecimalSI), true},
		{decQuantity(mostPositive/2, 0, DecimalSI), 2, decQuantity((mostPositive/2)*2, 0, DecimalSI), true},
		{decQuantity(mostPositive/-2, 0, DecimalSI), -2, decQuantity((mostPositive/2)*2, 0, DecimalSI), true},
		{decQuantity(mostPositive, 0, DecimalSI), 2,
			bigDecQuantity(big.NewInt(0).Mul(bigMostPositive, big.NewInt(2)), 0, DecimalSI), false},
		{decQuantity(mostPositive, 0, DecimalSI), 10, decQuantity(mostPositive, 1, DecimalSI), false},
		{decQuantity(mostPositive, 0, DecimalSI), -10, decQuantity(-mostPositive, 1, DecimalSI), false},
		{decQuantity(mostNegative, 0, DecimalSI), 0, decQuantity(0, 1, DecimalSI), true},
		{decQuantity(mostNegative, 0, DecimalSI), 1, decQuantity(mostNegative, 0, DecimalSI), true},
		{decQuantity(mostNegative, 0, DecimalSI), -1,
			bigDecQuantity(big.NewInt(0).Add(bigMostPositive, big.NewInt(1)), 0, DecimalSI), false},
		{decQuantity(mostNegative/2, 0, DecimalSI), 2, decQuantity(mostNegative, 0, DecimalSI), true},
		{decQuantity(mostNegative/-2, 0, DecimalSI), -2, decQuantity(mostNegative, 0, DecimalSI), true},
		{decQuantity(mostNegative, 0, DecimalSI), 2,
			bigDecQuantity(big.NewInt(0).Mul(bigMostNegative, big.NewInt(2)), 0, DecimalSI), false},
		{decQuantity(mostNegative, 0, DecimalSI), 10, decQuantity(mostNegative, 1, DecimalSI), false},
		{decQuantity(mostNegative, 0, DecimalSI), -10,
			bigDecQuantity(big.NewInt(0).Add(bigMostPositive, big.NewInt(1)), 1, DecimalSI), false},
	}

	for i, test := range tests {
		if ok := test.a.Mul(test.b); test.ok != ok {
			t.Errorf("[%d] Expected ok: %t, got ok: %t", i, test.ok, ok)
		}
		if test.a.Cmp(test.expected) != 0 {
			t.Errorf("[%d] Expected %q, got %q", i, test.expected.AsDec().String(), test.a.AsDec().String())
		}
	}
}

func TestAddSubRoundTrip(t *testing.T) {
	for k := -10; k <= 10; k++ {
		q := Quantity{Format: DecimalSI}
		var order []int64
		for i := 0; i < 100; i++ {
			j := rand.Int63()
			order = append(order, j)
			q.Add(*NewScaledQuantity(j, Scale(k)))
		}
		for _, j := range order {
			q.Sub(*NewScaledQuantity(j, Scale(k)))
		}
		if !q.IsZero() {
			t.Errorf("addition and subtraction did not cancel: %s", &q)
		}
	}
}

func TestAddSubRoundTripAcrossScales(t *testing.T) {
	q := Quantity{Format: DecimalSI}
	var order []int64
	for i := 0; i < 100; i++ {
		j := rand.Int63()
		order = append(order, j)
		q.Add(*NewScaledQuantity(j, Scale(j%20-10)))
	}
	for _, j := range order {
		q.Sub(*NewScaledQuantity(j, Scale(j%20-10)))
	}
	if !q.IsZero() {
		t.Errorf("addition and subtraction did not cancel: %s", &q)
	}
}

func TestNegateRoundTrip(t *testing.T) {
	for _, asDec := range []bool{false, true} {
		for k := -10; k <= 10; k++ {
			for i := 0; i < 100; i++ {
				j := rand.Int63()
				q := *NewScaledQuantity(j, Scale(k))
				if asDec {
					q.AsDec()
				}

				b := q.DeepCopy()
				b.Neg()
				b.Neg()
				if b.Cmp(q) != 0 {
					t.Errorf("double negation did not cancel: %s", &q)
				}
			}
		}
	}
}

func TestQuantityAsApproximateFloat64(t *testing.T) {
	// NOTE: this table should be kept in sync with TestQuantityAsFloat64Slow
	table := []struct {
		in  Quantity
		out float64
	}{
		{decQuantity(0, 0, DecimalSI), 0.0},
		{decQuantity(0, 0, DecimalExponent), 0.0},
		{decQuantity(0, 0, BinarySI), 0.0},

		{decQuantity(1, 0, DecimalSI), 1},
		{decQuantity(1, 0, DecimalExponent), 1},
		{decQuantity(1, 0, BinarySI), 1},

		// Binary suffixes
		{decQuantity(1024, 0, BinarySI), 1024},
		{decQuantity(8*1024, 0, BinarySI), 8 * 1024},
		{decQuantity(7*1024*1024, 0, BinarySI), 7 * 1024 * 1024},
		{decQuantity(7*1024*1024, 1, BinarySI), (7 * 1024 * 1024) * 10},
		{decQuantity(7*1024*1024, 4, BinarySI), (7 * 1024 * 1024) * 10000},
		{decQuantity(7*1024*1024, 8, BinarySI), (7 * 1024 * 1024) * 100000000},
		{decQuantity(7*1024*1024, -1, BinarySI), (7 * 1024 * 1024) * math.Pow10(-1)}, // '* Pow10' and '/ float(10)' do not round the same way
		{decQuantity(7*1024*1024, -8, BinarySI), (7 * 1024 * 1024) / float64(100000000)},

		{decQuantity(1024, 0, DecimalSI), 1024},
		{decQuantity(8*1024, 0, DecimalSI), 8 * 1024},
		{decQuantity(7*1024*1024, 0, DecimalSI), 7 * 1024 * 1024},
		{decQuantity(7*1024*1024, 1, DecimalSI), (7 * 1024 * 1024) * 10},
		{decQuantity(7*1024*1024, 4, DecimalSI), (7 * 1024 * 1024) * 10000},
		{decQuantity(7*1024*1024, 8, DecimalSI), (7 * 1024 * 1024) * 100000000},
		{decQuantity(7*1024*1024, -1, DecimalSI), (7 * 1024 * 1024) * math.Pow10(-1)}, // '* Pow10' and '/ float(10)' do not round the same way
		{decQuantity(7*1024*1024, -8, DecimalSI), (7 * 1024 * 1024) / float64(100000000)},

		{decQuantity(1024, 0, DecimalExponent), 1024},
		{decQuantity(8*1024, 0, DecimalExponent), 8 * 1024},
		{decQuantity(7*1024*1024, 0, DecimalExponent), 7 * 1024 * 1024},
		{decQuantity(7*1024*1024, 1, DecimalExponent), (7 * 1024 * 1024) * 10},
		{decQuantity(7*1024*1024, 4, DecimalExponent), (7 * 1024 * 1024) * 10000},
		{decQuantity(7*1024*1024, 8, DecimalExponent), (7 * 1024 * 1024) * 100000000},
		{decQuantity(7*1024*1024, -1, DecimalExponent), (7 * 1024 * 1024) * math.Pow10(-1)}, // '* Pow10' and '/ float(10)' do not round the same way
		{decQuantity(7*1024*1024, -8, DecimalExponent), (7 * 1024 * 1024) / float64(100000000)},

		// very large numbers
		{Quantity{d: maxAllowed, Format: DecimalSI}, math.MaxInt64},
		{Quantity{d: maxAllowed, Format: BinarySI}, math.MaxInt64},
		{decQuantity(12, 18, DecimalSI), 1.2e19},

		// infinities caused due to float64 overflow
		{decQuantity(12, 500, DecimalSI), math.Inf(0)},
		{decQuantity(-12, 500, DecimalSI), math.Inf(-1)},
	}

	for i, item := range table {
		t.Run(fmt.Sprintf("%s %s", item.in.Format, item.in.String()), func(t *testing.T) {
			out := item.in.AsApproximateFloat64()
			if out != item.out {
				t.Fatalf("test %d expected %v, got %v", i+1, item.out, out)
			}
			if item.in.d.Dec != nil {
				if i, ok := item.in.AsInt64(); ok {
					q := intQuantity(i, 0, item.in.Format)
					out := q.AsApproximateFloat64()
					if out != item.out {
						t.Fatalf("as int quantity: expected %v, got %v", item.out, out)
					}
				}
			}
		})
	}
}

func TestQuantityAsFloat64Slow(t *testing.T) {
	// NOTE: this table should be kept in sync with TestQuantityAsApproximateFloat64
	table := []struct {
		in  Quantity
		out float64
	}{
		{decQuantity(0, 0, DecimalSI), 0.0},
		{decQuantity(0, 0, DecimalExponent), 0.0},
		{decQuantity(0, 0, BinarySI), 0.0},

		{decQuantity(1, 0, DecimalSI), 1},
		{decQuantity(1, 0, DecimalExponent), 1},
		{decQuantity(1, 0, BinarySI), 1},

		// Binary suffixes
		{decQuantity(1024, 0, BinarySI), 1024},
		{decQuantity(8*1024, 0, BinarySI), 8 * 1024},
		{decQuantity(7*1024*1024, 0, BinarySI), 7 * 1024 * 1024},
		{decQuantity(7*1024*1024, 1, BinarySI), (7 * 1024 * 1024) * 10},
		{decQuantity(7*1024*1024, 4, BinarySI), (7 * 1024 * 1024) * 10000},
		{decQuantity(7*1024*1024, 8, BinarySI), (7 * 1024 * 1024) * 100000000},
		{decQuantity(7*1024*1024, -1, BinarySI), (7 * 1024 * 1024) / float64(10)},
		{decQuantity(7*1024*1024, -8, BinarySI), (7 * 1024 * 1024) / float64(100000000)},

		{decQuantity(1024, 0, DecimalSI), 1024},
		{decQuantity(8*1024, 0, DecimalSI), 8 * 1024},
		{decQuantity(7*1024*1024, 0, DecimalSI), 7 * 1024 * 1024},
		{decQuantity(7*1024*1024, 1, DecimalSI), (7 * 1024 * 1024) * 10},
		{decQuantity(7*1024*1024, 4, DecimalSI), (7 * 1024 * 1024) * 10000},
		{decQuantity(7*1024*1024, 8, DecimalSI), (7 * 1024 * 1024) * 100000000},
		{decQuantity(7*1024*1024, -1, DecimalSI), (7 * 1024 * 1024) / float64(10)},
		{decQuantity(7*1024*1024, -8, DecimalSI), (7 * 1024 * 1024) / float64(100000000)},

		{decQuantity(1024, 0, DecimalExponent), 1024},
		{decQuantity(8*1024, 0, DecimalExponent), 8 * 1024},
		{decQuantity(7*1024*1024, 0, DecimalExponent), 7 * 1024 * 1024},
		{decQuantity(7*1024*1024, 1, DecimalExponent), (7 * 1024 * 1024) * 10},
		{decQuantity(7*1024*1024, 4, DecimalExponent), (7 * 1024 * 1024) * 10000},
		{decQuantity(7*1024*1024, 8, DecimalExponent), (7 * 1024 * 1024) * 100000000},
		{decQuantity(7*1024*1024, -1, DecimalExponent), (7 * 1024 * 1024) / float64(10)},
		{decQuantity(7*1024*1024, -8, DecimalExponent), (7 * 1024 * 1024) / float64(100000000)},

		// very large numbers
		{Quantity{d: maxAllowed, Format: DecimalSI}, math.MaxInt64},
		{Quantity{d: maxAllowed, Format: BinarySI}, math.MaxInt64},
		{decQuantity(12, 18, DecimalSI), 1.2e19},

		// infinities caused due to float64 overflow
		{decQuantity(12, 500, DecimalSI), math.Inf(0)},
		{decQuantity(-12, 500, DecimalSI), math.Inf(-1)},
	}

	for i, item := range table {
		t.Run(fmt.Sprintf("%s %s", item.in.Format, item.in.String()), func(t *testing.T) {
			out := item.in.AsFloat64Slow()
			if out != item.out {
				t.Fatalf("test %d expected %v, got %v", i+1, item.out, out)
			}
			if item.in.d.Dec != nil {
				if i, ok := item.in.AsInt64(); ok {
					q := intQuantity(i, 0, item.in.Format)
					out := q.AsFloat64Slow()
					if out != item.out {
						t.Fatalf("as int quantity: expected %v, got %v", item.out, out)
					}
				}
			}
		})
	}
}

func TestStringQuantityAsApproximateFloat64(t *testing.T) {
	table := []struct {
		in  string
		out float64
	}{
		{"2Ki", 2048},
		{"1.1Ki", 1126.4e+0},
		{"1Mi", 1.048576e+06},
		{"2Gi", 2.147483648e+09},
	}

	for _, item := range table {
		t.Run(item.in, func(t *testing.T) {
			in, err := ParseQuantity(item.in)
			if err != nil {
				t.Fatal(err)
			}
			out := in.AsApproximateFloat64()
			if out != item.out {
				t.Fatalf("expected %v, got %v", item.out, out)
			}
			if in.d.Dec != nil {
				if i, ok := in.AsInt64(); ok {
					q := intQuantity(i, 0, in.Format)
					out := q.AsApproximateFloat64()
					if out != item.out {
						t.Fatalf("as int quantity: expected %v, got %v", item.out, out)
					}
				}
			}
		})
	}
}

func TestStringQuantityAsFloat64Slow(t *testing.T) {
	table := []struct {
		in  string
		out float64
	}{
		{"2Ki", 2048},
		{"1.1Ki", 1126.4e+0},
		{"1Mi", 1.048576e+06},
		{"2Gi", 2.147483648e+09},
	}

	for _, item := range table {
		t.Run(item.in, func(t *testing.T) {
			in, err := ParseQuantity(item.in)
			if err != nil {
				t.Fatal(err)
			}
			out := in.AsFloat64Slow()
			if out != item.out {
				t.Fatalf("expected %v, got %v", item.out, out)
			}
			if in.d.Dec != nil {
				if i, ok := in.AsInt64(); ok {
					q := intQuantity(i, 0, in.Format)
					out := q.AsFloat64Slow()
					if out != item.out {
						t.Fatalf("as int quantity: expected %v, got %v", item.out, out)
					}
				}
			}
		})
	}
}

func benchmarkQuantities() []Quantity {
	return []Quantity{
		intQuantity(1024*1024*1024, 0, BinarySI),
		intQuantity(1024*1024*1024*1024, 0, BinarySI),
		intQuantity(1000000, 3, DecimalSI),
		intQuantity(1000000000, 0, DecimalSI),
		intQuantity(1, -3, DecimalSI),
		intQuantity(80, -3, DecimalSI),
		intQuantity(1080, -3, DecimalSI),
		intQuantity(0, 0, BinarySI),
		intQuantity(1, 9, DecimalExponent),
		intQuantity(1, -9, DecimalSI),
		intQuantity(1000000, 10, DecimalSI),
	}
}

func BenchmarkQuantityString(b *testing.B) {
	values := benchmarkQuantities()
	b.ResetTimer()
	var s string
	for i := 0; i < b.N; i++ {
		q := values[i%len(values)]
		q.s = ""
		s = q.String()
	}
	b.StopTimer()
	if len(s) == 0 {
		b.Fatal(s)
	}
}

func BenchmarkQuantityStringPrecalc(b *testing.B) {
	values := benchmarkQuantities()
	for i := range values {
		_ = values[i].String()
	}
	b.ResetTimer()
	var s string
	for i := 0; i < b.N; i++ {
		q := values[i%len(values)]
		s = q.String()
	}
	b.StopTimer()
	if len(s) == 0 {
		b.Fatal(s)
	}
}

func BenchmarkQuantityStringBinarySI(b *testing.B) {
	values := benchmarkQuantities()
	for i := range values {
		values[i].Format = BinarySI
	}
	b.ResetTimer()
	var s string
	for i := 0; i < b.N; i++ {
		q := values[i%len(values)]
		q.s = ""
		s = q.String()
	}
	b.StopTimer()
	if len(s) == 0 {
		b.Fatal(s)
	}
}

func BenchmarkQuantityMarshalJSON(b *testing.B) {
	values := benchmarkQuantities()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := values[i%len(values)]
		q.s = ""
		if _, err := q.MarshalJSON(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

func BenchmarkQuantityUnmarshalJSON(b *testing.B) {
	values := benchmarkQuantities()
	var json [][]byte
	for _, v := range values {
		data, _ := v.MarshalJSON()
		json = append(json, data)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var q Quantity
		if err := q.UnmarshalJSON(json[i%len(values)]); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

func BenchmarkParseQuantity(b *testing.B) {
	values := benchmarkQuantities()
	var strings []string
	for _, v := range values {
		strings = append(strings, v.String())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseQuantity(strings[i%len(values)]); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

func BenchmarkCanonicalize(b *testing.B) {
	values := benchmarkQuantities()
	b.ResetTimer()
	buffer := make([]byte, 0, 100)
	for i := 0; i < b.N; i++ {
		s, _ := values[i%len(values)].CanonicalizeBytes(buffer)
		if len(s) == 0 {
			b.Fatal(s)
		}
	}
	b.StopTimer()
}

func BenchmarkQuantityRoundUp(b *testing.B) {
	values := benchmarkQuantities()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := values[i%len(values)]
		copied := q
		copied.RoundUp(-3)
	}
	b.StopTimer()
}

func BenchmarkQuantityCopy(b *testing.B) {
	values := benchmarkQuantities()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		values[i%len(values)].DeepCopy()
	}
	b.StopTimer()
}

func BenchmarkQuantityAdd(b *testing.B) {
	values := benchmarkQuantities()
	base := &Quantity{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := values[i%len(values)]
		base.d.Dec = nil
		base.i = int64Amount{value: 100}
		base.Add(q)
	}
	b.StopTimer()
}

func BenchmarkQuantityCmp(b *testing.B) {
	values := benchmarkQuantities()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := values[i%len(values)]
		if q.Cmp(q) != 0 {
			b.Fatal(q)
		}
	}
	b.StopTimer()
}

func BenchmarkQuantityAsApproximateFloat64(b *testing.B) {
	values := benchmarkQuantities()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := values[i%len(values)]
		if q.AsApproximateFloat64() == -1 {
			b.Fatal(q)
		}
	}
	b.StopTimer()
}

func BenchmarkQuantityAsFloat64Slow(b *testing.B) {
	values := benchmarkQuantities()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := values[i%len(values)]
		if q.AsFloat64Slow() == -1 {
			b.Fatal(q)
		}
	}
	b.StopTimer()
}

var _ pflag.Value = &QuantityValue{}

func TestQuantityValueSet(t *testing.T) {
	q := QuantityValue{}

	if err := q.Set("invalid"); err == nil {

		t.Error("'invalid' did not trigger a parse error")
	}

	if err := q.Set("1Mi"); err != nil {
		t.Errorf("parsing 1Mi should have worked, got: %v", err)
	}
	if q.Value() != 1024*1024 {
		t.Errorf("quantity should have been set to 1Mi, got: %v", q)
	}

	data, err := json.Marshal(q)
	if err != nil {
		t.Errorf("unexpected encoding error: %v", err)
	}
	expected := `"1Mi"`
	if string(data) != expected {
		t.Errorf("expected 1Mi value to be encoded as %q, got: %q", expected, string(data))
	}
}

func ExampleQuantityValue() {
	q := QuantityValue{
		Quantity: MustParse("1Mi"),
	}
	fs := pflag.FlagSet{}
	fs.SetOutput(os.Stdout)
	fs.Var(&q, "mem", "sets amount of memory")
	fs.PrintDefaults()
	// Output:
	// --mem quantity   sets amount of memory (default 1Mi)
}

func TestQuantityUnmarshalCBOR(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         []byte
		want       Quantity
		errMessage string
	}{
		{
			name: "null",
			in:   []byte{0xf6}, // null
			want: Quantity{},
		},
		{
			name: "text string input",
			in:   []byte("\x621M"), // "1M"
			want: Quantity{i: int64Amount{value: 1, scale: 6}},
		},
		{
			name: "byte string input",
			in:   []byte("\x421M"), // '1M'
			want: Quantity{i: int64Amount{value: 1, scale: 6}},
		},
		{
			name: "whitespace",
			in:   []byte("\x4a \t\n\r1M \t\n\r"), // h'20090a0d314d20090a0d'
			want: Quantity{i: int64Amount{value: 1, scale: 6}},
		},
		{
			name:       "empty byte string",
			in:         []byte{0x40},
			errMessage: ErrFormatWrong.Error(),
		},
		{
			name:       "empty text string",
			in:         []byte{0x60},
			errMessage: ErrFormatWrong.Error(),
		},
		{
			name:       "unsupported input type",
			in:         []byte{0x07}, // 7
			errMessage: "cbor: cannot unmarshal positive integer into Go value of type string",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got Quantity
			if err := got.UnmarshalCBOR(tc.in); err != nil {
				if tc.errMessage == "" {
					t.Fatalf("want nil error, got: %v", err)
				} else if gotMessage := err.Error(); tc.errMessage != gotMessage {
					t.Fatalf("want error: %q, got: %q", tc.errMessage, gotMessage)
				}
			} else if tc.errMessage != "" {
				t.Fatalf("got nil error, want: %s", tc.errMessage)
			}

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected diff:\n%s", diff)
			}
		})
	}
}

func TestQuantityRoundtripCBOR(t *testing.T) {
	for i := 0; i < 500; i++ {
		var initial, final Quantity
		fuzzer.Fuzz(&initial)
		b, err := cbor.Marshal(initial)
		if err != nil {
			t.Errorf("error encoding %v: %v", initial, err)
			continue
		}
		err = cbor.Unmarshal(b, &final)
		if err != nil {
			t.Errorf("%v: error decoding %v: %v", initial, string(b), err)
		}
		if final.Cmp(initial) != 0 {
			diag, err := cbor.Diagnose(b)
			if err != nil {
				t.Logf("failed to produce diagnostic encoding of 0x%x: %v", b, err)
			}
			t.Errorf("Expected equal: %v, %v (cbor was '%s')", initial, final, diag)
		}
	}
}

func TestMilliChecked(t *testing.T) {
	for _, tc := range []struct {
		name        string
		q           *Quantity
		expectError bool
	}{
		{
			name:        "overflow",
			q:           func() *Quantity { q := MustParse("16Pi"); return &q }(),
			expectError: true,
		},
		{
			name:        "1 SI",
			q:           NewQuantity(1, BinarySI),
			expectError: false,
		},
		{
			name:        "0 SI",
			q:           NewQuantity(0, BinarySI),
			expectError: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.q.CheckedMilliValue()
			if err == nil && tc.expectError {
				t.Error("expected error but got none")
			} else if err != nil && !tc.expectError {
				t.Errorf("expected no error error but got %v", err)
			}
		})
	}
}
