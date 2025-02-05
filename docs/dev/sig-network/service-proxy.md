# Service Proxying

Kube-proxy is the default implementation of service proxying in
Kubernetes, but it is possible for a network plugin to implement their
own proxy that is better or faster or that integrates better with the
technologies used in the rest of the network plugin.

In theory, it is also possible to write a new plugin-agnostic service
proxy implementation, but in practice it is not likely to be _fully_
plugin-agnostic. In particular, given the way that the semantics of
NetworkPolicy and Service interact, it may not be possible for some
network plugins to implement NetworkPolicy correctly without making
assumptions about the behavior of the service proxy.

(The rest of this document assumes that you already generally
understand [Kubernetes Services] and [the service proxy] from an
end-user point of view. In particular, it does not bother to explain
certain service features whose behavior on the service proxy side is
already fully understandable just from the end-user documentation.)

[Kubernetes Services]: https://kubernetes.io/docs/concepts/services-networking/
[the service proxy]: https://kubernetes.io/docs/reference/networking/virtual-ips/

## Note on Dual Stack, and Legacy vs "Modern" APIs

IPv4 and IPv6 proxying are generally entirely separate from each
other. While they _can_ be implemented together in a single proxy,
none of the semantics of service proxying _require_ that they be
implemented together. For example, if a dual-stack service has IPv4
endpoints but no IPv6 endpoints, then it is expected that its IPv4
cluster IP will work but its IPv6 cluster IP will not; the IPv4 rules
are completely unaware of the state of the IPv6 endpoints, and vice
versa.

(The proxy implementations in kube-proxy make use of a "metaproxier"
that takes one single-stack IPv4 proxy and one single-stack IPv6
proxy, and then passes only the matching EndpointSlice events to
each.)

Some legacy APIs, such as the `.spec.clusterIP` field of `Service`,
and all of `Endpoints`, do not handle dual stack, and can't be used in
the implementation of dual-stack proxying (even as one single-stack
half of a dual-stack metaproxier whole). Instead, modern service
proxies should look at `.spec.clusterIPs`, and use `EndpointSlice`
rather than `Endpoints`.

## Note on NAT

This document is written assuming that you will implement service
proxying by DNAT'ing (and sometimes SNAT'ing) connections. Any service
proxy implementation must do something at least vaguely equivalent to
this, but it may not always literally be NAT.

For example, the old userspace proxy did not NAT connections, but
instead accepted inbound connections to service IPs in userspace, and
then created new outbound connections to the endpoint IPs, and copied
data between the connections. (Thus, all connections passing through
the userspace proxy would arrive at their endpoint with a node IP as
their source IP.)

In other cases, some proxies may be able to do clever hacks to avoid
NAT'ing (especially SNAT'ing) that would otherwise be needed.

## Basic Service Handling

A service may accept connections to a variety of IPs and ports:

  - the service's `.spec.clusterIPs`, on any port listed in
    `.spec.ports[*].port`

  - any of the service's `.spec.externalIPs`, on any port listed in
    `.spec.ports[*].port`

  - any of the service's `.status.loadBalancer.ingress[*].ip` IPs, on
    any port listed in `.spec.ports[*].port`

  - any local IP, on any port listed in `.spec.ports[*].nodePort`

      - (Actually, _by default_ kube-proxy accepts NodePort
        connections on any IP, but it also allows you to restrict
        NodePorts to a specific subset of node IPs by passing the
        `--nodeport-addresses` flag. As of 1.31, kube-proxy now warns
        users who don't use `--nodeport-addresses`, suggesting they
        should use `--nodeport-addresses primary`, which makes
        kube-proxy only accept NodePort connections on the node's
        primary IP of each supported IP family.)

      - The iptables backend of kube-proxy even allows NodePort
        connections on `127.0.0.1` (but not `::1`), though the other
        backends do not, and this behavior is considered deprecated
        and can be disabled.

If a service has at least one usable endpoint, then connections to any
of the above destinations should be randomly DNAT'ed to one of the
usable endpoint IPs (with the destination port changed to the
corresponding `.spec.port[].targetPort` value, if that is set). If a
service does not have any usable endpoints, then connections to any of
the above destinations should be rejected (ie, actively refused with
an ICMP error; not simply dropped).

