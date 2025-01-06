# Versioning

This document describes the versioning policy for this module.
This policy is designed so the following goals can be achieved.

**Users are provided a codebase of value that is stable and secure.**

## Policy

* Versioning of this module will be idiomatic of a Go project using [Go modules](https://github.com/golang/go/wiki/Modules).
  * [Semantic import versioning](https://github.com/golang/go/wiki/Modules#semantic-import-versioning) will be used.
    * Versions will comply with [semver 2.0](https://semver.org/spec/v2.0.0.html).
    * Any `v2` or higher version of this module will be included as a `/vN` at the end of the module path used in `go.mod` files and in the package import path.

* GitHub releases will be made for all releases.
