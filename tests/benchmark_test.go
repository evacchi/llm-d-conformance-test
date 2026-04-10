package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aneeshkp/llm-d-conformance-test/framework/deployer"
	"github.com/aneeshkp/llm-d-conformance-test/framework/metrics"
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
	benchmarkJobName = "llmd-benchmark-job"
)

var _ = Describe("Benchmark Smoke Test", Label("benchmark"), Ordered, func() {
	var (
		benchNamespace  string
		modelName       string
		useAppWrapper   bool
		useSmokeTestJob bool
		targetURL       string
		streamer        *deployer.Streamer
		vllmMetrics     []*metrics.ScrapeResult
		eppMetrics      []*metrics.ScrapeResult
		scenarioDir     string
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

			// Derive target for the benchmark Job (applied in Phase 4)
			if endpoint == "" {
				var gwErr error
				targetURL, gwErr = dep.FindGatewayTarget(ctx, benchNamespace)
				Expect(gwErr).NotTo(HaveOccurred(), "could not derive target URL from Gateway")
				logStep("[benchmark] Derived target URL: %s", targetURL)
				useSmokeTestJob = true
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

				// Skip benchmark Job pods — they are transient and won't stay Running/Ready
				if strings.HasPrefix(podName, benchmarkJobName) {
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

	// ── Phase 3b: INFRASTRUCTURE READINESS ──────────────────────
	It("should have Gateway programmed", func() {
		logStep("[benchmark] Checking Gateway status")
		err := retry.UntilSuccess(ctx, retry.Options{
			Timeout:  2 * time.Minute,
			Interval: 10 * time.Second,
			Name:     "gateway-programmed",
		}, func() error {
			out, err := dep.Kubectl(ctx, "get", "gateway", "-n", benchNamespace,
				"-o", "jsonpath={range .items[*]}{.metadata.name}={range .status.conditions[*]}{.type}:{.status},{end}{\"\\n\"}{end}")
			if err != nil {
				return fmt.Errorf("getting gateway: %w", err)
			}
			out = strings.TrimSpace(out)
			if out == "" {
				return fmt.Errorf("no Gateway found in namespace %s", benchNamespace)
			}
			if !strings.Contains(out, "Programmed:True") {
				logStep("[benchmark]   Gateway status: %s", out)
				return fmt.Errorf("Gateway not yet Programmed")
			}
			logStep("[benchmark] Gateway is Programmed: %s", out)
			return nil
		})
		Expect(err).NotTo(HaveOccurred(), "Gateway should be Programmed")
	})

	It("should have HTTPRoute accepted", func() {
		logStep("[benchmark] Checking HTTPRoute status")
		out, err := dep.Kubectl(ctx, "get", "httproute", "-n", benchNamespace,
			"-o", "jsonpath={range .items[*]}{.metadata.name}={range .status.parents[*].conditions[*]}{.type}:{.status},{end}{\"\\n\"}{end}")
		Expect(err).NotTo(HaveOccurred(), "getting HTTPRoute")
		out = strings.TrimSpace(out)
		Expect(out).NotTo(BeEmpty(), "no HTTPRoute found in namespace")
		logStep("[benchmark] HTTPRoute status: %s", out)
		Expect(out).To(ContainSubstring("Accepted:True"), "HTTPRoute should be Accepted")
		Expect(out).To(ContainSubstring("ResolvedRefs:True"), "HTTPRoute should have ResolvedRefs")
	})

	It("should have InferencePool", func() {
		logStep("[benchmark] Checking InferencePool")
		out, err := dep.Kubectl(ctx, "get", "inferencepool", "-n", benchNamespace,
			"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		Expect(err).NotTo(HaveOccurred(), "getting InferencePool")
		out = strings.TrimSpace(out)
		Expect(out).NotTo(BeEmpty(), "no InferencePool found in namespace")
		logStep("[benchmark] InferencePool: %s", out)
	})

	// ── Phase 4: RUN BENCHMARK ───────────────────────────────────
	It("should complete benchmark", func() {
		if !useSmokeTestJob {
			Skip("using external --endpoint, skipping in-cluster benchmark")
		}

		// Build benchmark Job (GuideLLM if image configured, curl smoke test otherwise)
		jobCfg := deployer.BenchmarkJobConfig{
			Name: benchmarkJobName, Namespace: benchNamespace,
			Target: targetURL, Model: modelName,
			Image: benchmarkImage, Data: benchmarkData,
			Rate: benchmarkRate, MaxSeconds: benchmarkMaxS,
		}
		if jobCfg.Image != "" {
			logStep("[benchmark] Creating GuideLLM benchmark Job %s (image=%s, data=%s, rate=%d, max-seconds=%d)",
				benchmarkJobName, jobCfg.Image, jobCfg.Data, jobCfg.Rate, jobCfg.MaxSeconds)
		} else {
			logStep("[benchmark] Creating smoke-test Job %s (target=%s)", benchmarkJobName, targetURL)
		}

		job := deployer.BuildBenchmarkJob(jobCfg)
		Expect(dep.ApplyResources(ctx, []*deployer.K8sResource{job}, benchNamespace)).To(Succeed(), "applying benchmark Job")

		timeout := 15 * time.Minute
		if jobCfg.Image != "" {
			timeout = 60 * time.Minute // GuideLLM needs more time
		}
		logStep("[benchmark] Waiting for benchmark Job %s to complete", benchmarkJobName)
		err := dep.WaitForJobCompletion(ctx, benchmarkJobName, benchNamespace, timeout)
		Expect(err).NotTo(HaveOccurred(), "benchmark Job failed")

		logs, _ := dep.GetJobLogs(ctx, benchmarkJobName, benchNamespace)
		logStep("[benchmark] Benchmark output:\n%s", logs)
	})

	// ── Phase 5: SCRAPE METRICS ─────────────────────────────────
	It("should scrape metrics from pods", func() {
		if !useSmokeTestJob {
			Skip("metrics scraping requires in-cluster benchmark")
		}

		scenarioDir = filepath.Base(filepath.Dir(helmfilePath))

		scraper := &metrics.Scraper{
			Kubectl:   dep.Kubectl,
			Namespace: benchNamespace,
			LogFunc:   logStep,
		}

		allPods, _ := dep.Kubectl(ctx, "get", "pods", "-n", benchNamespace,
			"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")

		// Scrape vLLM pods
		logStep("[benchmark] Scraping vLLM metrics")
		for _, pod := range strings.Split(strings.TrimSpace(allPods), "\n") {
			pod = strings.TrimSpace(pod)
			if pod != "" && (strings.Contains(pod, "decode") || strings.Contains(pod, "prefill")) {
				result, err := scraper.ScrapePod(ctx, pod, 8000)
				if err != nil {
					logStep("[benchmark]   WARNING: scrape %s failed: %v", pod, err)
					continue
				}
				logStep("[benchmark]   scraped %s (%d metrics)", pod, len(result.GetAllValues(metrics.MetricRequestSuccess))+1)
				vllmMetrics = append(vllmMetrics, result)
			}
		}

		// Scrape EPP pods
		logStep("[benchmark] Scraping EPP metrics")
		for _, pod := range strings.Split(strings.TrimSpace(allPods), "\n") {
			pod = strings.TrimSpace(pod)
			if pod != "" && strings.Contains(pod, "epp") {
				result, err := scraper.ScrapePod(ctx, pod, 9090)
				if err != nil {
					result, err = scraper.ScrapePod(ctx, pod, 8080)
				}
				if err != nil {
					logStep("[benchmark]   WARNING: scrape %s failed: %v", pod, err)
					continue
				}
				logStep("[benchmark]   scraped %s", pod)
				eppMetrics = append(eppMetrics, result)
			}
		}

		logStep("[benchmark] Scraped %d vLLM pod(s), %d EPP pod(s)", len(vllmMetrics), len(eppMetrics))
	})

	// ── Phase 5a: vllm:request_success_total ─────────────────────
	It("vllm:request_success_total should be > 0", func() {
		if len(vllmMetrics) == 0 {
			Skip("no vLLM metrics scraped")
		}
		checks := metrics.ValidatePDMetrics(vllmMetrics)
		for _, c := range checks {
			if c.Metric == metrics.MetricRequestSuccess {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("vllm:request_success_total not found")
	})

	// ── Phase 5b: vllm:prompt_tokens_total ───────────────────────
	It("vllm:prompt_tokens_total should be > 0", func() {
		if len(vllmMetrics) == 0 {
			Skip("no vLLM metrics scraped")
		}
		checks := metrics.ValidatePDMetrics(vllmMetrics)
		for _, c := range checks {
			if c.Metric == metrics.MetricPromptTokens {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("vllm:prompt_tokens_total not found")
	})

	// ── Phase 5c: vllm:generation_tokens_total ───────────────────
	It("vllm:generation_tokens_total should be > 0", func() {
		if len(vllmMetrics) == 0 {
			Skip("no vLLM metrics scraped")
		}
		checks := metrics.ValidatePDMetrics(vllmMetrics)
		for _, c := range checks {
			if c.Metric == metrics.MetricGenTokens {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("vllm:generation_tokens_total not found")
	})

	// ── Phase 5d: vllm:prefix_cache_queries ──────────────────────
	It("vllm:prefix_cache_queries should be > 0 (cache-aware routing)", func() {
		if scenarioDir != "is-vanilla" && scenarioDir != "is-balanced" {
			Skip("prefix cache check only for IS scenarios")
		}
		if len(vllmMetrics) == 0 {
			Skip("no vLLM metrics scraped")
		}
		checks := metrics.ValidateCacheAwareMetrics(vllmMetrics, nil)
		for _, c := range checks {
			if c.Metric == metrics.MetricPrefixCacheQueries {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("vllm:prefix_cache_queries not found")
	})

	// ── Phase 5e: vllm:prefix_cache_hits ─────────────────────────
	It("vllm:prefix_cache_hits should be > 0 (cache hits from repeated prefix)", func() {
		if scenarioDir != "is-vanilla" && scenarioDir != "is-balanced" {
			Skip("prefix cache check only for IS scenarios")
		}
		if len(vllmMetrics) == 0 {
			Skip("no vLLM metrics scraped")
		}
		checks := metrics.ValidateCacheAwareMetrics(vllmMetrics, nil)
		for _, c := range checks {
			if c.Metric == metrics.MetricPrefixCacheHits {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("vllm:prefix_cache_hits not found")
	})

	// ── Phase 5f: vllm:gpu_cache_usage_perc ──────────────────────
	It("vllm:gpu_cache_usage_perc should be > 0 (KV cache in use)", func() {
		if len(vllmMetrics) == 0 {
			Skip("no vLLM metrics scraped")
		}
		checks := metrics.ValidateCacheAwareMetrics(vllmMetrics, nil)
		for _, c := range checks {
			if c.Metric == metrics.MetricGPUCacheUsage {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("vllm:gpu_cache_usage_perc not found")
	})

	// ── Phase 5g: nixl:kv_transfer_count_total (P/D only) ────────
	It("nixl:kv_transfer_count_total should be > 0 (NIXL KV transfers)", func() {
		if scenarioDir != "pd" {
			Skip("NIXL check only for P/D scenarios")
		}
		if len(vllmMetrics) == 0 {
			Skip("no vLLM metrics scraped")
		}
		checks := metrics.ValidatePDMetrics(vllmMetrics)
		for _, c := range checks {
			if c.Metric == metrics.MetricNIXLTransfers {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		logStep("[benchmark] WARNING: NIXL transfer metrics not available")
		AddReportEntry("warning", "NIXL metrics not available — may not be supported in this vLLM version")
	})

	// ── Phase 5h: nixl:kv_transfer_failures_total (P/D only) ─────
	It("nixl:kv_transfer_failures_total should be 0 (no KV transfer failures)", func() {
		if scenarioDir != "pd" {
			Skip("NIXL check only for P/D scenarios")
		}
		if len(vllmMetrics) == 0 {
			Skip("no vLLM metrics scraped")
		}
		checks := metrics.ValidatePDMetrics(vllmMetrics)
		for _, c := range checks {
			if c.Metric == metrics.MetricNIXLFailures {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		logStep("[benchmark] WARNING: NIXL failure metrics not available")
		AddReportEntry("warning", "NIXL failure metrics not available")
	})

	// ── Phase 5i: scheduler_e2e_duration ─────────────────────────
	It("inference_extension_scheduler_e2e_duration should be > 0", func() {
		if len(eppMetrics) == 0 {
			Skip("no EPP metrics scraped")
		}
		checks := metrics.ValidateSchedulerMetrics(eppMetrics)
		for _, c := range checks {
			if c.Metric == metrics.MetricSchedulerE2E {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("inference_extension_scheduler_e2e_duration not found")
	})

	// ── Phase 5j: inference_objective_request_total ──────────────
	It("inference_objective_request_total should be > 0", func() {
		if len(eppMetrics) == 0 {
			Skip("no EPP metrics scraped")
		}
		checks := metrics.ValidateSchedulerMetrics(eppMetrics)
		for _, c := range checks {
			if c.Metric == metrics.MetricRequestTotal {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("inference_objective_request_total not found")
	})

	// ── Phase 5k: inference_objective_request_error_total ────────
	It("inference_objective_request_error_total should be 0", func() {
		if len(eppMetrics) == 0 {
			Skip("no EPP metrics scraped")
		}
		checks := metrics.ValidateSchedulerMetrics(eppMetrics)
		for _, c := range checks {
			if c.Metric == metrics.MetricRequestErrorTotal {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("inference_objective_request_error_total not found")
	})

	// ── Phase 5l: inference_pool_ready_pods ──────────────────────
	It("inference_pool_ready_pods should be > 0", func() {
		if len(eppMetrics) == 0 {
			Skip("no EPP metrics scraped")
		}
		checks := metrics.ValidateSchedulerMetrics(eppMetrics)
		for _, c := range checks {
			if c.Metric == metrics.MetricPoolReadyPods {
				logStep("[benchmark] %s", c.Message)
				if !c.Passed {
					Fail(c.Message)
				}
				return
			}
		}
		Skip("inference_pool_ready_pods not found")
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

		// Clean up standalone benchmark Job
		if useSmokeTestJob {
			_, _ = dep.Kubectl(ctx, "delete", "job", benchmarkJobName, "-n", benchNamespace, "--ignore-not-found=true")
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

// getPodLogsByPattern collects logs from pods matching a name pattern in the namespace.
func getPodLogsByPattern(dep *deployer.Deployer, ctx context.Context, namespace, pattern string) string {
	allPods, _ := dep.Kubectl(ctx, "get", "pods", "-n", namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	var logs string
	for _, pod := range strings.Split(strings.TrimSpace(allPods), "\n") {
		pod = strings.TrimSpace(pod)
		if pod != "" && strings.Contains(pod, pattern) {
			out, _ := dep.Kubectl(ctx, "logs", pod, "-n", namespace,
				"--all-containers=true", "--tail=200")
			logs += out
		}
	}
	return logs
}
