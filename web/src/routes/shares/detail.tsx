import { Home, Trash2 } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

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
import { hoservaClient, type components } from "@/lib/api/client";
import { buildNfsExportLine, buildSmbStanza } from "@/routes/shares/config-preview";
import { formatBytes } from "@/routes/storage-setup/config-preview";

type Share = components["schemas"]["Share"];
type ShareSMB = components["schemas"]["ShareSMB"];
type ShareNFS = components["schemas"]["ShareNFS"];
type ShareCacheMode = components["schemas"]["ShareCacheMode"];
type ArrayCreatePolicy = components["schemas"]["ArrayCreatePolicy"];
type ShareAccessLevel = components["schemas"]["ShareAccessLevel"];
type ShareBrowseEntry = components["schemas"]["ShareBrowseEntry"];

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

  const [share, setShare] = useState<Share | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);

  const [createPolicyDraft, setCreatePolicyDraft] = useState<ArrayCreatePolicy>("mspmfs");
  const [savingAllocation, setSavingAllocation] = useState(false);

  const [cacheModeDraft, setCacheModeDraft] = useState<ShareCacheMode>("cache-then-move");
  const [cacheConfirmOpen, setCacheConfirmOpen] = useState(false);
  const [savingCache, setSavingCache] = useState(false);

  const [smbDraft, setSmbDraft] = useState<ShareSMB | null>(null);
  const [savingSmb, setSavingSmb] = useState(false);

  const [nfsDraft, setNfsDraft] = useState<ShareNFS | null>(null);
  const [savingNfs, setSavingNfs] = useState(false);

  const [permissionRows, setPermissionRows] = useState<PermissionRow[] | null>(null);
  const [savingPermissions, setSavingPermissions] = useState(false);

  const [browseAcknowledged, setBrowseAcknowledged] = useState(false);
  const [browsePath, setBrowsePath] = useState("");
  const [browseEntries, setBrowseEntries] = useState<ShareBrowseEntry[] | null>(null);
  const [browseLoading, setBrowseLoading] = useState(false);
  const [browseError, setBrowseError] = useState<string | null>(null);
  const [deleteFileTarget, setDeleteFileTarget] = useState<string | null>(null);
  const [deleteFileBusy, setDeleteFileBusy] = useState(false);

  const [removeOpen, setRemoveOpen] = useState(false);
  const [removeBusy, setRemoveBusy] = useState(false);
  const [deleteDataOpen, setDeleteDataOpen] = useState(false);
  const [deleteDataConfirm, setDeleteDataConfirm] = useState("");
  const [deleteDataBusy, setDeleteDataBusy] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    Promise.all([
      hoservaClient.GET("/shares/{name}", { params: { path: { name } }, signal: controller.signal }),
      hoservaClient.GET("/users", { signal: controller.signal }),
      hoservaClient.GET("/user-groups", { signal: controller.signal }),
      hoservaClient.GET("/shares/{name}/permissions", {
        params: { path: { name } },
        signal: controller.signal,
      }),
    ])
      .then(([shareResult, usersResult, groupsResult, permissionsResult]) => {
        if (controller.signal.aborted) return;
        if (shareResult.error) {
          setError(shareResult.error.message);
          return;
        }
        const loaded = shareResult.data;
        if (!loaded) return;
        setShare(loaded);
        setCreatePolicyDraft(loaded.createPolicy);
        setCacheModeDraft(loaded.cacheMode);
        setSmbDraft(loaded.smb);
        setNfsDraft(loaded.nfs);

        const userList = usersResult.data?.users ?? [];
        const groupList = groupsResult.data?.groups ?? [];

        const permissions = permissionsResult.data;
        const userAccess = new Map(permissions?.users.map((entry) => [entry.userId, entry.access]));
        const groupAccess = new Map(permissions?.groups.map((entry) => [entry.groupId, entry.access]));
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
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : String(err));
        }
      });
    return () => controller.abort();
  }, [name]);

  async function saveShare(patch: Partial<components["schemas"]["UpdateShareRequest"]>): Promise<boolean> {
    setSaveError(null);
    const { data, error: apiError } = await hoservaClient.PATCH("/shares/{name}", {
      params: { path: { name } },
      body: patch,
    });
    if (apiError) {
      setSaveError(apiError.message);
      return false;
    }
    if (data) {
      setShare(data);
    }
    return true;
  }

  async function handleSaveAllocation(): Promise<void> {
    setSavingAllocation(true);
    try {
      await saveShare({ createPolicy: createPolicyDraft });
    } finally {
      setSavingAllocation(false);
    }
  }

  async function handleConfirmCacheMode(): Promise<void> {
    setSavingCache(true);
    try {
      const ok = await saveShare({ cacheMode: cacheModeDraft });
      if (ok) {
        setCacheConfirmOpen(false);
      }
    } finally {
      setSavingCache(false);
    }
  }

  async function handleSaveSmb(): Promise<void> {
    if (!smbDraft) return;
    setSavingSmb(true);
    try {
      await saveShare({ smb: smbDraft });
    } finally {
      setSavingSmb(false);
    }
  }

  async function handleSaveNfs(): Promise<void> {
    if (!nfsDraft) return;
    setSavingNfs(true);
    try {
      await saveShare({ nfs: nfsDraft });
    } finally {
      setSavingNfs(false);
    }
  }

  async function handleSavePermissions(): Promise<void> {
    if (!permissionRows) return;
    setSavingPermissions(true);
    setSaveError(null);
    try {
      const { error: apiError } = await hoservaClient.PUT("/shares/{name}/permissions", {
        params: { path: { name } },
        body: {
          users: permissionRows
            .filter((row) => row.kind === "user" && row.access !== "none")
            .map((row) => ({ userId: row.id, access: row.access })),
          groups: permissionRows
            .filter((row) => row.kind === "group" && row.access !== "none")
            .map((row) => ({ groupId: row.id, access: row.access })),
        },
      });
      if (apiError) {
        setSaveError(apiError.message);
      }
    } finally {
      setSavingPermissions(false);
    }
  }

  async function loadBrowse(path: string): Promise<void> {
    setBrowseLoading(true);
    setBrowseError(null);
    try {
      const { data, error: apiError } = await hoservaClient.GET("/shares/{name}/browse", {
        params: { path: { name }, query: { path } },
      });
      if (apiError) {
        setBrowseError(apiError.message);
        return;
      }
      setBrowsePath(path);
      setBrowseEntries(data?.entries ?? []);
    } finally {
      setBrowseLoading(false);
    }
  }

  async function handleDeleteFile(): Promise<void> {
    if (deleteFileTarget === null) return;
    setDeleteFileBusy(true);
    try {
      const { error: apiError } = await hoservaClient.DELETE("/shares/{name}/browse", {
        params: { path: { name }, query: { path: deleteFileTarget } },
        body: { confirm: true },
      });
      if (apiError) {
        setBrowseError(apiError.message);
        setDeleteFileTarget(null);
        return;
      }
      setDeleteFileTarget(null);
      await loadBrowse(browsePath);
    } finally {
      setDeleteFileBusy(false);
    }
  }

  async function handleRemoveDefinition(): Promise<void> {
    setRemoveBusy(true);
    try {
      const { error: apiError } = await hoservaClient.DELETE("/shares/{name}", {
        params: { path: { name } },
        body: { confirm: true },
      });
      if (apiError) {
        setSaveError(apiError.message);
        return;
      }
      navigate(PATHS.shares);
    } finally {
      setRemoveBusy(false);
    }
  }

  async function handleDeleteData(): Promise<void> {
    setDeleteDataBusy(true);
    try {
      const { error: apiError } = await hoservaClient.POST("/shares/{name}/data/delete", {
        params: { path: { name } },
        body: { confirmation: deleteDataConfirm },
      });
      if (apiError) {
        setSaveError(apiError.message);
        return;
      }
      setDeleteDataOpen(false);
      setDeleteDataConfirm("");
    } finally {
      setDeleteDataBusy(false);
    }
  }

  const includedInParity = share ? share.cacheMode !== "cache-only" : false;

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

  if (!share && !error) {
    return <LoadingBlock />;
  }

  if (!share) {
    return <Banner tone="error" title={error ?? t("shares.detail.notFound", { name })} />;
  }

  const smbStanza = buildSmbStanza(share.name, share.path, smbDraft ?? share.smb);
  const nfsLine = buildNfsExportLine(share.path, nfsDraft ?? share.nfs);
  const browseSegments = browsePath.split("/").filter((segment) => segment.length > 0);

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
                  <p>{t("shares.detail.general.sizeUsed", { size: "—" })}</p>
                  <p className="flex items-center gap-2">
                    {t("shares.detail.general.includedInParity")}
                    <StatusBadge tone={includedInParity ? "success" : "outline"}>
                      {includedInParity ? t("common.yes") : t("common.no")}
                    </StatusBadge>
                  </p>
                  <div className="sm:col-span-2">
                    <InlineNote description={t("shares.detail.general.perDiskUnavailable")} />
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
                    loading={savingAllocation}
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
                  <Button disabled={cacheModeDraft === share.cacheMode} onClick={() => setCacheConfirmOpen(true)}>
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
                    <Button loading={savingSmb} onClick={() => void handleSaveSmb()}>
                      {t("shares.detail.save")}
                    </Button>
                  </CardFooter>
                </Card>

                <Card>
                  <CardHeader>
                    <CardTitle>{t("shares.detail.smb.permissions.title")}</CardTitle>
                  </CardHeader>
                  <CardPanel>
                    {permissionRows ? (
                      <DataTable columns={permissionColumns} rows={permissionRows} getRowKey={(row) => `${row.kind}-${row.id}`} />
                    ) : (
                      <LoadingBlock rows={2} />
                    )}
                  </CardPanel>
                  <CardFooter className="justify-end border-t">
                    <Button loading={savingPermissions} onClick={() => void handleSavePermissions()}>
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
                  <Button loading={savingNfs} onClick={() => void handleSaveNfs()}>
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
                    {browseLoading || !browseEntries ? (
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

      <ConfirmDialog
        open={cacheConfirmOpen}
        onOpenChange={setCacheConfirmOpen}
        title={t("shares.detail.cache.confirmTitle")}
        description={t("shares.detail.cache.confirmDescription")}
        loading={savingCache}
        onConfirm={() => void handleConfirmCacheMode()}
      />

      <ConfirmDialog
        open={deleteFileTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteFileTarget(null);
        }}
        title={t("shares.detail.browse.deleteConfirmTitle")}
        description={
          deleteFileTarget !== null
            ? t("shares.detail.browse.deleteConfirmDescription", { path: deleteFileTarget })
            : undefined
        }
        destructive
        loading={deleteFileBusy}
        onConfirm={() => void handleDeleteFile()}
      />

      <ConfirmDialog
        open={removeOpen}
        onOpenChange={setRemoveOpen}
        title={t("shares.detail.danger.removeConfirmTitle")}
        description={t("shares.detail.danger.removeConfirmDescription")}
        destructive
        loading={removeBusy}
        onConfirm={() => void handleRemoveDefinition()}
      />

      <FormOverlay
        open={deleteDataOpen}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteDataOpen(false);
            setDeleteDataConfirm("");
          }
        }}
        title={t("shares.detail.danger.deleteDataConfirmTitle")}
        description={t("shares.detail.danger.deleteDataConfirmDescription")}
        footer={
          <Button
            variant="destructive"
            disabled={deleteDataConfirm !== share.name || deleteDataBusy}
            loading={deleteDataBusy}
            onClick={() => void handleDeleteData()}
          >
            {t("shares.detail.danger.deleteDataAction")}
          </Button>
        }
      >
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
