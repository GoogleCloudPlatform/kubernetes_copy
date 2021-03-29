// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	authorizationv1 "github.com/openshift/api/authorization/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeRoleBindingRestrictions implements RoleBindingRestrictionInterface
type FakeRoleBindingRestrictions struct {
	Fake *FakeAuthorizationV1
	ns   string
}

var rolebindingrestrictionsResource = schema.GroupVersionResource{Group: "authorization.openshift.io", Version: "v1", Resource: "rolebindingrestrictions"}

var rolebindingrestrictionsKind = schema.GroupVersionKind{Group: "authorization.openshift.io", Version: "v1", Kind: "RoleBindingRestriction"}

// Get takes name of the roleBindingRestriction, and returns the corresponding roleBindingRestriction object, and an error if there is any.
func (c *FakeRoleBindingRestrictions) Get(ctx context.Context, name string, options v1.GetOptions) (result *authorizationv1.RoleBindingRestriction, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewGetAction(rolebindingrestrictionsResource, c.ns, name), &authorizationv1.RoleBindingRestriction{})

	if obj == nil {
		return nil, err
	}
	return obj.(*authorizationv1.RoleBindingRestriction), err
}

// List takes label and field selectors, and returns the list of RoleBindingRestrictions that match those selectors.
func (c *FakeRoleBindingRestrictions) List(ctx context.Context, opts v1.ListOptions) (result *authorizationv1.RoleBindingRestrictionList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewListAction(rolebindingrestrictionsResource, rolebindingrestrictionsKind, c.ns, opts), &authorizationv1.RoleBindingRestrictionList{})

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &authorizationv1.RoleBindingRestrictionList{ListMeta: obj.(*authorizationv1.RoleBindingRestrictionList).ListMeta}
	for _, item := range obj.(*authorizationv1.RoleBindingRestrictionList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested roleBindingRestrictions.
func (c *FakeRoleBindingRestrictions) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchAction(rolebindingrestrictionsResource, c.ns, opts))

}

// Create takes the representation of a roleBindingRestriction and creates it.  Returns the server's representation of the roleBindingRestriction, and an error, if there is any.
func (c *FakeRoleBindingRestrictions) Create(ctx context.Context, roleBindingRestriction *authorizationv1.RoleBindingRestriction, opts v1.CreateOptions) (result *authorizationv1.RoleBindingRestriction, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewCreateAction(rolebindingrestrictionsResource, c.ns, roleBindingRestriction), &authorizationv1.RoleBindingRestriction{})

	if obj == nil {
		return nil, err
	}
	return obj.(*authorizationv1.RoleBindingRestriction), err
}

// Update takes the representation of a roleBindingRestriction and updates it. Returns the server's representation of the roleBindingRestriction, and an error, if there is any.
func (c *FakeRoleBindingRestrictions) Update(ctx context.Context, roleBindingRestriction *authorizationv1.RoleBindingRestriction, opts v1.UpdateOptions) (result *authorizationv1.RoleBindingRestriction, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateAction(rolebindingrestrictionsResource, c.ns, roleBindingRestriction), &authorizationv1.RoleBindingRestriction{})

	if obj == nil {
		return nil, err
	}
	return obj.(*authorizationv1.RoleBindingRestriction), err
}

// Delete takes name of the roleBindingRestriction and deletes it. Returns an error if one occurs.
func (c *FakeRoleBindingRestrictions) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteAction(rolebindingrestrictionsResource, c.ns, name), &authorizationv1.RoleBindingRestriction{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeRoleBindingRestrictions) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewDeleteCollectionAction(rolebindingrestrictionsResource, c.ns, listOpts)

	_, err := c.Fake.Invokes(action, &authorizationv1.RoleBindingRestrictionList{})
	return err
}

// Patch applies the patch and returns the patched roleBindingRestriction.
func (c *FakeRoleBindingRestrictions) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *authorizationv1.RoleBindingRestriction, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceAction(rolebindingrestrictionsResource, c.ns, name, pt, data, subresources...), &authorizationv1.RoleBindingRestriction{})

	if obj == nil {
		return nil, err
	}
	return obj.(*authorizationv1.RoleBindingRestriction), err
}
