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
	"bytes"
	"errors"
	"fmt"
	math "math"
	"math/big"
	"strconv"
	"strings"

	cbor "k8s.io/apimachinery/pkg/runtime/serializer/cbor/direct"

	inf "gopkg.in/inf.v0"
)

// Quantity is a fixed-point representation of a number.
// It provides convenient marshaling/unmarshaling in JSON and YAML,
// in addition to String() and AsInt64() accessors.
//
// The serialization format is:
//
// ```
// <quantity>        ::= <signedNumber><suffix>
//
//	(Note that <suffix> may be empty, from the "" case in <decimalSI>.)
//
// <digit>           ::= 0 | 1 | ... | 9
// <digits>          ::= <digit> | <digit><digits>
// <number>          ::= <digits> | <digits>.<digits> | <digits>. | .<digits>
// <sign>            ::= "+" | "-"
// <signedNumber>    ::= <number> | <sign><number>
// <suffix>          ::= <binarySI> | <decimalExponent> | <decimalSI>
// <binarySI>        ::= Ki | Mi | Gi | Ti | Pi | Ei
//
//	(International System of units; See: http://physics.nist.gov/cuu/Units/binary.html)
//
// <decimalSI>       ::= m | "" | k | M | G | T | P | E
//
//	(Note that 1024 = 1Ki but 1000 = 1k; I didn't choose the capitalization.)
//
// <decimalExponent> ::= "e" <signedNumber> | "E" <signedNumber>
// ```
//
// No matter which of the three exponent forms is used, no quantity may represent
// a number greater than 2^63-1 in magnitude, nor may it have more than 3 decimal
// places. Numbers larger or more precise will be capped or rounded up.
// (E.g.: 0.1m will rounded up to 1m.)
// This may be extended in the future if we require larger or smaller quantities.
//
// When a Quantity is parsed from a string, it will remember the type of suffix
// it had, and will use the same type again when it is serialized.
//
// Before serializing, Quantity will be put in "canonical form".
// This means that Exponent/suffix will be adjusted up or down (with a
// corresponding increase or decrease in Mantissa) such that:
//
// - No precision is lost
// - No fractional digits will be emitted
// - The exponent (or suffix) is as large as possible.
//
// The sign will be omitted unless the number is negative.
//
// Examples:
//
// - 1.5 will be serialized as "1500m"
// - 1.5Gi will be serialized as "1536Mi"
//
// Note that the quantity will NEVER be internally represented by a
// floating point number. That is the whole point of this exercise.
//
// Non-canonical values will still parse as long as they are well formed,
// but will be re-emitted in their canonical form. (So always use canonical
// form, or don't diff.)
//
// This format is intended to make it difficult to use these numbers without
// writing some sort of special handling code in the hopes that that will
// cause implementors to also use a fixed point implementation.
//
// +protobuf=true
// +protobuf.embed=string
// +protobuf.options.marshal=false
// +protobuf.options.(gogoproto.goproto_stringer)=false
// +k8s:deepcopy-gen=true
// +k8s:openapi-gen=true
type Quantity struct {
	// i is the quantity in int64 scaled form, if d.Dec == nil
	i int64Amount
	// d is the quantity in inf.Dec form if d.Dec != nil
	d infDecAmount
	// s is the generated value of this quantity to avoid recalculation
	s string

	// Change Format at will. See the comment for Canonicalize for
	// more details.
	Format
}

// CanonicalValue allows a quantity amount to be converted to a string.
type CanonicalValue interface {
	// AsCanonicalBytes returns a byte array representing the string representation
	// of the value mantissa and an int32 representing its exponent in base-10. Callers may
	// pass a byte slice to the method to avoid allocations.
	AsCanonicalBytes(out []byte) ([]byte, int32)
	// AsCanonicalBase1024Bytes returns a byte array representing the string representation
	// of the value mantissa and an int32 representing its exponent in base-1024. Callers
	// may pass a byte slice to the method to avoid allocations.
	AsCanonicalBase1024Bytes(out []byte) ([]byte, int32)
}

// Format lists the three possible formattings of a quantity.
type Format string

