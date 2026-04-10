// Package metrics provides Prometheus metrics scraping and validation for vLLM and EPP components.
package metrics

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// vLLM metric names.
const (
	MetricPrefixCacheQueries    = "vllm:prefix_cache_queries"
	MetricPrefixCacheQueriesAlt = "vllm:prefix_cache_queries_total"
	MetricPrefixCacheHits       = "vllm:prefix_cache_hits"
	MetricPrefixCacheHitsAlt    = "vllm:prefix_cache_hits_total"
	MetricGPUCacheUsage         = "vllm:gpu_cache_usage_perc"
	MetricPromptTokens          = "vllm:prompt_tokens_total"
	MetricGenTokens             = "vllm:generation_tokens_total"
	MetricRequestSuccess        = "vllm:request_success_total"
	MetricPreemptions           = "vllm:num_preemptions_total"
)

// NIXL metric names (experimental — may not be available in all vLLM versions).
const (
	MetricNIXLTransfers = "nixl:kv_transfer_count_total"
	MetricNIXLFailures  = "nixl:kv_transfer_failures_total"
)

// EPP / scheduler metric names.
const (
	MetricSchedulerE2E      = "inference_extension_scheduler_e2e_duration_seconds_count"
	MetricRequestTotal      = "inference_objective_request_total"
	MetricRequestErrorTotal = "inference_objective_request_error_total"
	MetricPoolReadyPods     = "inference_pool_ready_pods"
	MetricPrefixIndexerSize = "inference_extension_prefix_indexer_size"
)

// Thresholds.
const (
	// maxAcceptablePreemptions is the upper bound for vLLM preemptions in a healthy P/D deployment.
	maxAcceptablePreemptions = 10
)

// vLLM workload label selector pattern used across the framework.
const WorkloadLabelFmt = "app.kubernetes.io/name=%s,app.kubernetes.io/component=llminferenceservice-workload"

// Metric represents a single parsed Prometheus metric line.
type Metric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// ScrapeResult holds all metrics scraped from one endpoint.
// After parsing, metrics are indexed by name for O(1) lookups.
type ScrapeResult struct {
	Source string // e.g. "vllm-pod-xyz" or "epp-pod-xyz"
	index  map[string][]Metric
}

// newScrapeResult creates a ScrapeResult with an indexed metric map.
func newScrapeResult(source string, raw []Metric) *ScrapeResult {
	idx := make(map[string][]Metric, len(raw))
	for _, m := range raw {
		idx[m.Name] = append(idx[m.Name], m)
	}
	return &ScrapeResult{Source: source, index: idx}
}

// GetValue returns the value of the first metric matching the given name, or 0 if not found.
func (r *ScrapeResult) GetValue(name string) (float64, bool) {
	if ms, ok := r.index[name]; ok && len(ms) > 0 {
		return ms[0].Value, true
	}
	return 0, false
}

// getValueWithFallback tries the primary metric name, then falls back to an alternative.
// vLLM versions differ in whether counters have a _total suffix.
func (r *ScrapeResult) getValueWithFallback(primary, fallback string) (float64, bool) {
	if v, ok := r.GetValue(primary); ok {
		return v, true
	}
	return r.GetValue(fallback)
}

// GetAllValues returns all values for metrics matching the given name.
func (r *ScrapeResult) GetAllValues(name string) []float64 {
	ms := r.index[name]
	vals := make([]float64, len(ms))
	for i, m := range ms {
		vals[i] = m.Value
	}
	return vals
}

// GetWithLabel returns the value of the first metric matching name and a specific label value.
func (r *ScrapeResult) GetWithLabel(name, labelKey, labelValue string) (float64, bool) {
	for _, m := range r.index[name] {
		if m.Labels[labelKey] == labelValue {
			return m.Value, true
		}
	}
	return 0, false
}

// ParsePrometheusText parses Prometheus text exposition format into Metrics.
func ParsePrometheusText(body string) []Metric {
	var metrics []Metric
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m, ok := parseLine(line)
		if ok {
			metrics = append(metrics, m)
		}
	}
	return metrics
}

