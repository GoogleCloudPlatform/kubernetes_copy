// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package proto

// ValueOrNil returns nil if has is false, or a pointer to a new variable
// containing the value returned by the specified getter.
//
// This function is similar to the wrappers (proto.Int32(), proto.String(),
// etc.), but is generic (works for any field type) and works with the hasser
// and getter of a field, as opposed to a value.
//
// This is convenient when populating builder fields.
//
// Example:
//
//	hop := attr.GetDirectHop()
//	injectedRoute := ripb.InjectedRoute_builder{
//	  Prefixes: route.GetPrefixes(),
//	  NextHop:  proto.ValueOrNil(hop.HasAddress(), hop.GetAddress),
//	}
func ValueOrNil[T any](has bool, getter func() T) *T {
	if !has {
		return nil
	}
	v := getter()
	return &v
}

// ValueOrDefault returns the protobuf message val if val is not nil, otherwise
// it returns a pointer to an empty val message.
//
// This function allows for translating code from the old Open Struct API to the
// new Opaque API.
//
// The old Open Struct API represented oneof fields with a wrapper struct:
//
//	var signedImg *accountpb.SignedImage
//	profile := &accountpb.Profile{
//		// The Avatar oneof will be set, with an empty SignedImage.
//		Avatar: &accountpb.Profile_SignedImage{signedImg},
//	}
//
// The new Opaque API treats oneof fields like regular fields, there are no more
// wrapper structs:
//
//	var signedImg *accountpb.SignedImage
//	profile := &accountpb.Profile{}
//	profile.SetSignedImage(signedImg)
//
// For convenience, the Opaque API also offers Builders, which allow for a
// direct translation of struct initialization. However, because Builders use
// nilness to represent field presence (but there is no non-nil wrapper struct
// anymore), Builders cannot distinguish between an unset oneof and a set oneof
// with nil message. The above code would need to be translated with help of the
// ValueOrDefault function to retain the same behavior:
//
//	var signedImg *accountpb.SignedImage
//	return &accountpb.Profile_builder{
//		SignedImage: proto.ValueOrDefault(signedImg),
//	}.Build()
func ValueOrDefault[T interface {
	*P
	Message
}, P any](val T) T {
	if val == nil {
		return T(new(P))
	}
	return val
}

// ValueOrDefaultBytes is like ValueOrDefault but for working with fields of
// type []byte.
func ValueOrDefaultBytes(val []byte) []byte {
	if val == nil {
		return []byte{}
	}
	return val
}
