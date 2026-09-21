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
import { hoservaClient, type components } from "@/lib/api/client";
import { formatBytes } from "@/routes/storage-setup/config-preview";

type Share = components["schemas"]["Share"];
type SharePermissionsResult = components["schemas"]["SharePermissionsResult"];

function shareBrowsePath(name: string): string {
  return `${shareDetailPath(name)}?tab=${SHARE_TAB_BROWSE}`;
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
  const [shares, setShares] = useState<Share[] | null>(null);
  const [permissionsByShare, setPermissionsByShare] = useState<Record<string, SharePermissionsResult>>({});
  const [error, setError] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [createName, setCreateName] = useState("");
  const [createBusy, setCreateBusy] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Share | null>(null);
  const [deleteConfirm, setDeleteConfirm] = useState("");
  const [deleteBusy, setDeleteBusy] = useState(false);

  function load(signal?: AbortSignal): void {
    hoservaClient
      .GET("/shares", { signal })
      .then(({ data, error: apiError }) => {
        if (signal?.aborted) return;
        if (apiError) {
          setError(apiError.message);
          return;
        }
        setError(null);
        const list = data?.shares ?? [];
        setShares(list);
        return Promise.all(
          list.map((share) =>
            hoservaClient
              .GET("/shares/{name}/permissions", { params: { path: { name: share.name } }, signal })
              .then((result) => [share.name, result] as const),
          ),
        ).then((entries) => {
          if (signal?.aborted) return;
          const next: Record<string, SharePermissionsResult> = {};
          for (const [name, result] of entries) {
            if (result.data) {
              next[name] = result.data;
            }
          }
          setPermissionsByShare(next);
        });
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

  async function handleCreate(): Promise<void> {
    const name = createName.trim();
    if (name.length === 0) {
      return;
    }
    setCreateBusy(true);
    try {
      const { error: apiError } = await hoservaClient.POST("/shares", { body: { name } });
      if (apiError) {
        setError(apiError.message);
        return;
      }
      setCreateOpen(false);
      setCreateName("");
      load();
    } finally {
      setCreateBusy(false);
    }
  }

  async function handleDelete(): Promise<void> {
    if (!deleteTarget) return;
    setDeleteBusy(true);
    try {
      const { error: apiError } = await hoservaClient.DELETE("/shares/{name}", {
        params: { path: { name: deleteTarget.name } },
        body: { confirm: true },
      });
      if (apiError) {
        setError(apiError.message);
        return;
      }
      setDeleteTarget(null);
      setDeleteConfirm("");
      load();
    } finally {
      setDeleteBusy(false);
    }
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
        <Button onClick={() => setCreateOpen(true)}>
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
            <Button onClick={() => setCreateOpen(true)}>
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
        onOpenChange={setCreateOpen}
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
