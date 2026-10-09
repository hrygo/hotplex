package session

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/security"
)

// --- users ---

func TestUsersStore_CreateAndGet(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	u := &security.User{ID: "u-1", Username: "alice", PasswordHash: "$2a$12$fake", Role: "admin", Status: "active"}
	require.NoError(t, store.CreateUser(ctx, u, 1700000000))

	got, err := store.GetUserByUsername(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, "u-1", got.ID)
	require.Equal(t, "admin", got.Role)

	got2, err := store.GetUserByID(ctx, "u-1")
	require.NoError(t, err)
	require.Equal(t, "alice", got2.Username)
}

func TestUsersStore_GetByUsername_NotFound(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	_, err := store.GetUserByUsername(context.Background(), "nobody")
	require.ErrorIs(t, err, security.ErrUserNotFound)
}

func TestUsersStore_ListAndUpdateStatus(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-2", Username: "bob", Role: "user", Status: "active"}, 1700000000))

	list, err := store.ListUsers(ctx, 100, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)

	require.NoError(t, store.UpdateUserStatus(ctx, "u-2", "disabled", 1800000000))
	disabled, _ := store.GetUserByID(ctx, "u-2")
	require.Equal(t, "disabled", disabled.Status)

	require.NoError(t, store.UpdateUserPassword(ctx, "u-2", "new_hash_123", 1850000000))
	updatedUser, _ := store.GetUserByID(ctx, "u-2")
	require.Equal(t, "new_hash_123", updatedUser.PasswordHash)
}

func TestUsersStore_ListByIDs(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()

	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", DisplayName: "Alice Lee", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-2", Username: "bob", DisplayName: "Bob", Role: "user", Status: "active"}, 1700000000))

	cases := []struct {
		name    string
		ids     []string
		wantIDs []string
	}{
		{name: "empty input", ids: nil, wantIDs: nil},
		{name: "all found", ids: []string{"u-1", "u-2"}, wantIDs: []string{"u-1", "u-2"}},
		{name: "partial found", ids: []string{"u-1", "missing"}, wantIDs: []string{"u-1"}},
		{name: "dedupes duplicates", ids: []string{"u-1", "u-1", "u-2"}, wantIDs: []string{"u-1", "u-2"}},
		{name: "filters empty", ids: []string{"", "u-2"}, wantIDs: []string{"u-2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := store.ListByIDs(ctx, tc.ids)
			require.NoError(t, err)
			require.Len(t, got, len(tc.wantIDs))
			for _, id := range tc.wantIDs {
				require.Contains(t, got, id)
			}
		})
	}

	// Display name round-trips so admin views can render it.
	got, err := store.ListByIDs(ctx, []string{"u-1"})
	require.NoError(t, err)
	require.Equal(t, "Alice Lee", got["u-1"].DisplayName)
}

func TestUsersStore_HasAdmin(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()

	// 空库:无 admin
	got, err := store.HasAdmin(ctx)
	require.NoError(t, err)
	require.False(t, got)

	// 非 admin 用户不计入
	require.NoError(t, store.CreateUser(ctx, &security.User{
		ID: "u-1", Username: "alice", PasswordHash: "$2a$12$fake", Role: "user", Status: "active",
	}, 1700000000))
	got, err = store.HasAdmin(ctx)
	require.NoError(t, err)
	require.False(t, got)

	// 出现 admin → true
	require.NoError(t, store.CreateUser(ctx, &security.User{
		ID: "u-2", Username: "bob", PasswordHash: "$2a$12$fake", Role: "admin", Status: "active",
	}, 1700000000))
	got, err = store.HasAdmin(ctx)
	require.NoError(t, err)
	require.True(t, got)
}

// --- workspaces ---

func TestWorkspacesStore_CreateUniqueConflict(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))

	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "proj", WorkDir: "/tmp/proj"}, 1700000000))
	// 同 owner 同 work_dir 冲突（UNIQUE(owner_user_id, work_dir)，spec §6.2 per-user 1:1）
	err := store.CreateWorkspace(ctx, &Workspace{ID: "ws-2", OwnerUserID: "u-1", Name: "proj2", WorkDir: "/tmp/proj"}, 1700000000)
	require.Error(t, err, "同 owner+work_dir 必须 1:1 唯一")
}

