package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------- Daemon registration sets owner_id ----------

func TestDaemonRegisterSetsOwnerID(t *testing.T) {
	ctx := context.Background()

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/daemon/register", DaemonRegisterRequest{
		WorkspaceID: testWorkspaceID,
		DaemonID:    "owner-test-daemon",
		DeviceName:  "test-machine",
		Runtimes: []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Version string `json:"version"`
			Status  string `json:"status"`
		}{
			{Name: "Claude", Type: "claude", Version: "2.0.0", Status: "online"},
		},
	})
	testHandler.DaemonRegister(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DaemonRegister: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Runtimes []AgentRuntimeResponse `json:"runtimes"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Runtimes) == 0 {
		t.Fatal("expected at least 1 runtime")
	}

	runtimeID := resp.Runtimes[0].ID

	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	// Verify owner_id was set to the authenticated user
	if resp.Runtimes[0].OwnerID == nil {
		t.Fatal("expected owner_id to be set")
	}
	if *resp.Runtimes[0].OwnerID != testUserID {
		t.Fatalf("expected owner_id=%s, got %s", testUserID, *resp.Runtimes[0].OwnerID)
	}
}

// ---------- Runtime visibility in API responses ----------

func TestRuntimeResponseIncludesVisibility(t *testing.T) {
	ctx := context.Background()

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/daemon/register", DaemonRegisterRequest{
		WorkspaceID: testWorkspaceID,
		DaemonID:    "vis-test-daemon",
		DeviceName:  "test-machine",
		Runtimes: []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Version string `json:"version"`
			Status  string `json:"status"`
		}{
			{Name: "Claude", Type: "claude", Version: "2.0.0", Status: "online"},
		},
	})
	testHandler.DaemonRegister(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DaemonRegister: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Runtimes []AgentRuntimeResponse `json:"runtimes"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	runtimeID := resp.Runtimes[0].ID

	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	// Should default to "workspace"
	if resp.Runtimes[0].Visibility != "workspace" {
		t.Fatalf("expected default visibility 'workspace', got '%s'", resp.Runtimes[0].Visibility)
	}
}

// ---------- ListAgentRuntimes filters private runtimes for non-owners ----------

