BRANCH=`git rev-parse --abbrev-ref HEAD`
COMMIT=`git rev-parse --short HEAD`
GOLDFLAGS="-X main.branch $(BRANCH) -X main.commit $(COMMIT)"
GOFILES = $(shell find . -name \*.go)

TESTFLAGS_RACE=-race=false
ifdef ENABLE_RACE
	TESTFLAGS_RACE=-race=true
endif

TESTFLAGS_CPU=
ifdef CPU
	TESTFLAGS_CPU=-cpu=$(CPU)
endif
TESTFLAGS = $(TESTFLAGS_RACE) $(TESTFLAGS_CPU) $(EXTRA_TESTFLAGS)

TESTFLAGS_TIMEOUT=30m
ifdef TIMEOUT
	TESTFLAGS_TIMEOUT=$(TIMEOUT)
endif

TESTFLAGS_ENABLE_STRICT_MODE=false
ifdef ENABLE_STRICT_MODE
	TESTFLAGS_ENABLE_STRICT_MODE=$(ENABLE_STRICT_MODE)
endif

.EXPORT_ALL_VARIABLES:
TEST_ENABLE_STRICT_MODE=${TESTFLAGS_ENABLE_STRICT_MODE}

.PHONY: fmt
fmt:
	@echo "Verifying gofmt, failures can be fixed with ./scripts/fix.sh"
	@!(gofmt -l -s -d ${GOFILES} | grep '[a-z]')

	@echo "Verifying goimports, failures can be fixed with ./scripts/fix.sh"
	@!(go run golang.org/x/tools/cmd/goimports@latest -l -d ${GOFILES} | grep '[a-z]')

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: test
test:
	@echo "hashmap freelist test"
	BBOLT_VERIFY=all TEST_FREELIST_TYPE=hashmap go test -v ${TESTFLAGS} -timeout ${TESTFLAGS_TIMEOUT}
	BBOLT_VERIFY=all TEST_FREELIST_TYPE=hashmap go test -v ${TESTFLAGS} ./internal/...
	BBOLT_VERIFY=all TEST_FREELIST_TYPE=hashmap go test -v ${TESTFLAGS} ./cmd/bbolt

	@echo "array freelist test"
	BBOLT_VERIFY=all TEST_FREELIST_TYPE=array go test -v ${TESTFLAGS} -timeout ${TESTFLAGS_TIMEOUT}
	BBOLT_VERIFY=all TEST_FREELIST_TYPE=array go test -v ${TESTFLAGS} ./internal/...
	BBOLT_VERIFY=all TEST_FREELIST_TYPE=array go test -v ${TESTFLAGS} ./cmd/bbolt

.PHONY: coverage
coverage:
	@echo "hashmap freelist test"
	TEST_FREELIST_TYPE=hashmap go test -v -timeout ${TESTFLAGS_TIMEOUT} \
		-coverprofile cover-freelist-hashmap.out -covermode atomic

	@echo "array freelist test"
	TEST_FREELIST_TYPE=array go test -v -timeout ${TESTFLAGS_TIMEOUT} \
		-coverprofile cover-freelist-array.out -covermode atomic

BOLT_CMD=bbolt

build:
	go build -o bin/${BOLT_CMD} ./cmd/${BOLT_CMD}

.PHONY: clean
clean: # Clean binaries
	rm -f ./bin/${BOLT_CMD}

.PHONY: gofail-enable
gofail-enable: install-gofail
	gofail enable .

.PHONY: gofail-disable
gofail-disable: install-gofail
	gofail disable .

.PHONY: install-gofail
install-gofail:
	go install go.etcd.io/gofail

.PHONY: test-failpoint
test-failpoint:
	@echo "[failpoint] hashmap freelist test"
	BBOLT_VERIFY=all TEST_FREELIST_TYPE=hashmap go test -v ${TESTFLAGS} -timeout 30m ./tests/failpoint

	@echo "[failpoint] array freelist test"
	BBOLT_VERIFY=all TEST_FREELIST_TYPE=array go test -v ${TESTFLAGS} -timeout 30m ./tests/failpoint

.PHONY: test-robustness # Running robustness tests requires root permission for now
# TODO: Remove sudo once we fully migrate to the prow infrastructure
test-robustness: gofail-enable build
	sudo env PATH=$$PATH go test -v ${TESTFLAGS} ./tests/dmflakey -test.root
	sudo env PATH=$(PWD)/bin:$$PATH go test -v ${TESTFLAGS} ${ROBUSTNESS_TESTFLAGS} ./tests/robustness -test.root

.PHONY: test-benchmark-compare
# Runs benchmark tests on the current git ref and the given REF, and compares
# the two.
test-benchmark-compare: install-benchstat
	@git fetch
	./scripts/compare_benchmarks.sh $(REF)

.PHONY: install-benchstat
install-benchstat:
	go install golang.org/x/perf/cmd/benchstat@latest
