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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestIsDNS1123Label(t *testing.T) {
	goodValues := []string{
		"a", "ab", "abc", "a1", "a-1", "a--1--2--b",
		"0", "01", "012", "1a", "1-a", "1--a--b--2",
		strings.Repeat("a", 63),
	}
	for _, val := range goodValues {
		if msgs := IsDNS1123Label(val); len(msgs) != 0 {
			t.Errorf("expected true for '%s': %v", val, msgs)
		}
	}

	badValues := []string{
		"", "A", "ABC", "aBc", "A1", "A-1", "1-A",
		"-", "a-", "-a", "1-", "-1",
		"_", "a_", "_a", "a_b", "1_", "_1", "1_2",
		".", "a.", ".a", "a.b", "1.", ".1", "1.2",
		" ", "a ", " a", "a b", "1 ", " 1", "1 2",
		strings.Repeat("a", 64),
	}
	for _, val := range badValues {
		if msgs := IsDNS1123Label(val); len(msgs) == 0 {
			t.Errorf("expected false for '%s'", val)
		}
	}
}

func TestIsDNS1123Subdomain(t *testing.T) {
	goodValues := []string{
		"a", "ab", "abc", "a1", "a-1", "a--1--2--b",
		"0", "01", "012", "1a", "1-a", "1--a--b--2",
		"a.a", "ab.a", "abc.a", "a1.a", "a-1.a", "a--1--2--b.a",
		"a.1", "ab.1", "abc.1", "a1.1", "a-1.1", "a--1--2--b.1",
		"0.a", "01.a", "012.a", "1a.a", "1-a.a", "1--a--b--2",
		"0.1", "01.1", "012.1", "1a.1", "1-a.1", "1--a--b--2.1",
		"a.b.c.d.e", "aa.bb.cc.dd.ee", "1.2.3.4.5", "11.22.33.44.55",
		strings.Repeat("a", 253),
	}
	for _, val := range goodValues {
		if msgs := IsDNS1123Subdomain(val); len(msgs) != 0 {
			t.Errorf("expected true for '%s': %v", val, msgs)
		}
	}

	badValues := []string{
		"", "A", "ABC", "aBc", "A1", "A-1", "1-A",
		"-", "a-", "-a", "1-", "-1",
		"_", "a_", "_a", "a_b", "1_", "_1", "1_2",
		".", "a.", ".a", "a..b", "1.", ".1", "1..2",
		" ", "a ", " a", "a b", "1 ", " 1", "1 2",
		"A.a", "aB.a", "ab.A", "A1.a", "a1.A",
		"A.1", "aB.1", "A1.1", "1A.1",
		"0.A", "01.A", "012.A", "1A.a", "1a.A",
		"A.B.C.D.E", "AA.BB.CC.DD.EE", "a.B.c.d.e", "aa.bB.cc.dd.ee",
		"a@b", "a,b", "a_b", "a;b",
		"a:b", "a%b", "a?b", "a$b",
		strings.Repeat("a", 254),
	}
	for _, val := range badValues {
		if msgs := IsDNS1123Subdomain(val); len(msgs) == 0 {
			t.Errorf("expected false for '%s'", val)
		}
	}
}

func TestIsDNS1035Label(t *testing.T) {
	goodValues := []string{
		"a", "ab", "abc", "a1", "a-1", "a--1--2--b",
		strings.Repeat("a", 63),
	}
	for _, val := range goodValues {
		if msgs := IsDNS1035Label(val); len(msgs) != 0 {
			t.Errorf("expected true for '%s': %v", val, msgs)
		}
	}

	badValues := []string{
		"0", "01", "012", "1a", "1-a", "1--a--b--2",
		"", "A", "ABC", "aBc", "A1", "A-1", "1-A",
		"-", "a-", "-a", "1-", "-1",
		"_", "a_", "_a", "a_b", "1_", "_1", "1_2",
		".", "a.", ".a", "a.b", "1.", ".1", "1.2",
		" ", "a ", " a", "a b", "1 ", " 1", "1 2",
		strings.Repeat("a", 64),
	}
	for _, val := range badValues {
		if msgs := IsDNS1035Label(val); len(msgs) == 0 {
			t.Errorf("expected false for '%s'", val)
		}
	}
}

