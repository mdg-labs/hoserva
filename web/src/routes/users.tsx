import { MoreHorizontal, Plus, Users as UsersIcon } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ChoiceCards } from "@/components/patterns/choice-cards";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { CopyValue } from "@/components/patterns/copy-value";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { MultiPick } from "@/components/patterns/multi-pick";
import { SecretInput } from "@/components/patterns/secret-input";
import { SegmentedChoice } from "@/components/patterns/segmented-choice";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { SidePanel } from "@/components/patterns/side-panel";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Fieldset, FieldsetLegend } from "@/components/ui/fieldset";
import { Input } from "@/components/ui/input";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "@/components/ui/select";
import { SHARE_ACCESS_NONE, shareAccessOptions } from "@/hooks/share-access-options";
import type { components } from "@/lib/api/client";
import {
  deleteApiToken,
  deleteSession,
  deleteUser,
  deleteUserGroup,
  getApiTokens,
  getSessions,
  getShares,
  getUserGroups,
  getUsers,
  getUserSharePermissions,
  patchUser,
  postUser,
  postUserGroup,
  postUserPassword,
  postUserToken,
  putUserGroupMembers,
  putUserSharePermissions,
} from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";

type UserSummary = components["schemas"]["UserSummary"];
type UserGroup = components["schemas"]["UserGroup"];
type Session = components["schemas"]["Session"];
type ApiTokenSummary = components["schemas"]["ApiTokenSummary"];
type ApiTokenCreated = components["schemas"]["ApiTokenCreated"];
type ApiTokenRole = components["schemas"]["ApiTokenRole"];
type Share = components["schemas"]["Share"];
type ShareAccessLevel = components["schemas"]["ShareAccessLevel"];
type EditableRole = "viewer" | "share-only";

const EDITABLE_ROLES: EditableRole[] = ["viewer", "share-only"];
const USER_ROLE_FIELD_NAME = "user-role";

interface PendingRevoke {
  kind: "session" | "token" | "user" | "group";
  id: string;
  label: string;
}

