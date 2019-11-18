// This is a generated file. Do not edit directly.

module k8s.io/cluster-bootstrap

go 1.12

require (
	github.com/stretchr/testify v1.3.0
	golang.org/x/crypto v0.0.0-20190820162420-60c769a6c586 // indirect
	gopkg.in/square/go-jose.v2 v2.2.2
	k8s.io/api v0.0.0
	k8s.io/apimachinery v0.0.0
	k8s.io/klog v1.0.0
)

replace (
	golang.org/x/sys => golang.org/x/sys v0.0.0-20190813064441-fde4db37ae7a // pinned to release-branch.go1.13
	golang.org/x/tools => golang.org/x/tools v0.0.0-20190821162956-65e3620a7ae7 // pinned to release-branch.go1.13
	k8s.io/api => ../api
	k8s.io/apimachinery => ../apimachinery
	k8s.io/cluster-bootstrap => ../cluster-bootstrap
	sigs.k8s.io/structured-merge-diff => github.com/jennybuckley/structured-merge-diff v0.0.0-20191115224326-281a8914c6c4
)
