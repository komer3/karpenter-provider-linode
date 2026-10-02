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

package instancetype_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"unique"

	"github.com/linode/linodego/v2"
	"github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"

	v1 "github.com/linode/karpenter-provider-linode/pkg/apis/v1alpha1"
	linodecache "github.com/linode/karpenter-provider-linode/pkg/cache"
	"github.com/linode/karpenter-provider-linode/pkg/fake"
	"github.com/linode/karpenter-provider-linode/pkg/operator/options"
	"github.com/linode/karpenter-provider-linode/pkg/providers/instancetype"
	"github.com/linode/karpenter-provider-linode/pkg/test"
)

// These tests use only an in-memory catalog mock. They do not start envtest or
// contact Linode or a Kubernetes API server. Run with -run '^TestDRA'.
func TestDRAInventoryOptIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		allowed                 []string
		gpus, accelerated, want int
	}{
		{name: "disabled", gpus: 4},
		{name: "not allowlisted", allowed: []string{"another-type"}, gpus: 4},
		{name: "one GPU", allowed: []string{"test-gpu-plan"}, gpus: 1, want: 1},
		{name: "multiple GPUs", allowed: []string{"test-gpu-plan"}, gpus: 8, want: 8},
		{name: "separate accelerator count", allowed: []string{"test-gpu-plan"}, gpus: 2, accelerated: 4, want: 2},
		{name: "CPU only", allowed: []string{"test-gpu-plan"}},
		{name: "non GPU accelerator", allowed: []string{"test-gpu-plan"}, accelerated: 4},
		{name: "invalid negative GPU count", allowed: []string{"test-gpu-plan"}, gpus: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info := draType("test-gpu-plan", tc.gpus)
			info.AcceleratedDevices = tc.accelerated
			it := instancetype.NewDefaultResolver(fake.DefaultRegion, tc.allowed...).Resolve(draContext(t), &info, draNodeClass())
			assertDRAInventory(t, it, tc.want)
			if _, ok := it.Capacity[corev1.ResourceName("nvidia.com/gpu")]; ok {
				t.Fatal("DRA inventory must not advertise legacy scalar GPU capacity")
			}
			if len(it.Capacity) != 3 {
				t.Fatalf("ordinary CPU, RAM and pod capacity changed: %v", it.Capacity)
			}
		})
	}
}

func TestDRAInventoryIsolation(t *testing.T) {
	t.Parallel()
	allowed := []string{"test-gpu-plan"}
	resolver := instancetype.NewDefaultResolver(fake.DefaultRegion, allowed...)
	allowed[0] = "another-type"
	info := draType("test-gpu-plan", 2)
	first := resolver.Resolve(draContext(t), &info, draNodeClass())
	second := resolver.Resolve(draContext(t), &info, draNodeClass())
	if !reflect.DeepEqual(first.DynamicResources, second.DynamicResources) {
		t.Fatal("predicted inventory is not deterministic")
	}
	*first.DynamicResources.ResourceSliceTemplates[0].Devices[0].Attributes["type"].StringValue = "changed"
	if got := *first.DynamicResources.ResourceSliceTemplates[0].Devices[1].Attributes["type"].StringValue; got != "gpu" {
		t.Fatal("device attributes alias each other")
	}
	assertDRAInventory(t, second, 2)
}

func TestDRACacheKey(t *testing.T) {
	t.Parallel()
	nodeClass := draNodeClass()
	enabled := instancetype.NewDefaultResolver(fake.DefaultRegion, "a", "b").CacheKey(nodeClass)
	reordered := instancetype.NewDefaultResolver(fake.DefaultRegion, "b", "a", "a").CacheKey(nodeClass)
	disabled := instancetype.NewDefaultResolver(fake.DefaultRegion).CacheKey(nodeClass)
	if enabled != reordered || enabled == disabled {
		t.Fatalf("cache key does not distinguish normalized DRA configuration: %q, %q, %q", enabled, reordered, disabled)
	}
}

