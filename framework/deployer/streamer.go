package deployer

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"sync"
)

// LogFunc is a function that receives log lines from streaming kubectl commands.
type LogFunc func(format string, args ...interface{})

// Streamer manages background kubectl streaming processes (events, logs)
// and forwards their output line-by-line to a log function.
type Streamer struct {
	kubeconfig string
	namespace  string
	logFunc    LogFunc

	mu      sync.Mutex
	cancels []context.CancelFunc
}

// NewStreamer creates a new Streamer for the given namespace.
func NewStreamer(kubeconfig, namespace string, logFunc LogFunc) *Streamer {
	return &Streamer{
		kubeconfig: kubeconfig,
		namespace:  namespace,
		logFunc:    logFunc,
	}
}

// StreamEvents starts `kubectl get events --watch` in the background.
// Each event line is forwarded to logFunc with the given prefix.
// Call Stop() to terminate all streams.
func (s *Streamer) StreamEvents(ctx context.Context, prefix string) {
	args := []string{"get", "events", "-n", s.namespace, "--watch", "--no-headers",
		"-o", "custom-columns=TIME:.lastTimestamp,TYPE:.type,REASON:.reason,OBJECT:.involvedObject.kind/.involvedObject.name,MESSAGE:.message"}
	s.startStream(ctx, prefix, args)
}

// StreamPodLogs starts `kubectl logs -f <pod> --all-containers --prefix` in the background.
// Each log line is forwarded to logFunc.
func (s *Streamer) StreamPodLogs(ctx context.Context, pod string) {
	args := []string{"logs", "-f", pod, "-n", s.namespace, "--all-containers", "--prefix", "--tail=20"}
	s.startStream(ctx, fmt.Sprintf("[%s]", pod), args)
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
		cmd.Stderr = nil // discard stderr

		if err := cmd.Start(); err != nil {
			return
		}

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				s.logFunc("%s %s", prefix, line)
			}
		}

		_ = cmd.Wait()
	}()
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
