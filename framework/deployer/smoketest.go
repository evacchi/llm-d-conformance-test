package deployer

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	curlImage       = "curlimages/curl:8.5.0"
	warmupSleepSecs = 30

	// smokeTestScript runs health check, model listing, and inference from inside the cluster.
	// It expects TARGET and MODEL environment variables to be set.
	smokeTestScript = `set -e
echo "=== Health Check ==="
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$TARGET/health")
if [ "$HTTP_CODE" = "200" ]; then
  echo "Health check passed (HTTP $HTTP_CODE)"
else
  echo "Health check failed (HTTP $HTTP_CODE)"
  exit 1
fi

echo ""
echo "=== List Models ==="
curl -sf "$TARGET/v1/models"
echo ""

echo ""
echo "=== Chat Completion ==="
BODY=$(printf '{"model":"%s","messages":[{"role":"user","content":"What is 2+2? Answer in one word."}],"max_tokens":20}' "$MODEL")
RESPONSE=$(curl -sf "$TARGET/v1/chat/completions" -H "Content-Type: application/json" -d "$BODY")
echo "$RESPONSE"

echo ""
echo "=== Smoke test passed ==="
`
)

// SmokeTestConfig configures the in-cluster smoke-test Job.
type SmokeTestConfig struct {
	Name      string // Job name
	Namespace string
	Target    string // In-cluster service URL (e.g., http://gw-istio.ns.svc.cluster.local)
	Model     string // Model name for inference test
}

// DeriveTargetFromGateway scans parsed resources for a Gateway and builds the in-cluster URL.
// Format: http://{gateway-name}-istio.{namespace}.svc.cluster.local
// Returns empty string if no Gateway is found.
func DeriveTargetFromGateway(resources []*K8sResource, namespace string) string {
	for _, r := range resources {
		if r.Kind == "Gateway" {
			return fmt.Sprintf("http://%s-istio.%s.svc.cluster.local", r.Name, namespace)
		}
	}
	return ""
}

// FindGatewayTarget queries the cluster for a Gateway resource and derives the in-cluster URL.
func (d *Deployer) FindGatewayTarget(ctx context.Context, namespace string) (string, error) {
	out, err := d.Kubectl(ctx, "get", "gateway", "-n", namespace,
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", fmt.Errorf("no Gateway found: %w", err)
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf("no Gateway found in namespace %s", namespace)
	}
	return fmt.Sprintf("http://%s-istio.%s.svc.cluster.local", name, namespace), nil
}

// BuildSmokeTestJob constructs a K8s Job that validates health, model listing,
// and inference from inside the cluster.
//
// The Job has:
//   - Init container: polls /v1/models until the model server is ready, then waits for warmup
//   - Main container: runs health check, model listing, and a chat completion request
func BuildSmokeTestJob(cfg SmokeTestConfig) *K8sResource {
	initScript := fmt.Sprintf(
		`until curl -sf "$TARGET/v1/models"; do echo "Waiting for model server..."; sleep 10; done; echo "Model server ready, waiting %ds for warmup..."; sleep %d`,
		warmupSleepSecs, warmupSleepSecs,
	)

	raw := map[string]interface{}{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]interface{}{
			"name":      cfg.Name,
			"namespace": cfg.Namespace,
		},
		"spec": map[string]interface{}{
			"backoffLimit":          2,
			"activeDeadlineSeconds": 900, // 15 minutes
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"restartPolicy": "Never",
					"initContainers": []interface{}{
						map[string]interface{}{
							"name":    "wait-for-ready",
							"image":   curlImage,
							"command": []interface{}{"sh", "-c", initScript},
							"env": []interface{}{
								map[string]interface{}{
									"name":  "TARGET",
									"value": cfg.Target,
								},
							},
						},
					},
					"containers": []interface{}{
						map[string]interface{}{
							"name":    "smoke-test",
							"image":   curlImage,
							"command": []interface{}{"sh", "-c", smokeTestScript},
							"env": []interface{}{
								map[string]interface{}{
									"name":  "TARGET",
									"value": cfg.Target,
								},
								map[string]interface{}{
									"name":  "MODEL",
									"value": cfg.Model,
								},
							},
						},
					},
				},
			},
		},
	}

	return &K8sResource{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       cfg.Name,
		Namespace:  cfg.Namespace,
		Raw:        raw,
	}
}

// WaitForJobCompletion polls a Job until it succeeds, fails, or times out.
func (d *Deployer) WaitForJobCompletion(ctx context.Context, jobName, namespace string, timeout time.Duration) error {
	d.logProgress("Waiting for Job %s to complete (timeout=%s)...", jobName, timeout)

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	jobSeen := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				logs, _ := d.GetJobLogs(ctx, jobName, namespace)
				return fmt.Errorf("Job %s timed out after %v\nLogs:\n%s", jobName, timeout, logs)
			}

			// Get job status
			out, err := d.Kubectl(ctx, "get", "job", jobName, "-n", namespace,
				"-o", "jsonpath={.status.succeeded},{.status.failed},{.status.active}")
			if err != nil {
				if jobSeen {
					d.logProgress("Job %s was deleted, treating as completed", jobName)
					return nil
				}
				d.logProgress("Job %s not found yet", jobName)
				continue
			}

			jobSeen = true

			parts := strings.Split(strings.TrimSpace(out), ",")
			succeeded, failed, active := "", "", ""
			if len(parts) >= 1 {
				succeeded = parts[0]
			}
			if len(parts) >= 2 {
				failed = parts[1]
			}
			if len(parts) >= 3 {
				active = parts[2]
			}

			if succeeded == "1" {
				d.logProgress("Job %s completed successfully", jobName)
				return nil
			}

			// Check for fatal pod errors (bad image, missing secret, etc.)
			if fatalErr := d.checkJobPodErrors(ctx, jobName, namespace); fatalErr != "" {
				logs, _ := d.GetJobLogs(ctx, jobName, namespace)
				return fmt.Errorf("Job %s has fatal pod error: %s\nLogs:\n%s", jobName, fatalErr, logs)
			}

			// If all retries exhausted and nothing active, the job has failed
			if failed != "" && failed != "0" && (active == "" || active == "0") {
				logs, _ := d.GetJobLogs(ctx, jobName, namespace)
				return fmt.Errorf("Job %s failed\nLogs:\n%s", jobName, logs)
			}

			d.logProgress("Job %s: succeeded=%s failed=%s active=%s", jobName, succeeded, failed, active)
		}
	}
}

// checkJobPodErrors looks for fatal waiting reasons on a Job's pods.
func (d *Deployer) checkJobPodErrors(ctx context.Context, jobName, namespace string) string {
	out, _ := d.Kubectl(ctx, "get", "pods", "-n", namespace, "-l", "job-name="+jobName,
		"-o", "jsonpath={range .items[*]}{.metadata.name} {range .status.initContainerStatuses[*]}{.state.waiting.reason} {end}{range .status.containerStatuses[*]}{.state.waiting.reason} {end}{\"\\n\"}{end}")

	fatalReasons := []string{
		"ErrImagePull", "ImagePullBackOff",
		"CreateContainerConfigError", "CreateContainerError",
		"InvalidImageName",
	}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		for _, reason := range fatalReasons {
			if strings.Contains(line, reason) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// GetJobLogs returns the logs from all containers of a Job's pods.
func (d *Deployer) GetJobLogs(ctx context.Context, jobName, namespace string) (string, error) {
	return d.Kubectl(ctx, "logs", "job/"+jobName, "-n", namespace, "--all-containers=true", "--tail=100")
}
