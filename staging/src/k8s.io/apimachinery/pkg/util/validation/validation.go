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

package validation

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

const qnameCharFmt string = "[A-Za-z0-9]"
const qnameExtCharFmt string = "[-A-Za-z0-9_.]"
const qualifiedNameFmt string = "(" + qnameCharFmt + qnameExtCharFmt + "*)?" + qnameCharFmt
const qualifiedNameErrMsg string = "must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character"
const qualifiedNameMaxLength int = 63

var qualifiedNameRegexp = regexp.MustCompile("^" + qualifiedNameFmt + "$")

// IsQualifiedName tests whether the value passed is what Kubernetes calls a
// "qualified name".  This is a format used in various places throughout the
// system.  If the value is not valid, a list of error strings is returned.
// Otherwise an empty list (or nil) is returned.
func IsQualifiedName(value string) []string {
	var errs []string
	parts := strings.Split(value, "/")
	var name string
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		var prefix string
		prefix, name = parts[0], parts[1]
		if len(prefix) == 0 {
			errs = append(errs, "prefix part "+EmptyError())
		} else if msgs := IsDNS1123Subdomain(prefix); len(msgs) != 0 {
			errs = append(errs, prefixEach(msgs, "prefix part ")...)
		}
	default:
		return append(errs, "a qualified name "+RegexError(qualifiedNameErrMsg, qualifiedNameFmt, "MyName", "my.name", "123-abc")+
			" with an optional DNS subdomain prefix and '/' (e.g. 'example.com/MyName')")
	}

	if len(name) == 0 {
		errs = append(errs, "name part "+EmptyError())
	} else if len(name) > qualifiedNameMaxLength {
		errs = append(errs, "name part "+MaxLenError(qualifiedNameMaxLength))
	}
	if !qualifiedNameRegexp.MatchString(name) {
		errs = append(errs, "name part "+RegexError(qualifiedNameErrMsg, qualifiedNameFmt, "MyName", "my.name", "123-abc"))
	}
	return errs
}

// IsFullyQualifiedName checks if the name is fully qualified. This is similar
// to IsFullyQualifiedDomainName but requires a minimum of 3 segments instead of
// 2 and does not accept a trailing . as valid.
// TODO: This function is deprecated and preserved until all callers migrate to
// IsFullyQualifiedDomainName; please don't add new callers.
func IsFullyQualifiedName(fldPath *field.Path, name string) field.ErrorList {
	var allErrors field.ErrorList
	if len(name) == 0 {
		return append(allErrors, field.Required(fldPath, ""))
	}
	if errs := IsDNS1123Subdomain(name); len(errs) > 0 {
		return append(allErrors, field.Invalid(fldPath, name, strings.Join(errs, ",")))
	}
	if len(strings.Split(name, ".")) < 3 {
		return append(allErrors, field.Invalid(fldPath, name, "should be a domain with at least three segments separated by dots"))
	}
	return allErrors
}

// IsFullyQualifiedDomainName checks if the domain name is fully qualified. This
// is similar to IsFullyQualifiedName but only requires a minimum of 2 segments
// instead of 3 and accepts a trailing . as valid.
func IsFullyQualifiedDomainName(fldPath *field.Path, name string) field.ErrorList {
	var allErrors field.ErrorList
	if len(name) == 0 {
		return append(allErrors, field.Required(fldPath, ""))
	}
	if strings.HasSuffix(name, ".") {
		name = name[:len(name)-1]
	}
	if errs := IsDNS1123Subdomain(name); len(errs) > 0 {
		return append(allErrors, field.Invalid(fldPath, name, strings.Join(errs, ",")))
	}
	if len(strings.Split(name, ".")) < 2 {
		return append(allErrors, field.Invalid(fldPath, name, "should be a domain with at least two segments separated by dots"))
	}
	for _, label := range strings.Split(name, ".") {
		if errs := IsDNS1123Label(label); len(errs) > 0 {
			return append(allErrors, field.Invalid(fldPath, label, strings.Join(errs, ",")))
		}
	}
	return allErrors
}

