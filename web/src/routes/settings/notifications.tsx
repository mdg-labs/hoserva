import { Plus } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { DataTable } from "@/components/patterns/data-table";
import { showFeedbackToast } from "@/components/patterns/feedback-toast";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { LoadingBlock } from "@/components/patterns/loading";
import { SecretInput } from "@/components/patterns/secret-input";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardFooter,
  CardFrame,
  CardFrameAction,
  CardFrameDescription,
  CardFrameHeader,
  CardFrameTitle,
  CardHeader,
  CardPanel,
  CardTitle,
} from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  CHANNEL_ICONS,
  NOTIFICATION_EVENT_TYPES,
  NOTIFICATION_LEVELS,
  type NotificationChannelType,
  type NotificationEventType,
  type NotificationLevel,
} from "@/lib/notification-catalog";
import type { components } from "@/lib/api/client";
import {
  getNotificationChannels,
  getNotificationRouting,
  getQuietHours,
  postNotificationChannel,
  postNotificationChannelTest,
  putNotificationChannel,
  putNotificationRoute,
  putQuietHours,
} from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";

type NotificationChannel = components["schemas"]["NotificationChannel"];
type NotificationRoutingEntry = components["schemas"]["NotificationRoutingEntry"];
type NotificationQuietHours = components["schemas"]["NotificationQuietHours"];

type RoutingDraft = Record<NotificationEventType, NotificationRoutingEntry>;

const ROUTING_COLUMN_EVENT = "event";
const ROUTING_COLUMN_SEVERITY = "severity";

function buildRoutingDraft(entries: NotificationRoutingEntry[]): RoutingDraft {
  const draft = {} as RoutingDraft;
  for (const eventType of NOTIFICATION_EVENT_TYPES) {
    const existing = entries.find((entry) => entry.eventType === eventType);
    draft[eventType] = existing ?? {
      eventType,
      severity: "warning",
      channelIds: [],
    };
  }
  return draft;
}

