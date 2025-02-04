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

package cel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"

	"github.com/blang/semver/v4"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/ext"

	resourceapi "k8s.io/api/resource/v1beta1"
	"k8s.io/apimachinery/pkg/util/version"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/apiserver/pkg/cel/library"
	"k8s.io/utils/ptr"
)

const (
	deviceVar     = "device"
	driverVar     = "driver"
	attributesVar = "attributes"
	capacityVar   = "capacity"
)

var (
	lazyCompilerInit sync.Once
	lazyCompiler     *compiler

	// A variant of AnyType = https://github.com/kubernetes/kubernetes/blob/ec2e0de35a298363872897e5904501b029817af3/staging/src/k8s.io/apiserver/pkg/cel/types.go#L550:
	// unknown actual type (could be bool, int, string, etc.) but with a known maximum size.
	attributeType = withMaxElements(apiservercel.AnyType, resourceapi.DeviceAttributeMaxValueLength)

	// Other strings also have a known maximum size.
	domainType = withMaxElements(apiservercel.StringType, resourceapi.DeviceMaxDomainLength)
	idType     = withMaxElements(apiservercel.StringType, resourceapi.DeviceMaxIDLength)
	driverType = withMaxElements(apiservercel.StringType, resourceapi.DriverNameMaxLength)

	// Each map is bound by the maximum number of different attributes.
	innerAttributesMapType = apiservercel.NewMapType(idType, attributeType, resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice)
	outerAttributesMapType = apiservercel.NewMapType(domainType, innerAttributesMapType, resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice)

	// Same for capacity.
	innerCapacityMapType = apiservercel.NewMapType(idType, apiservercel.QuantityDeclType, resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice)
	outerCapacityMapType = apiservercel.NewMapType(domainType, innerCapacityMapType, resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice)
)

func GetCompiler() *compiler {
	lazyCompilerInit.Do(func() {
		lazyCompiler = newCompiler()
	})
	return lazyCompiler
}

// CompilationResult represents a compiled expression.
type CompilationResult struct {
	Program     cel.Program
	Error       *apiservercel.Error
	Expression  string
	OutputType  *cel.Type
	Environment *cel.Env

	// MaxCost represents the worst-case cost of the compiled MessageExpression in terms of CEL's cost units,
	// as used by cel.EstimateCost.
	MaxCost uint64

	emptyMapVal ref.Val
}

// Device defines the input values for a CEL selector expression.
type Device struct {
	// Driver gets used as domain for any attribute which does not already
	// have a domain prefix. If set, then it is also made available as a
	// string attribute.
	Driver     string
	Attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	Capacity   map[resourceapi.QualifiedName]resourceapi.DeviceCapacity
}

type compiler struct {
	// deviceType is a definition for the type of the `device` variable.
	// This is needed for the cost estimator. Both are currently version-independent.
	// If that ever changes, some additional logic might be needed to make
	// cost estimates version-dependent.
	deviceType *apiservercel.DeclType
	envset     *environment.EnvSet
}

// Options contains several additional parameters
// for [CompileCELExpression]. All of them have reasonable
// defaults.
type Options struct {
	// EnvType allows to override the default environment type [environment.StoredExpressions].
	EnvType *environment.Type

	// CostLimit allows overriding the default runtime cost limit [resourceapi.CELSelectorExpressionMaxCost].
	CostLimit *uint64

	// DisableCostEstimation can be set to skip estimating the worst-case CEL cost.
	// If disabled or after an error, [CompilationResult.MaxCost] will be set to [math.Uint64].
	DisableCostEstimation bool
}

