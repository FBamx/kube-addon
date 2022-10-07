/*
Copyright 2022.

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

package v1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// AdvancedJobSpec defines the desired state of AdvancedJob
type AdvancedJobSpec struct {
	Parallelism             *intstr.IntOrString `json:"parallelism,omitempty" protobuf:"varint,1,opt,name=parallelism"`
	Template                v1.PodTemplateSpec  `json:"template,omitempty"`
	Completions             *int32              `json:"completions,omitempty" protobuf:"varint,2,opt,name=completions"`
	ActiveDeadlineSeconds   *int64              `json:"activeDeadlineSeconds,omitempty" protobuf:"varint,3,opt,name=activeDeadlineSeconds"`
	BackoffLimit            *int32              `json:"backoffLimit,omitempty" protobuf:"varint,7,opt,name=backoffLimit"`
	TTLSecondsAfterFinished *int32              `json:"ttlSecondsAfterFinished,omitempty" protobuf:"varint,8,opt,name=ttlSecondsAfterFinished"`
	Suspend                 *bool               `json:"suspend,omitempty" protobuf:"varint,10,opt,name=suspend"`
}

// AdvancedJobStatus defines the observed state of AdvancedJob
type AdvancedJobStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// AdvancedJob is the Schema for the advancedjobs API
type AdvancedJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AdvancedJobSpec   `json:"spec,omitempty"`
	Status AdvancedJobStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// AdvancedJobList contains a list of AdvancedJob
type AdvancedJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AdvancedJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AdvancedJob{}, &AdvancedJobList{})
}
