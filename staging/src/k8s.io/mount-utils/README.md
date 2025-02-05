## Purpose

This repository defines an interface to mounting filesystems to be consumed by
various Kubernetes and out-of-tree CSI components.

Consumers of this repository can make use of functions like 'Mount' to mount
source to target as fstype with given options, 'Unmount' to unmount a target.
Other useful functions include 'List' all mounted file systems and find all
mount references to a path using 'GetMountRefs'

## Dependencies

This repository depends on the following cli tools

Linux platform:
* systemd-run (optional)
* mount
* umount
* fsck
* mkfs.ext*
* mkfs.xfs
* blkid
* resize2fs
* xfs_growfs
* btrfs
* blockdev
* dumpe2fs
* xfs_io

Windows platform:
* cmd
* mklink
* rmdir
* powershell

You can install these dependencies using the following commands in dockerfile:

```shell
# Install dependencies

FROM --platform=${TARGETARCH} debian:12.5 AS tools

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    mount \
    udev \
    btrfs-progs \
    e2fsprogs \
    xfsprogs \
    util-linux \
    rsync

COPY csi-deps.sh /csi-deps.sh
RUN /csi-deps.sh

# Build the final image

FROM --platform=${TARGETARCH} scratch AS final

COPY --from=gcr.io/distroless/base-debian12 . .
COPY --from=tools /dest /
## You csi driver binary
## COPY --from=csi-build /bin /bin

# Check if the dependencies are installed correctly

FROM --platform=${TARGETARCH} final AS check-tools

COPY --from=tools /bin/sh /bin/sh
COPY csi-deps-check.sh /csi-deps-check.sh

SHELL ["/bin/sh"]
RUN /csi-deps-check.sh
```

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community
page](http://kubernetes.io/community/).

You can reach the maintainers of this repository at:

- Slack: #sig-storage (on https://kubernetes.slack.com -- get an
  invite at slack.kubernetes.io)
- Mailing List:
  https://groups.google.com/forum/#!forum/kubernetes-sig-storage

### Code of Conduct

Participation in the Kubernetes community is governed by the [Kubernetes
Code of Conduct](code-of-conduct.md).

### Contibution Guidelines

See [CONTRIBUTING.md](CONTRIBUTING.md) for more information.
