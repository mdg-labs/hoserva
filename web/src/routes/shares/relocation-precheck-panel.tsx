import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { stateTone } from "@/routes/apps/containers";
import type { RelocationPrecheckState } from "@/routes/shares/relocation-precheck";

const OPEN_FILES_SHOWN = 10;

// RelocationPrecheckPanel lists, inside the relocation dialog, the containers that use the
// share and the files held open, with a Stop button per active container (doc 09 §2).
export function RelocationPrecheckPanel({ precheck }: { precheck: RelocationPrecheckState }): React.ReactElement | null {
  const { t } = useTranslation();

  if (!precheck.enabled) {
    return null;
  }
  if (precheck.error) {
    return (
      <Banner
        tone="error"
        title={t("shares.detail.cache.precheck.loadError")}
        description={precheck.error}
        action={
          <Button size="xs" variant="outline" onClick={precheck.retry}>
            {t("shares.detail.cache.precheck.retry")}
          </Button>
        }
      />
    );
  }
  if (precheck.loading || !precheck.data) {
    return <LoadingBlock rows={2} />;
  }

  const { containers, openPaths, dockerAvailable } = precheck.data;
  const anyActive = containers.some((container) => container.active);

  return (
    <div className="flex flex-col gap-3">
      {!dockerAvailable ? <InlineNote description={t("shares.detail.cache.precheck.noDocker")} /> : null}
      {containers.length > 0 ? (
        <>
          <Banner
            tone={anyActive ? "warning" : "info"}
            title={t(
              anyActive
                ? "shares.detail.cache.precheck.containersTitle"
                : "shares.detail.cache.precheck.stoppedContainersTitle",
            )}
            description={t(
              anyActive
                ? "shares.detail.cache.precheck.containersDescription"
                : "shares.detail.cache.precheck.stoppedContainersDescription",
            )}
          />
          <ul className="flex flex-col gap-2">
            {containers.map((container) => (
              <li key={container.id} className="flex items-start justify-between gap-3 rounded-md border p-3 text-sm">
                <div className="flex min-w-0 flex-col gap-1">
                  <span className="flex items-center gap-2 font-medium">
                    {container.name}
                    <StatusBadge tone={stateTone(container.state)}>{t(`apps.state.${container.state}`)}</StatusBadge>
                  </span>
                  <span className="break-all font-mono text-xs text-muted-foreground">{container.mounts.join(", ")}</span>
                </div>
                {container.active ? (
                  <Button
                    size="xs"
                    variant="outline"
                    aria-label={t("shares.detail.cache.precheck.stopLabel", { name: container.name })}
                    loading={precheck.stoppingId === container.id}
                    disabled={precheck.busy}
                    onClick={() => void precheck.stopContainer(container.id)}
                  >
                    {t("shares.detail.cache.precheck.stop")}
                  </Button>
                ) : null}
              </li>
            ))}
          </ul>
        </>
      ) : null}
      {precheck.stopError ? <Banner tone="error" title={precheck.stopError} /> : null}
      {openPaths.length > 0 ? (
        <div className="flex flex-col gap-1 text-sm">
          <p className="font-medium">{t("shares.detail.cache.precheck.openFilesTitle")}</p>
          <p className="text-muted-foreground">{t("shares.detail.cache.precheck.openFilesDescription")}</p>
          <ul className="font-mono text-xs">
            {openPaths.slice(0, OPEN_FILES_SHOWN).map((path) => (
              <li key={path} className="break-all">
                {path}
              </li>
            ))}
          </ul>
          {openPaths.length > OPEN_FILES_SHOWN ? (
            <p className="text-muted-foreground">
              {t("shares.detail.cache.precheck.moreOpenFiles", { count: openPaths.length - OPEN_FILES_SHOWN })}
            </p>
          ) : null}
        </div>
      ) : null}
      {dockerAvailable && containers.length === 0 && openPaths.length === 0 ? (
        <InlineNote description={t("shares.detail.cache.precheck.nothingInUse")} />
      ) : null}
      {anyActive ? <p className="text-sm text-muted-foreground">{t("shares.detail.cache.precheck.relocateBlocked")}</p> : null}
    </div>
  );
}
