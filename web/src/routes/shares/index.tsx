import { FolderOpen, MoreHorizontal, Plus } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { TypedConfirm } from "@/components/patterns/typed-confirm";
import { Button } from "@/components/ui/button";
import { Field, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import { SHARE_TAB_BROWSE } from "@/hooks/share-detail-tabs";
import { shareDetailPath } from "@/hooks/paths";
import type { components } from "@/lib/api/client";
import { isApiError } from "@/lib/api/errors";
import { deleteShare, getSharePermissions, getShares, postShare } from "@/lib/api/operations";
import { parseClientResult } from "@/lib/api/request";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import { formatBytes } from "@/routes/storage-setup/config-preview";

type Share = components["schemas"]["Share"];
type SharePermissionsResult = components["schemas"]["SharePermissionsResult"];

function shareBrowsePath(name: string): string {
  return `${shareDetailPath(name)}?tab=${SHARE_TAB_BROWSE}`;
}

function shareMutationError(err: unknown, t: (key: string, options?: Record<string, unknown>) => string): string {
  if (isApiError(err) && err.code === "maintenance_mode") {
    return t("shares.errors.maintenanceMode");
  }
  if (isApiError(err)) {
    return err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return String(err);
}

function accessSummary(permissions: SharePermissionsResult | undefined, t: (key: string, options?: Record<string, unknown>) => string): string {
  if (!permissions) {
    return "—";
  }
  const withAccess =
    permissions.users.filter((entry) => entry.access !== "none").length +
    permissions.groups.filter((entry) => entry.access !== "none").length;
  if (withAccess === 0) {
    return t("shares.list.noExplicitAccess");
  }
  return t("shares.list.accessCount", { count: withAccess });
}

export function SharesPage(): React.ReactElement {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const sharesQuery = useApiQuery<{ shares: Share[] }>({
    queryKey: "shares",
    queryFn: (signal) => getShares(signal),
    fallbackError: t("shares.list.loadFailed"),
  });
  const shares = sharesQuery.data?.shares ?? null;
  const [permissionsByShare, setPermissionsByShare] = useState<Record<string, SharePermissionsResult>>({});
  const [createOpen, setCreateOpen] = useState(false);
  const [createName, setCreateName] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<Share | null>(null);
  const [deleteConfirm, setDeleteConfirm] = useState("");

  const createMutation = useApiMutation<string, Share>({
    mutationFn: async (name) => {
      const result = await postShare(name);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });
  const deleteMutation = useApiMutation<string, unknown>({
    mutationFn: async (name) => {
      const result = await deleteShare(name);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });

  useEffect(() => {
    const list = sharesQuery.data?.shares;
    if (!list) {
      return;
    }
    const controller = new AbortController();
    Promise.all(
      list.map((share) =>
        getSharePermissions(share.name, controller.signal).then((entry) => [share.name, entry] as const),
      ),
    )
      .then((entries) => {
        if (controller.signal.aborted) return;
        const next: Record<string, SharePermissionsResult> = {};
        for (const [name, entry] of entries) {
          const data = parseClientResult(entry).data;
          if (data) {
            next[name] = data;
          }
        }
        setPermissionsByShare(next);
      })
      .catch(() => undefined);
    return () => controller.abort();
  }, [sharesQuery.data]);

  async function handleCreate(): Promise<void> {
    const name = createName.trim();
    if (name.length === 0) {
      return;
    }
    const result = await createMutation.mutate(name);
    if (!result.ok) {
      return;
    }
    setCreateOpen(false);
    setCreateName("");
    await sharesQuery.refresh();
  }

  async function handleDelete(): Promise<void> {
    if (!deleteTarget) return;
    const result = await deleteMutation.mutate(deleteTarget.name);
    if (!result.ok) {
      return;
    }
    setDeleteTarget(null);
    setDeleteConfirm("");
    await sharesQuery.refresh();
  }

  const columns: DataTableColumn<Share>[] = [
    {
      id: "name",
      header: t("shares.list.columns.name"),
      cell: (share) => (
        <Link to={shareDetailPath(share.name)} className="font-medium">
          {share.name}
        </Link>
      ),
    },
    { id: "path", header: t("shares.list.columns.path"), cell: (share) => share.path },
    {
      id: "sizeUsed",
      header: t("shares.list.columns.sizeUsed"),
      cell: (share) => (share.usage ? formatBytes(share.usage.totalBytes) : t("shares.list.notYetSynced")),
    },
    {
      id: "cacheMode",
      header: t("shares.list.columns.cacheMode"),
      cell: (share) => t(`shares.cacheModes.${share.cacheMode}.label`),
    },
    {
      id: "smb",
      header: t("shares.list.columns.smb"),
      cell: (share) => (
        <StatusBadge tone={share.smb.enabled ? "success" : "outline"}>
          {share.smb.enabled ? t("common.yes") : t("common.no")}
        </StatusBadge>
      ),
    },
    {
      id: "nfs",
      header: t("shares.list.columns.nfs"),
      cell: (share) => (
        <StatusBadge tone={share.nfs.enabled ? "success" : "outline"}>
          {share.nfs.enabled ? t("common.yes") : t("common.no")}
        </StatusBadge>
      ),
    },
    {
      id: "access",
      header: t("shares.list.columns.access"),
      cell: (share) => accessSummary(permissionsByShare[share.name], t),
    },
    {
      id: "actions",
      header: t("shares.list.columns.actions"),
      cell: (share) => (
        <Menu>
          <MenuTrigger
            render={
              <Button
                size="icon-sm"
                variant="ghost"
                aria-label={t("shares.list.actionsMenu", { name: share.name })}
              >
                <MoreHorizontal />
              </Button>
            }
          />
          <MenuContent>
            <MenuItem onClick={() => navigate(shareDetailPath(share.name))}>
              {t("shares.list.edit")}
            </MenuItem>
            <MenuItem onClick={() => navigate(shareBrowsePath(share.name))}>
              {t("shares.list.browse")}
            </MenuItem>
            <MenuItem
              className="text-destructive-foreground"
              onClick={() => {
                setDeleteConfirm("");
                deleteMutation.reset();
                setDeleteTarget(share);
              }}
            >
              {t("shares.list.delete")}
            </MenuItem>
          </MenuContent>
        </Menu>
      ),
    },
  ];

  const error = sharesQuery.error;
  const createError = createMutation.error;
  const createBusy = createMutation.pending;
  const deleteError = deleteMutation.error;
  const deleteBusy = deleteMutation.pending;

  if (shares === null && !error) {
    return <LoadingBlock />;
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h1 className="text-2xl font-semibold font-heading">{t("nav.shares")}</h1>
          <p className="text-muted-foreground">{t("shares.list.description")}</p>
        </div>
        <Button
          onClick={() => {
            createMutation.reset();
            setCreateOpen(true);
          }}
        >
          <Plus aria-hidden="true" />
          {t("shares.list.create")}
        </Button>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      {shares && shares.length === 0 ? (
        <EmptyState
          icon={FolderOpen}
          title={t("shares.list.empty.title")}
          description={t("shares.list.empty.description")}
          action={
            <Button
              onClick={() => {
                createMutation.reset();
                setCreateOpen(true);
              }}
            >
              <Plus aria-hidden="true" />
              {t("shares.list.create")}
            </Button>
          }
        />
      ) : (
        <DataTable columns={columns} rows={shares ?? []} getRowKey={(share) => share.name} />
      )}

      <FormOverlay
        open={createOpen}
        onOpenChange={(open) => {
          setCreateOpen(open);
          if (!open) {
            createMutation.reset();
            setCreateName("");
          }
        }}
        title={t("shares.list.create")}
        description={t("shares.create.description")}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              {t("confirm.cancel")}
            </Button>
            <Button loading={createBusy} disabled={createName.trim().length === 0} onClick={() => void handleCreate()}>
              {t("shares.list.create")}
            </Button>
          </div>
        }
      >
        {createError ? <Banner tone="error" title={createError} /> : null}
        <Field>
          <FieldLabel>{t("shares.create.name")}</FieldLabel>
          <Input value={createName} onChange={(event) => setCreateName(event.target.value)} />
        </Field>
      </FormOverlay>

      <FormOverlay
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteTarget(null);
            setDeleteConfirm("");
            deleteMutation.reset();
          }
        }}
        title={t("shares.list.deleteTitle")}
        description={t("shares.list.deleteDescription")}
        footer={
          <Button
            variant="destructive"
            disabled={!deleteTarget || deleteConfirm !== deleteTarget.name || deleteBusy}
            loading={deleteBusy}
            onClick={() => void handleDelete()}
          >
            {t("shares.list.delete")}
          </Button>
        }
      >
        {deleteError ? <Banner tone="error" title={deleteError} /> : null}
        {deleteTarget ? (
          <TypedConfirm
            phrase={deleteTarget.name}
            value={deleteConfirm}
            onChange={setDeleteConfirm}
            title={t("shares.list.deleteConfirmTitle")}
            description={t("shares.list.deleteConfirmDescription")}
            items={[t("shares.list.deleteItem", { name: deleteTarget.name })]}
          />
        ) : null}
      </FormOverlay>
    </div>
  );
}