func TestWorkspacesStore_DifferentOwnersSameWorkDir(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-2", Username: "bob", Role: "user", Status: "active"}, 1700000000))
	// 不同 owner 同 work_dir 允许（协作场景，spec §2.2）
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "p", WorkDir: "/tmp/shared"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-2", OwnerUserID: "u-2", Name: "p", WorkDir: "/tmp/shared"}, 1700000000))
}

func TestWorkspacesStore_GetByOwnerAndWorkDir(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "p", WorkDir: "/tmp/x"}, 1700000000))

	got, err := store.GetWorkspaceByOwnerAndWorkDir(ctx, "u-1", "/tmp/x")
	require.NoError(t, err)
	require.Equal(t, "ws-1", got.ID)

	_, err = store.GetWorkspaceByOwnerAndWorkDir(ctx, "u-1", "/tmp/other")
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
}

func TestWorkspacesStore_ListByOwnerIsolated(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-2", Username: "bob", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "a", WorkDir: "/tmp/a"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-2", OwnerUserID: "u-1", Name: "b", WorkDir: "/tmp/b"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-3", OwnerUserID: "u-2", Name: "c", WorkDir: "/tmp/c"}, 1700000000))

	list, err := store.ListWorkspacesByOwner(ctx, "u-1", 100, 0)
	require.NoError(t, err)
	require.Len(t, list, 2, "只返回 owner=u-1 的 workspace")
	for _, w := range list {
		require.Equal(t, "u-1", w.OwnerUserID)
	}
}

func TestWorkspacesStore_ListAllWorkspaces(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-2", Username: "bob", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "a", WorkDir: "/tmp/a", AgentConfigOverrides: `{"SOUL.md":"x"}`}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-2", OwnerUserID: "u-2", Name: "b", WorkDir: "/tmp/b"}, 1700000000))

	all, err := store.ListAllWorkspaces(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2, "ListAllWorkspaces returns workspaces across all owners")
	ids := map[string]bool{}
	for _, w := range all {
		ids[w.ID] = true
		require.Equal(t, "active", w.Status)
	}
	require.True(t, ids["ws-1"])
	require.True(t, ids["ws-2"])
}

func TestWorkspacesStore_CountActiveSessions(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "p", WorkDir: "/tmp/x"}, 1700000000))

	n, err := store.CountActiveSessionsInWorkspace(ctx, "ws-1")
	require.NoError(t, err)
	require.Equal(t, 0, n, "新 workspace 无活跃会话")
}

func TestWorkspacesStore_DeleteIfEmpty_NoSessions(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "p", WorkDir: "/tmp/x"}, 1700000000))

	// 无活跃会话：原子删除成功。
	require.NoError(t, store.DeleteWorkspaceIfEmpty(ctx, "ws-1"))
	_, err := store.GetWorkspaceByID(ctx, "ws-1")
	require.ErrorIs(t, err, ErrWorkspaceNotFound, "workspace 应已删除")
}

func TestWorkspacesStore_DeleteIfEmpty_BlockedByActiveSession(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "p", WorkDir: "/tmp/x"}, 1700000000))

	// 插入一条活跃会话（state=running, workspace_id=ws-1）模拟 Count↔Delete 间的并发新建。
	_, err := store.db.ExecContext(ctx, queries["sessions.upsert_session"],
		"sess-1", "u-1", "u-1", "", "", "", "claude_code", "running", "webchat", "{}", "/tmp/x", "",
		int64(1700000000), int64(1700000000), int64(1800000000), int64(1800000000), "{}", "", "ck-1", "ws-1",
		"legacy", "", nil, nil, nil, nil, nil, nil, nil, "sess-1")
	require.NoError(t, err)

	// 有活跃会话：原子删除拒绝（防 TOCTOU 后 workspace_id 悬空）。
	err = store.DeleteWorkspaceIfEmpty(ctx, "ws-1")
	require.ErrorIs(t, err, ErrWorkspaceNotEmpty)
	// workspace 仍存在（未被删）。
	got, gerr := store.GetWorkspaceByID(ctx, "ws-1")
	require.NoError(t, gerr)
	require.Equal(t, "ws-1", got.ID)
}

