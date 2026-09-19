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
}

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
