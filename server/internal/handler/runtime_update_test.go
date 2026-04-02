package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------- Update runtime visibility ----------

func TestUpdateRuntimeVisibility(t *testing.T) {
	ctx := context.Background()

	// Create a runtime owned by testUser
	var runtimeID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, visibility)
		VALUES ($1, 'update-vis-daemon', 'Update Vis Runtime', 'local', 'claude', 'online', '', '{}'::jsonb, $2, 'workspace')
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&runtimeID)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	// Update to private
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"visibility": "private"})
	req := httptest.NewRequest("PATCH", "/api/runtimes/"+runtimeID, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.UpdateAgentRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgentRuntime: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp AgentRuntimeResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Visibility != "private" {
		t.Fatalf("expected visibility 'private', got '%s'", resp.Visibility)
	}

	// Update back to workspace
	w = httptest.NewRecorder()
	buf.Reset()
	json.NewEncoder(&buf).Encode(map[string]string{"visibility": "workspace"})
	req = httptest.NewRequest("PATCH", "/api/runtimes/"+runtimeID, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.UpdateAgentRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgentRuntime back to workspace: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Visibility != "workspace" {
		t.Fatalf("expected visibility 'workspace', got '%s'", resp.Visibility)
	}
}

func TestUpdateRuntimeVisibilityInvalidValue(t *testing.T) {
	ctx := context.Background()

	var runtimeID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'update-bad-daemon', 'Bad Vis Runtime', 'local', 'claude', 'online', '', '{}'::jsonb, $2)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&runtimeID)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"visibility": "invalid"})
	req := httptest.NewRequest("PATCH", "/api/runtimes/"+runtimeID, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.UpdateAgentRuntime(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("UpdateAgentRuntime with invalid visibility: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateRuntimeByNonOwnerFails(t *testing.T) {
	ctx := context.Background()

	// Create user2
	var user2ID string
	err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('NonOwner', 'rt-nonowner@test.ai') RETURNING id`).Scan(&user2ID)
	if err != nil {
		t.Fatalf("create user2: %v", err)
	}
	_, _ = testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, user2ID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, user2ID)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, user2ID)
	})

	// Runtime owned by testUser
	var runtimeID string
	err = testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'update-nonowner-daemon', 'NonOwner Runtime', 'local', 'claude', 'online', '', '{}'::jsonb, $2)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&runtimeID)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	// user2 tries to update — should fail
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"visibility": "private"})
	req := httptest.NewRequest("PATCH", "/api/runtimes/"+runtimeID, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", user2ID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.UpdateAgentRuntime(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("UpdateAgentRuntime by non-owner: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Update agent approval_required ----------

func TestUpdateAgentApprovalRequired(t *testing.T) {
	ctx := context.Background()

	// Create runtime + agent
	var runtimeID, agentID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'update-appr-daemon', 'Appr Runtime', 'local', 'claude', 'online', '', '{}'::jsonb, $2) RETURNING id
	`, testWorkspaceID, testUserID).Scan(&runtimeID)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}

	err = testPool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id, tools, triggers, approval_required)
		VALUES ($1, 'Appr Update Agent', 'local', '{}'::jsonb, $2, 'workspace', 1, $3, '[]'::jsonb, '[]'::jsonb, false) RETURNING id
	`, testWorkspaceID, runtimeID, testUserID).Scan(&agentID)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, agentID)
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	// Verify initial state
	w := httptest.NewRecorder()
	req := newRequest("GET", "/api/agents/"+agentID, nil)
	req = withURLParam(req, "id", agentID)
	testHandler.GetAgent(w, req)
	var initialAgent AgentResponse
	json.NewDecoder(w.Body).Decode(&initialAgent)
	if initialAgent.ApprovalRequired != false {
		t.Fatalf("expected initial approval_required=false, got %v", initialAgent.ApprovalRequired)
	}

	// Update to true
	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/agents/"+agentID, map[string]any{
		"approval_required": true,
	})
	req = withURLParam(req, "id", agentID)
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgent: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var updated AgentResponse
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.ApprovalRequired != true {
		t.Fatalf("expected approval_required=true after update, got %v", updated.ApprovalRequired)
	}
}
