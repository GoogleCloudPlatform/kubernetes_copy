// +build !ignore_autogenerated

/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file was autogenerated by deepcopy-gen. Do not edit it manually!

package v1alpha1

import (
	conversion "k8s.io/kubernetes/pkg/conversion"
	runtime "k8s.io/kubernetes/pkg/runtime"
	reflect "reflect"
)

func init() {
	SchemeBuilder.Register(RegisterDeepCopies)
}

// RegisterDeepCopies adds deep-copy functions to the given scheme. Public
// to allow building arbitrary schemes.
func RegisterDeepCopies(scheme *runtime.Scheme) error {
	return scheme.AddGeneratedDeepCopyFuncs(
		conversion.GeneratedDeepCopyFunc{Fn: DeepCopy_v1alpha1_API, InType: reflect.TypeOf(&API{})},
		conversion.GeneratedDeepCopyFunc{Fn: DeepCopy_v1alpha1_ClusterInfo, InType: reflect.TypeOf(&ClusterInfo{})},
		conversion.GeneratedDeepCopyFunc{Fn: DeepCopy_v1alpha1_Discovery, InType: reflect.TypeOf(&Discovery{})},
		conversion.GeneratedDeepCopyFunc{Fn: DeepCopy_v1alpha1_Etcd, InType: reflect.TypeOf(&Etcd{})},
		conversion.GeneratedDeepCopyFunc{Fn: DeepCopy_v1alpha1_MasterConfiguration, InType: reflect.TypeOf(&MasterConfiguration{})},
		conversion.GeneratedDeepCopyFunc{Fn: DeepCopy_v1alpha1_Networking, InType: reflect.TypeOf(&Networking{})},
		conversion.GeneratedDeepCopyFunc{Fn: DeepCopy_v1alpha1_NodeConfiguration, InType: reflect.TypeOf(&NodeConfiguration{})},
		conversion.GeneratedDeepCopyFunc{Fn: DeepCopy_v1alpha1_Secrets, InType: reflect.TypeOf(&Secrets{})},
	)
}

func DeepCopy_v1alpha1_API(in interface{}, out interface{}, c *conversion.Cloner) error {
	{
		in := in.(*API)
		out := out.(*API)
		if in.AdvertiseAddresses != nil {
			in, out := &in.AdvertiseAddresses, &out.AdvertiseAddresses
			*out = make([]string, len(*in))
			copy(*out, *in)
		} else {
			out.AdvertiseAddresses = nil
		}
		if in.ExternalDNSNames != nil {
			in, out := &in.ExternalDNSNames, &out.ExternalDNSNames
			*out = make([]string, len(*in))
			copy(*out, *in)
		} else {
			out.ExternalDNSNames = nil
		}
		out.BindPort = in.BindPort
		return nil
	}
}

func DeepCopy_v1alpha1_ClusterInfo(in interface{}, out interface{}, c *conversion.Cloner) error {
	{
		in := in.(*ClusterInfo)
		out := out.(*ClusterInfo)
		out.TypeMeta = in.TypeMeta
		if in.CertificateAuthorities != nil {
			in, out := &in.CertificateAuthorities, &out.CertificateAuthorities
			*out = make([]string, len(*in))
			copy(*out, *in)
		} else {
			out.CertificateAuthorities = nil
		}
		if in.Endpoints != nil {
			in, out := &in.Endpoints, &out.Endpoints
			*out = make([]string, len(*in))
			copy(*out, *in)
		} else {
			out.Endpoints = nil
		}
		return nil
	}
}

func DeepCopy_v1alpha1_Discovery(in interface{}, out interface{}, c *conversion.Cloner) error {
	{
		in := in.(*Discovery)
		out := out.(*Discovery)
		out.BindPort = in.BindPort
		return nil
	}
}

func DeepCopy_v1alpha1_Etcd(in interface{}, out interface{}, c *conversion.Cloner) error {
	{
		in := in.(*Etcd)
		out := out.(*Etcd)
		if in.Endpoints != nil {
			in, out := &in.Endpoints, &out.Endpoints
			*out = make([]string, len(*in))
			copy(*out, *in)
		} else {
			out.Endpoints = nil
		}
		out.CAFile = in.CAFile
		out.CertFile = in.CertFile
		out.KeyFile = in.KeyFile
		return nil
	}
}

func DeepCopy_v1alpha1_MasterConfiguration(in interface{}, out interface{}, c *conversion.Cloner) error {
	{
		in := in.(*MasterConfiguration)
		out := out.(*MasterConfiguration)
		out.TypeMeta = in.TypeMeta
		if err := DeepCopy_v1alpha1_Secrets(&in.Secrets, &out.Secrets, c); err != nil {
			return err
		}
		if err := DeepCopy_v1alpha1_API(&in.API, &out.API, c); err != nil {
			return err
		}
		if err := DeepCopy_v1alpha1_Etcd(&in.Etcd, &out.Etcd, c); err != nil {
			return err
		}
		out.Discovery = in.Discovery
		out.Networking = in.Networking
		out.KubernetesVersion = in.KubernetesVersion
		out.CloudProvider = in.CloudProvider
		return nil
	}
}

func DeepCopy_v1alpha1_Networking(in interface{}, out interface{}, c *conversion.Cloner) error {
	{
		in := in.(*Networking)
		out := out.(*Networking)
		out.ServiceSubnet = in.ServiceSubnet
		out.PodSubnet = in.PodSubnet
		out.DNSDomain = in.DNSDomain
		return nil
	}
}

func DeepCopy_v1alpha1_NodeConfiguration(in interface{}, out interface{}, c *conversion.Cloner) error {
	{
		in := in.(*NodeConfiguration)
		out := out.(*NodeConfiguration)
		out.TypeMeta = in.TypeMeta
		if in.MasterAddresses != nil {
			in, out := &in.MasterAddresses, &out.MasterAddresses
			*out = make([]string, len(*in))
			copy(*out, *in)
		} else {
			out.MasterAddresses = nil
		}
		if err := DeepCopy_v1alpha1_Secrets(&in.Secrets, &out.Secrets, c); err != nil {
			return err
		}
		out.APIPort = in.APIPort
		out.DiscoveryPort = in.DiscoveryPort
		return nil
	}
}

func DeepCopy_v1alpha1_Secrets(in interface{}, out interface{}, c *conversion.Cloner) error {
	{
		in := in.(*Secrets)
		out := out.(*Secrets)
		out.GivenToken = in.GivenToken
		out.TokenID = in.TokenID
		if in.Token != nil {
			in, out := &in.Token, &out.Token
			*out = make([]byte, len(*in))
			copy(*out, *in)
		} else {
			out.Token = nil
		}
		out.BearerToken = in.BearerToken
		return nil
	}
}
