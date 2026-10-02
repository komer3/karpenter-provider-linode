/*
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

package instancetype

import (
	"reflect"
	"testing"
	"unique"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

const nvidiaWholeGPUSelector = "device.driver == 'gpu.nvidia.com' && device.attributes['gpu.nvidia.com'].type == 'gpu'"

// Exercise the pinned core's actual allocation/CEL contract using a fake
// DeviceClass client and in-memory NodeClaims. No API server is started.
func TestDRAWholeGPUAllocation(t *testing.T) {
	t.Parallel()
	ctx := draContext(t)
	info := draType("test-gpu-plan", 2)
	it := NewDefaultResolver("test-region", info.ID).Resolve(ctx, &info, draNodeClass())
	allocator := draAllocator(t, nvidiaWholeGPUSelector)
	nodeA := &draNodeClaimMock{name: "node-a", instanceType: it}
	nodeB := &draNodeClaimMock{name: "node-b", instanceType: it}
	for _, name := range []string{"claim-a", "claim-b"} {
		result, err := allocator.Allocate(ctx, nodeA, []*resourcev1.ResourceClaim{draClaim(name)})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.InstanceTypes, nodeA.InstanceTypes()) || result.Allocation == nil {
			t.Fatalf("expected a GPU allocation on the selected instance type, got %+v", result)
		}
		result.Allocation.Commit(ctx)
	}
	if _, err := allocator.Allocate(ctx, nodeA, []*resourcev1.ResourceClaim{draClaim("claim-c")}); err == nil {
		t.Fatal("a third exclusive claim must not fit on two GPUs")
	}
	result, err := allocator.Allocate(ctx, nodeB, []*resourcev1.ResourceClaim{draClaim("claim-c")})
	if err != nil || result.Allocation == nil {
		t.Fatalf("identical templates on a different node must have independent capacity: %v", err)
	}
	result.Allocation.Commit(ctx)
	if _, err := allocator.Allocate(ctx, nodeA, []*resourcev1.ResourceClaim{draClaim("claim-a")}); err != nil {
		t.Fatalf("reusing an already reserved claim must not consume another GPU: %v", err)
	}
}

func TestDRASelectorAndOptInRejection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, selector string
		enabled        bool
	}{
		{name: "not opted in", selector: nvidiaWholeGPUSelector},
		{name: "wrong driver", selector: "device.driver == 'other.example.com'", enabled: true},
		{name: "MIG", selector: "device.attributes['gpu.nvidia.com'].type == 'mig'", enabled: true},
		{name: "unmodeled product", selector: nvidiaWholeGPUSelector + " && device.attributes['gpu.nvidia.com'].productName == 'NVIDIA H100'", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info := draType("test-gpu-plan", 2)
			var allowed []string
			if tc.enabled {
				allowed = []string{info.ID}
			}
			it := NewDefaultResolver("test-region", allowed...).Resolve(draContext(t), &info, draNodeClass())
			node := &draNodeClaimMock{name: "node", instanceType: it}
			if _, err := draAllocator(t, tc.selector).Allocate(draContext(t), node, []*resourcev1.ResourceClaim{draClaim("claim")}); err == nil {
				t.Fatal("unsupported request unexpectedly matched the predicted inventory")
			}
		})
	}
}

func draAllocator(t *testing.T, selector string) *dynamicresources.Allocator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := resourcev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	class := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu.nvidia.com"},
		Spec: resourcev1.DeviceClassSpec{Selectors: []resourcev1.DeviceSelector{{
			CEL: &resourcev1.CELDeviceSelector{Expression: selector},
		}}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class).Build()
	return dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{}, nil, client, nil)
}

func draClaim(name string) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name: "gpu",
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "gpu.nvidia.com", AllocationMode: resourcev1.DeviceAllocationModeExactCount, Count: 1,
			},
		}}}},
	}
}

type draNodeClaimMock struct {
	name         string
	instanceType *cloudprovider.InstanceType
}

func (n *draNodeClaimMock) ID() dynamicresources.NodeClaimID        { return unique.Make(n.name) }
func (n *draNodeClaimMock) NodeName() string                        { return "" }
func (n *draNodeClaimMock) NodePoolID() dynamicresources.NodePoolID { return unique.Make("test-pool") }
func (n *draNodeClaimMock) Requirements() scheduling.Requirements {
	return scheduling.NewRequirements()
}
func (n *draNodeClaimMock) InstanceTypes() []dynamicresources.InstanceTypeID {
	return []dynamicresources.InstanceTypeID{unique.Make(n.instanceType.Name)}
}
func (n *draNodeClaimMock) ResourceSlices() map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice {
	slices := make([]dynamicresources.ResourceSlice, len(n.instanceType.DynamicResources.ResourceSliceTemplates))
	for i, template := range n.instanceType.DynamicResources.ResourceSliceTemplates {
		slices[i] = dynamicresources.NewTemplateSlice(template)
	}
	return map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice{unique.Make(n.instanceType.Name): slices}
}