func TestDRAListGetAndOfferingCache(t *testing.T) {
	t.Parallel()
	ctx := draContext(t)
	types := []linodego.LinodeType{draType("test-gpu-plan", 2), draType("test-cpu-plan", 0)}
	api := &draCatalogMock{LinodeClient: fake.NewLinodeClient()}
	t.Cleanup(api.Reset)
	api.ListTypesOutput.Set(&types)
	offerings := []linodego.RegionAvailability{
		{Region: fake.DefaultRegion, Plan: types[0].ID, Available: true},
		{Region: fake.DefaultRegion, Plan: types[1].ID, Available: true},
	}
	api.GetRegionAvailabilityOutput.Set(&offerings)
	instanceTypesCache := cache.New(cache.NoExpiration, 0)
	offeringCache := cache.New(cache.NoExpiration, 0)
	discoveredCapacityCache := cache.New(cache.NoExpiration, 0)
	unavailableOfferings := linodecache.NewUnavailableOfferings()
	for _, c := range []*cache.Cache{instanceTypesCache, offeringCache, discoveredCapacityCache} {
		t.Cleanup(c.Flush)
	}
	t.Cleanup(unavailableOfferings.Flush)
	provider := instancetype.NewDefaultProvider(api, instancetype.NewDefaultResolver(fake.DefaultRegion, "test-gpu-plan"),
		instanceTypesCache, offeringCache, discoveredCapacityCache, unavailableOfferings)
	if err := provider.UpdateInstanceTypes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := provider.UpdateInstanceTypeOfferings(ctx); err != nil {
		t.Fatal(err)
	}
	nodeClass := draNodeClass()
	// Exercise Get before List caches the resolver result, then repeated List
	// and Get calls including the offering-cache hit path.
	before, err := provider.Get(ctx, nodeClass, "test-gpu-plan")
	if err != nil {
		t.Fatal(err)
	}
	assertDRAInventory(t, before, 2)
	for range 2 {
		types, err := provider.List(ctx, nodeClass)
		if err != nil {
			t.Fatal(err)
		}
		if len(types) != 2 {
			t.Fatalf("expected two types, got %d", len(types))
		}
		for _, it := range types {
			want := 0
			if it.Name == "test-gpu-plan" {
				want = 2
			}
			assertDRAInventory(t, it, want)
			if len(it.Offerings) != 1 || !it.Offerings[0].Available {
				t.Fatalf("expected an available offering for %s", it.Name)
			}
		}
		cached, err := provider.Get(ctx, nodeClass, "test-gpu-plan")
		if err != nil {
			t.Fatal(err)
		}
		assertDRAInventory(t, cached, 2)
		if len(cached.Offerings) != 0 {
			t.Fatal("InjectOfferings mutated the cached instance type")
		}
	}
	if api.ListTypesBehavior.Calls() != 1 || api.RegionAvailabilityBehavior.Calls() != 1 {
		t.Fatalf("List/Get unexpectedly called the catalog API: types=%d, availability=%d",
			api.ListTypesBehavior.Calls(), api.RegionAvailabilityBehavior.Calls())
	}
	if region := *api.RegionAvailabilityBehavior.CalledWithInput.At(0); region != fake.DefaultRegion {
		t.Fatalf("availability requested region %q, want %q", region, fake.DefaultRegion)
	}
	// A refreshed API count invalidates the cached prediction.
	types[0].GPUs = 1
	api.ListTypesOutput.Set(&types)
	if err := provider.UpdateInstanceTypes(ctx); err != nil {
		t.Fatal(err)
	}
	refreshed, err := provider.Get(ctx, nodeClass, "test-gpu-plan")
	if err != nil {
		t.Fatal(err)
	}
	assertDRAInventory(t, refreshed, 1)
}