func TestWorkspacesStore_DeleteIfEmpty_AlreadyDeletedReturnsNotFound(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "p", WorkDir: "/tmp/x"}, 1700000000))

	// 第一次删除成功。
	require.NoError(t, store.DeleteWorkspaceIfEmpty(ctx, "ws-1"))

	// 第二次：workspace 已不存在（模拟并发 actor 在 Get↔Delete 间已删除）。
	// 修复前 RowsAffected==0 一律返回 ErrWorkspaceNotEmpty（误导 409）；
	// 修复后重新检查存在性，返回 ErrWorkspaceNotFound（404）。
	err := store.DeleteWorkspaceIfEmpty(ctx, "ws-1")
	require.ErrorIs(t, err, ErrWorkspaceNotFound, "已删除的 workspace 应返回 ErrWorkspaceNotFound 而非 ErrWorkspaceNotEmpty")
}

// TestWorkspacesStore_PermissionModeRoundTrip verifies permission_mode persists across
// create → get → update → get (issue #789). Also exercises the scanWorkspace column-count
// contract: a mismatch between SELECT columns and scan destinations surfaces here.
func TestWorkspacesStore_PermissionModeRoundTrip(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))

	// Create with permission_mode = "workspace".
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{
		ID: "ws-1", OwnerUserID: "u-1", Name: "proj", WorkDir: "/tmp/p", PermissionMode: "workspace",
	}, 1700000000))

	got, err := store.GetWorkspaceByID(ctx, "ws-1")
	require.NoError(t, err)
	require.Equal(t, "workspace", got.PermissionMode, "permission_mode round-trips via create+get")

	// Update to "auto-edit" — CAS requires the cached updated_at from the prior Get.
	got.PermissionMode = "auto-edit"
	require.NoError(t, store.UpdateWorkspace(ctx, got, 1700000001))
	got2, err := store.GetWorkspaceByID(ctx, "ws-1")
	require.NoError(t, err)
	require.Equal(t, "auto-edit", got2.PermissionMode, "permission_mode round-trips via update")
	require.Equal(t, int64(1700000001), got2.UpdatedAt, "CAS success bumps updated_at")

	// Clear permission_mode: "" → nullableString → NULL.
	got2.PermissionMode = ""
	require.NoError(t, store.UpdateWorkspace(ctx, got2, 1700000002))
	got3, err := store.GetWorkspaceByID(ctx, "ws-1")
	require.NoError(t, err)
	require.Empty(t, got3.PermissionMode, "empty permission_mode persists as NULL and reads back empty")
}

// TestWorkspacesStore_PermissionModeDefaultNull verifies the backward-compatible default:
// a workspace created without permission_mode stores NULL and reads back empty (the "worker
// default" — the store does NOT normalize; each worker applies its own default). Existing
// rows upgraded by migration 022 behave identically — zero-damage upgrade (issue #789).
func TestWorkspacesStore_PermissionModeDefaultNull(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "u-1", Username: "alice", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateWorkspace(ctx, &Workspace{ID: "ws-1", OwnerUserID: "u-1", Name: "p", WorkDir: "/tmp/x"}, 1700000000))
	got, err := store.GetWorkspaceByID(ctx, "ws-1")
	require.NoError(t, err)
	require.Empty(t, got.PermissionMode, "NULL permission_mode reads back empty")
}

// --- invitations ---

