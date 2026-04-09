// Package deployer provides Kubernetes deployment helpers for LLMInferenceService resources.
package deployer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aneeshkp/llm-d-conformance-test/framework/config"
	"github.com/aneeshkp/llm-d-conformance-test/framework/retry"
)

// Platform identifies the target Kubernetes platform.
type Platform string

const (
	PlatformOCP Platform = "ocp"
	PlatformAKS Platform = "aks"
	PlatformGKS Platform = "gks"
	PlatformAny Platform = "any"
)

// Deployer manages deployment of LLMInferenceService resources on Kubernetes.
type Deployer struct {
	Kubeconfig  string
	Platform    Platform
	Namespace   string
	ModelSource string // "pvc", "hf", or "pvc-snapshot"
	MockImage      string // if set, replace vLLM image with mock and remove GPU resources
	PullSecretName string // override pull secret name to copy (default: auto-detect from manifest)
	// LogFunc is called with progress messages. If nil, progress is silent.
	LogFunc func(format string, args ...interface{})
}

// New creates a Deployer for the given platform and namespace.
func New(kubeconfig string, platform Platform, namespace string) *Deployer {
	return &Deployer{
		Kubeconfig: kubeconfig,
		Platform:   platform,
		Namespace:  namespace,
	}
}

func (d *Deployer) logProgress(format string, args ...interface{}) {
	if d.LogFunc != nil {
		d.LogFunc(format, args...)
	}
}

// DeployResult captures the outcome of a deployment attempt.
type DeployResult struct {
	Name      string
	Namespace string
	Success   bool
	Error     error
	Duration  time.Duration
	Logs      []string
}

// Deploy applies a test case's LLMInferenceService manifest to the cluster.
func (d *Deployer) Deploy(ctx context.Context, tc *config.TestCase) *DeployResult {
	start := time.Now()
	result := &DeployResult{
		Name:      tc.Name,
		Namespace: d.resolveNamespace(tc),
	}

	ns := result.Namespace

	// Ensure namespace exists
	if err := d.ensureNamespace(ctx, ns); err != nil {
		result.Error = fmt.Errorf("creating namespace %s: %w", ns, err)
		result.Duration = time.Since(start)
		return result
	}
	result.Logs = append(result.Logs, fmt.Sprintf("Namespace %s ready", ns))

	// Copy image pull secrets referenced in the manifest from istio-system
	if err := d.ensurePullSecrets(ctx, tc.Deployment.ManifestPath, ns); err != nil {
		d.logProgress("  Warning: failed to copy pull secrets: %v", err)
	}

	// Apply the manifest
	manifestPath := tc.Deployment.ManifestPath
	if manifestPath == "" {
		result.Error = fmt.Errorf("no manifest path specified for test case %s", tc.Name)
		result.Duration = time.Since(start)
		return result
	}

	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		result.Error = fmt.Errorf("manifest not found: %s", manifestPath)
		result.Duration = time.Since(start)
		return result
	}

	// Patch the manifest with the correct model URI and name from test case config.
	// This allows one manifest to work with both hf:// and pvc:// sources.
	patchedPath, err := d.patchManifest(manifestPath, tc)
	if err != nil {
		result.Error = fmt.Errorf("patching manifest: %w", err)
		result.Duration = time.Since(start)
		return result
	}
	if patchedPath != manifestPath {
		defer func() { _ = os.Remove(patchedPath) }()
	}

	output, err := d.Kubectl(ctx, "apply", "-n", ns, "-f", patchedPath)
	if err != nil {
		result.Error = fmt.Errorf("applying manifest %s: %w\nOutput: %s", manifestPath, err, output)
		result.Duration = time.Since(start)
		return result
	}
	result.Logs = append(result.Logs, fmt.Sprintf("Applied manifest: %s", manifestPath), output)

	result.Success = true
	result.Duration = time.Since(start)
	return result
}

