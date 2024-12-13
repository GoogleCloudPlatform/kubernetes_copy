package runtime

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/utilities"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/durationpb"
	field_mask "google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var valuesKeyRegexp = regexp.MustCompile(`^(.*)\[(.*)\]$`)

var currentQueryParser QueryParameterParser = &DefaultQueryParser{}

// QueryParameterParser defines interface for all query parameter parsers
type QueryParameterParser interface {
	Parse(msg proto.Message, values url.Values, filter *utilities.DoubleArray) error
}

// PopulateQueryParameters parses query parameters
// into "msg" using current query parser
func PopulateQueryParameters(msg proto.Message, values url.Values, filter *utilities.DoubleArray) error {
	return currentQueryParser.Parse(msg, values, filter)
}

// DefaultQueryParser is a QueryParameterParser which implements the default
// query parameters parsing behavior.
//
// See https://github.com/grpc-ecosystem/grpc-gateway/issues/2632 for more context.
type DefaultQueryParser struct{}

// Parse populates "values" into "msg".
// A value is ignored if its key starts with one of the elements in "filter".
func (*DefaultQueryParser) Parse(msg proto.Message, values url.Values, filter *utilities.DoubleArray) error {
	for key, values := range values {
		if match := valuesKeyRegexp.FindStringSubmatch(key); len(match) == 3 {
			key = match[1]
			values = append([]string{match[2]}, values...)
		}

		msgValue := msg.ProtoReflect()
		fieldPath := normalizeFieldPath(msgValue, strings.Split(key, "."))
		if filter.HasCommonPrefix(fieldPath) {
			continue
		}
		if err := populateFieldValueFromPath(msgValue, fieldPath, values); err != nil {
			return err
		}
	}
	return nil
}

// PopulateFieldFromPath sets a value in a nested Protobuf structure.
func PopulateFieldFromPath(msg proto.Message, fieldPathString string, value string) error {
	fieldPath := strings.Split(fieldPathString, ".")
	return populateFieldValueFromPath(msg.ProtoReflect(), fieldPath, []string{value})
}

func normalizeFieldPath(msgValue protoreflect.Message, fieldPath []string) []string {
	newFieldPath := make([]string, 0, len(fieldPath))
	for i, fieldName := range fieldPath {
		fields := msgValue.Descriptor().Fields()
		fieldDesc := fields.ByTextName(fieldName)
		if fieldDesc == nil {
			fieldDesc = fields.ByJSONName(fieldName)
		}
		if fieldDesc == nil {
			// return initial field path values if no matching  message field was found
			return fieldPath
		}

		newFieldPath = append(newFieldPath, string(fieldDesc.Name()))

		// If this is the last element, we're done
		if i == len(fieldPath)-1 {
			break
		}

		// Only singular message fields are allowed
		if fieldDesc.Message() == nil || fieldDesc.Cardinality() == protoreflect.Repeated {
			return fieldPath
		}

		// Get the nested message
		msgValue = msgValue.Get(fieldDesc).Message()
	}

	return newFieldPath
}

func populateFieldValueFromPath(msgValue protoreflect.Message, fieldPath []string, values []string) error {
	if len(fieldPath) < 1 {
		return errors.New("no field path")
	}
	if len(values) < 1 {
		return errors.New("no value provided")
	}

	var fieldDescriptor protoreflect.FieldDescriptor
	for i, fieldName := range fieldPath {
		fields := msgValue.Descriptor().Fields()

		// Get field by name
		fieldDescriptor = fields.ByName(protoreflect.Name(fieldName))
		if fieldDescriptor == nil {
			fieldDescriptor = fields.ByJSONName(fieldName)
			if fieldDescriptor == nil {
				// We're not returning an error here because this could just be
				// an extra query parameter that isn't part of the request.
				grpclog.Infof("field not found in %q: %q", msgValue.Descriptor().FullName(), strings.Join(fieldPath, "."))
				return nil
			}
		}

		// If this is the last element, we're done
		if i == len(fieldPath)-1 {
			break
		}

		// Only singular message fields are allowed
		if fieldDescriptor.Message() == nil || fieldDescriptor.Cardinality() == protoreflect.Repeated {
			return fmt.Errorf("invalid path: %q is not a message", fieldName)
		}

		// Get the nested message
		msgValue = msgValue.Mutable(fieldDescriptor).Message()
	}

	// Check if oneof already set
	if of := fieldDescriptor.ContainingOneof(); of != nil {
		if f := msgValue.WhichOneof(of); f != nil {
			return fmt.Errorf("field already set for oneof %q", of.FullName().Name())
		}
	}

	switch {
	case fieldDescriptor.IsList():
		return populateRepeatedField(fieldDescriptor, msgValue.Mutable(fieldDescriptor).List(), values)
	case fieldDescriptor.IsMap():
		return populateMapField(fieldDescriptor, msgValue.Mutable(fieldDescriptor).Map(), values)
	}

	if len(values) > 1 {
		return fmt.Errorf("too many values for field %q: %s", fieldDescriptor.FullName().Name(), strings.Join(values, ", "))
	}

	return populateField(fieldDescriptor, msgValue, values[0])
}

func populateField(fieldDescriptor protoreflect.FieldDescriptor, msgValue protoreflect.Message, value string) error {
	v, err := parseField(fieldDescriptor, value)
	if err != nil {
		return fmt.Errorf("parsing field %q: %w", fieldDescriptor.FullName().Name(), err)
	}

	msgValue.Set(fieldDescriptor, v)
	return nil
}