const (
	DecimalExponent = Format("DecimalExponent") // e.g., 12e6
	BinarySI        = Format("BinarySI")        // e.g., 12Mi (12 * 2^20)
	DecimalSI       = Format("DecimalSI")       // e.g., 12M  (12 * 10^6)
)

// MustParse turns the given string into a quantity or panics; for tests
// or other cases where you know the string is valid.
func MustParse(str string) Quantity {
	q, err := ParseQuantity(str)
	if err != nil {
		panic(fmt.Errorf("cannot parse '%v': %v", str, err))
	}
	return q
}

const (
	// splitREString is used to separate a number from its suffix; as such,
	// this is overly permissive, but that's OK-- it will be checked later.
	splitREString = "^([+-]?[0-9.]+)([eEinumkKMGTP]*[-+]?[0-9]*)$"
)

var (
	// Errors that could happen while parsing a string.
	ErrFormatWrong = errors.New("quantities must match the regular expression '" + splitREString + "'")
	ErrNumeric     = errors.New("unable to parse numeric part of quantity")
	ErrSuffix      = errors.New("unable to parse quantity's suffix")
)

// parseQuantityString is a fast scanner for quantity values.
func parseQuantityString(str string) (positive bool, value, num, denom, suffix string, err error) {
	positive = true
	pos := 0
	end := len(str)

	// handle leading sign
	if pos < end {
		switch str[0] {
		case '-':
			positive = false
			pos++
		case '+':
			pos++
		}
	}

	// strip leading zeros
Zeroes:
	for i := pos; ; i++ {
		if i >= end {
			num = "0"
			value = num
			return
		}
		switch str[i] {
		case '0':
			pos++
		default:
			break Zeroes
		}
	}

	// extract the numerator
Num:
	for i := pos; ; i++ {
		if i >= end {
			num = str[pos:end]
			value = str[0:end]
			return
		}
		switch str[i] {
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		default:
			num = str[pos:i]
			pos = i
			break Num
		}
	}

	// if we stripped all numerator positions, always return 0
	if len(num) == 0 {
		num = "0"
	}

	// handle a denominator
	if pos < end && str[pos] == '.' {
		pos++
	Denom:
		for i := pos; ; i++ {
			if i >= end {
				denom = str[pos:end]
				value = str[0:end]
				return
			}
			switch str[i] {
			case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			default:
				denom = str[pos:i]
				pos = i
				break Denom
			}
		}
		// TODO: we currently allow 1.G, but we may not want to in the future.
		// if len(denom) == 0 {
		// 	err = ErrFormatWrong
		// 	return
		// }
	}
	value = str[0:pos]

	// grab the elements of the suffix
	suffixStart := pos
	for i := pos; ; i++ {
		if i >= end {
			suffix = str[suffixStart:end]
			return
		}
		if !strings.ContainsAny(str[i:i+1], "eEinumkKMGTP") {
			pos = i
			break
		}
	}
	if pos < end {
		switch str[pos] {
		case '-', '+':
			pos++
		}
	}
Suffix:
	for i := pos; ; i++ {
		if i >= end {
			suffix = str[suffixStart:end]
			return
		}
		switch str[i] {
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		default:
			break Suffix
		}
	}
	// we encountered a non decimal in the Suffix loop, but the last character
	// was not a valid exponent
	err = ErrFormatWrong
	return
}