// WaitForReady waits for the LLMInferenceService to become ready,
// showing detailed status of all sub-resources during the wait.
func (d *Deployer) WaitForReady(ctx context.Context, tc *config.TestCase) error {
	ns := d.resolveNamespace(tc)
	timeout := tc.Deployment.ReadyTimeout.Duration
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	name := ExtractLLMISVCName(tc)
	waitStart := time.Now()
	return retry.UntilSuccess(ctx, retry.Options{
		Timeout:  timeout,
		Interval: 15 * time.Second,
		Name:     fmt.Sprintf("wait-ready-%s", name),
	}, func() error {
		// Call 1: Get llmisvc status (ready + reason + url in one call)
		llmStatus, _ := d.Kubectl(ctx, "get", "llmisvc", name, "-n", ns, "-o",
			"jsonpath={.status.conditions[?(@.type==\"Ready\")].status}|{.status.conditions[?(@.type==\"Ready\")].reason}|{.status.url}")
		parts := strings.SplitN(llmStatus, "|", 3)
		ready, reason := "", ""
		if len(parts) >= 1 {
			ready = strings.TrimSpace(parts[0])
		}
		if len(parts) >= 2 {
			reason = strings.TrimSpace(parts[1])
		}

		if strings.EqualFold(ready, "true") {
			d.logProgress("─── [%s] READY ───", name)
			return nil
		}
		if reason == "" {
			reason = "Waiting"
		}

		elapsed := time.Since(waitStart).Truncate(time.Second)
		remaining := (timeout - time.Since(waitStart)).Truncate(time.Second)
		if remaining < 0 {
			remaining = 0
		}

		// Call 2: Get all sub-resources in one call
		label := fmt.Sprintf("app.kubernetes.io/name=%s", name)
		subOut, _ := d.Kubectl(ctx, "get", "svc,pods", "-n", ns, "-l", label,
			"-o", "jsonpath=svc:{.items[?(@.kind==\"Service\")].metadata.name} pod:{range .items[?(@.kind==\"Pod\")]}{.status.phase}/{range .status.containerStatuses[*]}{.ready}{end} {end}")
		// Parse sub-resource output
		svc, podStatus := "no", "none"
		for _, part := range strings.Fields(subOut) {
			if strings.HasPrefix(part, "svc:") {
				if v := strings.TrimPrefix(part, "svc:"); v != "" {
					svc = "ok"
				}
			} else {
				podStatus = part
			}
		}

		// Check route and pool (these are different API groups, need separate calls)
		// Combined into one shell with ;
		routePool, _ := d.Kubectl(ctx, "get", "httproute,inferencepool", "-n", ns,
			"--ignore-not-found=true",
			"-o", "jsonpath=route:{.items[?(@.kind==\"HTTPRoute\")].metadata.name} pool:{.items[?(@.kind==\"InferencePool\")].metadata.name}")
		route, pool := "no", "no"
		for _, part := range strings.Fields(routePool) {
			if strings.HasPrefix(part, "route:") {
				if v := strings.TrimPrefix(part, "route:"); v != "" {
					route = "ok"
				}
			}
			if strings.HasPrefix(part, "pool:") {
				if v := strings.TrimPrefix(part, "pool:"); v != "" {
					pool = "ok"
				}
			}
		}

		d.logProgress("[%s] %s | svc=%s route=%s pool=%s pod=%s | %s/%s",
			name, reason, svc, route, pool, podStatus, elapsed, remaining)

		// Call 3: Show last vLLM log line (only call that can't be combined)
		vllmLog, _ := d.Kubectl(ctx, "logs", "-n", ns, "-l",
			fmt.Sprintf("app.kubernetes.io/name=%s,app.kubernetes.io/component=llminferenceservice-workload", name),
			"-c", "main", "--tail=1")
		if vllmLog = strings.TrimSpace(vllmLog); vllmLog != "" &&
			!strings.Contains(vllmLog, "Defaulted container") &&
			!strings.Contains(vllmLog, "Error from server") &&
			!strings.Contains(vllmLog, "is waiting to start") &&
			!strings.Contains(vllmLog, "PodInitializing") {
			d.logProgress("  vLLM: %s", vllmLog)
		}

		return fmt.Errorf("llmisvc %s not ready (READY=%s, REASON=%s)", name, ready, reason)
	})
}

