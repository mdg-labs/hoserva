// The Advanced tab's container settings of the install wizard (doc 03 §5.4):
// network mode, restart policy, resource limits and raw extra parameters.
// Everything is a request field the API validates; the form only collects
// them, and the API's warnings and errors come back through the plan.
import { Globe, Network, Server } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ChoiceCards, type ChoiceCardOption } from "@/components/patterns/choice-cards";
import { CopyValue } from "@/components/patterns/copy-value";
import { LoadingBlock } from "@/components/patterns/loading";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
  customNetworks,
  malformedLimits,
  NETWORK_BRIDGE,
  NETWORK_HOST,
  RESTART_POLICIES,
  type AdvancedField,
  type AdvancedSettings,
  type ConversionWarning,
  type DockerNetwork,
} from "@/routes/apps/install-form";

export type NetworkChoices =
  | { kind: "loading" }
  | { kind: "failed"; message: string }
  | { kind: "ready"; networks: DockerNetwork[] };

const END_ALIGN = "inline-end";
const DECIMAL_MODE = "decimal";
const NUMERIC_MODE = "numeric";
const NETWORK_GROUP = "install-network-mode";
const NETWORK_NAME_ID = "install-network-name";
const RESTART_ID = "install-restart";
const CPUS_ID = "install-cpus";
const MEMORY_ID = "install-memory";
const EXTRA_ID = "install-extra-params";
const OFF = "off";
const TEMPLATE_NETWORK = "__template__";
const OTHER_NETWORK = "__other__";
const TEMPLATE_RESTART = "__template__";