// ParseQuantity turns str into a Quantity, or returns an error.
func ParseQuantity(str string) (Quantity, error) {
	if len(str) == 0 {
		return Quantity{}, ErrFormatWrong
	}
	if str == "0" {
		return Quantity{Format: DecimalSI, s: str}, nil
	}

	positive, value, num, denom, suf, err := parseQuantityString(str)
	if err != nil {
		return Quantity{}, err
	}

	base, exponent, format, ok := quantitySuffixer.interpret(suffix(suf))
	if !ok {
		return Quantity{}, ErrSuffix
	}

	precision := int32(0)
	scale := int32(0)
	mantissa := int64(1)
	switch format {
	case DecimalExponent, DecimalSI:
		scale = exponent
		precision = maxInt64Factors - int32(len(num)+len(denom))
	case BinarySI:
		scale = 0
		switch {
		case exponent >= 0 && len(denom) == 0:
			// only handle positive binary numbers with the fast path
			mantissa = int64(int64(mantissa) << uint64(exponent))
			// 1Mi (2^20) has ~6 digits of decimal precision, so exponent*3/10 -1 is roughly the precision
			precision = 15 - int32(len(num)) - int32(float32(exponent)*3/10) - 1
		default:
			precision = -1
		}
	}

	if precision >= 0 {
		// if we have a denominator, shift the entire value to the left by the number of places in the
		// denominator
		scale -= int32(len(denom))
		if scale >= int32(Nano) {
			shifted := num + denom

			var value int64
			value, err := strconv.ParseInt(shifted, 10, 64)
			if err != nil {
				return Quantity{}, ErrNumeric
			}
			if result, ok := int64Multiply(value, int64(mantissa)); ok {
				if !positive {
					result = -result
				}
				// if the number is in canonical form, reuse the string
				switch format {
				case BinarySI:
					if exponent%10 == 0 && (value&0x07 != 0) {
						return Quantity{i: int64Amount{value: result, scale: Scale(scale)}, Format: format, s: str}, nil
					}
				default:
					if scale%3 == 0 && !strings.HasSuffix(shifted, "000") && shifted[0] != '0' {
						return Quantity{i: int64Amount{value: result, scale: Scale(scale)}, Format: format, s: str}, nil
					}
				}
				return Quantity{i: int64Amount{value: result, scale: Scale(scale)}, Format: format}, nil
			}
		}
	}

	amount := new(inf.Dec)
	if _, ok := amount.SetString(value); !ok {
		return Quantity{}, ErrNumeric
	}

	// So that no one but us has to think about suffixes, remove it.
	if base == 10 {
		amount.SetScale(amount.Scale() + Scale(exponent).infScale())
	} else if base == 2 {
		// numericSuffix = 2 ** exponent
		numericSuffix := big.NewInt(1).Lsh(bigOne, uint(exponent))
		ub := amount.UnscaledBig()
		amount.SetUnscaledBig(ub.Mul(ub, numericSuffix))
	}

	// Cap at min/max bounds.
	sign := amount.Sign()
	if sign == -1 {
		amount.Neg(amount)
	}

	// This rounds non-zero values up to the minimum representable value, under the theory that
	// if you want some resources, you should get some resources, even if you asked for way too small
	// of an amount.  Arguably, this should be inf.RoundHalfUp (normal rounding), but that would have
	// the side effect of rounding values < .5n to zero.
	if v, ok := amount.Unscaled(); v != int64(0) || !ok {
		amount.Round(amount, Nano.infScale(), inf.RoundUp)
	}

	// The max is just a simple cap.
	// TODO: this prevents accumulating quantities greater than int64, for instance quota across a cluster
	if format == BinarySI && amount.Cmp(maxAllowed.Dec) > 0 {
		amount.Set(maxAllowed.Dec)
	}

	if format == BinarySI && amount.Cmp(decOne) < 0 && amount.Cmp(decZero) > 0 {
		// This avoids rounding and hopefully confusion, too.
		format = DecimalSI
	}
	if sign == -1 {
		amount.Neg(amount)
	}

	return Quantity{d: infDecAmount{amount}, Format: format}, nil
}

// DeepCopy returns a deep-copy of the Quantity value.  Note that the method
// receiver is a value, so we can mutate it in-place and return it.
func (q Quantity) DeepCopy() Quantity {
	if q.d.Dec != nil {
		tmp := &inf.Dec{}
		q.d.Dec = tmp.Set(q.d.Dec)
	}
	return q
}

// OpenAPISchemaType is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
//
// See: https://github.com/kubernetes/kube-openapi/tree/master/pkg/generators
func (_ Quantity) OpenAPISchemaType() []string { return []string{"string"} }

// OpenAPISchemaFormat is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
func (_ Quantity) OpenAPISchemaFormat() string { return "" }