func TestIsCIdentifier(t *testing.T) {
	goodValues := []string{
		"a", "ab", "abc", "a1", "_a", "a_", "a_b", "a_1", "a__1__2__b", "__abc_123",
		"A", "AB", "AbC", "A1", "_A", "A_", "A_B", "A_1", "A__1__2__B", "__123_ABC",
	}
	for _, val := range goodValues {
		if msgs := IsCIdentifier(val); len(msgs) != 0 {
			t.Errorf("expected true for '%s': %v", val, msgs)
		}
	}

	badValues := []string{
		"", "1", "123", "1a",
		"-", "a-", "-a", "1-", "-1", "1_", "1_2",
		".", "a.", ".a", "a.b", "1.", ".1", "1.2",
		" ", "a ", " a", "a b", "1 ", " 1", "1 2",
		"#a#",
	}
	for _, val := range badValues {
		if msgs := IsCIdentifier(val); len(msgs) == 0 {
			t.Errorf("expected false for '%s'", val)
		}
	}
}

func TestIsValidPortNum(t *testing.T) {
	goodValues := []int{1, 2, 1000, 16384, 32768, 65535}
	for _, val := range goodValues {
		if msgs := IsValidPortNum(val); len(msgs) != 0 {
			t.Errorf("expected true for %d, got %v", val, msgs)
		}
	}

	badValues := []int{0, -1, 65536, 100000}
	for _, val := range badValues {
		if msgs := IsValidPortNum(val); len(msgs) == 0 {
			t.Errorf("expected false for %d", val)
		}
	}
}

func TestIsInRange(t *testing.T) {
	goodValues := []struct {
		value int
		min   int
		max   int
	}{{1, 0, 10}, {5, 5, 20}, {25, 10, 25}}
	for _, val := range goodValues {
		if msgs := IsInRange(val.value, val.min, val.max); len(msgs) > 0 {
			t.Errorf("expected no errors for %#v, but got %v", val, msgs)
		}
	}

	badValues := []struct {
		value int
		min   int
		max   int
	}{{1, 2, 10}, {5, -4, 2}, {25, 100, 120}}
	for _, val := range badValues {
		if msgs := IsInRange(val.value, val.min, val.max); len(msgs) == 0 {
			t.Errorf("expected errors for %#v", val)
		}
	}
}

func createGroupIDs(ids ...int64) []int64 {
	var output []int64
	for _, id := range ids {
		output = append(output, int64(id))
	}
	return output
}

func createUserIDs(ids ...int64) []int64 {
	var output []int64
	for _, id := range ids {
		output = append(output, int64(id))
	}
	return output
}

func TestIsValidGroupID(t *testing.T) {
	goodValues := createGroupIDs(0, 1, 1000, 65535, 2147483647)
	for _, val := range goodValues {
		if msgs := IsValidGroupID(val); len(msgs) != 0 {
			t.Errorf("expected true for '%d': %v", val, msgs)
		}
	}

	badValues := createGroupIDs(-1, -1003, 2147483648, 4147483647)
	for _, val := range badValues {
		if msgs := IsValidGroupID(val); len(msgs) == 0 {
			t.Errorf("expected false for '%d'", val)
		}
	}
}

func TestIsValidUserID(t *testing.T) {
	goodValues := createUserIDs(0, 1, 1000, 65535, 2147483647)
	for _, val := range goodValues {
		if msgs := IsValidUserID(val); len(msgs) != 0 {
			t.Errorf("expected true for '%d': %v", val, msgs)
		}
	}

	badValues := createUserIDs(-1, -1003, 2147483648, 4147483647)
	for _, val := range badValues {
		if msgs := IsValidUserID(val); len(msgs) == 0 {
			t.Errorf("expected false for '%d'", val)
		}
	}
}

func TestIsValidPortName(t *testing.T) {
	goodValues := []string{"telnet", "re-mail-ck", "pop3", "a", "a-1", "1-a", "a-1-b-2-c", "1-a-2-b-3"}
	for _, val := range goodValues {
		if msgs := IsValidPortName(val); len(msgs) != 0 {
			t.Errorf("expected true for %q: %v", val, msgs)
		}
	}

	badValues := []string{"longerthan15characters", "", strings.Repeat("a", 16), "12345", "1-2-3-4", "-begin", "end-", "two--hyphens", "whois++"}
	for _, val := range badValues {
		if msgs := IsValidPortName(val); len(msgs) == 0 {
			t.Errorf("expected false for %q", val)
		}
	}
}

