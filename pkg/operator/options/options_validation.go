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

package options

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/awslabs/operatorpkg/serrors"
	"go.uber.org/multierr"
)

func (o *Options) Validate() error {
	return multierr.Combine(
		o.validateEndpoint(),
		o.validateVMMemoryOverheadPercent(),
		o.validateRequiredFields(),
		o.validateMode(),
		o.validateNVIDIADRA(),
	)
}

func (o *Options) validateNVIDIADRA() error {
	if o.NVIDIADRAInstanceTypes == "" {
		return nil
	}
	if o.Mode != "lke" {
		return fmt.Errorf("nvidia-dra-instance-types is only supported in lke mode")
	}
	for _, name := range o.NVIDIADRAInstanceTypeNames() {
		if name == "" || strings.ContainsAny(name, " \t\r\n") {
			return fmt.Errorf("nvidia-dra-instance-types must contain comma-separated, non-empty instance type IDs without whitespace")
		}
	}
	return nil
}

func (o *Options) validateEndpoint() error {
	if o.ClusterEndpoint == "" {
		return nil
	}
	endpoint, err := url.Parse(o.ClusterEndpoint)
	// url.Parse() will accept a lot of input without error; make
	// sure it's a real URL
	if err != nil || !endpoint.IsAbs() || endpoint.Hostname() == "" {
		return serrors.Wrap(fmt.Errorf("cluster endpoint URL is not valid"), "cluster-endpoint", o.ClusterEndpoint)
	}
	return nil
}

func (o *Options) validateVMMemoryOverheadPercent() error {
	if o.VMMemoryOverheadPercent < 0 {
		return fmt.Errorf("vm-memory-overhead-percent cannot be negative")
	}
	return nil
}

func (o *Options) validateRequiredFields() error {
	if o.ClusterName == "" {
		return fmt.Errorf("missing field, cluster-name")
	}
	if o.Mode == "instance" && o.ClusterRegion == "" {
		return fmt.Errorf("missing field, cluster-region")
	}
	return nil
}

func (o *Options) validateMode() error {
	if o.Mode != "lke" && o.Mode != "instance" {
		return fmt.Errorf("invalid mode %q, must be either 'lke' or 'instance'", o.Mode)
	}
	return nil
}
