# Kubernetes (K8s)

[![CII Best Practices](https://bestpractices.coreinfrastructure.org/projects/569/badge)](https://bestpractices.coreinfrastructure.org/projects/569) [![Go Report Card](https://goreportcard.com/badge/github.com/kubernetes/kubernetes)](https://goreportcard.com/report/github.com/kubernetes/kubernetes) ![GitHub release (latest SemVer)](https://img.shields.io/github/v/release/kubernetes/kubernetes?sort=semver)

<img src="https://github.com/kubernetes/kubernetes/raw/master/logo/logo.png" width="100">

----

Kubernetes, also known as K8s, is an open source system for managing [containerized applications]
across multiple hosts. It provides basic mechanisms for the deployment, maintenance,
and scaling of applications.

Kubernetes builds upon a decade and a half of experience at Google running
production workloads at scale using a system called [Borg],
combined with best-of-breed ideas and practices from the community.

Kubernetes is hosted by the Cloud Native Computing Foundation ([CNCF]).
If your company wants to help shape the evolution of
technologies that are container-packaged, dynamically scheduled,
and microservices-oriented, consider joining the CNCF.
For details about who's involved and how Kubernetes plays a role,
read the CNCF [announcement].

----

## To start using K8s

See our documentation on [kubernetes.io].

Take a free course on [Scalable Microservices with Kubernetes].

To use Kubernetes code as a library in other applications, see the [list of published components](https://git.k8s.io/kubernetes/staging/README.md).
Use of the `k8s.io/kubernetes` module or `k8s.io/kubernetes/...` packages as libraries is not supported.

## To start developing K8s

The [community repository] hosts all information about
building Kubernetes from source, how to contribute code
and documentation, who to contact about what, etc.

If you want to build Kubernetes right away there are two options:

##### You have a working [Go environment].

```
git clone https://github.com/kubernetes/kubernetes
cd kubernetes
make
```

##### You have a working [Docker environment].

```
git clone https://github.com/kubernetes/kubernetes
cd kubernetes
make quick-release
```

For the full story, head over to the [developer's documentation].

## Support

If you need support, start with the [troubleshooting guide],
and work your way through the process that we've outlined.

That said, if you have questions, reach out to us
[one way or another][communication].

[announcement]: https://cncf.io/news/announcement/2015/07/new-cloud-native-computing-foundation-drive-alignment-among-container
[Borg]: https://research.google.com/pubs/pub43438.html?authuser=1
[CNCF]: https://www.cncf.io/about
[communication]: https://git.k8s.io/community/communication
[community repository]: https://git.k8s.io/community
[containerized applications]: https://kubernetes.io/docs/concepts/overview/what-is-kubernetes/
[developer's documentation]: https://git.k8s.io/community/contributors/devel#readme
[Docker environment]: https://docs.docker.com/engine
[Go environment]: https://go.dev/doc/install
[kubernetes.io]: https://kubernetes.io
[Scalable Microservices with Kubernetes]: https://www.udacity.com/course/scalable-microservices-with-kubernetes--ud615
[troubleshooting guide]: https://kubernetes.io/docs/tasks/debug/

## Community Meetings 

The [Calendar](https://www.kubernetes.dev/resources/calendar/) has the list of all the meetings in the Kubernetes community in a single location.

## Adopters

The [User Case Studies](https://kubernetes.io/case-studies/) website has real-world use cases of organizations across industries that are deploying/migrating to Kubernetes.

## Governance 

Kubernetes project is governed by a framework of principles, values, policies and processes to help our community and constituents towards our shared goals.

The [Kubernetes Community](https://github.com/kubernetes/community/blob/master/governance.md) is the launching point for learning about how we organize ourselves.

The [Kubernetes Steering community repo](https://github.com/kubernetes/steering) is used by the Kubernetes Steering Committee, which oversees governance of the Kubernetes project.


## Roadmap 

The [Kubernetes Enhancements repo](https://github.com/kubernetes/enhancements) provides information about Kubernetes releases, as well as feature tracking and backlogs.