func TestInvitationsStore_CreateAndMarkUsedCAS(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "admin-1", Username: "admin", Role: "admin", Status: "active"}, 1700000000))

	inv := &Invitation{ID: "inv-1", Code: "CODE123", CreatedBy: "admin-1", Role: "user", ExpiresAt: 1800000000}
	require.NoError(t, store.CreateInvitation(ctx, inv, 1700000000))

	got, err := store.GetInvitationByCode(ctx, "CODE123")
	require.NoError(t, err)
	require.Nil(t, got.UsedBy, "新建邀请 used_by 应为 NULL")

	// accept-invite 流程（spec §8.6）：先创建 user，再 mark invitation used。
	// used_by 的 FK 要求 user 先存在。
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "new-user-1", Username: "newbie", Role: "user", Status: "active"}, 1700000000))
	require.NoError(t, store.MarkInvitationUsed(ctx, "inv-1", "new-user-1", 1750000000))
	got2, _ := store.GetInvitationByCode(ctx, "CODE123")
	require.NotNil(t, got2.UsedBy)
	require.Equal(t, "new-user-1", *got2.UsedBy)

	// CAS：重复 mark 失败（防重放，spec §8.6）
	err = store.MarkInvitationUsed(ctx, "inv-1", "new-user-2", 1750000001)
	require.ErrorIs(t, err, ErrInvitationAlreadyUsed)
}

func TestInvitationsStore_GetByCode_NotFound(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	_, err := store.GetInvitationByCode(context.Background(), "NOPE")
	require.ErrorIs(t, err, ErrInvitationNotFound)
}

