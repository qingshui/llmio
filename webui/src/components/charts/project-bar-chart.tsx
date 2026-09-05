"use client"

import { useState } from "react"
import { Bar, BarChart, CartesianGrid, LabelList, XAxis, YAxis } from "recharts"

import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import {
  type ChartConfig,
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart"
import type { ProjectCount, ProjectMetrics } from "@/lib/api"

const metricLabels = {
  calls: "调用次数",
  tokens: "Token 量",
} as const

type MetricKey = keyof typeof metricLabels

// Token 量数值很大（十亿级），用紧凑格式展示
const compactFormatter = new Intl.NumberFormat("zh", { notation: "compact", maximumFractionDigits: 1 })

const predefinedColors = [
  "var(--chart-1)",
  "var(--chart-2)",
  "var(--chart-3)",
  "var(--chart-4)",
  "var(--chart-5)",
  "var(--chart-6)",
  "var(--chart-7)",
  "var(--chart-8)",
  "var(--chart-9)",
  "var(--chart-10)",
]

const generateChartConfig = (): ChartConfig => ({
  calls: {
    label: metricLabels.calls,
  },
  tokens: {
    label: metricLabels.tokens,
  },
})

const generateChartData = (data: ProjectCount[]) => {
  return data.map((item, index) => ({
    project: item.project,
    calls: item.calls,
    tokens: item.tokens,
    fill: predefinedColors[index % predefinedColors.length],
  }))
}

const formatValue = (value: number, mode: MetricKey) =>
  mode === "tokens" ? compactFormatter.format(value) : value.toLocaleString()

interface ProjectRankingChartProps {
  data: ProjectMetrics
}

export function ProjectRankingChart({ data }: ProjectRankingChartProps) {
  const [mode, setMode] = useState<MetricKey>("calls")

  const chartData = generateChartData(data[mode])
  const chartConfig = generateChartConfig()

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between">
        <CardTitle>项目排行</CardTitle>
        <div className="flex items-center gap-1">
          {(Object.keys(metricLabels) as MetricKey[]).map((key) => (
            <Button
              key={key}
              variant={mode === key ? "secondary" : "ghost"}
              size="sm"
              className="h-7 px-2 text-xs"
              onClick={() => setMode(key)}
            >
              {metricLabels[key]}
            </Button>
          ))}
        </div>
      </CardHeader>
      <CardContent>
        <ChartContainer config={chartConfig} className="aspect-auto h-[320px] w-full">
          <BarChart
            accessibilityLayer
            data={chartData}
            barSize={32}
          >
            <CartesianGrid vertical={false} />
            <XAxis
              dataKey="project"
              tickLine={false}
              axisLine={false}
              tickMargin={16}
              interval={0}
              tickFormatter={(value) => String(value)}
            />
            <YAxis
              dataKey={mode}
              tickLine={false}
              axisLine={false}
              width={60}
              tickFormatter={(value) => formatValue(Number(value), mode)}
            />
            <ChartTooltip
              cursor={false}
              content={<ChartTooltipContent indicator="line" hideLabel />}
            />
            <Bar
              dataKey={mode}
              fill="var(--color-calls)"
              radius={[8, 8, 0, 0]}
            >
              <LabelList
                dataKey={mode}
                position="top"
                offset={12}
                className="fill-foreground font-medium"
                fontSize={12}
                formatter={(value: number) => formatValue(value, mode)}
              />
            </Bar>
          </BarChart>
        </ChartContainer>
      </CardContent>
    </Card>
  )
}