func populateRepeatedField(fieldDescriptor protoreflect.FieldDescriptor, list protoreflect.List, values []string) error {
	for _, value := range values {
		v, err := parseField(fieldDescriptor, value)
		if err != nil {
			return fmt.Errorf("parsing list %q: %w", fieldDescriptor.FullName().Name(), err)
		}
		list.Append(v)
	}

	return nil
}

func populateMapField(fieldDescriptor protoreflect.FieldDescriptor, mp protoreflect.Map, values []string) error {
	if len(values) != 2 {
		return fmt.Errorf("more than one value provided for key %q in map %q", values[0], fieldDescriptor.FullName())
	}

	key, err := parseField(fieldDescriptor.MapKey(), values[0])
	if err != nil {
		return fmt.Errorf("parsing map key %q: %w", fieldDescriptor.FullName().Name(), err)
	}

	value, err := parseField(fieldDescriptor.MapValue(), values[1])
	if err != nil {
		return fmt.Errorf("parsing map value %q: %w", fieldDescriptor.FullName().Name(), err)
	}

	mp.Set(key.MapKey(), value)

	return nil
}

func parseField(fieldDescriptor protoreflect.FieldDescriptor, value string) (protoreflect.Value, error) {
	switch fieldDescriptor.Kind() {
	case protoreflect.BoolKind:
		v, err := strconv.ParseBool(value)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfBool(v), nil
	case protoreflect.EnumKind:
		enum, err := protoregistry.GlobalTypes.FindEnumByName(fieldDescriptor.Enum().FullName())
		if err != nil {
			if errors.Is(err, protoregistry.NotFound) {
				return protoreflect.Value{}, fmt.Errorf("enum %q is not registered", fieldDescriptor.Enum().FullName())
			}
			return protoreflect.Value{}, fmt.Errorf("failed to look up enum: %w", err)
		}
		// Look for enum by name
		v := enum.Descriptor().Values().ByName(protoreflect.Name(value))
		if v == nil {
			i, err := strconv.Atoi(value)
			if err != nil {
				return protoreflect.Value{}, fmt.Errorf("%q is not a valid value", value)
			}
			// Look for enum by number
			if v = enum.Descriptor().Values().ByNumber(protoreflect.EnumNumber(i)); v == nil {
				return protoreflect.Value{}, fmt.Errorf("%q is not a valid value", value)
			}
		}
		return protoreflect.ValueOfEnum(v.Number()), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		v, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfInt32(int32(v)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfInt64(v), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		v, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfUint32(uint32(v)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfUint64(v), nil
	case protoreflect.FloatKind:
		v, err := strconv.ParseFloat(value, 32)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfFloat32(float32(v)), nil
	case protoreflect.DoubleKind:
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfFloat64(v), nil
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(value), nil
	case protoreflect.BytesKind:
		v, err := Bytes(value)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfBytes(v), nil
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return parseMessage(fieldDescriptor.Message(), value)
	default:
		panic(fmt.Sprintf("unknown field kind: %v", fieldDescriptor.Kind()))
	}
}

func parseMessage(msgDescriptor protoreflect.MessageDescriptor, value string) (protoreflect.Value, error) {
	var msg proto.Message
	switch msgDescriptor.FullName() {
	case "google.protobuf.Timestamp":
		t, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return protoreflect.Value{}, err
		}
		timestamp := timestamppb.New(t)
		if ok := timestamp.IsValid(); !ok {
			return protoreflect.Value{}, fmt.Errorf("%s before 0001-01-01", value)
		}
		msg = timestamp
	case "google.protobuf.Duration":
		d, err := time.ParseDuration(value)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg = durationpb.New(d)
	case "google.protobuf.DoubleValue":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg = wrapperspb.Double(v)
	case "google.protobuf.FloatValue":
		v, err := strconv.ParseFloat(value, 32)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg = wrapperspb.Float(float32(v))
	case "google.protobuf.Int64Value":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg = wrapperspb.Int64(v)
	case "google.protobuf.Int32Value":
		v, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg = wrapperspb.Int32(int32(v))
	case "google.protobuf.UInt64Value":
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg = wrapperspb.UInt64(v)
	case "google.protobuf.UInt32Value":
		v, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg = wrapperspb.UInt32(uint32(v))
	case "google.protobuf.BoolValue":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg = wrapperspb.Bool(v)
	case "google.protobuf.StringValue":
		msg = wrapperspb.String(value)
	case "google.protobuf.BytesValue":
		v, err := Bytes(value)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg = wrapperspb.Bytes(v)
	case "google.protobuf.FieldMask":
		fm := &field_mask.FieldMask{}
		fm.Paths = append(fm.Paths, strings.Split(value, ",")...)
		msg = fm
	case "google.protobuf.Value":
		var v structpb.Value
		if err := protojson.Unmarshal([]byte(value), &v); err != nil {
			return protoreflect.Value{}, err
		}
		msg = &v
	case "google.protobuf.Struct":
		var v structpb.Struct
		if err := protojson.Unmarshal([]byte(value), &v); err != nil {
			return protoreflect.Value{}, err
		}
		msg = &v
	default:
		return protoreflect.Value{}, fmt.Errorf("unsupported message type: %q", string(msgDescriptor.FullName()))
	}

	return protoreflect.ValueOfMessage(msg.ProtoReflect()), nil
}
