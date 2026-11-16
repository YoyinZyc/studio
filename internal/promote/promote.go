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

// Package promote implements task promotion ("local-to-remote push") across
// Kubernetes cluster contexts, including preflight checks, sandbox Git branch
// push, and Gateway/Workspace/Task synchronization over public AX v1alpha1 gRPC.
package promote

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agentexecutor/studio/internal/kube"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var (
	validBranchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,120}$`)
	k8sSecretURI  = regexp.MustCompile(`^ate-secret://k8s\.io/default/([^/]+)/([^/]+)/([^/]+)$`)
	nonLabelChars = regexp.MustCompile(`[^a-z0-9-]+`)
)

// GuestExecFunc executes a non-interactive command inside a task's sandbox actor.
type GuestExecFunc func(ctx context.Context, atespace, taskName, kubeContext, axNamespace string, command []string, cwd string) (stdout, stderr string, exitCode int, err error)

// ServerResolver resolves a gRPC endpoint URL for a Kubernetes context.
type ServerResolver func(opts kube.TunnelOptions) (string, error)

// CheckStatus indicates the outcome of a single preflight check.
type CheckStatus string

const (
	// CheckOK indicates the preflight check passed.
	CheckOK CheckStatus = "ok"
	// CheckWarn indicates an actionable warning that does not hard-block promotion.
	CheckWarn CheckStatus = "warn"
	// CheckFail indicates a hard blocker preventing promotion.
	CheckFail CheckStatus = "fail"
)

