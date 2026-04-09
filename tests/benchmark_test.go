package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aneeshkp/llm-d-conformance-test/framework/client"
	"github.com/aneeshkp/llm-d-conformance-test/framework/deployer"
	"github.com/aneeshkp/llm-d-conformance-test/framework/retry"
)

// guideName returns the llm-d guide subdirectory that the helmfile expects
// in LLM_D_GUIDES. Derived from the helmfile directory name.
func guideName(hfPath string) string {
	dir := filepath.Base(filepath.Dir(hfPath))
	switch dir {
	case "is-vanilla", "is-balanced":
		return "inference-scheduling"
	case "pd":
		return "pd-disaggregation"
	case "precise-prefix-cache":
		return "precise-prefix-cache-aware"
	case "multimodal":
		return "inference-scheduling"
	default:
		return dir
	}
}

// helmfileExtraEnv builds the environment variables needed by the helmfile template.
func helmfileExtraEnv(hfPath, model string) map[string]string {
	env := map[string]string{
		"MODEL": model,
	}
	if guidesPath != "" {
		guide := guideName(hfPath)
		env["LLM_D_GUIDES"] = filepath.Join(guidesPath, guide)
	}
	return env
}

const (
	appWrapperName   = "llmd-benchmark"
	smokeTestJobName = "llmd-smoketest"
)

