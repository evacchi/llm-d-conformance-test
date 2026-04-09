package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Resource kinds classified as prerequisites (applied before AppWrapper).
var prerequisiteKinds = map[string]bool{
	// RBAC
	"ServiceAccount":     true,
	"Role":               true,
	"RoleBinding":        true,
	"ClusterRole":        true,
	"ClusterRoleBinding": true,
	// Gateway API
	"Gateway":        true,
	"HTTPRoute":      true,
	"GRPCRoute":      true,
	"TCPRoute":       true,
	"ReferenceGrant": true,
	// Inference / Istio
	"InferencePool":      true,
	"DestinationRule":    true,
	"VirtualService":     true,
	"PeerAuthentication": true,
	// Configuration
	"ConfigMap": true,
	"Secret":    true,
	// Kubernetes infrastructure
	"CustomResourceDefinition": true,
	"Namespace":                true,
	"PersistentVolumeClaim":    true,
	// Monitoring
	"ServiceMonitor": true,
	"PodMonitor":     true,
	// Kueue
	"LocalQueue": true,
}

// Resource kinds that can be wrapped inside an AppWrapper.
var wrappableKinds = map[string]bool{
	"Deployment":      true,
	"StatefulSet":     true,
	"Job":             true,
	"Service":         true,
	"Pod":             true,
	"ReplicaSet":      true,
	"DaemonSet":       true,
	"LeaderWorkerSet": true,
}

// K8sResource is a parsed Kubernetes resource with enough metadata for classification.
type K8sResource struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	Raw        map[string]interface{} // full resource as a map
}

func (r *K8sResource) String() string {
	return fmt.Sprintf("%s/%s (%s)", r.Kind, r.Name, r.APIVersion)
}

// ClassifiedResources holds resources split into prerequisites and workloads.
type ClassifiedResources struct {
	Prerequisites []*K8sResource
	Workloads     []*K8sResource
}

// ParseMultiDocYAML splits a multi-document YAML string into individual K8sResources.
func ParseMultiDocYAML(yamlContent string) ([]*K8sResource, error) {
	var resources []*K8sResource
	decoder := yaml.NewDecoder(strings.NewReader(yamlContent))
	for {
		var raw interface{}
		err := decoder.Decode(&raw)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return resources, fmt.Errorf("decoding YAML document: %w", err)
		}
		if raw == nil {
			continue
		}
		// Skip non-map documents (e.g. helmfile log lines like "Adding repo...")
		rawMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		r := &K8sResource{Raw: rawMap}
		if v, ok := rawMap["apiVersion"].(string); ok {
			r.APIVersion = v
		}
		if v, ok := rawMap["kind"].(string); ok {
			r.Kind = v
		}
		if meta, ok := rawMap["metadata"].(map[string]interface{}); ok {
			if v, ok := meta["name"].(string); ok {
				r.Name = v
			}
			if v, ok := meta["namespace"].(string); ok {
				r.Namespace = v
			}
		}
		if r.Kind == "" {
			continue // skip empty or malformed documents
		}
		resources = append(resources, r)
	}
	return resources, nil
}

// ClassifyResources splits resources into prerequisites and wrappable workloads.
// Unknown kinds default to prerequisites (safer — they get applied first).
func ClassifyResources(resources []*K8sResource) *ClassifiedResources {
	result := &ClassifiedResources{}
	for _, r := range resources {
		if prerequisiteKinds[r.Kind] {
			result.Prerequisites = append(result.Prerequisites, r)
		} else if wrappableKinds[r.Kind] {
			result.Workloads = append(result.Workloads, r)
		} else {
			// Unknown kind — treat as prerequisite (safer default)
			result.Prerequisites = append(result.Prerequisites, r)
		}
	}
	return result
}

// AppWrapperConfig controls how the AppWrapper is constructed.
type AppWrapperConfig struct {
	Name      string // AppWrapper name (default: "llmd-benchmark")
	Namespace string // target namespace
	QueueName string // Kueue queue name (default: "benchmark-queue")
}

// BuildAppWrapper constructs an AppWrapper CR wrapping the given workloads.
func BuildAppWrapper(workloads []*K8sResource, cfg AppWrapperConfig) (map[string]interface{}, error) {
	if cfg.Name == "" {
		cfg.Name = "llmd-benchmark"
	}
	if cfg.QueueName == "" {
		cfg.QueueName = "benchmark-queue"
	}

	components := make([]interface{}, 0, len(workloads))
	for _, w := range workloads {
		components = append(components, map[string]interface{}{
			"template": w.Raw,
		})
	}

	aw := map[string]interface{}{
		"apiVersion": "workload.codeflare.dev/v1beta2",
		"kind":       "AppWrapper",
		"metadata": map[string]interface{}{
			"name":      cfg.Name,
			"namespace": cfg.Namespace,
			"labels": map[string]interface{}{
				"kueue.x-k8s.io/queue-name": cfg.QueueName,
			},
		},
		"spec": map[string]interface{}{
			"components": components,
		},
	}
	return aw, nil
}