func TestInvitationsStore_List(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()
	require.NoError(t, store.CreateUser(ctx, &security.User{ID: "admin-1", Username: "admin", Role: "admin", Status: "active"}, 1700000000))
	require.NoError(t, store.CreateInvitation(ctx, &Invitation{ID: "inv-1", Code: "C1", CreatedBy: "admin-1", Role: "user", ExpiresAt: 1800000000}, 1700000000))
	require.NoError(t, store.CreateInvitation(ctx, &Invitation{ID: "inv-2", Code: "C2", CreatedBy: "admin-1", Role: "user", ExpiresAt: 1800000000}, 1700000001))

	list, err := store.ListInvitations(ctx, 100, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

// --- user identities (spec ④) ---

func TestIdentities_CreateAndGet(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()

	// Create a user first.
	u := &security.User{ID: "u-sso-1", Username: "keycloak:sub123", Role: "user", Status: "active"}
	require.NoError(t, store.CreateUser(ctx, u, 1700000000))

	// Create an identity.
	ident := &UserIdentity{
		ID:          "ident-1",
		UserID:      "u-sso-1",
		Provider:    "keycloak",
		Subject:     "sub123",
		DisplayName: "Alice",
		Email:       "alice@example.com",
	}
	require.NoError(t, store.CreateUserIdentity(ctx, ident, 1700000001))

	// Lookup by (provider, subject).
	got, err := store.GetUserIdentityByProviderSubject(ctx, "keycloak", "sub123")
	require.NoError(t, err)
	require.Equal(t, "ident-1", got.ID)
	require.Equal(t, "u-sso-1", got.UserID)
	require.Equal(t, "Alice", got.DisplayName)
}

func TestIdentities_GetByProviderSubject_NotFound(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	_, err := store.GetUserIdentityByProviderSubject(context.Background(), "keycloak", "nonexistent")
	require.ErrorIs(t, err, ErrIdentityNotFound)
}

func TestIdentities_UpdateProfile(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()

	u := &security.User{ID: "u-sso-2", Username: "authing:sub456", Role: "user", Status: "active"}
	require.NoError(t, store.CreateUser(ctx, u, 1700000000))

	ident := &UserIdentity{
		ID:          "ident-2",
		UserID:      "u-sso-2",
		Provider:    "authing",
		Subject:     "sub456",
		DisplayName: "Bob",
		Email:       "bob@old.com",
	}
	require.NoError(t, store.CreateUserIdentity(ctx, ident, 1700000001))

	// Update profile from IdP.
	require.NoError(t, store.UpdateUserIdentityProfile(ctx, "ident-2", "Bob Smith", "bob@new.com", 1700000002))

	got, err := store.GetUserIdentityByProviderSubject(ctx, "authing", "sub456")
	require.NoError(t, err)
	require.Equal(t, "Bob Smith", got.DisplayName)
	require.Equal(t, "bob@new.com", got.Email)
}

func TestIdentities_UniqueProviderSubject(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()

	u := &security.User{ID: "u-sso-3", Username: "oidc:sub789", Role: "user", Status: "active"}
	require.NoError(t, store.CreateUser(ctx, u, 1700000000))

	ident1 := &UserIdentity{
		ID: "ident-3a", UserID: "u-sso-3", Provider: "oidc", Subject: "sub789",
	}
	require.NoError(t, store.CreateUserIdentity(ctx, ident1, 1700000001))

	// Same provider+subject should fail (UNIQUE constraint).
	ident2 := &UserIdentity{
		ID: "ident-3b", UserID: "u-sso-3", Provider: "oidc", Subject: "sub789",
	}
	err := store.CreateUserIdentity(ctx, ident2, 1700000002)
	require.Error(t, err, "duplicate provider+subject must fail")
}

func TestIdentities_GetOrCreateUserByIdentity_CreatesUserAndIdentityAtomically(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()

	result, err := store.GetOrCreateUserByIdentity(ctx,
		"keycloak", "sub123", "keycloak:sub123", "Alice", "alice@example.com",
		"u-sso-1", "ident-1", 1700000000)
	require.NoError(t, err)
	require.True(t, result.Created)
	require.Equal(t, "u-sso-1", result.User.ID)
	require.Equal(t, "keycloak:sub123", result.User.Username)
	require.Equal(t, "active", result.User.Status)
	require.Equal(t, "ident-1", result.Identity.ID)
	require.Equal(t, "u-sso-1", result.Identity.UserID)

	user, err := store.GetUserByUsername(ctx, "keycloak:sub123")
	require.NoError(t, err)
	require.Equal(t, "u-sso-1", user.ID)
	identity, err := store.GetUserIdentityByProviderSubject(ctx, "keycloak", "sub123")
	require.NoError(t, err)
	require.Equal(t, "u-sso-1", identity.UserID)
}

func TestIdentities_GetOrCreateUserByIdentity_ReusesAndSyncsProfile(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()

	first, err := store.GetOrCreateUserByIdentity(ctx,
		"authing", "sub456", "authing:sub456", "Bob", "bob@old.example.com",
		"u-sso-2", "ident-2", 1700000000)
	require.NoError(t, err)
	require.True(t, first.Created)

	second, err := store.GetOrCreateUserByIdentity(ctx,
		"authing", "sub456", "authing:sub456", "Bob Smith", "bob@new.example.com",
		"ignored-user", "ignored-ident", 1700000001)
	require.NoError(t, err)
	require.False(t, second.Created)
	require.Equal(t, first.User.ID, second.User.ID)
	require.Equal(t, first.Identity.ID, second.Identity.ID)
	require.Equal(t, "Bob Smith", second.Identity.DisplayName)
	require.Equal(t, "bob@new.example.com", second.Identity.Email)
}

func TestIdentities_GetOrCreateUserByIdentity_ConcurrentFirstLogin(t *testing.T) {
	t.Parallel()
	store, _ := helperDB(t)
	ctx := context.Background()

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan *IdentityUserResult, workers)
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := store.GetOrCreateUserByIdentity(ctx,
				"oidc", "sub789", "oidc:sub789", "Dana", "dana@example.com",
				"user-concurrent-"+string(rune('a'+i)), "ident-concurrent-"+string(rune('a'+i)), 1700000000+int64(i))
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	var userID string
	var created int
	for result := range results {
		if result.Created {
			created++
		}
		if userID == "" {
			userID = result.User.ID
		}
		require.Equal(t, userID, result.User.ID)
		require.Equal(t, userID, result.Identity.UserID)
	}
	require.Equal(t, 1, created)
}
