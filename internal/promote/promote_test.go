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

package promote

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/agentexecutor/studio/internal/kube"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type fakeAXServer struct {
	v1alpha1.UnimplementedAXServer
	mu         sync.Mutex
	tasks      map[string]*v1alpha1.Task
	workspaces map[string]*v1alpha1.Workspace
	gateways   map[string]*v1alpha1.Gateway
	resumed    []string
	suspended  []string
}

func newFakeAXServer() *fakeAXServer {
	return &fakeAXServer{
		tasks:      map[string]*v1alpha1.Task{},
		workspaces: map[string]*v1alpha1.Workspace{},
		gateways:   map[string]*v1alpha1.Gateway{},
	}
}

func (f *fakeAXServer) start(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	v1alpha1.RegisterAXServer(srv, f)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return "http://" + ln.Addr().String()
}

func (f *fakeAXServer) GetTask(_ context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return proto.Clone(t).(*v1alpha1.Task), nil
}

func (f *fakeAXServer) CreateTask(_ context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetTask().GetMetadata().GetName()
	if _, exists := f.tasks[name]; exists {
		return nil, status.Error(codes.AlreadyExists, "task already exists")
	}
	cp := proto.Clone(req.GetTask()).(*v1alpha1.Task)
	if cp.Spec.GetSessionId() == "" {
		cp.Spec.SessionId = "generated-session"
	}
	f.tasks[name] = cp
	return proto.Clone(cp).(*v1alpha1.Task), nil
}

func (f *fakeAXServer) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	f.resumed = append(f.resumed, req.GetName())
	return proto.Clone(t).(*v1alpha1.Task), nil
}

func (f *fakeAXServer) SuspendTask(_ context.Context, req *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	f.suspended = append(f.suspended, req.GetName())
	return proto.Clone(t).(*v1alpha1.Task), nil
}

func (f *fakeAXServer) GetWorkspace(_ context.Context, req *v1alpha1.GetWorkspaceRequest) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.workspaces[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return proto.Clone(w).(*v1alpha1.Workspace), nil
}

func (f *fakeAXServer) ListWorkspaces(_ context.Context, _ *v1alpha1.ListWorkspacesRequest) (*v1alpha1.ListWorkspacesResponse, error) {
	return &v1alpha1.ListWorkspacesResponse{}, nil
}

func (f *fakeAXServer) CreateWorkspace(_ context.Context, req *v1alpha1.CreateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetWorkspace().GetMetadata().GetName()
	if existing, exists := f.workspaces[name]; exists {
		if proto.Equal(existing.GetSpec(), req.GetWorkspace().GetSpec()) {
			return proto.Clone(existing).(*v1alpha1.Workspace), nil
		}
		return nil, status.Error(codes.FailedPrecondition, "workspace is immutable")
	}
	cp := proto.Clone(req.GetWorkspace()).(*v1alpha1.Workspace)
	f.workspaces[name] = cp
	return proto.Clone(cp).(*v1alpha1.Workspace), nil
}

