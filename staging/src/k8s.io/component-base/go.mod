// This is a generated file. Do not edit directly.

module k8s.io/component-base

go 1.15

require (
	github.com/blang/semver v3.5.1+incompatible
	github.com/go-logr/logr v0.2.0
	github.com/google/go-cmp v0.5.2
	github.com/matttproud/golang_protobuf_extensions v1.0.2-0.20181231171920-c182affec369 // indirect
	github.com/moby/term v0.0.0-20200915141129-7f0af18e79f2
	github.com/prometheus/client_golang v1.8.0
	github.com/prometheus/client_model v0.2.0
	github.com/prometheus/common v0.15.0
	github.com/prometheus/procfs v0.2.0
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.6.1
	go.uber.org/zap v1.13.0
	golang.org/x/tools v0.0.0-20201030143252-cf7a54d06671 // indirect
	honnef.co/go/tools v0.0.1-2020.1.4 // indirect
	k8s.io/apimachinery v0.0.0
	k8s.io/client-go v0.0.0
	k8s.io/klog/v2 v2.4.0
	k8s.io/utils v0.0.0-20201104234853-8146046b121e
)

replace (
	k8s.io/api => ../api
	k8s.io/apimachinery => ../apimachinery
	k8s.io/client-go => ../client-go
	k8s.io/component-base => ../component-base
)
