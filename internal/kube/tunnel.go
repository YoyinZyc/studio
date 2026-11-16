// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kube

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

var portForwardRegex = regexp.MustCompile(`Forwarding from .*:(\d+)\s+->`)

// TunnelInfo mirrors ~/.ax/tunnels/<ctx>.json so AX Studio and the `ax` CLI
// seamlessly share background port-forward tunnels.
type TunnelInfo struct {
	Context   string    `json:"context"`
	Namespace string    `json:"namespace"`
	Service   string    `json:"service"`
	Port      int       `json:"port"`
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"created_at"`
}

// TunnelOptions configures resolving or establishing a tunnel to `svc/ax-server`.
type TunnelOptions struct {
	ServerURL string
	Context   string
	Namespace string
	Service   string
	Port      int
}

func sanitizeContext(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	if sb.Len() == 0 {
		return "default"
	}
	return sb.String()
}

func tunnelDir() (string, error) {
	if dir := os.Getenv("AX_HOME"); dir != "" {
		p := filepath.Join(dir, "tunnels")
		return p, os.MkdirAll(p, 0o755)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, ".ax", "tunnels")
	return p, os.MkdirAll(p, 0o755)
}

func getTunnel(ctxName string) (*TunnelInfo, error) {
	dir, err := tunnelDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, sanitizeContext(ctxName)+".json"))
	if err != nil {
		return nil, err
	}
	var info TunnelInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func saveTunnel(info *TunnelInfo) error {
	dir, err := tunnelDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sanitizeContext(info.Context)+".json"), data, 0o644)
}

func stopTunnel(info *TunnelInfo) {
	if info == nil {
		return
	}
	if info.PID > 0 {
		_ = syscall.Kill(-info.PID, syscall.SIGTERM)
		_ = syscall.Kill(info.PID, syscall.SIGTERM)
	}
	if dir, err := tunnelDir(); err == nil {
		_ = os.Remove(filepath.Join(dir, sanitizeContext(info.Context)+".json"))
	}
}

func isTunnelHealthy(port int) bool {
	if port <= 0 {
		return false
	}
	client := http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// EnsureServerURL returns a healthy `http://127.0.0.1:<port>` endpoint for
// `svc/ax-server` on the requested Kubernetes context, reusing any active tunnel
// recorded in `~/.ax/tunnels/` or starting a new `kubectl port-forward`.
func EnsureServerURL(opts TunnelOptions) (string, error) {
	if opts.ServerURL != "" {
		return opts.ServerURL, nil
	}
	if env := os.Getenv("AX_SERVER"); env != "" && opts.Context == "" {
		return env, nil
	}

	ctxName, err := CurrentContext(opts.Context)
	if err != nil || ctxName == "" {
		return "", fmt.Errorf("no active Kubernetes context configured")
	}
	if strings.HasPrefix(ctxName, "-") {
		return "", fmt.Errorf("invalid context name %q", ctxName)
	}

	if opts.Namespace == "" {
		if envNS := os.Getenv("AX_SYSTEM_NAMESPACE"); envNS != "" {
			opts.Namespace = envNS
		} else {
			opts.Namespace = v1alpha1.DefaultSystemNamespace
		}
	}
	if opts.Service == "" {
		opts.Service = "ax-server"
	}
	if opts.Port == 0 {
		opts.Port = 8080
	}

	if existing, err := getTunnel(ctxName); err == nil && existing != nil {
		sameTarget := (existing.Namespace == "" || existing.Namespace == opts.Namespace) &&
			(existing.Service == "" || existing.Service == opts.Service)
		if sameTarget && isTunnelHealthy(existing.Port) {
			return fmt.Sprintf("http://127.0.0.1:%d", existing.Port), nil
		}
		stopTunnel(existing)
	}

	kubectlBin, err := ResolveKubectl(nil)
	if err != nil {
		return "", fmt.Errorf("kubectl not found (~/.ax/bin/kubectl or PATH): %w", err)
	}

	dir, err := tunnelDir()
	if err != nil {
		return "", fmt.Errorf("creating tunnel directory: %w", err)
	}
	logPath := filepath.Join(dir, sanitizeContext(ctxName)+".log")
	kArgs := []string{
		"--context", ctxName,
		"port-forward", "-n", opts.Namespace,
		fmt.Sprintf("svc/%s", opts.Service),
		fmt.Sprintf(":%d", opts.Port),
	}

	var (
		cmd       *exec.Cmd
		localPort int
		lastErr   error
	)
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		logFile, openErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			return "", fmt.Errorf("opening tunnel log: %w", openErr)
		}

		cmd = exec.Command(kubectlBin, kArgs...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout = logFile
		cmd.Stderr = logFile

		startErr := cmd.Start()
		_ = logFile.Close()
		if startErr != nil {
			return "", fmt.Errorf("spawning kubectl port-forward for context %q: %w", ctxName, startErr)
		}

		exited := make(chan struct{})
		go func(c *exec.Cmd) {
			_ = c.Wait()
			close(exited)
		}(cmd)

		localPort = 0
		earlyExit := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			logData, _ := os.ReadFile(logPath)
			if match := portForwardRegex.FindStringSubmatch(string(logData)); len(match) > 1 {
				if p, convErr := strconv.Atoi(match[1]); convErr == nil && p > 0 {
					localPort = p
					break
				}
			}
			select {
			case <-exited:
				logData, _ := os.ReadFile(logPath)
				lastErr = fmt.Errorf("kubectl port-forward exited for context %q: %s", ctxName, strings.TrimSpace(string(logData)))
				earlyExit = true
			default:
			}
			if earlyExit {
				break
			}
		}

		if localPort == 0 && !earlyExit {
			_ = cmd.Process.Kill()
			<-exited
			logData, _ := os.ReadFile(logPath)
			lastErr = fmt.Errorf("timeout waiting for kubectl port-forward on context %q: %s", ctxName, strings.TrimSpace(string(logData)))
		}
		if localPort > 0 {
			healthy := false
			for i := 0; i < 15; i++ {
				if isTunnelHealthy(localPort) {
					healthy = true
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if healthy {
				break
			}
			_ = cmd.Process.Kill()
			<-exited
			localPort = 0
			lastErr = fmt.Errorf("port-forward on context %q did not pass /healthz check", ctxName)
		}
	}

	if localPort == 0 {
		if lastErr != nil {
			return "", lastErr
		}
		return "", fmt.Errorf("failed to establish port-forward to %s/%s on context %q", opts.Namespace, opts.Service, ctxName)
	}

	_ = saveTunnel(&TunnelInfo{
		Context:   ctxName,
		Namespace: opts.Namespace,
		Service:   opts.Service,
		Port:      localPort,
		PID:       cmd.Process.Pid,
		CreatedAt: time.Now().UTC(),
	})
	return fmt.Sprintf("http://127.0.0.1:%d", localPort), nil
}