// OpenAPIV3OneOfTypes is used by the kube-openapi generator when constructing
// the OpenAPI v3 spec of this type.
func (Quantity) OpenAPIV3OneOfTypes() []string { return []string{"string", "number"} }

// CanonicalizeBytes returns the canonical form of q and its suffix (see comment on Quantity).
//
// Note about BinarySI:
//   - If q.Format is set to BinarySI and q.Amount represents a non-zero value between
//     -1 and +1, it will be emitted as if q.Format were DecimalSI.
//   - Otherwise, if q.Format is set to BinarySI, fractional parts of q.Amount will be
//     rounded up. (1.1i becomes 2i.)
func (q *Quantity) CanonicalizeBytes(out []byte) (result, suffix []byte) {
	if q.IsZero() {
		return zeroBytes, nil
	}

	var rounded CanonicalValue
	format := q.Format
	switch format {
	case DecimalExponent, DecimalSI:
	case BinarySI:
		if q.CmpInt64(-1024) > 0 && q.CmpInt64(1024) < 0 {
			// This avoids rounding and hopefully confusion, too.
			format = DecimalSI
		} else {
			var exact bool
			if rounded, exact = q.AsScale(0); !exact {
				// Don't lose precision-- show as DecimalSI
				format = DecimalSI
			}
		}
	default:
		format = DecimalExponent
	}

	// TODO: If BinarySI formatting is requested but would cause rounding, upgrade to
	// one of the other formats.
	switch format {
	case DecimalExponent, DecimalSI:
		number, exponent := q.AsCanonicalBytes(out)
		suffix, _ := quantitySuffixer.constructBytes(10, exponent, format)
		return number, suffix
	default:
		// format must be BinarySI
		number, exponent := rounded.AsCanonicalBase1024Bytes(out)
		suffix, _ := quantitySuffixer.constructBytes(2, exponent*10, format)
		return number, suffix
	}
}

// AsApproximateFloat64 returns a float64 representation of the quantity which
// may lose precision. If precision matter more than performance, see
// AsFloat64Slow. If the value of the quantity is outside the range of a
// float64 +Inf/-Inf will be returned.
func (q *Quantity) AsApproximateFloat64() float64 {
	var base float64
	var exponent int
	if q.d.Dec != nil {
		base, _ = big.NewFloat(0).SetInt(q.d.Dec.UnscaledBig()).Float64()
		exponent = int(-q.d.Dec.Scale())
	} else {
		base = float64(q.i.value)
		exponent = int(q.i.scale)
	}
	if exponent == 0 {
		return base
	}

	return base * math.Pow10(exponent)
}

// AsFloat64Slow returns a float64 representation of the quantity.  This is
// more precise than AsApproximateFloat64 but significantly slower.  If the
// value of the quantity is outside the range of a float64 +Inf/-Inf will be
// returned.
func (q *Quantity) AsFloat64Slow() float64 {
	infDec := q.AsDec()

	var absScale int64
	if infDec.Scale() < 0 {
		absScale = int64(-infDec.Scale())
	} else {
		absScale = int64(infDec.Scale())
	}
	pow10AbsScale := big.NewInt(10)
	pow10AbsScale = pow10AbsScale.Exp(pow10AbsScale, big.NewInt(absScale), nil)

	var resultBigFloat *big.Float
	if infDec.Scale() < 0 {
		resultBigInt := new(big.Int).Mul(infDec.UnscaledBig(), pow10AbsScale)
		resultBigFloat = new(big.Float).SetInt(resultBigInt)
	} else {
		pow10AbsScaleFloat := new(big.Float).SetInt(pow10AbsScale)
		resultBigFloat = new(big.Float).SetInt(infDec.UnscaledBig())
		resultBigFloat = resultBigFloat.Quo(resultBigFloat, pow10AbsScaleFloat)
	}

	result, _ := resultBigFloat.Float64()
	return result
}

// AsInt64 returns a representation of the current value as an int64 if a fast conversion
// is possible. If false is returned, callers must use the inf.Dec form of this quantity.
func (q *Quantity) AsInt64() (int64, bool) {
	if q.d.Dec != nil {
		return 0, false
	}
	return q.i.AsInt64()
}

