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
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// These are local Helm template unit tests with fixture values, not cluster tests.
// No Kubernetes client, Helm install, or network connection is used.
func TestDRAChart(t *testing.T) {
	helm, env := draHelmEnvironment(t)
	t.Parallel()
	for _, tc := range []struct {
		name, values, allowlist string
	}{
		{name: "disabled by default"},
		{name: "explicit empty allowlist", values: "settings:\n  dra:\n    nvidiaGPUInstanceTypes: []\n"},
		{name: "enabled", values: "settings:\n  dra:\n    nvidiaGPUInstanceTypes: [test-gpu-a, test-gpu-b]\n", allowlist: "test-gpu-a,test-gpu-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			output, err := renderDRAChart(t, helm, env, tc.values)
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			deployment, role := decodeDRAChart(t, output)
			assertDRAChart(t, deployment, role, tc.allowlist)
		})
	}
}

func assertDRAChart(t *testing.T, deployment *appsv1.Deployment, role *rbacv1.ClusterRole, allowlist string) {
	t.Helper()
	env := map[string]string{}
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		if _, exists := env[variable.Name]; exists {
			t.Fatalf("duplicate environment variable %s", variable.Name)
		}
		env[variable.Name] = variable.Value
	}
	_, providerEnabled := env["NVIDIA_DRA_INSTANCE_TYPES"]
	_, coreEnabled := env["IGNORE_DRA_REQUESTS"]
	if providerEnabled != (allowlist != "") || coreEnabled != providerEnabled {
		t.Fatalf("DRA opt-in did not enable both flags: %v", env)
	}
	if providerEnabled && (env["NVIDIA_DRA_INSTANCE_TYPES"] != allowlist || env["IGNORE_DRA_REQUESTS"] != "false") {
		t.Fatalf("unexpected DRA environment: %v", env)
	}
	var draRules []rbacv1.PolicyRule
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, "resource.k8s.io") {
			draRules = append(draRules, rule)
		}
	}
	var expected []rbacv1.PolicyRule
	if providerEnabled {
		expected = []rbacv1.PolicyRule{{
			APIGroups: []string{"resource.k8s.io"},
			Resources: []string{"resourceclaims", "resourceslices", "deviceclasses"},
			Verbs:     []string{"get", "list", "watch"},
		}}
	}
	if !reflect.DeepEqual(draRules, expected) {
		t.Fatalf("unexpected DRA permissions: %+v, want %+v", draRules, expected)
	}
}

func TestDRAChartRejectsConflictingConfiguration(t *testing.T) {
	helm, env := draHelmEnvironment(t)
	t.Parallel()
	for _, tc := range []struct{ name, values, want string }{
		{name: "empty entry", values: "settings:\n  dra:\n    nvidiaGPUInstanceTypes: [\"\"]\n", want: "must be nonempty IDs"},
		{name: "whitespace entry", values: "settings:\n  dra:\n    nvidiaGPUInstanceTypes: [\" \" ]\n", want: "must be nonempty IDs"},
		{name: "comma in entry", values: "settings:\n  dra:\n    nvidiaGPUInstanceTypes: [\"a,b\"]\n", want: "must be nonempty IDs"},
		{name: "non string entry", values: "settings:\n  dra:\n    nvidiaGPUInstanceTypes: [1]\n", want: "entries must be strings"},
		{name: "scalar allowlist", values: "settings:\n  dra:\n    nvidiaGPUInstanceTypes: test-gpu-a\n", want: "must be a list"},
		{name: "instance mode", values: "settings:\n  mode: instance\n  dra:\n    nvidiaGPUInstanceTypes: [test-gpu-a]\n", want: "requires settings.mode=lke"},
		{name: "core environment override", values: "settings:\n  dra:\n    nvidiaGPUInstanceTypes: [test-gpu-a]\ncontroller:\n  env:\n    - name: IGNORE_DRA_REQUESTS\n      value: \"true\"\n", want: "duplicate controller.env"},
		{name: "provider environment override", values: "settings:\n  dra:\n    nvidiaGPUInstanceTypes: [test-gpu-a]\ncontroller:\n  env:\n    - name: NVIDIA_DRA_INSTANCE_TYPES\n      value: other\n", want: "duplicate controller.env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			output, err := renderDRAChart(t, helm, env, tc.values)
			if err == nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("expected %q error, got %v\n%s", tc.want, err, output)
			}
		})
	}
}

// Capture the tool path and subprocess environment before t.Parallel because
// the existing Options suite clears the process environment after each spec.
func draHelmEnvironment(t *testing.T) (helm string, env []string) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("Helm is required for these local template unit tests")
	}
	return helm, os.Environ()
}

func renderDRAChart(t *testing.T, helm string, env []string, values string) ([]byte, error) {
	t.Helper()
	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesFile, []byte("credentialsSecretRef: test-credentials\n"+values), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), helm, "template", "dra-test", "../../../charts/karpenter", "--namespace", "karpenter",
		"--values", valuesFile, "--show-only", "templates/deployment.yaml", "--show-only", "templates/clusterrole-core.yaml")
	cmd.Env = env
	return cmd.CombinedOutput()
}

func decodeDRAChart(t *testing.T, output []byte) (*appsv1.Deployment, *rbacv1.ClusterRole) {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var deployment *appsv1.Deployment
	var role *rbacv1.ClusterRole
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			t.Fatal(err)
		}
		switch kind.Kind {
		case "Deployment":
			deployment = &appsv1.Deployment{}
			if err := json.Unmarshal(raw, deployment); err != nil {
				t.Fatal(err)
			}
		case "ClusterRole":
			role = &rbacv1.ClusterRole{}
			if err := json.Unmarshal(raw, role); err != nil {
				t.Fatal(err)
			}
		}
	}
	if deployment == nil || role == nil {
		t.Fatal("expected Deployment and ClusterRole in rendered chart")
	}
	return deployment, role
}
