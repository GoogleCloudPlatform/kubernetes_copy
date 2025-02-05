# Verify audience scoped tokens with TokenReview

This example program demonstrates how to validate a token presented to a service
external to the kube-apiserver using the TokenReview API.

You can adopt the source code from this example to write programs that validate
audience-scoped service account tokens.

## Running this example

Make sure you have a Kubernetes cluster and `kubectl` is configured:

    kubectl get nodes

Compile this example on your workstation:

```
cd verify-audience-scoped-tokens-with-tokenreview
go build -o ./app
```

Now, run this application on your workstation with your local kubeconfig file:

```
# override required audiences with -target-audience flag
# or specify a kubeconfig file with flag
kubectl create token default --namespace default | ./app -kubeconfig=$HOME/.kube/config
```

Running this command will execute the following operations on your cluster:

1. **Create a new ServiceAccount token:** This will create a new JWT for the `default`
   ServiceAccount in the `default` namespace.
2. **Create a TokenReview request:** The TokenReview will be submitted to the apiserver
   which will validate the token against the requested `-target-audience` (more information
   on JWT audience claims can be found in [RFC 7519]().
3. **Inspect the TokenReview status in response:** Checking the `status.authenticated` and
   `status.error` fields allow you to understand whether your token is considered valid,
   and if not, what part of the validation failed.

[RFC 7519]: https://datatracker.ietf.org/doc/html/rfc7519#section-4.1.3

## Cleanup

Because tokens are ephemeral by nature and not persisted anywhere, there is no clean-up required.
