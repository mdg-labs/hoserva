import type { components } from "@/lib/api/client";

export type ParityDiffCategory =
  | "removed"
  | "updated"
  | "added"
  | "moved"
  | "copied"
  | "moved_by_hoserva";

export interface ParityDiffGroup {
  category: ParityDiffCategory;
  paths: string[];
  count: number;
}

type ApiParityDiffGroup = components["schemas"]["ParityDiffGroup"];
type ApiParityDiffCategory = components["schemas"]["ParityDiffCategory"];

export const PARITY_DIFF_ORDER: ParityDiffCategory[] = [
  "removed",
  "updated",
  "added",
  "moved",
  "copied",
  "moved_by_hoserva",
];

export function sortParityDiffGroups(groups: ParityDiffGroup[]): ParityDiffGroup[] {
  const rank = new Map(PARITY_DIFF_ORDER.map((category, index) => [category, index]));
  return [...groups].sort(
    (left, right) => (rank.get(left.category) ?? 99) - (rank.get(right.category) ?? 99),
  );
}

export function parityDiffGroupsFromAPI(groups: ApiParityDiffGroup[]): ParityDiffGroup[] {
  return sortParityDiffGroups(
    groups.map((group) => ({
      category: apiCategoryToUi(group.category),
      paths: group.paths ?? [],
      count: group.count,
    })),
  );
}

function apiCategoryToUi(category: ApiParityDiffCategory): ParityDiffCategory {
  switch (category) {
    case "moved_by_hoserva":
      return "moved_by_hoserva";
    default:
      return category;
  }
}
