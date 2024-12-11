// Copied from https://github.com/etcd-io/etcd/blob/main/client/pkg/verify/verify.go
package common

import (
	"fmt"
	"os"
	"strings"
)

const ENV_VERIFY = "BBOLT_VERIFY"

type VerificationType string

const (
	ENV_VERIFY_VALUE_ALL    VerificationType = "all"
	ENV_VERIFY_VALUE_ASSERT VerificationType = "assert"
)

func getEnvVerify() string {
	return strings.ToLower(os.Getenv(ENV_VERIFY))
}

func IsVerificationEnabled(verification VerificationType) bool {
	env := getEnvVerify()
	return env == string(ENV_VERIFY_VALUE_ALL) || env == strings.ToLower(string(verification))
}

// EnableVerifications sets `ENV_VERIFY` and returns a function that
// can be used to bring the original settings.
func EnableVerifications(verification VerificationType) func() {
	previousEnv := getEnvVerify()
	os.Setenv(ENV_VERIFY, string(verification))
	return func() {
		os.Setenv(ENV_VERIFY, previousEnv)
	}
}

// EnableAllVerifications enables verification and returns a function
// that can be used to bring the original settings.
func EnableAllVerifications() func() {
	return EnableVerifications(ENV_VERIFY_VALUE_ALL)
}

// DisableVerifications unsets `ENV_VERIFY` and returns a function that
// can be used to bring the original settings.
func DisableVerifications() func() {
	previousEnv := getEnvVerify()
	os.Unsetenv(ENV_VERIFY)
	return func() {
		os.Setenv(ENV_VERIFY, previousEnv)
	}
}

// Verify performs verification if the assertions are enabled.
// In the default setup running in tests and skipped in the production code.
func Verify(f func()) {
	if IsVerificationEnabled(ENV_VERIFY_VALUE_ASSERT) {
		f()
	}
}

// Assert will panic with a given formatted message if the given condition is false.
func Assert(condition bool, msg string, v ...any) {
	if !condition {
		panic(fmt.Sprintf("assertion failed: "+msg, v...))
	}
}
