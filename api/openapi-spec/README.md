# Kubernetes's OpenAPI Specification

This folder contains an [OpenAPI specification](https://github.com/OAI/OpenAPI-Specification) for Kubernetes API.

## Vendor Extensions

Kubernetes extends OpenAPI using these extensions. Note the version that
extensions have been added.

### `x-kubernetes-group-version-kind`

Operations and Definitions may have `x-kubernetes-group-version-kind` if they
are associated with a [kubernetes resource](https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources).


For example:

``` json
"paths": {
    ...
    "/api/v1/namespaces/{namespace}/pods/{name}": {
        ...
        "get": {
        ...
            "x-kubernetes-group-version-kind": {
            "group": "",
            "version": "v1",
            "kind": "Pod"
            }
        }
    }
}
```

### `x-kubernetes-action`

Operations and Definitions may have `x-kubernetes-action` if they
are associated with a [kubernetes resource](https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources).
Action can be one of `get`, `list`, `put`, `patch`, `post`, `delete`, `deletecollection`, `watch`, `watchlist`, `proxy`, or `connect`.


For example:

``` json
"paths": {
    ...
    "/api/v1/namespaces/{namespace}/pods/{name}": {
        ...
        "get": {
        ...
            "x-kubernetes-action": "list"
        }
    }
}
```

### `x-kubernetes-patch-strategy` and `x-kubernetes-patch-merge-key`

Some of the definitions may have these extensions. For more information about PatchStrategy and PatchMergeKey see
[strategic-merge-patch](https://git.k8s.io/community/contributors/devel/sig-api-machinery/strategic-merge-patch.md).

### `x-kubernetes-list-map-keys`, `x-kubernetes-list-type` and  `x-kubernetes-map-type`

`x-kubernetes-list-map-keys` and `x-kubernetes-list-type` are used in array fields to specify list types and unique keys. `x-kubernetes-map-type` defines the type of map, often used to specify behavior for complex map structures. For more information see [Merge Strategy](https://kubernetes.io/docs/reference/using-api/server-side-apply/#merge-strategy).

### Extensions mainly used in CustomResourceDefinitions
The following extensions are used in CustomResourceDefinitions. For more information see [Extend the Kubernetes API with CustomResourceDefinitions](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions)

* `x-kubernetes-preserve-unknown-fields` indicates that unknown fields should be preserved when unmarshaling data for this type. When set to true, Kubernetes will retain fields that are not specified in the OpenAPI schema. 
* `x-kubernetes-embedded-resource` specifies that a field is an embedded Kubernetes resource object. 
* `x-kubernetes-int-or-string` designates that a field can accept either an integer or a string. 
* `x-kubernetes-validations` allows additional JSON Schema validation rules on custom resource fields. 