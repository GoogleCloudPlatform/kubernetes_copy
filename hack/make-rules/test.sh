#!/usr/bin/env bash

# Copyright 2014 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/../..
source "${KUBE_ROOT}/hack/lib/init.sh"

kube::golang::setup_env
kube::golang::setup_gomaxprocs

# start the cache mutation detector by default so that cache mutators will be found
KUBE_CACHE_MUTATION_DETECTOR="${KUBE_CACHE_MUTATION_DETECTOR:-true}"
export KUBE_CACHE_MUTATION_DETECTOR

# panic the server on watch decode errors since they are considered coder mistakes
KUBE_PANIC_WATCH_DECODE_ERROR="${KUBE_PANIC_WATCH_DECODE_ERROR:-true}"
export KUBE_PANIC_WATCH_DECODE_ERROR

kube::test::find_dirs() {
  (
    cd "${KUBE_ROOT}"
    find -L . -not \( \
        \( \
          -path './_artifacts/*' \
          -o -path './_output/*' \
          -o -path './_gopath/*' \
          -o -path './cmd/kubeadm/test/*' \
          -o -path './contrib/podex/*' \
          -o -path './output/*' \
          -o -path './release/*' \
          -o -path './target/*' \
          -o -path './test/e2e/e2e_test.go' \
          -o -path './test/e2e_node/*' \
          -o -path './test/e2e_kubeadm/*' \
          -o -path './test/integration/*' \
          -o -path './third_party/*' \
          -o -path './staging/*' \
          -o -path './vendor/*' \
        \) -prune \
      \) -name '*_test.go' -print0 | xargs -0n1 dirname | sed "s|^\./|${KUBE_GO_PACKAGE}/|" | LC_ALL=C sort -u

    find ./staging -name '*_test.go' -not -path '*/test/integration/*' -prune -print0 | xargs -0n1 dirname | sed 's|^\./staging/src/|./vendor/|' | LC_ALL=C sort -u
  )
}

# TODO: This timeout should really be lower, this is a *long* time to test one
# package, however pkg/api/testing in particular will fail with a lower timeout
# currently. We should attempt to lower this over time.
KUBE_TIMEOUT=${KUBE_TIMEOUT:--timeout=180s}
KUBE_COVER=${KUBE_COVER:-n} # set to 'y' to enable coverage collection
KUBE_COVERMODE=${KUBE_COVERMODE:-atomic}
# The directory to save test coverage reports to, if generating them. If unset,
# a semi-predictable temporary directory will be used.
KUBE_COVER_REPORT_DIR="${KUBE_COVER_REPORT_DIR:-}"
# How many 'go test' instances to run simultaneously when running tests in
# coverage mode.
KUBE_COVERPROCS=${KUBE_COVERPROCS:-4}
# use KUBE_RACE="" to disable the race detector
# this is defaulted to "-race" in make test as well
# NOTE: DO NOT ADD A COLON HERE. KUBE_RACE="" is meaningful!
KUBE_RACE=${KUBE_RACE-"-race"}
# Set to the goveralls binary path to report coverage results to Coveralls.io.
KUBE_GOVERALLS_BIN=${KUBE_GOVERALLS_BIN:-}
# once we have multiple group supports
# Create a junit-style XML test report in this directory if set.
KUBE_JUNIT_REPORT_DIR=${KUBE_JUNIT_REPORT_DIR:-}
# If KUBE_JUNIT_REPORT_DIR is unset, and ARTIFACTS is set, then have them match.
if [[ -z "${KUBE_JUNIT_REPORT_DIR:-}" && -n "${ARTIFACTS:-}" ]]; then
    export KUBE_JUNIT_REPORT_DIR="${ARTIFACTS}"
fi
# Set to 'y' to keep the verbose stdout from tests when KUBE_JUNIT_REPORT_DIR is
# set.
KUBE_KEEP_VERBOSE_TEST_OUTPUT=${KUBE_KEEP_VERBOSE_TEST_OUTPUT:-n}

