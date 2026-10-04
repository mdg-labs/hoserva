import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import type { CachePartition, ImportBlocker } from "@/routes/tools-migrate/mapping";
import { formatBytes } from "@/routes/storage-setup/config-preview";

export interface PartitionChoice extends CachePartition {
  sizeBytes: number;
}

export interface PartitionOptions {
  loading: boolean;
  error: string | null;
  partitions: PartitionChoice[];
  selected: string;
  onSelect: (device: string) => void;
}

function CachePartitionPicker({ options }: { options: PartitionOptions }): React.ReactElement {
  const { t } = useTranslation();

  return (
    <section className="flex flex-col gap-2" aria-label={t("toolsMigrate.review.import.partition.title")}>
      <h3 className="font-medium">{t("toolsMigrate.review.import.partition.title")}</h3>
      <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.import.partition.description")}</p>
      {options.error ? <Banner tone="error" title={options.error} /> : null}
      {options.loading ? <LoadingBlock rows={1} /> : null}
      {!options.loading && !options.error && options.partitions.length === 0 ? (
        <Banner tone="warning" title={t("toolsMigrate.review.import.partition.none")} />
      ) : null}
      {options.partitions.length > 0 ? (
        <Field>
          <FieldLabel>{t("toolsMigrate.review.import.partition.label")}</FieldLabel>
          <Select
            value={options.selected}
            onValueChange={(value) => value && options.onSelect(value)}
            items={options.partitions.map((partition) => ({
              value: partition.device,
              label: t("toolsMigrate.review.import.partition.option", {
                device: partition.device,
                size: formatBytes(partition.sizeBytes),
              }),
            }))}
          >
            <SelectTrigger aria-label={t("toolsMigrate.review.import.partition.label")}>
              <SelectValue placeholder={t("toolsMigrate.review.import.partition.placeholder")} />
            </SelectTrigger>
            <SelectContent>
              {options.partitions.map((partition) => (
                <SelectItem key={partition.device} value={partition.device}>
                  {t("toolsMigrate.review.import.partition.option", {
                    device: partition.device,
                    size: formatBytes(partition.sizeBytes),
                  })}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
      ) : null}
    </section>
  );
}

export function ImportConfirm({
  confirmed,
  onConfirmedChange,
  blocker,
  noGo,
  partition,
}: {
  confirmed: boolean;
  onConfirmedChange: (confirmed: boolean) => void;
  blocker: ImportBlocker | null;
  noGo: boolean;
  partition: PartitionOptions | null;
}): React.ReactElement {
  const { t } = useTranslation();

  return (
    <div className="flex flex-col gap-4">
      {partition ? <CachePartitionPicker options={partition} /> : null}
      <section className="flex flex-col gap-3" aria-label={t("toolsMigrate.review.import.title")}>
        <h3 className="font-medium">{t("toolsMigrate.review.import.title")}</h3>
        <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.import.description")}</p>
        {noGo ? <InlineNote description={t("toolsMigrate.review.import.noGo")} /> : null}
        {!noGo && blocker ? (
          <InlineNote description={t(`toolsMigrate.review.import.blockers.${blocker}`)} />
        ) : null}
        <Field className="flex-row items-start gap-2">
          <Checkbox
            checked={confirmed}
            onCheckedChange={(checked) => onConfirmedChange(checked === true)}
            aria-label={t("toolsMigrate.review.import.confirm")}
          />
          <div className="flex min-w-0 flex-col gap-1">
            <FieldLabel className="cursor-default">{t("toolsMigrate.review.import.confirm")}</FieldLabel>
            <FieldDescription>{t("toolsMigrate.review.import.confirmHint")}</FieldDescription>
          </div>
        </Field>
      </section>
    </div>
  );
}
