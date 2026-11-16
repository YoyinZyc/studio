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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type recordingRunner struct {
	specs []Spec
}

func (r *recordingRunner) LookPath(file string) (string, error) {
	return "/usr/bin/" + file, nil
}

func (r *recordingRunner) Run(_ context.Context, spec Spec) (Result, error) {
	r.specs = append(r.specs, spec)
	return Result{Stdout: "ok\n", ExitCode: 0}, nil
}

func TestValidateNameAndImageRef(t *testing.T) {
	for _, valid := range []string{"a", "my-task-1", "local"} {
		if err := ValidateName(valid); err != nil {
			t.Errorf("ValidateName(%q) unexpected error: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "-flag", "UpperCase", "bad_underscore", strings.Repeat("a", 64)} {
		if err := ValidateName(invalid); err == nil {
			t.Errorf("ValidateName(%q) expected error, got nil", invalid)
		}
	}

	validImg := "ghcr.io/google/ax/ax-task-runner@sha256:5382526a63b0cabf694ac3e0b7ac5fe95c94975c563677e7078169f05ff911b0"
	if err := ValidateImageRef(validImg); err != nil {
		t.Errorf("ValidateImageRef(%q) unexpected error: %v", validImg, err)
	}
	if err := ValidateImageRef("ghcr.io/google/ax/ax-task-runner:latest"); err == nil {
		t.Errorf("expected tag-only image to fail digest check")
	}
}

func TestValidateContainerHost(t *testing.T) {
	var warn bytes.Buffer
	if err := ValidateContainerHost(ContainerHostInfo{
		KernelVersion: "6.12.5-linuxkit",
		MemTotalBytes: 8 * 1024 * 1024 * 1024,
		Rootless:      false,
	}, false, "darwin", &warn); err != nil || warn.Len() > 0 {
		t.Fatalf("expected healthy host to pass without warnings, err=%v warn=%q", err, warn.String())
	}

	if err := ValidateContainerHost(ContainerHostInfo{
		KernelVersion: "6.6.12",
		MemTotalBytes: 8 * 1024 * 1024 * 1024,
	}, false, "linux", nil); err == nil {
		t.Fatal("expected < 6.8 kernel to fail validation")
	}

	if err := ValidateContainerHost(ContainerHostInfo{
		KernelVersion: "6.12.0",
		Rootless:      true,
	}, true, "darwin", nil); err == nil {
		t.Fatal("expected rootless Podman to fail validation")
	}
}

func TestOfflineKubeconfigContextSwitch(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config")
	initial := `apiVersion: v1
kind: Config
current-context: local
contexts:
- name: local
  context:
    cluster: kind-local
    user: kind-local
- name: team-gke
  context:
    cluster: gke-cluster
    user: gke-user
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", cfgPath)
	t.Setenv("KUBECONTEXT", "")

	cur, err := CurrentContext("")
	if err != nil || cur != "local" {
		t.Fatalf("expected current context 'local', got %q (%v)", cur, err)
	}

	if err := setCurrentContextYAML("team-gke"); err != nil {
		t.Fatalf("setCurrentContextYAML failed: %v", err)
	}
	curAfter, err := CurrentContext("")
	if err != nil || curAfter != "team-gke" {
		t.Fatalf("expected switched context 'team-gke', got %q (%v)", curAfter, err)
	}
	entries, err := ListContexts("")
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected 2 contexts, got %+v (%v)", entries, err)
	}
}

func TestExecInTaskSandboxPositionalArgs(t *testing.T) {
	rec := &recordingRunner{}
	_, _, _, err := ExecInTaskSandbox(
		context.Background(),
		rec,
		"/usr/local/bin/ax",
		"default",
		"demo-task",
		"local",
		"ax-system",
		[]string{"git", "diff", "--no-index", "--", "/dev/null", "notes with space.txt"},
		"/workspace/repo",
	)
	if err != nil {
		t.Fatalf("ExecInTaskSandbox unexpected error: %v", err)
	}
	if len(rec.specs) != 1 {
		t.Fatalf("expected 1 recorded command, got %d", len(rec.specs))
	}
	wantArgs := []string{
		"--context", "local",
		"-n", "ax-system",
		"-a", "default",
		"ssh", "demo-task", "--",
		"/bin/sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", "/workspace/repo",
		"git", "diff", "--no-index", "--", "/dev/null", "notes with space.txt",
	}
	if !reflect.DeepEqual(rec.specs[0].Args, wantArgs) {
		t.Fatalf("unexpected args:\ngot:  %#v\nwant: %#v", rec.specs[0].Args, wantArgs)
	}
}