// ToDec promotes the quantity in place to use an inf.Dec representation and returns itself.
func (q *Quantity) ToDec() *Quantity {
	if q.d.Dec == nil {
		q.d.Dec = q.i.AsDec()
		q.i = int64Amount{}
	}
	return q
}

// AsDec returns the quantity as represented by a scaled inf.Dec.
func (q *Quantity) AsDec() *inf.Dec {
	if q.d.Dec != nil {
		return q.d.Dec
	}
	q.d.Dec = q.i.AsDec()
	q.i = int64Amount{}
	return q.d.Dec
}

// AsCanonicalBytes returns the canonical byte representation of this quantity as a mantissa
// and base 10 exponent. The out byte slice may be passed to the method to avoid an extra
// allocation.
func (q *Quantity) AsCanonicalBytes(out []byte) (result []byte, exponent int32) {
	if q.d.Dec != nil {
		return q.d.AsCanonicalBytes(out)
	}
	return q.i.AsCanonicalBytes(out)
}

// IsZero returns true if the quantity is equal to zero.
func (q *Quantity) IsZero() bool {
	if q.d.Dec != nil {
		return q.d.Dec.Sign() == 0
	}
	return q.i.value == 0
}

// Sign returns 0 if the quantity is zero, -1 if the quantity is less than zero, or 1 if the
// quantity is greater than zero.
func (q *Quantity) Sign() int {
	if q.d.Dec != nil {
		return q.d.Dec.Sign()
	}
	return q.i.Sign()
}

// AsScale returns the current value, rounded up to the provided scale, and returns
// false if the scale resulted in a loss of precision.
func (q *Quantity) AsScale(scale Scale) (CanonicalValue, bool) {
	if q.d.Dec != nil {
		return q.d.AsScale(scale)
	}
	return q.i.AsScale(scale)
}

// RoundUp updates the quantity to the provided scale, ensuring that the value is at
// least 1. False is returned if the rounding operation resulted in a loss of precision.
// Negative numbers are rounded away from zero (-9 scale 1 rounds to -10).
func (q *Quantity) RoundUp(scale Scale) bool {
	if q.d.Dec != nil {
		q.s = ""
		d, exact := q.d.AsScale(scale)
		q.d = d
		return exact
	}
	// avoid clearing the string value if we have already calculated it
	if q.i.scale >= scale {
		return true
	}
	q.s = ""
	i, exact := q.i.AsScale(scale)
	q.i = i
	return exact
}

// Add adds the provide y quantity to the current value. If the current value is zero,
// the format of the quantity will be updated to the format of y.
func (q *Quantity) Add(y Quantity) {
	q.s = ""
	if q.d.Dec == nil && y.d.Dec == nil {
		if q.i.value == 0 {
			q.Format = y.Format
		}
		if q.i.Add(y.i) {
			return
		}
	} else if q.IsZero() {
		q.Format = y.Format
	}
	q.ToDec().d.Dec.Add(q.d.Dec, y.AsDec())
}

// Sub subtracts the provided quantity from the current value in place. If the current
// value is zero, the format of the quantity will be updated to the format of y.
func (q *Quantity) Sub(y Quantity) {
	q.s = ""
	if q.IsZero() {
		q.Format = y.Format
	}
	if q.d.Dec == nil && y.d.Dec == nil && q.i.Sub(y.i) {
		return
	}
	q.ToDec().d.Dec.Sub(q.d.Dec, y.AsDec())
}

// Mul multiplies the provided y to the current value.
// It will return false if the result is inexact. Otherwise, it will return true.
func (q *Quantity) Mul(y int64) bool {
	q.s = ""
	if q.d.Dec == nil && q.i.Mul(y) {
		return true
	}
	return q.ToDec().d.Dec.Mul(q.d.Dec, inf.NewDec(y, inf.Scale(0))).UnscaledBig().IsInt64()
}

