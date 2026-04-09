package deployer

import (
	"strings"
	"testing"
)

func TestParseMultiDocYAML(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantKinds []string
		wantErr   bool
	}{
		{
			name: "single document",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
`,
			wantKinds: []string{"ConfigMap"},
		},
		{
			name: "multiple documents",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
---
apiVersion: v1
kind: Service
metadata:
  name: svc1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dep1
`,
			wantKinds: []string{"ConfigMap", "Service", "Deployment"},
		},
		{
			name:  "helmfile log lines mixed in (string documents)",
			input: "Adding repo llm-d-modelservice https://example.com\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n",
			wantKinds: []string{"ConfigMap"},
		},
		{
			name:      "empty input",
			input:     "",
			wantKinds: nil,
		},
		{
			name:      "only separators",
			input:     "---\n---\n---\n",
			wantKinds: nil,
		},
		{
			name: "document without kind is skipped",
			input: `apiVersion: v1
metadata:
  name: no-kind
---
apiVersion: v1
kind: Secret
metadata:
  name: my-secret
`,
			wantKinds: []string{"Secret"},
		},
		{
			name: "extracts namespace from metadata",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  namespace: my-ns
`,
			wantKinds: []string{"ConfigMap"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resources, err := ParseMultiDocYAML(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseMultiDocYAML() error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(resources) != len(tt.wantKinds) {
				t.Fatalf("got %d resources, want %d", len(resources), len(tt.wantKinds))
			}
			for i, r := range resources {
				if r.Kind != tt.wantKinds[i] {
					t.Errorf("resource[%d].Kind = %q, want %q", i, r.Kind, tt.wantKinds[i])
				}
			}
		})
	}

	// Verify namespace extraction
	t.Run("namespace is parsed", func(t *testing.T) {
		resources, _ := ParseMultiDocYAML(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
  namespace: my-ns
`)
		if len(resources) != 1 {
			t.Fatalf("expected 1 resource, got %d", len(resources))
		}
		if resources[0].Namespace != "my-ns" {
			t.Errorf("Namespace = %q, want %q", resources[0].Namespace, "my-ns")
		}
	})
}

func TestParseMultiDocYAML_HelmfileOutput(t *testing.T) {
	// Simulates realistic helmfile template output with log lines on stderr
	// that might leak into stdout via CombinedOutput.
	input := `Adding repo llm-d-modelservice https://llm-d-incubation.github.io/llm-d-modelservice/
Adding repo llm-d-infra https://llm-d-incubation.github.io/llm-d-infra/
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: llm-d-sa
  namespace: test-ns
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-decode
  namespace: test-ns
spec:
  replicas: 3
---
apiVersion: v1
kind: Service
metadata:
  name: vllm-svc
  namespace: test-ns
spec:
  ports:
    - port: 8000
`

	resources, err := ParseMultiDocYAML(input)
	if err != nil {
		t.Fatalf("ParseMultiDocYAML() failed on helmfile-like output: %v", err)
	}
	if len(resources) != 3 {
		t.Fatalf("expected 3 resources, got %d", len(resources))
	}

	expected := []struct {
		kind string
		name string
	}{
		{"ServiceAccount", "llm-d-sa"},
		{"Deployment", "vllm-decode"},
		{"Service", "vllm-svc"},
	}
	for i, e := range expected {
		if resources[i].Kind != e.kind {
			t.Errorf("resource[%d].Kind = %q, want %q", i, resources[i].Kind, e.kind)
		}
		if resources[i].Name != e.name {
			t.Errorf("resource[%d].Name = %q, want %q", i, resources[i].Name, e.name)
		}
	}
}