// Allowed characters in an HTTP Path as defined by RFC 3986. A HTTP path may
// contain:
// * unreserved characters (alphanumeric, '-', '.', '_', '~')
// * percent-encoded octets
// * sub-delims ("!", "$", "&", "'", "(", ")", "*", "+", ",", ";", "=")
// * a colon character (":")
const httpPathFmt string = `[A-Za-z0-9/\-._~%!$&'()*+,;=:]+`

var httpPathRegexp = regexp.MustCompile("^" + httpPathFmt + "$")

// IsDomainPrefixedPath checks if the given string is a domain-prefixed path
// (e.g. acme.io/foo). All characters before the first "/" must be a valid
// subdomain as defined by RFC 1123. All characters trailing the first "/" must
// be valid HTTP Path characters as defined by RFC 3986.
func IsDomainPrefixedPath(fldPath *field.Path, dpPath string) field.ErrorList {
	var allErrs field.ErrorList
	if len(dpPath) == 0 {
		return append(allErrs, field.Required(fldPath, ""))
	}

	segments := strings.SplitN(dpPath, "/", 2)
	if len(segments) != 2 || len(segments[0]) == 0 || len(segments[1]) == 0 {
		return append(allErrs, field.Invalid(fldPath, dpPath, "must be a domain-prefixed path (such as \"acme.io/foo\")"))
	}

	host := segments[0]
	for _, err := range IsDNS1123Subdomain(host) {
		allErrs = append(allErrs, field.Invalid(fldPath, host, err))
	}

	path := segments[1]
	if !httpPathRegexp.MatchString(path) {
		return append(allErrs, field.Invalid(fldPath, path, RegexError("Invalid path", httpPathFmt)))
	}

	return allErrs
}

const labelValueFmt string = "(" + qualifiedNameFmt + ")?"
const labelValueErrMsg string = "a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character"

// LabelValueMaxLength is a label's max length
const LabelValueMaxLength int = 63

var labelValueRegexp = regexp.MustCompile("^" + labelValueFmt + "$")

// IsValidLabelValue tests whether the value passed is a valid label value.  If
// the value is not valid, a list of error strings is returned.  Otherwise an
// empty list (or nil) is returned.
func IsValidLabelValue(value string) []string {
	var errs []string
	if len(value) > LabelValueMaxLength {
		errs = append(errs, MaxLenError(LabelValueMaxLength))
	}
	if !labelValueRegexp.MatchString(value) {
		errs = append(errs, RegexError(labelValueErrMsg, labelValueFmt, "MyValue", "my_value", "12345"))
	}
	return errs
}

const dns1123LabelFmt string = "[a-z0-9]([-a-z0-9]*[a-z0-9])?"
const dns1123LabelFmtWithUnderscore string = "_?[a-z0-9]([-_a-z0-9]*[a-z0-9])?"

const dns1123LabelErrMsg string = "a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character"

// DNS1123LabelMaxLength is a label's max length in DNS (RFC 1123)
const DNS1123LabelMaxLength int = 63

var dns1123LabelRegexp = regexp.MustCompile("^" + dns1123LabelFmt + "$")

// IsDNS1123Label tests for a string that conforms to the definition of a label in
// DNS (RFC 1123).
func IsDNS1123Label(value string) []string {
	var errs []string
	if len(value) > DNS1123LabelMaxLength {
		errs = append(errs, MaxLenError(DNS1123LabelMaxLength))
	}
	if !dns1123LabelRegexp.MatchString(value) {
		if dns1123SubdomainRegexp.MatchString(value) {
			// It was a valid subdomain and not a valid label.  Since we
			// already checked length, it must be dots.
			errs = append(errs, "must not contain dots")
		} else {
			errs = append(errs, RegexError(dns1123LabelErrMsg, dns1123LabelFmt, "my-name", "123-abc"))
		}
	}
	return errs
}

const dns1123SubdomainFmt string = dns1123LabelFmt + "(\\." + dns1123LabelFmt + ")*"
const dns1123SubdomainErrorMsg string = "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character"