// CheckResourceExists checks if a named Kubernetes resource exists in the namespace.
func (d *Deployer) CheckResourceExists(ctx context.Context, kind, name, ns string) (bool, error) {
	out, err := d.Kubectl(ctx, "get", kind, "-n", ns, "-l",
		fmt.Sprintf("app.kubernetes.io/name=%s", name),
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// CheckInferencePoolExists checks if any InferencePool exists in the namespace.
func (d *Deployer) CheckInferencePoolExists(ctx context.Context, ns string) (bool, error) {
	out, err := d.Kubectl(ctx, "get", "inferencepool", "-n", ns,
		"--ignore-not-found=true",
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// GetPodStatus returns pod status lines for pods matching the given llmisvc name.
func (d *Deployer) GetPodStatus(ctx context.Context, name, ns string) (string, error) {
	return d.Kubectl(ctx, "get", "pods", "-n", ns, "-l",
		fmt.Sprintf("app.kubernetes.io/name=%s", name),
		"-o", "jsonpath={range .items[*]}{.metadata.name} {.status.phase} restarts={range .status.containerStatuses[*]}{.restartCount}{end}{\"\\n\"}{end}")
}

// GetPodLogs returns the last N lines of logs from the main container.
func (d *Deployer) GetPodLogs(ctx context.Context, name, ns string, tail int) (string, error) {
	return d.Kubectl(ctx, "logs", "-n", ns, "-l",
		fmt.Sprintf("app.kubernetes.io/name=%s", name),
		"-c", "main", fmt.Sprintf("--tail=%d", tail))
}

// Cleanup deletes all resources for a test case.
func (d *Deployer) Cleanup(ctx context.Context, tc *config.TestCase) error {
	ns := d.resolveNamespace(tc)
	manifestPath := tc.Deployment.ManifestPath

	if manifestPath == "" {
		return fmt.Errorf("no manifest path for cleanup of %s", tc.Name)
	}

	output, err := d.Kubectl(ctx, "delete", "-n", ns, "-f", manifestPath, "--ignore-not-found=true", "--timeout=120s")
	if err != nil {
		return fmt.Errorf("cleanup failed for %s: %w\nOutput: %s", tc.Name, err, output)
	}

	// Wait for pods to terminate
	name := ExtractLLMISVCName(tc)
	_ = retry.UntilSuccess(ctx, retry.Options{
		Timeout:  2 * time.Minute,
		Interval: 5 * time.Second,
		Name:     fmt.Sprintf("cleanup-wait-%s", name),
	}, func() error {
		output, err := d.Kubectl(ctx, "get", "pods", "-n", ns, "-l",
			fmt.Sprintf("app.kubernetes.io/name=%s", name),
			"-o", "jsonpath={.items}")
		if err != nil {
			return nil // kubectl error likely means resources are gone
		}
		if output == "" || output == "[]" {
			return nil
		}
		return fmt.Errorf("pods still terminating for %s", name)
	})

	return nil
}

// CleanupNamespace deletes the entire namespace.
func (d *Deployer) CleanupNamespace(ctx context.Context, namespace string) error {
	output, err := d.Kubectl(ctx, "delete", "namespace", namespace, "--ignore-not-found=true", "--timeout=120s")
	if err != nil {
		return fmt.Errorf("deleting namespace %s: %w\nOutput: %s", namespace, err, output)
	}
	return nil
}

// GetServiceEndpoint returns the inference service URL.
// Tries the specific LLMInferenceService name first, then falls back to any in the namespace.
func (d *Deployer) GetServiceEndpoint(ctx context.Context, tc *config.TestCase) (string, error) {
	ns := d.resolveNamespace(tc)

	// Try specific name first, fall back to any llmisvc in the namespace
	name := ExtractLLMISVCName(tc)
	output, err := d.Kubectl(ctx, "get", "llmisvc", name, "-n", ns, "-o", "jsonpath={.status.url}")
	if err != nil || strings.TrimSpace(output) == "" {
		output, err = d.Kubectl(ctx, "get", "llmisvc", "-n", ns, "-o", "jsonpath={.items[0].status.url}")
	}
	if err != nil {
		return "", fmt.Errorf("getting service endpoint: %w", err)
	}

	url := strings.TrimSpace(output)
	if url == "" {
		// Fallback: try to get any service ClusterIP in the namespace
		svcOutput, err := d.Kubectl(ctx, "get", "svc", "-n", ns, "-l",
			"app.kubernetes.io/part-of=llminferenceservice",
			"-o", "jsonpath={.items[0].spec.clusterIP}")
		if err != nil || strings.TrimSpace(svcOutput) == "" {
			return "", fmt.Errorf("no endpoint found for any llmisvc in namespace %s", ns)
		}
		port := tc.Validation.HealthPort
		if port == 0 {
			port = 8000
		}
		scheme := "https"
		if tc.Validation.HealthScheme == "HTTP" {
			scheme = "http"
		}
		url = fmt.Sprintf("%s://%s:%d", scheme, strings.TrimSpace(svcOutput), port)
	}

	return url, nil
}

// GetPlatformInfo gathers cluster, KServe, and vLLM version info for reporting.
func (d *Deployer) GetPlatformInfo(ctx context.Context) map[string]string {
	info := make(map[string]string)

	if output, err := d.Kubectl(ctx, "version", "-o", "json"); err == nil {
		info["kubernetesVersionRaw"] = output
	}

	// Detect platform
	switch d.Platform {
	case PlatformOCP:
		if output, err := d.runCommand(ctx, "oc", "version"); err == nil {
			info["ocpVersion"] = strings.TrimSpace(output)
		}
	case PlatformAKS:
		info["platform"] = "aks"
	case PlatformGKS:
		info["platform"] = "gks"
	}

	if output, err := d.Kubectl(ctx, "get", "nodes", "-o",
		"jsonpath={.items[0].status.nodeInfo.kubeletVersion}"); err == nil {
		info["kubeletVersion"] = strings.TrimSpace(output)
	}

	// KServe version — from the controller deployment image tag
	for _, ns := range []string{"opendatahub", "kserve", "kserve-system"} {
		output, err := d.Kubectl(ctx, "get", "deployment", "-n", ns, "-l",
			"control-plane=kserve-controller-manager",
			"-o", "jsonpath={.items[0].spec.template.spec.containers[0].image}")
		if err == nil && strings.TrimSpace(output) != "" {
			info["kserveImage"] = strings.TrimSpace(output)
			// Extract version from image tag (e.g., "image:v0.15.1" → "v0.15.1")
			if idx := strings.LastIndex(output, ":"); idx >= 0 {
				info["kserveVersion"] = output[idx+1:]
			}
			break
		}
	}

	// vLLM image — from the inferenceservice-config configmap
	for _, ns := range []string{"opendatahub", "kserve", "kserve-system"} {
		output, err := d.Kubectl(ctx, "get", "configmap", "inferenceservice-config", "-n", ns,
			"-o", "jsonpath={.data.storageInitializer}")
		if err == nil && strings.TrimSpace(output) != "" {
			info["storageInitializerConfig"] = strings.TrimSpace(output)
			break
		}
	}

	return info
}

// GetVLLMVersion gets the vLLM image and version from a running pod in the namespace.
func (d *Deployer) GetVLLMVersion(ctx context.Context) map[string]string {
	info := make(map[string]string)

	// Get pod name
	podName, err := d.Kubectl(ctx, "get", "pods", "-n", d.Namespace, "-l",
		"app.kubernetes.io/component=llminferenceservice-workload",
		"-o", "jsonpath={.items[0].metadata.name}")
	pod := strings.TrimSpace(podName)
	if err != nil || pod == "" {
		return info
	}

	// Get image
	img, _ := d.Kubectl(ctx, "get", "pod", pod, "-n", d.Namespace,
		"-o", "jsonpath={.spec.containers[?(@.name=='main')].image}")
	if img = strings.TrimSpace(img); img != "" {
		info["vllmImage"] = img
	}

	// Get actual vLLM version from inside the pod
	ver, _ := d.Kubectl(ctx, "exec", pod, "-n", d.Namespace, "-c", "main",
		"--", "python3", "-c", "import vllm; print(vllm.__version__)")
	for _, line := range strings.Split(ver, "\n") {
		line = strings.TrimSpace(line)
		// Version string is like "0.8.5" or "0.8.5.post1" — starts with a digit, no spaces
		if line != "" && len(line) < 30 && line[0] >= '0' && line[0] <= '9' && !strings.Contains(line, " ") {
			info["vllmVersion"] = line
			break
		}
	}

	return info
}

// CheckCRDExists verifies a CRD is installed on the cluster.
func (d *Deployer) CheckCRDExists(ctx context.Context, crdName string) (bool, error) {
	output, err := d.Kubectl(ctx, "get", "crd", crdName, "--ignore-not-found=true")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

func (d *Deployer) ensureNamespace(ctx context.Context, ns string) error {
	_, err := d.Kubectl(ctx, "get", "namespace", ns)
	if err == nil {
		return nil // namespace exists
	}
	_, err = d.Kubectl(ctx, "create", "namespace", ns)
	return err
}

// ensurePullSecrets reads the manifest for imagePullSecrets references and copies
// them from istio-system into the target namespace if they don't already exist.
func (d *Deployer) ensurePullSecrets(ctx context.Context, manifestPath, ns string) error {
	// OCP clusters have pull secrets configured globally — no need to copy
	if d.Platform == PlatformOCP {
		return nil
	}

	seen := map[string]bool{}

	// If an explicit pull secret name is set, use that; otherwise auto-detect from manifest
	if d.PullSecretName != "" {
		seen[d.PullSecretName] = true
	} else {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			return fmt.Errorf("reading manifest: %w", err)
		}
		// Parse secret names from "- name: <secret>" lines under imagePullSecrets
		lines := strings.Split(string(data), "\n")
		inPullSecrets := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "imagePullSecrets:" {
				inPullSecrets = true
				continue
			}
			if inPullSecrets {
				if strings.HasPrefix(trimmed, "- name: ") {
					name := strings.TrimPrefix(trimmed, "- name: ")
					seen[name] = true
					continue
				}
				inPullSecrets = false
			}
		}
	}

	sourceNamespaces := []string{"istio-system", "kserve", "opendatahub"}
	for secretName := range seen {
		// Skip if already exists in target namespace
		if _, err := d.Kubectl(ctx, "get", "secret", secretName, "-n", ns); err == nil {
			continue
		}

		// Try to copy from known source namespaces
		copied := false
		for _, srcNS := range sourceNamespaces {
			if _, err := d.Kubectl(ctx, "get", "secret", secretName, "-n", srcNS); err != nil {
				continue
			}
			// Get the secret and re-apply in target namespace
			secretYAML, err := d.Kubectl(ctx, "get", "secret", secretName, "-n", srcNS, "-o", "yaml")
			if err != nil {
				continue
			}
			// Replace namespace and strip cluster-specific metadata
			secretYAML = strings.ReplaceAll(secretYAML, "namespace: "+srcNS, "namespace: "+ns)
			// Remove resourceVersion, uid, creationTimestamp so it can be created fresh
			var cleanLines []string
			for _, l := range strings.Split(secretYAML, "\n") {
				t := strings.TrimSpace(l)
				if strings.HasPrefix(t, "resourceVersion:") ||
					strings.HasPrefix(t, "uid:") ||
					strings.HasPrefix(t, "creationTimestamp:") {
					continue
				}
				cleanLines = append(cleanLines, l)
			}
			tmpFile, err := os.CreateTemp("", "pull-secret-*.yaml")
			if err != nil {
				return fmt.Errorf("creating temp file: %w", err)
			}
			_, _ = tmpFile.WriteString(strings.Join(cleanLines, "\n"))
			_ = tmpFile.Close()
			_, err = d.Kubectl(ctx, "apply", "-n", ns, "-f", tmpFile.Name())
			_ = os.Remove(tmpFile.Name())
			if err != nil {
				continue
			}
			d.logProgress("  Copied pull secret %s from %s to %s", secretName, srcNS, ns)
			copied = true
			break
		}
		if !copied {
			return fmt.Errorf("pull secret %q not found in any of %v", secretName, sourceNamespaces)
		}
	}
	return nil
}

// Kubectl runs a kubectl command with the deployer's kubeconfig and platform settings.
func (d *Deployer) Kubectl(ctx context.Context, args ...string) (string, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if d.Kubeconfig != "" {
		cmdArgs = append(cmdArgs, "--kubeconfig", d.Kubeconfig)
	}
	cmdArgs = append(cmdArgs, args...)

	bin := "kubectl"
	if d.Platform == PlatformOCP {
		// Prefer oc on OpenShift, fall back to kubectl
		if _, err := exec.LookPath("oc"); err == nil {
			bin = "oc"
		}
	}

	return d.runCommand(ctx, bin, cmdArgs...)
}

func (d *Deployer) runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.Output() // stdout only — avoids exec-plugin/OIDC noise from stderr
	return string(output), err
}

// patchManifest reads a manifest, patches the model URI and name from the test case config,
// and writes a temp file. This allows one manifest template to work with any model and source.
func (d *Deployer) patchManifest(manifestPath string, tc *config.TestCase) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("reading manifest %s: %w", manifestPath, err)
	}

	lines := strings.Split(string(data), "\n")
	patched := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Patch uri: line
		if strings.HasPrefix(trimmed, "uri:") && (strings.Contains(trimmed, "hf://") || strings.Contains(trimmed, "pvc://")) {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			lines[i] = fmt.Sprintf("%suri: %s", indent, tc.Model.URI)
			patched = true
		}
		// Patch name: line under model (the one after uri)
		if strings.HasPrefix(trimmed, "name:") && i > 0 {
			prevTrimmed := strings.TrimSpace(lines[i-1])
			if strings.HasPrefix(prevTrimmed, "uri:") {
				indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
				lines[i] = fmt.Sprintf("%sname: %s", indent, tc.Model.Name)
				patched = true
			}
		}
	}

	// Mock mode: replace main vLLM container with mock image (no GPU, no model download)
	// Only patches under spec.template.containers, NOT spec.router.scheduler.template.containers
	if d.MockImage != "" {
		var newLines []string
		skip := false
		containerIndent := 0
		inSchedulerTemplate := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			lineIndent := len(line) - len(strings.TrimLeft(line, " "))

			// Track if we're inside the scheduler template section
			if trimmed == "scheduler:" || strings.HasPrefix(trimmed, "scheduler: ") {
				inSchedulerTemplate = true
			}
			// spec.template / spec.prefill.template are at lower indent than scheduler.template
			if trimmed == "template:" && !inSchedulerTemplate {
				// Already outside scheduler — keep as false
			} else if trimmed == "template:" && lineIndent <= 4 {
				inSchedulerTemplate = false
			}

			// Replace all "- name: main" under spec.template and spec.prefill.template, not scheduler.template
			if trimmed == "- name: main" && !inSchedulerTemplate {
				containerIndent = lineIndent
				skip = true
				indent := strings.Repeat(" ", containerIndent)
				newLines = append(newLines,
					indent+"- name: main",
					indent+"  image: "+d.MockImage,
					indent+"  imagePullPolicy: Always",
					indent+"  command: [\"python3\"]",
					indent+"  args: [\"/app/server.py\"]",
					indent+"  resources:",
					indent+"    limits:",
					indent+"      cpu: \"500m\"",
					indent+"      memory: 128Mi",
					indent+"    requests:",
					indent+"      cpu: \"100m\"",
					indent+"      memory: 64Mi",
				)
				patched = true
				continue
			}

			if skip {
				if trimmed != "" && lineIndent <= containerIndent {
					skip = false
				} else {
					continue
				}
			}

			newLines = append(newLines, line)
		}
		// Filter empty lines left behind by container block removal
		var filteredLines []string
		for _, line := range newLines {
			if line != "" {
				filteredLines = append(filteredLines, line)
			}
		}
		lines = filteredLines
		d.logProgress("  Mock mode: using image %s (no GPU)", d.MockImage)
	}

	if !patched {
		return manifestPath, nil
	}

	tmpFile, err := os.CreateTemp("", "llmisvc-patched-*.yaml")
	if err != nil {
		return "", fmt.Errorf("creating temp manifest: %w", err)
	}
	if _, err := tmpFile.WriteString(strings.Join(lines, "\n")); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return "", fmt.Errorf("writing patched manifest: %w", err)
	}
	_ = tmpFile.Close()
	d.logProgress("  Patched manifest: uri=%s, name=%s", tc.Model.URI, tc.Model.Name)
	return tmpFile.Name(), nil
}

