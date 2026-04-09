package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	"github.com/aneeshkp/llm-d-conformance-test/framework/cleanup"
	"github.com/aneeshkp/llm-d-conformance-test/framework/client"
	"github.com/aneeshkp/llm-d-conformance-test/framework/config"
	"github.com/aneeshkp/llm-d-conformance-test/framework/deployer"
	"github.com/aneeshkp/llm-d-conformance-test/framework/metrics"
	"github.com/aneeshkp/llm-d-conformance-test/framework/model"
	"github.com/aneeshkp/llm-d-conformance-test/framework/reporter"
	"github.com/aneeshkp/llm-d-conformance-test/framework/retry"
)

var (
	ctx        context.Context
	dep        *deployer.Deployer
	dl         *model.Downloader
	cleanupMgr *cleanup.Manager
	rep        *reporter.Reporter
	testCases  []*config.TestCase
)

var _ = BeforeSuite(func() {
	ctx = context.Background()

	// Verify manifests are available (cloned via 'make setup')
	// Skip when running benchmark tests (helmfile-based) or discover mode.
	if testMode != "discover" && helmfilePath == "" {
		manifestDir := filepath.Join(findRootDir(), "deploy", "manifests")
		entries, _ := filepath.Glob(filepath.Join(manifestDir, "*.yaml"))
		if len(entries) == 0 {
			Fail("No manifests found in deploy/manifests/ — run 'make setup' to clone the manifest repo first")
		}
	}

	// Resolve platform
	p := deployer.Platform(platform)

	// Create deployer
	dep = deployer.New(kubeconfig, p, namespace)
	dep.MockImage = mockImage
	dep.PullSecretName = pullSecretName
	dep.LogFunc = func(format string, args ...interface{}) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}

	// Create model downloader for PVC-based caching
	dl = model.NewDownloader(kubeconfig, namespace, storageClass, storageSize, platform, modelSource)
	dl.LogFunc = func(format string, args ...interface{}) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}

	// Create cleanup manager
	cleanupMgr = cleanup.NewManager(dep)

	// Load filtered test cases (for profile support and reporter)
	var err error
	testCases, err = resolveTestCases()
	if err != nil {
		GinkgoWriter.Printf("Warning: failed to resolve test cases: %v\n", err)
	}
	// Build O(1) lookup set for profile filtering
	resolvedTestCaseSet = make(map[string]bool, len(testCases))
	for _, tc := range testCases {
		resolvedTestCaseSet[tc.Name] = true
	}

	// Determine profile name for reporting
	profileName := "custom"
	if profilePath != "" {
		profile, err := config.LoadProfile(profilePath)
		if err == nil {
			profileName = profile.Name
		}
	}

	// Create reporter
	rep = reporter.New(reportDir, "llm-d-conformance", profileName, string(p))

	// Collect environment info
	info := dep.GetPlatformInfo(ctx)
	rep.SetEnvironment(reporter.EnvironmentInfo{
		Platform:          string(p),
		KubernetesVersion: info["kubeletVersion"],
		Namespace:         namespace,
		Extra:             info,
	})

	GinkgoWriter.Printf("Loaded %d test cases for platform %s (mode=%s)\n", len(testCases), p, testMode)
	if testMode == "discover" {
		GinkgoWriter.Printf("Discover mode: will validate existing deployment at endpoint %s\n", endpoint)
	}
})

var _ = AfterSuite(func() {
	// Cleanup all remaining resources (skip if noCleanup flag is set)
	if cleanupMgr != nil && !noCleanup {
		errs := cleanupMgr.CleanupAll(ctx)
		for _, err := range errs {
			GinkgoWriter.Printf("Cleanup error: %v\n", err)
		}
	}

	// Finalize report
	if rep != nil {
		path, err := rep.Finalize()
		if err != nil {
			GinkgoWriter.Printf("Failed to write report: %v\n", err)
		} else {
			GinkgoWriter.Printf("Report written to: %s\n", path)
		}
	}
})

// loadAllTestCases loads and pre-filters test cases at spec construction time.
// This runs during Describe (before BeforeSuite), so we use findRootDir directly.
// Pre-filtering avoids creating hundreds of skipped Ginkgo specs for test cases
// that won't run (e.g., when using TESTCASE= or PROFILE=).
func loadAllTestCases() []*config.TestCase {
	rootDir := findRootDir()
	dir := filepath.Join(rootDir, "configs", "testcases")
	cases, err := config.LoadTestCasesFromDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: failed to load test cases: %v\n", err)
		return nil
	}

	// Pre-filter by TESTCASE flag
	if testCaseName != "" {
		names := strings.Split(testCaseName, ",")
		nameSet := make(map[string]bool, len(names))
		for _, n := range names {
			nameSet[strings.TrimSpace(n)] = true
		}
		var filtered []*config.TestCase
		for _, tc := range cases {
			if nameSet[tc.Name] {
				filtered = append(filtered, tc)
			}
		}
		return filtered
	}

	// Pre-filter by PROFILE flag
	if profilePath != "" {
		resolved := resolveRelativePath(profilePath)
		profile, err := config.LoadProfile(resolved)
		if err == nil {
			profileCases, err := config.ResolveProfileTestCases(profile, dir)
			if err == nil {
				return profileCases
			}
		}
	}

	return cases
}

// shouldRunTestCase checks if a test case matches the runtime filters.
// shouldRunTestCase is a defensive check — loadAllTestCases already pre-filters
// by TESTCASE and PROFILE, so this mainly handles edge cases.
func shouldRunTestCase(tc *config.TestCase) bool {
	if profilePath != "" && len(testCases) > 0 {
		if _, ok := resolvedTestCaseSet[tc.Name]; !ok {
			return false
		}
	}

	return true
}