// Cmp returns 0 if the quantity is equal to y, -1 if the quantity is less than y, or 1 if the
// quantity is greater than y.
func (q *Quantity) Cmp(y Quantity) int {
	if q.d.Dec == nil && y.d.Dec == nil {
		return q.i.Cmp(y.i)
	}
	return q.AsDec().Cmp(y.AsDec())
}

// CmpInt64 returns 0 if the quantity is equal to y, -1 if the quantity is less than y, or 1 if the
// quantity is greater than y.
func (q *Quantity) CmpInt64(y int64) int {
	if q.d.Dec != nil {
		return q.d.Dec.Cmp(inf.NewDec(y, inf.Scale(0)))
	}
	return q.i.Cmp(int64Amount{value: y})
}

// Neg sets quantity to be the negative value of itself.
func (q *Quantity) Neg() {
	q.s = ""
	if q.d.Dec == nil {
		q.i.value = -q.i.value
		return
	}
	q.d.Dec.Neg(q.d.Dec)
}

// Equal checks equality of two Quantities. This is useful for testing with
// cmp.Equal.
func (q Quantity) Equal(v Quantity) bool {
	return q.Cmp(v) == 0
}

// int64QuantityExpectedBytes is the expected width in bytes of the canonical string representation
// of most Quantity values.
const int64QuantityExpectedBytes = 18

// String formats the Quantity as a string, caching the result if not calculated.
// String is an expensive operation and caching this result significantly reduces the cost of
// normal parse / marshal operations on Quantity.
func (q *Quantity) String() string {
	if q == nil {
		return "<nil>"
	}
	if len(q.s) == 0 {
		result := make([]byte, 0, int64QuantityExpectedBytes)
		number, suffix := q.CanonicalizeBytes(result)
		number = append(number, suffix...)
		q.s = string(number)
	}
	return q.s
}

// MarshalJSON implements the json.Marshaller interface.
func (q Quantity) MarshalJSON() ([]byte, error) {
	if len(q.s) > 0 {
		out := make([]byte, len(q.s)+2)
		out[0], out[len(out)-1] = '"', '"'
		copy(out[1:], q.s)
		return out, nil
	}
	result := make([]byte, int64QuantityExpectedBytes)
	result[0] = '"'
	number, suffix := q.CanonicalizeBytes(result[1:1])
	// if the same slice was returned to us that we passed in, avoid another allocation by copying number into
	// the source slice and returning that
	if len(number) > 0 && &number[0] == &result[1] && (len(number)+len(suffix)+2) <= int64QuantityExpectedBytes {
		number = append(number, suffix...)
		number = append(number, '"')
		return result[:1+len(number)], nil
	}
	// if CanonicalizeBytes needed more space than our slice provided, we may need to allocate again so use
	// append
	result = result[:1]
	result = append(result, number...)
	result = append(result, suffix...)
	result = append(result, '"')
	return result, nil
}

func (q Quantity) MarshalCBOR() ([]byte, error) {
	// The call to String() should never return the string "<nil>" because the receiver's
	// address will never be nil.
	return cbor.Marshal(q.String())
}

// ToUnstructured implements the value.UnstructuredConverter interface.
func (q Quantity) ToUnstructured() interface{} {
	return q.String()
}

// UnmarshalJSON implements the json.Unmarshaller interface.
// TODO: Remove support for leading/trailing whitespace
func (q *Quantity) UnmarshalJSON(value []byte) error {
	l := len(value)
	if l == 4 && bytes.Equal(value, []byte("null")) {
		q.d.Dec = nil
		q.i = int64Amount{}
		return nil
	}
	if l >= 2 && value[0] == '"' && value[l-1] == '"' {
		value = value[1 : l-1]
	}

	parsed, err := ParseQuantity(strings.TrimSpace(string(value)))
	if err != nil {
		return err
	}

	// This copy is safe because parsed will not be referred to again.
	*q = parsed
	return nil
}

func (q *Quantity) UnmarshalCBOR(value []byte) error {
	var s *string
	if err := cbor.Unmarshal(value, &s); err != nil {
		return err
	}

	if s == nil {
		q.d.Dec = nil
		q.i = int64Amount{}
		return nil
	}

	parsed, err := ParseQuantity(strings.TrimSpace(*s))
	if err != nil {
		return err
	}

	*q = parsed
	return nil
}