func TestIsQualifiedName(t *testing.T) {
	successCases := []string{
		"simple",
		"now-with-dashes",
		"1-starts-with-num",
		"1234",
		"simple/simple",
		"now-with-dashes/simple",
		"now-with-dashes/now-with-dashes",
		"now.with.dots/simple",
		"now-with.dashes-and.dots/simple",
		"1-num.2-num/3-num",
		"1234/5678",
		"1.2.3.4/5678",
		"Uppercase_Is_OK_123",
		"example.com/Uppercase_Is_OK_123",
		"requests.storage-foo",
		strings.Repeat("a", 63),
		strings.Repeat("a", 253) + "/" + strings.Repeat("b", 63),
	}
	for i := range successCases {
		if errs := IsQualifiedName(successCases[i]); len(errs) != 0 {
			t.Errorf("case[%d]: %q: expected success: %v", i, successCases[i], errs)
		}
	}

	errorCases := []string{
		"nospecialchars%^=@",
		"cantendwithadash-",
		"-cantstartwithadash-",
		"only/one/slash",
		"Example.com/abc",
		"example_com/abc",
		"example.com/",
		"/simple",
		strings.Repeat("a", 64),
		strings.Repeat("a", 254) + "/abc",
	}
	for i := range errorCases {
		if errs := IsQualifiedName(errorCases[i]); len(errs) == 0 {
			t.Errorf("case[%d]: %q: expected failure", i, errorCases[i])
		}
	}
}

func TestIsValidLabelValue(t *testing.T) {
	successCases := []string{
		"simple",
		"now-with-dashes",
		"1-starts-with-num",
		"end-with-num-1",
		"1234",                  // only num
		strings.Repeat("a", 63), // to the limit
		"",                      // empty value
	}
	for i := range successCases {
		if errs := IsValidLabelValue(successCases[i]); len(errs) != 0 {
			t.Errorf("case %s expected success: %v", successCases[i], errs)
		}
	}

	errorCases := []string{
		"nospecialchars%^=@",
		"Tama-nui-te-rā.is.Māori.sun",
		"\\backslashes\\are\\bad",
		"-starts-with-dash",
		"ends-with-dash-",
		".starts.with.dot",
		"ends.with.dot.",
		strings.Repeat("a", 64), // over the limit
	}
	for i := range errorCases {
		if errs := IsValidLabelValue(errorCases[i]); len(errs) == 0 {
			t.Errorf("case[%d] expected failure", i)
		}
	}
}

func TestIsHTTPHeaderName(t *testing.T) {
	goodValues := []string{
		// Common ones
		"Accept-Encoding", "Host", "If-Modified-Since", "X-Forwarded-For",
		// Weirdo, but still conforming names
		"a", "ab", "abc", "a1", "-a", "a-", "a-b", "a-1", "a--1--2--b", "--abc-123",
		"A", "AB", "AbC", "A1", "-A", "A-", "A-B", "A-1", "A--1--2--B", "--123-ABC",
	}
	for _, val := range goodValues {
		if msgs := IsHTTPHeaderName(val); len(msgs) != 0 {
			t.Errorf("expected true for '%s': %v", val, msgs)
		}
	}

	badValues := []string{
		"Host:", "X-Forwarded-For:", "X-@Home",
		"", "_", "a_", "_a", "1_", "1_2", ".", "a.", ".a", "a.b", "1.", ".1", "1.2",
		" ", "a ", " a", "a b", "1 ", " 1", "1 2", "#a#", "^", ",", ";", "=", "<",
		"?", "@", "{",
	}
	for _, val := range badValues {
		if msgs := IsHTTPHeaderName(val); len(msgs) == 0 {
			t.Errorf("expected false for '%s'", val)
		}
	}
}

