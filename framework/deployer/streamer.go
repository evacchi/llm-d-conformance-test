package deployer

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// LogFunc is a function that receives log lines from streaming kubectl commands.
type LogFunc func(format string, args ...interface{})

// Streamer manages background kubectl streaming processes (events, logs)
// and forwards their output line-by-line to a log function.
type Streamer struct {
	kubeconfig string
	namespace  string
	logFunc    LogFunc

	mu             sync.Mutex
	cancels        []context.CancelFunc
	streamingPods  map[string]bool // pods we've already started streaming
}

// NewStreamer creates a new Streamer for the given namespace.
func NewStreamer(kubeconfig, namespace string, logFunc LogFunc) *Streamer {
	return &Streamer{
		kubeconfig:    kubeconfig,
		namespace:     namespace,
		logFunc:       logFunc,
		streamingPods: make(map[string]bool),
	}
}

// StreamEvents starts `kubectl get events --watch` in the background.
// Each event line is forwarded to logFunc with the given prefix.
// Deduplicates repeated identical messages (e.g. Unhealthy probe failures).
func (s *Streamer) StreamEvents(ctx context.Context, prefix string) {
	args := []string{"get", "events", "-n", s.namespace, "--watch", "--no-headers",
		"-o", "custom-columns=TYPE:.type,REASON:.reason,OBJECT:.involvedObject.kind/.involvedObject.name,MESSAGE:.message"}
	s.startFilteredStream(ctx, prefix, args)
}

// StreamPodLogs starts `kubectl logs -f <pod> --all-containers --prefix` in the background.
// Each log line is forwarded to logFunc.
func (s *Streamer) StreamPodLogs(ctx context.Context, pod string) {
	s.mu.Lock()
	if s.streamingPods[pod] {
		s.mu.Unlock()
		return // already streaming this pod
	}
	s.streamingPods[pod] = true
	s.mu.Unlock()

	args := []string{"logs", "-f", pod, "-n", s.namespace, "--all-containers", "--prefix", "--tail=20"}
	s.startStream(ctx, "", args)
}

// StreamAllPodLogs periodically discovers pods in the namespace and starts
// streaming logs from any new pods it finds. This runs until the context
// is cancelled or Stop() is called.
func (s *Streamer) StreamAllPodLogs(ctx context.Context, interval time.Duration) {
	streamCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.cancels = append(s.cancels, cancel)
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Run immediately, then on tick
		s.discoverAndStreamPods(streamCtx)

		for {
			select {
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				s.discoverAndStreamPods(streamCtx)
			}
		}
	}()
}

func (s *Streamer) discoverAndStreamPods(ctx context.Context) {
	args := []string{"get", "pods", "-n", s.namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}"}
	if s.kubeconfig != "" {
		args = append([]string{"--kubeconfig", s.kubeconfig}, args...)
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.Output() // stdout only — avoids exec-plugin noise
	if err != nil {
		return
	}

	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name != "" {
			s.StreamPodLogs(ctx, name)
		}
	}
}

func (s *Streamer) startStream(parentCtx context.Context, prefix string, kubectlArgs []string) {
	ctx, cancel := context.WithCancel(parentCtx)

	s.mu.Lock()
	s.cancels = append(s.cancels, cancel)
	s.mu.Unlock()

	args := make([]string, 0, len(kubectlArgs)+2)
	if s.kubeconfig != "" {
		args = append(args, "--kubeconfig", s.kubeconfig)
	}
	args = append(args, kubectlArgs...)

	go func() {
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		cmd.Stderr = nil // discard stderr (avoids exec-plugin noise)

		if err := cmd.Start(); err != nil {
			return
		}

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			if prefix != "" {
				s.logFunc("%s %s", prefix, line)
			} else {
				s.logFunc("%s", line)
			}
		}

		_ = cmd.Wait()
	}()
}

// startFilteredStream is like startStream but deduplicates consecutive identical lines
// and suppresses repeated events (e.g. Unhealthy probe failures).
func (s *Streamer) startFilteredStream(parentCtx context.Context, prefix string, kubectlArgs []string) {
	ctx, cancel := context.WithCancel(parentCtx)

	s.mu.Lock()
	s.cancels = append(s.cancels, cancel)
	s.mu.Unlock()

	args := make([]string, 0, len(kubectlArgs)+2)
	if s.kubeconfig != "" {
		args = append(args, "--kubeconfig", s.kubeconfig)
	}
	args = append(args, kubectlArgs...)

	go func() {
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		cmd.Stderr = nil

		if err := cmd.Start(); err != nil {
			return
		}

		scanner := bufio.NewScanner(stdout)
		seen := make(map[string]int) // message -> count
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			// Deduplicate: use REASON+OBJECT+MESSAGE as the key
			// (skip the TYPE column which is always first)
			key := line
			if count := seen[key]; count > 0 {
				seen[key] = count + 1
				// Log suppression notice every 10 repeats
				if count == 5 {
					s.logFunc("%s (suppressing repeated: %s)", prefix, truncate(line, 80))
				}
				continue
			}
			seen[key] = 1
			s.logFunc("%s %s", prefix, line)
		}

		_ = cmd.Wait()
	}()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Stop terminates all background streaming processes.
func (s *Streamer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.cancels = nil
}