// NewDecimalQuantity returns a new Quantity representing the given
// value in the given format.
func NewDecimalQuantity(b inf.Dec, format Format) *Quantity {
	return &Quantity{
		d:      infDecAmount{&b},
		Format: format,
	}
}

// NewQuantity returns a new Quantity representing the given
// value in the given format.
func NewQuantity(value int64, format Format) *Quantity {
	return &Quantity{
		i:      int64Amount{value: value},
		Format: format,
	}
}

// NewMilliQuantity returns a new Quantity representing the given
// value * 1/1000 in the given format. Note that BinarySI formatting
// will round fractional values, and will be changed to DecimalSI for
// values x where (-1 < x < 1) && (x != 0).
func NewMilliQuantity(value int64, format Format) *Quantity {
	return &Quantity{
		i:      int64Amount{value: value, scale: -3},
		Format: format,
	}
}

// NewScaledQuantity returns a new Quantity representing the given
// value * 10^scale in DecimalSI format.
func NewScaledQuantity(value int64, scale Scale) *Quantity {
	return &Quantity{
		i:      int64Amount{value: value, scale: scale},
		Format: DecimalSI,
	}
}

// Value returns the unscaled value of q rounded up to the nearest integer away from 0.
func (q *Quantity) Value() int64 {
	return q.ScaledValue(0)
}

// MilliValue returns the value of ceil(q * 1000); this could overflow an int64;
// if that's a concern, call Value() first to verify the number is small enough.
//
// Deprecated: Prefer CheckedMilliValue which will error on overflow instead of
// silently overflowing.
func (q *Quantity) MilliValue() int64 {
	return q.ScaledValue(Milli)
}

// CheckedMilliValue returns the value of ceil(q * 1000);
// This may not fit in an int64 this could overflow an int64, in which case an error will be returned.
func (q *Quantity) CheckedMilliValue() (int64, error) {
	// TODO: check underflow?
	if q.Value() > MaxMilliValue {
		return 0, fmt.Errorf("value %v overflows MilliValue, cannot convert", q.Value())
	}
	return q.ScaledValue(Milli), nil
}

// ScaledValue returns the value of ceil(q / 10^scale).
// For example, NewQuantity(1, DecimalSI).ScaledValue(Milli) returns 1000.
// This could overflow an int64.
// To detect overflow, call Value() first and verify the expected magnitude.
func (q *Quantity) ScaledValue(scale Scale) int64 {
	if q.d.Dec == nil {
		i, _ := q.i.AsScaledInt64(scale)
		return i
	}
	dec := q.d.Dec
	return scaledValue(dec.UnscaledBig(), int(dec.Scale()), int(scale.infScale()))
}

// Set sets q's value to be value.
func (q *Quantity) Set(value int64) {
	q.SetScaled(value, 0)
}

// SetMilli sets q's value to be value * 1/1000.
func (q *Quantity) SetMilli(value int64) {
	q.SetScaled(value, Milli)
}

// SetScaled sets q's value to be value * 10^scale
func (q *Quantity) SetScaled(value int64, scale Scale) {
	q.s = ""
	q.d.Dec = nil
	q.i = int64Amount{value: value, scale: scale}
}

// QuantityValue makes it possible to use a Quantity as value for a command
// line parameter.
//
// +protobuf=true
// +protobuf.embed=string
// +protobuf.options.marshal=false
// +protobuf.options.(gogoproto.goproto_stringer)=false
// +k8s:deepcopy-gen=true
type QuantityValue struct {
	Quantity
}

// Set implements pflag.Value.Set and Go flag.Value.Set.
func (q *QuantityValue) Set(s string) error {
	quantity, err := ParseQuantity(s)
	if err != nil {
		return err
	}
	q.Quantity = quantity
	return nil
}

// Type implements pflag.Value.Type.
func (q QuantityValue) Type() string {
	return "quantity"
}
