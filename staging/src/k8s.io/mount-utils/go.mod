// This is a generated file. Do not edit directly.

module k8s.io/mount-utils

go 1.22.0

require (
	github.com/moby/sys/mountinfo v0.6.2
	github.com/stretchr/testify v1.8.4
	golang.org/x/sys v0.18.0
	k8s.io/klog/v2 v2.120.1
	k8s.io/utils v0.0.0-20230726121419-3b25d923346b
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/go-logr/logr v1.4.1 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rogpeppe/go-internal v1.10.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace k8s.io/mount-utils => ../mount-utils

godebug default=go1.22
