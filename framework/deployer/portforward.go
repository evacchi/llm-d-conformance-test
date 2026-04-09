package deployer

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// PortForwardResult holds a running port-forward process.
type PortForwardResult struct {
	LocalPort int
	URL       string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
}

// Stop terminates the port-forward process.
func (pf *PortForwardResult) Stop() {
	if pf.cancel != nil {
		pf.cancel()
	}
	if pf.cmd != nil && pf.cmd.Process != nil {
		_ = pf.cmd.Process.Kill()
		_ = pf.cmd.Wait()
	}
}

// FindGatewayService finds the istio gateway service name in the namespace.
// It first checks for a Gateway resource and derives the service name ({gateway}-istio),
// then falls back to searching for services matching *gateway*istio*.
func (d *Deployer) FindGatewayService(ctx context.Context, namespace string) (string, int, error) {
	// Try Gateway resource → derive istio service name
	out, err := d.Kubectl(ctx, "get", "gateway", "-n", namespace,
		"-o", "jsonpath={.items[0].metadata.name}")
	if err == nil {
		name := strings.TrimSpace(out)
		if name != "" {
			svcName := name + "-istio"
			// Verify the service exists
			if _, verifyErr := d.Kubectl(ctx, "get", "svc", svcName, "-n", namespace); verifyErr == nil {
				return svcName, 80, nil
			}
		}
	}

	// Fallback: find services matching *gateway*istio*
	out, err = d.Kubectl(ctx, "get", "svc", "-n", namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return "", 0, fmt.Errorf("listing services: %w", err)
	}
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if strings.Contains(name, "gateway") && strings.Contains(name, "istio") {
			return name, 80, nil
		}
	}

	return "", 0, fmt.Errorf("no gateway service found in namespace %s", namespace)
}

// StartPortForward starts kubectl port-forward to the given service and waits for it to be ready.
func (d *Deployer) StartPortForward(ctx context.Context, namespace, svcName string, remotePort int) (*PortForwardResult, error) {
	localPort, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("finding free port: %w", err)
	}

	pfCtx, cancel := context.WithCancel(ctx)

	args := []string{"port-forward", "-n", namespace, "svc/" + svcName, fmt.Sprintf("%d:%d", localPort, remotePort)}
	if d.Kubeconfig != "" {
		args = append([]string{"--kubeconfig", d.Kubeconfig}, args...)
	}

	cmd := exec.CommandContext(pfCtx, "kubectl", args...)
	cmd.Stderr = nil // discard exec-plugin noise

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("starting port-forward: %w", err)
	}

	// Wait for the port to accept connections
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 500*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return &PortForwardResult{
				LocalPort: localPort,
				URL:       fmt.Sprintf("http://localhost:%d", localPort),
				cmd:       cmd,
				cancel:    cancel,
			}, nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Cleanup on timeout
	cancel()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return nil, fmt.Errorf("port-forward to svc/%s did not become ready within 15s", svcName)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}