By default, the "usable" endpoints are ones that are "serving" and not
"terminating" (according to their conditions in their EndpointSlice's
`.endpoints[].conditions`). However, if a service has no serving,
non-terminating endpoints, but it does have serving, terminating
endpoints, then we use those instead. This behavior improves service
availability, particularly in the case of `externalTrafficPolicy:
Local` services.

## SNAT / Masquerading

Whenever possible, connections to a service should be only DNAT'ed and
not SNAT'ed, so as to preserve the original client IP.

However, if a packet has been DNAT'ed in the host network namespace on
Node X and then sent to some endpoint, then in general, the reply to
that packet must also pass through the host network namespace on Node
X on its way back to the original client, in order to be un-DNAT'ed.
This means that in any case where the normal routing of the reply
packet would _not_ pass back through the host network namespace on
Node X, then it will be necessary to SNAT the packet to Node X's IP,
to ensure that it does. Eg:

  - For "hairpin" connections (where a pod connects to a service IP
    which then redirects back to the same pod), the connection needs
    to be SNAT'ed because otherwise the pod would receive a packet
    with its own IP as the source IP, and so it would try to send the
    reply directly to itself, rather than sending it back to the host
    network namespace for un-DNAT'ing first.

  - Likewise, if the proxy supports localhost NodePort connections,
    and a host-network process tries to connect to a NodePort service
    via `127.0.0.1`, it is necessary to SNAT the packet, since
    otherwise the destination pod would think the packet came from its
    own `127.0.0.1`.

  - For connections where both the source and the destination are not
    local to the node (eg, an external client connecting to a NodePort
    on Node X which resolves to an endpoint pod on Node Y), then it is
    necessary to SNAT the connection, since otherwise the other node
    would try to send the reply packet directly back to the external
    client.

## Service IP Routing

### Cluster IPs

Service cluster IPs must be reachable from all pods and from all
nodes. It is up to the network plugin and service proxy implementation
to decide whether connections to cluster IPs are accepted from other
sources.

The network plugin and service proxy are responsible for ensuring that
pods and nodes can reach cluster IPs. For example, it might be
necessary to add a route to the service network CIDR(s) from pods or
nodes, to ensure that cluster IPs are not misinterpreted as being the
IPs of remote hosts on the Internet.

(When using the default kube-proxy implementation, network plugins are
required to ensure that cluster IP packets get routed to the host
network namespace on the node. Other service proxy implementations may
have different requirements.)

Cluster IPs are not expected to respond to ICMP pings.

### Load Balancer IPs

