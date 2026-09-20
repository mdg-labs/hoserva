import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { showFeedbackToast } from "@/components/patterns/feedback-toast";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { LoadingBlock } from "@/components/patterns/loading";
import { NumberUnit } from "@/components/patterns/number-unit";
import { SecretInput } from "@/components/patterns/secret-input";
import { SegmentedChoice } from "@/components/patterns/segmented-choice";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardFooter, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { hoservaClient, type components } from "@/lib/api/client";

type NetworkSettings = components["schemas"]["NetworkSettings"];
type NetworkInterface = components["schemas"]["NetworkInterface"];
type DNS01Provider = components["schemas"]["DNS01Provider"];

const METHOD_DHCP = "dhcp";
const METHOD_STATIC = "static";
const ADDRESS_METHOD_FIELD = "address-method";
const INTERFACE_FIELD = "network-interface";
const DNS01_PROVIDER_FIELD = "dns01-provider";
const PROVIDER_CLOUDFLARE: DNS01Provider = "cloudflare";
const PROVIDER_RFC2136: DNS01Provider = "rfc2136";

function dnsToDraft(dns: string[] | undefined): string {
  return (dns ?? []).join(", ");
}

function parseDns(raw: string): string[] {
  return raw
    .split(/[,\s]+/)
    .map((part) => part.trim())
    .filter(Boolean);
}

function certTone(daysRemaining: number): "success" | "warning" | "error" {
  if (daysRemaining < 0) {
    return "error";
  }
  if (daysRemaining < 30) {
    return "warning";
  }
  return "success";
}