// skipIfDiscover skips the current spec if running in discover mode.
func skipIfDiscover() {
	if testMode == "discover" {
		Skip("discover mode — using existing deployment")
	}
}

// skipIfCache skips the current spec if running in cache mode (download only).
func skipIfCache() {
	if testMode == "cache" {
		Skip("cache mode — download only, skipping deployment and validation")
	}
}

// resolvedTestCaseSet is populated in BeforeSuite for O(1) profile filtering.
var resolvedTestCaseSet map[string]bool
var preCleanupDone bool
var vllmVersionCaptured bool

var _ = Describe("LLM-D Conformance", func() {
	allCases := loadAllTestCases()

	for _, tc := range allCases {
		tc := tc // capture range variable

		// Convert labels to Ginkgo Labels (safe copy to avoid slice mutation)
		allLabels := make([]string, 0, len(tc.Labels)+1)
		allLabels = append(allLabels, tc.Labels...)
		allLabels = append(allLabels, tc.Model.Category)
		ginkgoLabels := Label(allLabels...)

		// Each test case is an Ordered container — phases run sequentially,
		// and if one fails, subsequent phases are skipped.
		Describe(tc.Name, Ordered, ginkgoLabels, func() {
			var (
				svcEndpoint  string
				llmClient    *client.LLMClient
				start        time.Time
				shouldRun    bool
				modelInfo    reporter.ModelInfo
				vllmMetrics  []*metrics.ScrapeResult
				eppMetrics   []*metrics.ScrapeResult
			)

			BeforeAll(func() {
				shouldRun = shouldRunTestCase(tc)
				if !shouldRun {
					Skip(fmt.Sprintf("filtered out by flags (testcase=%s, labels=%s)", testCaseName, labels))
				}
				// Resolve manifest path (flat directory — cloned from manifest repo)
				if tc.Deployment.ManifestPath != "" && !filepath.IsAbs(tc.Deployment.ManifestPath) {
					rootDir := findRootDir()
					manifestDir := filepath.Join(rootDir, "deploy", "manifests")
					resolved := filepath.Join(manifestDir, tc.Deployment.ManifestPath)
					if _, err := os.Stat(resolved); os.IsNotExist(err) {
						Fail(fmt.Sprintf("manifest %s not found at %s — run 'make setup' to clone the manifest repo", tc.Deployment.ManifestPath, manifestDir))
					}
					tc.Deployment.ManifestPath = resolved
				}
				start = time.Now()

				// Apply model override if provided
				if modelOverride != "" {
					tc.Model.Name = modelOverride
					tc.Model.URI = "hf://" + modelOverride
				}

				// Set the model URI based on model source — deployer patches the manifest with this.
				// In cache mode, keep the original URI so PREP doesn't skip (it checks for pvc:// prefix).
				if testMode != "cache" && (modelSource == "pvc" || modelSource == "pvc-snapshot") {
					tc.Model.URI = dl.PVCModelURI(tc)
				}
				// For hf, tc.Model.URI already has hf:// from the test case config

				modelInfo = reporter.ModelInfo{
					Name:     tc.Model.Name,
					URI:      tc.Model.URI,
					Category: tc.Model.Category,
				}
				logStep("Testing: %s (%s) [mode=%s, model-source=%s]", tc.Name, tc.Description, testMode, modelSource)

				// Pre-cleanup: delete all leftover LLMInferenceServices to free GPUs (first test case only)
				if testMode != "cache" && testMode != "discover" && !preCleanupDone {
					logStep("[%s] Pre-cleanup: deleting any existing LLMInferenceServices", tc.Name)
					_, _ = dep.Kubectl(ctx, "delete", "llmisvc", "--all", "-n", dep.Namespace, "--ignore-not-found", "--wait=true", "--timeout=120s")
					preCleanupDone = true
				}
			})

			// Record each phase result directly to reporter
			AfterEach(func() {
				if !shouldRun {
					return
				}
				report := CurrentSpecReport()
				status := reporter.StatusPass
				if report.Failed() {
					status = reporter.StatusFail
				} else if report.State.String() == "skipped" {
					status = reporter.StatusSkip
				}
				if rep != nil {
					rep.AddResult(reporter.TestResult{
						Name:        fmt.Sprintf("%s/%s", tc.Name, report.LeafNodeText),
						Description: tc.Description,
						Category:    tc.Model.Category,
						Status:      status,
						StartTime:   start,
						EndTime:     time.Now(),
						Duration:    time.Since(start).String(),
						Model:       modelInfo,
					})
				}
			})

			// ── Phase 1: PREP ──────────────────────────────────────
			It("should download model to PVC cache", func() {
				skipIfDiscover()
				if modelSource == "hf" {
					Skip("model-source=hf — vLLM downloads at startup")
				}
				if strings.HasPrefix(tc.Model.URI, "pvc://") {
					Skip(fmt.Sprintf("model URI is already pvc:// (%s)", tc.Model.URI))
				}
				if tc.Model.Cache == nil || !tc.Model.Cache.Enabled {
					Skip("cache not enabled for this test case")
				}

				logStep("[%s] Phase 1: PREP — Downloading model to PVC cache", tc.Name)
				cacheResult := dl.DownloadModel(ctx, tc)
				if cacheResult.Status == model.CacheStatusFailed {
					Fail(fmt.Sprintf("Model download failed: %v", cacheResult.Error))
				}
				if cacheResult.Status == model.CacheStatusNotFound {
					Fail(fmt.Sprintf("Model PVC not found: %v", cacheResult.Error))
				}
				logStep("[%s] PREP PASSED: model cached in PVC %s (%s)", tc.Name, cacheResult.PVCName, cacheResult.Duration)
			})

			// ── Phase 2: PREREQ ────────────────────────────────────
			It("should have LLMInferenceService CRD installed", func() {
				skipIfDiscover()
				skipIfCache()
				logStep("[%s] Phase 2: PREREQ — Checking CRD", tc.Name)
				crdExists, err := dep.CheckCRDExists(ctx, "llminferenceservices.serving.kserve.io")
				if err != nil || !crdExists {
					Fail(fmt.Sprintf("LLMInferenceService CRD not found: %v", err))
				}
			})

			// ── Phase 3: DEPLOY ────────────────────────────────────
			It("should deploy LLMInferenceService manifest", func() {
				skipIfDiscover()
				skipIfCache()
				logStep("[%s] Phase 3: DEPLOY — Applying manifest (model-source=%s)", tc.Name, modelSource)
				deployResult := dep.Deploy(ctx, tc)
				if !deployResult.Success {
					Fail(fmt.Sprintf("kubectl apply failed: %v", deployResult.Error))
				}
				cleanupMgr.Track(tc)
				logStep("[%s] DEPLOY PASSED in %s", tc.Name, deployResult.Duration)

				// Update modelInfo with the actual deployed URI (pvc:// or hf://)
				name := deployer.ExtractLLMISVCName(tc)
				deployedURI, _ := dep.Kubectl(ctx, "get", "llmisvc", name, "-n", dep.Namespace,
					"-o", "jsonpath={.spec.model.uri}")
				if uri := strings.TrimSpace(deployedURI); uri != "" {
					modelInfo.URI = uri
					logStep("[%s]   Model URI: %s", tc.Name, uri)
				}
			})

			// ── Phase 4a: Service created ──────────────────────────
			It("should create Service", func() {
				skipIfDiscover()
				skipIfCache()
				name := deployer.ExtractLLMISVCName(tc)
				logStep("[%s] Checking Service", tc.Name)

				err := retry.UntilSuccess(ctx, retry.Options{
					Timeout:  2 * time.Minute,
					Interval: 5 * time.Second,
					Name:     "service-" + name,
				}, func() error {
					ok, _ := dep.CheckResourceExists(ctx, "svc", name, dep.Namespace)
					if !ok {
						return fmt.Errorf("no Service found for %s", name)
					}
					return nil
				})
				if err != nil {
					Fail(fmt.Sprintf("Service not created: %v", err))
				}
			})

			// ── Phase 4b: HTTPRoute created ────────────────────────
			It("should create HTTPRoute", func() {
				skipIfDiscover()
				skipIfCache()
				name := deployer.ExtractLLMISVCName(tc)
				logStep("[%s] Checking HTTPRoute", tc.Name)

				err := retry.UntilSuccess(ctx, retry.Options{
					Timeout:  2 * time.Minute,
					Interval: 5 * time.Second,
					Name:     "httproute-" + name,
				}, func() error {
					ok, _ := dep.CheckResourceExists(ctx, "httproute", name, dep.Namespace)
					if !ok {
						return fmt.Errorf("no HTTPRoute found for %s", name)
					}
					return nil
				})
				if err != nil {
					Fail(fmt.Sprintf("HTTPRoute not created: %v", err))
				}
			})

			// ── Phase 4c: Gateway programmed ──────────────────────
			It("should have Gateway programmed with address", func() {
				skipIfDiscover()
				skipIfCache()
				logStep("[%s] Checking Gateway is Programmed", tc.Name)

				err := retry.UntilSuccess(ctx, retry.Options{
					Timeout:  2 * time.Minute,
					Interval: 5 * time.Second,
					Name:     "gateway-programmed",
				}, func() error {
					// Find gateway referenced by the LLMInferenceService (check common namespaces)
					for _, gwNS := range []string{"opendatahub", "kserve", "istio-system", dep.Namespace} {
						out, err := dep.Kubectl(ctx, "get", "gateway", "-n", gwNS,
							"-o", "jsonpath={range .items[*]}{.metadata.name}|{.status.conditions[?(@.type==\"Programmed\")].status}|{.status.addresses[0].value}{\"\\n\"}{end}")
						if err != nil || strings.TrimSpace(out) == "" {
							continue
						}
						for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
							parts := strings.SplitN(line, "|", 3)
							if len(parts) < 3 {
								continue
							}
							gwName := strings.TrimSpace(parts[0])
							programmed := strings.TrimSpace(parts[1])
							address := strings.TrimSpace(parts[2])
							if strings.EqualFold(programmed, "True") && address != "" {
								logStep("[%s]   Gateway %s/%s: Programmed=True, address=%s", tc.Name, gwNS, gwName, address)
								return nil
							}
							return fmt.Errorf("Gateway %s/%s: Programmed=%s, address=%s", gwNS, gwName, programmed, address)
						}
					}
					return fmt.Errorf("no Gateway found in cluster")
				})
				if err != nil {
					Fail(fmt.Sprintf("Gateway not programmed: %v", err))
				}
			})

			// ── Phase 4d: HTTPRoute accepted by Gateway ───────────
			It("should have HTTPRoute accepted by Gateway (resolvedRefs=True)", func() {
				skipIfDiscover()
				skipIfCache()
				name := deployer.ExtractLLMISVCName(tc)
				logStep("[%s] Checking HTTPRoute is accepted by Gateway", tc.Name)

				err := retry.UntilSuccess(ctx, retry.Options{
					Timeout:  2 * time.Minute,
					Interval: 5 * time.Second,
					Name:     "httproute-accepted-" + name,
				}, func() error {
					// Get HTTPRoute parent status
					out, _ := dep.Kubectl(ctx, "get", "httproute", "-n", dep.Namespace, "-l",
						fmt.Sprintf("app.kubernetes.io/name=%s", name),
						"-o", "jsonpath={range .items[*]}{.metadata.name}|{.status.parents[0].conditions[?(@.type==\"Accepted\")].status}|{.status.parents[0].conditions[?(@.type==\"ResolvedRefs\")].status}{end}")
					out = strings.TrimSpace(out)
					if out == "" {
						return fmt.Errorf("HTTPRoute status not available yet")
					}
					parts := strings.SplitN(out, "|", 3)
					if len(parts) < 3 {
						return fmt.Errorf("unexpected HTTPRoute status format: %s", out)
					}
					routeName := strings.TrimSpace(parts[0])
					accepted := strings.TrimSpace(parts[1])
					resolvedRefs := strings.TrimSpace(parts[2])

					if !strings.EqualFold(accepted, "True") {
						return fmt.Errorf("HTTPRoute %s not accepted by Gateway (Accepted=%s)", routeName, accepted)
					}
					if !strings.EqualFold(resolvedRefs, "True") {
						return fmt.Errorf("HTTPRoute %s has unresolved refs (ResolvedRefs=%s)", routeName, resolvedRefs)
					}
					logStep("[%s]   HTTPRoute %s: Accepted=True, ResolvedRefs=True", tc.Name, routeName)
					return nil
				})
				if err != nil {
					Fail(fmt.Sprintf("HTTPRoute not accepted by Gateway: %v", err))
				}
			})

			// ── Phase 4e: InferencePool created ────────────────────
			It("should create InferencePool", func() {
				skipIfDiscover()
				skipIfCache()
				// No InferencePool without a scheduler
				if tc.Deployment.ManifestPath != "" {
					data, err := os.ReadFile(tc.Deployment.ManifestPath)
					if err == nil && !strings.Contains(string(data), "scheduler:") {
						Skip("no scheduler in manifest — no InferencePool expected")
					}
				}
				logStep("[%s] Checking InferencePool", tc.Name)

				err := retry.UntilSuccess(ctx, retry.Options{
					Timeout:  2 * time.Minute,
					Interval: 5 * time.Second,
					Name:     "inferencepool-" + tc.Name,
				}, func() error {
					ok, _ := dep.CheckInferencePoolExists(ctx, dep.Namespace)
					if !ok {
						return fmt.Errorf("no InferencePool found")
					}
					return nil
				})
				if err != nil {
					Fail(fmt.Sprintf("InferencePool not created: %v", err))
				}
			})

			// ── Phase 4d: Pods running ─────────────────────────────
			It("should have pods running without crashes", func() {
				skipIfDiscover()
				skipIfCache()
				name := deployer.ExtractLLMISVCName(tc)
				logStep("[%s] Checking pods (waiting for Running state, detecting crashes)", tc.Name)

				err := retry.UntilSuccess(ctx, retry.Options{
					Timeout:  5 * time.Minute,
					Interval: 10 * time.Second,
					Name:     "pods-" + name,
				}, func() error {
					// Get pod status: name, phase, ready, restarts
					out, _ := dep.Kubectl(ctx, "get", "pods", "-n", dep.Namespace, "-l",
						fmt.Sprintf("app.kubernetes.io/name=%s", name),
						"-o", "jsonpath={range .items[*]}{.metadata.name} phase={.status.phase} ready={.status.containerStatuses[0].ready} restarts={.status.containerStatuses[0].restartCount} reason={.status.containerStatuses[0].state.waiting.reason}{\"\\n\"}{end}")
					pods := strings.TrimSpace(out)
					if pods == "" {
						return fmt.Errorf("no pods found yet for %s", name)
					}
					fetchLogs := func(podName string) string {
						logs, _ := dep.Kubectl(ctx, "logs", podName, "-n", dep.Namespace, "--tail=10", "--all-containers=true")
						return strings.TrimSpace(logs)
					}

					allRunning := true
					for _, line := range strings.Split(pods, "\n") {
						line = strings.TrimSpace(line)
						if line == "" {
							continue
						}
						logStep("[%s]   %s", tc.Name, line)

						// Parse fields from the jsonpath output
						fields := make(map[string]string)
						for _, part := range strings.Fields(line) {
							if k, v, ok := strings.Cut(part, "="); ok {
								fields[k] = v
							}
						}
						podName := strings.Fields(line)[0]

						// Detect crashes immediately via reason field
						reason := fields["reason"]
						if reason == "CrashLoopBackOff" || reason == "Error" || reason == "CreateContainerError" {
							Fail(fmt.Sprintf("Pod crash detected: %s\nLogs:\n%s", line, fetchLogs(podName)))
						}

						// Check restarts > 1 (allow 0-1 for init)
						if count := fields["restarts"]; count != "" && count != "0" && count != "1" {
							Fail(fmt.Sprintf("Pod restarting (restarts=%s): %s\nLogs:\n%s", count, line, fetchLogs(podName)))
						}

						if fields["phase"] != "Running" {
							allRunning = false
						}
					}
					if !allRunning {
						return fmt.Errorf("not all pods Running yet")
					}
					return nil
				})
				if err != nil {
					// On timeout, get detailed pod description
					desc, _ := dep.Kubectl(ctx, "describe", "pods", "-n", dep.Namespace, "-l",
						fmt.Sprintf("app.kubernetes.io/name=%s", name))
					Fail(fmt.Sprintf("Pods not running after timeout: %v\n\nPod details:\n%s", err, strings.TrimSpace(desc)))
				}
			})

			// Note: storage-initializer completion is implicitly verified by the READY phase below.
			// KServe injects storage-initializer as an init container — the pod won't become Ready
			// until the init container completes. No need to check separately.

			// ── Phase 4f: READY ────────────────────────────────────
			It("should become READY", func() {
				skipIfDiscover()
				skipIfCache()
				logStep("[%s] Phase 4: READY — Waiting for service readiness (timeout=%s)", tc.Name, tc.Deployment.ReadyTimeout.Duration)
				err := dep.WaitForReady(ctx, tc)
				if err != nil {
					Fail(fmt.Sprintf("Service did not become ready: %v", err))
				}
				// Capture vLLM image and version (once per run)
				vllmInfo := dep.GetVLLMVersion(ctx)
				if img, ok := vllmInfo["vllmImage"]; ok {
					modelInfo.VLLMImage = img
					logStep("[%s]   vLLM image: %s", tc.Name, img)
					// Update report environment info (first time only)
					if rep != nil {
						rep.UpdateExtra("vllmImage", img)
						if ver, ok := vllmInfo["vllmVersion"]; ok {
							rep.UpdateExtra("vllmVersion", ver)
						}
					}
				}

				// Get version from inside the running pod
				name := deployer.ExtractLLMISVCName(tc)
				label := fmt.Sprintf(metrics.WorkloadLabelFmt, name)
				podName, _ := dep.Kubectl(ctx, "get", "pods", "-n", dep.Namespace, "-l", label,
					"-o", "jsonpath={.items[0].metadata.name}")
				if pod := strings.TrimSpace(podName); pod != "" {
					ver, _ := dep.Kubectl(ctx, "exec", pod, "-n", dep.Namespace, "-c", "main",
						"--", "python3", "-c", "import vllm; print(vllm.__version__)")
					// Extract just the version line (filter warnings/noise)
					for _, line := range strings.Split(ver, "\n") {
						line = strings.TrimSpace(line)
						if line != "" &&
							!strings.Contains(line, "Warning") &&
							!strings.Contains(line, "Error") &&
							!strings.Contains(line, "import") &&
							!strings.Contains(line, "Traceback") {
							modelInfo.VLLMVersion = line
							logStep("[%s]   vLLM version: %s", tc.Name, line)
							break
						}
					}
				}
			})

			// ── Phase 5: Model files at /mnt/models ────────────────
			It("should have model files at /mnt/models", func() {
				skipIfDiscover()
				skipIfCache()
				name := deployer.ExtractLLMISVCName(tc)
				logStep("[%s] Checking model files at /mnt/models", tc.Name)

				// Get pod name first, then exec into it
				label := fmt.Sprintf(metrics.WorkloadLabelFmt, name)
				podName, _ := dep.Kubectl(ctx, "get", "pods", "-n", dep.Namespace, "-l", label,
					"-o", "jsonpath={.items[0].metadata.name}")
				pod := strings.TrimSpace(podName)
				if pod == "" {
					Fail("No vLLM pod found to check /mnt/models")
				}

				out, err := dep.Kubectl(ctx, "exec", pod, "-n", dep.Namespace, "-c", "main",
					"--", "ls", "/mnt/models/")
				if err != nil {
					Fail(fmt.Sprintf("Failed to list /mnt/models: %v", err))
				}
				files := strings.TrimSpace(out)
				if files == "" {
					Fail("/mnt/models is empty — model not found")
				}

				// Check for config.json (required by vLLM)
				if !strings.Contains(files, "config.json") {
					Fail(fmt.Sprintf("/mnt/models missing config.json, found: %s", files))
				}

				// Check for model weights
				hasWeights := strings.Contains(files, ".safetensors") ||
					strings.Contains(files, ".bin") ||
					strings.Contains(files, ".gguf")
				if !hasWeights {
					Fail(fmt.Sprintf("/mnt/models missing model weights (.safetensors/.bin/.gguf), found: %s", files))
				}

				logStep("[%s]   /mnt/models contents: %s", tc.Name, strings.ReplaceAll(files, "\n", ", "))
			})

			// ── Phase 6: HEALTH ────────────────────────────────────
			It("should pass health check", func() {
				skipIfCache()
				logStep("[%s] Phase 6: HEALTH — Validating /health endpoint", tc.Name)

				var err error
				svcEndpoint, err = dep.GetServiceEndpoint(ctx, tc)
				if err != nil {
					if endpoint != "" {
						svcEndpoint = endpoint
					} else {
						Fail(fmt.Sprintf("Could not get service endpoint: %v", err))
					}
				}
				logStep("[%s]   Endpoint: %s", tc.Name, svcEndpoint)

				llmClient = client.New(svcEndpoint)
				err = retry.UntilSuccess(ctx, retry.Options{
					Timeout:  tc.Validation.Timeout.Duration,
					Interval: tc.Validation.RetryInterval.Duration,
					Name:     "health-" + tc.Name,
				}, func() error {
					return llmClient.HealthCheck(ctx)
				})
				if err != nil {
					Fail(fmt.Sprintf("/health failed: %v", err))
				}

				// Capture vLLM version (once per run, any mode)
				if rep != nil && !vllmVersionCaptured {
					vllmInfo := dep.GetVLLMVersion(ctx)
					if img, ok := vllmInfo["vllmImage"]; ok {
						rep.UpdateExtra("vllmImage", img)
						logStep("[%s]   vLLM image: %s", tc.Name, img)
					}
					if ver, ok := vllmInfo["vllmVersion"]; ok {
						rep.UpdateExtra("vllmVersion", ver)
						logStep("[%s]   vLLM version: %s", tc.Name, ver)
					}
					vllmVersionCaptured = true
				}
			})

			// ── Phase 6: MODEL ─────────────────────────────────────
			It("should list model in /v1/models", func() {
				skipIfCache()
				logStep("[%s] Phase 6: MODEL — Validating model listing", tc.Name)
				models, err := llmClient.ListModels(ctx)
				if err != nil {
					Fail(fmt.Sprintf("/v1/models request failed: %v", err))
				}
				found := false
				var listedModels []string
				for _, m := range models.Data {
					listedModels = append(listedModels, m.ID)
					if m.ID == tc.Model.Name {
						found = true
					}
				}
				if !found {
					if modelOverride != "" {
						// User explicitly set MODEL= — don't override, let it fail
						logStep("[%s]   WARNING: model %s not found, available: %v", tc.Name, tc.Model.Name, listedModels)
					} else if len(listedModels) > 0 {
						// Auto-detect: use whatever model is running
						logStep("[%s]   Model %s not found, using %s instead", tc.Name, tc.Model.Name, listedModels[0])
						tc.Model.Name = listedModels[0]
					} else {
						logStep("[%s]   WARNING: no models listed at /v1/models", tc.Name)
					}
				} else {
					logStep("[%s]   Model %s found", tc.Name, tc.Model.Name)
				}
			})

			// ── Phase 7: INFERENCE ─────────────────────────────────
			It("should return inference response", func() {
				skipIfCache()
				hasChatPrompts := len(tc.Validation.ChatPrompts) > 0
				hasTestPrompts := len(tc.Validation.TestPrompts) > 0
				if !tc.Validation.InferenceCheck || (!hasTestPrompts && !hasChatPrompts) {
					Skip("inferenceCheck=false or no prompts configured")
				}

				// Structured chat prompts (system+user) — used for cache-aware tests
				if hasChatPrompts {
					logStep("[%s] Phase 7: INFERENCE — Running %d structured chat prompt(s)", tc.Name, len(tc.Validation.ChatPrompts))
					for i, cp := range tc.Validation.ChatPrompts {
						var messages []client.ChatMessage
						if cp.System != "" {
							messages = append(messages, client.ChatMessage{Role: "system", Content: cp.System})
						}
						messages = append(messages, client.ChatMessage{Role: "user", Content: cp.User})

						resp, err := llmClient.ChatCompletions(ctx, client.ChatRequest{
							Model:     tc.Model.Name,
							Messages:  messages,
							MaxTokens: 50,
						})
						if err != nil {
							Fail(fmt.Sprintf("chat[%d] %q failed: %v", i, cp.User, err))
						}
						if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
							Fail(fmt.Sprintf("chat[%d] %q returned empty response", i, cp.User))
						}
						logStep("[%s]   chat[%d] PASSED (tokens=%d, user=%q)", tc.Name, i, resp.Usage.TotalTokens, cp.User)

						// Wait between requests so the EPP prefix indexer can update
						// and route the next request to the pod that cached the prefix.
						if i < len(tc.Validation.ChatPrompts)-1 {
							logStep("[%s]   waiting 5s for EPP prefix index sync...", tc.Name)
							time.Sleep(5 * time.Second)
						}
					}
					return
				}

				// Simple string prompts — standard test cases
				logStep("[%s] Phase 7: INFERENCE — Running %d test prompt(s)", tc.Name, len(tc.Validation.TestPrompts))
				for i, prompt := range tc.Validation.TestPrompts {
					resp, err := llmClient.ChatCompletions(ctx, client.ChatRequest{
						Model: tc.Model.Name,
						Messages: []client.ChatMessage{
							{Role: "user", Content: prompt},
						},
						MaxTokens: 50,
					})
					if err != nil {
						compResp, compErr := llmClient.Completions(ctx, client.CompletionRequest{
							Model:     tc.Model.Name,
							Prompt:    prompt,
							MaxTokens: 50,
						})
						if compErr != nil {
							Fail(fmt.Sprintf("prompt[%d] %q failed: chat=%v, completions=%v", i, prompt, err, compErr))
						}
						if len(compResp.Choices) == 0 || compResp.Choices[0].Text == "" {
							Fail(fmt.Sprintf("prompt[%d] %q returned empty response", i, prompt))
						}
						logStep("[%s]   prompt[%d] PASSED via /v1/completions (tokens=%d)", tc.Name, i, compResp.Usage.TotalTokens)
					} else {
						if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
							Fail(fmt.Sprintf("prompt[%d] %q returned empty response", i, prompt))
						}
						logStep("[%s]   prompt[%d] PASSED via /v1/chat/completions (tokens=%d)", tc.Name, i, resp.Usage.TotalTokens)
					}
				}
			})

			// ── Phase 8: METRICS — Scrape ─────────────────────────
			It("should scrape metrics from pods", func() {
				skipIfCache()
				if mockImage != "" {
					Skip("mock mode — no real vLLM metrics")
				}
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.Enabled {
					Skip("metricsCheck not enabled")
				}

				name := deployer.ExtractLLMISVCName(tc)
				scraper := &metrics.Scraper{
					Kubectl:   dep.Kubectl,
					Namespace: dep.Namespace,
					LogFunc:   logStep,
				}

				if mc.CheckVLLM || mc.CheckPD || mc.CheckPrefixCache {
					logStep("[%s] Phase 8: METRICS — Scraping vLLM pod metrics", tc.Name)
					var err error
					vllmMetrics, err = scraper.ScrapeVLLMPods(ctx, name)
					if err != nil {
						logStep("[%s]   WARNING: could not scrape vLLM metrics: %v", tc.Name, err)
					} else {
						logStep("[%s]   scraped %d vLLM pod(s)", tc.Name, len(vllmMetrics))
					}
				}

				if mc.CheckEPP || mc.CheckScheduler || mc.CheckPrefixCache {
					logStep("[%s]   scraping EPP scheduler metrics...", tc.Name)
					var err error
					eppMetrics, err = scraper.ScrapeEPPPods(ctx, name)
					if err != nil {
						logStep("[%s]   WARNING: could not scrape EPP metrics: %v", tc.Name, err)
					} else {
						logStep("[%s]   scraped %d EPP pod(s)", tc.Name, len(eppMetrics))
					}
				}
			})

			// ── Phase 8a: vllm:prefix_cache_queries ───────────────
			It("vllm:prefix_cache_queries should be > 0 (prefix cache lookups received)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckPrefixCache {
					Skip("prefix cache check not enabled")
				}
				checks := metrics.ValidateCacheAwareMetrics(vllmMetrics, nil)
				for _, c := range checks {
					if c.Metric == metrics.MetricPrefixCacheQueries {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("vllm:prefix_cache_queries not found in scraped metrics")
			})

			// ── Phase 8b: vllm:prefix_cache_hits ──────────────────
			It("vllm:prefix_cache_hits should be > 0 (cache hits from repeated prefix)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckPrefixCache {
					Skip("prefix cache check not enabled")
				}
				checks := metrics.ValidateCacheAwareMetrics(vllmMetrics, nil)
				for _, c := range checks {
					if c.Metric == metrics.MetricPrefixCacheHits {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("vllm:prefix_cache_hits not found in scraped metrics")
			})

			// ── Phase 8c: prefix cache hit rate ───────────────────
			It("prefix_cache_hit_rate should be > 0% (cache-aware routing effective)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckPrefixCache {
					Skip("prefix cache check not enabled")
				}
				checks := metrics.ValidateCacheAwareMetrics(vllmMetrics, nil)
				for _, c := range checks {
					if c.Metric == "derived:prefix_cache_hit_rate" {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("not enough data to compute hit rate")
			})

			// ── Phase 8d: vllm:gpu_cache_usage_perc ───────────────
			It("vllm:gpu_cache_usage_perc should be > 0 (KV cache in use)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || (!mc.CheckPrefixCache && !mc.CheckVLLM) {
					Skip("vLLM cache check not enabled")
				}
				if len(vllmMetrics) == 0 {
					Skip("no vLLM metrics scraped")
				}
				checks := metrics.ValidateCacheAwareMetrics(vllmMetrics, nil)
				for _, c := range checks {
					if c.Metric == metrics.MetricGPUCacheUsage {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("vllm:gpu_cache_usage_perc not found")
			})

			// ── Phase 8e: EPP prefix_indexer_size ─────────────────
			It("inference_extension_prefix_indexer_size should be > 0 (EPP prefix index populated)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckPrefixCache {
					Skip("prefix cache check not enabled")
				}
				if len(eppMetrics) == 0 {
					Skip("no EPP metrics scraped")
				}
				checks := metrics.ValidateCacheAwareMetrics(nil, eppMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricPrefixIndexerSize {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("inference_extension_prefix_indexer_size not found")
			})

			// ── Phase 8f: vllm:prompt_tokens_total ────────────────
			It("vllm:prompt_tokens_total should be > 0 (prefill pods processing prompts)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckPD {
					Skip("P/D check not enabled")
				}
				if len(vllmMetrics) == 0 {
					Skip("no vLLM metrics scraped")
				}
				checks := metrics.ValidatePDMetrics(vllmMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricPromptTokens {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("vllm:prompt_tokens_total not found")
			})

			// ── Phase 8g: vllm:generation_tokens_total ────────────
			It("vllm:generation_tokens_total should be > 0 (decode pods generating tokens)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckVLLM {
					Skip("vLLM check not enabled")
				}
				if len(vllmMetrics) == 0 {
					Skip("no vLLM metrics scraped")
				}
				checks := metrics.ValidatePDMetrics(vllmMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricGenTokens {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("vllm:generation_tokens_total not found")
			})

			// ── Phase 8h: vllm:request_success_total ──────────────
			It("vllm:request_success_total should be > 0 (requests completed)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckVLLM {
					Skip("vLLM check not enabled")
				}
				if len(vllmMetrics) == 0 {
					Skip("no vLLM metrics scraped")
				}
				checks := metrics.ValidatePDMetrics(vllmMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricRequestSuccess {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("vllm:request_success_total not found")
			})

			// ── Phase 8i: nixl:kv_transfer_count_total ────────────
			It("nixl:kv_transfer_count_total should be > 0 (NIXL KV transfers happened)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckNIXL {
					Skip("NIXL check not enabled")
				}
				if len(vllmMetrics) == 0 {
					Skip("no vLLM metrics scraped")
				}
				checks := metrics.ValidatePDMetrics(vllmMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricNIXLTransfers {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				logStep("[%s]   WARNING: nixl:kv_transfer_count_total not found (no RDMA/NIXL on this cluster)", tc.Name)
				AddReportEntry("warning", "NIXL metrics not available — no RDMA on this cluster")
			})

			// ── Phase 8j: nixl:kv_transfer_failures_total ─────────
			It("nixl:kv_transfer_failures_total should be 0 (no KV transfer failures)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckNIXL {
					Skip("NIXL check not enabled")
				}
				if len(vllmMetrics) == 0 {
					Skip("no vLLM metrics scraped")
				}
				checks := metrics.ValidatePDMetrics(vllmMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricNIXLFailures {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				logStep("[%s]   WARNING: nixl:kv_transfer_failures_total not found (no RDMA/NIXL on this cluster)", tc.Name)
				AddReportEntry("warning", "NIXL failure metrics not available — no RDMA on this cluster")
			})

			// ── Phase 8k: scheduler_e2e_duration ──────────────────
			It("inference_extension_scheduler_e2e_duration should be > 0 (scheduler routing requests)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || !mc.CheckScheduler {
					Skip("scheduler check not enabled")
				}
				if len(eppMetrics) == 0 {
					Skip("no EPP metrics scraped")
				}
				checks := metrics.ValidateSchedulerMetrics(eppMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricSchedulerE2E {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("inference_extension_scheduler_e2e_duration not found")
			})

			// ── Phase 8l: inference_objective_request_total ───────
			It("inference_objective_request_total should be > 0 (requests routed through EPP)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || (!mc.CheckScheduler && !mc.CheckEPP) {
					Skip("EPP/scheduler check not enabled")
				}
				if len(eppMetrics) == 0 {
					Skip("no EPP metrics scraped")
				}
				checks := metrics.ValidateSchedulerMetrics(eppMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricRequestTotal {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("inference_objective_request_total not found")
			})

			// ── Phase 8m: inference_objective_request_error_total ─
			It("inference_objective_request_error_total should be 0 (no routing errors)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || (!mc.CheckScheduler && !mc.CheckEPP) {
					Skip("EPP/scheduler check not enabled")
				}
				if len(eppMetrics) == 0 {
					Skip("no EPP metrics scraped")
				}
				checks := metrics.ValidateSchedulerMetrics(eppMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricRequestErrorTotal {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("inference_objective_request_error_total not found")
			})

			// ── Phase 8n: inference_pool_ready_pods ───────────────
			It("inference_pool_ready_pods should be > 0 (pods available for routing)", func() {
				skipIfCache()
				mc := tc.Validation.MetricsCheck
				if mc == nil || (!mc.CheckScheduler && !mc.CheckEPP) {
					Skip("EPP/scheduler check not enabled")
				}
				if len(eppMetrics) == 0 {
					Skip("no EPP metrics scraped")
				}
				checks := metrics.ValidateSchedulerMetrics(eppMetrics)
				for _, c := range checks {
					if c.Metric == metrics.MetricPoolReadyPods {
						logStep("[%s]   %s", tc.Name, c.Message)
						if !c.Passed {
							Fail(c.Message)
						}
						return
					}
				}
				Skip("inference_pool_ready_pods not found")
			})

			// ── Phase 9: CLEANUP ───────────────────────────────────
			AfterAll(func() {
				if !shouldRun {
					return
				}
				if testMode == "discover" || testMode == "cache" || noCleanup {
					if noCleanup {
						logStep("[%s] CLEANUP SKIPPED: --no-cleanup flag set (resources left running)", tc.Name)
					}
					return
				}
				if tc.Cleanup {
					logStep("[%s] Phase 9: CLEANUP — Removing deployed resources", tc.Name)
					_ = cleanupMgr.CleanupOne(ctx, tc.Name)
					if tc.Model.Cache != nil && tc.Model.Cache.Enabled {
						dl.Cleanup(ctx, tc)
					}
				}
			})
		})
	}
})

