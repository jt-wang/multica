package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestRegistrationModeOpen verifies that in "open" mode (default),
// any email can register and gets a workspace.
func TestRegistrationModeOpen(t *testing.T) {
	const email = "reg-open-test@example.com"
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM verification_code WHERE email = $1`, email)
		user, err := testHandler.Queries.GetUserByEmail(ctx, email)
		if err == nil {
			ws, _ := testHandler.Queries.ListWorkspaces(ctx, user.ID)
			for _, w := range ws {
				_ = testHandler.Queries.DeleteWorkspace(ctx, w.ID)
			}
		}
		testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)
	})

	os.Setenv("MULTICA_REGISTRATION_MODE", "open")
	defer os.Unsetenv("MULTICA_REGISTRATION_MODE")

	// Send code should succeed
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/auth/send-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.SendCode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("SendCode in open mode: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify code should succeed and create user + workspace
	dbCode, err := testHandler.Queries.GetLatestVerificationCode(ctx, email)
	if err != nil {
		t.Fatalf("GetLatestVerificationCode: %v", err)
	}

	w = httptest.NewRecorder()
	buf.Reset()
	json.NewEncoder(&buf).Encode(map[string]string{"email": email, "code": dbCode.Code})
	req = httptest.NewRequest("POST", "/auth/verify-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.VerifyCode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("VerifyCode in open mode: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegistrationModeInviteOnly verifies that in "invite_only" mode,
// only users who are already members of a workspace can log in.
// New users (not pre-added) get rejected.
func TestRegistrationModeInviteOnly(t *testing.T) {
	const email = "reg-invite-new@example.com"
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM verification_code WHERE email = $1`, email)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)
	})

	os.Setenv("MULTICA_REGISTRATION_MODE", "invite_only")
	defer os.Unsetenv("MULTICA_REGISTRATION_MODE")

	// Send code itself should be rejected — no email wasted
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/auth/send-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.SendCode(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("SendCode invite_only (new user): expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegistrationModeInviteOnlyAllowsExistingMember verifies that
// a user who has been pre-added as a member CAN log in with invite_only mode.
func TestRegistrationModeInviteOnlyAllowsExistingMember(t *testing.T) {
	const email = "reg-invite-member@example.com"
	ctx := context.Background()

	// Pre-create user and add them to a workspace
	var userID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email) VALUES ('Invited Member', $1) RETURNING id
	`, email).Scan(&userID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Add as member to existing test workspace
	_, err = testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')
	`, testWorkspaceID, userID)
	if err != nil {
		t.Fatalf("add member: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM verification_code WHERE email = $1`, email)
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, userID)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	os.Setenv("MULTICA_REGISTRATION_MODE", "invite_only")
	defer os.Unsetenv("MULTICA_REGISTRATION_MODE")

	// Send code
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/auth/send-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.SendCode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("SendCode: expected 200, got %d", w.Code)
	}

	dbCode, err := testHandler.Queries.GetLatestVerificationCode(ctx, email)
	if err != nil {
		t.Fatalf("GetLatestVerificationCode: %v", err)
	}

	// VerifyCode should succeed — user is already a member
	w = httptest.NewRecorder()
	buf.Reset()
	json.NewEncoder(&buf).Encode(map[string]string{"email": email, "code": dbCode.Code})
	req = httptest.NewRequest("POST", "/auth/verify-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.VerifyCode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("VerifyCode invite_only (existing member): expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegistrationModeClosed verifies that in "closed" mode,
// no one can register, even existing members.
func TestRegistrationModeClosed(t *testing.T) {
	const email = "reg-closed-test@example.com"
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM verification_code WHERE email = $1`, email)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)
	})

	os.Setenv("MULTICA_REGISTRATION_MODE", "closed")
	defer os.Unsetenv("MULTICA_REGISTRATION_MODE")

	// Send code itself should be blocked in closed mode
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/auth/send-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.SendCode(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("SendCode in closed mode: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAllowedDomainsAccepted verifies that emails from allowed domains work.
func TestAllowedDomainsAccepted(t *testing.T) {
	const email = "reg-domain-ok@allowed.com"
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM verification_code WHERE email = $1`, email)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)
	})

	os.Setenv("MULTICA_ALLOWED_DOMAINS", "allowed.com,another.com")
	defer os.Unsetenv("MULTICA_ALLOWED_DOMAINS")

	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/auth/send-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.SendCode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("SendCode with allowed domain: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAllowedDomainsRejected verifies that emails from non-allowed domains get rejected.
func TestAllowedDomainsRejected(t *testing.T) {
	const email = "reg-domain-bad@blocked.com"
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM verification_code WHERE email = $1`, email)
	})

	os.Setenv("MULTICA_ALLOWED_DOMAINS", "allowed.com,another.com")
	defer os.Unsetenv("MULTICA_ALLOWED_DOMAINS")

	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/auth/send-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.SendCode(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("SendCode with blocked domain: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAllowedDomainsEmptyMeansAll verifies that empty MULTICA_ALLOWED_DOMAINS
// means all domains are allowed (default behavior).
func TestAllowedDomainsEmptyMeansAll(t *testing.T) {
	const email = "reg-domain-any@whatever.com"
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM verification_code WHERE email = $1`, email)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)
	})

	os.Setenv("MULTICA_ALLOWED_DOMAINS", "")
	defer os.Unsetenv("MULTICA_ALLOWED_DOMAINS")

	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/auth/send-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.SendCode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("SendCode with empty allowed domains: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegistrationModeInviteOnlyNoAutoWorkspace verifies that in invite_only
// mode, a user who logs in (as existing member) does NOT get an auto-created workspace.
func TestRegistrationModeInviteOnlyNoAutoWorkspace(t *testing.T) {
	const email = "reg-invite-noauto@example.com"
	ctx := context.Background()

	// Pre-create user and add to workspace
	var userID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email) VALUES ('NoAuto Member', $1) RETURNING id
	`, email).Scan(&userID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err = testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')
	`, testWorkspaceID, userID)
	if err != nil {
		t.Fatalf("add member: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM verification_code WHERE email = $1`, email)
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, userID)
		// Remove any auto-created workspaces
		ws, _ := testHandler.Queries.ListWorkspaces(ctx, parseUUID(userID))
		for _, w := range ws {
			if w.ID != parseUUID(testWorkspaceID) {
				_ = testHandler.Queries.DeleteWorkspace(ctx, w.ID)
			}
		}
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	os.Setenv("MULTICA_REGISTRATION_MODE", "invite_only")
	defer os.Unsetenv("MULTICA_REGISTRATION_MODE")

	// Send + verify code
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/auth/send-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.SendCode(w, req)

	dbCode, _ := testHandler.Queries.GetLatestVerificationCode(ctx, email)

	w = httptest.NewRecorder()
	buf.Reset()
	json.NewEncoder(&buf).Encode(map[string]string{"email": email, "code": dbCode.Code})
	req = httptest.NewRequest("POST", "/auth/verify-code", &buf)
	req.Header.Set("Content-Type", "application/json")
	testHandler.VerifyCode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("VerifyCode: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should NOT have created an additional workspace
	workspaces, err := testHandler.Queries.ListWorkspaces(ctx, parseUUID(userID))
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	for _, ws := range workspaces {
		if ws.ID != parseUUID(testWorkspaceID) {
			t.Fatalf("invite_only mode should not auto-create workspaces, but found extra workspace: %s", ws.Name)
		}
	}
}