export function UsersPage(): React.ReactElement {
  const { t } = useTranslation();
  const usersQuery = useApiQuery<{ users: UserSummary[] }>({
    queryKey: "users",
    queryFn: (signal) => getUsers(signal),
    fallbackError: t("users.errors.loadFailed"),
  });
  const groupsQuery = useApiQuery<{ groups: UserGroup[] }>({
    queryKey: "user-groups",
    queryFn: (signal) => getUserGroups(signal),
    fallbackError: t("users.errors.groupsLoadFailed"),
  });
  const sessionsQuery = useApiQuery<{ sessions: Session[] }>({
    queryKey: "sessions",
    queryFn: (signal) => getSessions(signal),
    fallbackError: t("users.errors.sessionsLoadFailed"),
  });
  const tokensQuery = useApiQuery<{ tokens: ApiTokenSummary[] }>({
    queryKey: "api-tokens",
    queryFn: (signal) => getApiTokens(signal),
    fallbackError: t("users.errors.tokensLoadFailed"),
  });
  const sharesQuery = useApiQuery<{ shares: Share[] }>({
    queryKey: "users-shares",
    queryFn: (signal) => getShares(signal),
    fallbackError: t("users.errors.sharesLoadFailed"),
  });

  const users = usersQuery.data?.users ?? null;
  const groups = groupsQuery.data?.groups ?? null;
  const sessions = sessionsQuery.data?.sessions ?? null;
  const tokens = tokensQuery.data?.tokens ?? null;
  const shares = sharesQuery.data?.shares ?? [];

  const [actionError, setActionError] = useState<string | null>(null);

  const [panelOpen, setPanelOpen] = useState(false);
  const [editingUser, setEditingUser] = useState<UserSummary | null>(null);
  const [usernameDraft, setUsernameDraft] = useState("");
  const [roleDraft, setRoleDraft] = useState<EditableRole>("share-only");
  const [smbAccessDraft, setSmbAccessDraft] = useState(false);
  const [passwordDraft, setPasswordDraft] = useState("");
  const [groupIdsDraft, setGroupIdsDraft] = useState<string[]>([]);
  const [sharePermissionsDraft, setSharePermissionsDraft] = useState<Record<string, ShareAccessLevel>>({});
  const [panelBusy, setPanelBusy] = useState(false);

  const [groupFormOpen, setGroupFormOpen] = useState(false);
  const [groupNameDraft, setGroupNameDraft] = useState("");

  const [tokenFormOpen, setTokenFormOpen] = useState(false);
  const [tokenUsername, setTokenUsername] = useState("");
  const [tokenName, setTokenName] = useState("");
  const [tokenRole, setTokenRole] = useState<ApiTokenRole>("viewer");
  const [createdToken, setCreatedToken] = useState<ApiTokenCreated | null>(null);

  const [pendingRevoke, setPendingRevoke] = useState<PendingRevoke | null>(null);

  // Keyed on the editing user's id so switching users (or closing and
  // reopening for someone else) resets and refetches through the hook's
  // own stale-response guard, rather than a hand-rolled ref (#228).
  const permissionsQuery = useApiQuery<{ permissions: { shareName: string; access: ShareAccessLevel }[] }>({
    queryKey: ["user-permissions", editingUser?.id ?? null],
    queryFn: (signal) => {
      if (!editingUser) {
        return Promise.resolve({ data: undefined, response: { ok: true } });
      }
      return getUserSharePermissions(editingUser.id, signal);
    },
    enabled: panelOpen && editingUser !== null,
    fallbackError: t("users.errors.permissionsLoadFailed"),
  });

  // Adjusts the draft when the permissions query's data changes, rather
  // than in an effect (react-hooks/set-state-in-effect).
  const [seenPermissionsData, setSeenPermissionsData] = useState<typeof permissionsQuery.data>(null);
  if (panelOpen && editingUser && permissionsQuery.data && permissionsQuery.data !== seenPermissionsData) {
    setSeenPermissionsData(permissionsQuery.data);
    const draft: Record<string, ShareAccessLevel> = {};
    for (const entry of permissionsQuery.data.permissions ?? []) {
      draft[entry.shareName] = entry.access;
    }
    setSharePermissionsDraft(draft);
  }

  const saveRoleMutation = useApiMutation<{ userId: string; role: EditableRole }, UserSummary>({
    mutationFn: ({ userId, role }) => patchUser(userId, { role }),
  });
  const createUserMutation = useApiMutation<{ username: string; role: EditableRole }, UserSummary>({
    mutationFn: (body) => postUser(body),
  });
  const setPasswordMutation = useApiMutation<{ userId: string; password: string }, unknown>({
    mutationFn: ({ userId, password }) => postUserPassword(userId, password),
  });
  const groupMembersMutation = useApiMutation<{ groupId: string; userIds: string[] }, unknown>({
    mutationFn: ({ groupId, userIds }) => putUserGroupMembers(groupId, userIds),
  });
  const sharePermissionsMutation = useApiMutation<
    { userId: string; permissions: { shareName: string; access: ShareAccessLevel }[] },
    unknown
  >({
    mutationFn: ({ userId, permissions }) => putUserSharePermissions(userId, { permissions }),
  });
  const createGroupMutation = useApiMutation<string, UserGroup>({
    mutationFn: (name) => postUserGroup(name),
  });
  const createTokenMutation = useApiMutation<{ username: string; name: string; role: ApiTokenRole }, ApiTokenCreated>({
    mutationFn: ({ username, name, role }) => postUserToken(username, { name, role }),
  });
  const revokeMutation = useApiMutation<PendingRevoke, unknown>({
    mutationFn: (target) => {
      if (target.kind === "session") return deleteSession(target.id);
      if (target.kind === "token") return deleteApiToken(target.id);
      if (target.kind === "user") return deleteUser(target.id);
      return deleteUserGroup(target.id);
    },
  });

  function openCreatePanel(): void {
    setEditingUser(null);
    setUsernameDraft("");
    setRoleDraft("share-only");
    setSmbAccessDraft(false);
    setPasswordDraft("");
    setGroupIdsDraft([]);
    setSharePermissionsDraft({});
    setActionError(null);
    setPanelOpen(true);
  }

  function openEditPanel(user: UserSummary): void {
    setEditingUser(user);
    setUsernameDraft(user.username);
    setRoleDraft(user.role === "admin" ? "viewer" : user.role);
    setSmbAccessDraft(false);
    setPasswordDraft("");
    setGroupIdsDraft((groups ?? []).filter((group) => group.memberUserIds.includes(user.id)).map((group) => group.id));
    setSharePermissionsDraft({});
    setActionError(null);
    setPanelOpen(true);
  }

  async function syncGroupMembership(userId: string): Promise<string | null> {
    const currentGroupIds = new Set(
      (groups ?? []).filter((group) => group.memberUserIds.includes(userId)).map((group) => group.id),
    );
    const nextGroupIds = new Set(groupIdsDraft);
    const changed = (groups ?? []).filter(
      (group) => currentGroupIds.has(group.id) !== nextGroupIds.has(group.id),
    );
    for (const group of changed) {
      const memberSet = new Set(group.memberUserIds);
      if (nextGroupIds.has(group.id)) {
        memberSet.add(userId);
      } else {
        memberSet.delete(userId);
      }
      const result = await groupMembersMutation.mutate({ groupId: group.id, userIds: [...memberSet] });
      if (!result.ok) {
        return result.aborted ? null : result.error;
      }
    }
    return null;
  }

  async function handleSaveUser(): Promise<void> {
    // `refreshing`, not `loading`: reopening the panel for the same user
    // keeps the previous fetch's `data` (the query key is unchanged), so
    // `loading` (which requires `data === null`) would stay false while a
    // stale draft that was never re-seeded for this opening is still on
    // screen — saving it would wipe that user's permissions (issue #271
    // finding 2).
    if (editingUser && (permissionsQuery.refreshing || permissionsQuery.error)) {
      setActionError(permissionsQuery.error ?? t("users.errors.permissionsLoadFailed"));
      return;
    }
    setPanelBusy(true);
    setActionError(null);
    try {
      let userId = editingUser?.id;
      if (editingUser) {
        if (editingUser.role !== "admin") {
          const result = await saveRoleMutation.mutate({ userId: editingUser.id, role: roleDraft });
          if (!result.ok) {
            if (!result.aborted) setActionError(result.error);
            return;
          }
        }
      } else {
        const name = usernameDraft.trim();
        if (name.length === 0) {
          setActionError(t("users.errors.usernameRequired"));
          return;
        }
        const result = await createUserMutation.mutate({ username: name, role: roleDraft });
        if (!result.ok) {
          if (!result.aborted) setActionError(result.error);
          return;
        }
        userId = result.data?.id;
      }

      if (!userId) return;

      if (smbAccessDraft && passwordDraft.length > 0) {
        const result = await setPasswordMutation.mutate({ userId, password: passwordDraft });
        if (!result.ok) {
          if (!result.aborted) setActionError(result.error);
          return;
        }
      }

      const groupSyncError = await syncGroupMembership(userId);
      if (groupSyncError) {
        setActionError(groupSyncError);
        return;
      }

      if (editingUser) {
        const permissions = Object.entries(sharePermissionsDraft)
          .filter(([, access]) => access !== "none")
          .map(([shareName, access]) => ({ shareName, access }));
        const result = await sharePermissionsMutation.mutate({ userId, permissions });
        if (!result.ok) {
          if (!result.aborted) setActionError(result.error);
          return;
        }
      }

      setPanelOpen(false);
      await Promise.all([usersQuery.refresh(), groupsQuery.refresh()]);
    } finally {
      setPanelBusy(false);
    }
  }

  async function handleCreateGroup(): Promise<void> {
    const name = groupNameDraft.trim();
    if (name.length === 0) return;
    const result = await createGroupMutation.mutate(name);
    if (!result.ok) {
      if (!result.aborted) setActionError(result.error);
      return;
    }
    setGroupFormOpen(false);
    setGroupNameDraft("");
    await groupsQuery.refresh();
  }

  async function handleCreateToken(): Promise<void> {
    const name = tokenName.trim();
    if (name.length === 0 || tokenUsername.length === 0) return;
    const result = await createTokenMutation.mutate({ username: tokenUsername, name, role: tokenRole });
    if (!result.ok) {
      if (!result.aborted) setActionError(result.error);
      return;
    }
    if (result.data) {
      setCreatedToken(result.data);
    }
    setTokenFormOpen(false);
    setTokenName("");
    await tokensQuery.refresh();
  }

  function requestDeleteUser(user: UserSummary): void {
    setActionError(null);
    setPendingRevoke({ kind: "user", id: user.id, label: user.username });
  }

  function requestDeleteGroup(group: UserGroup): void {
    setActionError(null);
    setPendingRevoke({ kind: "group", id: group.id, label: group.name });
  }

  function requestRevokeSession(session: Session): void {
    setActionError(null);
    setPendingRevoke({ kind: "session", id: session.id, label: session.username });
  }

  function requestRevokeToken(token: ApiTokenSummary): void {
    setActionError(null);
    setPendingRevoke({ kind: "token", id: token.id, label: token.name });
  }

  async function handleRevoke(): Promise<void> {
    if (!pendingRevoke) return;
    const result = await revokeMutation.mutate(pendingRevoke);
    if (!result.ok) {
      if (!result.aborted) setActionError(result.error);
      return;
    }
    const kind = pendingRevoke.kind;
    setPendingRevoke(null);
    if (kind === "session") await sessionsQuery.refresh();
    else if (kind === "token") await tokensQuery.refresh();
    else if (kind === "user") await usersQuery.refresh();
    else await groupsQuery.refresh();
  }

  const userColumns: DataTableColumn<UserSummary>[] = [
    { id: "username", header: t("users.columns.username"), cell: (user) => user.username },
    {
      id: "role",
      header: t("users.columns.role"),
      cell: (user) => <StatusBadge tone={user.role === "admin" ? "info" : "outline"}>{t(`users.roles.${user.role}`)}</StatusBadge>,
    },
    {
      id: "uiLogin",
      header: t("users.columns.uiLogin"),
      cell: (user) => (
        <StatusBadge tone={user.role === "share-only" ? "outline" : "success"}>
          {user.role === "share-only" ? t("common.no") : t("common.yes")}
        </StatusBadge>
      ),
    },
    {
      id: "totp",
      header: t("users.columns.totp"),
      cell: (user) => (
        <StatusBadge tone={user.totpEnrolled ? "success" : "outline"}>
          {user.totpEnrolled ? t("common.yes") : t("common.no")}
        </StatusBadge>
      ),
    },
    {
      id: "lastLogin",
      header: t("users.columns.lastLogin"),
      cell: (user) => (user.lastLogin ? new Date(user.lastLogin).toLocaleString() : t("users.neverLoggedIn")),
    },
    {
      id: "actions",
      header: t("users.columns.actions"),
      cell: (user) => (
        <Menu>
          <MenuTrigger
            render={
              <Button size="icon-sm" variant="ghost" aria-label={t("users.actionsMenu", { username: user.username })}>
                <MoreHorizontal />
              </Button>
            }
          />
          <MenuContent>
            <MenuItem onClick={() => openEditPanel(user)}>{t("users.edit")}</MenuItem>
            <MenuItem
              disabled={user.role === "admin"}
              className="text-destructive-foreground"
              onClick={() => requestDeleteUser(user)}
            >
              {t("users.delete")}
            </MenuItem>
          </MenuContent>
        </Menu>
      ),
    },
  ];

  const groupColumns: DataTableColumn<UserGroup>[] = [
    { id: "name", header: t("users.groups.columns.name"), cell: (group) => group.name },
    {
      id: "members",
      header: t("users.groups.columns.members"),
      cell: (group) => t("users.groups.memberCount", { count: group.memberUserIds.length }),
    },
    {
      id: "actions",
      header: t("users.groups.columns.actions"),
      cell: (group) => (
        <Button
          size="sm"
          variant="destructive-outline"
          onClick={() => requestDeleteGroup(group)}
        >
          {t("users.groups.delete")}
        </Button>
      ),
    },
  ];

  const sessionColumns: DataTableColumn<Session>[] = [
    { id: "username", header: t("users.sessions.columns.username"), cell: (session) => session.username },
    {
      id: "created",
      header: t("users.sessions.columns.created"),
      cell: (session) => new Date(session.createdAt).toLocaleString(),
    },
    {
      id: "expires",
      header: t("users.sessions.columns.expires"),
      cell: (session) => new Date(session.expiresAt).toLocaleString(),
    },
    {
      id: "actions",
      header: t("users.sessions.columns.actions"),
      cell: (session) => (
        <Button
          size="sm"
          variant="destructive-outline"
          onClick={() => requestRevokeSession(session)}
        >
          {t("users.sessions.revoke")}
        </Button>
      ),
    },
  ];

  const tokenColumns: DataTableColumn<ApiTokenSummary>[] = [
    { id: "username", header: t("users.tokens.columns.username"), cell: (token) => token.username },
    { id: "name", header: t("users.tokens.columns.name"), cell: (token) => token.name },
    { id: "role", header: t("users.tokens.columns.role"), cell: (token) => t(`users.roles.${token.role}`) },
    {
      id: "created",
      header: t("users.tokens.columns.created"),
      cell: (token) => new Date(token.createdAt).toLocaleString(),
    },
    {
      id: "actions",
      header: t("users.tokens.columns.actions"),
      cell: (token) => (
        <Button
          size="sm"
          variant="destructive-outline"
          onClick={() => requestRevokeToken(token)}
        >
          {t("users.tokens.revoke")}
        </Button>
      ),
    },
  ];

  const tokenEligibleUsers = (users ?? []).filter((user) => user.role !== "share-only");
  const tokenRoleOptions: ApiTokenRole[] =
    tokenUsername && (users ?? []).find((user) => user.username === tokenUsername)?.role === "admin"
      ? ["viewer", "admin"]
      : ["viewer"];

  const sharePermissionColumns: DataTableColumn<Share>[] = [
    { id: "name", header: t("users.panel.permissions.columns.share"), cell: (share) => share.name },
    {
      id: "access",
      header: t("users.panel.permissions.columns.access"),
      cell: (share) => {
        const fieldName = `user-access-${share.name}`;
        const currentAccess = sharePermissionsDraft[share.name] ?? SHARE_ACCESS_NONE;
        return (
          <SegmentedChoice
            name={fieldName}
            value={currentAccess}
            onChange={(value) =>
              setSharePermissionsDraft((current) => ({ ...current, [share.name]: value as ShareAccessLevel }))
            }
            options={shareAccessOptions(t)}
          />
        );
      },
    },
  ];

  const error = usersQuery.error;

  if (users === null && !error) {
    return <LoadingBlock />;
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h1 className="text-2xl font-semibold font-heading">{t("nav.users")}</h1>
          <p className="text-muted-foreground">{t("users.description")}</p>
        </div>
        <Button onClick={openCreatePanel}>
          <Plus aria-hidden="true" />
          {t("users.create")}
        </Button>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      {actionError ? <Banner tone="error" title={actionError} /> : null}

      {users && users.length === 0 ? (
        <EmptyState icon={UsersIcon} title={t("users.empty.title")} description={t("users.empty.description")} />
      ) : (
        <DataTable columns={userColumns} rows={users ?? []} getRowKey={(user) => user.id} />
      )}

      <Card>
        <CardHeader className="flex flex-row items-center justify-between gap-4">
          <CardTitle>{t("users.groups.title")}</CardTitle>
          <Button size="sm" onClick={() => setGroupFormOpen(true)}>
            <Plus aria-hidden="true" />
            {t("users.groups.create")}
          </Button>
        </CardHeader>
        <CardPanel>
          {groupsQuery.error ? (
            <Banner tone="error" title={groupsQuery.error} />
          ) : (groups ?? []).length === 0 ? (
            <p className="text-muted-foreground text-sm">{t("users.groups.empty")}</p>
          ) : (
            <DataTable columns={groupColumns} rows={groups ?? []} getRowKey={(group) => group.id} />
          )}
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("users.sessions.title")}</CardTitle>
        </CardHeader>
        <CardPanel>
          {sessionsQuery.error ? (
            <Banner tone="error" title={sessionsQuery.error} />
          ) : (sessions ?? []).length === 0 ? (
            <p className="text-muted-foreground text-sm">{t("users.sessions.empty")}</p>
          ) : (
            <DataTable columns={sessionColumns} rows={sessions ?? []} getRowKey={(session) => session.id} />
          )}
        </CardPanel>
      </Card>

      <Card>
        <CardHeader className="flex flex-row items-center justify-between gap-4">
          <CardTitle>{t("users.tokens.title")}</CardTitle>
          <Button size="sm" onClick={() => setTokenFormOpen(true)}>
            <Plus aria-hidden="true" />
            {t("users.tokens.create")}
          </Button>
        </CardHeader>
        <CardPanel>
          {tokensQuery.error ? (
            <Banner tone="error" title={tokensQuery.error} />
          ) : (tokens ?? []).length === 0 ? (
            <p className="text-muted-foreground text-sm">{t("users.tokens.empty")}</p>
          ) : (
            <DataTable columns={tokenColumns} rows={tokens ?? []} getRowKey={(token) => token.id} />
          )}
        </CardPanel>
      </Card>

      <Fieldset disabled>
        <FieldsetLegend>{t("users.oidc.title")}</FieldsetLegend>
        <InlineNote description={t("users.oidc.description")} />
      </Fieldset>

      <SidePanel
        open={panelOpen}
        onOpenChange={setPanelOpen}
        title={editingUser ? t("users.panel.editTitle", { username: editingUser.username }) : t("users.panel.createTitle")}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={() => setPanelOpen(false)}>
              {t("confirm.cancel")}
            </Button>
            <Button
              loading={panelBusy}
              disabled={Boolean(editingUser) && (permissionsQuery.refreshing || Boolean(permissionsQuery.error))}
              onClick={() => void handleSaveUser()}
            >
              {t("shares.detail.save")}
            </Button>
          </div>
        }
      >
        {!editingUser ? (
          <Field>
            <FieldLabel>{t("users.panel.username")}</FieldLabel>
            <Input value={usernameDraft} onChange={(event) => setUsernameDraft(event.target.value)} />
          </Field>
        ) : null}

        {editingUser?.role === "admin" ? (
          <InlineNote description={t("users.panel.adminRoleLocked")} />
        ) : (
          <div className="flex flex-col gap-2">
            <p className="font-medium text-sm">{t("users.panel.role")}</p>
            <ChoiceCards
              name={USER_ROLE_FIELD_NAME}
              value={roleDraft}
              onChange={(value) => setRoleDraft(value as EditableRole)}
              options={EDITABLE_ROLES.map((role) => ({
                value: role,
                title: t(`users.roles.${role}`),
                description: t(`users.panel.roleDescriptions.${role}`),
              }))}
            />
          </div>
        )}

        {editingUser ? (
          <div className="flex flex-col gap-2">
            <SettingSwitch
              label={t("users.panel.uiLoginAccess")}
              description={t("users.panel.uiLoginAccessDescription")}
              checked={editingUser.role !== "share-only" && editingUser.hasCredential}
              disabled
            />
            <SettingSwitch
              label={t("users.panel.smbAccess")}
              description={t("users.panel.smbAccessDescription")}
              checked={editingUser.hasCredential}
              disabled
            />
            <Field className="flex-row items-start gap-2">
              <Checkbox
                checked={smbAccessDraft}
                onCheckedChange={(checked) => setSmbAccessDraft(checked === true)}
                aria-label={t("users.panel.smbAccessSetAction")}
              />
              <div className="flex min-w-0 flex-col gap-1">
                <FieldLabel className="cursor-default">{t("users.panel.smbAccessSetAction")}</FieldLabel>
                <FieldDescription>{t("users.panel.smbAccessSetActionDescription")}</FieldDescription>
              </div>
            </Field>
          </div>
        ) : (
          <SettingSwitch
            label={t("users.panel.smbAccess")}
            description={t("users.panel.smbAccessDescription")}
            checked={smbAccessDraft}
            onCheckedChange={setSmbAccessDraft}
          />
        )}
        {smbAccessDraft ? (
          <Field>
            <FieldLabel>{t("users.panel.password")}</FieldLabel>
            <SecretInput value={passwordDraft} onChange={setPasswordDraft} showStrength showGenerate />
          </Field>
        ) : null}

        <div className="flex flex-col gap-2">
          <p className="font-medium text-sm">{t("users.panel.groups")}</p>
          <MultiPick
            value={groupIdsDraft}
            onChange={setGroupIdsDraft}
            options={(groups ?? []).map((group) => ({ value: group.id, label: group.name }))}
            placeholder={t("users.panel.groupsPlaceholder")}
          />
        </div>

        {editingUser ? (
          <div className="flex flex-col gap-2">
            <p className="font-medium text-sm">{t("users.panel.permissions.title")}</p>
            {permissionsQuery.error ? (
              <Banner tone="error" title={permissionsQuery.error} />
            ) : permissionsQuery.loading ? (
              <LoadingBlock rows={2} />
            ) : sharesQuery.error ? (
              <Banner tone="error" title={sharesQuery.error} />
            ) : shares.length === 0 ? (
              <p className="text-muted-foreground text-sm">{t("users.panel.permissions.empty")}</p>
            ) : (
              <DataTable columns={sharePermissionColumns} rows={shares} getRowKey={(share) => share.name} />
            )}
          </div>
        ) : null}
      </SidePanel>

      <FormOverlay
        open={groupFormOpen}
        onOpenChange={setGroupFormOpen}
        title={t("users.groups.create")}
        footer={
          <Button loading={createGroupMutation.pending} disabled={groupNameDraft.trim().length === 0} onClick={() => void handleCreateGroup()}>
            {t("users.groups.create")}
          </Button>
        }
      >
        <Field>
          <FieldLabel>{t("users.groups.name")}</FieldLabel>
          <Input value={groupNameDraft} onChange={(event) => setGroupNameDraft(event.target.value)} />
        </Field>
      </FormOverlay>

      <FormOverlay
        open={tokenFormOpen}
        onOpenChange={setTokenFormOpen}
        title={t("users.tokens.create")}
        footer={
          <Button
            loading={createTokenMutation.pending}
            disabled={tokenUsername.length === 0 || tokenName.trim().length === 0}
            onClick={() => void handleCreateToken()}
          >
            {t("users.tokens.create")}
          </Button>
        }
      >
        <Field>
          <FieldLabel>{t("users.tokens.account")}</FieldLabel>
          <Select value={tokenUsername} onValueChange={(value) => value && setTokenUsername(value)}>
            <SelectTrigger>
              <SelectValue placeholder={t("users.tokens.accountPlaceholder")} />
            </SelectTrigger>
            <SelectPopup>
              {tokenEligibleUsers.map((user) => (
                <SelectItem key={user.id} value={user.username}>
                  {user.username}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
        </Field>
        <Field>
          <FieldLabel>{t("users.tokens.name")}</FieldLabel>
          <Input value={tokenName} onChange={(event) => setTokenName(event.target.value)} />
        </Field>
        <Field>
          <FieldLabel>{t("users.tokens.role")}</FieldLabel>
          <Select value={tokenRole} onValueChange={(value) => value && setTokenRole(value as ApiTokenRole)}>
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectPopup>
              {tokenRoleOptions.map((role) => (
                <SelectItem key={role} value={role}>
                  {t(`users.roles.${role}`)}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
        </Field>
      </FormOverlay>

      <Dialog open={createdToken !== null} onOpenChange={(open) => !open && setCreatedToken(null)}>
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>{t("users.tokens.createdTitle")}</DialogTitle>
            <DialogDescription>{t("users.tokens.createdDescription")}</DialogDescription>
          </DialogHeader>
          <DialogPanel>
            {createdToken ? <CopyValue value={createdToken.token} label={t("users.tokens.createdTitle")} /> : null}
          </DialogPanel>
          <DialogFooter>
            <DialogClose render={<Button />}>{t("confirm.confirm")}</DialogClose>
          </DialogFooter>
        </DialogPopup>
      </Dialog>

      <ConfirmDialog
        open={pendingRevoke !== null}
        onOpenChange={(open) => !open && setPendingRevoke(null)}
        title={
          pendingRevoke
            ? t(`users.revokeTitles.${pendingRevoke.kind}`, { label: pendingRevoke.label })
            : ""
        }
        destructive
        loading={revokeMutation.pending}
        onConfirm={() => void handleRevoke()}
      />
    </div>
  );
}
