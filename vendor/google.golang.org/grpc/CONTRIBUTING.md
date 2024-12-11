# How to contribute

We definitely welcome your patches and contributions to gRPC! Please read the gRPC
organization's [governance rules](https://github.com/grpc/grpc-community/blob/master/governance.md)
and [contribution guidelines](https://github.com/grpc/grpc-community/blob/master/CONTRIBUTING.md) before proceeding.

If you are new to GitHub, please start by reading [Pull Request howto](https://help.github.com/articles/about-pull-requests/)

## Legal requirements

In order to protect both you and ourselves, you will need to sign the
[Contributor License Agreement](https://identity.linuxfoundation.org/projects/cncf).

## Guidelines for Pull Requests
How to get your contributions merged smoothly and quickly.

- Create **small PRs** that are narrowly focused on **addressing a single
  concern**. We often times receive PRs that are trying to fix several things at
  a time, but only one fix is considered acceptable, nothing gets merged and
  both author's & review's time is wasted. Create more PRs to address different
  concerns and everyone will be happy.

- If you are searching for features to work on, issues labeled [Status: Help
  Wanted](https://github.com/grpc/grpc-go/issues?q=is%3Aissue+is%3Aopen+sort%3Aupdated-desc+label%3A%22Status%3A+Help+Wanted%22)
  is a great place to start. These issues are well-documented and usually can be
  resolved with a single pull request.

- If you are adding a new file, make sure it has the copyright message template
  at the top as a comment. You can copy over the message from an existing file
  and update the year.

- The grpc package should only depend on standard Go packages and a small number
  of exceptions. If your contribution introduces new dependencies which are NOT
  in the [list](https://godoc.org/google.golang.org/grpc?imports), you need a
  discussion with gRPC-Go authors and consultants.

- For speculative changes, consider opening an issue and discussing it first. If
  you are suggesting a behavioral or API change, consider starting with a [gRFC
  proposal](https://github.com/grpc/proposal).

- Provide a good **PR description** as a record of **what** change is being made
  and **why** it was made. Link to a GitHub issue if it exists.

- If you want to fix formatting or style, consider whether your changes are an
  obvious improvement or might be considered a personal preference. If a style
  change is based on preference, it likely will not be accepted. If it corrects
  widely agreed-upon anti-patterns, then please do create a PR and explain the
  benefits of the change.

- Unless your PR is trivial, you should expect there will be reviewer comments
  that you'll need to address before merging. We'll mark it as `Status: Requires
  Reporter Clarification` if we expect you to respond to these comments in a
  timely manner. If the PR remains inactive for 6 days, it will be marked as
  `stale` and automatically close 7 days after that if we don't hear back from
  you.

- Maintain **clean commit history** and use **meaningful commit messages**. PRs
  with messy commit history are difficult to review and won't be merged. Use
  `rebase -i upstream/master` to curate your commit history and/or to bring in
  latest changes from master (but avoid rebasing in the middle of a code
  review).

- Keep your PR up to date with upstream/master (if there are merge conflicts, we
  can't really merge your change).

- **All tests need to be passing** before your change can be merged. We
  recommend you **run tests locally** before creating your PR to catch breakages
  early on.
  - `./scripts/vet.sh` to catch vet errors
  - `go test -cpu 1,4 -timeout 7m ./...` to run the tests
  - `go test -race -cpu 1,4 -timeout 7m ./...` to run tests in race mode

- Exceptions to the rules can be made if there's a compelling reason for doing so.