// ──────────────────────────────────────────────────────────────────
// Test Lifecycle
//
// Each test case goes through these phases in order. A failure at any
// phase fails the test with a clear error and triggers cleanup.
//
// Phase 1: PREP      — Download model to PVC (if cache.enabled=true)
//                       Pass criteria: download Job succeeds (.status.succeeded=1)
//                       Fail: Job fails or times out
//
// Phase 2: PREREQ    — Check that LLMInferenceService CRD exists
//                       Pass criteria: CRD is installed on the cluster
//                       Fail: CRD not found (cluster not set up correctly)
//
// Phase 3: DEPLOY    — kubectl apply the LLMInferenceService manifest
//                       Pass criteria: kubectl apply exits 0
//                       Fail: manifest not found, invalid YAML, API error
//
// Phase 4: READY     — Wait for llmisvc .status.ready=True
//                       Pass criteria: status becomes True within readyTimeout
//                       Fail: timeout (model download in-pod, OOM, scheduling failure)
//
// Phase 5: HEALTH    — GET /health on the vLLM endpoint
//                       Pass criteria: HTTP 200 within validation.timeout
//                       Fail: non-200 response or connection refused after retries
//
// Phase 6: MODEL     — GET /v1/models, verify model name is listed
//                       Pass criteria: model.name appears in response (warning only if missing)
//
// Phase 7: INFERENCE — POST /v1/chat/completions with test prompts
//                       Pass criteria: non-empty response with choices[].message.content
//                       Fallback: tries /v1/completions if chat fails
//                       Fail: empty response or HTTP error on both APIs
//
// Phase 8: CLEANUP   — kubectl delete the manifest, wait for pod termination
// ──────────────────────────────────────────────────────────────────

// logStep logs a phase step to both GinkgoWriter and stderr for immediate visibility.
func logStep(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	By(msg)
	fmt.Fprintln(os.Stderr, msg)
}

// resolveTestCases loads filtered test cases for BeforeSuite (profile support, reporter).
func resolveTestCases() ([]*config.TestCase, error) {
	allCases, err := config.LoadTestCasesFromDir(testCaseDir)
	if err != nil {
		return nil, fmt.Errorf("loading test cases from %s: %w", testCaseDir, err)
	}
	if testCaseName != "" {
		names := strings.Split(testCaseName, ",")
		for i := range names {
			names[i] = strings.TrimSpace(names[i])
		}
		return config.FilterTestCasesByNames(allCases, names), nil
	}
	if labels != "" {
		labelList := strings.Split(labels, ",")
		for i := range labelList {
			labelList[i] = strings.TrimSpace(labelList[i])
		}
		return config.FilterTestCasesByLabels(allCases, labelList), nil
	}
	if profilePath != "" {
		profile, err := config.LoadProfile(profilePath)
		if err != nil {
			return nil, fmt.Errorf("loading profile: %w", err)
		}
		return config.ResolveProfileTestCases(profile, testCaseDir)
	}
	return allCases, nil
}

