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

package options_test

import (
	"flag"
	"reflect"
	"testing"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	"github.com/linode/karpenter-provider-linode/pkg/operator/options"
)

func TestDRAOptions(t *testing.T) {
	// Environment-backed options intentionally run without t.Parallel.
	const gpuTypeA = "test-gpu-a"
	for _, tc := range []struct {
		name, env  string
		args, want []string
		invalid    bool
	}{
		{name: "disabled by default"},
		{name: "environment", env: "test-gpu-a,test-gpu-b", want: []string{gpuTypeA, "test-gpu-b"}},
		{name: "flags override environment", env: gpuTypeA, args: []string{"--nvidia-dra-instance-types=test-gpu-b"}, want: []string{"test-gpu-b"}},
		{name: "explicit disable", env: gpuTypeA, args: []string{"--nvidia-dra-instance-types="}},
		{name: "trim whitespace", env: " test-gpu-a , test-gpu-b ", want: []string{gpuTypeA, "test-gpu-b"}},
		{name: "empty entry", env: "test-gpu-a,,test-gpu-b", invalid: true},
		{name: "trailing comma", env: "test-gpu-a,", invalid: true},
		{name: "blank entry", env: " ", invalid: true},
		{name: "embedded whitespace", env: "test gpu", invalid: true},
		{name: "instance mode", env: gpuTypeA, args: []string{"--mode=instance", "--cluster-region=test-region"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NVIDIA_DRA_INSTANCE_TYPES", tc.env)
			opts := &options.Options{}
			fs := &coreoptions.FlagSet{FlagSet: flag.NewFlagSet("test", flag.ContinueOnError)}
			opts.AddFlags(fs)
			err := opts.Parse(fs, append([]string{"--cluster-name=test", "--mode=lke"}, tc.args...)...)
			if (err != nil) != tc.invalid {
				t.Fatalf("unexpected validation result: %v", err)
			}
			if !tc.invalid && !reflect.DeepEqual(opts.NVIDIADRAInstanceTypeNames(), tc.want) {
				t.Fatalf("got %v, want %v", opts.NVIDIADRAInstanceTypeNames(), tc.want)
			}
		})
	}
}
