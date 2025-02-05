/*
Copyright 2021 The Kubernetes Authors.

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

package v1

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-cmp/cmp" //nolint:depguard
	"github.com/spf13/pflag"

	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/version"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/featuregate"
	"k8s.io/component-base/logs/internal/setverbositylevel"
	"k8s.io/component-base/logs/klogflags"
	baseversion "k8s.io/component-base/version"
)

const (
	// LogFlushFreqDefault is the default for the corresponding command line
	// parameter.
	LogFlushFreqDefault = 5 * time.Second
)

const (
	// LogFlushFreqFlagName is the name of the command line parameter.
	// Depending on how flags get added, it is either a stand-alone
	// value (logs.AddFlags) or part of LoggingConfiguration.
	LogFlushFreqFlagName = "log-flush-frequency"
)

// NewLoggingConfiguration returns a struct holding the default logging configuration.
func NewLoggingConfiguration() *LoggingConfiguration {
	c := LoggingConfiguration{}
	SetRecommendedLoggingConfiguration(&c)
	return &c
}

// Applying configurations multiple times is not safe unless it's guaranteed that there
// are no goroutines which might call logging functions. The default for ValidateAndApply
// and ValidateAndApplyWithOptions is to return an error when called more than once.
// Binaries and unit tests can override that behavior.
var ReapplyHandling = ReapplyHandlingError

type ReapplyHandlingType int

const (
	// ReapplyHandlingError is the default: calling ValidateAndApply or
	// ValidateAndApplyWithOptions again returns an error.
	ReapplyHandlingError ReapplyHandlingType = iota
	// ReapplyHandlingIgnoreUnchanged silently ignores any additional calls of
	// ValidateAndApply or ValidateAndApplyWithOptions if the configuration
	// is unchanged, otherwise they return an error.
	ReapplyHandlingIgnoreUnchanged
)

// ValidateAndApply combines validation and application of the logging configuration.
// This should be invoked as early as possible because then the rest of the program
// startup (including validation of other options) will already run with the final
// logging configuration.
//
// The optional FeatureGate controls logging features. If nil, the default for
// these features is used.
//
// Logging options must be applied as early as possible during the program
// startup. Some changes are global and cannot be done safely when there are
// already goroutines running.
func ValidateAndApply(c *LoggingConfiguration, featureGate featuregate.FeatureGate) error {
	return validateAndApply(c, nil, featureGate, nil)
}

// ValidateAndApplyWithOptions is a variant of ValidateAndApply which accepts
// additional options beyond those that can be configured through the API. This
// is meant for testing.
//
// Logging options must be applied as early as possible during the program
// startup. Some changes are global and cannot be done safely when there are
// already goroutines running.
func ValidateAndApplyWithOptions(c *LoggingConfiguration, options *LoggingOptions, featureGate featuregate.FeatureGate) error {
	return validateAndApply(c, options, featureGate, nil)
}

// +k8s:deepcopy-gen=false

// LoggingOptions can be used with ValidateAndApplyWithOptions to override
// certain global defaults.
type LoggingOptions struct {
	// ErrorStream can be used to override the os.Stderr default.
	ErrorStream io.Writer

	// InfoStream can be used to override the os.Stdout default.
	InfoStream io.Writer
}

// ValidateAndApplyAsField is a variant of ValidateAndApply that should be used
// when the LoggingConfiguration is embedded in some larger configuration
// structure.
func ValidateAndApplyAsField(c *LoggingConfiguration, featureGate featuregate.FeatureGate, fldPath *field.Path) error {
	return validateAndApply(c, nil, featureGate, fldPath)
}

func validateAndApply(c *LoggingConfiguration, options *LoggingOptions, featureGate featuregate.FeatureGate, fldPath *field.Path) error {
	errs := Validate(c, featureGate, fldPath)
	if len(errs) > 0 {
		return errs.ToAggregate()
	}
	return apply(c, options, featureGate)
}

// Validate can be used to check for invalid settings without applying them.
// Most binaries should validate and apply the logging configuration as soon
// as possible via ValidateAndApply. The field path is optional: nil
// can be passed when the struct is not embedded in some larger struct.
func Validate(c *LoggingConfiguration, featureGate featuregate.FeatureGate, fldPath *field.Path) field.ErrorList {
	errs := field.ErrorList{}
	if c.Format != DefaultLogFormat {
		// WordSepNormalizeFunc is just a guess. Commands should use it,
		// but we cannot know for sure.
		allFlags := unsupportedLoggingFlags(cliflag.WordSepNormalizeFunc)
		for _, f := range allFlags {
			if f.DefValue != f.Value.String() {
				errs = append(errs, field.Invalid(fldPath.Child("format"), c.Format, fmt.Sprintf("Non-default format doesn't honor flag: %s", f.Name)))
			}
		}
	}
	format, err := logRegistry.get(c.Format)
	if err != nil {
		errs = append(errs, field.Invalid(fldPath.Child("format"), c.Format, "Unsupported log format"))
	} else if format != nil {
		emulatedVersion := baseversion.DefaultBuildEffectiveVersion().BinaryVersion()
		if featureGate != nil {
			if fg, ok := featureGate.(featuregate.MutableVersionedFeatureGate); ok {
				emulatedVersion = fg.EmulationVersion()
			}
		}
		formatFeature := featuregate.FeatureForVersion(emulatedVersion, format.versionedFeatures, LoggingAlphaOptions)
		if formatFeature != LoggingStableOptions {
			enabled := true
			if featureGate != nil {
				enabled = featureGate.Enabled(formatFeature)
			}
			if !enabled {
				errs = append(errs, field.Forbidden(fldPath.Child("format"), fmt.Sprintf("Log format %s is disabled, see %s feature", c.Format, formatFeature)))
			}
		}
	}

	// The type in our struct is uint32, but klog only accepts positive int32.
	if c.Verbosity > math.MaxInt32 {
		errs = append(errs, field.Invalid(fldPath.Child("verbosity"), c.Verbosity, fmt.Sprintf("Must be <= %d", math.MaxInt32)))
	}
	vmoduleFldPath := fldPath.Child("vmodule")
	if len(c.VModule) > 0 && c.Format != "" && c.Format != "text" {
		errs = append(errs, field.Forbidden(vmoduleFldPath, "Only supported for text log format"))
	}
	for i, item := range c.VModule {
		if item.FilePattern == "" {
			errs = append(errs, field.Required(vmoduleFldPath.Index(i), "File pattern must not be empty"))
		}
		if strings.ContainsAny(item.FilePattern, "=,") {
			errs = append(errs, field.Invalid(vmoduleFldPath.Index(i), item.FilePattern, "File pattern must not contain equal sign or comma"))
		}
		if item.Verbosity > math.MaxInt32 {
			errs = append(errs, field.Invalid(vmoduleFldPath.Index(i), item.Verbosity, fmt.Sprintf("Must be <= %d", math.MaxInt32)))
		}
	}

	errs = append(errs, validateFormatOptions(c, featureGate, fldPath.Child("options"))...)
	return errs
}

func validateFormatOptions(c *LoggingConfiguration, featureGate featuregate.FeatureGate, fldPath *field.Path) field.ErrorList {
	emulatedVersion := baseversion.DefaultBuildEffectiveVersion().BinaryVersion()
	if featureGate != nil {
		if fg, ok := featureGate.(featuregate.MutableVersionedFeatureGate); ok {
			emulatedVersion = fg.EmulationVersion()
		}
	}
	errs := field.ErrorList{}
	errs = append(errs, validateTextOptions(c, featureGate, fldPath.Child("text"), emulatedVersion)...)
	errs = append(errs, validateJSONOptions(c, featureGate, fldPath.Child("json"), emulatedVersion)...)
	return errs
}

func validateTextOptions(c *LoggingConfiguration, featureGate featuregate.FeatureGate, fldPath *field.Path, emulatedVersion *version.Version) field.ErrorList {
	errs := field.ErrorList{}
	textSplitStreamVersionedFeatures := []featuregate.VersionedFeature{
		{Version: version.MajorMinor(1, 23), Feature: LoggingAlphaOptions},
		// Uncomment the following line to promote the feature to Beta.
		// {Version: version.MajorMinor(1, 33), Feature: LoggingBetaOptions},
		// Uncomment the following line to promote the feature to GA.
		// {Version: version.MajorMinor(1, 33), Feature: LoggingStableOptions},
	}
	if gate := featuregate.FeatureForVersion(emulatedVersion, textSplitStreamVersionedFeatures, LoggingAlphaOptions); c.Options.Text.SplitStream && !featureEnabled(featureGate, gate) {
		errs = append(errs, field.Forbidden(fldPath.Child("splitStream"), fmt.Sprintf("Feature %s is disabled", gate)))
	}

	textInfoBufferSizeVersionedFeatures := []featuregate.VersionedFeature{
		{Version: version.MajorMinor(1, 23), Feature: LoggingAlphaOptions},
		// Uncomment the following line to promote the feature to Beta.
		// {Version: version.MajorMinor(1, 33), Feature: LoggingBetaOptions},
		// Uncomment the following line to promote the feature to GA.
		// {Version: version.MajorMinor(1, 33), Feature: LoggingStableOptions},
	}
	if gate := featuregate.FeatureForVersion(emulatedVersion, textInfoBufferSizeVersionedFeatures, LoggingAlphaOptions); c.Options.Text.InfoBufferSize.Value() != 0 && !featureEnabled(featureGate, gate) {
		errs = append(errs, field.Forbidden(fldPath.Child("infoBufferSize"), fmt.Sprintf("Feature %s is disabled", gate)))
	}
	return errs
}

func validateJSONOptions(c *LoggingConfiguration, featureGate featuregate.FeatureGate, fldPath *field.Path, emulatedVersion *version.Version) field.ErrorList {
	errs := field.ErrorList{}
	jsonSplitStreamVersionedFeatures := []featuregate.VersionedFeature{
		{Version: version.MajorMinor(1, 23), Feature: LoggingAlphaOptions},
		// Uncomment the following line to promote the feature to Beta.
		// {Version: version.MajorMinor(1, 33), Feature: LoggingBetaOptions},
		// Uncomment the following line to promote the feature to GA.
		// {Version: version.MajorMinor(1, 33), Feature: LoggingStableOptions},
	}
	if gate := featuregate.FeatureForVersion(emulatedVersion, jsonSplitStreamVersionedFeatures, LoggingAlphaOptions); c.Options.JSON.SplitStream && !featureEnabled(featureGate, gate) {
		errs = append(errs, field.Forbidden(fldPath.Child("splitStream"), fmt.Sprintf("Feature %s is disabled", gate)))
	}

	jsonInfoBufferSizeVersionedFeatures := []featuregate.VersionedFeature{
		{Version: version.MajorMinor(1, 23), Feature: LoggingAlphaOptions},
		// Uncomment the following line to promote the feature to Beta.
		// {Version: version.MajorMinor(1, 33), Feature: LoggingBetaOptions},
		// Uncomment the following line to promote the feature to GA.
		// {Version: version.MajorMinor(1, 33), Feature: LoggingStableOptions},
	}
	if gate := featuregate.FeatureForVersion(emulatedVersion, jsonInfoBufferSizeVersionedFeatures, LoggingAlphaOptions); c.Options.JSON.InfoBufferSize.Value() != 0 && !featureEnabled(featureGate, gate) {
		errs = append(errs, field.Forbidden(fldPath.Child("infoBufferSize"), fmt.Sprintf("Feature %s is disabled", gate)))
	}
	return errs
}

func featureEnabled(featureGate featuregate.FeatureGate, feature featuregate.Feature) bool {
	enabled := false
	if featureGate != nil {
		enabled = featureGate.Enabled(feature)
	}
	return enabled
}

func apply(c *LoggingConfiguration, options *LoggingOptions, featureGate featuregate.FeatureGate) error {
	p := &parameters{
		C:                        c,
		Options:                  options,
		ContextualLoggingEnabled: contextualLoggingDefault,
	}
	if featureGate != nil {
		p.ContextualLoggingEnabled = featureGate.Enabled(ContextualLogging)
	}

	oldP := applyParameters.Load()
	if oldP != nil {
		switch ReapplyHandling {
		case ReapplyHandlingError:
			return errors.New("logging configuration was already applied earlier, changing it is not allowed")
		case ReapplyHandlingIgnoreUnchanged:
			if diff := cmp.Diff(oldP, p); diff != "" {
				return fmt.Errorf("the logging configuration should not be changed after setting it once (- old setting, + new setting):\n%s", diff)
			}
			return nil
		default:
			return fmt.Errorf("invalid value %d for ReapplyHandling", ReapplyHandling)
		}
	}
	applyParameters.Store(p)

	// if log format not exists, use nil loggr
	format, _ := logRegistry.get(c.Format)
	if format.factory == nil {
		klog.ClearLogger()
	} else {
		if options == nil {
			options = &LoggingOptions{
				ErrorStream: os.Stderr,
				InfoStream:  os.Stdout,
			}
		}
		log, control := format.factory.Create(*c, *options)
		if control.SetVerbosityLevel != nil {
			setverbositylevel.Mutex.Lock()
			defer setverbositylevel.Mutex.Unlock()
			setverbositylevel.Callbacks = append(setverbositylevel.Callbacks, control.SetVerbosityLevel)
		}
		opts := []klog.LoggerOption{
			klog.ContextualLogger(p.ContextualLoggingEnabled),
			klog.FlushLogger(control.Flush),
		}
		if writer, ok := log.GetSink().(textlogger.KlogBufferWriter); ok {
			opts = append(opts, klog.WriteKlogBuffer(writer.WriteKlogBuffer))
		}
		klog.SetLoggerWithOptions(log, opts...)
	}
	if err := loggingFlags.Lookup("v").Value.Set(VerbosityLevelPflag(&c.Verbosity).String()); err != nil {
		return fmt.Errorf("internal error while setting klog verbosity: %v", err)
	}
	if err := loggingFlags.Lookup("vmodule").Value.Set(VModuleConfigurationPflag(&c.VModule).String()); err != nil {
		return fmt.Errorf("internal error while setting klog vmodule: %v", err)
	}
	setSlogDefaultLogger()
	klog.StartFlushDaemon(c.FlushFrequency.Duration.Duration)
	klog.EnableContextualLogging(p.ContextualLoggingEnabled)
	return nil
}

type parameters struct {
	C                        *LoggingConfiguration
	Options                  *LoggingOptions
	ContextualLoggingEnabled bool
}

var applyParameters atomic.Pointer[parameters]

// ResetForTest restores the default settings. This is not thread-safe and should only
// be used when there are no goroutines running. The intended users are unit
// tests in other packages.
func ResetForTest(featureGate featuregate.FeatureGate) error {
	oldP := applyParameters.Load()
	if oldP == nil {
		// Nothing to do.
		return nil
	}

	// This makes it possible to call apply again without triggering errors.
	applyParameters.Store(nil)

	// Restore defaults. Shouldn't fail, but check anyway.
	config := NewLoggingConfiguration()
	if err := ValidateAndApply(config, featureGate); err != nil {
		return fmt.Errorf("apply default configuration: %v", err)
	}

	// And again...
	applyParameters.Store(nil)

	return nil
}

// AddFlags adds command line flags for the configuration.
func AddFlags(c *LoggingConfiguration, fs *pflag.FlagSet) {
	addFlags(c, fs)
}

// AddGoFlags is a variant of AddFlags for a standard FlagSet.
func AddGoFlags(c *LoggingConfiguration, fs *flag.FlagSet) {
	addFlags(c, goFlagSet{FlagSet: fs})
}

// flagSet is the interface implemented by pflag.FlagSet, with
// just those methods defined which are needed by addFlags.
type flagSet interface {
	BoolVar(p *bool, name string, value bool, usage string)
	DurationVar(p *time.Duration, name string, value time.Duration, usage string)
	StringVar(p *string, name string, value string, usage string)
	Var(value pflag.Value, name string, usage string)
	VarP(value pflag.Value, name, shorthand, usage string)
}

// goFlagSet implements flagSet for a stdlib flag.FlagSet.
type goFlagSet struct {
	*flag.FlagSet
}

func (fs goFlagSet) Var(value pflag.Value, name string, usage string) {
	fs.FlagSet.Var(value, name, usage)
}

func (fs goFlagSet) VarP(value pflag.Value, name, shorthand, usage string) {
	// Ignore shorthand, it's not needed and not supported.
	fs.FlagSet.Var(value, name, usage)
}

// addFlags can be used with both flag.FlagSet and pflag.FlagSet. The internal
// interface definition avoids duplicating this code.
func addFlags(c *LoggingConfiguration, fs flagSet) {
	formats := logRegistry.list()
	fs.StringVar(&c.Format, "logging-format", c.Format, fmt.Sprintf("Sets the log format. Permitted formats: %s.", formats))
	// No new log formats should be added after generation is of flag options
	logRegistry.freeze()

	fs.DurationVar(&c.FlushFrequency.Duration.Duration, LogFlushFreqFlagName, c.FlushFrequency.Duration.Duration, "Maximum number of seconds between log flushes")
	fs.VarP(VerbosityLevelPflag(&c.Verbosity), "v", "v", "number for the log level verbosity")
	fs.Var(VModuleConfigurationPflag(&c.VModule), "vmodule", "comma-separated list of pattern=N settings for file-filtered logging (only works for text log format)")

	fs.BoolVar(&c.Options.Text.SplitStream, "log-text-split-stream", false, "[Alpha] In text format, write error messages to stderr and info messages to stdout. The default is to write a single stream to stdout. Enable the LoggingAlphaOptions feature gate to use this.")
	fs.Var(&c.Options.Text.InfoBufferSize, "log-text-info-buffer-size", "[Alpha] In text format with split output streams, the info messages can be buffered for a while to increase performance. The default value of zero bytes disables buffering. The size can be specified as number of bytes (512), multiples of 1000 (1K), multiples of 1024 (2Ki), or powers of those (3M, 4G, 5Mi, 6Gi). Enable the LoggingAlphaOptions feature gate to use this.")

	// JSON options. We only register them if "json" is a valid format. The
	// config file API however always has them.
	if _, err := logRegistry.get("json"); err == nil {
		fs.BoolVar(&c.Options.JSON.SplitStream, "log-json-split-stream", false, "[Alpha] In JSON format, write error messages to stderr and info messages to stdout. The default is to write a single stream to stdout. Enable the LoggingAlphaOptions feature gate to use this.")
		fs.Var(&c.Options.JSON.InfoBufferSize, "log-json-info-buffer-size", "[Alpha] In JSON format with split output streams, the info messages can be buffered for a while to increase performance. The default value of zero bytes disables buffering. The size can be specified as number of bytes (512), multiples of 1000 (1K), multiples of 1024 (2Ki), or powers of those (3M, 4G, 5Mi, 6Gi). Enable the LoggingAlphaOptions feature gate to use this.")
	}
}

// SetRecommendedLoggingConfiguration sets the default logging configuration
// for fields that are unset.
//
// Consumers who embed LoggingConfiguration in their own configuration structs
// may set custom defaults and then should call this function to add the
// global defaults.
func SetRecommendedLoggingConfiguration(c *LoggingConfiguration) {
	if c.Format == "" {
		c.Format = "text"
	}
	if c.FlushFrequency.Duration.Duration == 0 {
		c.FlushFrequency.Duration.Duration = LogFlushFreqDefault
		c.FlushFrequency.SerializeAsString = true
	}
	setRecommendedOutputRouting(&c.Options.Text.OutputRoutingOptions)
	setRecommendedOutputRouting(&c.Options.JSON.OutputRoutingOptions)
}

func setRecommendedOutputRouting(o *OutputRoutingOptions) {
	var empty resource.QuantityValue
	if o.InfoBufferSize == empty {
		o.InfoBufferSize = resource.QuantityValue{
			// This is similar, but not quite the same as a default
			// constructed instance.
			Quantity: *resource.NewQuantity(0, resource.DecimalSI),
		}
		// This sets the unexported Quantity.s which will be compared
		// by reflect.DeepEqual in some tests.
		_ = o.InfoBufferSize.String()
	}
}

// loggingFlags captures the state of the logging flags, in particular their default value
// before flag parsing. It is used by unsupportedLoggingFlags.
var loggingFlags pflag.FlagSet

func init() {
	var fs flag.FlagSet
	klogflags.Init(&fs)
	loggingFlags.AddGoFlagSet(&fs)
}

// List of logs (k8s.io/klog + k8s.io/component-base/logs) flags supported by all logging formats
var supportedLogsFlags = map[string]struct{}{
	"v": {},
}

// unsupportedLoggingFlags lists unsupported logging flags. The normalize
// function is optional.
func unsupportedLoggingFlags(normalizeFunc func(f *pflag.FlagSet, name string) pflag.NormalizedName) []*pflag.Flag {
	// k8s.io/component-base/logs and klog flags
	pfs := &pflag.FlagSet{}
	loggingFlags.VisitAll(func(flag *pflag.Flag) {
		if _, found := supportedLogsFlags[flag.Name]; !found {
			// Normalization changes flag.Name, so make a copy.
			clone := *flag
			pfs.AddFlag(&clone)
		}
	})

	// Apply normalization.
	pfs.SetNormalizeFunc(normalizeFunc)

	var allFlags []*pflag.Flag
	pfs.VisitAll(func(flag *pflag.Flag) {
		allFlags = append(allFlags, flag)
	})
	return allFlags
}
