import { ArrowLeft, ExternalLink, ImageOff, PackageSearch } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { CodeView } from "@/components/patterns/code-view";
import { EmptyState } from "@/components/patterns/empty-state";
import { LoadingBlock } from "@/components/patterns/loading";
import { PrivilegeSummary } from "@/components/patterns/privilege-summary";
import { SourceBadge } from "@/components/patterns/source-badge";
import { TemplateIcon } from "@/components/patterns/template-icon";
import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { ScrollArea } from "@/components/ui/scroll-area";
import type { components } from "@/lib/api/client";
import { catalogScreenshotPath, getCatalogTemplate } from "@/lib/api/operations";
import { useApiQuery } from "@/lib/api/use-api-query";
import { installPath } from "@/routes/apps/catalog-filter";
import { sourceNoteKey } from "@/routes/apps/source-note";

type CatalogTemplate = components["schemas"]["CatalogTemplate"];

const CATALOG_PATH = "/apps/catalog";
const NOT_FOUND_CODE = "template_not_found";

// The docs address comes from the template, which a user-added source wrote,
// so only a web address becomes a link.
function webAddress(address: string): string | null {
  try {
    const url = new URL(address);
    return url.protocol === "https:" || url.protocol === "http:" ? url.href : null;
  } catch {
    return null;
  }
}

// The project, support and donate addresses come from the template as well,
// and only an https address with a host and no embedded credentials becomes a
// link: the daemon refuses anything else, and this holds the page to the same
// rule for an answer from anywhere else.
function isControl(character: string): boolean {
  const code = character.charCodeAt(0);
  return code < 0x20 || (code >= 0x7f && code <= 0x9f);
}

function httpsAddress(address: string | undefined): string | null {
  if (address === undefined || /\s/.test(address) || [...address].some(isControl)) {
    return null;
  }
  try {
    const url = new URL(address);
    if (url.protocol !== "https:" || url.hostname === "" || url.username !== "" || url.password !== "") {
      return null;
    }
    return url.href;
  } catch {
    return null;
  }
}

function Screenshot({
  id,
  index,
  title,
}: {
  id: string;
  index: number;
  title: string;
}): React.ReactElement {
  const { t } = useTranslation();
  const [failed, setFailed] = useState(false);
  if (failed) {
    return (
      <div className="flex h-48 w-72 flex-col items-center justify-center gap-2 rounded-md border border-dashed text-muted-foreground text-sm">
        <ImageOff aria-hidden="true" className="size-5" />
        <span>{t("apps.catalogDetail.screenshots.unavailable")}</span>
      </div>
    );
  }
  return (
    <img
      src={catalogScreenshotPath(id, index)}
      alt={t("apps.catalogDetail.screenshots.alt", { number: index + 1, title })}
      loading="lazy"
      onError={() => setFailed(true)}
      className="h-48 w-auto rounded-md border bg-muted object-contain"
    />
  );
}

function BackLink(): React.ReactElement {
  const { t } = useTranslation();
  return (
    <Link to={CATALOG_PATH} className={buttonVariants({ variant: "ghost", size: "sm" })}>
      <ArrowLeft aria-hidden="true" />
      {t("apps.catalogDetail.back")}
    </Link>
  );
}

