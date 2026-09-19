import type { TFunction } from "i18next";
import type { ChoiceCardOption } from "@/components/patterns/choice-cards";
import { PlainTerm } from "@/components/patterns/plain-term";
import type { CreatePolicy } from "@/routes/storage-setup/validation";

const POLICY_MSPMFS = "mspmfs";
const POLICY_MFS = "mfs";
const POLICY_LFS = "lfs";
const POLICY_FF = "ff";
const CREATE_POLICY_FIELD = "create-policy";

export function buildPoolPolicyOptions(t: TFunction): ChoiceCardOption[] {
  return [
    {
      value: POLICY_MSPMFS,
      title: <PlainTerm label={t("storageSetup.pool.policies.mspmfs.label")} term={t("storageSetup.pool.policies.mspmfs.term")} />,
      description: t("storageSetup.pool.policies.mspmfs.description"),
    },
    {
      value: POLICY_MFS,
      title: <PlainTerm label={t("storageSetup.pool.policies.mfs.label")} term={t("storageSetup.pool.policies.mfs.term")} />,
      description: t("storageSetup.pool.policies.mfs.description"),
    },
    {
      value: POLICY_LFS,
      title: <PlainTerm label={t("storageSetup.pool.policies.lfs.label")} term={t("storageSetup.pool.policies.lfs.term")} />,
      description: t("storageSetup.pool.policies.lfs.description"),
    },
    {
      value: POLICY_FF,
      title: <PlainTerm label={t("storageSetup.pool.policies.ff.label")} term={t("storageSetup.pool.policies.ff.term")} />,
      description: t("storageSetup.pool.policies.ff.description"),
    },
  ];
}

export function createPolicyFieldName(): string {
  return CREATE_POLICY_FIELD;
}

export function parseCreatePolicy(value: string): CreatePolicy {
  return value as CreatePolicy;
}