const dns1123SubdomainFmtWithUnderscore string = dns1123LabelFmtWithUnderscore + "(\\." + dns1123LabelFmtWithUnderscore + ")*"
const dns1123SubdomainErrorMsgFG string = "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '_', '-' or '.', and must start and end with an alphanumeric character"

// DNS1123SubdomainMaxLength is a subdomain's max length in DNS (RFC 1123)
const DNS1123SubdomainMaxLength int = 253

var dns1123SubdomainRegexp = regexp.MustCompile("^" + dns1123SubdomainFmt + "$")
var dns1123SubdomainRegexpWithUnderscore = regexp.MustCompile("^" + dns1123SubdomainFmtWithUnderscore + "$")

// IsDNS1123Subdomain tests for a string that conforms to the definition of a
// subdomain in DNS (RFC 1123).
func IsDNS1123Subdomain(value string) []string {
	var errs []string
	if len(value) > DNS1123SubdomainMaxLength {
		errs = append(errs, MaxLenError(DNS1123SubdomainMaxLength))
	}
	if !dns1123SubdomainRegexp.MatchString(value) {
		errs = append(errs, RegexError(dns1123SubdomainErrorMsg, dns1123SubdomainFmt, "example.com"))
	}
	return errs
}

// IsDNS1123SubdomainWithUnderscore tests for a string that conforms to the definition of a
// subdomain in DNS (RFC 1123), but allows the use of an underscore in the string
func IsDNS1123SubdomainWithUnderscore(value string) []string {
	var errs []string
	if len(value) > DNS1123SubdomainMaxLength {
		errs = append(errs, MaxLenError(DNS1123SubdomainMaxLength))
	}
	if !dns1123SubdomainRegexpWithUnderscore.MatchString(value) {
		errs = append(errs, RegexError(dns1123SubdomainErrorMsgFG, dns1123SubdomainFmt, "example.com"))
	}
	return errs
}

const dns1035LabelFmt string = "[a-z]([-a-z0-9]*[a-z0-9])?"
const dns1035LabelErrMsg string = "a DNS-1035 label must consist of lower case alphanumeric characters or '-', start with an alphabetic character, and end with an alphanumeric character"

// DNS1035LabelMaxLength is a label's max length in DNS (RFC 1035)
const DNS1035LabelMaxLength int = 63

var dns1035LabelRegexp = regexp.MustCompile("^" + dns1035LabelFmt + "$")

// IsDNS1035Label tests for a string that conforms to the definition of a label in
// DNS (RFC 1035).
func IsDNS1035Label(value string) []string {
	var errs []string
	if len(value) > DNS1035LabelMaxLength {
		errs = append(errs, MaxLenError(DNS1035LabelMaxLength))
	}
	if !dns1035LabelRegexp.MatchString(value) {
		errs = append(errs, RegexError(dns1035LabelErrMsg, dns1035LabelFmt, "my-name", "abc-123"))
	}
	return errs
}

// wildcard definition - RFC 1034 section 4.3.3.
// examples:
// - valid: *.bar.com, *.foo.bar.com
// - invalid: *.*.bar.com, *.foo.*.com, *bar.com, f*.bar.com, *
const wildcardDNS1123SubdomainFmt = "\\*\\." + dns1123SubdomainFmt
const wildcardDNS1123SubdomainErrMsg = "a wildcard DNS-1123 subdomain must start with '*.', followed by a valid DNS subdomain, which must consist of lower case alphanumeric characters, '-' or '.' and end with an alphanumeric character"

// IsWildcardDNS1123Subdomain tests for a string that conforms to the definition of a
// wildcard subdomain in DNS (RFC 1034 section 4.3.3).
func IsWildcardDNS1123Subdomain(value string) []string {
	wildcardDNS1123SubdomainRegexp := regexp.MustCompile("^" + wildcardDNS1123SubdomainFmt + "$")

	var errs []string
	if len(value) > DNS1123SubdomainMaxLength {
		errs = append(errs, MaxLenError(DNS1123SubdomainMaxLength))
	}
	if !wildcardDNS1123SubdomainRegexp.MatchString(value) {
		errs = append(errs, RegexError(wildcardDNS1123SubdomainErrMsg, wildcardDNS1123SubdomainFmt, "*.example.com"))
	}
	return errs
}