export function CatalogDetailPage(): React.ReactElement {
  const { t } = useTranslation();
  const { appId = "" } = useParams();

  const query = useApiQuery<CatalogTemplate | "missing">({
    queryKey: ["catalog-template", appId],
    queryFn: async (signal) => {
      const result = await getCatalogTemplate(appId, signal);
      if (result.error?.code === NOT_FOUND_CODE) {
        return { data: "missing", response: { ok: true } };
      }
      return result;
    },
    fallbackError: t("apps.catalogDetail.loadFailed"),
  });

  const template = query.data;

  if (template === null) {
    return (
      <div className="flex flex-col gap-4">
        <div>
          <BackLink />
        </div>
        {query.error ? (
          <Banner
            tone="error"
            title={t("apps.catalogDetail.loadFailed")}
            description={query.error}
            action={
              <Button size="xs" variant="outline" onClick={() => void query.refresh()}>
                {t("apps.installed.retry")}
              </Button>
            }
          />
        ) : (
          <LoadingBlock />
        )}
      </div>
    );
  }

  if (template === "missing") {
    return (
      <div className="flex flex-col gap-4">
        <div>
          <BackLink />
        </div>
        <EmptyState
          icon={PackageSearch}
          title={t("apps.catalogDetail.missing.title", { id: appId })}
          description={t("apps.catalogDetail.missing.description")}
        />
      </div>
    );
  }

  const docs = webAddress(template.docs);
  const links = [
    { key: "project", href: httpsAddress(template.links?.project) },
    { key: "support", href: httpsAddress(template.links?.support) },
    { key: "donate", href: httpsAddress(template.links?.donate) },
  ].flatMap((link) => (link.href === null ? [] : [{ key: link.key, href: link.href }]));
  const screenshotCount = template.screenshotCount > 0 ? template.screenshotCount : 0;
  const sourceNote = t(sourceNoteKey(template.sourceKind, template.signed), { source: template.source });

  return (
    <div className="flex flex-col gap-4">
      <div>
        <BackLink />
      </div>

      {query.error ? <Banner tone="warning" title={query.error} /> : null}

      <Card>
        <CardPanel className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
          <div className="flex min-w-0 items-start gap-4">
            <TemplateIcon id={template.id} title={template.title} className="size-16 shrink-0" />
            <div className="flex min-w-0 flex-col gap-2">
              <h1 className="font-heading text-2xl font-semibold">{template.title}</h1>
              <div className="flex flex-wrap items-center gap-2">
                <SourceBadge kind={template.sourceKind} signed={template.signed} />
                <span className="text-muted-foreground text-sm">
                  {t("apps.catalogDetail.revision", { revision: template.revision })}
                </span>
              </div>
              {template.maintainer ? (
                <p className="text-sm">
                  {t("apps.catalogDetail.maintainer", { maintainer: template.maintainer })}
                </p>
              ) : null}
              <p className="text-muted-foreground text-sm">{sourceNote}</p>
              <div className="flex flex-wrap gap-1">
                {template.categories.map((category) => (
                  <Badge key={category} variant="outline">
                    {t(`apps.catalog.categories.${category}`, { defaultValue: category })}
                  </Badge>
                ))}
              </div>
            </div>
          </div>
          <div className="flex flex-wrap gap-2">
            {docs ? (
              <a
                href={docs}
                target="_blank"
                rel="noopener noreferrer"
                className={buttonVariants({ variant: "outline" })}
              >
                <ExternalLink aria-hidden="true" />
                {t("apps.catalogDetail.docs")}
              </a>
            ) : null}
            {links.map((link) => (
              <a
                key={link.key}
                href={link.href}
                target="_blank"
                rel="noopener noreferrer"
                className={buttonVariants({ variant: "outline" })}
              >
                <ExternalLink aria-hidden="true" />
                {t(`apps.catalogDetail.links.${link.key}`)}
              </a>
            ))}
            <Link to={installPath(template.id)} className={buttonVariants()}>
              {t("apps.catalogDetail.install")}
            </Link>
          </div>
        </CardPanel>
      </Card>

      {template.description ? (
        <Card>
          <CardHeader>
            <CardTitle>{t("apps.catalogDetail.about.title")}</CardTitle>
          </CardHeader>
          <CardPanel>
            <p className="whitespace-pre-line text-sm">{template.description}</p>
          </CardPanel>
        </Card>
      ) : null}

      {screenshotCount > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>{t("apps.catalogDetail.screenshots.title")}</CardTitle>
          </CardHeader>
          <CardPanel>
            <ScrollArea className="h-auto w-full">
              <ul className="flex gap-3 pb-3">
                {Array.from({ length: screenshotCount }, (_, index) => (
                  <li key={index} className="shrink-0">
                    <Screenshot key={`${template.id}/${index}`} id={template.id} index={index} title={template.title} />
                  </li>
                ))}
              </ul>
            </ScrollArea>
          </CardPanel>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>{t("apps.catalogDetail.privileges.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-3">
          <p className="text-muted-foreground text-sm">{t("apps.catalogDetail.privileges.description")}</p>
          <PrivilegeSummary privileges={template.privileges} />
        </CardPanel>
      </Card>

      <CodeView title={t("apps.catalogDetail.rawTemplate")} code={template.compose} />
    </div>
  );
}
