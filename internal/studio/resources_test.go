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

package studio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *taskStubServer) ListGateways(_ context.Context, req *v1alpha1.ListGatewaysRequest) (*v1alpha1.ListGatewaysResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []*v1alpha1.Gateway
	for _, g := range s.gateways {
		if g.GetMetadata().GetAtespace() == req.GetAtespace() {
			all = append(all, g)
		}
	}
	page, next, err := stubPage(s, all, req.GetPageSize(), req.GetPageToken())
	return &v1alpha1.ListGatewaysResponse{Gateways: page, NextPageToken: next}, err
}

func (s *taskStubServer) DeleteGateway(_ context.Context, req *v1alpha1.DeleteGatewayRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := req.GetAtespace() + "/" + req.GetName()
	if s.gateways[k] == nil {
		return nil, status.Errorf(codes.NotFound, "gateway %s not found", k)
	}
	delete(s.gateways, k)
	return &emptypb.Empty{}, nil
}

func (s *taskStubServer) ListWorkspaces(_ context.Context, req *v1alpha1.ListWorkspacesRequest) (*v1alpha1.ListWorkspacesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []*v1alpha1.Workspace
	for _, w := range s.workspaces {
		if w.GetMetadata().GetAtespace() == req.GetAtespace() {
			all = append(all, w)
		}
	}
	page, next, err := stubPage(s, all, req.GetPageSize(), req.GetPageToken())
	return &v1alpha1.ListWorkspacesResponse{Workspaces: page, NextPageToken: next}, err
}

func (s *taskStubServer) DeleteWorkspace(_ context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := req.GetAtespace() + "/" + req.GetName()
	if s.workspaces[k] == nil {
		return nil, status.Errorf(codes.NotFound, "workspace %s not found", k)
	}
	delete(s.workspaces, k)
	return &emptypb.Empty{}, nil
}

func (s *taskStubServer) ListModels(_ context.Context, req *v1alpha1.ListModelsRequest) (*v1alpha1.ListModelsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := append([]*v1alpha1.Model(nil), s.models...)
	if s.models == nil {
		all = []*v1alpha1.Model{{Kind: "Model", Metadata: &v1alpha1.ObjectMeta{Name: "gemini-flash", Atespace: req.GetAtespace()},
			Spec: &v1alpha1.ModelSpec{Provider: "gemini", Model: "gemini-3.8-flash", SecretKey: &v1alpha1.SecretKeyRef{Name: "gemini-api-creds", Key: "GEMINI_API_KEY"}}}}
	}
	page, next, err := stubPage(s, all, req.GetPageSize(), req.GetPageToken())
	return &v1alpha1.ListModelsResponse{Models: page, NextPageToken: next}, err
}

// A list is every page: ax-server returns at most 1000 resources at a time,
// and 50 unless asked for more.
func TestListsFollowPageTokens(t *testing.T) {
	h, cookie, _, stub := newTaskTestServer(t)
	stub.mu.Lock()
	stub.maxPage = 2
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		meta := &v1alpha1.ObjectMeta{Name: name, Atespace: "default"}
		stub.tasks["default/"+name] = &v1alpha1.Task{Kind: "Task", Metadata: meta}
		stub.workspaces["default/"+name] = &v1alpha1.Workspace{Kind: "Workspace", Metadata: meta}
		stub.gateways["default/"+name] = &v1alpha1.Gateway{Kind: "Gateway", Metadata: meta}
		stub.models = append(stub.models, &v1alpha1.Model{Kind: "Model", Metadata: meta})
	}
	stub.mu.Unlock()

	// Each kind has the five added here; all but models also have the one the
	// test server starts with.
	for kind, want := range map[string]int{"tasks": 6, "workspaces": 6, "gateways": 6, "models": 5} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/"+kind+"?context=ax-local&atespace=default", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var got map[string][]struct{ Metadata struct{ Name string } }
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", kind, rec.Code, rec.Body.String())
		}
		names := map[string]bool{}
		for _, x := range got[kind] {
			names[x.Metadata.Name] = true
		}
		if len(got[kind]) != want || len(names) != want {
			t.Errorf("%s lists %d resources (%d distinct), want %d: %s", kind, len(got[kind]), len(names), want, rec.Body.String())
		}
		if stub.pageSize != listPageSize {
			t.Errorf("%s are listed with page size %d, want %d", kind, stub.pageSize, listPageSize)
		}
	}
}

func TestModelsAreListed(t *testing.T) {
	h, cookie, _, _ := newTaskTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/models?context=ax-local&atespace=default", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var got struct {
		Models []struct {
			Metadata struct{ Name string }
			Spec     struct{ Provider, Model string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || len(got.Models) != 1 ||
		got.Models[0].Metadata.Name != "gemini-flash" || got.Models[0].Spec.Model != "gemini-3.8-flash" {
		t.Fatalf("models returned %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkspacesAndGatewaysAreListedAndDeleted(t *testing.T) {
	h, cookie, csrf, stub := newTaskTestServer(t)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		req.AddCookie(cookie)
		if method == http.MethodPost {
			req.Header.Set(csrfHeaderName, csrf)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	var ws struct {
		Workspaces []struct {
			Metadata struct{ Name string }
			Status   struct{ Phase, LastSetupError string }
		}
	}
	rec := call(http.MethodGet, "/api/workspaces?context=ax-local&atespace=default", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &ws); err != nil || rec.Code != http.StatusOK || len(ws.Workspaces) != 1 {
		t.Fatalf("workspaces returned %d: %s", rec.Code, rec.Body.String())
	}
	if w := ws.Workspaces[0]; w.Metadata.Name != "ws-fix" || w.Status.Phase != "Failed" || w.Status.LastSetupError != "git clone: repository not found" {
		t.Errorf("unexpected workspace: %+v", w)
	}
	var gws struct{ Gateways []json.RawMessage }
	rec = call(http.MethodGet, "/api/gateways?context=ax-local&atespace=default", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &gws); err != nil || len(gws.Gateways) != 1 || !strings.Contains(string(gws.Gateways[0]), `"gw-fix"`) {
		t.Fatalf("gateways returned %d: %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/workspaces/delete", `{"context":"ax-local","atespace":"default","name":"Not_A_Name"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad name is refused, got %d", rec.Code)
	}
	if rec := call(http.MethodPost, "/api/workspaces/delete", `{"context":"ax-local","atespace":"default","name":"ws-fix"}`); rec.Code != http.StatusOK || len(stub.workspaces) != 0 {
		t.Errorf("delete workspace returned %d: %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/gateways/delete", `{"context":"ax-local","atespace":"default","name":"gw-fix"}`); rec.Code != http.StatusOK || len(stub.gateways) != 0 {
		t.Errorf("delete gateway returned %d: %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/gateways/delete", `{"context":"ax-local","atespace":"default","name":"gw-fix"}`); rec.Code != http.StatusNotFound {
		t.Errorf("deleting it again says it's gone, got %d", rec.Code)
	}
}