const cIdentifierFmt string = "[A-Za-z_][A-Za-z0-9_]*"
const identifierErrMsg string = "a valid C identifier must start with alphabetic character or '_', followed by a string of alphanumeric characters or '_'"

var cIdentifierRegexp = regexp.MustCompile("^" + cIdentifierFmt + "$")

// IsCIdentifier tests for a string that conforms the definition of an identifier
// in C. This checks the format, but not the length.
func IsCIdentifier(value string) []string {
	if !cIdentifierRegexp.MatchString(value) {
		return []string{RegexError(identifierErrMsg, cIdentifierFmt, "my_name", "MY_NAME", "MyName")}
	}
	return nil
}

// IsValidPortNum tests that the argument is a valid, non-zero port number.
func IsValidPortNum(port int) []string {
	if 1 <= port && port <= 65535 {
		return nil
	}
	return []string{InclusiveRangeError(1, 65535)}
}

// IsInRange tests that the argument is in an inclusive range.
func IsInRange(value int, min int, max int) []string {
	if value >= min && value <= max {
		return nil
	}
	return []string{InclusiveRangeError(min, max)}
}

// Now in libcontainer UID/GID limits is 0 ~ 1<<31 - 1
// TODO: once we have a type for UID/GID we should make these that type.
const (
	minUserID  = 0
	maxUserID  = math.MaxInt32
	minGroupID = 0
	maxGroupID = math.MaxInt32
)

// IsValidGroupID tests that the argument is a valid Unix GID.
func IsValidGroupID(gid int64) []string {
	if minGroupID <= gid && gid <= maxGroupID {
		return nil
	}
	return []string{InclusiveRangeError(minGroupID, maxGroupID)}
}

// IsValidUserID tests that the argument is a valid Unix UID.
func IsValidUserID(uid int64) []string {
	if minUserID <= uid && uid <= maxUserID {
		return nil
	}
	return []string{InclusiveRangeError(minUserID, maxUserID)}
}

var portNameCharsetRegex = regexp.MustCompile("^[-a-z0-9]+$")
var portNameOneLetterRegexp = regexp.MustCompile("[a-z]")

// IsValidPortName check that the argument is valid syntax. It must be
// non-empty and no more than 15 characters long. It may contain only [-a-z0-9]
// and must contain at least one letter [a-z]. It must not start or end with a
// hyphen, nor contain adjacent hyphens.
//
// Note: We only allow lower-case characters, even though RFC 6335 is case
// insensitive.
func IsValidPortName(port string) []string {
	var errs []string
	if len(port) > 15 {
		errs = append(errs, MaxLenError(15))
	}
	if !portNameCharsetRegex.MatchString(port) {
		errs = append(errs, "must contain only alpha-numeric characters (a-z, 0-9), and hyphens (-)")
	}
	if !portNameOneLetterRegexp.MatchString(port) {
		errs = append(errs, "must contain at least one letter (a-z)")
	}
	if strings.Contains(port, "--") {
		errs = append(errs, "must not contain consecutive hyphens")
	}
	if len(port) > 0 && (port[0] == '-' || port[len(port)-1] == '-') {
		errs = append(errs, "must not begin or end with a hyphen")
	}
	return errs
}

const percentFmt string = "[0-9]+%"
const percentErrMsg string = "a valid percent string must be a numeric string followed by an ending '%'"

var percentRegexp = regexp.MustCompile("^" + percentFmt + "$")

// IsValidPercent checks that string is in the form of a percentage
func IsValidPercent(percent string) []string {
	if !percentRegexp.MatchString(percent) {
		return []string{RegexError(percentErrMsg, percentFmt, "1%", "93%")}
	}
	return nil
}

const httpHeaderNameFmt string = "[-A-Za-z0-9]+"
const httpHeaderNameErrMsg string = "a valid HTTP header must consist of alphanumeric characters or '-'"

var httpHeaderNameRegexp = regexp.MustCompile("^" + httpHeaderNameFmt + "$")