func TestIsValidPercent(t *testing.T) {
	goodValues := []string{
		"0%",
		"00000%",
		"1%",
		"01%",
		"99%",
		"100%",
		"101%",
	}
	for _, val := range goodValues {
		if msgs := IsValidPercent(val); len(msgs) != 0 {
			t.Errorf("expected true for %q: %v", val, msgs)
		}
	}

	badValues := []string{
		"",
		"0",
		"100",
		"0.0%",
		"99.9%",
		"hundred",
		" 1%",
		"1% ",
		"-0%",
		"-1%",
		"+1%",
	}
	for _, val := range badValues {
		if msgs := IsValidPercent(val); len(msgs) == 0 {
			t.Errorf("expected false for %q", val)
		}
	}
}

func TestIsConfigMapKey(t *testing.T) {
	successCases := []string{
		"a",
		"good",
		"good-good",
		"still.good",
		"this.is.also.good",
		".so.is.this",
		"THIS_IS_GOOD",
		"so_is_this_17",
	}

	for i := range successCases {
		if errs := IsConfigMapKey(successCases[i]); len(errs) != 0 {
			t.Errorf("[%d] expected success: %v", i, errs)
		}
	}

	failureCases := []string{
		".",
		"..",
		"..bad",
		"b*d",
		"bad!&bad",
	}

	for i := range failureCases {
		if errs := IsConfigMapKey(failureCases[i]); len(errs) == 0 {
			t.Errorf("[%d] expected failure", i)
		}
	}
}

func TestIsWildcardDNS1123Subdomain(t *testing.T) {
	goodValues := []string{
		"*.example.com",
		"*.bar.com",
		"*.foo.bar.com",
	}
	for _, val := range goodValues {
		if errs := IsWildcardDNS1123Subdomain(val); len(errs) != 0 {
			t.Errorf("expected no errors for %q: %v", val, errs)
		}
	}

	badValues := []string{
		"*.*.bar.com",
		"*.foo.*.com",
		"*bar.com",
		"f*.bar.com",
		"*",
	}
	for _, val := range badValues {
		if errs := IsWildcardDNS1123Subdomain(val); len(errs) == 0 {
			t.Errorf("expected errors for %q", val)
		}
	}
}

func TestIsFullyQualifiedDomainName(t *testing.T) {
	goodValues := []string{
		"a.com",
		"k8s.io",
		"dev.k8s.io",
		"dev.k8s.io.",
		"foo.example.com",
		"this.is.a.really.long.fqdn",
		"bbc.co.uk",
		"10.0.0.1", // DNS labels can start with numbers and there is no requirement for letters.
		"hyphens-are-good.k8s.io",
		strings.Repeat("a", 63) + ".k8s.io",
		strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 54) + ".k8s.io",
	}
	for _, val := range goodValues {
		if err := IsFullyQualifiedDomainName(field.NewPath(""), val).ToAggregate(); err != nil {
			t.Errorf("expected no errors for %q: %v", val, err)
		}
	}

	badValues := []string{
		".",
		"...",
		".io",
		"com",
		".com",
		"Dev.k8s.io",
		".foo.example.com",
		"*.example.com",
		"*.bar.com",
		"*.foo.bar.com",
		"underscores_are_bad.k8s.io",
		"foo@bar.example.com",
		"http://foo.example.com",
		strings.Repeat("a", 64) + ".k8s.io",
		strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 55) + ".k8s.io",
	}
	for _, val := range badValues {
		if err := IsFullyQualifiedDomainName(field.NewPath(""), val).ToAggregate(); err == nil {
			t.Errorf("expected errors for %q", val)
		}
	}
}