export function AdvancedSettingsFields({
  settings,
  onChange,
  networks,
  onRetryNetworks,
  available,
  missingNetwork,
  errors,
}: {
  settings: AdvancedSettings;
  onChange: (next: AdvancedSettings) => void;
  networks: NetworkChoices;
  onRetryNetworks: () => void;
  available: boolean;
  missingNetwork: ConversionWarning | null;
  errors: Partial<Record<AdvancedField, string>>;
}): React.ReactElement {
  const { t } = useTranslation();
  const [otherChosen, setOtherChosen] = useState(false);
  const set = (patch: Partial<AdvancedSettings>): void => onChange({ ...settings, ...patch });
  const malformed = malformedLimits(settings);

  const custom = networks.kind === "ready" ? customNetworks(networks.networks) : [];
  const listed = new Set(custom.map((network) => network.name));
  const chosen = settings.networkMode === "" ? TEMPLATE_NETWORK : settings.networkMode;
  const selected = otherChosen
    ? OTHER_NETWORK
    : chosen === TEMPLATE_NETWORK || chosen === NETWORK_BRIDGE || chosen === NETWORK_HOST || listed.has(chosen)
      ? chosen
      : OTHER_NETWORK;

  const options: ChoiceCardOption[] = [
    {
      value: TEMPLATE_NETWORK,
      title: t("apps.install.advanced.network.template.title"),
      description: t("apps.install.advanced.network.template.description"),
      icon: <Server className="size-4" aria-hidden="true" />,
    },
    {
      value: NETWORK_BRIDGE,
      title: t("apps.install.advanced.network.bridge.title"),
      description: t("apps.install.advanced.network.bridge.description"),
      icon: <Network className="size-4" aria-hidden="true" />,
    },
    {
      value: NETWORK_HOST,
      title: t("apps.install.advanced.network.host.title"),
      description: t("apps.install.advanced.network.host.description"),
      icon: <Globe className="size-4" aria-hidden="true" />,
    },
    ...custom.map((network) => ({
      value: network.name,
      title: network.name,
      description: t("apps.install.advanced.network.custom.description", { driver: network.driver }),
      icon: <Network className="size-4" aria-hidden="true" />,
    })),
    {
      value: OTHER_NETWORK,
      title: t("apps.install.advanced.network.other.title"),
      description: t("apps.install.advanced.network.other.description"),
      icon: <Network className="size-4" aria-hidden="true" />,
    },
  ];

  const restartLabels: Record<string, string> = {
    [TEMPLATE_RESTART]: t("apps.install.advanced.restart.template"),
    no: t("apps.install.advanced.restart.no"),
    always: t("apps.install.advanced.restart.always"),
    "unless-stopped": t("apps.install.advanced.restart.unlessStopped"),
    "on-failure": t("apps.install.advanced.restart.onFailure"),
  };
  const restartItems = [TEMPLATE_RESTART, ...RESTART_POLICIES].map((value) => ({ value, label: restartLabels[value] }));

  return (
    <>
      {available ? (
        <div role="group" aria-labelledby="install-network-label" className="flex w-full flex-col gap-2">
          <span id="install-network-label" className="font-medium text-base/4.5 sm:text-sm/4">
            {t("apps.install.advanced.network.label")}
          </span>
          <p className="text-muted-foreground text-xs">{t("apps.install.advanced.network.help")}</p>
          <ChoiceCards
            name={NETWORK_GROUP}
            value={selected}
            options={options}
            onChange={(next) => {
              const other = next === OTHER_NETWORK;
              setOtherChosen(other);
              set({ networkMode: other || next === TEMPLATE_NETWORK ? "" : next });
            }}
          />
          {networks.kind === "loading" ? <LoadingBlock /> : null}
          {networks.kind === "failed" ? (
            <Banner
              tone="warning"
              title={t("apps.install.advanced.network.listFailed")}
              description={networks.message}
              action={
                <Button size="xs" variant="outline" onClick={onRetryNetworks}>
                  {t("apps.installed.retry")}
                </Button>
              }
            />
          ) : null}
          {selected === OTHER_NETWORK ? (
            <Field className="w-full">
              <FieldLabel htmlFor={NETWORK_NAME_ID}>{t("apps.install.advanced.network.nameLabel")}</FieldLabel>
              <Input
                id={NETWORK_NAME_ID}
                value={settings.networkMode}
                onChange={(event) => set({ networkMode: event.target.value })}
                aria-invalid={errors.networkMode !== undefined}
                autoComplete={OFF}
              />
            </Field>
          ) : null}
          {settings.networkMode === NETWORK_HOST ? (
            <Banner
              tone="warning"
              title={t("apps.install.advanced.network.hostWarningTitle")}
              description={t("apps.install.advanced.network.hostWarning")}
            />
          ) : null}
          {missingNetwork !== null && missingNetwork.command !== undefined ? (
            <div className="flex flex-col gap-2">
              <p className="text-sm">{t("apps.install.advanced.network.missing", { name: missingNetwork.detail ?? settings.networkMode })}</p>
              <CopyValue value={missingNetwork.command} label={t("apps.install.advanced.network.command")} />
            </div>
          ) : null}
          {errors.networkMode !== undefined ? (
            <p role="alert" className="text-destructive text-sm">
              {errors.networkMode}
            </p>
          ) : null}
        </div>
      ) : (
        <p className="text-muted-foreground text-sm">{t("apps.install.advanced.severalServices")}</p>
      )}

      <Field invalid={errors.restart !== undefined} className="w-full">
        <FieldLabel htmlFor={RESTART_ID}>{t("apps.install.advanced.restart.label")}</FieldLabel>
        <Select
          value={settings.restart === "" ? TEMPLATE_RESTART : settings.restart}
          onValueChange={(next) => set({ restart: next === null || next === TEMPLATE_RESTART ? "" : next })}
          items={restartItems}
        >
          <SelectTrigger id={RESTART_ID}>
            <SelectValue />
          </SelectTrigger>
          <SelectPopup>
            {restartItems.map((item) => (
              <SelectItem key={item.value} value={item.value}>
                {item.label}
              </SelectItem>
            ))}
          </SelectPopup>
        </Select>
        <FieldDescription>{t("apps.install.advanced.restart.help")}</FieldDescription>
        {errors.restart !== undefined ? (
          <FieldError match role="alert">
            {errors.restart}
          </FieldError>
        ) : null}
      </Field>

      {available ? (
        <>
          <Field invalid={errors.cpus !== undefined || malformed.includes("cpus")} className="w-full">
            <FieldLabel htmlFor={CPUS_ID}>{t("apps.install.advanced.cpus.label")}</FieldLabel>
            <InputGroup>
              <InputGroupInput
                render={
                  <Input
                    id={CPUS_ID}
                    inputMode={DECIMAL_MODE}
                    value={settings.cpus}
                    placeholder={t("apps.install.advanced.noLimit")}
                    onChange={(event) => set({ cpus: event.target.value })}
                    aria-invalid={errors.cpus !== undefined || malformed.includes("cpus")}
                    autoComplete={OFF}
                  />
                }
              />
              <InputGroupAddon align={END_ALIGN}>{t("apps.install.advanced.cpus.unit")}</InputGroupAddon>
            </InputGroup>
            <FieldDescription>{t("apps.install.advanced.cpus.help")}</FieldDescription>
            {errors.cpus !== undefined || malformed.includes("cpus") ? (
              <FieldError match role="alert">
                {errors.cpus ?? t("apps.install.advanced.cpus.malformed")}
              </FieldError>
            ) : null}
          </Field>

          <Field invalid={errors.memoryMiB !== undefined || malformed.includes("memoryMiB")} className="w-full">
            <FieldLabel htmlFor={MEMORY_ID}>{t("apps.install.advanced.memory.label")}</FieldLabel>
            <InputGroup>
              <InputGroupInput
                render={
                  <Input
                    id={MEMORY_ID}
                    inputMode={NUMERIC_MODE}
                    value={settings.memoryMiB}
                    placeholder={t("apps.install.advanced.noLimit")}
                    onChange={(event) => set({ memoryMiB: event.target.value })}
                    aria-invalid={errors.memoryMiB !== undefined || malformed.includes("memoryMiB")}
                    autoComplete={OFF}
                  />
                }
              />
              <InputGroupAddon align={END_ALIGN}>{t("apps.install.advanced.memory.unit")}</InputGroupAddon>
            </InputGroup>
            <FieldDescription>{t("apps.install.advanced.memory.help")}</FieldDescription>
            {errors.memoryMiB !== undefined || malformed.includes("memoryMiB") ? (
              <FieldError match role="alert">
                {errors.memoryMiB ?? t("apps.install.advanced.memory.malformed")}
              </FieldError>
            ) : null}
          </Field>

          <Field invalid={errors.extraParams !== undefined} className="w-full">
            <FieldLabel htmlFor={EXTRA_ID}>{t("apps.install.advanced.extra.label")}</FieldLabel>
            <Textarea
              id={EXTRA_ID}
              value={settings.extraParams}
              placeholder={t("apps.install.advanced.extra.placeholder")}
              onChange={(event) => set({ extraParams: event.target.value })}
              aria-invalid={errors.extraParams !== undefined}
              autoComplete={OFF}
              spellCheck={false}
            />
            <FieldDescription>{t("apps.install.advanced.extra.help")}</FieldDescription>
            {errors.extraParams !== undefined ? (
              <FieldError match role="alert">
                {errors.extraParams}
              </FieldError>
            ) : null}
          </Field>
        </>
      ) : null}
    </>
  );
}
