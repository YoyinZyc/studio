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
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

var (
	dns1123LabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	sha256DigestRe = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// ValidateName verifies that name is a valid RFC 1123 DNS label (1..63 lowercase
// alphanumeric characters or '-', starting and ending with alphanumeric).
func ValidateName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("name is required")
	}
	if len(trimmed) > 63 {
		return fmt.Errorf("name %q exceeds 63 characters", trimmed)
	}
	if !dns1123LabelRe.MatchString(trimmed) {
		return fmt.Errorf("name %q must consist of lowercase alphanumeric characters or '-', and start and end with an alphanumeric character", trimmed)
	}
	return nil
}

// ValidateImageRef verifies that a container image reference is non-empty and
// digest-pinned (`@sha256:...`), as required by Agent Substrate.
func ValidateImageRef(image string) error {
	trimmed := strings.TrimSpace(image)
	if trimmed == "" {
		return nil
	}
	if strings.ContainsAny(trimmed, " \t\r\n") {
		return fmt.Errorf("image %q must not contain whitespace", trimmed)
	}
	if !sha256DigestRe.MatchString(trimmed) {
		return fmt.Errorf("image %q must be digest-pinned (@sha256:<64-hex-chars>)", trimmed)
	}
	return nil
}

// ValidateTaskForPromotion performs client-side preflight checks on a Task
// manifest prior to promoting it to a remote cluster.
func ValidateTaskForPromotion(task *v1alpha1.Task) error {
	if task == nil {
		return errors.New("task is nil")
	}
	if err := ValidateName(task.GetMetadata().GetName()); err != nil {
		return fmt.Errorf("task metadata.name: %w", err)
	}
	if task.GetSpec() == nil {
		return errors.New("task spec is required")
	}
	img := strings.TrimSpace(task.GetSpec().GetProfile().GetImage())
	if err := ValidateImageRef(img); err != nil {
		return fmt.Errorf("task spec.profile.image: %w", err)
	}
	if strings.HasPrefix(img, "localhost:") || strings.HasPrefix(img, "127.0.0.1:") {
		return fmt.Errorf("task image %q points to a loopback registry that remote clusters cannot pull", img)
	}
	return nil
}

// ValidateWorkspaceForPromotion checks a Workspace manifest prior to promotion.
func ValidateWorkspaceForPromotion(ws *v1alpha1.Workspace) error {
	if ws == nil {
		return nil
	}
	if err := ValidateName(ws.GetMetadata().GetName()); err != nil {
		return fmt.Errorf("workspace metadata.name: %w", err)
	}
	for _, repo := range ws.GetSpec().GetSetup().GetGit() {
		if strings.TrimSpace(repo.GetRepo()) == "" {
			return errors.New("workspace git setup entry has an empty repo URL")
		}
	}
	return nil
}

// ValidateGatewayForPromotion checks a Gateway manifest prior to promotion.
func ValidateGatewayForPromotion(gw *v1alpha1.Gateway) error {
	if gw == nil {
		return nil
	}
	if err := ValidateName(gw.GetMetadata().GetName()); err != nil {
		return fmt.Errorf("gateway metadata.name: %w", err)
	}
	return nil
}