// IsHTTPHeaderName checks that a string conforms to the Go HTTP library's
// definition of a valid header field name (a stricter subset than RFC7230).
func IsHTTPHeaderName(value string) []string {
	if !httpHeaderNameRegexp.MatchString(value) {
		return []string{RegexError(httpHeaderNameErrMsg, httpHeaderNameFmt, "X-Header-Name")}
	}
	return nil
}

const envVarNameFmt = "[-._a-zA-Z][-._a-zA-Z0-9]*"
const envVarNameFmtErrMsg string = "a valid environment variable name must consist of alphabetic characters, digits, '_', '-', or '.', and must not start with a digit"

// TODO(hirazawaui): Rename this when the RelaxedEnvironmentVariableValidation gate is removed.
const relaxedEnvVarNameFmtErrMsg string = "a valid environment variable name must consist only of printable ASCII characters other than '='"

var envVarNameRegexp = regexp.MustCompile("^" + envVarNameFmt + "$")

// IsEnvVarName tests if a string is a valid environment variable name.
func IsEnvVarName(value string) []string {
	var errs []string
	if !envVarNameRegexp.MatchString(value) {
		errs = append(errs, RegexError(envVarNameFmtErrMsg, envVarNameFmt, "my.env-name", "MY_ENV.NAME", "MyEnvName1"))
	}

	errs = append(errs, hasChDirPrefix(value)...)
	return errs
}

// IsRelaxedEnvVarName tests if a string is a valid environment variable name.
func IsRelaxedEnvVarName(value string) []string {
	var errs []string

	if len(value) == 0 {
		errs = append(errs, "environment variable name "+EmptyError())
	}

	for _, r := range value {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || r == '=' {
			errs = append(errs, relaxedEnvVarNameFmtErrMsg)
			break
		}
	}

	return errs
}

const configMapKeyFmt = `[-._a-zA-Z0-9]+`
const configMapKeyErrMsg string = "a valid config key must consist of alphanumeric characters, '-', '_' or '.'"

var configMapKeyRegexp = regexp.MustCompile("^" + configMapKeyFmt + "$")

// IsConfigMapKey tests for a string that is a valid key for a ConfigMap or Secret
func IsConfigMapKey(value string) []string {
	var errs []string
	if len(value) > DNS1123SubdomainMaxLength {
		errs = append(errs, MaxLenError(DNS1123SubdomainMaxLength))
	}
	if !configMapKeyRegexp.MatchString(value) {
		errs = append(errs, RegexError(configMapKeyErrMsg, configMapKeyFmt, "key.name", "KEY_NAME", "key-name"))
	}
	errs = append(errs, hasChDirPrefix(value)...)
	return errs
}

// MaxLenError returns a string explanation of a "string too long" validation
// failure.
func MaxLenError(length int) string {
	return fmt.Sprintf("must be no more than %d characters", length)
}

// RegexError returns a string explanation of a regex validation failure.
func RegexError(msg string, fmt string, examples ...string) string {
	if len(examples) == 0 {
		return msg + " (regex used for validation is '" + fmt + "')"
	}
	msg += " (e.g. "
	for i := range examples {
		if i > 0 {
			msg += " or "
		}
		msg += "'" + examples[i] + "', "
	}
	msg += "regex used for validation is '" + fmt + "')"
	return msg
}

// EmptyError returns a string explanation of a "must not be empty" validation
// failure.
func EmptyError() string {
	return "must be non-empty"
}

func prefixEach(msgs []string, prefix string) []string {
	for i := range msgs {
		msgs[i] = prefix + msgs[i]
	}
	return msgs
}

// InclusiveRangeError returns a string explanation of a numeric "must be
// between" validation failure.
func InclusiveRangeError(lo, hi int) string {
	return fmt.Sprintf(`must be between %d and %d, inclusive`, lo, hi)
}

func hasChDirPrefix(value string) []string {
	var errs []string
	switch {
	case value == ".":
		errs = append(errs, `must not be '.'`)
	case value == "..":
		errs = append(errs, `must not be '..'`)
	case strings.HasPrefix(value, ".."):
		errs = append(errs, `must not start with '..'`)
	}
	return errs
}