export function NetworkSettingsPage(): React.ReactElement {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [settings, setSettings] = useState<NetworkSettings | null>(null);
  const [selected, setSelected] = useState("");
  const [method, setMethod] = useState<string>(METHOD_DHCP);
  const [address, setAddress] = useState("");
  const [prefix, setPrefix] = useState(24);
  const [gateway, setGateway] = useState("");
  const [dns, setDns] = useState("");
  const [port, setPort] = useState(8008);
  const [allowAllOpen, setAllowAllOpen] = useState(false);
  const [leOpen, setLeOpen] = useState(false);
  const [leBusy, setLeBusy] = useState(false);
  const [leDomain, setLeDomain] = useState("");
  const [leProvider, setLeProvider] = useState<DNS01Provider>(PROVIDER_CLOUDFLARE);
  const [leToken, setLeToken] = useState("");
  const [leNameserver, setLeNameserver] = useState("");
  const [leTsigKey, setLeTsigKey] = useState("");
  const [leTsigSecret, setLeTsigSecret] = useState("");
  const formSeeded = useRef(false);

  const seedForm = (data: NetworkSettings): void => {
    const ifaces = data.interfaces ?? [];
    const current = ifaces.find((iface) => iface.name === selected) ?? ifaces[0];
    if (current) {
      setSelected(current.name);
      setMethod(current.method);
      setAddress(current.address ?? "");
      setPrefix(current.prefix ?? 24);
      setGateway(current.gateway ?? "");
      setDns(dnsToDraft(current.dns));
    }
    setPort(data.listenPort);
  };

  const load = useCallback((signal?: AbortSignal) => {
    return hoservaClient.GET("/settings/network", { signal }).then(({ data, error: apiError }) => {
      if (signal?.aborted) {
        return;
      }
      if (apiError) {
        setError(apiError.message);
        return;
      }
      if (data) {
        setError(null);
        setSettings(data);
        if (!formSeeded.current) {
          formSeeded.current = true;
          const ifaces = data.interfaces ?? [];
          const current = ifaces[0];
          if (current) {
            setSelected(current.name);
            setMethod(current.method);
            setAddress(current.address ?? "");
            setPrefix(current.prefix ?? 24);
            setGateway(current.gateway ?? "");
            setDns(dnsToDraft(current.dns));
          }
          setPort(data.listenPort);
        } else {
          setPort(data.listenPort);
        }
      }
    });
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    load(controller.signal)
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : String(err));
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) {
          setLoading(false);
        }
      });
    return () => controller.abort();
  }, [load]);

  useEffect(() => {
    if (!settings?.pending) {
      return;
    }
    const id = window.setInterval(() => {
      void load().catch(() => undefined);
    }, 1000);
    return () => window.clearInterval(id);
  }, [settings?.pending, load]);

  const selectedIface = useMemo(
    () => (settings?.interfaces ?? []).find((iface) => iface.name === selected),
    [settings, selected],
  );

  function selectIface(iface: NetworkInterface): void {
    setSelected(iface.name);
    setMethod(iface.method);
    setAddress(iface.address ?? "");
    setPrefix(iface.prefix ?? 24);
    setGateway(iface.gateway ?? "");
    setDns(dnsToDraft(iface.dns));
  }

  async function handleApplyAddressing(): Promise<void> {
    setError(null);
    setSaving(true);
    try {
      const { data, error: apiError } = await hoservaClient.PUT("/settings/network", {
        body: {
          interface: selected,
          method: method === METHOD_STATIC ? METHOD_STATIC : METHOD_DHCP,
          address: method === METHOD_STATIC ? address : undefined,
          prefix: method === METHOD_STATIC ? prefix : undefined,
          gateway: method === METHOD_STATIC ? gateway : undefined,
          dns: parseDns(dns),
        },
      });
      if (apiError) {
        setError(apiError.message);
        return;
      }
      if (data) {
        setSettings(data);
        seedForm(data);
      }
    } finally {
      setSaving(false);
    }
  }

  async function handleConfirm(): Promise<void> {
    setError(null);
    const { data, error: apiError } = await hoservaClient.POST("/settings/network/confirm", {});
    if (apiError) {
      setError(apiError.message);
      return;
    }
      if (data) {
        setSettings(data);
        seedForm(data);
      }
  }

  async function handleRegen(): Promise<void> {
    setError(null);
    const { data, error: apiError } = await hoservaClient.POST("/settings/network/certificate", {});
    if (apiError) {
      setError(apiError.message);
      return;
    }
    if (data) {
      setSettings(data);
      seedForm(data);
      showFeedbackToast({ type: "success", title: t("settings.network.regenDone") });
    }
  }

  async function handleLetsEncrypt(): Promise<void> {
    if (leDomain.trim().length === 0) {
      setError(t("settings.network.leDomainRequired"));
      return;
    }
    setError(null);
    setLeBusy(true);
    try {
      const { error: apiError } = await hoservaClient.POST("/settings/network/lets-encrypt", {
        body: {
          domain: leDomain.trim(),
          provider: leProvider,
          cloudflareAPIToken: leProvider === PROVIDER_CLOUDFLARE ? leToken || undefined : undefined,
          rfc2136Nameserver: leProvider === PROVIDER_RFC2136 ? leNameserver || undefined : undefined,
          rfc2136TsigKeyName: leProvider === PROVIDER_RFC2136 ? leTsigKey || undefined : undefined,
          rfc2136TsigSecret: leProvider === PROVIDER_RFC2136 ? leTsigSecret || undefined : undefined,
        },
      });
      if (apiError) {
        setError(apiError.message);
        return;
      }
      setLeOpen(false);
      setLeToken("");
      setLeTsigSecret("");
      showFeedbackToast({ type: "success", title: t("settings.network.leQueued") });
      await load();
    } finally {
      setLeBusy(false);
    }
  }

  async function handleDisableLetsEncrypt(): Promise<void> {
    setError(null);
    const { data, error: apiError } = await hoservaClient.DELETE("/settings/network/lets-encrypt", {});
    if (apiError) {
      setError(apiError.message);
      return;
    }
    if (data) {
      setSettings(data);
      seedForm(data);
      showFeedbackToast({ type: "success", title: t("settings.network.leDisabled") });
    }
  }

  async function handleAllowAll(next: boolean): Promise<void> {
    if (next) {
      setAllowAllOpen(true);
      return;
    }
    await persistAccess(false);
  }

  async function persistAccess(allowAllSources: boolean): Promise<void> {
    setError(null);
    const { data, error: apiError } = await hoservaClient.PUT("/settings/network", {
      body: { allowAllSources },
    });
    if (apiError) {
      setError(apiError.message);
      return;
    }
      if (data) {
        setSettings(data);
        seedForm(data);
      }
  }

  async function handlePortSave(): Promise<void> {
    setError(null);
    setSaving(true);
    try {
      const { data, error: apiError } = await hoservaClient.PUT("/settings/network", {
        body: { listenPort: port },
      });
      if (apiError) {
        setError(apiError.message);
        return;
      }
      if (data) {
        setSettings(data);
        seedForm(data);
      }
    } finally {
      setSaving(false);
    }
  }

  if (loading) {
    return <LoadingBlock />;
  }

  const editable = settings?.editable === true;
  const pending = settings?.pending;
  const columns: DataTableColumn<NetworkInterface>[] = [
    { id: "name", header: t("settings.network.columns.name"), cell: (row) => row.name },
    {
      id: "address",
      header: t("settings.network.columns.address"),
      cell: (row) => (row.address ? `${row.address}/${row.prefix ?? ""}` : "—"),
    },
    {
      id: "method",
      header: t("settings.network.columns.method"),
      cell: (row) => t(row.method === METHOD_STATIC ? "settings.network.methodStatic" : "settings.network.methodDhcp"),
    },
    {
      id: "state",
      header: t("settings.network.columns.state"),
      cell: (row) => t(row.state === "up" ? "settings.network.stateUp" : "settings.network.stateDown"),
    },
  ];

  const daysRemaining = settings?.certificate.daysRemaining ?? 0;
  const certKey = daysRemaining < 0 ? "certExpired" : daysRemaining < 30 ? "certExpiring" : "certValid";

  return (
    <div className="flex flex-col gap-4">
      {error ? <Banner tone="error" title={error} /> : null}
      {settings && !editable && settings.readOnlyReason ? (
        <Banner tone="info" title={t("settings.network.readOnlyTitle")} description={settings.readOnlyReason} />
      ) : null}
      {pending ? (
        <Banner
          tone="warning"
          title={t("settings.network.pendingTitle")}
          description={t("settings.network.pendingDescription", { seconds: pending.remainingSeconds })}
          action={
            <Button type="button" onClick={() => void handleConfirm()}>
              {t("settings.network.confirmChange")}
            </Button>
          }
        />
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.network.interfacesTitle")}</CardTitle>
        </CardHeader>
        <CardPanel>
          <DataTable
            columns={columns}
            rows={settings?.interfaces ?? []}
            getRowKey={(row) => row.name}
          />
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.network.addressingTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <Field>
            <FieldLabel>{t("settings.network.selectInterface")}</FieldLabel>
            <SegmentedChoice
              name={INTERFACE_FIELD}
              value={selected}
              onChange={(name) => {
                const next = (settings?.interfaces ?? []).find((iface) => iface.name === name);
                if (next) {
                  selectIface(next);
                }
              }}
              disabled={!editable}
              options={(settings?.interfaces ?? []).map((iface) => ({
                value: iface.name,
                label: iface.name,
              }))}
            />
          </Field>
          <Field>
            <FieldLabel>{t("settings.network.method")}</FieldLabel>
            <SegmentedChoice
              name={ADDRESS_METHOD_FIELD}
              value={method}
              onChange={setMethod}
              disabled={!editable}
              options={[
                { value: METHOD_DHCP, label: t("settings.network.methodDhcp") },
                { value: METHOD_STATIC, label: t("settings.network.methodStatic") },
              ]}
            />
            <FieldDescription>{t("settings.network.addressingDescription")}</FieldDescription>
          </Field>
          {method === METHOD_STATIC ? (
            <>
              <Field>
                <FieldLabel>{t("settings.network.address")}</FieldLabel>
                <Input value={address} onChange={(event) => setAddress(event.target.value)} disabled={!editable} />
              </Field>
              <Field>
                <FieldLabel>{t("settings.network.prefix")}</FieldLabel>
                <NumberUnit value={prefix} onChange={setPrefix} unit="" min={0} max={128} disabled={!editable} />
              </Field>
              <Field>
                <FieldLabel>{t("settings.network.gateway")}</FieldLabel>
                <Input value={gateway} onChange={(event) => setGateway(event.target.value)} disabled={!editable} />
              </Field>
            </>
          ) : null}
          <Field>
            <FieldLabel>{t("settings.network.dns")}</FieldLabel>
            <Input
              value={dns}
              onChange={(event) => setDns(event.target.value)}
              placeholder={t("settings.network.dnsPlaceholder")}
              disabled={!editable}
            />
          </Field>
        </CardPanel>
        <CardFooter className="flex justify-end gap-2 border-t">
          <Button
            type="button"
            variant="outline"
            disabled={!editable || !selectedIface || saving}
            onClick={() => selectedIface && selectIface(selectedIface)}
          >
            {t("settings.actions.cancel")}
          </Button>
          <Button type="button" loading={saving} disabled={!editable || !selected} onClick={() => void handleApplyAddressing()}>
            {t("settings.network.apply")}
          </Button>
        </CardFooter>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.network.httpsTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <Field>
            <FieldLabel>{t("settings.network.certExpiry")}</FieldLabel>
            <div className="flex items-center gap-2">
              <StatusBadge tone={certTone(daysRemaining)}>{t(`settings.network.${certKey}`)}</StatusBadge>
              <span className="text-sm text-muted-foreground">
                {settings?.certificate.notAfter
                  ? new Date(settings.certificate.notAfter).toLocaleDateString()
                  : null}
              </span>
            </div>
            <FieldDescription>{t("settings.network.httpsDescription")}</FieldDescription>
          </Field>
          {settings?.letsEncrypt.lastError ? (
            <Banner tone="error" title={t("settings.network.leLastError")} description={settings.letsEncrypt.lastError} />
          ) : null}
          {settings?.letsEncrypt.configured ? (
            <Field>
              <FieldLabel>{t("settings.network.leStatus")}</FieldLabel>
              <FieldDescription>
                {t("settings.network.leConfigured", {
                  domain: settings.letsEncrypt.domain ?? "",
                  provider:
                    settings.letsEncrypt.provider === PROVIDER_RFC2136
                      ? t("settings.network.leProviderRfc2136")
                      : t("settings.network.leProviderCloudflare"),
                })}
                {settings.letsEncrypt.enabled
                  ? ` ${t("settings.network.leRenewalOn")}`
                  : ` ${t("settings.network.leRenewalOff")}`}
              </FieldDescription>
            </Field>
          ) : null}
        </CardPanel>
        <CardFooter className="flex flex-wrap justify-end gap-2 border-t">
          {settings?.letsEncrypt.enabled ? (
            <Button type="button" variant="outline" onClick={() => void handleDisableLetsEncrypt()}>
              {t("settings.network.leDisable")}
            </Button>
          ) : null}
          <Button
            type="button"
            variant="outline"
            onClick={() => {
              setLeDomain(settings?.letsEncrypt.domain ?? "");
              setLeProvider(settings?.letsEncrypt.provider === PROVIDER_RFC2136 ? PROVIDER_RFC2136 : PROVIDER_CLOUDFLARE);
              setLeOpen(true);
            }}
          >
            {t("settings.network.leSetup")}
          </Button>
          <Button type="button" variant="outline" onClick={() => void handleRegen()}>
            {t("settings.network.regenCert")}
          </Button>
        </CardFooter>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.network.accessTitle")}</CardTitle>
        </CardHeader>
        <CardPanel>
          <SettingSwitch
            label={t("settings.network.allowAll")}
            description={t("settings.network.allowAllDescription")}
            checked={settings?.allowAllSources === true}
            onCheckedChange={(checked) => void handleAllowAll(checked)}
          />
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.network.portTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <Field>
            <FieldLabel>{t("settings.network.portTitle")}</FieldLabel>
            <NumberUnit value={port} onChange={setPort} unit={t("settings.network.portUnit")} min={1} max={65535} />
            <FieldDescription>
              {settings?.listenPortRestartRequired
                ? t("settings.network.portRestart")
                : t("settings.network.portDescription")}
            </FieldDescription>
          </Field>
        </CardPanel>
        <CardFooter className="flex justify-end border-t">
          <Button type="button" loading={saving} disabled={port === settings?.listenPort} onClick={() => void handlePortSave()}>
            {t("settings.actions.save")}
          </Button>
        </CardFooter>
      </Card>

      <ConfirmDialog
        open={allowAllOpen}
        onOpenChange={setAllowAllOpen}
        title={t("settings.network.allowAllConfirmTitle")}
        description={t("settings.network.allowAllConfirmDescription")}
        confirmLabel={t("settings.network.allowAll")}
        onConfirm={() => {
          setAllowAllOpen(false);
          void persistAccess(true);
        }}
      />

      <FormOverlay
        open={leOpen}
        onOpenChange={setLeOpen}
        title={t("settings.network.leSetup")}
        description={t("settings.network.leDescription")}
        footer={
          <div className="flex justify-end gap-2">
            <Button type="button" variant="outline" onClick={() => setLeOpen(false)}>
              {t("settings.actions.cancel")}
            </Button>
            <Button type="button" loading={leBusy} onClick={() => void handleLetsEncrypt()}>
              {t("settings.network.leIssue")}
            </Button>
          </div>
        }
      >
        <Field>
          <FieldLabel>{t("settings.network.leDomain")}</FieldLabel>
          <Input
            value={leDomain}
            onChange={(event) => setLeDomain(event.target.value)}
            placeholder={t("settings.network.leDomainPlaceholder")}
          />
        </Field>
        <Field>
          <FieldLabel>{t("settings.network.leProvider")}</FieldLabel>
          <SegmentedChoice
            name={DNS01_PROVIDER_FIELD}
            value={leProvider}
            onChange={(value) => setLeProvider(value as DNS01Provider)}
            options={[
              { value: PROVIDER_CLOUDFLARE, label: t("settings.network.leProviderCloudflare") },
              { value: PROVIDER_RFC2136, label: t("settings.network.leProviderRfc2136") },
            ]}
          />
        </Field>
        {leProvider === PROVIDER_CLOUDFLARE ? (
          <Field>
            <FieldLabel>{t("settings.network.leCloudflareToken")}</FieldLabel>
            <SecretInput
              value={leToken}
              onChange={setLeToken}
              placeholder={t("settings.network.leCloudflareTokenPlaceholder")}
            />
          </Field>
        ) : (
          <>
            <Field>
              <FieldLabel>{t("settings.network.leNameserver")}</FieldLabel>
              <Input value={leNameserver} onChange={(event) => setLeNameserver(event.target.value)} />
            </Field>
            <Field>
              <FieldLabel>{t("settings.network.leTsigKey")}</FieldLabel>
              <Input value={leTsigKey} onChange={(event) => setLeTsigKey(event.target.value)} />
            </Field>
            <Field>
              <FieldLabel>{t("settings.network.leTsigSecret")}</FieldLabel>
              <SecretInput value={leTsigSecret} onChange={setLeTsigSecret} />
            </Field>
          </>
        )}
      </FormOverlay>
    </div>
  );
}
