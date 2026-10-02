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

package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linode/linodego/v2"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	sdk "github.com/linode/karpenter-provider-linode/pkg/linode"
	"github.com/linode/karpenter-provider-linode/pkg/operator/options"
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
			ctx := coreoptions.ToContext(t.Context(), &coreoptions.Options{IgnoreDRARequests: tc.ignore})
			ctx = options.ToContext(ctx, &options.Options{
				Mode: tc.mode, ClusterName: "test", ClusterRegion: "test-region", NVIDIADRAInstanceTypes: "test-gpu-plan",
			})
			api := &draOperatorMock{}
			_, err := NewOperator(ctx, &coreoperator.Operator{}, api)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected %q, got %v", tc.wantError, err)
			}
			if api.called != tc.wantCatalog {
				t.Fatalf("catalog called=%v, want %v", api.called, tc.wantCatalog)
			}
		})
	}
}

type draOperatorMock struct {
	sdk.LinodeAPI // All calls are mocked; unexpected methods panic.
	called        bool
}

func (m *draOperatorMock) ListLKEClusters(context.Context, *linodego.ListOptions) ([]linodego.LKECluster, error) {
	m.called = true
	return nil, errors.New("mock catalog reached")
}
