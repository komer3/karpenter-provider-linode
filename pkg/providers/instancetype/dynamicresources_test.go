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
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/linode/linodego/v2"
	"github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	v1 "github.com/linode/karpenter-provider-linode/pkg/apis/v1alpha1"
	linodecache "github.com/linode/karpenter-provider-linode/pkg/cache"
	sdk "github.com/linode/karpenter-provider-linode/pkg/linode"
	"github.com/linode/karpenter-provider-linode/pkg/operator/options"
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
			it := NewDefaultResolver("test-region", tc.allowed...).Resolve(draContext(t), &info, draNodeClass())
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
	resolver := NewDefaultResolver("test-region", allowed...)
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
	enabled := NewDefaultResolver("test-region", "a", "b").CacheKey(nodeClass)
	reordered := NewDefaultResolver("test-region", "b", "a", "a").CacheKey(nodeClass)
	disabled := NewDefaultResolver("test-region").CacheKey(nodeClass)
	if enabled != reordered || enabled == disabled {
		t.Fatalf("cache key does not distinguish normalized DRA configuration: %q, %q, %q", enabled, reordered, disabled)
	}
}

func TestDRAListGetAndOfferingCache(t *testing.T) {
	t.Parallel()
	ctx := draContext(t)
	api := &draCatalogMock{types: []linodego.LinodeType{draType("test-gpu-plan", 2), draType("test-cpu-plan", 0)}}
	provider := NewDefaultProvider(api, NewDefaultResolver("test-region", "test-gpu-plan"),
		cache.New(cache.NoExpiration, 0), cache.New(cache.NoExpiration, 0), cache.New(cache.NoExpiration, 0),
		linodecache.NewUnavailableOfferings())
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
	if api.listCalls != 1 || api.availabilityCalls != 1 {
		t.Fatalf("List/Get unexpectedly called the catalog API: %+v", api)
	}
	// A refreshed API count invalidates the cached prediction.
	api.types[0].GPUs = 1
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
	return options.ToContext(t.Context(), &options.Options{Mode: "lke", ClusterRegion: "test-region", VMMemoryOverheadPercent: 0.075})
}

func draNodeClass() *v1.LinodeNodeClass {
	return &v1.LinodeNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
}

func draType(name string, gpus int) linodego.LinodeType {
	return linodego.LinodeType{ID: name, GPUs: gpus, Memory: 32768, VCPUs: 8}
}

type draCatalogMock struct {
	sdk.LinodeAPI                // Unexpected API calls panic rather than reaching a service.
	types                        []linodego.LinodeType
	listCalls, availabilityCalls int
}

func (m *draCatalogMock) ListTypes(context.Context, *linodego.ListOptions) ([]linodego.LinodeType, error) {
	m.listCalls++
	return append([]linodego.LinodeType(nil), m.types...), nil
}

func (m *draCatalogMock) GetRegionAvailability(_ context.Context, region string) ([]linodego.RegionAvailability, error) {
	m.availabilityCalls++
	offerings := make([]linodego.RegionAvailability, len(m.types))
	for i, info := range m.types {
		offerings[i] = linodego.RegionAvailability{Region: region, Plan: info.ID, Available: true}
	}
	return offerings, nil
}
