# apimachinery

Scheme, typing, encoding, decoding, and conversion packages for Kubernetes and Kubernetes-like API objects.


## Purpose

This library is a shared dependency for servers and clients to work with Kubernetes API infrastructure without direct
type dependencies. Its first consumers are `k8s.io/kubernetes`, `k8s.io/client-go`, and `k8s.io/apiserver`.


## Compatibility

There are *NO compatibility guarantees* for this repository. It is in direct support of Kubernetes, so branches
will track Kubernetes and be compatible with that repo. As we more cleanly separate the layers, we will review the
compatibility guarantee.

The `kubernetes-1.x.y` tags are not to be referenced by golang packages and are the same exact code commit as the `v0.x.y` tags which can be used by golang to match the x.y specific version of Kubernetes.

## Where does it come from?

`apimachinery` is synced from https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apimachinery.
Code changes are made in that location, merged into `k8s.io/kubernetes` and later synced here.


## Things you should *NOT* do

 1. Add API types to this repo. This is for the machinery, not for the types.
 2. Directly modify any files under `pkg` in this repo. Those are driven from `k8s.io/kubernetes/staging/src/k8s.io/apimachinery`.
 3. Expect compatibility. This repo is direct support of Kubernetes and the API isn't yet stable enough for API guarantees.

