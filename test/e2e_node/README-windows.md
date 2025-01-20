# Windoes Node End-To-End (e2e) tests

Node e2e tests are component tests meant for testing the Kubelet code on a custom host environment, they are now supported on the Windows platform and can be executed locally, with remote execution planned as the next step.

*Note: There is no scheduler running. The e2e tests have to do manual scheduling, e.g. by using `framework.PodClient`.*

# Running tests

## Locally

Why run tests *locally*? It is much faster than running tests remotely.

Prerequisites:
- [containerd](https://github.com/containerd/containerd/blob/main/docs/getting-started.md#installing-containerd-on-windows), 
containerd will need to be ran with admin privilege
- Working CNI
  - Ensure that you have a valid CNI configuration. For testing purposes, a [nat](https://www.jamessturtevant.com/posts/Windows-Containers-on-Windows-10-without-Docker-using-Containerd/) configuration should work. 

From the Kubernetes base directory, run with admin privilege:

```cmd
go build -o /test/e2e_node/_output/kubelet.exe .\cmd\kubelet\ 

go test ./test/e2e_node -c 

 .\e2e_node.test.exe --bearer-token=vQIYfdCt7wIFOZtO --test.v --test.paniconexit0 --container-runtime-endpoint "npipe://./pipe/containerd-containerd" --prepull-images=false --ginkgo.focus "when creating a windows static pod" --k8s-bin-dir ./test/e2e_node/_output/
```

It will run the specified windows e2e node tests, which will in turn:
- Build the Kuberlet source code
- Start a local instance of *etcd*
- Start a local instance of *kube-apiserver*
- Start a local instance of *kubelet*
- Run the test using the locally started processes
- Output the test results to STDOUT
- Stop *kubelet*, *kube-apiserver*, and *etcd*

