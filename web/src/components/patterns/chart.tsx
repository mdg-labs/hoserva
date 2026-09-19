import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

const CHART_LABEL_KEY = "label";
const CHART_VALUE_KEY = "value";
const CHART_GRID_DASH = "3 3";

import { Card, CardDescription, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { EmptyState } from "@/components/patterns/empty-state";
import { LineChart as LineChartIcon } from "lucide-react";

export interface ChartPoint {
  label: string;
  value: number;
}

export function TimeSeriesChart({
  title,
  description,
  data,
  valueFormatter,
  emptyTitle,
  emptyDescription,
}: {
  title: ReactNode;
  description?: ReactNode;
  data: ChartPoint[] | null;
  valueFormatter?: (value: number) => string;
  emptyTitle?: ReactNode;
  emptyDescription?: ReactNode;
}): React.ReactElement {
  const { t } = useTranslation();
  const hasData = data != null && data.length > 0;

  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        {description ? <CardDescription>{description}</CardDescription> : null}
      </CardHeader>
      <CardPanel className="h-48">
        {hasData ? (
          <ResponsiveContainer width="100%" height="100%">
            <AreaChart data={data}>
              <CartesianGrid strokeDasharray={CHART_GRID_DASH} className="stroke-border" />
              <XAxis dataKey={CHART_LABEL_KEY} tick={{ fontSize: 12 }} />
              <YAxis tick={{ fontSize: 12 }} />
              <Tooltip
                formatter={(value: number) =>
                  valueFormatter ? valueFormatter(value) : String(value)
                }
              />
              <Area
                type="monotone"
                dataKey={CHART_VALUE_KEY}
                className="fill-primary/20 stroke-primary"
                strokeWidth={2}
              />
            </AreaChart>
          </ResponsiveContainer>
        ) : (
          <EmptyState
            icon={LineChartIcon}
            title={emptyTitle ?? t("chart.emptyTitle")}
            description={emptyDescription ?? t("chart.emptyDescription")}
          />
        )}
      </CardPanel>
    </Card>
  );
}