export function NotificationsSettingsPage(): React.ReactElement {
  const { t } = useTranslation();
  const channelsQuery = useApiQuery<{ channels: NotificationChannel[] }>({
    queryKey: "notification-channels",
    queryFn: (signal) => getNotificationChannels(signal),
    fallbackError: t("settings.notifications.loadFailed"),
  });
  const routingQuery = useApiQuery<{ routing: NotificationRoutingEntry[] }>({
    queryKey: "notification-routing",
    queryFn: (signal) => getNotificationRouting(signal),
    fallbackError: t("settings.notifications.loadFailed"),
  });
  const quietHoursQuery = useApiQuery<NotificationQuietHours>({
    queryKey: "notification-quiet-hours",
    queryFn: (signal) => getQuietHours(signal),
    fallbackError: t("settings.notifications.loadFailed"),
  });

  const channels = channelsQuery.data?.channels ?? [];
  const savedRouting = useMemo(
    () => (routingQuery.data ? buildRoutingDraft(routingQuery.data.routing ?? []) : null),
    [routingQuery.data],
  );
  const savedQuietHours = quietHoursQuery.data;

  const [routingDraft, setRoutingDraft] = useState<RoutingDraft | null>(null);
  const [quietHoursDraft, setQuietHoursDraft] = useState<NotificationQuietHours | null>(null);
  const [testingChannelId, setTestingChannelId] = useState<string | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [channelType, setChannelType] = useState<NotificationChannelType>("email");
  const [channelName, setChannelName] = useState("");
  const [channelSecret, setChannelSecret] = useState("");
  const [addValidationError, setAddValidationError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  // Set at handleSave's entry and cleared in `finally`, so Save/Cancel stay
  // disabled for the whole handler, not just while the two mutations' own
  // `pending` flags are true — those drop back to false while the trailing
  // `Promise.all` refresh is still in flight and `formDirty` is still true
  // (the queries' data hasn't caught up yet), which would let a second
  // click re-submit the same routing/quiet-hours save (issue #271 finding).
  const [saveBusy, setSaveBusy] = useState(false);

  // Adjusts the drafts when the loaded (or just-saved) values change,
  // rather than in an effect (react-hooks/set-state-in-effect).
  const [seenRouting, setSeenRouting] = useState<RoutingDraft | null>(null);
  if (savedRouting && savedRouting !== seenRouting) {
    setSeenRouting(savedRouting);
    setRoutingDraft(savedRouting);
  }

  const [seenQuietHours, setSeenQuietHours] = useState<NotificationQuietHours | null>(null);
  if (savedQuietHours && savedQuietHours !== seenQuietHours) {
    setSeenQuietHours(savedQuietHours);
    setQuietHoursDraft(savedQuietHours);
  }

  const channelEnabledMutation = useApiMutation<{ channel: NotificationChannel; enabled: boolean }, NotificationChannel>(
    {
      mutationFn: ({ channel, enabled }) =>
        putNotificationChannel(channel.id, { name: channel.name, type: channel.type, enabled }),
    },
  );
  const channelTestMutation = useApiMutation<string, components["schemas"]["NotificationTestResult"]>({
    mutationFn: (channelId) => postNotificationChannelTest(channelId),
  });
  const addChannelMutation = useApiMutation<
    { name: string; type: NotificationChannelType; enabled: boolean; secret?: string },
    NotificationChannel
  >({
    mutationFn: (body) => postNotificationChannel(body),
  });
  const quietHoursMutation = useApiMutation<
    { enabled: boolean; start: string; end: string },
    NotificationQuietHours
  >({
    mutationFn: (body) => putQuietHours(body),
  });
  const routingMutation = useApiMutation<
    { eventType: NotificationEventType; severity: NotificationLevel; channelIds: string[] },
    NotificationRoutingEntry
  >({
    mutationFn: ({ eventType, severity, channelIds }) => putNotificationRoute(eventType, { severity, channelIds }),
  });

  const routingDirty = useMemo(() => {
    if (!savedRouting || !routingDraft) {
      return false;
    }
    return JSON.stringify(savedRouting) !== JSON.stringify(routingDraft);
  }, [routingDraft, savedRouting]);

  const quietHoursDirty = useMemo(() => {
    if (!savedQuietHours || !quietHoursDraft) {
      return false;
    }
    return JSON.stringify(savedQuietHours) !== JSON.stringify(quietHoursDraft);
  }, [quietHoursDraft, savedQuietHours]);

  const formDirty = routingDirty || quietHoursDirty;

  async function handleChannelEnabled(channel: NotificationChannel, enabled: boolean): Promise<void> {
    setActionError(null);
    const result = await channelEnabledMutation.mutate({ channel, enabled });
    if (!result.ok) {
      if (!result.aborted) {
        setActionError(result.error);
      }
      return;
    }
    await channelsQuery.refresh();
  }

  async function handleTestSend(channelId: string): Promise<void> {
    setTestingChannelId(channelId);
    try {
      const result = await channelTestMutation.mutate(channelId);
      if (!result.ok) {
        if (!result.aborted) {
          showFeedbackToast({
            type: "error",
            title: t("settings.notifications.testFailed"),
            description: result.error,
          });
        }
        return;
      }
      if (result.data?.success) {
        showFeedbackToast({
          type: "success",
          title: t("settings.notifications.testSucceeded"),
        });
        return;
      }
      showFeedbackToast({
        type: "error",
        title: t("settings.notifications.testFailed"),
        description: result.data?.error ?? undefined,
      });
    } finally {
      setTestingChannelId(null);
    }
  }

  async function handleAddChannel(): Promise<void> {
    if (channelName.trim().length === 0) {
      setAddValidationError(t("settings.notifications.channelNameRequired"));
      return;
    }
    setAddValidationError(null);
    const result = await addChannelMutation.mutate({
      name: channelName.trim(),
      type: channelType,
      enabled: true,
      secret: channelSecret || undefined,
    });
    if (!result.ok) {
      return;
    }
    setAddOpen(false);
    setChannelName("");
    setChannelSecret("");
    setChannelType("email");
    showFeedbackToast({
      type: "success",
      title: t("settings.notifications.channelAdded"),
    });
    await channelsQuery.refresh();
  }

  function updateRouting(
    eventType: NotificationEventType,
    patch: Partial<Pick<NotificationRoutingEntry, "severity" | "channelIds">>,
  ): void {
    setRoutingDraft((current) => {
      if (!current) {
        return current;
      }
      return {
        ...current,
        [eventType]: {
          ...current[eventType],
          ...patch,
        },
      };
    });
  }

  async function handleSave(): Promise<void> {
    if (!routingDraft || !quietHoursDraft) {
      return;
    }
    setSaveBusy(true);
    setActionError(null);
    try {
      if (quietHoursDirty) {
        const result = await quietHoursMutation.mutate({
          enabled: quietHoursDraft.enabled,
          start: quietHoursDraft.start,
          end: quietHoursDraft.end,
        });
        if (!result.ok) {
          if (!result.aborted) {
            setActionError(result.error);
          }
          return;
        }
      }

      if (routingDirty) {
        for (const eventType of NOTIFICATION_EVENT_TYPES) {
          const entry = routingDraft[eventType];
          const previous = savedRouting?.[eventType];
          if (previous && JSON.stringify(previous) === JSON.stringify(entry)) {
            continue;
          }
          const result = await routingMutation.mutate({
            eventType,
            severity: entry.severity,
            channelIds: entry.channelIds,
          });
          if (!result.ok) {
            if (!result.aborted) {
              setActionError(result.error);
            }
            return;
          }
        }
      }

      await Promise.all([quietHoursQuery.refresh(), routingQuery.refresh()]);
    } finally {
      setSaveBusy(false);
    }
  }

  function handleCancel(): void {
    setActionError(null);
    if (savedRouting) {
      setRoutingDraft(savedRouting);
    }
    if (savedQuietHours) {
      setQuietHoursDraft(savedQuietHours);
    }
  }

  const loading = channelsQuery.loading || routingQuery.loading || quietHoursQuery.loading;
  const saving = saveBusy || quietHoursMutation.pending || routingMutation.pending;

  if (loading) {
    return <LoadingBlock />;
  }

  const error =
    channelsQuery.error ?? routingQuery.error ?? quietHoursQuery.error ?? actionError;

  if (!routingDraft || !quietHoursDraft) {
    return <Banner tone="error" title={error ?? t("settings.notifications.loadFailed")} />;
  }

  return (
    <div className="flex flex-col gap-4">
      {error ? <Banner tone="error" title={error} /> : null}

      <Card>
        <CardHeader className="flex flex-row items-center justify-between gap-4">
          <CardTitle>{t("settings.notifications.channelsTitle")}</CardTitle>
          <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
            <Plus aria-hidden="true" className="size-4" />
            {t("settings.notifications.addChannel")}
          </Button>
        </CardHeader>
        <CardPanel className="flex flex-col gap-3">
          {channels.length === 0 && !channelsQuery.error ? (
            <p className="text-muted-foreground text-sm">{t("settings.notifications.noChannels")}</p>
          ) : null}
          {channels.map((channel) => {
            const Icon = CHANNEL_ICONS[channel.type];
            return (
              <CardFrame key={channel.id}>
                <CardFrameHeader>
                  <CardFrameTitle>
                    <span className="flex items-center gap-2">
                      <Icon aria-hidden="true" className="size-4" />
                      {channel.name}
                    </span>
                  </CardFrameTitle>
                  <CardFrameDescription>{t(`welcome.channelTypes.${channel.type}`)}</CardFrameDescription>
                  <CardFrameAction className="flex flex-wrap items-center gap-2">
                    <SettingSwitch
                      label={t("settings.notifications.channelEnabled")}
                      checked={channel.enabled}
                      onCheckedChange={(enabled) => void handleChannelEnabled(channel, enabled)}
                      className="items-center"
                    />
                    <Button
                      type="button"
                      size="sm"
                      variant="outline"
                      loading={channelTestMutation.pending && testingChannelId === channel.id}
                      onClick={() => void handleTestSend(channel.id)}
                    >
                      {t("settings.notifications.sendTest")}
                    </Button>
                  </CardFrameAction>
                </CardFrameHeader>
              </CardFrame>
            );
          })}
        </CardPanel>
      </Card>

      <div className="flex flex-col gap-2">
        <h2 className="font-medium text-lg">{t("settings.notifications.routingTitle")}</h2>
        <p className="text-muted-foreground text-sm">{t("settings.notifications.routingDescription")}</p>
        <DataTable
          rows={NOTIFICATION_EVENT_TYPES.map((eventType) => routingDraft[eventType])}
          getRowKey={(row) => row.eventType}
          columns={[
            {
              id: ROUTING_COLUMN_EVENT,
              header: t("settings.notifications.columns.event"),
              cell: (row) => t(`topBar.notifications.eventTypes.${row.eventType}`),
            },
            {
              id: ROUTING_COLUMN_SEVERITY,
              header: t("settings.notifications.columns.severity"),
              cell: (row) => (
                <Select
                  value={row.severity}
                  onValueChange={(value) =>
                    value && updateRouting(row.eventType, { severity: value as NotificationLevel })
                  }
                >
                  <SelectTrigger className="w-full max-w-40">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectPopup>
                    {NOTIFICATION_LEVELS.map((level) => (
                      <SelectItem key={level} value={level}>
                        {t(`settings.notifications.levels.${level}`)}
                      </SelectItem>
                    ))}
                  </SelectPopup>
                </Select>
              ),
            },
            ...channels.map((channel) => ({
              id: channel.id,
              header: channel.name,
              cell: (row: NotificationRoutingEntry) => (
                <Checkbox
                  checked={row.channelIds.includes(channel.id)}
                  onCheckedChange={(checked) => {
                    const channelIds = checked
                      ? [...row.channelIds, channel.id]
                      : row.channelIds.filter((id) => id !== channel.id);
                    updateRouting(row.eventType, { channelIds });
                  }}
                  aria-label={t("settings.notifications.routeToChannel", {
                    event: t(`topBar.notifications.eventTypes.${row.eventType}`),
                    channel: channel.name,
                  })}
                />
              ),
            })),
          ]}
        />
      </div>

      <Card>
        <CardHeader>
          <CardTitle>{t("settings.notifications.quietHoursTitle")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          <SettingSwitch
            label={t("settings.notifications.quietHoursEnabled")}
            description={t("settings.notifications.quietHoursDescription")}
            checked={quietHoursDraft.enabled}
            onCheckedChange={(enabled) =>
              setQuietHoursDraft((current) => current && { ...current, enabled })
            }
          />
          <div className="grid gap-4 sm:grid-cols-2">
            <Field>
              <FieldLabel>{t("settings.notifications.quietHoursStart")}</FieldLabel>
              <Input
                type="time"
                value={quietHoursDraft.start}
                disabled={!quietHoursDraft.enabled}
                onChange={(event) =>
                  setQuietHoursDraft((current) => current && { ...current, start: event.target.value })
                }
              />
            </Field>
            <Field>
              <FieldLabel>{t("settings.notifications.quietHoursEnd")}</FieldLabel>
              <Input
                type="time"
                value={quietHoursDraft.end}
                disabled={!quietHoursDraft.enabled}
                onChange={(event) =>
                  setQuietHoursDraft((current) => current && { ...current, end: event.target.value })
                }
              />
            </Field>
          </div>
          <SettingSwitch
            label={t("settings.notifications.criticalAlwaysDelivers")}
            description={t("settings.notifications.criticalAlwaysDeliversDescription")}
            checked={quietHoursDraft.criticalAlwaysDelivers}
            disabled
          />
        </CardPanel>
        <CardFooter className="flex justify-end gap-2 border-t">
          <Button type="button" variant="outline" disabled={!formDirty || saving} onClick={handleCancel}>
            {t("settings.actions.cancel")}
          </Button>
          <Button type="button" loading={saving} disabled={!formDirty} onClick={() => void handleSave()}>
            {t("settings.actions.save")}
          </Button>
        </CardFooter>
      </Card>

      <FormOverlay
        open={addOpen}
        onOpenChange={(open) => {
          setAddOpen(open);
          if (open) {
            setAddValidationError(null);
            addChannelMutation.reset();
          }
        }}
        title={t("settings.notifications.addChannel")}
        description={t("settings.notifications.addChannelDescription")}
        footer={
          <div className="flex justify-end gap-2">
            <Button type="button" variant="outline" onClick={() => setAddOpen(false)}>
              {t("settings.actions.cancel")}
            </Button>
            <Button type="button" loading={addChannelMutation.pending} onClick={() => void handleAddChannel()}>
              {t("settings.notifications.addChannel")}
            </Button>
          </div>
        }
      >
        {addValidationError ?? addChannelMutation.error ? (
          <Banner tone="error" title={addValidationError ?? addChannelMutation.error ?? ""} />
        ) : null}
        <Field>
          <FieldLabel>{t("settings.notifications.channelType")}</FieldLabel>
          <Select
            value={channelType}
            onValueChange={(value) => value && setChannelType(value as NotificationChannelType)}
          >
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectPopup>
              {(Object.keys(CHANNEL_ICONS) as NotificationChannelType[]).map((type) => {
                const Icon = CHANNEL_ICONS[type];
                return (
                  <SelectItem key={type} value={type}>
                    <span className="flex items-center gap-2">
                      <Icon aria-hidden="true" className="size-4" />
                      {t(`welcome.channelTypes.${type}`)}
                    </span>
                  </SelectItem>
                );
              })}
            </SelectPopup>
          </Select>
        </Field>
        <Field>
          <FieldLabel>{t("settings.notifications.channelName")}</FieldLabel>
          <Input
            value={channelName}
            onChange={(event) => setChannelName(event.target.value)}
            placeholder={t("welcome.fields.channelNamePlaceholder")}
          />
        </Field>
        <Field>
          <FieldLabel>{t("settings.notifications.channelSecret")}</FieldLabel>
          <SecretInput
            value={channelSecret}
            onChange={setChannelSecret}
            placeholder={t("welcome.fields.channelSecretPlaceholder")}
          />
        </Field>
      </FormOverlay>
    </div>
  );
}