var _ = Describe("Benchmark Smoke Test", Label("benchmark"), Ordered, func() {
	var (
		benchNamespace  string
		svcEndpoint     string
		llmClient       *client.LLMClient
		modelName       string
		useAppWrapper   bool
		useSmokeTestJob bool
		targetURL       string
		streamer        *deployer.Streamer
	)

	BeforeAll(func() {
		if helmfilePath == "" {
			Skip("--helmfile not provided, skipping benchmark tests")
		}

		// Resolve helmfile path relative to project root
		if !filepath.IsAbs(helmfilePath) {
			helmfilePath = filepath.Join(findRootDir(), helmfilePath)
		}
		if _, err := os.Stat(helmfilePath); os.IsNotExist(err) {
			Fail(fmt.Sprintf("helmfile not found: %s", helmfilePath))
		}

		// Resolve guides path
		if guidesPath != "" && !filepath.IsAbs(guidesPath) {
			guidesPath = filepath.Join(findRootDir(), guidesPath)
		}

		// Use a dedicated namespace for the benchmark
		benchNamespace = namespace
		if benchNamespace == "" || benchNamespace == "llm-conformance-test" {
			benchNamespace = "llm-d-benchmark"
		}

		// Default model
		modelName = "Qwen/Qwen2.5-7B-Instruct"
		if modelOverride != "" {
			modelName = modelOverride
		}

		// Use AppWrapper if the CRD is available on the cluster
		out, err := dep.Kubectl(ctx, "get", "crd", "appwrappers.workload.codeflare.dev", "--ignore-not-found=true")
		useAppWrapper = err == nil && strings.TrimSpace(out) != ""
		if useAppWrapper {
			logStep("[benchmark] AppWrapper CRD detected — will wrap workloads in AppWrapper")
		} else {
			logStep("[benchmark] No AppWrapper CRD — will use helmfile sync directly")
		}
	})

	// ── Phase 1: DEPLOY ──────────────────────────────────────────
	It("should deploy infrastructure", func() {
		extraEnv := helmfileExtraEnv(helmfilePath, modelName)

		if !useAppWrapper {
			// Simple path: helmfile sync
			logStep("[benchmark] Deploying via helmfile sync: %s", helmfilePath)
			err := dep.HelmfileSync(ctx, helmfilePath, helmfileEnv, benchNamespace, extraEnv)
			Expect(err).NotTo(HaveOccurred(), "helmfile sync failed")
			logStep("[benchmark] Helmfile sync completed")

			// Start streaming K8s events and pod logs in the background
			streamer = deployer.NewStreamer(kubeconfig, benchNamespace, logStep)
			streamer.StreamEvents(ctx, "[event]")
			streamer.StreamAllPodLogs(ctx, 10*time.Second)

			// Deploy in-cluster smoke-test Job (if no external endpoint provided)
			if endpoint == "" {
				var gwErr error
				targetURL, gwErr = dep.FindGatewayTarget(ctx, benchNamespace)
				Expect(gwErr).NotTo(HaveOccurred(), "could not derive target URL from Gateway")
				logStep("[benchmark] Derived target URL: %s", targetURL)

				job := deployer.BuildSmokeTestJob(deployer.SmokeTestConfig{
					Name: smokeTestJobName, Namespace: benchNamespace,
					Target: targetURL, Model: modelName,
				})
				Expect(dep.ApplyResources(ctx, []*deployer.K8sResource{job}, benchNamespace)).To(Succeed(), "applying smoke-test Job")
				useSmokeTestJob = true
				logStep("[benchmark] Smoke-test Job applied")
			}
			return
		}

		// AppWrapper path: render → classify → apply prereqs → wrap & apply AppWrapper
		logStep("[benchmark] Rendering helmfile template: %s", helmfilePath)
		rendered, err := dep.HelmfileTemplate(ctx, helmfilePath, helmfileEnv, benchNamespace, extraEnv)
		Expect(err).NotTo(HaveOccurred(), "helmfile template failed")

		resources, err := deployer.ParseMultiDocYAML(rendered)
		Expect(err).NotTo(HaveOccurred(), "parsing rendered YAML")
		logStep("[benchmark] Rendered %d resources", len(resources))

		// Ensure all resources target our namespace
		deployer.SetResourceNamespaces(resources, benchNamespace)

		// Classify
		classified := deployer.ClassifyResources(resources)
		logStep("[benchmark] Prerequisites: %s", deployer.ResourcesSummary(classified.Prerequisites))
		logStep("[benchmark] Workloads:      %s", deployer.ResourcesSummary(classified.Workloads))

		// Derive target from Gateway for the smoke-test Job (applied later, outside AppWrapper)
		if endpoint == "" {
			allResources := append(classified.Prerequisites, classified.Workloads...)
			targetURL = deployer.DeriveTargetFromGateway(allResources, benchNamespace)
			Expect(targetURL).NotTo(BeEmpty(), "no Gateway found in rendered resources — cannot derive in-cluster target URL")
			logStep("[benchmark] Derived target URL: %s", targetURL)
			useSmokeTestJob = true
		}

		// Ensure namespace exists
		_, _ = dep.Kubectl(ctx, "create", "namespace", benchNamespace, "--dry-run=client", "-o", "yaml")
		_, _ = dep.Kubectl(ctx, "apply", "-f", "-", "--dry-run=client") // no-op, just checking

		if err := dep.ApplyResources(ctx, classified.Prerequisites, benchNamespace); err != nil {
			// Some prereqs may fail on first apply (ordering), retry once
			logStep("[benchmark] Retrying prerequisite apply after 5s...")
			time.Sleep(5 * time.Second)
			Expect(dep.ApplyResources(ctx, classified.Prerequisites, benchNamespace)).To(Succeed(), "applying prerequisites")
		}
		logStep("[benchmark] Prerequisites applied")

		// Wrap workloads in AppWrapper and apply
		Expect(dep.ApplyAppWrapper(ctx, classified.Workloads, deployer.AppWrapperConfig{
			Name:      appWrapperName,
			Namespace: benchNamespace,
		})).To(Succeed(), "applying AppWrapper")
		logStep("[benchmark] AppWrapper applied")

		// Start streaming K8s events and pod logs in the background
		streamer = deployer.NewStreamer(kubeconfig, benchNamespace, logStep)
		streamer.StreamEvents(ctx, "[event]")
		streamer.StreamAllPodLogs(ctx, 10*time.Second)
	})

	// ── Phase 2: WAIT FOR APPWRAPPER (if applicable) ─────────────
	It("should have AppWrapper running", func() {
		if !useAppWrapper {
			Skip("not using AppWrapper")
		}
		logStep("[benchmark] Waiting for AppWrapper %s to reach Running phase", appWrapperName)
		err := retry.UntilSuccess(ctx, retry.Options{
			Timeout:  15 * time.Minute,
			Interval: 30 * time.Second,
			Name:     "appwrapper-running",
		}, func() error {
			phase, awErr := dep.GetAppWrapperPhase(ctx, appWrapperName, benchNamespace)
			if awErr != nil {
				logStep("[benchmark]   AppWrapper query failed: %v", awErr)
				return awErr
			}
			switch phase {
			case "Running":
				logStep("[benchmark]   AppWrapper is Running")
				return nil
			case "Failed":
				status, _ := dep.Kubectl(ctx, "get", "appwrapper", appWrapperName, "-n", benchNamespace, "-o", "yaml")
				Fail(fmt.Sprintf("AppWrapper %s failed.\nStatus:\n%s", appWrapperName, status))
			}
			logStep("[benchmark]   AppWrapper phase: %s — still waiting", phase)
			return fmt.Errorf("AppWrapper phase is %s, not Running", phase)
		})
		Expect(err).NotTo(HaveOccurred(), "AppWrapper did not reach Running")
	})

	// ── Phase 3: PODS READY ──────────────────────────────────────
	It("should have all pods running", func() {
		logStep("[benchmark] Waiting for pods to be ready")

		err := retry.UntilSuccess(ctx, retry.Options{
			Timeout:  15 * time.Minute,
			Interval: 15 * time.Second,
			Name:     "pods-ready",
		}, func() error {
			out, err := dep.Kubectl(ctx, "get", "pods", "-n", benchNamespace,
				"-o", "jsonpath={range .items[*]}{.metadata.name} phase={.status.phase} ready={.status.containerStatuses[0].ready} reason={.status.containerStatuses[0].state.waiting.reason}{\"\\n\"}{end}")
			if err != nil {
				return fmt.Errorf("listing pods: %w", err)
			}
			pods := strings.TrimSpace(out)
			if pods == "" {
				logStep("[benchmark]   no pods found yet in namespace %s", benchNamespace)
				return fmt.Errorf("no pods found in namespace %s", benchNamespace)
			}

			allRunning := true
			var pending []string
			for _, line := range strings.Split(pods, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}

				fields := make(map[string]string)
				parts := strings.Fields(line)
				podName := parts[0]
				for _, part := range parts {
					if k, v, ok := strings.Cut(part, "="); ok {
						fields[k] = v
					}
				}

				// Skip Job pods (smoke-test) — they are transient and won't stay Running/Ready
				if strings.HasPrefix(podName, smokeTestJobName) {
					continue
				}

				// Skip completed pods (phase=Succeeded)
				if fields["phase"] == "Succeeded" {
					continue
				}

				reason := fields["reason"]
				if reason == "CrashLoopBackOff" || reason == "Error" || reason == "CreateContainerError" || reason == "ErrImagePull" || reason == "ImagePullBackOff" {
					logs, _ := dep.Kubectl(ctx, "logs", podName, "-n", benchNamespace, "--tail=10", "--all-containers=true")
					Fail(fmt.Sprintf("Pod crash detected: %s\nLogs:\n%s", line, logs))
				}

				if fields["phase"] != "Running" || fields["ready"] != "true" {
					allRunning = false
					pending = append(pending, fmt.Sprintf("%s (phase=%s ready=%s)", podName, fields["phase"], fields["ready"]))
				}
			}
			if !allRunning {
				logStep("[benchmark]   waiting on %d pod(s): %s", len(pending), strings.Join(pending, ", "))
				return fmt.Errorf("not all pods Running/Ready yet")
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred(), "pods did not become ready")
	})

	// ── Phase 4: VALIDATE SERVICE ────────────────────────────────
	It("should validate inference service", func() {
		if useSmokeTestJob {
			// Deploy smoke-test Job as standalone resource (not inside AppWrapper)
			logStep("[benchmark] Creating smoke-test Job %s (target=%s)", smokeTestJobName, targetURL)
			job := deployer.BuildSmokeTestJob(deployer.SmokeTestConfig{
				Name: smokeTestJobName, Namespace: benchNamespace,
				Target: targetURL, Model: modelName,
			})
			Expect(dep.ApplyResources(ctx, []*deployer.K8sResource{job}, benchNamespace)).To(Succeed(), "applying smoke-test Job")

			// Wait for it to complete
			logStep("[benchmark] Waiting for smoke-test Job %s to complete", smokeTestJobName)
			err := dep.WaitForJobCompletion(ctx, smokeTestJobName, benchNamespace, 15*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "smoke test Job failed")

			logs, _ := dep.GetJobLogs(ctx, smokeTestJobName, benchNamespace)
			logStep("[benchmark] Smoke test output:\n%s", logs)
		} else {
			// External endpoint validation
			svcEndpoint = endpoint
			logStep("[benchmark] Service endpoint: %s", svcEndpoint)
			llmClient = client.New(svcEndpoint)

			// Health check
			logStep("[benchmark] Checking /health endpoint")
			err := retry.UntilSuccess(ctx, retry.Options{
				Timeout:  2 * time.Minute,
				Interval: 5 * time.Second,
				Name:     "health-check",
			}, func() error {
				return llmClient.HealthCheck(ctx)
			})
			Expect(err).NotTo(HaveOccurred(), "/health failed")

			// Model listing
			logStep("[benchmark] Checking /v1/models")
			models, err := llmClient.ListModels(ctx)
			Expect(err).NotTo(HaveOccurred(), "/v1/models failed")
			Expect(models.Data).NotTo(BeEmpty(), "no models listed")
			found := false
			for _, m := range models.Data {
				if m.ID == modelName {
					found = true
					break
				}
			}
			if !found && len(models.Data) > 0 {
				logStep("[benchmark] Model %s not found, using %s", modelName, models.Data[0].ID)
				modelName = models.Data[0].ID
			}
			logStep("[benchmark] Model available: %s", modelName)

			// Inference
			logStep("[benchmark] Running inference test")
			resp, err := llmClient.ChatCompletions(ctx, client.ChatRequest{
				Model: modelName,
				Messages: []client.ChatMessage{
					{Role: "user", Content: "What is 2+2? Answer in one word."},
				},
				MaxTokens: 20,
			})
			Expect(err).NotTo(HaveOccurred(), "chat completion failed")
			Expect(resp.Choices).NotTo(BeEmpty(), "no choices returned")
			Expect(resp.Choices[0].Message.Content).NotTo(BeEmpty(), "empty response content")
			logStep("[benchmark] Inference OK: %q (tokens=%d)", resp.Choices[0].Message.Content, resp.Usage.TotalTokens)
		}
	})

	// ── CLEANUP ──────────────────────────────────────────────────
	AfterAll(func() {
		// Stop background streams
		if streamer != nil {
			streamer.Stop()
		}

		if helmfilePath == "" {
			return
		}
		if noCleanup {
			logStep("[benchmark] CLEANUP SKIPPED: --nocleanup flag set")
			return
		}

		logStep("[benchmark] Cleaning up namespace %s", benchNamespace)

		// Clean up standalone smoke-test Job
		if useSmokeTestJob {
			_, _ = dep.Kubectl(ctx, "delete", "job", smokeTestJobName, "-n", benchNamespace, "--ignore-not-found=true")
		}

		if useAppWrapper {
			// Delete AppWrapper first — it cascades to wrapped resources
			if err := dep.DeleteAppWrapper(ctx, appWrapperName, benchNamespace); err != nil {
				logStep("[benchmark] AppWrapper delete failed: %v", err)
			}
		}

		// Clean up everything via helmfile destroy or namespace delete
		extraEnv := helmfileExtraEnv(helmfilePath, modelName)
		if err := dep.HelmfileDestroy(ctx, helmfilePath, helmfileEnv, benchNamespace, extraEnv); err != nil {
			logStep("[benchmark] helmfile destroy failed, deleting namespace: %v", err)
			_ = dep.CleanupNamespace(ctx, benchNamespace)
		}
	})
})