func (f *fakeAXServer) GetGateway(_ context.Context, req *v1alpha1.GetGatewayRequest) (*v1alpha1.Gateway, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.gateways[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return proto.Clone(g).(*v1alpha1.Gateway), nil
}

func (f *fakeAXServer) CreateGateway(_ context.Context, req *v1alpha1.CreateGatewayRequest) (*v1alpha1.Gateway, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetGateway().GetMetadata().GetName()
	if _, exists := f.gateways[name]; exists {
		return nil, status.Error(codes.AlreadyExists, "gateway already exists")
	}
	cp := proto.Clone(req.GetGateway()).(*v1alpha1.Gateway)
	f.gateways[name] = cp
	return proto.Clone(cp).(*v1alpha1.Gateway), nil
}

func (f *fakeAXServer) UpdateGateway(_ context.Context, req *v1alpha1.UpdateGatewayRequest) (*v1alpha1.Gateway, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetGateway().GetMetadata().GetName()
	cp := proto.Clone(req.GetGateway()).(*v1alpha1.Gateway)
	f.gateways[name] = cp
	return proto.Clone(cp).(*v1alpha1.Gateway), nil
}

func TestPreflightAndPromoteEndToEnd(t *testing.T) {
	srcSrv := newFakeAXServer()
	dstSrv := newFakeAXServer()

	srcSrv.gateways["agent-gw"] = &v1alpha1.Gateway{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindGateway,
		Metadata:   &v1alpha1.ObjectMeta{Name: "agent-gw", Atespace: "default"},
		Spec: &v1alpha1.GatewaySpec{
			Egress: []*ateapipb.EgressRule{
				{
					Https: &ateapipb.HttpsRule{Hostnames: []string{"github.com"}},
				},
			},
		},
	}
	srcSrv.workspaces["repo-ws"] = &v1alpha1.Workspace{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindWorkspace,
		Metadata:   &v1alpha1.ObjectMeta{Name: "repo-ws", Atespace: "default"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Git: []*v1alpha1.GitRepo{
					{Name: "origin", Repo: "https://github.com/pallets/itsdangerous.git", Ref: "main", Dir: "."},
				},
			},
		},
	}
	srcSrv.tasks["fix-bug-1"] = &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   &v1alpha1.ObjectMeta{Name: "fix-bug-1", Atespace: "default", Uid: "uid-parent-123"},
		Spec: &v1alpha1.TaskSpec{
			Commands:  []string{"pytest"},
			Workspace: &v1alpha1.WorkspaceRef{Name: "repo-ws"},
			Gateway:   &v1alpha1.GatewayRef{Name: "agent-gw"},
			Goal:      &v1alpha1.Goal{Spec: "Fix rate limiter"},
			SessionId: "sess-shared-42",
			Debug:     true,
		},
		Status: &v1alpha1.TaskStatus{
			State: v1alpha1.TaskState_TASK_STATE_WORKING,
			Actor: "actor-1",
		},
	}

	dstSrv.workspaces["repo-ws"] = proto.Clone(srcSrv.workspaces["repo-ws"]).(*v1alpha1.Workspace)

	srcURL := srcSrv.start(t)
	dstURL := dstSrv.start(t)
	resolver := func(opts kube.TunnelOptions) (string, error) {
		if opts.Context == "local" {
			return srcURL, nil
		}
		return dstURL, nil
	}

	var pushedCommands [][]string
	guestExec := func(_ context.Context, _, _, _, _ string, cmd []string, _ string) (string, string, int, error) {
		pushedCommands = append(pushedCommands, cmd)
		joined := strings.Join(cmd, " ")
		switch {
		case strings.Contains(joined, "rev-parse --show-toplevel"):
			return "/workspace\n", "", 0, nil
		case strings.Contains(joined, "status --porcelain"):
			return " M src/limiter.py\n?? .ax/\n?? tests/test_limiter.py\n", "", 0, nil
		case strings.Contains(joined, "rev-parse --short HEAD"):
			return "abc1234\n", "", 0, nil
		default:
			return "", "", 0, nil
		}
	}

	report, err := Preflight(context.Background(), PreflightOptions{
		SourceContext: "local",
		TargetContext: "team-gke",
		TaskName:      "fix-bug-1",
		ResolveServer: resolver,
		GuestExec:     guestExec,
	})
	if err != nil {
		t.Fatalf("Preflight failed: %v", err)
	}
	if !report.CanPromote {
		t.Fatalf("expected CanPromote=true, got false: %+v", report.Checks)
	}
	if len(report.DirtyFiles) != 2 {
		t.Fatalf("expected 2 dirty files, got %v", report.DirtyFiles)
	}
	if report.SuggestedBranch != "ax/fix-bug-1" {
		t.Fatalf("unexpected SuggestedBranch %q", report.SuggestedBranch)
	}

	pushRes, err := PushSandboxBranch(context.Background(), PushBranchOptions{
		SourceContext: "local",
		TaskName:      "fix-bug-1",
		ResolveServer: resolver,
		GuestExec:     guestExec,
	})
	if err != nil {
		t.Fatalf("PushSandboxBranch failed: %v", err)
	}
	if pushRes.Branch != "ax/fix-bug-1" || pushRes.CommitSHA != "abc1234" {
		t.Fatalf("unexpected PushSandboxBranch result: %+v", pushRes)
	}

	promRes, err := Promote(context.Background(), PromoteOptions{
		SourceContext:  "local",
		TargetContext:  "team-gke",
		TaskName:       "fix-bug-1",
		BranchOverride: pushRes.Branch,
		HandoffNote:    "Run full integration suite on GKE",
		SuspendSource:  true,
		ResolveServer:  resolver,
	})
	if err != nil {
		t.Fatalf("Promote failed: %v", err)
	}
	if promRes.SessionID != "sess-shared-42" {
		t.Fatalf("expected shared sessionId sess-shared-42, got %q", promRes.SessionID)
	}
	if promRes.WorkspaceName == "repo-ws" {
		t.Fatalf("expected immutable workspace collision with branch override to derive suffixed name, got %q", promRes.WorkspaceName)
	}
	if len(srcSrv.suspended) != 1 || srcSrv.suspended[0] != "fix-bug-1" {
		t.Fatalf("expected source task to be suspended, got %v", srcSrv.suspended)
	}
	if len(dstSrv.resumed) != 1 {
		t.Fatalf("expected promoted task to be resumed on target, got %v", dstSrv.resumed)
	}
	promotedTask := dstSrv.tasks[promRes.TaskName]
	if !strings.Contains(promotedTask.GetSpec().GetGoal().GetSpec(), "Run full integration suite on GKE") {
		t.Fatalf("expected handoff note in goal, got %q", promotedTask.GetSpec().GetGoal().GetSpec())
	}
}
