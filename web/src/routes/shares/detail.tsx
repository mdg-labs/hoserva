import { Home, Trash2 } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { toastManager } from "@/components/ui/toast";
import { jobDetailPath } from "@/hooks/paths";

import { Banner } from "@/components/patterns/banner";
import { ChoiceCards } from "@/components/patterns/choice-cards";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { CopyValue } from "@/components/patterns/copy-value";
import { CodeView } from "@/components/patterns/code-view";
import { DangerZone, type DangerZoneAction } from "@/components/patterns/danger-zone";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { DetailTabs } from "@/components/patterns/detail-tabs";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { InlineNote } from "@/components/patterns/inline-note";
import { ListInput } from "@/components/patterns/list-input";
import { LoadingBlock } from "@/components/patterns/loading";
import { NumberUnit } from "@/components/patterns/number-unit";
import { PlainTerm } from "@/components/patterns/plain-term";
import { SegmentedChoice } from "@/components/patterns/segmented-choice";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { StatusBadge } from "@/components/patterns/status-badge";
import { TypedConfirm } from "@/components/patterns/typed-confirm";
import { Breadcrumb, BreadcrumbButton, BreadcrumbItem, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator } from "@/components/ui/breadcrumb";
import { Button } from "@/components/ui/button";
import { Card, CardFooter, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "@/components/ui/select";
import { PATHS } from "@/hooks/paths";
import {
  SHARE_TAB_ALLOCATION,
  SHARE_TAB_BROWSE,
  SHARE_TAB_CACHE,
  SHARE_TAB_DANGER,
  SHARE_TAB_GENERAL,
  SHARE_TAB_NFS,
  SHARE_TAB_SMB,
} from "@/hooks/share-detail-tabs";
import { shareAccessOptions } from "@/hooks/share-access-options";
import type { components } from "@/lib/api/client";
import {
  deleteShare,
  deleteShareFile,
  getShare,
  getSharePermissions,
  getUserGroups,
  getUsers,
  getShareBrowse,
  patchShare,
  postShareDataDelete,
  postShareRelocate,
  putSharePermissions,
} from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import { shareMutationError, shareRelocationDirection } from "@/routes/shares/cache-mode";
import { buildNfsExportLine, buildSmbStanza } from "@/routes/shares/config-preview";
import { formatBytes } from "@/routes/storage-setup/config-preview";

type Share = components["schemas"]["Share"];
type ShareSMB = components["schemas"]["ShareSMB"];
type ShareNFS = components["schemas"]["ShareNFS"];
type ShareCacheMode = components["schemas"]["ShareCacheMode"];
type ArrayCreatePolicy = components["schemas"]["ArrayCreatePolicy"];
type ShareAccessLevel = components["schemas"]["ShareAccessLevel"];
type ShareBrowseEntry = components["schemas"]["ShareBrowseEntry"];
type ShareDiskUsage = components["schemas"]["ShareDiskUsage"];


const CREATE_POLICIES: ArrayCreatePolicy[] = ["mspmfs", "mfs", "lfs", "ff"];
const CACHE_MODES: ShareCacheMode[] = ["cache-then-move", "cache-only", "array-only"];

const SQUASH_OPTIONS: NonNullable<ShareNFS["squash"]>[] = ["root_squash", "no_root_squash", "all_squash"];
const CREATE_POLICY_FIELD_NAME = "create-policy";
const CACHE_MODE_FIELD_NAME = "cache-mode";
const DEFAULT_TIME_MACHINE_MAX_SIZE = "500G";
const UNKNOWN_VALUE = "—";
const BROWSE_COLUMN_NAME = "name";
const BROWSE_COLUMN_SIZE = "size";
const BROWSE_COLUMN_DISK = "disk";
const BROWSE_COLUMN_ACTIONS = "actions";
const USAGE_COLUMN_DISK = "disk";
const USAGE_COLUMN_BYTES = "bytes";
const DANGER_ACTION_REMOVE = "remove";
const DANGER_ACTION_DELETE_DATA = "delete-data";

interface PermissionRow {
  kind: "user" | "group";
  id: string;
  label: string;
  access: ShareAccessLevel;
}

export function ShareDetailPage(): React.ReactElement {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { name = "" } = useParams();
  const [searchParams] = useSearchParams();
  const initialTab = searchParams.get("tab") ?? SHARE_TAB_GENERAL;

  const shareQuery = useApiQuery<Share>({
    queryKey: ["share", name],
    queryFn: (signal) => getShare(name, signal),
    fallbackError: t("shares.detail.loadFailed"),
  });
  const detailUsersQuery = useApiQuery<{ users: components["schemas"]["UserSummary"][] }>({
    queryKey: "share-detail-users",
    queryFn: (signal) => getUsers(signal),
    fallbackError: t("shares.detail.loadFailed"),
  });
  const detailGroupsQuery = useApiQuery<{ groups: components["schemas"]["UserGroup"][] }>({
    queryKey: "share-detail-groups",
    queryFn: (signal) => getUserGroups(signal),
    fallbackError: t("shares.detail.loadFailed"),
  });
  const sharePermissionsQuery = useApiQuery<components["schemas"]["SharePermissionsResult"]>({
    queryKey: ["share-permissions", name],
    queryFn: (signal) => getSharePermissions(name, signal),
    fallbackError: t("shares.detail.loadFailed"),
  });

  const [share, setShare] = useState<Share | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);

  const [createPolicyDraft, setCreatePolicyDraft] = useState<ArrayCreatePolicy>("mspmfs");

  const [cacheModeDraft, setCacheModeDraft] = useState<ShareCacheMode>("cache-then-move");
  const [cacheConfirmOpen, setCacheConfirmOpen] = useState(false);
  // The mode the share had when the dialog opened: saveCacheMode updates
  // share.cacheMode before the relocate call, so a relocate that then
  // fails must still be offered (and summarized) from this mode.
  const [cacheModeFrom, setCacheModeFrom] = useState<ShareCacheMode | null>(null);
  const [cacheDialogError, setCacheDialogError] = useState<string | null>(null);

  const [smbDraft, setSmbDraft] = useState<ShareSMB | null>(null);

  const [nfsDraft, setNfsDraft] = useState<ShareNFS | null>(null);

  const [permissionRows, setPermissionRows] = useState<PermissionRow[] | null>(null);

  const [browseAcknowledged, setBrowseAcknowledged] = useState(false);
  const [browsePath, setBrowsePath] = useState("");
  const [browseEntries, setBrowseEntries] = useState<ShareBrowseEntry[] | null>(null);
  const [browseError, setBrowseError] = useState<string | null>(null);
  const [deleteFileTarget, setDeleteFileTarget] = useState<string | null>(null);

  const [removeOpen, setRemoveOpen] = useState(false);
  const [removeDialogError, setRemoveDialogError] = useState<string | null>(null);
  const [deleteDataOpen, setDeleteDataOpen] = useState(false);
  const [deleteDataConfirm, setDeleteDataConfirm] = useState("");
  const [deleteDataError, setDeleteDataError] = useState<string | null>(null);

  // Adjusts state when the loaded share changes, rather than in an effect
  // (react-hooks/set-state-in-effect).
  const [seenShare, setSeenShare] = useState<Share | null>(null);
  if (shareQuery.data && shareQuery.data !== seenShare) {
    const loaded = shareQuery.data;
    setSeenShare(loaded);
    setShare(loaded);
    setCreatePolicyDraft(loaded.createPolicy);
    setCacheModeDraft(loaded.cacheMode);
    setSmbDraft(loaded.smb);
    setNfsDraft(loaded.nfs);
  }

  // Only seeds once all three of users, groups and this share's permissions
  // have loaded — a failed permissions fetch must never read as "no
  // access", and a save built from a partial draft would silently wipe
  // every grant this share actually has (issue #271 finding 2). Adjusted
  // during render, not in an effect, for the same reason as above; each
  // source is tracked separately since the three queries settle at
  // different times.
  const [seenUsers, setSeenUsers] = useState<typeof detailUsersQuery.data>(null);
  const [seenGroups, setSeenGroups] = useState<typeof detailGroupsQuery.data>(null);
  const [seenPermissions, setSeenPermissions] = useState<typeof sharePermissionsQuery.data>(null);
  const userList = detailUsersQuery.data?.users;
  const groupList = detailGroupsQuery.data?.groups;
  const permissions = sharePermissionsQuery.data;
  if (
    userList &&
    groupList &&
    permissions &&
    (detailUsersQuery.data !== seenUsers ||
      detailGroupsQuery.data !== seenGroups ||
      sharePermissionsQuery.data !== seenPermissions)
  ) {
    setSeenUsers(detailUsersQuery.data);
    setSeenGroups(detailGroupsQuery.data);
    setSeenPermissions(sharePermissionsQuery.data);
    const userAccess = new Map(permissions.users.map((entry) => [entry.userId, entry.access]));
    const groupAccess = new Map(permissions.groups.map((entry) => [entry.groupId, entry.access]));
    setPermissionRows([
      ...userList.map((user) => ({
        kind: "user" as const,
        id: user.id,
        label: user.username,
        access: userAccess.get(user.id) ?? "none",
      })),
      ...groupList.map((group) => ({
        kind: "group" as const,
        id: group.id,
        label: group.name,
        access: groupAccess.get(group.id) ?? "none",
      })),
    ]);
  }

  const permissionsLoadError =
    detailUsersQuery.error ?? detailGroupsQuery.error ?? sharePermissionsQuery.error;
  const permissionsLoading =
    detailUsersQuery.loading || detailGroupsQuery.loading || sharePermissionsQuery.loading;

  // Every share-detail mutation goes through useApiMutation, which already
  // treats openapi-fetch's non-OK-empty-body response the same as an
  // `error` (issue #271 finding 3) and never lets a rejected request or an
  // abort escape as an unhandled rejection. Allocation, cache mode, SMB and
  // NFS each keep a separate mutation instance so one section's spinner
  // never lights up another's Save button.
  const allocationMutation = useApiMutation<Partial<components["schemas"]["UpdateShareRequest"]>, Share>({
    mutationFn: async (patch) => {
      const result = await patchShare(name, patch);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });
  const cacheModeMutation = useApiMutation<Partial<components["schemas"]["UpdateShareRequest"]>, Share>({
    mutationFn: async (patch) => {
      const result = await patchShare(name, patch);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });
  const relocateMutation = useApiMutation<"cache" | "array", components["schemas"]["Job"] | undefined>({
    mutationFn: async (direction) => {
      const result = await postShareRelocate(name, direction);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });
  const smbMutation = useApiMutation<Partial<components["schemas"]["UpdateShareRequest"]>, Share>({
    mutationFn: async (patch) => {
      const result = await patchShare(name, patch);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });
  const nfsMutation = useApiMutation<Partial<components["schemas"]["UpdateShareRequest"]>, Share>({
    mutationFn: async (patch) => {
      const result = await patchShare(name, patch);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });
  const permissionsMutation = useApiMutation<components["schemas"]["UpdateSharePermissionsRequest"], unknown>({
    mutationFn: async (body) => {
      const result = await putSharePermissions(name, body);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });
  const browseMutation = useApiMutation<string, { entries: ShareBrowseEntry[] }>({
    mutationFn: (path) => getShareBrowse(name, path),
  });
  const deleteFileMutation = useApiMutation<string, unknown>({
    mutationFn: (path) => deleteShareFile(name, path),
  });
  const removeMutation = useApiMutation<undefined, unknown>({
    mutationFn: async () => {
      const result = await deleteShare(name);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });
  const deleteDataMutation = useApiMutation<string, unknown>({
    mutationFn: (confirmation) => postShareDataDelete(name, confirmation),
  });

  async function handleSaveAllocation(): Promise<void> {
    setSaveError(null);
    const result = await allocationMutation.mutate({ createPolicy: createPolicyDraft });
    if (result.ok) {
      if (result.data) setShare(result.data);
    } else if (!result.aborted) {
      setSaveError(result.error);
    }
  }

  async function handleSaveCacheModeOnly(): Promise<void> {
    setCacheDialogError(null);
    const result = await cacheModeMutation.mutate({ cacheMode: cacheModeDraft });
    if (result.ok) {
      if (result.data) setShare(result.data);
      setCacheConfirmOpen(false);
    } else if (!result.aborted) {
      setCacheDialogError(result.error);
    }
  }

  async function handleSaveCacheModeAndRelocate(): Promise<void> {
    if (!cacheModeFrom) {
      return;
    }
    const direction = shareRelocationDirection(cacheModeFrom, cacheModeDraft);
    if (!direction) {
      return;
    }
    setCacheDialogError(null);
    const patchResult = await cacheModeMutation.mutate({ cacheMode: cacheModeDraft });
    if (!patchResult.ok) {
      if (!patchResult.aborted) setCacheDialogError(patchResult.error);
      return;
    }
    if (patchResult.data) setShare(patchResult.data);

    const relocateResult = await relocateMutation.mutate(direction);
    if (!relocateResult.ok) {
      if (!relocateResult.aborted) setCacheDialogError(relocateResult.error);
      return;
    }
    const relocatedJob = relocateResult.data;
    if (relocatedJob) {
      toastManager.add({
        type: "success",
        title: t("cache.relocation.queuedTitle"),
        description: t("cache.relocation.queuedDescription"),
        actionProps: {
          children: t("cache.relocation.viewJob"),
          onClick: () => navigate(jobDetailPath(relocatedJob.id)),
        },
      });
    }
    setCacheConfirmOpen(false);
  }

  async function handleSaveSmb(): Promise<void> {
    if (!smbDraft) return;
    setSaveError(null);
    const result = await smbMutation.mutate({ smb: smbDraft });
    if (result.ok) {
      if (result.data) setShare(result.data);
    } else if (!result.aborted) {
      setSaveError(result.error);
    }
  }

  async function handleSaveNfs(): Promise<void> {
    if (!nfsDraft) return;
    setSaveError(null);
    const result = await nfsMutation.mutate({ nfs: nfsDraft });
    if (result.ok) {
      if (result.data) setShare(result.data);
    } else if (!result.aborted) {
      setSaveError(result.error);
    }
  }

  async function handleSavePermissions(): Promise<void> {
    if (!permissionRows || permissionsLoading || permissionsLoadError) return;
    setSaveError(null);
    const result = await permissionsMutation.mutate({
      users: permissionRows
        .filter((row) => row.kind === "user" && row.access !== "none")
        .map((row) => ({ userId: row.id, access: row.access })),
      groups: permissionRows
        .filter((row) => row.kind === "group" && row.access !== "none")
        .map((row) => ({ groupId: row.id, access: row.access })),
    });
    if (!result.ok && !result.aborted) {
      setSaveError(result.error);
    }
  }

  async function loadBrowse(path: string): Promise<void> {
    setBrowseError(null);
    const result = await browseMutation.mutate(path);
    if (!result.ok) {
      if (!result.aborted) setBrowseError(result.error);
      return;
    }
    setBrowsePath(path);
    setBrowseEntries(result.data?.entries ?? []);
  }

  async function handleDeleteFile(): Promise<void> {
    if (deleteFileTarget === null) return;
    const result = await deleteFileMutation.mutate(deleteFileTarget);
    if (!result.ok) {
      if (!result.aborted) setBrowseError(result.error);
      setDeleteFileTarget(null);
      return;
    }
    setDeleteFileTarget(null);
    await loadBrowse(browsePath);
  }

  async function handleRemoveDefinition(): Promise<void> {
    setRemoveDialogError(null);
    const result = await removeMutation.mutate(undefined);
    if (!result.ok) {
      if (!result.aborted) setRemoveDialogError(result.error);
      return;
    }
    navigate(PATHS.shares);
  }

  async function handleDeleteData(): Promise<void> {
    setDeleteDataError(null);
    const result = await deleteDataMutation.mutate(deleteDataConfirm);
    if (!result.ok) {
      if (!result.aborted) setDeleteDataError(result.error);
      return;
    }
    setDeleteDataOpen(false);
    setDeleteDataConfirm("");
  }

  const cacheDialogBusy = cacheModeMutation.pending || relocateMutation.pending;

  const includedInParity = share ? share.cacheMode !== "cache-only" : false;
  const cacheRelocationDirection = cacheModeFrom ? shareRelocationDirection(cacheModeFrom, cacheModeDraft) : null;

  const permissionColumns: DataTableColumn<PermissionRow>[] = useMemo(
    () => [
      {
        id: "label",
        header: t("shares.detail.smb.permissions.columns.name"),
        cell: (row) => (
          <span className="flex items-center gap-2">
            {row.label}
            <StatusBadge tone="outline">
              {row.kind === "user" ? t("shares.detail.smb.permissions.user") : t("shares.detail.smb.permissions.group")}
            </StatusBadge>
          </span>
        ),
      },
      {
        id: "access",
        header: t("shares.detail.smb.permissions.columns.access"),
        cell: (row) => {
          const fieldName = `access-${row.kind}-${row.id}`;
          return (
            <SegmentedChoice
              name={fieldName}
              value={row.access}
              onChange={(value) =>
                setPermissionRows((current) =>
                  (current ?? []).map((entry) =>
                    entry.kind === row.kind && entry.id === row.id
                      ? { ...entry, access: value as ShareAccessLevel }
                      : entry,
                  ),
                )
              }
              options={shareAccessOptions(t)}
            />
          );
        },
      },
    ],
    [t],
  );

  const error = shareQuery.error;

  if (!share && !error) {
    return <LoadingBlock />;
  }

  if (!share) {
    return <Banner tone="error" title={error ?? t("shares.detail.notFound", { name })} />;
  }

  const smbStanza = buildSmbStanza(share.name, share.path, smbDraft ?? share.smb);
  const nfsLine = buildNfsExportLine(share.path, nfsDraft ?? share.nfs);
  const browseSegments = browsePath.split("/").filter((segment) => segment.length > 0);

  const usageColumns: DataTableColumn<ShareDiskUsage>[] = [
    {
      id: USAGE_COLUMN_DISK,
      header: t("shares.detail.general.perDisk.columns.disk"),
      cell: (row) => row.disk,
    },
    {
      id: USAGE_COLUMN_BYTES,
      header: t("shares.detail.general.perDisk.columns.bytes"),
      cell: (row) => formatBytes(row.bytes),
    },
  ];

  const browseColumns: DataTableColumn<ShareBrowseEntry>[] = [
    {
      id: BROWSE_COLUMN_NAME,
      header: t("shares.detail.browse.columns.name"),
      cell: (entry) =>
        entry.type === "directory" ? (
          <button
            type="button"
            className="font-medium text-foreground hover:underline"
            onClick={() => void loadBrowse(browsePath ? `${browsePath}/${entry.name}` : entry.name)}
          >
            {entry.name}
          </button>
        ) : (
          entry.name
        ),
    },
    {
      id: BROWSE_COLUMN_SIZE,
      header: t("shares.detail.browse.columns.size"),
      cell: (entry) =>
        entry.type === "file" && entry.sizeBytes !== undefined ? formatBytes(entry.sizeBytes) : UNKNOWN_VALUE,
    },
    {
      id: BROWSE_COLUMN_DISK,
      header: t("shares.detail.browse.columns.disk"),
      cell: (entry) => entry.disk || UNKNOWN_VALUE,
    },
    {
      id: BROWSE_COLUMN_ACTIONS,
      header: t("shares.detail.browse.columns.actions"),
      cell: (entry) => (
        <Button
          variant="ghost"
          size="icon-sm"
          aria-label={t("shares.detail.browse.deleteAction", { name: entry.name })}
          onClick={() => setDeleteFileTarget(browsePath ? `${browsePath}/${entry.name}` : entry.name)}
        >
          <Trash2 aria-hidden="true" />
        </Button>
      ),
    },
  ];

  const dangerActions: DangerZoneAction[] = [
    {
      id: DANGER_ACTION_REMOVE,
      title: t("shares.detail.danger.removeTitle"),
      description: t("shares.detail.danger.removeDescription"),
      actionLabel: t("shares.detail.danger.removeAction"),
      onAction: () => setRemoveOpen(true),
    },
    {
      id: DANGER_ACTION_DELETE_DATA,
      title: t("shares.detail.danger.deleteDataTitle"),
      description: t("shares.detail.danger.deleteDataDescription"),
      actionLabel: t("shares.detail.danger.deleteDataAction"),
      onAction: () => {
        setDeleteDataConfirm("");
        setDeleteDataError(null);
        setDeleteDataOpen(true);
      },
    },
  ];

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{share.name}</h1>
        <p className="text-muted-foreground">{share.path}</p>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      {saveError ? <Banner tone="error" title={saveError} /> : null}

      <DetailTabs
        defaultTab={initialTab}
        tabs={[
          {
            id: SHARE_TAB_GENERAL,
            label: t("shares.detail.tabs.general"),
            content: (
              <Card>
                <CardHeader>
                  <CardTitle>{t("shares.detail.general.title")}</CardTitle>
                </CardHeader>
                <CardPanel className="grid gap-3 text-sm sm:grid-cols-2">
                  <p>{t("shares.detail.general.name", { name: share.name })}</p>
                  <p>{t("shares.detail.general.path", { path: share.path })}</p>
                  <p>
                    {share.usage
                      ? t("shares.detail.general.sizeUsed", {
                          size: formatBytes(share.usage.totalBytes),
                          date: new Date(share.usage.asOf).toLocaleString(),
                        })
                      : t("shares.detail.general.notYetSyncedTitle")}
                  </p>
                  <p className="flex items-center gap-2">
                    {t("shares.detail.general.includedInParity")}
                    <StatusBadge tone={includedInParity ? "success" : "outline"}>
                      {includedInParity ? t("common.yes") : t("common.no")}
                    </StatusBadge>
                  </p>
                  <div className="sm:col-span-2">
                    {share.usage ? (
                      share.usage.perDisk.length > 0 ? (
                        <div className="flex flex-col gap-2">
                          <h3 className="text-sm font-medium">{t("shares.detail.general.perDiskTitle")}</h3>
                          <DataTable columns={usageColumns} rows={share.usage.perDisk} getRowKey={(row) => row.disk} />
                        </div>
                      ) : (
                        <InlineNote description={t("shares.detail.general.perDiskEmpty")} />
                      )
                    ) : (
                      <InlineNote description={t("shares.detail.general.notYetSyncedDescription")} />
                    )}
                  </div>
                </CardPanel>
              </Card>
            ),
          },
          {
            id: SHARE_TAB_ALLOCATION,
            label: t("shares.detail.tabs.allocation"),
            content: (
              <Card>
                <CardHeader>
                  <CardTitle>{t("shares.detail.allocation.title")}</CardTitle>
                </CardHeader>
                <CardPanel>
                  <ChoiceCards
                    name={CREATE_POLICY_FIELD_NAME}
                    value={createPolicyDraft}
                    onChange={(value) => setCreatePolicyDraft(value as ArrayCreatePolicy)}
                    options={CREATE_POLICIES.map((policy) => ({
                      value: policy,
                      title: (
                        <PlainTerm label={t(`shares.detail.allocation.policies.${policy}.label`)} term={policy} />
                      ),
                      description: t(`shares.detail.allocation.policies.${policy}.description`),
                    }))}
                  />
                </CardPanel>
                <CardFooter className="justify-end border-t">
                  <Button
                    loading={allocationMutation.pending}
                    disabled={createPolicyDraft === share.createPolicy}
                    onClick={() => void handleSaveAllocation()}
                  >
                    {t("shares.detail.save")}
                  </Button>
                </CardFooter>
              </Card>
            ),
          },
          {
            id: SHARE_TAB_CACHE,
            label: t("shares.detail.tabs.cache"),
            content: (
              <Card>
                <CardHeader>
                  <CardTitle>{t("shares.detail.cache.title")}</CardTitle>
                </CardHeader>
                <CardPanel>
                  <ChoiceCards
                    name={CACHE_MODE_FIELD_NAME}
                    value={cacheModeDraft}
                    onChange={(value) => setCacheModeDraft(value as ShareCacheMode)}
                    options={CACHE_MODES.map((mode) => ({
                      value: mode,
                      title: t(`shares.cacheModes.${mode}.label`),
                      description: t(`shares.cacheModes.${mode}.description`),
                    }))}
                  />
                </CardPanel>
                <CardFooter className="justify-end border-t">
                  <Button
                    disabled={cacheModeDraft === share.cacheMode}
                    onClick={() => {
                      setCacheModeFrom(share.cacheMode);
                      setCacheConfirmOpen(true);
                    }}
                  >
                    {t("shares.detail.save")}
                  </Button>
                </CardFooter>
              </Card>
            ),
          },
          {
            id: SHARE_TAB_SMB,
            label: t("shares.detail.tabs.smb"),
            content: smbDraft ? (
              <div className="flex flex-col gap-4">
                <Card>
                  <CardHeader>
                    <CardTitle>{t("shares.detail.smb.title")}</CardTitle>
                  </CardHeader>
                  <CardPanel className="flex flex-col gap-4">
                    <SettingSwitch
                      label={t("shares.detail.smb.enabled")}
                      checked={smbDraft.enabled}
                      onCheckedChange={(enabled) => setSmbDraft({ ...smbDraft, enabled })}
                    />
                    <SettingSwitch
                      label={t("shares.detail.smb.guest")}
                      checked={smbDraft.guest}
                      onCheckedChange={(guest) => setSmbDraft({ ...smbDraft, guest })}
                    />
                    <SettingSwitch
                      label={t("shares.detail.smb.readOnly")}
                      checked={smbDraft.readOnly}
                      onCheckedChange={(readOnly) => setSmbDraft({ ...smbDraft, readOnly })}
                    />
                    <SettingSwitch
                      label={t("shares.detail.smb.browseable")}
                      checked={smbDraft.browseable}
                      onCheckedChange={(browseable) => setSmbDraft({ ...smbDraft, browseable })}
                    />
                    <SettingSwitch
                      label={t("shares.detail.smb.recycle")}
                      checked={smbDraft.recycle}
                      onCheckedChange={(recycle) => setSmbDraft({ ...smbDraft, recycle })}
                    />
                    <SettingSwitch
                      label={t("shares.detail.smb.timeMachine")}
                      checked={smbDraft.timeMachine}
                      onCheckedChange={(timeMachine) =>
                        setSmbDraft({
                          ...smbDraft,
                          timeMachine,
                          timeMachineMaxSize: timeMachine ? (smbDraft.timeMachineMaxSize ?? DEFAULT_TIME_MACHINE_MAX_SIZE) : null,
                        })
                      }
                    />
                    {smbDraft.timeMachine ? (
                      <NumberUnit
                        value={Number.parseInt(smbDraft.timeMachineMaxSize ?? "500", 10) || 0}
                        onChange={(value) => setSmbDraft({ ...smbDraft, timeMachineMaxSize: `${value}G` })}
                        unit={t("shares.detail.smb.timeMachineUnit")}
                        min={1}
                      />
                    ) : null}
                    <CodeView title={t("shares.detail.smb.exportPreview")} code={smbStanza} />
                  </CardPanel>
                  <CardFooter className="justify-end border-t">
                    <Button loading={smbMutation.pending} onClick={() => void handleSaveSmb()}>
                      {t("shares.detail.save")}
                    </Button>
                  </CardFooter>
                </Card>

                <Card>
                  <CardHeader>
                    <CardTitle>{t("shares.detail.smb.permissions.title")}</CardTitle>
                  </CardHeader>
                  <CardPanel>
                    {permissionsLoadError ? (
                      <Banner tone="error" title={permissionsLoadError} />
                    ) : permissionRows ? (
                      <DataTable columns={permissionColumns} rows={permissionRows} getRowKey={(row) => `${row.kind}-${row.id}`} />
                    ) : (
                      <LoadingBlock rows={2} />
                    )}
                  </CardPanel>
                  <CardFooter className="justify-end border-t">
                    <Button
                      loading={permissionsMutation.pending}
                      disabled={!permissionRows || permissionsLoading || Boolean(permissionsLoadError)}
                      onClick={() => void handleSavePermissions()}
                    >
                      {t("shares.detail.save")}
                    </Button>
                  </CardFooter>
                </Card>
              </div>
            ) : (
              <LoadingBlock />
            ),
          },
          {
            id: SHARE_TAB_NFS,
            label: t("shares.detail.tabs.nfs"),
            content: nfsDraft ? (
              <Card>
                <CardHeader>
                  <CardTitle>{t("shares.detail.nfs.title")}</CardTitle>
                </CardHeader>
                <CardPanel className="flex flex-col gap-4">
                  <SettingSwitch
                    label={t("shares.detail.nfs.enabled")}
                    checked={nfsDraft.enabled}
                    onCheckedChange={(enabled) => setNfsDraft({ ...nfsDraft, enabled })}
                  />
                  <div className="flex flex-col gap-2">
                    <p className="font-medium text-sm">{t("shares.detail.nfs.hosts")}</p>
                    <ListInput
                      value={nfsDraft.hosts}
                      onChange={(hosts) => setNfsDraft({ ...nfsDraft, hosts })}
                      placeholder={t("shares.detail.nfs.hostsPlaceholder")}
                    />
                  </div>
                  <div className="flex flex-col gap-2">
                    <p className="font-medium text-sm">{t("shares.detail.nfs.squash")}</p>
                    <Select
                      value={nfsDraft.squash}
                      onValueChange={(value) => value && setNfsDraft({ ...nfsDraft, squash: value as ShareNFS["squash"] })}
                    >
                      <SelectTrigger className="w-full max-w-64">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectPopup>
                        {SQUASH_OPTIONS.map((option) => (
                          <SelectItem key={option} value={option}>
                            {t(`shares.detail.nfs.squashOptions.${option}`)}
                          </SelectItem>
                        ))}
                      </SelectPopup>
                    </Select>
                  </div>
                  <div className="flex flex-col gap-2">
                    <p className="font-medium text-sm">{t("shares.detail.nfs.exportPreview")}</p>
                    <CopyValue value={nfsLine} label={t("shares.detail.nfs.exportPreview")} />
                  </div>
                </CardPanel>
                <CardFooter className="justify-end border-t">
                  <Button loading={nfsMutation.pending} onClick={() => void handleSaveNfs()}>
                    {t("shares.detail.save")}
                  </Button>
                </CardFooter>
              </Card>
            ) : (
              <LoadingBlock />
            ),
          },
          {
            id: SHARE_TAB_BROWSE,
            label: t("shares.detail.tabs.browse"),
            content: (
              <div className="flex flex-col gap-4">
                <InlineNote
                  title={t("shares.detail.browse.wakeWarningTitle")}
                  description={t("shares.detail.browse.wakeWarningDescription")}
                />
                {!browseAcknowledged ? (
                  <Button
                    onClick={() => {
                      setBrowseAcknowledged(true);
                      void loadBrowse("");
                    }}
                  >
                    {t("shares.detail.browse.start")}
                  </Button>
                ) : (
                  <>
                    <Breadcrumb>
                      <BreadcrumbList>
                        <BreadcrumbItem>
                          {browsePath === "" ? (
                            <BreadcrumbPage>
                              <Home aria-hidden="true" className="size-4" />
                            </BreadcrumbPage>
                          ) : (
                            <BreadcrumbButton onClick={() => void loadBrowse("")}>
                              <Home aria-hidden="true" className="size-4" />
                            </BreadcrumbButton>
                          )}
                        </BreadcrumbItem>
                        {browseSegments.map((segment, index) => {
                          const segmentPath = browseSegments.slice(0, index + 1).join("/");
                          const isLast = index === browseSegments.length - 1;
                          return (
                            <span key={segmentPath} className="flex items-center gap-1.5">
                              <BreadcrumbSeparator />
                              <BreadcrumbItem>
                                {isLast ? (
                                  <BreadcrumbPage>{segment}</BreadcrumbPage>
                                ) : (
                                  <BreadcrumbButton onClick={() => void loadBrowse(segmentPath)}>
                                    {segment}
                                  </BreadcrumbButton>
                                )}
                              </BreadcrumbItem>
                            </span>
                          );
                        })}
                      </BreadcrumbList>
                    </Breadcrumb>
                    {browseError ? <Banner tone="error" title={browseError} /> : null}
                    {browseMutation.pending || !browseEntries ? (
                      <LoadingBlock />
                    ) : (
                      <DataTable columns={browseColumns} rows={browseEntries} getRowKey={(entry) => entry.name} />
                    )}
                  </>
                )}
              </div>
            ),
          },
          {
            id: SHARE_TAB_DANGER,
            label: t("shares.detail.tabs.danger"),
            content: <DangerZone actions={dangerActions} />,
          },
        ]}
      />

      <FormOverlay
        open={cacheConfirmOpen}
        onOpenChange={(open) => {
          // Same rule as the disabled Cancel button: Escape and a backdrop
          // click both come through here as onOpenChange(false), so a busy
          // handler must ignore them too, not just the button (#375).
          if (!open && cacheDialogBusy) {
            return;
          }
          setCacheConfirmOpen(open);
          if (!open) {
            setCacheDialogError(null);
            setCacheModeFrom(null);
          }
        }}
        title={t("shares.detail.cache.confirmTitle")}
        description={t("shares.detail.cache.confirmDescription")}
        footer={
          <div className="flex flex-wrap justify-end gap-2">
            <Button variant="outline" disabled={cacheDialogBusy} onClick={() => setCacheConfirmOpen(false)}>
              {t("confirm.cancel")}
            </Button>
            <Button loading={cacheDialogBusy} onClick={() => void handleSaveCacheModeOnly()}>
              {t("shares.detail.cache.changeModeOnly")}
            </Button>
            {cacheRelocationDirection ? (
              <Button loading={cacheDialogBusy} onClick={() => void handleSaveCacheModeAndRelocate()}>
                {t("shares.detail.cache.relocateNow")}
              </Button>
            ) : null}
          </div>
        }
      >
        {cacheDialogError ? <Banner tone="error" title={cacheDialogError} /> : null}
        {share && cacheModeFrom ? (
          <p className="text-sm text-muted-foreground">
            {t("shares.detail.cache.modeChangeSummary", {
              share: share.name,
              from: t(`shares.cacheModes.${cacheModeFrom}.label`),
              to: t(`shares.cacheModes.${cacheModeDraft}.label`),
            })}
          </p>
        ) : null}
      </FormOverlay>

      <ConfirmDialog
        open={deleteFileTarget !== null}
        onOpenChange={(open) => {
          // Same rule as the cache-mode, delete-data and remove-definition
          // dialogs: Escape and a backdrop click both come through here as
          // onOpenChange(false), so a busy handler must ignore them too
          // (#375, #376).
          if (!open && deleteFileMutation.pending) {
            return;
          }
          if (!open) setDeleteFileTarget(null);
        }}
        title={t("shares.detail.browse.deleteConfirmTitle")}
        description={
          deleteFileTarget !== null
            ? t("shares.detail.browse.deleteConfirmDescription", { path: deleteFileTarget })
            : undefined
        }
        destructive
        loading={deleteFileMutation.pending}
        onConfirm={() => void handleDeleteFile()}
      />

      <ConfirmDialog
        open={removeOpen}
        onOpenChange={(open) => {
          // Same rule as the cache-mode and delete-data dialogs: Escape and
          // a backdrop click both come through here as onOpenChange(false),
          // so a busy handler must ignore them too (#375, #376).
          if (!open && removeMutation.pending) {
            return;
          }
          setRemoveOpen(open);
          if (!open) {
            setRemoveDialogError(null);
          }
        }}
        title={t("shares.detail.danger.removeConfirmTitle")}
        description={t("shares.detail.danger.removeConfirmDescription")}
        error={removeDialogError}
        destructive
        loading={removeMutation.pending}
        onConfirm={() => void handleRemoveDefinition()}
      />

      <FormOverlay
        open={deleteDataOpen}
        onOpenChange={(open) => {
          // Same rule as the cache-mode and remove-definition dialogs:
          // Escape and a backdrop click both come through here as
          // onOpenChange(false), so a busy handler must ignore them too
          // (#375, #376).
          if (!open && deleteDataMutation.pending) {
            return;
          }
          if (!open) {
            setDeleteDataOpen(false);
            setDeleteDataConfirm("");
            setDeleteDataError(null);
          }
        }}
        title={t("shares.detail.danger.deleteDataConfirmTitle")}
        description={t("shares.detail.danger.deleteDataConfirmDescription")}
        footer={
          <Button
            variant="destructive"
            disabled={deleteDataConfirm !== share.name || deleteDataMutation.pending}
            loading={deleteDataMutation.pending}
            onClick={() => void handleDeleteData()}
          >
            {t("shares.detail.danger.deleteDataAction")}
          </Button>
        }
      >
        {deleteDataError ? <Banner tone="error" title={deleteDataError} /> : null}
        <TypedConfirm
          phrase={share.name}
          value={deleteDataConfirm}
          onChange={setDeleteDataConfirm}
          title={t("shares.detail.danger.deleteDataConfirmTitle")}
          description={t("shares.detail.danger.deleteDataConfirmDescription")}
          items={[t("shares.detail.danger.deleteDataItem", { name: share.name })]}
        />
      </FormOverlay>
    </div>
  );
}
