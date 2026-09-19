import type { ReactNode } from "react";

import { CardFrame } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

export interface DataTableColumn<T> {
  id: string;
  header: ReactNode;
  cell: (row: T) => ReactNode;
  className?: string;
}

export function DataTable<T>({
  columns,
  rows,
  getRowKey,
  rowDisabled,
  rowDisabledReason,
}: {
  columns: DataTableColumn<T>[];
  rows: T[];
  getRowKey: (row: T) => string;
  rowDisabled?: (row: T) => boolean;
  rowDisabledReason?: (row: T) => ReactNode | undefined;
}): React.ReactElement {
  return (
    <CardFrame>
      <Table>
        <TableHeader>
          <TableRow>
            {columns.map((column) => (
              <TableHead key={column.id} className={column.className}>{column.header}</TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row) => {
            const disabled = rowDisabled?.(row) ?? false;
            const reason = disabled ? rowDisabledReason?.(row) : undefined;
            return (
              <TableRow key={getRowKey(row)} data-state={disabled ? "disabled" : undefined}>
                {columns.map((column, columnIndex) => (
                  <TableCell key={column.id} className={column.className}>
                    {column.cell(row)}
                    {columnIndex === 0 && reason ? (
                      <span className="sr-only">{reason}</span>
                    ) : null}
                  </TableCell>
                ))}
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </CardFrame>
  );
}
