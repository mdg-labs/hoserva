import { MoreHorizontal, Plus, Users as UsersIcon } from "lucide-react";
import { useEffect, useRef, useState } from "react";
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
import { hoservaClient, type components } from "@/lib/api/client";

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
  const [users, setUsers] = useState<UserSummary[] | null>(null);
  const [groups, setGroups] = useState<UserGroup[] | null>(null);
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [tokens, setTokens] = useState<ApiTokenSummary[] | null>(null);
  const [shares, setShares] = useState<Share[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const [panelOpen, setPanelOpen] = useState(false);
  const [editingUser, setEditingUser] = useState<UserSummary | null>(null);
  const [usernameDraft, setUsernameDraft] = useState("");
  const [roleDraft, setRoleDraft] = useState<EditableRole>("share-only");
  const [smbAccessDraft, setSmbAccessDraft] = useState(false);
  const [passwordDraft, setPasswordDraft] = useState("");
  const [groupIdsDraft, setGroupIdsDraft] = useState<string[]>([]);
  const [sharePermissionsDraft, setSharePermissionsDraft] = useState<Record<string, ShareAccessLevel>>({});
  // The user id openEditPanel's own permissions fetch is for — checked when
  // that fetch resolves so a slow response for a panel the user has since
  // closed or reopened for someone else can never overwrite that other
  // user's draft (#228 CodeRabbit finding).
  const editingPermissionsForRef = useRef<string | null>(null);
  const [panelBusy, setPanelBusy] = useState(false);

  const [groupFormOpen, setGroupFormOpen] = useState(false);
  const [groupNameDraft, setGroupNameDraft] = useState("");
  const [groupBusy, setGroupBusy] = useState(false);

  const [tokenFormOpen, setTokenFormOpen] = useState(false);
  const [tokenUsername, setTokenUsername] = useState("");
  const [tokenName, setTokenName] = useState("");
  const [tokenRole, setTokenRole] = useState<ApiTokenRole>("viewer");
  const [tokenBusy, setTokenBusy] = useState(false);
  const [createdToken, setCreatedToken] = useState<ApiTokenCreated | null>(null);

  const [pendingRevoke, setPendingRevoke] = useState<PendingRevoke | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);

  function load(signal?: AbortSignal): void {
    Promise.all([
      hoservaClient.GET("/users", { signal }),
      hoservaClient.GET("/user-groups", { signal }),
      hoservaClient.GET("/sessions", { signal }),
      hoservaClient.GET("/api-tokens", { signal }),
      hoservaClient.GET("/shares", { signal }),
    ])
      .then(([usersResult, groupsResult, sessionsResult, tokensResult, sharesResult]) => {
        if (signal?.aborted) return;
        if (usersResult.error) {
          setError(usersResult.error.message);
          return;
        }
        setError(null);
        setUsers(usersResult.data?.users ?? []);
        setGroups(groupsResult.data?.groups ?? []);
        setSessions(sessionsResult.data?.sessions ?? []);
        setTokens(tokensResult.data?.tokens ?? []);
        setShares(sharesResult.data?.shares ?? []);
      })
      .catch((err: unknown) => {
        if (!signal?.aborted) {
          setError(err instanceof Error ? err.message : String(err));
        }
      });
  }

  useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, []);

  function openCreatePanel(): void {
    setEditingUser(null);
    setUsernameDraft("");
    setRoleDraft("share-only");
    setSmbAccessDraft(false);
    setPasswordDraft("");
    setGroupIdsDraft([]);
    setSharePermissionsDraft({});
    setPanelOpen(true);
    editingPermissionsForRef.current = null;
  }

  function openEditPanel(user: UserSummary): void {
    setEditingUser(user);
    setUsernameDraft(user.username);
    setRoleDraft(user.role === "admin" ? "viewer" : user.role);
    setSmbAccessDraft(false);
    setPasswordDraft("");
    setGroupIdsDraft((groups ?? []).filter((group) => group.memberUserIds.includes(user.id)).map((group) => group.id));
    setSharePermissionsDraft({});
    setPanelOpen(true);
    editingPermissionsForRef.current = user.id;
    void hoservaClient
      .GET("/users/{userId}/permissions", { params: { path: { userId: user.id } } })
      .then(({ data }) => {
        if (editingPermissionsForRef.current !== user.id) return;
        const draft: Record<string, ShareAccessLevel> = {};
        for (const entry of data?.permissions ?? []) {
          draft[entry.shareName] = entry.access;
        }
        setSharePermissionsDraft(draft);
      });
  }

  async function syncGroupMembership(userId: string): Promise<void> {
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
      await hoservaClient.PUT("/user-groups/{groupId}/members", {
        params: { path: { groupId: group.id } },
        body: { userIds: [...memberSet] },
      });
    }
  }

  async function handleSaveUser(): Promise<void> {
    setPanelBusy(true);
    setActionError(null);
    try {
      let userId = editingUser?.id;
      if (editingUser) {
        if (editingUser.role !== "admin") {
          const { error: apiError } = await hoservaClient.PATCH("/users/{userId}", {
            params: { path: { userId: editingUser.id } },
            body: { role: roleDraft },
          });
          if (apiError) {
            setActionError(apiError.message);
            return;
          }
        }
      } else {
        const name = usernameDraft.trim();
        if (name.length === 0) {
          setActionError(t("users.errors.usernameRequired"));
          return;
        }
        const { data, error: apiError } = await hoservaClient.POST("/users", {
          body: { username: name, role: roleDraft },
        });
        if (apiError) {
          setActionError(apiError.message);
          return;
        }
        userId = data?.id;
      }

      if (!userId) return;

      if (smbAccessDraft && passwordDraft.length > 0) {
        const { error: apiError } = await hoservaClient.POST("/users/{userId}/password", {
          params: { path: { userId } },
          body: { password: passwordDraft },
        });
        if (apiError) {
          setActionError(apiError.message);
          return;
        }
      }

      await syncGroupMembership(userId);

      if (editingUser) {
        const permissions = Object.entries(sharePermissionsDraft)
          .filter(([, access]) => access !== "none")
          .map(([shareName, access]) => ({ shareName, access }));
        const { error: apiError } = await hoservaClient.PUT("/users/{userId}/permissions", {
          params: { path: { userId } },
          body: { permissions },
        });
        if (apiError) {
          setActionError(apiError.message);
          return;
        }
      }

      setPanelOpen(false);
      load();
    } finally {
      setPanelBusy(false);
    }
  }

  async function handleCreateGroup(): Promise<void> {
    const name = groupNameDraft.trim();
    if (name.length === 0) return;
    setGroupBusy(true);
    try {
      const { error: apiError } = await hoservaClient.POST("/user-groups", { body: { name } });
      if (apiError) {
        setActionError(apiError.message);
        return;
      }
      setGroupFormOpen(false);
      setGroupNameDraft("");
      load();
    } finally {
      setGroupBusy(false);
    }
  }

  async function handleCreateToken(): Promise<void> {
    const name = tokenName.trim();
    if (name.length === 0 || tokenUsername.length === 0) return;
    setTokenBusy(true);
    try {
      const { data, error: apiError } = await hoservaClient.POST("/users/{username}/tokens", {
        params: { path: { username: tokenUsername } },
        body: { name, role: tokenRole },
      });
      if (apiError) {
        setActionError(apiError.message);
        return;
      }
      if (data) {
        setCreatedToken(data);
      }
      setTokenFormOpen(false);
      setTokenName("");
      load();
    } finally {
      setTokenBusy(false);
    }
  }

  function requestDeleteUser(user: UserSummary): void {
    setPendingRevoke({ kind: "user", id: user.id, label: user.username });
  }

  function requestDeleteGroup(group: UserGroup): void {
    setPendingRevoke({ kind: "group", id: group.id, label: group.name });
  }

  function requestRevokeSession(session: Session): void {
    setPendingRevoke({ kind: "session", id: session.id, label: session.username });
  }

  function requestRevokeToken(token: ApiTokenSummary): void {
    setPendingRevoke({ kind: "token", id: token.id, label: token.name });
  }

  async function handleRevoke(): Promise<void> {
    if (!pendingRevoke) return;
    setRevokeBusy(true);
    try {
      let apiError: { message: string } | undefined;
      if (pendingRevoke.kind === "session") {
        ({ error: apiError } = await hoservaClient.DELETE("/sessions/{sessionId}", {
          params: { path: { sessionId: pendingRevoke.id } },
        }));
      } else if (pendingRevoke.kind === "token") {
        ({ error: apiError } = await hoservaClient.DELETE("/api-tokens/{tokenId}", {
          params: { path: { tokenId: pendingRevoke.id } },
        }));
      } else if (pendingRevoke.kind === "user") {
        ({ error: apiError } = await hoservaClient.DELETE("/users/{userId}", {
          params: { path: { userId: pendingRevoke.id } },
        }));
      } else {
        ({ error: apiError } = await hoservaClient.DELETE("/user-groups/{groupId}", {
          params: { path: { groupId: pendingRevoke.id } },
        }));
      }
      if (apiError) {
        setActionError(apiError.message);
        return;
      }
      setPendingRevoke(null);
      load();
    } finally {
      setRevokeBusy(false);
    }
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
          {(groups ?? []).length === 0 ? (
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
          {(sessions ?? []).length === 0 ? (
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
          {(tokens ?? []).length === 0 ? (
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
            <Button loading={panelBusy} onClick={() => void handleSaveUser()}>
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
            {shares.length === 0 ? (
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
          <Button loading={groupBusy} disabled={groupNameDraft.trim().length === 0} onClick={() => void handleCreateGroup()}>
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
            loading={tokenBusy}
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
        loading={revokeBusy}
        onConfirm={() => void handleRevoke()}
      />
    </div>
  );
}