// CompileCELExpression returns a compiled CEL expression. It evaluates to bool.
//
// TODO (https://github.com/kubernetes/kubernetes/issues/125826): validate AST to detect invalid attribute names.
func (c compiler) CompileCELExpression(expression string, options Options) CompilationResult {
	resultError := func(errorString string, errType apiservercel.ErrorType) CompilationResult {
		return CompilationResult{
			Error: &apiservercel.Error{
				Type:   errType,
				Detail: errorString,
			},
			Expression: expression,
			MaxCost:    math.MaxUint64,
		}
	}

	env, err := c.envset.Env(ptr.Deref(options.EnvType, environment.StoredExpressions))
	if err != nil {
		return resultError(fmt.Sprintf("unexpected error loading CEL environment: %v", err), apiservercel.ErrorTypeInternal)
	}

	ast, issues := env.Compile(expression)
	if issues != nil {
		return resultError("compilation failed: "+issues.String(), apiservercel.ErrorTypeInvalid)
	}
	expectedReturnType := cel.BoolType
	if ast.OutputType() != expectedReturnType &&
		ast.OutputType() != cel.AnyType {
		return resultError(fmt.Sprintf("must evaluate to %v or the unknown type, not %v", expectedReturnType.String(), ast.OutputType().String()), apiservercel.ErrorTypeInvalid)
	}
	_, err = cel.AstToCheckedExpr(ast)
	if err != nil {
		// should be impossible since env.Compile returned no issues
		return resultError("unexpected compilation error: "+err.Error(), apiservercel.ErrorTypeInternal)
	}
	prog, err := env.Program(ast,
		// The Kubernetes CEL base environment sets the VAP limit as runtime cost limit.
		// DRA has its own default cost limit and also allows the caller to change that
		// limit.
		cel.CostLimit(ptr.Deref(options.CostLimit, resourceapi.CELSelectorExpressionMaxCost)),
		cel.InterruptCheckFrequency(celconfig.CheckFrequency),
	)
	if err != nil {
		return resultError("program instantiation failed: "+err.Error(), apiservercel.ErrorTypeInternal)
	}

	compilationResult := CompilationResult{
		Program:     prog,
		Expression:  expression,
		OutputType:  ast.OutputType(),
		Environment: env,
		emptyMapVal: env.CELTypeAdapter().NativeToValue(map[string]any{}),
		MaxCost:     math.MaxUint64,
	}

	if !options.DisableCostEstimation {
		// We don't have a SizeEstimator. The potential size of the input (= a
		// device) is already declared in the definition of the environment.
		estimator := c.newCostEstimator()
		costEst, err := env.EstimateCost(ast, estimator)
		if err != nil {
			compilationResult.Error = &apiservercel.Error{Type: apiservercel.ErrorTypeInternal, Detail: "cost estimation failed: " + err.Error()}
			return compilationResult
		}
		compilationResult.MaxCost = costEst.Max
	}

	return compilationResult
}

func (c *compiler) newCostEstimator() *library.CostEstimator {
	return &library.CostEstimator{SizeEstimator: &sizeEstimator{compiler: c}}
}

// getAttributeValue returns the native representation of the one value that
// should be stored in the attribute, otherwise an error. An error is
// also returned when there is no supported value.
func getAttributeValue(attr resourceapi.DeviceAttribute) (any, error) {
	switch {
	case attr.IntValue != nil:
		return *attr.IntValue, nil
	case attr.BoolValue != nil:
		return *attr.BoolValue, nil
	case attr.StringValue != nil:
		return *attr.StringValue, nil
	case attr.VersionValue != nil:
		v, err := semver.Parse(*attr.VersionValue)
		if err != nil {
			return nil, fmt.Errorf("parse semantic version: %w", err)
		}
		return apiservercel.Semver{Version: v}, nil
	default:
		return nil, errors.New("unsupported attribute value")
	}
}

var boolType = reflect.TypeOf(true)

func (c CompilationResult) DeviceMatches(ctx context.Context, input Device) (bool, *cel.EvalDetails, error) {
	// TODO (future): avoid building these maps and instead use a proxy
	// which wraps the underlying maps and directly looks up values.
	attributes := make(map[string]any)
	for name, attr := range input.Attributes {
		value, err := getAttributeValue(attr)
		if err != nil {
			return false, nil, fmt.Errorf("attribute %s: %w", name, err)
		}
		domain, id := parseQualifiedName(name, input.Driver)
		if attributes[domain] == nil {
			attributes[domain] = make(map[string]any)
		}
		attributes[domain].(map[string]any)[id] = value
	}

	capacity := make(map[string]any)
	for name, cap := range input.Capacity {
		domain, id := parseQualifiedName(name, input.Driver)
		if capacity[domain] == nil {
			capacity[domain] = make(map[string]apiservercel.Quantity)
		}
		capacity[domain].(map[string]apiservercel.Quantity)[id] = apiservercel.Quantity{Quantity: &cap.Value}
	}

	variables := map[string]any{
		deviceVar: map[string]any{
			driverVar:     input.Driver,
			attributesVar: newStringInterfaceMapWithDefault(c.Environment.CELTypeAdapter(), attributes, c.emptyMapVal),
			capacityVar:   newStringInterfaceMapWithDefault(c.Environment.CELTypeAdapter(), capacity, c.emptyMapVal),
		},
	}

	result, details, err := c.Program.ContextEval(ctx, variables)
	if err != nil {
		return false, details, err
	}
	resultAny, err := result.ConvertToNative(boolType)
	if err != nil {
		return false, details, fmt.Errorf("CEL result of type %s could not be converted to bool: %w", result.Type().TypeName(), err)
	}
	resultBool, ok := resultAny.(bool)
	if !ok {
		return false, details, fmt.Errorf("CEL native result value should have been a bool, got instead: %T", resultAny)
	}
	return resultBool, details, nil
}

