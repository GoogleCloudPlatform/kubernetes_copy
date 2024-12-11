// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package flags implements command-line flag parsing.
package flags

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"go.uber.org/zap"
)

// SetFlagsFromEnv parses all registered flags in the given flagset,
// and if they are not already set it attempts to set their values from
// environment variables. Environment variables take the name of the flag but
// are UPPERCASE, have the given prefix  and any dashes are replaced by
// underscores - for example: some-flag => ETCD_SOME_FLAG
func SetFlagsFromEnv(lg *zap.Logger, prefix string, fs *flag.FlagSet) error {
	var err error
	alreadySet := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		alreadySet[FlagToEnv(prefix, f.Name)] = true
	})
	usedEnvKey := make(map[string]bool)
	fs.VisitAll(func(f *flag.Flag) {
		if serr := setFlagFromEnv(lg, fs, prefix, f.Name, usedEnvKey, alreadySet, true); serr != nil {
			err = serr
		}
	})
	verifyEnv(lg, prefix, usedEnvKey, alreadySet)
	return err
}

// SetPflagsFromEnv is similar to SetFlagsFromEnv. However, the accepted flagset type is pflag.FlagSet
// and it does not do any logging.
func SetPflagsFromEnv(lg *zap.Logger, prefix string, fs *pflag.FlagSet) error {
	var err error
	alreadySet := make(map[string]bool)
	usedEnvKey := make(map[string]bool)
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			alreadySet[FlagToEnv(prefix, f.Name)] = true
		}
		if serr := setFlagFromEnv(lg, fs, prefix, f.Name, usedEnvKey, alreadySet, false); serr != nil {
			err = serr
		}
	})
	verifyEnv(lg, prefix, usedEnvKey, alreadySet)
	return err
}

// FlagToEnv converts flag string to upper-case environment variable key string.
func FlagToEnv(prefix, name string) string {
	return prefix + "_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func verifyEnv(lg *zap.Logger, prefix string, usedEnvKey, alreadySet map[string]bool) {
	for _, env := range os.Environ() {
		kv := strings.SplitN(env, "=", 2)
		if len(kv) != 2 {
			if lg != nil {
				lg.Warn("found invalid environment variable", zap.String("environment-variable", env))
			}
		}
		if usedEnvKey[kv[0]] {
			continue
		}
		if alreadySet[kv[0]] {
			if lg != nil {
				lg.Fatal(
					"conflicting environment variable is shadowed by corresponding command-line flag (either unset environment variable or disable flag))",
					zap.String("environment-variable", kv[0]),
				)
			}
		}
		if strings.HasPrefix(env, prefix+"_") {
			if lg != nil {
				lg.Warn("unrecognized environment variable", zap.String("environment-variable", env))
			}
		}
	}
}

type flagSetter interface {
	Set(fk string, fv string) error
}

func setFlagFromEnv(lg *zap.Logger, fs flagSetter, prefix, fname string, usedEnvKey, alreadySet map[string]bool, log bool) error {
	key := FlagToEnv(prefix, fname)
	if !alreadySet[key] {
		val := os.Getenv(key)
		if val != "" {
			usedEnvKey[key] = true
			if serr := fs.Set(fname, val); serr != nil {
				return fmt.Errorf("invalid value %q for %s: %w", val, key, serr)
			}
			if log && lg != nil {
				lg.Info(
					"recognized and used environment variable",
					zap.String("variable-name", key),
					zap.String("variable-value", val),
				)
			}
		}
	}
	return nil
}

func IsSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// GetBoolFlagVal returns the value of the a given bool flag if it is explicitly set
// in the cmd line arguments, otherwise returns nil.
func GetBoolFlagVal(fs *flag.FlagSet, flagName string) (*bool, error) {
	if !IsSet(fs, flagName) {
		return nil, nil
	}
	flagVal, parseErr := strconv.ParseBool(fs.Lookup(flagName).Value.String())
	if parseErr != nil {
		return nil, parseErr
	}
	return &flagVal, nil
}
