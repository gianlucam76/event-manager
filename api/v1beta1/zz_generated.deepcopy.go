//go:build !ignore_autogenerated

/*
Copyright 2023. projectsveltos.io. All rights reserved.

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

// Code generated by controller-gen. DO NOT EDIT.

package v1beta1

import (
	v1 "k8s.io/api/core/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	apiv1beta1 "github.com/projectsveltos/addon-controller/api/v1beta1"
	libsveltosapiv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EventTrigger) DeepCopyInto(out *EventTrigger) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EventTrigger.
func (in *EventTrigger) DeepCopy() *EventTrigger {
	if in == nil {
		return nil
	}
	out := new(EventTrigger)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *EventTrigger) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EventTriggerList) DeepCopyInto(out *EventTriggerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]EventTrigger, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EventTriggerList.
func (in *EventTriggerList) DeepCopy() *EventTriggerList {
	if in == nil {
		return nil
	}
	out := new(EventTriggerList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *EventTriggerList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EventTriggerSpec) DeepCopyInto(out *EventTriggerSpec) {
	*out = *in
	in.SourceClusterSelector.DeepCopyInto(&out.SourceClusterSelector)
	if in.ClusterSetRefs != nil {
		in, out := &in.ClusterSetRefs, &out.ClusterSetRefs
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	in.DestinationClusterSelector.DeepCopyInto(&out.DestinationClusterSelector)
	if in.DestinationCluster != nil {
		in, out := &in.DestinationCluster, &out.DestinationCluster
		*out = new(v1.ObjectReference)
		**out = **in
	}
	if in.ConfigMapGenerator != nil {
		in, out := &in.ConfigMapGenerator, &out.ConfigMapGenerator
		*out = make([]GeneratorReference, len(*in))
		copy(*out, *in)
	}
	if in.SecretGenerator != nil {
		in, out := &in.SecretGenerator, &out.SecretGenerator
		*out = make([]GeneratorReference, len(*in))
		copy(*out, *in)
	}
	if in.MaxUpdate != nil {
		in, out := &in.MaxUpdate, &out.MaxUpdate
		*out = new(intstr.IntOrString)
		**out = **in
	}
	if in.DependsOn != nil {
		in, out := &in.DependsOn, &out.DependsOn
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.TemplateResourceRefs != nil {
		in, out := &in.TemplateResourceRefs, &out.TemplateResourceRefs
		*out = make([]apiv1beta1.TemplateResourceRef, len(*in))
		copy(*out, *in)
	}
	if in.PolicyRefs != nil {
		in, out := &in.PolicyRefs, &out.PolicyRefs
		*out = make([]apiv1beta1.PolicyRef, len(*in))
		copy(*out, *in)
	}
	if in.HelmCharts != nil {
		in, out := &in.HelmCharts, &out.HelmCharts
		*out = make([]apiv1beta1.HelmChart, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.KustomizationRefs != nil {
		in, out := &in.KustomizationRefs, &out.KustomizationRefs
		*out = make([]apiv1beta1.KustomizationRef, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.ValidateHealths != nil {
		in, out := &in.ValidateHealths, &out.ValidateHealths
		*out = make([]libsveltosapiv1beta1.ValidateHealth, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.Patches != nil {
		in, out := &in.Patches, &out.Patches
		*out = make([]libsveltosapiv1beta1.Patch, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.DriftExclusions != nil {
		in, out := &in.DriftExclusions, &out.DriftExclusions
		*out = make([]libsveltosapiv1beta1.DriftExclusion, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.ExtraLabels != nil {
		in, out := &in.ExtraLabels, &out.ExtraLabels
		*out = make(map[string]string, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
	if in.ExtraAnnotations != nil {
		in, out := &in.ExtraAnnotations, &out.ExtraAnnotations
		*out = make(map[string]string, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EventTriggerSpec.
func (in *EventTriggerSpec) DeepCopy() *EventTriggerSpec {
	if in == nil {
		return nil
	}
	out := new(EventTriggerSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EventTriggerStatus) DeepCopyInto(out *EventTriggerStatus) {
	*out = *in
	if in.MatchingClusterRefs != nil {
		in, out := &in.MatchingClusterRefs, &out.MatchingClusterRefs
		*out = make([]v1.ObjectReference, len(*in))
		copy(*out, *in)
	}
	if in.DestinationMatchingClusterRefs != nil {
		in, out := &in.DestinationMatchingClusterRefs, &out.DestinationMatchingClusterRefs
		*out = make([]v1.ObjectReference, len(*in))
		copy(*out, *in)
	}
	if in.ClusterInfo != nil {
		in, out := &in.ClusterInfo, &out.ClusterInfo
		*out = make([]libsveltosapiv1beta1.ClusterInfo, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EventTriggerStatus.
func (in *EventTriggerStatus) DeepCopy() *EventTriggerStatus {
	if in == nil {
		return nil
	}
	out := new(EventTriggerStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *GeneratorReference) DeepCopyInto(out *GeneratorReference) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new GeneratorReference.
func (in *GeneratorReference) DeepCopy() *GeneratorReference {
	if in == nil {
		return nil
	}
	out := new(GeneratorReference)
	in.DeepCopyInto(out)
	return out
}