func TestIsFullyQualifiedName(t *testing.T) {
	goodValues := []string{
		"dev.k8s.io",
		"foo.example.com",
		"this.is.a.really.long.fqdn",
		"bbc.co.uk",
		"10.0.0.1", // DNS labels can start with numbers and there is no requirement for letters.
		"hyphens-are-good.k8s.io",
		strings.Repeat("a", 246) + ".k8s.io",
	}
	for _, val := range goodValues {
		if err := IsFullyQualifiedName(field.NewPath(""), val).ToAggregate(); err != nil {
			t.Errorf("expected no errors for %q: %v", val, err)
		}
	}

	badValues := []string{
		"...",
		"dev.k8s.io.",
		".io",
		"Dev.k8s.io",
		"k8s.io",
		"*.example.com",
		"*.bar.com",
		"*.foo.bar.com",
		"underscores_are_bad.k8s.io",
		"foo@bar.example.com",
		"http://foo.example.com",
		strings.Repeat("a", 247) + ".k8s.io",
	}
	for _, val := range badValues {
		if err := IsFullyQualifiedName(field.NewPath(""), val).ToAggregate(); err == nil {
			t.Errorf("expected errors for %q", val)
		}
	}

	messageTests := []struct {
		name       string
		targetName string
		err        string
	}{{
		name:       "name needs to be fully qualified, i.e., contains at least 2 dots",
		targetName: "k8s.io",
		err:        "should be a domain with at least three segments separated by dots",
	}, {
		name:       "name should not include scheme",
		targetName: "http://foo.k8s.io",
		err:        "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters",
	}, {
		name:       "email should be invalid",
		targetName: "example@foo.k8s.io",
		err:        "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters",
	}, {
		name:       "name cannot be empty",
		targetName: "",
		err:        "Required value",
	}, {
		name:       "name must conform to RFC 1123",
		targetName: "A.B.C",
		err:        "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters",
	}}
	for _, tc := range messageTests {
		err := IsFullyQualifiedName(field.NewPath(""), tc.targetName).ToAggregate()
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%q: unexpected error: %v", tc.name, err)
		case tc.err != "" && err == nil:
			t.Errorf("%q: unexpected no error, expected %s", tc.name, tc.err)
		case tc.err != "" && err != nil && !strings.Contains(err.Error(), tc.err):
			t.Errorf("%q: expected %s, got %v", tc.name, tc.err, err)
		}
	}
}

func TestIsDomainPrefixedPath(t *testing.T) {
	goodValues := []string{
		"a/b",
		"a/b/c/d",
		"a.com/foo",
		"a.b.c.d/foo",
		"k8s.io/foo/bar",
		"k8s.io/FOO/BAR",
		"dev.k8s.io/more/path",
		"this.is.a.really.long.fqdn/even/longer/path/just/because",
		"bbc.co.uk/path/goes/here",
		"10.0.0.1/foo",
		"hyphens-are-good.k8s.io/and-in-paths-too",
		strings.Repeat("a", 240) + ".k8s.io/a",
		"k8s.io/" + strings.Repeat("a", 240),
	}
	for _, val := range goodValues {
		if err := IsDomainPrefixedPath(field.NewPath(""), val).ToAggregate(); err != nil {
			t.Errorf("expected no errors for %q: %v", val, err)
		}
	}

	badValues := []string{
		".",
		"...",
		"/b",
		"com",
		".com",
		"a.b.c.d/foo?a=b",
		"a.b.c.d/foo#a",
		"Dev.k8s.io",
		".foo.example.com",
		"*.example.com",
		"example.com/foo{}[]@^`",
		"underscores_are_bad.k8s.io",
		"underscores_are_bad.k8s.io/foo",
		"foo@bar.example.com",
		"foo@bar.example.com/foo",
		strings.Repeat("a", 247) + ".k8s.io",
	}
	for _, val := range badValues {
		if err := IsDomainPrefixedPath(field.NewPath(""), val).ToAggregate(); err == nil {
			t.Errorf("expected errors for %q", val)
		}
	}
}

func TestIsRelaxedEnvVarName(t *testing.T) {
	goodValues := []string{
		"-", ":", "_", "+a", ">a", "<a",
		"a.", "a..", "*a", "%a", "?a",
		"a:a", "a_a", "aAz", "~a", "|a",
		"a0a", "a9", "/a", "a ", "#a",
		"0a", "0 a", "'a", "(a", "@a",
	}
	for _, val := range goodValues {
		if msgs := IsRelaxedEnvVarName(val); len(msgs) != 0 {
			t.Errorf("expected true for '%s': %v", val, msgs)
		}
	}

	badValues := []string{
		"", "=", "a=", "1=a", "a=b", "#%=&&",
		string(rune(1)) + "abc", string(rune(130)) + "abc",
		"Ç ç", "Ä ä", "Ñ ñ", "Ø ø",
	}

	for _, val := range badValues {
		if msgs := IsRelaxedEnvVarName(val); len(msgs) == 0 {
			t.Errorf("expected false for '%s'", val)
		}
	}
}