// RenderAppWrapperYAML builds an AppWrapper and returns it as YAML.
func RenderAppWrapperYAML(workloads []*K8sResource, cfg AppWrapperConfig) (string, error) {
	aw, err := BuildAppWrapper(workloads, cfg)
	if err != nil {
		return "", err
	}
	out, err := yaml.Marshal(aw)
	if err != nil {
		return "", fmt.Errorf("marshaling AppWrapper: %w", err)
	}
	return string(out), nil
}

// RenderResourcesYAML serializes a slice of resources back to multi-document YAML.
func RenderResourcesYAML(resources []*K8sResource) (string, error) {
	var parts []string
	for _, r := range resources {
		out, err := yaml.Marshal(r.Raw)
		if err != nil {
			return "", fmt.Errorf("marshaling %s: %w", r, err)
		}
		parts = append(parts, string(out))
	}
	return strings.Join(parts, "---\n"), nil
}

// ApplyResources writes resources to a temp file and applies them with kubectl.
func (d *Deployer) ApplyResources(ctx context.Context, resources []*K8sResource, namespace string) error {
	yamlContent, err := RenderResourcesYAML(resources)
	if err != nil {
		return err
	}
	return d.applyYAML(ctx, yamlContent, namespace)
}

// ApplyAppWrapper builds an AppWrapper from workloads and applies it.
func (d *Deployer) ApplyAppWrapper(ctx context.Context, workloads []*K8sResource, cfg AppWrapperConfig) error {
	yamlContent, err := RenderAppWrapperYAML(workloads, cfg)
	if err != nil {
		return err
	}
	return d.applyYAML(ctx, yamlContent, cfg.Namespace)
}

func (d *Deployer) applyYAML(ctx context.Context, yamlContent, namespace string) error {
	tmpFile, err := os.CreateTemp("", "k8s-resources-*.yaml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if _, err := tmpFile.WriteString(yamlContent); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	_ = tmpFile.Close()

	output, err := d.Kubectl(ctx, "apply", "-n", namespace, "-f", tmpFile.Name())
	if err != nil {
		return fmt.Errorf("kubectl apply failed: %w\nOutput: %s", err, output)
	}
	return nil
}

// WaitForAppWrapper waits for an AppWrapper to reach the Running phase.
func (d *Deployer) WaitForAppWrapper(ctx context.Context, name, namespace, timeout string) error {
	d.logProgress("Waiting for AppWrapper %s to be Running (timeout=%s)...", name, timeout)
	output, err := d.Kubectl(ctx, "wait", "--for=jsonpath={.status.phase}=Running",
		"appwrapper", name, "-n", namespace, "--timeout="+timeout)
	if err != nil {
		// Get AppWrapper status for debugging
		status, _ := d.Kubectl(ctx, "get", "appwrapper", name, "-n", namespace, "-o", "yaml")
		return fmt.Errorf("AppWrapper not Running: %w\nOutput: %s\nStatus:\n%s", err, output, status)
	}
	d.logProgress("AppWrapper %s is Running", name)
	return nil
}

// DeleteAppWrapper deletes an AppWrapper by name.
func (d *Deployer) DeleteAppWrapper(ctx context.Context, name, namespace string) error {
	output, err := d.Kubectl(ctx, "delete", "appwrapper", name, "-n", namespace,
		"--ignore-not-found=true", "--timeout=120s")
	if err != nil {
		return fmt.Errorf("deleting AppWrapper: %w\nOutput: %s", err, output)
	}
	return nil
}

// SetResourceNamespaces overwrites the namespace on all resources.
func SetResourceNamespaces(resources []*K8sResource, namespace string) {
	for _, r := range resources {
		if r.Kind == "Namespace" || r.Kind == "ClusterRole" || r.Kind == "ClusterRoleBinding" || r.Kind == "CustomResourceDefinition" {
			continue // cluster-scoped resources don't have namespace
		}
		r.Namespace = namespace
		if meta, ok := r.Raw["metadata"].(map[string]interface{}); ok {
			meta["namespace"] = namespace
		}
	}
}

// ResourcesSummary returns a human-readable summary of resource counts by kind.
func ResourcesSummary(resources []*K8sResource) string {
	counts := make(map[string]int)
	for _, r := range resources {
		counts[r.Kind]++
	}
	data, _ := json.Marshal(counts)
	return string(data)
}