// parseLine parses a single Prometheus metric line like:
//
//	vllm:prefix_cache_hits{model_name="Qwen/Qwen2.5-7B-Instruct"} 42.0
func parseLine(line string) (Metric, bool) {
	m := Metric{Labels: make(map[string]string)}

	nameEnd := strings.IndexByte(line, '{')
	if nameEnd >= 0 {
		m.Name = line[:nameEnd]
		labelEnd := strings.IndexByte(line[nameEnd:], '}')
		if labelEnd < 0 {
			return m, false
		}
		labelStr := line[nameEnd+1 : nameEnd+labelEnd]
		m.Labels = parseLabels(labelStr)
		line = strings.TrimSpace(line[nameEnd+labelEnd+1:])
	} else {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return m, false
		}
		m.Name = parts[0]
		line = parts[1]
	}

	val, err := strconv.ParseFloat(line, 64)
	if err != nil {
		return m, false
	}
	m.Value = val
	return m, true
}

func parseLabels(s string) map[string]string {
	labels := make(map[string]string)
	for _, part := range splitLabels(s) {
		part = strings.TrimSpace(part)
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		key := part[:eq]
		val := strings.Trim(part[eq+1:], "\"")
		labels[key] = val
	}
	return labels
}

// splitLabels splits comma-separated Prometheus label pairs, respecting quoted values.
func splitLabels(s string) []string {
	var parts []string
	var current strings.Builder
	inQuote := false
	for _, ch := range s {
		switch {
		case ch == '"':
			inQuote = !inQuote
			current.WriteRune(ch)
		case ch == ',' && !inQuote:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(ch)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// KubectlFunc is a function that runs kubectl commands (injected from deployer).
type KubectlFunc func(ctx context.Context, args ...string) (string, error)

// Scraper scrapes Prometheus metrics from pods via kubectl exec.
type Scraper struct {
	Kubectl   KubectlFunc
	Namespace string
	LogFunc   func(format string, args ...interface{})
}

func (s *Scraper) log(format string, args ...interface{}) {
	if s.LogFunc != nil {
		s.LogFunc(format, args...)
	}
}

// ScrapePod scrapes /metrics from a specific pod using kubectl exec.
// Tries container "main" first (conformance deployments), then without -c (auto-select, helmfile deployments).
func (s *Scraper) ScrapePod(ctx context.Context, podName string, port int) (*ScrapeResult, error) {
	// Try container "main" first, then auto-select
	for _, container := range []string{"main", ""} {
		out, err := s.scrapePodContainer(ctx, podName, port, container)
		if err == nil {
			return newScrapeResult(podName, ParsePrometheusText(out)), nil
		}
	}
	return nil, fmt.Errorf("scraping metrics from %s on port %d: all attempts failed", podName, port)
}

func (s *Scraper) scrapePodContainer(ctx context.Context, podName string, port int, container string) (string, error) {
	execArgs := func(cmd ...string) []string {
		args := []string{"exec", podName, "-n", s.Namespace}
		if container != "" {
			args = append(args, "-c", container)
		}
		args = append(args, "--")
		args = append(args, cmd...)
		return args
	}

	// Try HTTPS first (LLMInferenceService uses TLS), then HTTP (helmfile deployments)
	for _, scheme := range []string{"https", "http"} {
		metricsURL := fmt.Sprintf("%s://localhost:%d/metrics", scheme, port)

		// Try python3
		out, err := s.Kubectl(ctx, execArgs("python3", "-c",
			fmt.Sprintf("import urllib.request,ssl; print(urllib.request.urlopen('%s',context=ssl._create_unverified_context()).read().decode())", metricsURL))...)
		if err == nil && strings.TrimSpace(out) != "" {
			return out, nil
		}

		// Try wget
		out, err = s.Kubectl(ctx, execArgs("wget", "--no-check-certificate", "-qO-", metricsURL)...)
		if err == nil && strings.TrimSpace(out) != "" {
			return out, nil
		}

		// Try curl (available in some containers)
		out, err = s.Kubectl(ctx, execArgs("curl", "-sf", metricsURL)...)
		if err == nil && strings.TrimSpace(out) != "" {
			return out, nil
		}
	}

	return "", fmt.Errorf("all scrape attempts failed")
}

// listPods returns pod names matching the given label selector.
func (s *Scraper) listPods(ctx context.Context, label string) ([]string, error) {
	out, err := s.Kubectl(ctx, "get", "pods", "-n", s.Namespace, "-l", label,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// scrapePodsByLabel lists pods matching a label and scrapes /metrics from each.
func (s *Scraper) scrapePodsByLabel(ctx context.Context, label string, port int) ([]*ScrapeResult, error) {
	pods, err := s.listPods(ctx, label)
	if err != nil || len(pods) == 0 {
		return nil, fmt.Errorf("no pods found for label %s", label)
	}

	results := make([]*ScrapeResult, 0, len(pods))
	for _, podName := range pods {
		s.log("  scraping metrics from pod %s", podName)
		result, err := s.ScrapePod(ctx, podName, port)
		if err != nil {
			s.log("  WARNING: failed to scrape %s: %v", podName, err)
			continue
		}
		s.log("  scraped %d metric names from %s", len(result.index), podName)
		results = append(results, result)
	}
	return results, nil
}

// ScrapeVLLMPods scrapes /metrics from all vLLM pods (decode + prefill) matching the given llmisvc name.
func (s *Scraper) ScrapeVLLMPods(ctx context.Context, llmisvcName string) ([]*ScrapeResult, error) {
	// Scrape decode pods
	label := fmt.Sprintf(WorkloadLabelFmt, llmisvcName)
	results, err := s.scrapePodsByLabel(ctx, label, 8000)
	if err != nil {
		s.log("  WARNING: failed to scrape decode pods: %v", err)
	}

	// Also scrape prefill pods (P/D deployments have separate prefill workloads)
	prefillLabel := fmt.Sprintf("app.kubernetes.io/name=%s,app.kubernetes.io/component=llminferenceservice-workload-prefill", llmisvcName)
	if prefillResults, err := s.scrapePodsByLabel(ctx, prefillLabel, 8000); err == nil {
		results = append(results, prefillResults...)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no vLLM pods found for %s", llmisvcName)
	}
	return results, nil
}

// ScrapeEPPPods scrapes /metrics from EPP (scheduler) pods.
// Tries multiple label patterns since EPP naming varies across versions.
func (s *Scraper) ScrapeEPPPods(ctx context.Context, llmisvcName string) ([]*ScrapeResult, error) {
	labels := []string{
		fmt.Sprintf("app.kubernetes.io/name=%s-epp", llmisvcName),
		fmt.Sprintf("app.kubernetes.io/component=endpoint-picker,app.kubernetes.io/name=%s", llmisvcName),
		fmt.Sprintf("app.kubernetes.io/component=router-scheduler,app.kubernetes.io/name=%s", llmisvcName),
		fmt.Sprintf("kserve.io/component=scheduler,app.kubernetes.io/name=%s", llmisvcName),
	}

	for _, label := range labels {
		pods, _ := s.listPods(ctx, label)
		if len(pods) == 0 {
			continue
		}
		// EPP typically serves metrics on port 9090 or 8080
		results, err := s.scrapePodsByLabel(ctx, label, 9090)
		if err == nil && len(results) > 0 {
			return results, nil
		}
		return s.scrapePodsByLabel(ctx, label, 8080)
	}
	return nil, fmt.Errorf("no EPP pods found for %s", llmisvcName)
}

// CheckResult represents a single metric validation check.
type CheckResult struct {
	Name    string  // human-readable check name
	Metric  string  // Prometheus metric name
	Source  string  // pod name
	Value   float64 // actual value
	Passed  bool
	Message string
}

// ValidateCacheAwareMetrics checks that cache-aware routing metrics indicate prefix cache hits.
func ValidateCacheAwareMetrics(vllmResults []*ScrapeResult, eppResults []*ScrapeResult) []CheckResult {
	var checks []CheckResult

	// Check vLLM prefix cache metrics across all pods
	var totalQueries, totalHits float64
	for _, r := range vllmResults {
		if v, ok := r.getValueWithFallback(MetricPrefixCacheQueriesAlt, MetricPrefixCacheQueries); ok {
			totalQueries += v
		}
		if v, ok := r.getValueWithFallback(MetricPrefixCacheHitsAlt, MetricPrefixCacheHits); ok {
			totalHits += v
		}
	}

	if len(vllmResults) > 0 {
		checks = append(checks, CheckResult{
			Name:    "prefix cache queries received",
			Metric:  MetricPrefixCacheQueries,
			Value:   totalQueries,
			Passed:  totalQueries > 0,
			Message: fmt.Sprintf("prefix_cache_queries=%.0f (expected > 0)", totalQueries),
		}, CheckResult{
			Name:    "prefix cache hits occurred",
			Metric:  MetricPrefixCacheHits,
			Value:   totalHits,
			Passed:  totalHits > 0,
			Message: fmt.Sprintf("prefix_cache_hits=%.0f (expected > 0 — same prefix sent twice)", totalHits),
		})

		if totalQueries > 0 {
			hitRate := totalHits / totalQueries
			checks = append(checks, CheckResult{
				Name:    "prefix cache hit rate",
				Metric:  "derived:prefix_cache_hit_rate",
				Value:   hitRate,
				Passed:  hitRate > 0,
				Message: fmt.Sprintf("hit_rate=%.2f%% (%.0f hits / %.0f queries)", hitRate*100, totalHits, totalQueries),
			})
		}

		// Check KV cache utilization
		for _, r := range vllmResults {
			if v, ok := r.GetValue(MetricGPUCacheUsage); ok {
				checks = append(checks, CheckResult{
					Name:    "KV cache in use on " + r.Source,
					Metric:  MetricGPUCacheUsage,
					Source:  r.Source,
					Value:   v,
					Passed:  v > 0,
					Message: fmt.Sprintf("gpu_cache_usage=%.2f%% (expected > 0)", v*100),
				})
			}
		}
	}

	// Check EPP prefix indexer metrics
	for _, r := range eppResults {
		if v, ok := r.GetValue(MetricPrefixIndexerSize); ok {
			checks = append(checks, CheckResult{
				Name:    "EPP prefix index populated",
				Metric:  MetricPrefixIndexerSize,
				Source:  r.Source,
				Value:   v,
				Passed:  v > 0,
				Message: fmt.Sprintf("prefix_indexer_size=%.0f (expected > 0)", v),
			})
		}
	}

	return checks
}

// ValidatePDMetrics checks that prefill/decode disaggregation metrics look correct.
func ValidatePDMetrics(vllmResults []*ScrapeResult) []CheckResult {
	var checks []CheckResult

	var totalPromptTokens, totalGenTokens, totalRequests float64
	for _, r := range vllmResults {
		if v, ok := r.GetValue(MetricPromptTokens); ok {
			totalPromptTokens += v
		}
		if v, ok := r.GetValue(MetricGenTokens); ok {
			totalGenTokens += v
		}
		// Sum request_success across all finished_reason labels (stop + length), excluding abort
		for _, m := range r.index[MetricRequestSuccess] {
			if m.Labels["finished_reason"] != "abort" {
				totalRequests += m.Value
			}
		}
	}

	checks = append(checks, CheckResult{
		Name:    "prompt tokens processed (prefill working)",
		Metric:  MetricPromptTokens,
		Value:   totalPromptTokens,
		Passed:  totalPromptTokens > 0,
		Message: fmt.Sprintf("prompt_tokens=%.0f across %d pods (expected > 0)", totalPromptTokens, len(vllmResults)),
	}, CheckResult{
		Name:    "generation tokens produced (decode working)",
		Metric:  MetricGenTokens,
		Value:   totalGenTokens,
		Passed:  totalGenTokens > 0,
		Message: fmt.Sprintf("generation_tokens=%.0f across %d pods (expected > 0)", totalGenTokens, len(vllmResults)),
	}, CheckResult{
		Name:    "requests completed successfully",
		Metric:  MetricRequestSuccess,
		Value:   totalRequests,
		Passed:  totalRequests > 0,
		Message: fmt.Sprintf("request_success=%.0f (expected > 0)", totalRequests),
	})

	// NIXL KV transfer metrics — experimental, may not be available
	var totalTransfers, totalFailures float64
	hasNIXL := false
	for _, r := range vllmResults {
		if v, ok := r.GetValue(MetricNIXLTransfers); ok {
			totalTransfers += v
			hasNIXL = true
		}
		if v, ok := r.GetValue(MetricNIXLFailures); ok {
			totalFailures += v
		}
	}

	if hasNIXL {
		checks = append(checks, CheckResult{
			Name:    "NIXL KV transfers completed",
			Metric:  MetricNIXLTransfers,
			Value:   totalTransfers,
			Passed:  totalTransfers > 0,
			Message: fmt.Sprintf("kv_transfers=%.0f (expected > 0)", totalTransfers),
		}, CheckResult{
			Name:    "NIXL KV transfer failures",
			Metric:  MetricNIXLFailures,
			Value:   totalFailures,
			Passed:  totalFailures == 0,
			Message: fmt.Sprintf("kv_transfer_failures=%.0f (expected 0)", totalFailures),
		})
	}

	// Preemptions should be low/zero in healthy P/D
	for _, r := range vllmResults {
		if v, ok := r.GetValue(MetricPreemptions); ok {
			checks = append(checks, CheckResult{
				Name:    "no excessive preemptions on " + r.Source,
				Metric:  MetricPreemptions,
				Source:  r.Source,
				Value:   v,
				Passed:  v < maxAcceptablePreemptions,
				Message: fmt.Sprintf("preemptions=%.0f on %s (expected < %d)", v, r.Source, maxAcceptablePreemptions),
			})
		}
	}

	return checks
}

// ValidateSchedulerMetrics checks that EPP scheduler metrics indicate healthy routing.
func ValidateSchedulerMetrics(eppResults []*ScrapeResult) []CheckResult {
	var checks []CheckResult

	for _, r := range eppResults {
		if v, ok := r.GetValue(MetricSchedulerE2E); ok {
			checks = append(checks, CheckResult{
				Name:    "scheduler processed requests",
				Metric:  MetricSchedulerE2E,
				Source:  r.Source,
				Value:   v,
				Passed:  v > 0,
				Message: fmt.Sprintf("scheduler_requests=%.0f (expected > 0)", v),
			})
		}

		if v, ok := r.GetValue(MetricRequestTotal); ok {
			checks = append(checks, CheckResult{
				Name:    "requests routed through EPP",
				Metric:  MetricRequestTotal,
				Source:  r.Source,
				Value:   v,
				Passed:  v > 0,
				Message: fmt.Sprintf("routed_requests=%.0f (expected > 0)", v),
			})
		}

		if v, ok := r.GetValue(MetricRequestErrorTotal); ok {
			checks = append(checks, CheckResult{
				Name:    "no routing errors",
				Metric:  MetricRequestErrorTotal,
				Source:  r.Source,
				Value:   v,
				Passed:  v == 0,
				Message: fmt.Sprintf("routing_errors=%.0f (expected 0)", v),
			})
		}

		if v, ok := r.GetValue(MetricPoolReadyPods); ok {
			checks = append(checks, CheckResult{
				Name:    "inference pool has ready pods",
				Metric:  MetricPoolReadyPods,
				Source:  r.Source,
				Value:   v,
				Passed:  v > 0,
				Message: fmt.Sprintf("ready_pods=%.0f (expected > 0)", v),
			})
		}
	}

	return checks
}