func assertDRAInventory(t *testing.T, it *cloudprovider.InstanceType, count int) {
	t.Helper()
	templates := it.DynamicResources.ResourceSliceTemplates
	if count == 0 {
		if len(templates) != 0 {
			t.Fatalf("unexpected DRA inventory for %s", it.Name)
		}
		return
	}
	if len(templates) != 1 || len(templates[0].Devices) != count {
		t.Fatalf("expected one slice with %d devices: %+v", count, templates)
	}
	slice := templates[0]
	if slice.Driver.Value() != "gpu.nvidia.com" || slice.Pool.Name.Value() != "gpus" {
		t.Fatalf("unexpected driver/pool: %+v", slice)
	}
	if len(slice.SharedCounters) != 0 || len(it.DynamicResources.AttributeBindings) != 0 {
		t.Fatal("whole-GPU inventory must not imply sharing or topology")
	}
	for i, device := range slice.Devices {
		if device.Name.Value() != fmt.Sprintf("gpu-%d", i) {
			t.Fatalf("unexpected device identity: %v", device.Name.Value())
		}
		attr := device.Attributes["type"]
		if len(device.Attributes) != 1 || attr.StringValue == nil || *attr.StringValue != "gpu" {
			t.Fatalf("expected only type=gpu, got %+v", device.Attributes)
		}
		if len(device.Capacity) != 0 || device.AllowMultipleAllocations || len(device.ConsumesCounters) != 0 {
			t.Fatal("whole-GPU inventory must not invent memory or sharing")
		}
	}
}

func draContext(t *testing.T) context.Context {
	t.Helper()
	return options.ToContext(t.Context(), test.Options())
}

func draNodeClass() *v1.LinodeNodeClass {
	return test.LinodeNodeClass(v1.LinodeNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "test"}})
}

func draType(name string, gpus int) linodego.LinodeType {
	return linodego.LinodeType{ID: name, GPUs: gpus, Memory: 32768, VCPUs: 8}
}

const nvidiaWholeGPUSelector = "device.driver == 'gpu.nvidia.com' && device.attributes['gpu.nvidia.com'].type == 'gpu'"

// Exercise the pinned core's actual allocation/CEL contract using a fake
// DeviceClass client and in-memory NodeClaims. No API server is started.
func TestDRAWholeGPUAllocation(t *testing.T) {
	t.Parallel()
	ctx := draContext(t)
	info := draType("test-gpu-plan", 2)
	it := instancetype.NewDefaultResolver(fake.DefaultRegion, info.ID).Resolve(ctx, &info, draNodeClass())
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
			it := instancetype.NewDefaultResolver(fake.DefaultRegion, allowed...).Resolve(draContext(t), &info, draNodeClass())
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
	client := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(class).Build()
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

// Pinned core uses an unexported fakeNodeClaim in its allocator tests. There is
// no reusable exported fixture for this interface; this adapter supplies only
// in-memory identity and the provider's real ResourceSlice templates.
var _ dynamicresources.NodeClaim = (*draNodeClaimMock)(nil)

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

// The shared fake's catalog methods return AtomicPtr fixtures directly, without
// invoking their behavior hooks. This adapter retains those fixtures and adds
// the repository's MockedFunction call tracking; region uses its actual string
// argument rather than the shared availability hook's ListOptions input type.
type draCatalogMock struct {
	*fake.LinodeClient
	RegionAvailabilityBehavior fake.MockedFunction[string, []linodego.RegionAvailability]
}

func (m *draCatalogMock) ListTypes(ctx context.Context, opts *linodego.ListOptions) ([]linodego.LinodeType, error) {
	output, err := m.ListTypesBehavior.Invoke(opts, func(opts *linodego.ListOptions) (*[]linodego.LinodeType, error) {
		types, err := m.LinodeClient.ListTypes(ctx, opts)
		return &types, err
	})
	if output == nil {
		return nil, err
	}
	return *output, err
}

func (m *draCatalogMock) GetRegionAvailability(ctx context.Context, region string) ([]linodego.RegionAvailability, error) {
	output, err := m.RegionAvailabilityBehavior.Invoke(&region, func(region *string) (*[]linodego.RegionAvailability, error) {
		offerings, err := m.LinodeClient.GetRegionAvailability(ctx, *region)
		return &offerings, err
	})
	if output == nil {
		return nil, err
	}
	return *output, err
}

func (m *draCatalogMock) Reset() {
	m.LinodeClient.Reset()
	m.RegionAvailabilityBehavior.Reset()
}