// CheckItem represents one preflight check row in CUJ 5 ("Run on a Team Cluster").
type CheckItem struct {
	ID     string      `json:"id"`
	Label  string      `json:"label"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
}

// SecretRef identifies a Kubernetes Secret referenced by a Gateway credential URI.
type SecretRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	Present   bool   `json:"present"`
}

// PreflightOptions configures preflight validation before promoting a task.
type PreflightOptions struct {
	SourceContext   string
	SourceServerURL string
	TargetContext   string
	TargetServerURL string
	Atespace        string
	AXNamespace     string
	TaskName        string
	ResolveServer   ServerResolver
	GuestExec       GuestExecFunc
	Runner          kube.Runner
}

// PreflightReport summarizes promotion readiness and carried-over resources.
type PreflightReport struct {
	TaskName        string      `json:"taskName"`
	SourceContext   string      `json:"sourceContext"`
	TargetContext   string      `json:"targetContext"`
	Atespace        string      `json:"atespace"`
	SessionID       string      `json:"sessionId"`
	WorkspaceName   string      `json:"workspaceName,omitempty"`
	GatewayName     string      `json:"gatewayName,omitempty"`
	EgressRuleCount int         `json:"egressRuleCount"`
	GitRepoURL      string      `json:"gitRepoUrl,omitempty"`
	GitRef          string      `json:"gitRef,omitempty"`
	DirtyFiles      []string    `json:"dirtyFiles,omitempty"`
	SuggestedBranch string      `json:"suggestedBranch,omitempty"`
	MissingSecrets  []SecretRef `json:"missingSecrets,omitempty"`
	CanInstallAX    bool        `json:"canInstallAx"`
	CanPromote      bool        `json:"canPromote"`
	Checks          []CheckItem `json:"checks"`
}

// PushBranchOptions configures committing and pushing uncommitted sandbox files.
type PushBranchOptions struct {
	SourceContext   string
	SourceServerURL string
	Atespace        string
	AXNamespace     string
	TaskName        string
	Branch          string
	CommitMessage   string
	ResolveServer   ServerResolver
	GuestExec       GuestExecFunc
}

// PushBranchResult reports the pushed branch and commit SHA.
type PushBranchResult struct {
	Branch    string `json:"branch"`
	CommitSHA string `json:"commitSha"`
	RepoDir   string `json:"repoDir"`
}

// PromoteOptions configures promoting a task from a source cluster to a target cluster.
type PromoteOptions struct {
	SourceContext       string
	SourceServerURL     string
	TargetContext       string
	TargetServerURL     string
	Atespace            string
	AXNamespace         string
	TaskName            string
	TargetTaskName      string
	BranchOverride      string
	HandoffNote         string
	SuspendSource       bool
	ReplaceExistingTask bool
	ResolveServer       ServerResolver
}

// PromoteResult reports the resources synchronized on the target cluster.
type PromoteResult struct {
	SourceContext string `json:"sourceContext"`
	TargetContext string `json:"targetContext"`
	Atespace      string `json:"atespace"`
	TaskName      string `json:"taskName"`
	WorkspaceName string `json:"workspaceName,omitempty"`
	GatewayName   string `json:"gatewayName,omitempty"`
	SessionID     string `json:"sessionId"`
	Branch        string `json:"branch,omitempty"`
}

func resolveDefaults(atespace, axNamespace string, resolver ServerResolver, runner kube.Runner) (string, string, ServerResolver, kube.Runner) {
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	if axNamespace == "" {
		axNamespace = v1alpha1.DefaultSystemNamespace
	}
	if resolver == nil {
		resolver = kube.EnsureServerURL
	}
	if runner == nil {
		runner = kube.OSRunner{}
	}
	return atespace, axNamespace, resolver, runner
}

func dialAX(serverURL string) (v1alpha1.AXClient, *grpc.ClientConn, error) {
	target := strings.TrimPrefix(strings.TrimPrefix(serverURL, "http://"), "https://")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return v1alpha1.NewAXClient(conn), conn, nil
}

// ValidateBranchName verifies that branch is a safe Git ref name without traversal or flags.
func ValidateBranchName(branch string) error {
	if !validBranchRe.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".lock") {
		return fmt.Errorf("invalid git branch name %q", branch)
	}
	return nil
}

// SuggestedBranchName builds a clean `ax/<task>` branch name from a task name.
func SuggestedBranchName(taskName string) string {
	clean := nonLabelChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(taskName)), "-")
	clean = strings.Trim(clean, "-")
	if clean == "" {
		clean = "handoff"
	}
	return "ax/" + clean
}

// Preflight runs the 5 promotion checks against the source and target clusters.
func Preflight(ctx context.Context, opts PreflightOptions) (*PreflightReport, error) {
	if strings.TrimSpace(opts.TaskName) == "" {
		return nil, errors.New("task name is required")
	}
	if strings.TrimSpace(opts.TargetContext) == "" && strings.TrimSpace(opts.TargetServerURL) == "" {
		return nil, errors.New("target cluster context is required")
	}
	atespace, axNS, resolveServer, runner := resolveDefaults(opts.Atespace, opts.AXNamespace, opts.ResolveServer, opts.Runner)

	srcURL := opts.SourceServerURL
	if srcURL == "" {
		var err error
		srcURL, err = resolveServer(kube.TunnelOptions{Context: opts.SourceContext, Namespace: axNS})
		if err != nil {
			return nil, fmt.Errorf("connecting to source cluster: %w", err)
		}
	}
	srcClient, srcConn, err := dialAX(srcURL)
	if err != nil {
		return nil, fmt.Errorf("dialing source cluster: %w", err)
	}
	defer srcConn.Close()

	srcTask, err := srcClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: opts.TaskName})
	if err != nil {
		return nil, fmt.Errorf("loading task %q: %w", opts.TaskName, err)
	}

	var srcWS *v1alpha1.Workspace
	if wsName := srcTask.GetSpec().GetWorkspace().GetName(); wsName != "" {
		ws, wsErr := srcClient.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: atespace, Name: wsName})
		if wsErr != nil {
			return nil, fmt.Errorf("loading bound workspace %q: %w", wsName, wsErr)
		}
		srcWS = ws
	}

	var srcGW *v1alpha1.Gateway
	if gwName := srcTask.GetSpec().GetGateway().GetName(); gwName != "" {
		gw, gwErr := srcClient.GetGateway(ctx, &v1alpha1.GetGatewayRequest{Atespace: atespace, Name: gwName})
		if gwErr != nil {
			return nil, fmt.Errorf("loading bound gateway %q: %w", gwName, gwErr)
		}
		srcGW = gw
	}

	sessionID := srcTask.GetSpec().GetSessionId()
	if sessionID == "" {
		sessionID = srcTask.GetMetadata().GetUid()
	}

	report := &PreflightReport{
		TaskName:        opts.TaskName,
		SourceContext:   opts.SourceContext,
		TargetContext:   opts.TargetContext,
		Atespace:        atespace,
		SessionID:       sessionID,
		SuggestedBranch: SuggestedBranchName(opts.TaskName),
	}
	if srcWS != nil {
		report.WorkspaceName = srcWS.GetMetadata().GetName()
		if repos := srcWS.GetSpec().GetSetup().GetGit(); len(repos) > 0 {
			report.GitRepoURL = repos[0].GetRepo()
			report.GitRef = repos[0].GetRef()
		}
	}
	if srcGW != nil {
		report.GatewayName = srcGW.GetMetadata().GetName()
		report.EgressRuleCount = len(srcGW.GetSpec().GetEgress())
	}

	kubectlBin, _ := kube.ResolveKubectl(runner)

	// Check 1: Target cluster access & AX control plane readiness.
	targetReachable := false
	dstURL := opts.TargetServerURL
	if dstURL == "" {
		var resolveErr error
		dstURL, resolveErr = resolveServer(kube.TunnelOptions{Context: opts.TargetContext, Namespace: axNS})
		if resolveErr != nil {
			hasSubstrate := kubectlBin != "" && namespaceExists(ctx, runner, kubectlBin, opts.TargetContext, "ate-system")
			report.CanInstallAX = hasSubstrate
			detail := fmt.Sprintf("Cannot reach ax-server on %q: %v", opts.TargetContext, resolveErr)
			if hasSubstrate {
				detail = fmt.Sprintf("Agent Substrate is installed on %q, but AX (%s) is not deployed yet. Click 'Install AX' to deploy it.", opts.TargetContext, axNS)
			}
			report.Checks = append(report.Checks, CheckItem{
				ID:     "cluster_access",
				Label:  "Cluster access & AX control plane",
				Status: CheckFail,
				Detail: detail,
			})
		}
	}
	if dstURL != "" {
		dstClient, dstConn, dialErr := dialAX(dstURL)
		if dialErr == nil {
			_, pingErr := dstClient.ListWorkspaces(ctx, &v1alpha1.ListWorkspacesRequest{Atespace: atespace, Limit: 1})
			_ = dstConn.Close()
			if pingErr == nil {
				targetReachable = true
				report.Checks = append(report.Checks, CheckItem{
					ID:     "cluster_access",
					Label:  "Cluster access & AX control plane",
					Status: CheckOK,
					Detail: fmt.Sprintf("Connected to ax-server on %s (atespace %q)", nonEmpty(opts.TargetContext, dstURL), atespace),
				})
			} else {
				report.Checks = append(report.Checks, CheckItem{
					ID:     "cluster_access",
					Label:  "Cluster access & AX control plane",
					Status: CheckFail,
					Detail: fmt.Sprintf("ax-server on %s returned an error: %v", nonEmpty(opts.TargetContext, dstURL), pingErr),
				})
			}
		} else {
			report.Checks = append(report.Checks, CheckItem{
				ID:     "cluster_access",
				Label:  "Cluster access & AX control plane",
				Status: CheckFail,
				Detail: fmt.Sprintf("Failed to dial ax-server: %v", dialErr),
			})
		}
	}

	// Check 2: Substrate / AX version compatibility.
	expectedSubVer := kube.ExpectedSubstrateVersion
	if !targetReachable {
		report.Checks = append(report.Checks, CheckItem{
			ID:     "version_compat",
			Label:  "Compatible Substrate & AX version",
			Status: CheckWarn,
			Detail: "Skipped until target cluster is reachable",
		})
	} else {
		remoteVer := ""
		if kubectlBin != "" && opts.TargetContext != "" {
			remoteVer = readRemoteSubstrateVersion(ctx, runner, kubectlBin, opts.TargetContext)
		}
		switch remoteVer {
		case "", expectedSubVer:
			detail := fmt.Sprintf("AX v1alpha1 ready (Substrate %s)", nonEmpty(remoteVer, expectedSubVer))
			report.Checks = append(report.Checks, CheckItem{
				ID:     "version_compat",
				Label:  "Compatible Substrate & AX version",
				Status: CheckOK,
				Detail: detail,
			})
		default:
			report.Checks = append(report.Checks, CheckItem{
				ID:     "version_compat",
				Label:  "Compatible Substrate & AX version",
				Status: CheckWarn,
				Detail: fmt.Sprintf("Target runs Substrate %s (Studio targets %s)", remoteVer, expectedSubVer),
			})
		}
	}

	// Check 3: Manifest validation & registry reachability.
	manifestCheck := validateManifestsForRemote(srcTask, srcWS, srcGW)
	report.Checks = append(report.Checks, manifestCheck)

	// Check 4: Workspace rebuildability & remote credential Secrets.
	wsCheck, missingSecrets := checkWorkspaceAndSecrets(ctx, runner, kubectlBin, opts.TargetContext, srcWS, srcGW, targetReachable)
	report.MissingSecrets = missingSecrets
	report.Checks = append(report.Checks, wsCheck)

	// Check 5: Uncommitted sandbox changes (inspect via `ax ssh` when debug=true).
	changesCheck, dirtyFiles := inspectUncommittedSandboxFiles(ctx, opts, srcTask, srcWS, atespace, axNS)
	report.DirtyFiles = dirtyFiles
	report.Checks = append(report.Checks, changesCheck)

	canPromote := true
	for _, c := range report.Checks {
		if c.Status == CheckFail {
			canPromote = false
			break
		}
	}
	report.CanPromote = canPromote
	return report, nil
}

func validateManifestsForRemote(task *v1alpha1.Task, ws *v1alpha1.Workspace, gw *v1alpha1.Gateway) CheckItem {
	if err := kube.ValidateTaskForPromotion(task); err != nil {
		return CheckItem{ID: "manifest_valid", Label: "Task manifest passes cluster validation", Status: CheckFail, Detail: err.Error()}
	}
	if err := kube.ValidateWorkspaceForPromotion(ws); err != nil {
		return CheckItem{ID: "manifest_valid", Label: "Task manifest passes cluster validation", Status: CheckFail, Detail: err.Error()}
	}
	if err := kube.ValidateGatewayForPromotion(gw); err != nil {
		return CheckItem{ID: "manifest_valid", Label: "Task manifest passes cluster validation", Status: CheckFail, Detail: err.Error()}
	}
	summary := "Task spec validated"
	if gw != nil {
		summary = fmt.Sprintf("Task, Workspace & Gateway validated (%d egress allowlist rule(s) carry over)", len(gw.GetSpec().GetEgress()))
	} else if ws != nil {
		summary = "Task & Workspace manifests validated"
	}
	return CheckItem{ID: "manifest_valid", Label: "Task manifest passes cluster validation", Status: CheckOK, Detail: summary}
}

func checkWorkspaceAndSecrets(ctx context.Context, runner kube.Runner, kubectlBin, targetCtx string, ws *v1alpha1.Workspace, gw *v1alpha1.Gateway, targetReachable bool) (CheckItem, []SecretRef) {
	var missing []SecretRef
	if gw != nil {
		seen := map[string]bool{}
		for _, rule := range gw.GetSpec().GetEgress() {
			httpsRule := rule.GetHttps()
			if httpsRule == nil || httpsRule.GetEffects() == nil {
				continue
			}
			for _, rh := range httpsRule.GetEffects().GetReplaceHeaders() {
				m := k8sSecretURI.FindStringSubmatch(rh.GetCredentialUri())
				if len(m) != 4 {
					continue
				}
				key := m[1] + "/" + m[2] + "/" + m[3]
				if seen[key] {
					continue
				}
				seen[key] = true
				present := true
				if targetReachable && kubectlBin != "" && targetCtx != "" {
					present = secretExists(ctx, runner, kubectlBin, targetCtx, m[1], m[2])
				}
				if !present {
					missing = append(missing, SecretRef{Namespace: m[1], Name: m[2], Key: m[3], Present: false})
				}
			}
		}
	}

	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for _, s := range missing {
			names = append(names, fmt.Sprintf("%s/%s", s.Namespace, s.Name))
		}
		return CheckItem{
			ID:     "workspace_rebuild",
			Label:  "Workspace & credentials ready on target",
			Status: CheckWarn,
			Detail: fmt.Sprintf("Credential Secret(s) %s not found on %s; create or sync the credential before running.", strings.Join(names, ", "), targetCtx),
		}, missing
	}

	if ws == nil {
		return CheckItem{
			ID:     "workspace_rebuild",
			Label:  "Workspace & credentials ready on target",
			Status: CheckOK,
			Detail: "No Workspace bound (task starts with an empty /workspace)",
		}, nil
	}

	repos := ws.GetSpec().GetSetup().GetGit()
	files := ws.GetSpec().GetSetup().GetFiles()
	if len(repos) > 0 {
		ref := repos[0].GetRef()
		if ref == "" {
			ref = "main"
		}
		return CheckItem{
			ID:     "workspace_rebuild",
			Label:  "Workspace & credentials ready on target",
			Status: CheckOK,
			Detail: fmt.Sprintf("Clones %s (ref %q) cleanly on remote cluster", repos[0].GetRepo(), ref),
		}, nil
	}
	if len(files) > 0 {
		return CheckItem{
			ID:     "workspace_rebuild",
			Label:  "Workspace & credentials ready on target",
			Status: CheckOK,
			Detail: fmt.Sprintf("Materializes %d inline setup file(s) on remote cluster", len(files)),
		}, nil
	}
	return CheckItem{
		ID:     "workspace_rebuild",
		Label:  "Workspace & credentials ready on target",
		Status: CheckOK,
		Detail: fmt.Sprintf("Workspace %q carries over unchanged", ws.GetMetadata().GetName()),
	}, nil
}

func inspectUncommittedSandboxFiles(ctx context.Context, opts PreflightOptions, task *v1alpha1.Task, ws *v1alpha1.Workspace, atespace, axNS string) (CheckItem, []string) {
	if opts.GuestExec == nil || !task.GetSpec().GetDebug() || task.GetStatus().GetActor() == "" {
		return CheckItem{
			ID:     "uncommitted_changes",
			Label:  "Sandbox working tree synced to Git",
			Status: CheckOK,
			Detail: "Sandbox debug inspection off or actor not active; remote task will clone the Workspace Git ref directly",
		}, nil
	}
	state := task.GetStatus().GetState()
	if state == v1alpha1.TaskState_TASK_STATE_SUSPENDED || state == v1alpha1.TaskState_TASK_STATE_SUSPENDING || state == v1alpha1.TaskState_TASK_STATE_CANCELLED {
		return CheckItem{
			ID:     "uncommitted_changes",
			Label:  "Sandbox working tree synced to Git",
			Status: CheckOK,
			Detail: "Task is suspended; remote task will clone the configured Workspace Git ref",
		}, nil
	}

	taskName := task.GetMetadata().GetName()
	repoDir, err := LocateSandboxGitDir(ctx, opts.GuestExec, atespace, taskName, opts.SourceContext, axNS, task, ws)
	if err != nil || repoDir == "" {
		return CheckItem{
			ID:     "uncommitted_changes",
			Label:  "Sandbox working tree synced to Git",
			Status: CheckOK,
			Detail: "No Git repository detected inside /workspace",
		}, nil
	}

	stdout, _, exitCode, execErr := opts.GuestExec(ctx, atespace, taskName, opts.SourceContext, axNS, []string{"git", "status", "--porcelain"}, repoDir)
	if execErr != nil || exitCode != 0 {
		return CheckItem{
			ID:     "uncommitted_changes",
			Label:  "Sandbox working tree synced to Git",
			Status: CheckOK,
			Detail: "Could not read git status inside sandbox; remote task will use configured Git ref",
		}, nil
	}

	var dirty []string
	for _, line := range strings.Split(strings.TrimRight(stdout, "\r\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rel string
		if len(line) > 3 {
			rel = strings.TrimSpace(line[3:])
		} else {
			rel = strings.TrimSpace(line)
		}
		if rel == ".ax" || strings.HasPrefix(rel, ".ax/") {
			continue
		}
		dirty = append(dirty, rel)
	}
	if len(dirty) == 0 {
		return CheckItem{
			ID:     "uncommitted_changes",
			Label:  "Sandbox working tree synced to Git",
			Status: CheckOK,
			Detail: "Working tree inside /workspace is clean",
		}, nil
	}

	branch := SuggestedBranchName(taskName)
	return CheckItem{
		ID:     "uncommitted_changes",
		Label:  "Sandbox working tree synced to Git",
		Status: CheckWarn,
		Detail: fmt.Sprintf("%d uncommitted file(s) inside local sandbox aren't on a remote branch yet. Push to %s first so the remote cluster picks them up.", len(dirty), branch),
	}, dirty
}

// LocateSandboxGitDir finds the primary git working tree inside the task's workspace.
func LocateSandboxGitDir(ctx context.Context, guestExec GuestExecFunc, atespace, taskName, kubeContext, axNS string, task *v1alpha1.Task, ws *v1alpha1.Workspace) (string, error) {
	wsRoot := task.GetSpec().GetWorkspace().GetPath()
	if wsRoot == "" {
		wsRoot = v1alpha1.DefaultWorkspacePath
	}

	var candidates []string
	if ws != nil {
		for _, g := range ws.GetSpec().GetSetup().GetGit() {
			dir := strings.TrimSpace(g.GetDir())
			switch {
			case dir == "" && g.GetRepo() != "":
				base := strings.TrimSuffix(g.GetRepo(), ".git")
				if idx := strings.LastIndex(base, "/"); idx >= 0 {
					base = base[idx+1:]
				}
				if base != "" && base != "." {
					candidates = append(candidates, wsRoot+"/"+base)
				}
			case dir == ".":
				candidates = append(candidates, wsRoot)
			default:
				candidates = append(candidates, wsRoot+"/"+strings.TrimPrefix(dir, "/"))
			}
		}
	}
	candidates = append(candidates, wsRoot)

	for _, dir := range candidates {
		out, _, code, err := guestExec(ctx, atespace, taskName, kubeContext, axNS, []string{"git", "rev-parse", "--show-toplevel"}, dir)
		if err == nil && code == 0 && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out), nil
		}
	}
	return "", nil
}

// PushSandboxBranch commits uncommitted changes inside a local task sandbox and pushes them to origin/<branch>.
func PushSandboxBranch(ctx context.Context, opts PushBranchOptions) (*PushBranchResult, error) {
	if opts.GuestExec == nil {
		return nil, errors.New("guest execution is not configured")
	}
	branch := strings.TrimSpace(opts.Branch)
	if branch == "" {
		branch = SuggestedBranchName(opts.TaskName)
	}
	if err := ValidateBranchName(branch); err != nil {
		return nil, err
	}

	atespace, axNS, resolveServer, _ := resolveDefaults(opts.Atespace, opts.AXNamespace, opts.ResolveServer, nil)
	srcURL := opts.SourceServerURL
	if srcURL == "" {
		var err error
		srcURL, err = resolveServer(kube.TunnelOptions{Context: opts.SourceContext, Namespace: axNS})
		if err != nil {
			return nil, fmt.Errorf("connecting to source cluster: %w", err)
		}
	}
	srcClient, srcConn, err := dialAX(srcURL)
	if err != nil {
		return nil, err
	}
	defer srcConn.Close()

	task, err := srcClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: opts.TaskName})
	if err != nil {
		return nil, fmt.Errorf("fetching task %q: %w", opts.TaskName, err)
	}
	if !task.GetSpec().GetDebug() || task.GetStatus().GetActor() == "" {
		return nil, fmt.Errorf("task %q does not have an active debug-enabled sandbox", opts.TaskName)
	}
	var ws *v1alpha1.Workspace
	if wsName := task.GetSpec().GetWorkspace().GetName(); wsName != "" {
		ws, _ = srcClient.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: atespace, Name: wsName})
	}

	repoDir, err := LocateSandboxGitDir(ctx, opts.GuestExec, atespace, opts.TaskName, opts.SourceContext, axNS, task, ws)
	if err != nil || repoDir == "" {
		return nil, fmt.Errorf("no git repository found in sandbox for task %q", opts.TaskName)
	}

	commitMsg := strings.TrimSpace(opts.CommitMessage)
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("chore(ax): checkpoint changes from task %s", opts.TaskName)
	}

	if out, _, _, _ := opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "config", "user.name"}, repoDir); strings.TrimSpace(out) == "" {
		_, _, _, _ = opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "config", "user.name", "AX Studio"}, repoDir)
	}
	if out, _, _, _ := opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "config", "user.email"}, repoDir); strings.TrimSpace(out) == "" {
		_, _, _, _ = opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "config", "user.email", "ax-studio@ax.io"}, repoDir)
	}

	if _, stderr, code, err := opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "checkout", "-B", branch}, repoDir); err != nil || code != 0 {
		return nil, fmt.Errorf("checking out branch %q in sandbox: %s %v", branch, strings.TrimSpace(stderr), err)
	}
	if _, stderr, code, err := opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "add", "-A"}, repoDir); err != nil || code != 0 {
		return nil, fmt.Errorf("staging files in sandbox: %s %v", strings.TrimSpace(stderr), err)
	}
	// Never commit ax-task-runner's internal `.ax/` runtime directory when repoDir == /workspace.
	_, _, _, _ = opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "reset", "-q", "--", ".ax"}, repoDir)

	stOut, _, _, _ := opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "status", "--porcelain"}, repoDir)
	if strings.TrimSpace(stOut) != "" {
		if _, stderr, code, err := opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "commit", "-m", commitMsg}, repoDir); err != nil || code != 0 {
			return nil, fmt.Errorf("committing sandbox changes: %s %v", strings.TrimSpace(stderr), err)
		}
	}

	if _, stderr, code, err := opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "push", "-u", "origin", branch}, repoDir); err != nil || code != 0 {
		return nil, fmt.Errorf("pushing branch %q to origin (ensure Gateway allows git host and credentials): %s %v", branch, strings.TrimSpace(stderr), err)
	}

	shaOut, _, _, _ := opts.GuestExec(ctx, atespace, opts.TaskName, opts.SourceContext, axNS, []string{"git", "rev-parse", "--short", "HEAD"}, repoDir)
	return &PushBranchResult{
		Branch:    branch,
		CommitSHA: strings.TrimSpace(shaOut),
		RepoDir:   repoDir,
	}, nil
}

// Promote copies a Task's bound Gateway, Workspace, and a child Task (sharing session_id)
// onto the target cluster context and resumes the promoted task.
func Promote(ctx context.Context, opts PromoteOptions) (*PromoteResult, error) {
	if strings.TrimSpace(opts.TaskName) == "" {
		return nil, errors.New("task name is required")
	}
	if strings.TrimSpace(opts.TargetContext) == "" && strings.TrimSpace(opts.TargetServerURL) == "" {
		return nil, errors.New("target cluster context is required")
	}
	if opts.BranchOverride != "" {
		if err := ValidateBranchName(opts.BranchOverride); err != nil {
			return nil, err
		}
	}

	atespace, axNS, resolveServer, _ := resolveDefaults(opts.Atespace, opts.AXNamespace, opts.ResolveServer, nil)

	srcURL := opts.SourceServerURL
	if srcURL == "" {
		var err error
		srcURL, err = resolveServer(kube.TunnelOptions{Context: opts.SourceContext, Namespace: axNS})
		if err != nil {
			return nil, fmt.Errorf("resolving source cluster: %w", err)
		}
	}
	srcClient, srcConn, err := dialAX(srcURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to source cluster: %w", err)
	}
	defer srcConn.Close()

	srcTask, err := srcClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: opts.TaskName})
	if err != nil {
		return nil, fmt.Errorf("getting task %q: %w", opts.TaskName, err)
	}

	var srcWS *v1alpha1.Workspace
	if wsName := srcTask.GetSpec().GetWorkspace().GetName(); wsName != "" {
		ws, err := srcClient.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: atespace, Name: wsName})
		if err != nil {
			return nil, fmt.Errorf("getting workspace %q: %w", wsName, err)
		}
		srcWS = ws
	}

	var srcGW *v1alpha1.Gateway
	if gwName := srcTask.GetSpec().GetGateway().GetName(); gwName != "" {
		gw, err := srcClient.GetGateway(ctx, &v1alpha1.GetGatewayRequest{Atespace: atespace, Name: gwName})
		if err != nil {
			return nil, fmt.Errorf("getting gateway %q: %w", gwName, err)
		}
		srcGW = gw
	}

	if opts.SuspendSource {
		_, suspErr := srcClient.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{
			Atespace: atespace,
			Name:     opts.TaskName,
		})
		if suspErr != nil && status.Code(suspErr) != codes.FailedPrecondition {
			return nil, fmt.Errorf("suspending source task %q: %w", opts.TaskName, suspErr)
		}
	}

	dstURL := opts.TargetServerURL
	if dstURL == "" {
		dstURL, err = resolveServer(kube.TunnelOptions{Context: opts.TargetContext, Namespace: axNS})
		if err != nil {
			return nil, fmt.Errorf("resolving target cluster %q: %w", opts.TargetContext, err)
		}
	}
	dstClient, dstConn, err := dialAX(dstURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to target cluster %q: %w", opts.TargetContext, err)
	}
	defer dstConn.Close()

	res := &PromoteResult{
		SourceContext: opts.SourceContext,
		TargetContext: opts.TargetContext,
		Atespace:      atespace,
		Branch:        opts.BranchOverride,
	}

	// 1. Sync Gateway (create or update).
	if srcGW != nil {
		promotedGW := proto.Clone(srcGW).(*v1alpha1.Gateway)
		cleanObjectMeta(promotedGW.Metadata, atespace)
		if _, err := dstClient.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{Gateway: promotedGW}); err != nil {
			if status.Code(err) == codes.AlreadyExists {
				if _, upErr := dstClient.UpdateGateway(ctx, &v1alpha1.UpdateGatewayRequest{Gateway: promotedGW}); upErr != nil {
					return nil, fmt.Errorf("updating gateway %q on target: %w", promotedGW.GetMetadata().GetName(), upErr)
				}
			} else {
				return nil, fmt.Errorf("creating gateway %q on target: %w", promotedGW.GetMetadata().GetName(), err)
			}
		}
		res.GatewayName = promotedGW.GetMetadata().GetName()
	}

	// 2. Sync Workspace (immutable on target; derive suffixed name when branch override mutates spec).
	targetWSName := ""
	if srcWS != nil {
		promotedWS := proto.Clone(srcWS).(*v1alpha1.Workspace)
		promotedWS.Status = nil
		cleanObjectMeta(promotedWS.Metadata, atespace)
		if opts.BranchOverride != "" && promotedWS.GetSpec().GetSetup() != nil {
			for _, g := range promotedWS.GetSpec().GetSetup().GetGit() {
				g.Ref = opts.BranchOverride
			}
		}
		_, wsErr := dstClient.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{Workspace: promotedWS})
		if wsErr != nil && (status.Code(wsErr) == codes.FailedPrecondition || status.Code(wsErr) == codes.AlreadyExists) {
			sum := sha256.Sum256([]byte(opts.BranchOverride + ":" + time.Now().UTC().Format("150405")))
			promotedWS.Metadata.Name = truncateLabel(promotedWS.Metadata.Name, hex.EncodeToString(sum[:])[:6])
			if _, wsErr2 := dstClient.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{Workspace: promotedWS}); wsErr2 != nil {
				return nil, fmt.Errorf("creating workspace %q on target: %w", promotedWS.Metadata.Name, wsErr2)
			}
		} else if wsErr != nil {
			return nil, fmt.Errorf("promoting workspace %q to target: %w", promotedWS.Metadata.Name, wsErr)
		}
		targetWSName = promotedWS.Metadata.Name
		res.WorkspaceName = targetWSName
	}

	// 3. Create & Resume child Task sharing session_id.
	promotedTask := proto.Clone(srcTask).(*v1alpha1.Task)
	promotedTask.Status = nil
	cleanObjectMeta(promotedTask.Metadata, atespace)
	if promotedTask.Spec == nil {
		promotedTask.Spec = &v1alpha1.TaskSpec{}
	}

	sessionID := srcTask.GetSpec().GetSessionId()
	if sessionID == "" {
		sessionID = srcTask.GetMetadata().GetUid()
	}
	if sessionID != "" {
		promotedTask.Spec.SessionId = sessionID
	}
	if targetWSName != "" && promotedTask.Spec.Workspace != nil {
		promotedTask.Spec.Workspace.Name = targetWSName
	}
	if note := strings.TrimSpace(opts.HandoffNote); note != "" {
		existingGoal := strings.TrimSpace(promotedTask.GetSpec().GetGoal().GetSpec())
		if existingGoal != "" {
			promotedTask.Spec.Goal = &v1alpha1.Goal{Spec: existingGoal + "\n\nHandoff note:\n" + note}
		} else {
			promotedTask.Spec.Goal = &v1alpha1.Goal{Spec: note}
		}
	}

	if opts.TargetTaskName != "" {
		promotedTask.Metadata.Name = opts.TargetTaskName
	}

	if opts.ReplaceExistingTask {
		_, _ = dstClient.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{
			Atespace: atespace,
			Name:     promotedTask.Metadata.Name,
		})
	}

	createdTask, createErr := dstClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: promotedTask})
	if createErr != nil && status.Code(createErr) == codes.AlreadyExists && opts.TargetTaskName == "" && !opts.ReplaceExistingTask {
		promotedTask.Metadata.Name = truncateLabel(promotedTask.Metadata.Name, randomHex(3))
		createdTask, createErr = dstClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: promotedTask})
	}
	if createErr != nil {
		return nil, fmt.Errorf("creating task %q on target: %w", promotedTask.Metadata.Name, createErr)
	}

	resumedTask, resumeErr := dstClient.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{
		Atespace: atespace,
		Name:     createdTask.GetMetadata().GetName(),
	})
	if resumeErr != nil {
		return nil, fmt.Errorf("resuming promoted task %q on target: %w", createdTask.GetMetadata().GetName(), resumeErr)
	}

	res.TaskName = resumedTask.GetMetadata().GetName()
	res.SessionID = resumedTask.GetSpec().GetSessionId()
	return res, nil
}

func cleanObjectMeta(meta *v1alpha1.ObjectMeta, atespace string) {
	if meta == nil {
		return
	}
	meta.Atespace = atespace
	meta.Uid = ""
	meta.Owner = ""
	meta.CreateTime = nil
	meta.UpdateTime = nil
	meta.DeleteTime = nil
}

func truncateLabel(base, suffix string) string {
	base = strings.Trim(base, "-")
	maxBase := 63 - 1 - len(suffix)
	if len(base) > maxBase {
		base = strings.TrimRight(base[:maxBase], "-")
	}
	if base == "" {
		base = "task"
	}
	return base + "-" + suffix
}

func randomHex(numBytes int) string {
	b := make([]byte, numBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func namespaceExists(ctx context.Context, r kube.Runner, kubectlBin, kubeCtx, ns string) bool {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"--request-timeout=4s", "get", "ns", ns, "-o", "name"}
	if kubeCtx != "" {
		args = append([]string{"--context", kubeCtx}, args...)
	}
	_, err := r.Run(c, kube.Spec{Name: kubectlBin, Args: args})
	return err == nil
}

func secretExists(ctx context.Context, r kube.Runner, kubectlBin, kubeCtx, ns, name string) bool {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"--request-timeout=4s", "-n", ns, "get", "secret", name, "-o", "name"}
	if kubeCtx != "" {
		args = append([]string{"--context", kubeCtx}, args...)
	}
	_, err := r.Run(c, kube.Spec{Name: kubectlBin, Args: args})
	return err == nil
}

func readRemoteSubstrateVersion(ctx context.Context, r kube.Runner, kubectlBin, kubeCtx string) string {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"--request-timeout=4s", "get", "ns", "ate-system", "-o", `jsonpath={.metadata.annotations['ax\.io/substrate-version']}`}
	if kubeCtx != "" {
		args = append([]string{"--context", kubeCtx}, args...)
	}
	res, err := r.Run(c, kube.Spec{Name: kubectlBin, Args: args})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}