func TestClassifyResources(t *testing.T) {
	resources := []*K8sResource{
		{Kind: "ServiceAccount", Name: "sa1"},
		{Kind: "ConfigMap", Name: "cm1"},
		{Kind: "Secret", Name: "sec1"},
		{Kind: "Deployment", Name: "dep1"},
		{Kind: "Service", Name: "svc1"},
		{Kind: "StatefulSet", Name: "ss1"},
		{Kind: "InferencePool", Name: "pool1"},
		{Kind: "HTTPRoute", Name: "route1"},
		{Kind: "LeaderWorkerSet", Name: "lws1"},
		{Kind: "SomethingUnknown", Name: "unk1"},
	}

	classified := ClassifyResources(resources)

	// Prerequisites: ServiceAccount, ConfigMap, Secret, InferencePool, HTTPRoute, SomethingUnknown
	if len(classified.Prerequisites) != 6 {
		t.Errorf("expected 6 prerequisites, got %d", len(classified.Prerequisites))
		for _, r := range classified.Prerequisites {
			t.Logf("  prereq: %s/%s", r.Kind, r.Name)
		}
	}

	// Workloads: Deployment, Service, StatefulSet, LeaderWorkerSet
	if len(classified.Workloads) != 4 {
		t.Errorf("expected 4 workloads, got %d", len(classified.Workloads))
		for _, r := range classified.Workloads {
			t.Logf("  workload: %s/%s", r.Kind, r.Name)
		}
	}

	// Unknown kinds go to prerequisites
	foundUnknown := false
	for _, r := range classified.Prerequisites {
		if r.Kind == "SomethingUnknown" {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Error("expected unknown kind 'SomethingUnknown' in prerequisites")
	}
}

func TestBuildAppWrapper(t *testing.T) {
	workloads := []*K8sResource{
		{
			Kind: "Deployment",
			Name: "dep1",
			Raw: map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]interface{}{"name": "dep1"},
			},
		},
		{
			Kind: "Service",
			Name: "svc1",
			Raw: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]interface{}{"name": "svc1"},
			},
		},
	}

	aw, err := BuildAppWrapper(workloads, AppWrapperConfig{
		Name:      "test-aw",
		Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("BuildAppWrapper() error: %v", err)
	}

	// Check apiVersion and kind
	if aw["apiVersion"] != "workload.codeflare.dev/v1beta2" {
		t.Errorf("apiVersion = %v, want workload.codeflare.dev/v1beta2", aw["apiVersion"])
	}
	if aw["kind"] != "AppWrapper" {
		t.Errorf("kind = %v, want AppWrapper", aw["kind"])
	}

	// Check metadata
	meta := aw["metadata"].(map[string]interface{})
	if meta["name"] != "test-aw" {
		t.Errorf("metadata.name = %v, want test-aw", meta["name"])
	}
	if meta["namespace"] != "test-ns" {
		t.Errorf("metadata.namespace = %v, want test-ns", meta["namespace"])
	}

	// Check components
	spec := aw["spec"].(map[string]interface{})
	components := spec["components"].([]interface{})
	if len(components) != 2 {
		t.Fatalf("expected 2 components, got %d", len(components))
	}

	// Check first component wraps the deployment
	comp0 := components[0].(map[string]interface{})
	tmpl0 := comp0["template"].(map[string]interface{})
	if tmpl0["kind"] != "Deployment" {
		t.Errorf("component[0].template.kind = %v, want Deployment", tmpl0["kind"])
	}
}

func TestBuildAppWrapperDefaults(t *testing.T) {
	aw, err := BuildAppWrapper(nil, AppWrapperConfig{})
	if err != nil {
		t.Fatalf("BuildAppWrapper() error: %v", err)
	}

	meta := aw["metadata"].(map[string]interface{})
	if meta["name"] != "llmd-benchmark" {
		t.Errorf("default name = %v, want llmd-benchmark", meta["name"])
	}
	labels := meta["labels"].(map[string]interface{})
	if labels["kueue.x-k8s.io/queue-name"] != "benchmark-queue" {
		t.Errorf("default queue = %v, want benchmark-queue", labels["kueue.x-k8s.io/queue-name"])
	}
}

func TestSetResourceNamespaces(t *testing.T) {
	resources := []*K8sResource{
		{Kind: "Deployment", Name: "dep1", Raw: map[string]interface{}{"metadata": map[string]interface{}{"name": "dep1"}}},
		{Kind: "ClusterRole", Name: "cr1", Raw: map[string]interface{}{"metadata": map[string]interface{}{"name": "cr1"}}},
		{Kind: "Namespace", Name: "ns1", Raw: map[string]interface{}{"metadata": map[string]interface{}{"name": "ns1"}}},
		{Kind: "Service", Name: "svc1", Raw: map[string]interface{}{"metadata": map[string]interface{}{"name": "svc1"}}},
	}

	SetResourceNamespaces(resources, "target-ns")

	// Deployment and Service should get the namespace
	if resources[0].Namespace != "target-ns" {
		t.Errorf("Deployment namespace = %q, want target-ns", resources[0].Namespace)
	}
	if resources[3].Namespace != "target-ns" {
		t.Errorf("Service namespace = %q, want target-ns", resources[3].Namespace)
	}

	// ClusterRole and Namespace should NOT get namespace
	if resources[1].Namespace != "" {
		t.Errorf("ClusterRole namespace = %q, want empty", resources[1].Namespace)
	}
	if resources[2].Namespace != "" {
		t.Errorf("Namespace namespace = %q, want empty", resources[2].Namespace)
	}
}

func TestRenderAppWrapperYAML(t *testing.T) {
	workloads := []*K8sResource{
		{
			Kind: "Deployment",
			Name: "dep1",
			Raw: map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]interface{}{"name": "dep1"},
			},
		},
	}

	yaml, err := RenderAppWrapperYAML(workloads, AppWrapperConfig{
		Name:      "test-aw",
		Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("RenderAppWrapperYAML() error: %v", err)
	}

	if !strings.Contains(yaml, "kind: AppWrapper") {
		t.Error("rendered YAML missing 'kind: AppWrapper'")
	}
	if !strings.Contains(yaml, "name: test-aw") {
		t.Error("rendered YAML missing 'name: test-aw'")
	}
	if !strings.Contains(yaml, "namespace: test-ns") {
		t.Error("rendered YAML missing 'namespace: test-ns'")
	}
}

func TestResourcesSummary(t *testing.T) {
	resources := []*K8sResource{
		{Kind: "Deployment"},
		{Kind: "Deployment"},
		{Kind: "Service"},
		{Kind: "ConfigMap"},
	}

	summary := ResourcesSummary(resources)
	if !strings.Contains(summary, `"Deployment":2`) {
		t.Errorf("summary missing Deployment count: %s", summary)
	}
	if !strings.Contains(summary, `"Service":1`) {
		t.Errorf("summary missing Service count: %s", summary)
	}
}
