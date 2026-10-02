// The install wizard (doc 03 §5.4). Everything about an install is resolved by
// the API: `previewTemplateInstall` is asked again after each pause in typing,
// and what it answers (the ports it moved, the paths it defaulted, the Compose
// file and the privilege summary) is what the page shows. The page decides
// nothing about the inputs itself.
import { ArrowLeft, PackageSearch } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { CodeView } from "@/components/patterns/code-view";
import { EmptyState } from "@/components/patterns/empty-state";
import { LoadingBlock } from "@/components/patterns/loading";
import { NumberUnit } from "@/components/patterns/number-unit";
import { PathPicker } from "@/components/patterns/path-picker";
import { PrivilegeSummary } from "@/components/patterns/privilege-summary";
import { SecretInput } from "@/components/patterns/secret-input";
import { SourceBadge } from "@/components/patterns/source-badge";
import { TemplateIcon } from "@/components/patterns/template-icon";
import { TimezoneSelect } from "@/components/patterns/timezone";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs";
import type { components } from "@/lib/api/client";
import { getCatalogTemplate, getDockerNetworks, installTemplate, previewTemplateInstall, startStack } from "@/lib/api/operations";
import { apiErrorMessage, isAbortError, parseClientResult } from "@/lib/api/request";
import { useApiQuery } from "@/lib/api/use-api-query";
import { AdvancedSettingsFields, type NetworkChoices } from "@/routes/apps/advanced-settings";
import { catalogDetailPath } from "@/routes/apps/catalog-filter";
import {
  buildInstallRequest,
  hasInputError,
  hasPortConflict,
  inputOf,
  isAdvancedField,
  malformedLimits,
  missingNetwork,
  NO_ADVANCED,
  portConflict,
  secretNames,
  type AdvancedField,
  type AdvancedSettings,
  type TemplateInput,
  type TemplateInstallPlan,
} from "@/routes/apps/install-form";
import { InstallRun, type StartState } from "@/routes/apps/install-run";
import { PlanWarnings } from "@/routes/apps/plan-warnings";
import { sourceNoteKey } from "@/routes/apps/source-note";

type CatalogTemplate = components["schemas"]["CatalogTemplate"];
type DockerNetworks = components["schemas"]["ListDockerNetworksOK"];
type TemplateInstallResult = components["schemas"]["TemplateInstallResult"];

const NOT_FOUND_CODE = "template_not_found";
const NAME_ERROR_CODES = ["invalid_stack_name", "stack_exists", "stack_dir_exists"];
const NETWORK_ERROR_CODES = ["network_missing"];
const PREVIEW_DEBOUNCE_MS = 400;
const NO_DEVICE = "__none__";
const NAME_FIELD = "stack-name";
const BASIC_TAB = "basic";
const ADVANCED_TAB = "advanced";
const CATALOG_PATH = "/apps/catalog";
const NEW_PASSWORD = "new-password";
const OFF = "off";

type Failure = { code: string | undefined; message: string; input: string | null };
type Preview = { plan: TemplateInstallPlan | null; settledKey: string | null; failure: Failure | null };
type Run = { result: TemplateInstallResult; start: StartState };

function BackLink({ id }: { id: string }): React.ReactElement {
  const { t } = useTranslation();
  return (
    <Link to={catalogDetailPath(id)} className={buttonVariants({ variant: "ghost", size: "sm" })}>
      <ArrowLeft aria-hidden="true" />
      {t("apps.install.back")}
    </Link>
  );
}

function fieldId(name: string): string {
  return `install-${name}`;
}

function shownValue(input: TemplateInput, values: Record<string, string>): string {
  const typed = values[input.name];
  if (typed !== undefined) {
    return typed;
  }
  if (input.kind === "port") {
    return input.requestedValue ?? input.value ?? "";
  }
  return input.value ?? "";
}

