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

package operator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linode/linodego/v2"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	"github.com/linode/karpenter-provider-linode/pkg/fake"
	"github.com/linode/karpenter-provider-linode/pkg/operator"
	"github.com/linode/karpenter-provider-linode/pkg/operator/options"
	"github.com/linode/karpenter-provider-linode/pkg/test"
)

func TestDRAOperatorRequiresCoreOptIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode  string
		ignore      bool
		wantError   string
		wantCatalog bool
	}{
		{name: "core disabled", mode: "lke", ignore: true, wantError: "requires --ignore-dra-requests=false"},
		{name: "instance mode", mode: "instance", wantError: "only supported in lke mode"},
		{name: "enabled", mode: "lke", wantError: "mock catalog reached", wantCatalog: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := coreoptions.ToContext(t.Context(), coretest.Options(coretest.OptionsFields{IgnoreDRARequests: new(tc.ignore)}))
			opts := test.Options(test.OptionsFields{Mode: new(tc.mode)})
			opts.NVIDIADRAInstanceTypes = "test-gpu-plan"
			ctx = options.ToContext(ctx, opts)
			api := &draOperatorMock{LinodeClient: fake.NewLinodeClient()}
			t.Cleanup(api.Reset)
			api.ListLKEClustersBehavior.Error.Set(errors.New("mock catalog reached"))
			_, err := operator.NewOperator(ctx, &coreoperator.Operator{}, api)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected %q, got %v", tc.wantError, err)
			}
			wantCalls := 0
			if tc.wantCatalog {
				wantCalls = 1
			}
			if calls := api.ListLKEClustersBehavior.Calls(); calls != wantCalls {
				t.Fatalf("catalog calls=%d, want %d", calls, wantCalls)
			}
		})
	}
}

// ListLKEClusters on the shared fake has a canned response without behavior hooks.
// This adapter supplies the same MockedFunction setup/expectation API used by
// other Linode methods, without changing the shared fake for unrelated suites.
type draOperatorMock struct {
	*fake.LinodeClient
	ListLKEClustersBehavior fake.MockedFunction[linodego.ListOptions, []linodego.LKECluster]
}

func (m *draOperatorMock) ListLKEClusters(ctx context.Context, opts *linodego.ListOptions) ([]linodego.LKECluster, error) {
	output, err := m.ListLKEClustersBehavior.Invoke(opts, func(opts *linodego.ListOptions) (*[]linodego.LKECluster, error) {
		clusters, err := m.LinodeClient.ListLKEClusters(ctx, opts)
		return &clusters, err
	})
	if output == nil {
		return nil, err
	}
	return *output, err
}

func (m *draOperatorMock) Reset() {
	m.LinodeClient.Reset()
	m.ListLKEClustersBehavior.Reset()
}
