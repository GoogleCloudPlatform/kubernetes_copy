# OAuth2 for Go

[![Go Reference](https://pkg.go.dev/badge/golang.org/x/oauth2.svg)](https://pkg.go.dev/golang.org/x/oauth2)
[![Build Status](https://travis-ci.org/golang/oauth2.svg?branch=master)](https://travis-ci.org/golang/oauth2)

oauth2 package contains a client implementation for OAuth 2.0 spec.

See pkg.go.dev for further documentation and examples.

* [pkg.go.dev/golang.org/x/oauth2](https://pkg.go.dev/golang.org/x/oauth2)
* [pkg.go.dev/golang.org/x/oauth2/google](https://pkg.go.dev/golang.org/x/oauth2/google)

## Policy for new endpoints

We no longer accept new provider-specific packages in this repo if all
they do is add a single endpoint variable. If you just want to add a
single endpoint, add it to the
[pkg.go.dev/golang.org/x/oauth2/endpoints](https://pkg.go.dev/golang.org/x/oauth2/endpoints)
package.

## Report Issues / Send Patches

The main issue tracker for the oauth2 repository is located at
https://github.com/golang/oauth2/issues.

This repository uses Gerrit for code changes. To learn how to submit changes to
this repository, see https://go.dev/doc/contribute.

The git repository is https://go.googlesource.com/oauth2.

Note:

* Excluding trivial changes, all contributions should be connected to an existing issue.
* API changes must go through the [change proposal process](https://go.dev/s/proposal-process) before they can be accepted.
* The code owners are listed at [dev.golang.org/owners](https://dev.golang.org/owners#:~:text=x/oauth2).