func TestListRuntimesFiltersPrivate(t *testing.T) {
	ctx := context.Background()

	// Create a second user
	var user2ID string
	err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('User2', 'runtime-vis-user2@test.ai') RETURNING id`).Scan(&user2ID)
	if err != nil {
		t.Fatalf("create user2: %v", err)
	}
	_, err = testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, user2ID)
	if err != nil {
		t.Fatalf("add member: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, user2ID)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, user2ID)
	})

	// Create a private runtime owned by testUser
	var privateRuntimeID string
	err = testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, visibility)
		VALUES ($1, 'private-daemon', 'Private Runtime', 'local', 'claude', 'online', '', '{}'::jsonb, $2, 'private')
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&privateRuntimeID)
	if err != nil {
		t.Fatalf("create private runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, privateRuntimeID)
	})

	// ListAgentRuntimes as user2 should NOT see the private runtime
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/runtimes?workspace_id="+testWorkspaceID, nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", user2ID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	testHandler.ListAgentRuntimes(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ListAgentRuntimes: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var runtimes []AgentRuntimeResponse
	json.NewDecoder(w.Body).Decode(&runtimes)
	for _, rt := range runtimes {
		if rt.ID == privateRuntimeID {
			t.Fatalf("user2 should NOT see private runtime owned by testUser")
		}
	}

	// ListAgentRuntimes as the owner SHOULD see it
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/runtimes?workspace_id="+testWorkspaceID, nil)
	testHandler.ListAgentRuntimes(w, req)

	var ownerRuntimes []AgentRuntimeResponse
	json.NewDecoder(w.Body).Decode(&ownerRuntimes)
	found := false
	for _, rt := range ownerRuntimes {
		if rt.ID == privateRuntimeID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("owner should see their own private runtime")
	}
}

// ---------- Agent creation restricted to own runtimes ----------

func TestCreateAgentOnOwnRuntimeSucceeds(t *testing.T) {
	ctx := context.Background()

	// Create a runtime owned by testUser
	var runtimeID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, visibility)
		VALUES ($1, 'own-rt-daemon', 'Own Runtime', 'local', 'claude', 'online', '', '{}'::jsonb, $2, 'workspace')
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&runtimeID)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent WHERE runtime_id = $1`, runtimeID)
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/agents?workspace_id="+testWorkspaceID, map[string]any{
		"name":       "Own Runtime Agent",
		"runtime_id": runtimeID,
	})
	testHandler.CreateAgent(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateAgent on own runtime: expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateAgentOnOtherUserRuntimeFails(t *testing.T) {
	ctx := context.Background()

	// Create a second user
	var user2ID string
	err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('OtherOwner', 'other-rt-owner@test.ai') RETURNING id`).Scan(&user2ID)
	if err != nil {
		t.Fatalf("create user2: %v", err)
	}
	_, err = testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, user2ID)
	if err != nil {
		t.Fatalf("add member: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, user2ID)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, user2ID)
	})

	// Create a runtime owned by user2
	var runtimeID string
	err = testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, visibility)
		VALUES ($1, 'other-rt-daemon', 'Other Runtime', 'local', 'claude', 'online', '', '{}'::jsonb, $2, 'workspace')
		RETURNING id
	`, testWorkspaceID, user2ID).Scan(&runtimeID)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent WHERE runtime_id = $1`, runtimeID)
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	// user2 (regular member) tries to create agent on testUser's runtime — should fail
	// (testUser is workspace owner, so we need to test FROM user2's perspective)
	// Swap: runtime is owned by testUser, user2 is the one creating
	// Actually we need the reverse: user2 owns the runtime, and a regular member tries to use it.
	// Let's create user3 as a regular member and have them try to create on user2's runtime.
	var user3ID string
	err = testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('User3', 'other-rt-user3@test.ai') RETURNING id`).Scan(&user3ID)
	if err != nil {
		t.Fatalf("create user3: %v", err)
	}
	_, err = testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, user3ID)
	if err != nil {
		t.Fatalf("add user3 member: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, user3ID)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, user3ID)
	})

	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]any{
		"name":       "Unauthorized Agent",
		"runtime_id": runtimeID,
	})
	req := httptest.NewRequest("POST", "/api/agents?workspace_id="+testWorkspaceID, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", user3ID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	testHandler.CreateAgent(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("CreateAgent on other's runtime: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Admin can create agent on any runtime ----------

func TestAdminCanCreateAgentOnAnyRuntime(t *testing.T) {
	ctx := context.Background()

	// Create a second user with admin role
	var adminID string
	err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('AdminUser', 'admin-rt-test@test.ai') RETURNING id`).Scan(&adminID)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	_, err = testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'admin')`, testWorkspaceID, adminID)
	if err != nil {
		t.Fatalf("add admin member: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, adminID)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, adminID)
	})

	// Runtime owned by testUser
	var runtimeID string
	err = testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, visibility)
		VALUES ($1, 'admin-rt-daemon', 'Admin Test Runtime', 'local', 'claude', 'online', '', '{}'::jsonb, $2, 'workspace')
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&runtimeID)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent WHERE runtime_id = $1`, runtimeID)
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	// Admin creates agent on testUser's runtime — should succeed
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]any{
		"name":       "Admin Created Agent",
		"runtime_id": runtimeID,
	})
	req := httptest.NewRequest("POST", "/api/agents?workspace_id="+testWorkspaceID, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", adminID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	testHandler.CreateAgent(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("Admin CreateAgent on other's runtime: expected 201, got %d: %s", w.Code, w.Body.String())
	}
}