kube::test::usage() {
  kube::log::usage_from_stdin <<EOF
usage: $0 [OPTIONS] [TARGETS]

OPTIONS:
  -p <number>   : number of parallel workers, must be >= 1
EOF
}

isnum() {
  [[ "$1" =~ ^[0-9]+$ ]]
}

PARALLEL="${PARALLEL:-1}"
while getopts "hp:i:" opt ; do
  case ${opt} in
    h)
      kube::test::usage
      exit 0
      ;;
    p)
      PARALLEL="${OPTARG}"
      if ! isnum "${PARALLEL}" || [[ "${PARALLEL}" -le 0 ]]; then
        kube::log::usage "'$0': argument to -p must be numeric and greater than 0"
        kube::test::usage
        exit 1
      fi
      ;;
    i)
      kube::log::usage "'$0': use GOFLAGS='-count <num-iterations>'"
      kube::test::usage
      exit 1
      ;;
    :)
      kube::log::usage "Option -${OPTARG} <value>"
      kube::test::usage
      exit 1
      ;;
    ?)
      kube::test::usage
      exit 1
      ;;
  esac
done
shift $((OPTIND - 1))

# Use eval to preserve embedded quoted strings.
testargs=()
eval "testargs=(${KUBE_TEST_ARGS:-})"

# Used to filter verbose test output.
go_test_grep_pattern=".*"

goflags=()
# The junit report tool needs full test case information to produce a
# meaningful report.
if [[ -n "${KUBE_JUNIT_REPORT_DIR}" ]] ; then
  goflags+=(-v)
  goflags+=(-json)
  # Show only summary lines by matching lines like "status package/test"
  go_test_grep_pattern="^[^[:space:]]\+[[:space:]]\+[^[:space:]]\+/[^[[:space:]]\+"
fi

if [[ -n "${FULL_LOG:-}" ]] ; then
  go_test_grep_pattern=".*"
fi

# Filter out arguments that start with "-" and move them to goflags.
testcases=()
for arg; do
  if [[ "${arg}" == -* ]]; then
    goflags+=("${arg}")
  else
    testcases+=("${arg}")
  fi