func (d *Deployer) resolveNamespace(tc *config.TestCase) string {
	if tc.Deployment.Namespace != "" {
		return tc.Deployment.Namespace
	}
	if d.Namespace != "" {
		return d.Namespace
	}
	return "llm-test"
}

// ExtractLLMISVCName extracts the LLMInferenceService name from the manifest file.
// Falls back to displayName or test case name if manifest can't be read.
func ExtractLLMISVCName(tc *config.TestCase) string {
	// Try to read metadata.name from the manifest file
	if tc.Deployment.ManifestPath != "" {
		data, err := os.ReadFile(tc.Deployment.ManifestPath)
		if err == nil {
			lines := strings.Split(string(data), "\n")
			for i, line := range lines {
				if strings.TrimSpace(line) == "metadata:" && i+1 < len(lines) {
					next := strings.TrimSpace(lines[i+1])
					if strings.HasPrefix(next, "name:") {
						name := strings.TrimSpace(strings.TrimPrefix(next, "name:"))
						name = strings.Trim(name, "\"'")
						if name != "" {
							return name
						}
					}
				}
			}
		}
	}
	if tc.Model.DisplayName != "" {
		return sanitizeK8sName(tc.Model.DisplayName)
	}
	return sanitizeK8sName(tc.Name)
}

func sanitizeK8sName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, " ", "-")
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}
