// Package deployer — helmfile deployment support.
package deployer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// HelmfileSync deploys resources using helmfile sync.
func (d *Deployer) HelmfileSync(ctx context.Context, helmfilePath, environment, namespace string, extraEnv map[string]string) error {
	args := []string{
		"--file", helmfilePath,
		"--environment", environment,
		"-n", namespace,
		"sync",
	}
	output, err := d.runHelmfile(ctx, args, extraEnv)
	if err != nil {
		return fmt.Errorf("helmfile sync failed: %w\nOutput: %s", err, output)
	}
	d.logProgress("helmfile sync completed")
	return nil
}

// HelmfileTemplate renders resources without applying them.
// Returns the rendered multi-document YAML.
func (d *Deployer) HelmfileTemplate(ctx context.Context, helmfilePath, environment, namespace string, extraEnv map[string]string) (string, error) {
	args := []string{
		"--file", helmfilePath,
		"--environment", environment,
		"-n", namespace,
		"--quiet",
		"template",
	}
	output, err := d.runHelmfile(ctx, args, extraEnv)
	if err != nil {
		return "", fmt.Errorf("helmfile template failed: %w\nOutput: %s", err, output)
	}
	return output, nil
}

// HelmfileDestroy removes resources deployed by helmfile.
func (d *Deployer) HelmfileDestroy(ctx context.Context, helmfilePath, environment, namespace string, extraEnv map[string]string) error {
	args := []string{
		"--file", helmfilePath,
		"--environment", environment,
		"-n", namespace,
		"destroy",
	}
	output, err := d.runHelmfile(ctx, args, extraEnv)
	if err != nil {
		return fmt.Errorf("helmfile destroy failed: %w\nOutput: %s", err, output)
	}
	d.logProgress("helmfile destroy completed")
	return nil
}

func (d *Deployer) runHelmfile(ctx context.Context, args []string, extraEnv map[string]string) (string, error) {
	cmd := exec.CommandContext(ctx, "helmfile", args...)
	cmd.Env = os.Environ()
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	if d.Kubeconfig != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+d.Kubeconfig)
	}
	output, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(output))
	if len(out) > 2000 {
		// Truncate verbose helmfile output for logging
		out = out[:2000] + "\n... (truncated)"
	}
	return out, err
}

// WaitForAllPodsReady waits until all pods in the namespace are Running with all containers ready.
func (d *Deployer) WaitForAllPodsReady(ctx context.Context, namespace string, timeout string) error {
	d.logProgress("Waiting for all pods in %s to be ready (timeout=%s)...", namespace, timeout)

	// kubectl wait for all pods to have condition Ready
	output, err := d.Kubectl(ctx, "wait", "--for=condition=Ready", "pods", "--all",
		"-n", namespace, "--timeout="+timeout)
	if err != nil {
		// Get pod status for debugging
		status, _ := d.Kubectl(ctx, "get", "pods", "-n", namespace,
			"-o", "wide", "--show-labels")
		return fmt.Errorf("pods not ready: %w\nOutput: %s\nPod status:\n%s", err, output, status)
	}
	d.logProgress("All pods ready in %s", namespace)
	return nil
}

// FindServiceEndpoint discovers the inference service endpoint in a namespace.
// Looks for services with port 8000 (vLLM default) and returns the ClusterIP URL.
func (d *Deployer) FindServiceEndpoint(ctx context.Context, namespace string) (string, error) {
	// Try to find an InferencePool — the EPP service fronts the pool
	poolName, _ := d.Kubectl(ctx, "get", "inferencepool", "-n", namespace,
		"-o", "jsonpath={.items[0].metadata.name}")
	poolName = strings.TrimSpace(poolName)

	if poolName != "" {
		// EPP service is typically named <pool>-epp or gaie-<name>-epp
		// Try common patterns
		for _, svcPattern := range []string{
			poolName,
			poolName + "-epp",
		} {
			ip, err := d.Kubectl(ctx, "get", "svc", svcPattern, "-n", namespace,
				"-o", "jsonpath={.spec.clusterIP}")
			ip = strings.TrimSpace(ip)
			if err == nil && ip != "" && ip != "None" {
				port, _ := d.Kubectl(ctx, "get", "svc", svcPattern, "-n", namespace,
					"-o", "jsonpath={.spec.ports[0].port}")
				port = strings.TrimSpace(port)
				if port == "" {
					port = "8000"
				}
				return fmt.Sprintf("http://%s:%s", ip, port), nil
			}
		}
	}

	// Fallback: find any service with port 8000
	out, err := d.Kubectl(ctx, "get", "svc", "-n", namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}|{.spec.clusterIP}|{range .spec.ports[*]}{.port},{end}{\"\\n\"}{end}")
	if err != nil {
		return "", fmt.Errorf("listing services: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		ip := strings.TrimSpace(parts[1])
		ports := strings.TrimSpace(parts[2])
		if ip != "" && ip != "None" && strings.Contains(ports, "8000") {
			return fmt.Sprintf("http://%s:8000", ip), nil
		}
	}

	return "", fmt.Errorf("no inference service endpoint found in namespace %s", namespace)
}

// ListVLLMPods returns the names of vLLM pods in the namespace.
// Tries multiple label selectors to handle different chart labeling conventions.
func (d *Deployer) ListVLLMPods(ctx context.Context, namespace string) ([]string, error) {
	selectors := []string{
		"app.kubernetes.io/component=decode",
		"app.kubernetes.io/component=llminferenceservice-workload",
	}
	for _, sel := range selectors {
		out, err := d.Kubectl(ctx, "get", "pods", "-n", namespace, "-l", sel,
			"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		if err != nil {
			continue
		}
		var pods []string
		for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
			if name = strings.TrimSpace(name); name != "" {
				pods = append(pods, name)
			}
		}
		if len(pods) > 0 {
			return pods, nil
		}
	}

	// Fallback: find pods with a container exposing port 8000
	out, err := d.Kubectl(ctx, "get", "pods", "-n", namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}|{range .spec.containers[*]}{range .ports[*]}{.containerPort},{end}{end}{\"\\n\"}{end}")
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	var pods []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) == 2 && strings.Contains(parts[1], "8000") {
			pods = append(pods, strings.TrimSpace(parts[0]))
		}
	}
	if len(pods) > 0 {
		return pods, nil
	}
	return nil, fmt.Errorf("no vLLM pods found in namespace %s", namespace)
}

// ListEPPPods returns the names of EPP (endpoint picker) pods in the namespace.
func (d *Deployer) ListEPPPods(ctx context.Context, namespace string) ([]string, error) {
	selectors := []string{
		"app.kubernetes.io/component=endpoint-picker",
		"app.kubernetes.io/component=router-scheduler",
		"gateway-api-inference-extension/service=endpoint-picker",
	}
	for _, sel := range selectors {
		out, err := d.Kubectl(ctx, "get", "pods", "-n", namespace, "-l", sel,
			"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		if err != nil {
			continue
		}
		var pods []string
		for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
			if name = strings.TrimSpace(name); name != "" {
				pods = append(pods, name)
			}
		}
		if len(pods) > 0 {
			return pods, nil
		}
	}

	// Fallback: find pods with "epp" in their name
	out, err := d.Kubectl(ctx, "get", "pods", "-n", namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	var pods []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name != "" && strings.Contains(name, "epp") {
			pods = append(pods, name)
		}
	}
	if len(pods) > 0 {
		return pods, nil
	}
	return nil, fmt.Errorf("no EPP pods found in namespace %s", namespace)
}
