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
	appWrapperName      = "llmd-benchmark"
	smokeTestJobName    = "llmd-smoketest"
	pdValidationJobName = "llmd-pd-validation"
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

			// Auto-generate and apply HTTPRoute if missing
			if route, routeErr := dep.CreateHTTPRouteFromCluster(ctx, benchNamespace); routeErr == nil && route != nil {
				Expect(dep.ApplyResources(ctx, []*deployer.K8sResource{route}, benchNamespace)).To(Succeed(), "applying auto-generated HTTPRoute")
				logStep("[benchmark] Auto-generated HTTPRoute: %s", route.Name)
			}

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

		// Auto-generate HTTPRoute if Gateway + InferencePool exist but no HTTPRoute
		if route := deployer.CreateHTTPRoute(resources, benchNamespace); route != nil {
			resources = append(resources, route)
			logStep("[benchmark] Auto-generated HTTPRoute: %s", route.Name)
		}

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

	// ── Phase 5: P/D DISAGGREGATION VALIDATION ──────────────────
	It("should validate P/D KV transfer", func() {
		// Only run for P/D scenarios (helmfile path contains "pd")
		scenarioDir := filepath.Base(filepath.Dir(helmfilePath))
		if scenarioDir != "pd" {
			Skip("not a P/D scenario")
		}
		if !useSmokeTestJob {
			Skip("P/D validation requires in-cluster Job (no --endpoint)")
		}

		// Send a long prompt to trigger KV cache transfer between prefill and decode
		logStep("[benchmark] Creating P/D validation Job (long prompt to trigger KV transfer)")
		job := deployer.BuildPDValidationJob(deployer.SmokeTestConfig{
			Name: pdValidationJobName, Namespace: benchNamespace,
			Target: targetURL, Model: modelName,
		})
		Expect(dep.ApplyResources(ctx, []*deployer.K8sResource{job}, benchNamespace)).To(Succeed(), "applying P/D validation Job")

		err := dep.WaitForJobCompletion(ctx, pdValidationJobName, benchNamespace, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "P/D validation Job failed")

		logs, _ := dep.GetJobLogs(ctx, pdValidationJobName, benchNamespace)
		logStep("[benchmark] P/D validation output:\n%s", logs)

		// Check prefill pod logs for KV transfer confirmation
		logStep("[benchmark] Checking prefill pod logs for KV transfer")
		prefillLogs, _ := dep.Kubectl(ctx, "logs", "-n", benchNamespace,
			"-l", "app.kubernetes.io/component=prefill",
			"--all-containers=true", "--tail=200")
		// Fallback: search by pod name pattern
		if prefillLogs == "" {
			allPods, _ := dep.Kubectl(ctx, "get", "pods", "-n", benchNamespace,
				"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			for _, pod := range strings.Split(strings.TrimSpace(allPods), "\n") {
				if strings.Contains(pod, "prefill") {
					out, _ := dep.Kubectl(ctx, "logs", pod, "-n", benchNamespace,
						"--all-containers=true", "--tail=200")
					prefillLogs += out
				}
			}
		}
		logStep("[benchmark] Prefill logs (last 200 lines):\n%s", prefillLogs)
		Expect(prefillLogs).To(ContainSubstring("do_remote_decode"),
			"prefill pod logs should contain NIXL KV transfer confirmation (do_remote_decode)")

		// Check decode pod logs for KV transfer confirmation
		logStep("[benchmark] Checking decode pod logs for KV transfer")
		decodeLogs, _ := dep.Kubectl(ctx, "logs", "-n", benchNamespace,
			"-l", "app.kubernetes.io/component=decode",
			"--all-containers=true", "--tail=200")
		if decodeLogs == "" {
			allPods, _ := dep.Kubectl(ctx, "get", "pods", "-n", benchNamespace,
				"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			for _, pod := range strings.Split(strings.TrimSpace(allPods), "\n") {
				if strings.Contains(pod, "decode") && !strings.Contains(pod, "prefill") {
					out, _ := dep.Kubectl(ctx, "logs", pod, "-n", benchNamespace,
						"--all-containers=true", "--tail=200")
					decodeLogs += out
				}
			}
		}
		logStep("[benchmark] Decode logs (last 200 lines):\n%s", decodeLogs)
		Expect(decodeLogs).To(ContainSubstring("remote_block_ids"),
			"decode pod logs should contain NIXL KV transfer confirmation (remote_block_ids)")

		logStep("[benchmark] P/D KV transfer validated successfully")
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

		// Clean up standalone Jobs
		if useSmokeTestJob {
			_, _ = dep.Kubectl(ctx, "delete", "job", smokeTestJobName, "-n", benchNamespace, "--ignore-not-found=true")
			_, _ = dep.Kubectl(ctx, "delete", "job", pdValidationJobName, "-n", benchNamespace, "--ignore-not-found=true")
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