From a service proxy's point of view, there are two ways a
CloudProvider can implement LoadBalancer Services:

  1. "VIP mode", where the proxy redirects incoming connections to
     nodes without doing any rewriting (in particular, it preserves
     the load balancer IP as the destination IP rather than DNAT'ing
     it), thus allowing "Direct Server Return" (ie, the node can send
     the reply packets directly back to the original client rather
     than needing to pass them back through the load balancer, since
     the load balancer doesn't need to undo anything).

  2. "Proxy mode", where the load balancer works either by DNAT'ing
     incoming connections to NodePorts, or else by terminating
     incoming connections at the load balancer and then establishing a
     new connection from the load balancer to a NodePort.

Historically, the cloud provider indicated which mode it was using by
setting either `.status.loadBalancer.ingress[0].ip` (for VIP mode) or
`.status.loadBalancer.ingress[0].hostname` (for proxy mode). More
recently, this can also be indicated explicitly by setting
`.status.loadBalancer.ingress[0].ipMode` to either `"VIP"` or
`"Proxy"`. (Specifically, it can be set to `"Proxy"` when using the
`.ip` field to indicate Proxy mode; you cannot use VIP mode when using
`.hostname`.)

In Proxy mode, inbound load balancer connections will just appear as
NodePort connections, so the proxy can just treat the LoadBalancer
Service as though it was a NodePort Service.

For VIP mode load balancers, the proxy needs to create additional
rules to accept connections whose destination IP is the load balancer
IP rather than a node IP.

Unlike cluster IPs, load balancer IPs are considered to be external to
the cluster, and the service proxy does not "own" them in the way it
owns cluster IPs. The service proxy should not do anything with load
balancer IPs other than accepting/rejecting service connections that
are supposed to be accepted/rejected. In particular, it should not
make any attempts to change how other hosts deliver load balancer IP
traffic (eg, by sending ARP, ND, or BGP messages related to load
balancer IPs), and it should not intercept or respond to traffic that
is addressed to a load balancer IP but to a port which is not
associated with a Service of type LoadBalancer.

In some cases (notably with MetalLB), the load balancer IP may
actually be a secondary IP on one of the nodes, rather than an IP on
an external host. From the proxy's perspective though, this is not
really much different than the "normal" case, and in particular, even
if the load balancer IP is assigned to one node, other nodes should
still accept packets addressed to services using that load balancer
IP.

If a service has `.spec.loadBalancerSourceRanges` set, then
connections from load balancer IPs should only be accepted if the
source IP is in one of the indicated ranges; otherwise the connection
may be either dropped or rejected. (If the node's own primary IP is
listed in `.spec.loadBalancerSourceRanges`, then that implies that
connections from any local IP should be allowed, not just connections
specifically from the node's primary IP.)

Note that pods and nodes are allowed to connect to services via load
balancer IPs. Ideally, pod-to-load-balancer-IP connections should be
"short circuited" to the cluster IP by the service proxy, to ensure
that the source IP can be preserved. (If the connection was not
short-circuited, then normally the connection would end up SNAT'ed to
the node IP before being passed to the load balancer.)

### External IPs

For purposes of implementing a service proxy, external IPs are
essentially just a slightly-simplified form of VIP-mode load balancer
IPs. With external IPs, it is more common to encounter the case where
the IP is assigned to a specific node, but external IPs can also be
used where the IP is an external load balancer that supports Direct
Server Return.

As with load balancer IPs, pods and nodes are allowed to connect to
services via external IPs, and these connections may be short
circuited by the service proxy.

## "Local" Internal and External Traffic Policy

If a service has `.spec.internalTrafficPolicy` set to `"Local"`, then
"internal" traffic to the service (that is, traffic to the ClusterIPs)
should only be redirected to endpoint IPs on the node that the
connection originated from. If there are no endpoint IPs on that node
then internal connections to the service from that node should be
dropped.

If a service has `.spec.externalTrafficPolicy` set to `"Local"`, then
"external" traffic to the service (that is, traffic to NodePorts,
ExternalIPs, or Load Balancer IPs) should only be redirected to
endpoint IPs on the node where the connection first arrives in the
cluster. If there are no local endpoint IPs on that node then external
connections to the service via that node should be dropped.

(For external traffic policy in particular, it is important that
"wrongly-directed" traffic be _dropped_, not _rejected_, because there
is a race condition between when the proxy stops accepting new
connections for a service on a particular node versus when the load
balancer _notices_ that the proxy has stopped accepting new
connections. During that time, if the load balancer sends a connection
to a "bad" node, we drop the packets so that the client's network
stack will think there was just a transient network problem and retry
(hopefully getting directed to a valid node this time). If we rejected
the packets, the client would get back an error and might think the
service was actually unavailable.)

In the case of `externalTrafficPolicy: Local`, since the traffic is
guaranteed to not leave the node, there is no need to SNAT it, so the
original client IP can be preserved.

(There is a subtle interaction here with the "short circuit" behavior
of pod-to-load-balancer traffic: the behavior when a pod connects to a
load balancer IP should logically be the same whether the service
proxy short circuits it or not. Thus, when a pod connects to an
`externalTrafficPolicy: Local` service via its load balancer IP, the
connection must succeed _even if the pod is on a node with no
endpoints for that service_.)

## NodePort "Health" Checks

For `type: LoadBalancer` services with `externalTrafficPolicy: Local`,
the `.spec.healthCheckNodePort` field will be set. (The apiserver
fills it in with a random port if the user didn't specify it when
creating the service.) The proxy is expected to run an HTTP service on
this port which returns a certain JSON result, and more importantly,
which returns `200 OK` if there is at least one local endpoint for the
given service, or `503 Service Unavailable` if there are no local
endpoints.

This is used by CloudProviders when programming cloud load balancers
for `externalTrafficPolicy: Local` services: the cloud is told that
the service is available on all nodes, but that it should use the
`HealthCheckNodePort` server on each node to determine whether that
node is healthy or not. Since the service returns `200 OK` when there
is a local endpoint, and `503 Service Unavailable` when there is not,
this means that the cloud load balancer will end up only sending
traffic to nodes that have local endpoints, and thus those nodes can
safely accept the traffic without SNAT'ing it because they will never
need to bounce it to another node.

Note in particular that the `HealthCheckNodePort` *does not* indicate
whether the service itself is healthy; all it indicates is whether a
particular node can accept a connection to the service without needing
to redirect the connection to a different node. It is not really a
"health check" server at all, it just gets hooked up to the "health
check" feature of the cloud load balancer as a way to get the load
balancer to ignore the nodes we want it to ignore at any given time.

This is implemented by
`k8s.io/kubernetes/pkg/proxy/healthcheck/service_health.go`, and other
service proxy implementations should probably just copy that code
exactly for now. (It is not clear if any cloud load balancer
implementations actually parse the returned JSON?)

Note that setting `.spec.allocateLoadBalancerNodePorts` to `false` for a
LoadBalancer type service does not have any effect on the allocation of
`.spec.healthCheckNodePort`.

## Service Affinity

If a service has `.spec.sessionAffinity` set to `ClientIP`, then the
proxy should try to ensure that repeated connections from the same
client within a certain time all get redirected to the same endpoint.

The default affinity timeout for the iptables proxy is 3 hours. This
can be overridden via
`.spec.sessionAffinityConfig.clientIP.timeoutSeconds`. In theory,
other proxy implementations behave exactly the same way, but in
practice they may have different default timeouts, and may not allow
the timeout to be overridden (or may only allow overriding it within a
certain range).

## Ignored Services/Endpoints

If a Service has the label `service.kubernetes.io/service-proxy-name`
with a value that the proxy does not recognize as being its own name,
then the proxy should ignore the service completely, as it is being
handled by some other proxy.

Likewise, if an EndpointSlice has the label
`service.kubernetes.io/headless`, then this provides a hint that the
proxy doesn't need to look at it, because the corresponding Service
has no `.spec.clusterIPs` and so no proxy rules will be generated for
it.

(For efficiency, kube-proxy sets up its informers with a
`LabelSelector` that filters out these services/endpoints before any
kube-proxy code even sees them.)

## Topology

Proxies that implement the `TopologyAwareHints` feature should filter
out endpoint IPs from all services according to the rules for that
feature.

There is code in `k8s.io/kubernetes/pkg/proxy/topology.go` to
implement this.

## Port Listeners

In the past, for service NodePorts, as well as external IPs where the
IP is assigned to that node, kube-proxy would attempt to open a
listening socket on `:${node_port}` or `${external_ip}:${port}`, in an
attempt to ensure that it was not possible to create a service rule
that steals traffic away from a running server on the node, and
likewise that a server couldn't later be started that wants to listen on
the IP/port that has been claimed by a service rule.

There were various problems with this behavior though (both in concept
and in implementation) and this is no longer done. Instead, kube-proxy
simply assumes that nodes will not try to run services on ports in the
NodePort range.

## Other Lessons

These are lessons learned from the iptables and ipvs proxies which may
or may not apply to other proxy implementations...

### UDP conntrack Cleanup

If your proxy results in kernel conntrack entries being created (eg
for NAT'ed connections) then you will probably need to manually delete
certain conntrack entries for UDP services when those services change,
so that new packets don't get matched to a stale conntrack entry.
(This is not necessary for TCP or SCTP connections.)

In particular:

  - If a service loses UDP endpoints, you must delete any conntrack
    rules pointing to the deleted endpoint IPs with a source of:

      - the service's `.spec.clusterIPs`
      - the service's `.spec.externalIPs`
      - any of the service's `.status.loadBalancer.ingress[*].ip`
      - any of the service's `.spec.ports[*].nodePort` ports

  - If a UDP service that previously had no endpoints now has
    endpoints, you must delete any conntrack entries that are caching
    a "reject" rule for a connection to:

      - the service's `.spec.externalIPs`
      - any of the service's `.status.loadBalancer.ingress[*].ip`
      - any of the service's `.spec.ports[*].nodePort` ports

`k8s.io/kubernetes/pkg/proxy/endpoints.go` has code to (among other
things) figure out which services/endpoints meet those criteria, and
`k8s.io/kubernetes/pkg/util/conntrack` has code to perform the actual
removal.
