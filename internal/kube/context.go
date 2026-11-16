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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ContextEntry describes a configured kubeconfig context.
type ContextEntry struct {
	Name    string
	Cluster string
	User    string
	Current bool
}

type kubeConfig struct {
	CurrentContext string `yaml:"current-context"`
	Contexts       []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

func kubeconfigPaths() []string {
	if p := os.Getenv("KUBECONFIG"); p != "" {
		var out []string
		for _, item := range filepath.SplitList(p) {
			if item != "" {
				out = append(out, item)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return []string{filepath.Join(home, ".kube", "config")}
	}
	return nil
}

// CurrentContext returns the active Kubernetes context from override, $KUBECONTEXT,
// or kubeconfig.
func CurrentContext(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("KUBECONTEXT"); env != "" {
		return env, nil
	}
	parsedAny := false
	for _, path := range kubeconfigPaths() {
		if data, err := os.ReadFile(path); err == nil {
			var cfg kubeConfig
			if err := yaml.Unmarshal(data, &cfg); err == nil {
				parsedAny = true
				if cfg.CurrentContext != "" {
					for _, c := range cfg.Contexts {
						if c.Name == cfg.CurrentContext {
							return cfg.CurrentContext, nil
						}
					}
				}
			}
		}
	}
	if parsedAny {
		return "", nil
	}
	if kubectlBin, err := ResolveKubectl(nil); err == nil && kubectlBin != "" {
		if out, runErr := exec.Command(kubectlBin, "config", "current-context").Output(); runErr == nil {
			if ctx := strings.TrimSpace(string(out)); ctx != "" {
				return ctx, nil
			}
		}
	}
	return "", nil
}

// ListContexts returns all contexts defined in the user's kubeconfig files,
// sorted by name, with Current set on the active context.
func ListContexts(overrideCurrent string) ([]ContextEntry, error) {
	active, _ := CurrentContext(overrideCurrent)
	seen := make(map[string]ContextEntry)

	for _, path := range kubeconfigPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg kubeConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			continue
		}
		for _, c := range cfg.Contexts {
			if c.Name == "" {
				continue
			}
			if _, exists := seen[c.Name]; !exists {
				seen[c.Name] = ContextEntry{
					Name:    c.Name,
					Cluster: c.Context.Cluster,
					User:    c.Context.User,
					Current: c.Name == active,
				}
			}
		}
	}

	out := make([]ContextEntry, 0, len(seen))
	for _, entry := range seen {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SetCurrentContext switches `current-context` in kubeconfig to target, using
// `kubectl config use-context` when available or a pure-Go YAML update offline.
func SetCurrentContext(target string) error {
	target = strings.TrimSpace(target)
	if target == "" || strings.HasPrefix(target, "-") {
		return fmt.Errorf("invalid context name %q", target)
	}
	if kubectlBin, err := ResolveKubectl(nil); err == nil && kubectlBin != "" {
		out, runErr := exec.Command(kubectlBin, "config", "use-context", target).CombinedOutput()
		if runErr != nil {
			if msg := strings.TrimSpace(string(out)); msg != "" {
				return fmt.Errorf("%s", msg)
			}
			return fmt.Errorf("switching kubeconfig context to %q: %w", target, runErr)
		}
		return nil
	}
	return setCurrentContextYAML(target)
}

func setCurrentContextYAML(target string) error {
	entries, err := ListContexts("")
	if err != nil {
		return err
	}
	found := false
	for _, e := range entries {
		if e.Name == target {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no context exists with the name: %q", target)
	}

	paths := kubeconfigPaths()
	if len(paths) == 0 {
		return fmt.Errorf("cannot locate kubeconfig path")
	}
	primary := paths[0]
	raw, err := os.ReadFile(primary)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("unexpected kubeconfig YAML root")
	}
	mapping := doc.Content[0]
	updated := false
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "current-context" {
			mapping.Content[i+1].Value = target
			updated = true
			break
		}
	}
	if !updated {
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "current-context"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: target},
		)
	}
	encoded, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(primary), ".kubeconfig-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, primary); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
