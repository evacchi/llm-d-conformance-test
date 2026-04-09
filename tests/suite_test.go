package tests

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Command-line flags for controlling test execution.
var (
	profilePath  string
	testCaseDir  string
	platform     string
	namespace    string
	kubeconfig   string
	reportDir    string
	testCaseName string
	labels       string
	storageClass string
	storageSize  string // override PVC storage size (e.g., "50Gi")
	// Discover mode: validate an existing deployment without deploying
	testMode    string
	endpoint    string
	modelSource   string // "pvc" (default) or "hf"
	modelOverride string // override model repo ID (e.g., "Qwen/Qwen3-0.6B")
	noCleanup     bool
	mockImage      string // mock vLLM image for testing without GPU
	pullSecretName string // override pull secret name to copy into namespace
	// Benchmark / helmfile flags
	helmfilePath string // path to helmfile template for benchmark scenarios
	guidesPath   string // path to llm-d guides directory (exported as LLM_D_GUIDES)
	helmfileEnv  string // helmfile environment (default: "default")
)

func init() {
	flag.StringVar(&profilePath, "profile", "", "Path to test profile YAML (e.g., configs/profiles/smoke.yaml)")
	flag.StringVar(&testCaseDir, "testcase-dir", "configs/testcases", "Directory containing test case YAML files")
	flag.StringVar(&platform, "platform", "any", "Target platform: ocp, aks, gks, any")
	flag.StringVar(&namespace, "namespace", "llm-conformance-test", "Default Kubernetes namespace")
	flag.StringVar(&kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "Path to kubeconfig")
	flag.StringVar(&reportDir, "report-dir", "reports", "Directory for JSON reports")
	flag.StringVar(&testCaseName, "testcase", "", "Run a single test case by name (overrides profile)")
	flag.StringVar(&labels, "labels", "", "Comma-separated labels to filter test cases (overrides profile)")
	flag.StringVar(&storageClass, "storage-class", "", "Kubernetes StorageClass for model cache PVCs (uses cluster default if empty)")
	flag.StringVar(&storageSize, "storage-size", "", "Override PVC storage size (e.g., 50Gi). Default: from test case config")
	flag.StringVar(&testMode, "mode", "deploy", "Test mode: 'deploy' (full lifecycle), 'discover' (validate existing), or 'cache' (download models only)")
	flag.StringVar(&endpoint, "endpoint", "", "Service endpoint URL for discover mode (e.g., http://my-llm-svc:8000)")
	flag.StringVar(&modelSource, "model-source", "hf", "Model source: 'hf' (HuggingFace direct, default) or 'pvc' (download to PVC first)")
	flag.StringVar(&modelOverride, "model", "", "Override model repo ID (e.g., Qwen/Qwen3-0.6B)")
	flag.BoolVar(&noCleanup, "nocleanup", false, "Skip cleanup after tests (leave resources running for debugging)")
	flag.StringVar(&mockImage, "mock", "", "Mock vLLM image for testing without GPU (e.g., ghcr.io/aneeshkp/vllm-mock:latest)")
	flag.StringVar(&pullSecretName, "pull-secret", "", "Pull secret name to copy into test namespace (default: auto-detect from manifest)")
	flag.StringVar(&helmfilePath, "helmfile", "", "Path to helmfile template for benchmark scenarios (e.g., deploy/helmfiles/is-vanilla/helmfile.yaml.gotmpl)")
	flag.StringVar(&guidesPath, "guides", "", "Path to llm-d guides directory (exported as LLM_D_GUIDES for helmfile)")
	flag.StringVar(&helmfileEnv, "helmfile-env", "default", "Helmfile environment name")
}

// findRootDir walks up from the current working directory to find the project root (containing go.mod).
func findRootDir() string {
	for _, c := range []string{"..", "../..", "."} {
		if _, err := os.Stat(filepath.Join(c, "go.mod")); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return "."
}

// resolveRelativePath makes a relative path absolute by joining it with the project root.
func resolveRelativePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(findRootDir(), p)
}

func TestLLMDConformance(t *testing.T) {
	// Resolve relative paths against the project root since go test
	// sets the working directory to the test package directory.
	testCaseDir = resolveRelativePath(testCaseDir)
	if profilePath != "" {
		profilePath = resolveRelativePath(profilePath)
	}
	reportDir = resolveRelativePath(reportDir)

	RegisterFailHandler(Fail)
	RunSpecs(t, "LLM-D Conformance Test Suite")
}
