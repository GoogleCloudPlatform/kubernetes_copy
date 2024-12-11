package runtime

import (
	"google.golang.org/protobuf/proto"
)

// StringP returns a pointer to a string whose pointee is same as the given string value.
func StringP(val string) (*string, error) {
	return proto.String(val), nil
}

// BoolP parses the given string representation of a boolean value,
// and returns a pointer to a bool whose value is same as the parsed value.
func BoolP(val string) (*bool, error) {
	b, err := Bool(val)
	if err != nil {
		return nil, err
	}
	return proto.Bool(b), nil
}

// Float64P parses the given string representation of a floating point number,
// and returns a pointer to a float64 whose value is same as the parsed number.
func Float64P(val string) (*float64, error) {
	f, err := Float64(val)
	if err != nil {
		return nil, err
	}
	return proto.Float64(f), nil
}

// Float32P parses the given string representation of a floating point number,
// and returns a pointer to a float32 whose value is same as the parsed number.
func Float32P(val string) (*float32, error) {
	f, err := Float32(val)
	if err != nil {
		return nil, err
	}
	return proto.Float32(f), nil
}

// Int64P parses the given string representation of an integer
// and returns a pointer to an int64 whose value is same as the parsed integer.
func Int64P(val string) (*int64, error) {
	i, err := Int64(val)
	if err != nil {
		return nil, err
	}
	return proto.Int64(i), nil
}

// Int32P parses the given string representation of an integer
// and returns a pointer to an int32 whose value is same as the parsed integer.
func Int32P(val string) (*int32, error) {
	i, err := Int32(val)
	if err != nil {
		return nil, err
	}
	return proto.Int32(i), err
}

// Uint64P parses the given string representation of an integer
// and returns a pointer to a uint64 whose value is same as the parsed integer.
func Uint64P(val string) (*uint64, error) {
	i, err := Uint64(val)
	if err != nil {
		return nil, err
	}
	return proto.Uint64(i), err
}

// Uint32P parses the given string representation of an integer
// and returns a pointer to a uint32 whose value is same as the parsed integer.
func Uint32P(val string) (*uint32, error) {
	i, err := Uint32(val)
	if err != nil {
		return nil, err
	}
	return proto.Uint32(i), err
}