func newCompiler() *compiler {
	envset := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), true /* strictCost */)
	field := func(name string, declType *apiservercel.DeclType, required bool) *apiservercel.DeclField {
		return apiservercel.NewDeclField(name, declType, required, nil, nil)
	}
	fields := func(fields ...*apiservercel.DeclField) map[string]*apiservercel.DeclField {
		result := make(map[string]*apiservercel.DeclField, len(fields))
		for _, f := range fields {
			result[f.Name] = f
		}
		return result
	}

	deviceType := apiservercel.NewObjectType("kubernetes.DRADevice", fields(
		field(driverVar, driverType, true),
		field(attributesVar, outerAttributesMapType, true),
		field(capacityVar, outerCapacityMapType, true),
	))

	versioned := []environment.VersionedOptions{
		{
			IntroducedVersion: version.MajorMinor(1, 31),
			EnvOptions: []cel.EnvOption{
				cel.Variable(deviceVar, deviceType.CelType()),

				environment.UnversionedLib(library.SemverLib),

				// https://pkg.go.dev/github.com/google/cel-go/ext#Bindings
				//
				// This is useful to simplify attribute lookups because the
				// domain only needs to be given once:
				//
				//    cel.bind(dra, device.attributes["dra.example.com"], dra.oneBool && dra.anotherBool)
				ext.Bindings(ext.BindingsVersion(0)),
			},
			DeclTypes: []*apiservercel.DeclType{
				deviceType,
			},
		},
	}
	envset, err := envset.Extend(versioned...)
	if err != nil {
		panic(fmt.Errorf("internal error building CEL environment: %w", err))
	}
	return &compiler{envset: envset, deviceType: deviceType}
}

func withMaxElements(in *apiservercel.DeclType, maxElements uint64) *apiservercel.DeclType {
	out := *in
	out.MaxElements = int64(maxElements)
	return &out
}

// parseQualifiedName splits into domain and identified, using the default domain
// if the name does not contain one.
func parseQualifiedName(name resourceapi.QualifiedName, defaultDomain string) (string, string) {
	sep := strings.Index(string(name), "/")
	if sep == -1 {
		return defaultDomain, string(name)
	}
	return string(name[0:sep]), string(name[sep+1:])
}

// newStringInterfaceMapWithDefault is like
// https://pkg.go.dev/github.com/google/cel-go@v0.20.1/common/types#NewStringInterfaceMap,
// except that looking up an unknown key returns a default value.
func newStringInterfaceMapWithDefault(adapter types.Adapter, value map[string]any, defaultValue ref.Val) traits.Mapper {
	return mapper{
		Mapper:       types.NewStringInterfaceMap(adapter, value),
		defaultValue: defaultValue,
	}
}

type mapper struct {
	traits.Mapper
	defaultValue ref.Val
}

// Find wraps the mapper's Find so that a default empty map is returned when
// the lookup did not find the entry.
func (m mapper) Find(key ref.Val) (ref.Val, bool) {
	value, found := m.Mapper.Find(key)
	if found {
		return value, true
	}

	return m.defaultValue, true
}

// sizeEstimator tells the cost estimator the maximum size of maps or strings accessible through the `device` variable.
// Without this, the maximum string size of e.g. `device.attributes["dra.example.com"].services` would be unknown.
//
// sizeEstimator is derived from the sizeEstimator in k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel.
type sizeEstimator struct {
	compiler *compiler
}

func (s *sizeEstimator) EstimateSize(element checker.AstNode) *checker.SizeEstimate {
	path := element.Path()
	if len(path) == 0 {
		// Path() can return an empty list, early exit if it does since we can't
		// provide size estimates when that happens
		return nil
	}

	// The estimator provides information about the environment's variable(s).
	var currentNode *apiservercel.DeclType
	switch path[0] {
	case deviceVar:
		currentNode = s.compiler.deviceType
	default:
		// Unknown root, shouldn't happen.
		return nil
	}

	// Cut off initial variable from path, it was checked above.
	for _, name := range path[1:] {
		switch name {
		case "@items", "@values":
			if currentNode.ElemType == nil {
				return nil
			}
			currentNode = currentNode.ElemType
		case "@keys":
			if currentNode.KeyType == nil {
				return nil
			}
			currentNode = currentNode.KeyType
		default:
			field, ok := currentNode.Fields[name]
			if !ok {
				// If this is an attribute map, then we know that all elements
				// have the same maximum size as set in attributeType, regardless
				// of their name.
				if currentNode.ElemType == attributeType {
					currentNode = attributeType
					continue
				}
				return nil
			}
			if field.Type == nil {
				return nil
			}
			currentNode = field.Type
		}
	}
	return &checker.SizeEstimate{Min: 0, Max: uint64(currentNode.MaxElements)}
}

func (s *sizeEstimator) EstimateCallCost(function, overloadID string, target *checker.AstNode, args []checker.AstNode) *checker.CallEstimate {
	return nil
}
