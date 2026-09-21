import type { SegmentedChoiceOption } from "@/components/patterns/segmented-choice";

export const SHARE_ACCESS_NONE = "none";
const SHARE_ACCESS_READ_ONLY = "read-only";
const SHARE_ACCESS_READ_WRITE = "read-write";

export function shareAccessOptions(t: (key: string) => string): SegmentedChoiceOption[] {
  return [
    { value: SHARE_ACCESS_NONE, label: t("shares.detail.smb.access.none") },
    { value: SHARE_ACCESS_READ_ONLY, label: t("shares.detail.smb.access.readOnly") },
    { value: SHARE_ACCESS_READ_WRITE, label: t("shares.detail.smb.access.readWrite") },
  ];
}