done
if [[ ${#testcases[@]} -eq 0 ]]; then
  while IFS='' read -r line; do testcases+=("$line"); done < <(kube::test::find_dirs)
fi
set -- "${testcases[@]+${testcases[@]}}"

if [[ -n "${KUBE_RACE}" ]] ; then
  goflags+=("${KUBE_RACE}")
fi

junitFilenamePrefix() {
  if [[ -z "${KUBE_JUNIT_REPORT_DIR}" ]]; then
    echo ""
    return
  fi
  mkdir -p "${KUBE_JUNIT_REPORT_DIR}"
  echo "${KUBE_JUNIT_REPORT_DIR}/junit_$(kube::util::sortable_date)"
}

verifyAndSuggestPackagePath() {
  local specified_package_path="$1"
  local alternative_package_path="$2"
  local original_package_path="$3"
  local suggestion_package_path="$4"

  if [[ "${specified_package_path}" =~ '/...'$ ]]; then
    specified_package_path=${specified_package_path::-4}
  fi

  if ! [ -d "${specified_package_path}" ]; then
    # Because k8s sets a localized $GOPATH for testing, seeing the actual
    # directory can be confusing. Instead, just show $GOPATH if it exists in the
    # $specified_package_path.
    local printable_package_path
    printable_package_path=${specified_package_path//${GOPATH}/\$\{GOPATH\}}
    kube::log::error "specified test path '${printable_package_path}' does not exist"

    if [ -d "${alternative_package_path}" ]; then
      kube::log::info "try changing \"${original_package_path}\" to \"${suggestion_package_path}\""
    fi
    exit 1
  fi
}

verifyPathsToPackagesUnderTest() {
  local packages_under_test=("$@")

  for package_path in "${packages_under_test[@]}"; do
    local local_package_path="${package_path}"
    local go_package_path="${GOPATH}/src/${package_path}"

    if [[ "${package_path:0:2}" == "./" ]] ; then
      verifyAndSuggestPackagePath "${local_package_path}" "${go_package_path}" "${package_path}" "${package_path:2}"
    else
      verifyAndSuggestPackagePath "${go_package_path}" "${local_package_path}" "${package_path}" "./${package_path}"
    fi
  done
}

produceJUnitXMLReport() {
  local -r junit_filename_prefix=$1
  if [[ -z "${junit_filename_prefix}" ]]; then
    return
  fi

  local junit_xml_filename
  junit_xml_filename="${junit_filename_prefix}.xml"

  if ! command -v gotestsum >/dev/null 2>&1; then
    kube::log::status "gotestsum not found; installing from hack/tools"
    (
      cd "${KUBE_ROOT}/hack/tools"
      # override only for installing tools (in subshell), if set
      if [ -n "${KUBE_HACK_TOOLS_GOTOOLCHAIN+x}" ]; then
        export GOTOOLCHAIN="${KUBE_HACK_TOOLS_GOTOOLCHAIN}";
      fi
      GO111MODULE=on go install gotest.tools/gotestsum
    )
  fi
  gotestsum --junitfile "${junit_xml_filename}" --raw-command cat "${junit_filename_prefix}"*.stdout
  if [[ ! ${KUBE_KEEP_VERBOSE_TEST_OUTPUT} =~ ^[yY]$ ]]; then
    rm "${junit_filename_prefix}"*.stdout
  fi

  if ! command -v prune-junit-xml >/dev/null 2>&1; then
    # TODO: is this really necessary to log? we're not every going to get this from the host are we?
    kube::log::status "prune-junit-xml not found; installing from cmd/prune-junit-xml"
    pushd "${KUBE_ROOT}/cmd/prune-junit-xml" >/dev/null
      GO111MODULE=on go install .
    popd >/dev/null
  fi
  prune-junit-xml "${junit_xml_filename}"

  kube::log::status "Saved JUnit XML test report to ${junit_xml_filename}"
}

runTests() {
  local junit_filename_prefix
  junit_filename_prefix=$(junitFilenamePrefix)

  verifyPathsToPackagesUnderTest "$@"

  # If we're not collecting coverage, run all requested tests with one 'go test'
  # command, which is much faster.
  if [[ ! ${KUBE_COVER} =~ ^[yY]$ ]]; then
    kube::log::status "Running tests without code coverage ${KUBE_RACE:+"and with ${KUBE_RACE}"}"
    go test "${goflags[@]:+${goflags[@]}}" \
     "${KUBE_TIMEOUT}" "${@}" \
     "${testargs[@]:+${testargs[@]}}" \
     | tee ${junit_filename_prefix:+"${junit_filename_prefix}.stdout"} \
     | grep --binary-files=text "${go_test_grep_pattern}" && rc=$? || rc=$?
    produceJUnitXMLReport "${junit_filename_prefix}"
    return ${rc}
  fi

  kube::log::status "Running tests with code coverage ${KUBE_RACE:+"and with ${KUBE_RACE}"}"

  # Create coverage report directories.
  if [[ -z "${KUBE_COVER_REPORT_DIR}" ]]; then
    cover_report_dir="/tmp/k8s_coverage/$(kube::util::sortable_date)"
  else
    cover_report_dir="${KUBE_COVER_REPORT_DIR}"
  fi
  cover_profile="coverage.out"  # Name for each individual coverage profile
  kube::log::status "Saving coverage output in '${cover_report_dir}'"
  mkdir -p "${@+${@/#/${cover_report_dir}/}}"

  # Run all specified tests, collecting coverage results. Go currently doesn't
  # support collecting coverage across multiple packages at once, so we must issue
  # separate 'go test' commands for each package and then combine at the end.
  # To speed things up considerably, we can at least use xargs -P to run multiple
  # 'go test' commands at once.
  # To properly parse the test results if generating a JUnit test report, we
  # must make sure the output from PARALLEL runs is not mixed. To achieve this,
  # we spawn a subshell for each PARALLEL process, redirecting the output to
  # separate files.

  # ignore paths:
  # vendor/k8s.io/code-generator/cmd/generator: is fragile when run under coverage, so ignore it for now.
  #                            https://github.com/kubernetes/kubernetes/issues/24967
  # vendor/k8s.io/client-go/1.4/rest: causes cover internal errors
  #                            https://github.com/golang/go/issues/16540
  cover_ignore_dirs="vendor/k8s.io/code-generator/cmd/generator|vendor/k8s.io/client-go/1.4/rest"
  for path in ${cover_ignore_dirs//|/ }; do
      echo -e "skipped\tk8s.io/kubernetes/${path}"
  done

  printf "%s\n" "${@}" \
    | grep -Ev ${cover_ignore_dirs} \
    | xargs -I{} -n 1 -P "${KUBE_COVERPROCS}" \
    bash -c "set -o pipefail; _pkg=\"\$0\"; _pkg_out=\${_pkg//\//_}; \
      go test ${goflags[*]:+${goflags[*]}} \
        ${KUBE_TIMEOUT} \
        -cover -covermode=\"${KUBE_COVERMODE}\" \
        -coverprofile=\"${cover_report_dir}/\${_pkg}/${cover_profile}\" \
        \"\${_pkg}\" \
        ${testargs[*]:+${testargs[*]}} \
      | tee ${junit_filename_prefix:+\"${junit_filename_prefix}-\$_pkg_out.stdout\"} \
      | grep \"${go_test_grep_pattern}\"" \
    {} \
    && test_result=$? || test_result=$?

  produceJUnitXMLReport "${junit_filename_prefix}"

  COMBINED_COVER_PROFILE="${cover_report_dir}/combined-coverage.out"
  {
    # The combined coverage profile needs to start with a line indicating which
    # coverage mode was used (set, count, or atomic). This line is included in
    # each of the coverage profiles generated when running 'go test -cover', but
    # we strip these lines out when combining so that there's only one.
    echo "mode: ${KUBE_COVERMODE}"

    # Include all coverage reach data in the combined profile, but exclude the
    # 'mode' lines, as there should be only one.
    while IFS='' read -r x; do
      grep -h -v "^mode:" < "${x}" || true
    done < <(find "${cover_report_dir}" -name "${cover_profile}")
  } >"${COMBINED_COVER_PROFILE}"

  coverage_html_file="${cover_report_dir}/combined-coverage.html"
  go tool cover -html="${COMBINED_COVER_PROFILE}" -o="${coverage_html_file}"
  kube::log::status "Combined coverage report: ${coverage_html_file}"

  return ${test_result}
}

reportCoverageToCoveralls() {
  if [[ ${KUBE_COVER} =~ ^[yY]$ ]] && [[ -x "${KUBE_GOVERALLS_BIN}" ]]; then
    kube::log::status "Reporting coverage results to Coveralls for service ${CI_NAME:-}"
    ${KUBE_GOVERALLS_BIN} -coverprofile="${COMBINED_COVER_PROFILE}" \
    ${CI_NAME:+"-service=${CI_NAME}"} \
    ${COVERALLS_REPO_TOKEN:+"-repotoken=${COVERALLS_REPO_TOKEN}"} \
      || true
  fi
}

checkFDs() {
  # several unittests panic when httptest cannot open more sockets
  # due to the low default files limit on OS X.  Warn about low limit.
  local fileslimit
  fileslimit="$(ulimit -n)"
  if [[ ${fileslimit} -lt 1000 ]]; then
    echo "WARNING: ulimit -n (files) should be at least 1000, is ${fileslimit}, may cause test failure";
  fi
}

checkFDs

runTests "$@"

# We might run the tests for multiple versions, but we want to report only
# one of them to coveralls. Here we report coverage from the last run.
reportCoverageToCoveralls