export function InputField({
  input,
  value,
  optional,
  error,
  onChange,
}: {
  input: TemplateInput;
  value: string;
  optional: boolean;
  error: string | null;
  onChange: (value: string) => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const id = fieldId(input.name);
  const conflict = portConflict(input);
  const portError = conflict ? t("apps.install.port.taken", { port: conflict.requested }) : null;
  const shownError = error ?? portError;
  const invalid = shownError !== null;

  let control: React.ReactNode;
  switch (input.kind) {
    case "port":
      control = (
        <NumberUnit
          id={id}
          value={Number.parseInt(value, 10) || 0}
          onChange={(next) => onChange(String(next))}
          unit={t("apps.install.port.unit")}
          min={1}
          max={65535}
        />
      );
      break;
    case "path":
      control = (
        <PathPicker
          id={id}
          value={value}
          onChange={onChange}
          suggestions={input.suggestions}
          aria-invalid={invalid}
        />
      );
      break;
    case "secret":
      control = (
        <SecretInput
          id={id}
          value={value}
          onChange={onChange}
          showStrength
          placeholder={t("apps.install.secret.placeholder")}
          autoComplete={NEW_PASSWORD}
          aria-invalid={invalid}
        />
      );
      break;
    case "timezone":
      control = <TimezoneSelect value={value} onChange={onChange} />;
      break;
    case "device": {
      const devices = input.suggestions ?? [];
      control =
        devices.length === 0 ? (
          <p className="text-muted-foreground text-sm">{t("apps.install.device.none")}</p>
        ) : (
          <Select
            value={value === "" ? NO_DEVICE : value}
            onValueChange={(next) => onChange(next === null || next === NO_DEVICE ? "" : next)}
            items={[
              { value: NO_DEVICE, label: t("apps.install.device.useNone") },
              ...devices.map((device) => ({ value: device, label: device })),
            ]}
          >
            <SelectTrigger id={id}>
              <SelectValue />
            </SelectTrigger>
            <SelectPopup>
              <SelectItem value={NO_DEVICE}>{t("apps.install.device.useNone")}</SelectItem>
              {devices.map((device) => (
                <SelectItem key={device} value={device}>
                  {device}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
        );
      break;
    }
    default:
      control = (
        <Input
          id={id}
          value={value}
          placeholder={input.value}
          onChange={(event) => onChange(event.target.value)}
          aria-invalid={invalid}
          autoComplete={OFF}
        />
      );
  }

  return (
    <Field invalid={invalid} className="w-full">
      <FieldLabel htmlFor={id}>
        {input.label ?? input.name}
        {optional ? (
          <span className="text-muted-foreground text-xs font-normal">{t("apps.install.optional")}</span>
        ) : null}
      </FieldLabel>
      {control}
      {input.description ? <FieldDescription>{input.description}</FieldDescription> : null}
      {input.kind === "secret" ? <FieldDescription>{t("apps.install.secret.help")}</FieldDescription> : null}
      {shownError !== null ? (
        <FieldError match role="alert">
          {shownError}
        </FieldError>
      ) : null}
      {conflict ? (
        <Button type="button" size="xs" variant="outline" onClick={() => onChange(conflict.suggested)}>
          {t("apps.install.port.useNext", { port: conflict.suggested })}
        </Button>
      ) : null}
    </Field>
  );
}

function InstallForm({ template }: { template: CatalogTemplate }): React.ReactElement {
  const { t } = useTranslation();
  const [name, setName] = useState("");
  const [values, setValues] = useState<Record<string, string>>({});
  const [reloads, setReloads] = useState(0);
  const [advanced, setAdvanced] = useState<AdvancedSettings>(NO_ADVANCED);
  const [preview, setPreview] = useState<Preview>({ plan: null, settledKey: null, failure: null });
  const [installFailure, setInstallFailure] = useState<(Failure & { key: string }) | null>(null);
  const [installing, setInstalling] = useState(false);
  const [run, setRun] = useState<Run | null>(null);
  const installingRef = useRef(false);
  const startingRef = useRef(false);
  const hasPlanRef = useRef(false);

  const omitKey = [...secretNames(preview.plan?.inputs ?? [])].join("\n");
  const omit = useMemo(() => new Set(omitKey === "" ? [] : omitKey.split("\n")), [omitKey]);
  const previewBody = useMemo(() => buildInstallRequest(name, values, omit, advanced), [name, values, omit, advanced]);
  const previewKey = useMemo(() => JSON.stringify(previewBody), [previewBody]);
  const installBody = useMemo(() => buildInstallRequest(name, values, new Set(), advanced), [name, values, advanced]);
  const installKey = useMemo(() => JSON.stringify(installBody), [installBody]);

  const networkQuery = useApiQuery<DockerNetworks>({
    queryKey: ["docker-networks"],
    queryFn: (signal) => getDockerNetworks(signal),
    fallbackError: t("apps.install.advanced.network.listFailed"),
  });
  let networks: NetworkChoices;
  if (networkQuery.error !== null) {
    networks = { kind: "failed", message: networkQuery.error };
  } else if (networkQuery.data === null) {
    networks = { kind: "loading" };
  } else if (!networkQuery.data.available) {
    networks = { kind: "failed", message: networkQuery.data.message ?? t("apps.install.advanced.network.unreachable") };
  } else {
    networks = { kind: "ready", networks: networkQuery.data.networks };
  }

  useEffect(() => {
    const controller = new AbortController();
    const { signal } = controller;
    const timer = window.setTimeout(
      () => {
        void (async () => {
          let next: Preview;
          try {
            const body = JSON.parse(previewKey) as typeof previewBody;
            const result = await previewTemplateInstall(template.id, body, signal);
            if (signal.aborted) {
              return;
            }
            const parsed = parseClientResult(result, t("apps.install.previewFailed"));
            next =
              parsed.error !== null || parsed.data === undefined
                ? {
                    plan: null,
                    settledKey: previewKey,
                    failure: {
                      code: result.error?.code,
                      message: parsed.error ?? t("apps.install.previewFailed"),
                      input: inputOf(result.error),
                    },
                  }
                : { plan: parsed.data, settledKey: previewKey, failure: null };
          } catch (err: unknown) {
            if (isAbortError(err, signal)) {
              return;
            }
            next = {
              plan: null,
              settledKey: previewKey,
              failure: {
                code: undefined,
                message: err instanceof Error ? err.message : t("apps.install.previewFailed"),
                input: null,
              },
            };
          }
          const settled = next;
          if (settled.plan !== null) {
            hasPlanRef.current = true;
          }
          setPreview((prev) => ({ plan: settled.plan ?? prev.plan, settledKey: settled.settledKey, failure: settled.failure }));
        })();
      },
      hasPlanRef.current ? PREVIEW_DEBOUNCE_MS : 0,
    );
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [template.id, previewKey, reloads, t]);

  const plan = preview.plan;
  const pending = preview.settledKey !== previewKey;
  const failure = pending ? null : preview.failure;
  const shownInstallFailure = installFailure !== null && installFailure.key === installKey ? installFailure : null;
  const conflicted = plan !== null && hasPortConflict(plan);
  const inputNames = (plan?.inputs ?? []).map((input) => input.name);
  const missing = plan === null ? null : missingNetwork(plan);
  const malformed = malformedLimits(advanced);

  const errors = new Map<string, string>();
  const advancedErrors: Partial<Record<AdvancedField, string>> = {};
  const unattributed: Failure[] = [];
  for (const item of [failure, shownInstallFailure]) {
    if (item === null) {
      continue;
    }
    if (item.code !== undefined && NAME_ERROR_CODES.includes(item.code)) {
      errors.set(NAME_FIELD, item.message);
      unattributed.push(item);
    } else if (item.code !== undefined && NETWORK_ERROR_CODES.includes(item.code)) {
      advancedErrors.networkMode = item.message;
      unattributed.push(item);
    } else if (item.input !== null && inputNames.includes(item.input)) {
      errors.set(item.input, item.message);
    } else if (item.input !== null && isAdvancedField(item.input)) {
      advancedErrors[item.input] = item.message;
      unattributed.push(item);
    } else {
      unattributed.push(item);
    }
  }

  async function startRun(stackName: string): Promise<void> {
    if (startingRef.current) {
      return;
    }
    startingRef.current = true;
    const fail = (message: string): void => {
      setRun((prev) => (prev ? { ...prev, start: { kind: "notStarted", message } } : prev));
    };
    setRun((prev) => (prev ? { ...prev, start: { kind: "starting" } } : prev));
    try {
      const result = await startStack(stackName);
      const parsed = parseClientResult(result, t("apps.install.run.startRequestFailed"));
      if (parsed.error !== null || parsed.data === undefined) {
        fail(parsed.error ?? t("apps.install.run.startRequestFailed"));
      } else {
        const job = parsed.data;
        setRun((prev) => (prev ? { ...prev, start: { kind: "queued", job } } : prev));
      }
    } catch (err: unknown) {
      fail(err instanceof Error ? err.message : apiErrorMessage(undefined, t("apps.install.run.startRequestFailed")));
    } finally {
      startingRef.current = false;
    }
  }

  async function handleInstall(): Promise<void> {
    if (installingRef.current || plan === null) {
      return;
    }
    installingRef.current = true;
    setInstalling(true);
    setInstallFailure(null);
    const key = installKey;
    try {
      const result = await installTemplate(template.id, installBody);
      const parsed = parseClientResult(result, t("apps.install.installFailed"));
      if (parsed.error !== null || parsed.data === undefined) {
        setInstallFailure({
          key,
          code: result.error?.code,
          message: parsed.error ?? t("apps.install.installFailed"),
          input: inputOf(result.error),
        });
        return;
      }
      const installed = parsed.data;
      setRun({ result: installed, start: { kind: "starting" } });
      await startRun(installed.stack.name);
    } catch (err: unknown) {
      setInstallFailure({
        key,
        code: undefined,
        message: `${err instanceof Error ? err.message : t("apps.install.installFailed")} ${t("apps.install.installUnconfirmed")}`,
        input: null,
      });
    } finally {
      installingRef.current = false;
      setInstalling(false);
    }
  }

  const header = (
    <Card>
      <CardPanel className="flex min-w-0 items-start gap-4">
        <TemplateIcon id={template.id} title={template.title} className="size-16 shrink-0" />
        <div className="flex min-w-0 flex-col gap-2">
          <h1 className="font-heading text-2xl font-semibold">{t("apps.install.title", { title: template.title })}</h1>
          <div className="flex flex-wrap items-center gap-2">
            <SourceBadge kind={template.sourceKind} signed={template.signed} />
            <span className="text-muted-foreground text-sm">
              {t("apps.catalogDetail.revision", { revision: template.revision })}
            </span>
          </div>
          <p className="text-muted-foreground text-sm">
            {t(sourceNoteKey(template.sourceKind, template.signed), { source: template.source })}
          </p>
        </div>
      </CardPanel>
    </Card>
  );

  if (run !== null) {
    return (
      <div className="flex flex-col gap-4">
        {header}
        <InstallRun result={run.result} start={run.start} onRetryStart={() => void startRun(run.result.stack.name)} />
      </div>
    );
  }

  if (plan === null) {
    return (
      <div className="flex flex-col gap-4">
        <div>
          <BackLink id={template.id} />
        </div>
        {header}
        {failure ? (
          <Banner
            tone="error"
            title={t("apps.install.previewFailed")}
            description={failure.message}
            action={
              <Button
                size="xs"
                variant="outline"
                onClick={() => {
                  setPreview({ plan: null, settledKey: null, failure: null });
                  setReloads((n) => n + 1);
                }}
              >
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

  const setValue = (inputName: string, value: string): void => {
    setValues((prev) => ({ ...prev, [inputName]: value }));
  };
  const inputsIncomplete = hasInputError(plan);
  const installBlocked =
    pending || failure !== null || conflicted || inputsIncomplete || missing !== null || malformed.length > 0 || installing;

  return (
    <div className="flex flex-col gap-4">
      <div>
        <BackLink id={template.id} />
      </div>
      {header}

      {unattributed.map((item) => (
        <Banner
          key={item.message}
          tone="error"
          title={item.message}
          action={
            item === failure ? (
              <Button
                size="xs"
                variant="outline"
                onClick={() => {
                  setPreview((prev) => ({ ...prev, settledKey: null, failure: null }));
                  setReloads((n) => n + 1);
                }}
              >
                {t("apps.installed.retry")}
              </Button>
            ) : undefined
          }
        />
      ))}

      <Card>
        <CardHeader>
          <CardTitle>{t("apps.install.settings.title")}</CardTitle>
        </CardHeader>
        <CardPanel>
          <Tabs defaultValue={BASIC_TAB}>
            <TabsList>
              <TabsTab value={BASIC_TAB}>{t("apps.install.tabs.basic")}</TabsTab>
              <TabsTab value={ADVANCED_TAB}>{t("apps.install.tabs.advanced")}</TabsTab>
            </TabsList>
            <TabsPanel value={BASIC_TAB} className="flex flex-col gap-4">
              {plan.inputs.length === 0 ? (
                <p className="text-muted-foreground text-sm">{t("apps.install.settings.none")}</p>
              ) : (
                plan.inputs.map((input) => (
                  <InputField
                    key={input.name}
                    input={input}
                    value={shownValue(input, values)}
                    optional={input.kind === "string" && !input.required}
                    error={errors.get(input.name) ?? (values[input.name] !== undefined && input.error ? input.error : null)}
                    onChange={(value) => setValue(input.name, value)}
                  />
                ))
              )}
            </TabsPanel>
            <TabsPanel value={ADVANCED_TAB} className="flex flex-col gap-4">
              <Field invalid={errors.has(NAME_FIELD)} className="w-full">
                <FieldLabel htmlFor={fieldId(NAME_FIELD)}>{t("apps.install.name.label")}</FieldLabel>
                <Input
                  id={fieldId(NAME_FIELD)}
                  value={name}
                  placeholder={plan.name}
                  onChange={(event) => setName(event.target.value)}
                  aria-invalid={errors.has(NAME_FIELD)}
                  autoComplete={OFF}
                />
                <FieldDescription>{t("apps.install.name.help", { name: plan.name })}</FieldDescription>
                {errors.has(NAME_FIELD) ? (
                  <FieldError match role="alert">
                    {errors.get(NAME_FIELD)}
                  </FieldError>
                ) : null}
              </Field>
              <AdvancedSettingsFields
                settings={advanced}
                onChange={setAdvanced}
                networks={networks}
                onRetryNetworks={() => void networkQuery.refresh()}
                available={plan.advancedAvailable}
                missingNetwork={missing}
                errors={advancedErrors}
              />
            </TabsPanel>
          </Tabs>
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("apps.install.review.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <p className="text-muted-foreground text-sm">{t("apps.install.review.description")}</p>
          <PlanWarnings warnings={plan.warnings} />
          <PrivilegeSummary privileges={plan.privileges} />
          <CodeView title={t("apps.install.review.compose")} code={plan.compose} defaultOpen />
        </CardPanel>
      </Card>

      <div className="flex flex-col items-start gap-2">
        <Button loading={installing} disabled={installBlocked} onClick={() => void handleInstall()}>
          {t("apps.install.submit", { title: template.title })}
        </Button>
        {conflicted ? <p className="text-muted-foreground text-sm">{t("apps.install.port.blocked")}</p> : null}
        {inputsIncomplete ? <p className="text-muted-foreground text-sm">{t("apps.install.incomplete")}</p> : null}
        {missing !== null ? <p className="text-muted-foreground text-sm">{t("apps.install.networkBlocked")}</p> : null}
        {malformed.length > 0 ? <p className="text-muted-foreground text-sm">{t("apps.install.limitsBlocked")}</p> : null}
      </div>
    </div>
  );
}

export function InstallPage(): React.ReactElement {
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
    fallbackError: t("apps.install.loadFailed"),
  });

  const template = query.data;

  if (template === null) {
    return (
      <div className="flex flex-col gap-4">
        <div>
          <Link to={CATALOG_PATH} className={buttonVariants({ variant: "ghost", size: "sm" })}>
            <ArrowLeft aria-hidden="true" />
            {t("apps.catalogDetail.back")}
          </Link>
        </div>
        {query.error ? (
          <Banner
            tone="error"
            title={t("apps.install.loadFailed")}
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
          <Link to={CATALOG_PATH} className={buttonVariants({ variant: "ghost", size: "sm" })}>
            <ArrowLeft aria-hidden="true" />
            {t("apps.catalogDetail.back")}
          </Link>
        </div>
        <EmptyState
          icon={PackageSearch}
          title={t("apps.catalogDetail.missing.title", { id: appId })}
          description={t("apps.catalogDetail.missing.description")}
        />
      </div>
    );
  }

  return <InstallForm key={template.id} template={template} />;
}
