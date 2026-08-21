import { Fragment, useEffect, useMemo, useRef, useState, type KeyboardEvent, type PointerEvent, type ReactNode } from "react";
import KeyboardArrowDown from "@mui/icons-material/KeyboardArrowDown";
import KeyboardArrowRight from "@mui/icons-material/KeyboardArrowRight";
import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TableSortLabel from "@mui/material/TableSortLabel";
import Typography from "@mui/material/Typography";
import {
  chartCurrencyPolicy,
  chartDateTickIndexes,
  chartScale,
  chartSeriesDescription,
  chartViewBoxLayout,
  chartViewBoxPoint,
  chartViewportX,
  coverageStateLabel,
  featureLabels,
  featureTokenPercentage,
  formatChartCost,
  formatExactCost,
  formatExactTokens,
  nearestChartDataIndex,
  totalTokens,
  uncachedInputTokens,
} from "../lib/aiUsage";
import type { AIUsageDaily, AIUsageReport } from "../types/usage";
import { DetailSectionBand } from "./DetailSectionBand";
import { UsageCoverageStatus } from "./AIUsageCoverage";
import { overviewTypography } from "../theme/overview";

function costNumber(nanos?: string): number | null {
  if (!nanos) return null;
  const value = Number(nanos);
  return Number.isFinite(value) ? value / 1_000_000_000 : null;
}

function linePath(values: Array<number | null>, width: number, height: number, max: number): string {
  if (values.length === 0 || max <= 0) return "";
  let drawing = false; let path = "";
  values.forEach((value, index) => {
    if (value === null) { drawing = false; return; }
    const x = values.length === 1 ? width / 2 : index / (values.length - 1) * width;
    const y = height - value / max * height;
    path += `${drawing ? " L" : "M"}${x.toFixed(2)} ${y.toFixed(2)}`;
    drawing = true;
  });
  return path;
}

function chartDateLabel(date: string): string {
  const parsed = new Date(`${date}T00:00:00Z`);
  return Number.isNaN(parsed.valueOf()) ? date : new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", timeZone: "UTC" }).format(parsed);
}

function DailyCostChart({ days, mixedCurrency }: { days: AIUsageDaily[]; mixedCurrency: boolean }) {
  const [activeIndex, setActiveIndex] = useState<number | null>(null);
  const [tooltipLeft, setTooltipLeft] = useState<number | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const width = 960; const height = 270;
  const plot = { left: 70, right: 18, top: 24, bottom: 42 };
  const plotWidth = width - plot.left - plot.right; const plotHeight = height - plot.top - plot.bottom;
  const recordedCandidates = days.map((day) => day.recorded_currency && (day.recorded_cost_status === "available" || day.recorded_cost_status === "partial") ? costNumber(day.totals.estimated_cost_nanos) : null);
  const currentCandidates = days.map((day) => day.current_rate_currency && day.current_rate_status !== "unavailable" ? costNumber(day.current_rate_estimated_cost_nanos) : null);
  const recordedCurrency = days.find((_, index) => recordedCandidates[index] !== null)?.recorded_currency;
  const currentCurrency = days.find((_, index) => currentCandidates[index] !== null)?.current_rate_currency;
  const currencyPolicy = chartCurrencyPolicy(recordedCurrency, currentCurrency, mixedCurrency);
  const recorded = currencyPolicy.showRecorded ? recordedCandidates : recordedCandidates.map(() => null);
  const current = currencyPolicy.showCurrent ? currentCandidates : currentCandidates.map(() => null);
  const availableIndexes = days.flatMap((_, index) => recorded[index] !== null || current[index] !== null ? [index] : []);
  const rawMax = Math.max(0, ...recorded.map((value) => value ?? 0), ...current.map((value) => value ?? 0));
  const peakIndex = rawMax > 0 ? days.findIndex((_, index) => recorded[index] === rawMax || current[index] === rawMax) : -1;
  const peakDay = peakIndex >= 0 ? days[peakIndex] : null;
  const scale = chartScale(rawMax, availableIndexes.length > 0); const ticks = scale.ticks; const axisMax = scale.max;
  const recordedPath = linePath(recorded, plotWidth, plotHeight, axisMax);
  const currentPath = linePath(current, plotWidth, plotHeight, axisMax);
  const matchingSeries = Boolean(recordedPath && currentPath) && recorded.every((value, index) => value === current[index]);
  const visibleCurrentPath = matchingSeries ? "" : currentPath;
  const xForIndex = (index: number) => plot.left + (days.length === 1 ? plotWidth / 2 : index / (days.length - 1) * plotWidth);
  const yForValue = (value: number) => plot.top + plotHeight - value / axisMax * plotHeight;
  const chartCurrency = current.some((value) => value !== null) ? currentCurrency : recordedCurrency;
  const seriesDescription = `${chartSeriesDescription(Boolean(recordedPath), Boolean(currentPath))}${matchingSeries ? " Current-rate values match the recorded estimates for every comparable day." : ""}`;
  const selectedIndex = activeIndex !== null && availableIndexes.includes(activeIndex) ? activeIndex : null;
  const activeDay = selectedIndex === null ? null : days[selectedIndex];
  const activeX = selectedIndex === null ? null : xForIndex(selectedIndex);
  const tooltipTransform = selectedIndex !== null && selectedIndex <= Math.max(1, days.length * .2) ? "translateX(0)" : selectedIndex !== null && selectedIndex >= days.length * .8 ? "translateX(-100%)" : "translateX(-50%)";
  useEffect(() => {
    const scroller = scrollRef.current;
    if (!scroller) return;
    scroller.scrollLeft = Math.max(0, scroller.scrollWidth - scroller.clientWidth);
  }, [days]);

  const clearActiveDay = () => {
    setActiveIndex(null);
    setTooltipLeft(null);
  };
  const selectDay = (index: number | null, svg: SVGSVGElement) => {
    setActiveIndex(index);
    if (index === null) {
      setTooltipLeft(null);
      return;
    }
    const bounds = svg.getBoundingClientRect();
    const layout = chartViewBoxLayout(bounds.width, bounds.height, width, height);
    setTooltipLeft(chartViewportX(xForIndex(index), layout));
  };
  const selectPointerDay = (event: PointerEvent<SVGSVGElement>) => {
    const bounds = event.currentTarget.getBoundingClientRect();
    const layout = chartViewBoxLayout(bounds.width, bounds.height, width, height);
    const { x: svgX, y: svgY } = chartViewBoxPoint(event.clientX - bounds.left, event.clientY - bounds.top, layout);
    if (svgX < 0 || svgX > width || svgY < 0 || svgY > height) {
      clearActiveDay();
      return;
    }
    const target = days.length <= 1 ? 0 : Math.round(Math.max(0, Math.min(plotWidth, svgX - plot.left)) / plotWidth * (days.length - 1));
    selectDay(nearestChartDataIndex(target, availableIndexes), event.currentTarget);
  };
  const selectKeyboardDay = (event: KeyboardEvent<SVGSVGElement>) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const position = selectedIndex === null ? -1 : availableIndexes.indexOf(selectedIndex);
    if (event.key === "Home") selectDay(availableIndexes[0] ?? null, event.currentTarget);
    else if (event.key === "End") selectDay(availableIndexes.at(-1) ?? null, event.currentTarget);
    else if (event.key === "ArrowLeft") selectDay(availableIndexes[Math.max(0, position < 0 ? availableIndexes.length - 1 : position - 1)] ?? null, event.currentTarget);
    else selectDay(availableIndexes[Math.min(availableIndexes.length - 1, position < 0 ? 0 : position + 1)] ?? null, event.currentTarget);
  };
  if (availableIndexes.length === 0) return <Box sx={{ py: 5, textAlign: "center" }}><Typography color="textSecondary">No comparable single-currency daily cost values are available in this range.</Typography>{currencyPolicy.note && <Typography variant="caption" color="textSecondary">{currencyPolicy.note}</Typography>}</Box>;
  return <Box>
    <Box ref={scrollRef} sx={{ overflowX: "auto", pb: .5, overscrollBehaviorInline: "contain" }}>
      <Box sx={{ position: "relative", minWidth: { xs: 720, md: 0 } }}>
        <Box
          component="svg"
        viewBox={`0 0 ${width} ${height}`}
        preserveAspectRatio="xMidYMid meet"
        role="img"
        tabIndex={0}
        aria-labelledby="daily-cost-chart-title"
        aria-describedby="daily-cost-chart-desc daily-cost-chart-summary daily-usage-ledger-summary"
        onPointerMove={selectPointerDay}
        onPointerLeave={clearActiveDay}
        onFocus={(event) => selectDay(activeIndex !== null && availableIndexes.includes(activeIndex) ? activeIndex : availableIndexes.at(-1) ?? null, event.currentTarget)}
        onBlur={clearActiveDay}
        onKeyDown={selectKeyboardDay}
        sx={{ width: "100%", height: { xs: 230, md: 285 }, display: "block", borderRadius: "4px", outline: "none", "&:focus-visible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: -2 } }}
      >
        <title id="daily-cost-chart-title">Daily AI cost estimates</title>
        <desc id="daily-cost-chart-desc">{seriesDescription}{currencyPolicy.note ? ` ${currencyPolicy.note}` : ""}</desc>
        <text x={plot.left} y="13" fill="var(--mui-palette-text-secondary)" fontSize="11">{chartCurrency ? `${chartCurrency} per UTC day` : "Cost per UTC day"}</text>
        {ticks.map((tick) => {
          const y = yForValue(tick);
          return <g key={tick}>
            <line x1={plot.left} x2={plot.left + plotWidth} y1={y} y2={y} stroke="var(--mui-palette-divider)" opacity="0.7" />
            <text x={plot.left - 10} y={y + 4} textAnchor="end" fill="var(--mui-palette-text-secondary)" fontSize="11">{formatChartCost(tick, chartCurrency)}</text>
          </g>;
        })}
        {chartDateTickIndexes(days.length).map((index) => <text key={days[index].date} x={xForIndex(index)} y={plot.top + plotHeight + 27} textAnchor={index === 0 ? "start" : index === days.length - 1 ? "end" : "middle"} fill="var(--mui-palette-text-secondary)" fontSize="11">{chartDateLabel(days[index].date)}</text>)}
        <g transform={`translate(${plot.left} ${plot.top})`}>
          {visibleCurrentPath && <path d={visibleCurrentPath} fill="none" stroke="var(--mui-palette-warning-main)" strokeWidth="3" strokeDasharray="9 7" strokeLinecap="round" />}
          {recordedPath && <path d={recordedPath} fill="none" stroke="var(--mui-palette-primary-main)" strokeWidth="3" strokeLinecap="round" />}
          {!matchingSeries && current.map((value, index) => value === null ? null : <rect key={`current-${days[index].date}`} x={xForIndex(index) - plot.left - 3.5} y={yForValue(value) - plot.top - 3.5} width="7" height="7" fill="var(--mui-palette-warning-main)" stroke="var(--mui-palette-background-default)" strokeWidth="1.5" />)}
          {recorded.map((value, index) => value === null ? null : <circle key={`recorded-${days[index].date}`} cx={xForIndex(index) - plot.left} cy={yForValue(value) - plot.top} r="3.5" fill="var(--mui-palette-primary-main)" stroke="var(--mui-palette-background-default)" strokeWidth="1.5" />)}
        </g>
        {activeX !== null && <line x1={activeX} x2={activeX} y1={plot.top} y2={plot.top + plotHeight} stroke="var(--mui-palette-text-secondary)" strokeDasharray="3 5" opacity="0.7" />}
        {!matchingSeries && selectedIndex !== null && current[selectedIndex] !== null && <rect x={(activeX ?? 0) - 6.5} y={yForValue(current[selectedIndex] ?? 0) - 6.5} width="13" height="13" fill="var(--mui-palette-background-paper)" stroke="var(--mui-palette-warning-main)" strokeWidth="3" />}
        {selectedIndex !== null && recorded[selectedIndex] !== null && <circle cx={activeX ?? 0} cy={yForValue(recorded[selectedIndex] ?? 0)} r="7" fill="var(--mui-palette-background-paper)" stroke="var(--mui-palette-primary-main)" strokeWidth="3" />}
        </Box>
        {activeDay && tooltipLeft !== null && <Box role="status" aria-live="polite" sx={{ position: "absolute", top: 22, left: tooltipLeft, transform: tooltipTransform, minWidth: 210, maxWidth: 270, p: 1.25, borderRadius: "4px", border: "1px solid", borderColor: "divider", bgcolor: "background.paper", boxShadow: "none", pointerEvents: "none", zIndex: 1 }}>
        <Typography variant="subtitle2" sx={{ fontFamily: "monospace" }}>{activeDay.date} UTC{activeDay.current_partial_utc ? " · Partial UTC day" : ""}</Typography>
        {recordedPath && <Typography variant="caption" component="div" sx={{ mt: .5 }}><Box component="span" sx={{ color: "primary.main" }}>●</Box> Recorded: {recordedCost(activeDay)}</Typography>}
        {currentPath && <Typography variant="caption" component="div"><Box component="span" sx={{ color: "warning.main" }}>■</Box> Current rate: {currentRateCost(activeDay)}{matchingSeries ? " · matches recorded" : ""}</Typography>}
        <Typography variant="caption" component="div" color="textSecondary">Coverage: {activeDay.coverage.status}{activeDay.coverage.states?.length ? ` · ${activeDay.coverage.states.map(coverageStateLabel).join(", ")}` : ""}</Typography>
        </Box>}
      </Box>
    </Box>
    <Typography color="textSecondary" sx={{ display: { xs: "block", md: "none" }, mt: 0.5, ...overviewTypography.description }}>
      Newest dates are in view. Scroll left for earlier dates.
    </Typography>
    <Box sx={{ display: "flex", gap: 2, flexWrap: "wrap", alignItems: "center", mt: .5 }}>
      {recordedPath && <Typography variant="caption"><Box component="span" sx={{ display: "inline-block", width: 22, borderTop: "3px solid", borderColor: "primary.main", mr: .8, verticalAlign: "middle" }} />Recorded estimate (solid)</Typography>}
      {visibleCurrentPath && <Typography variant="caption"><Box component="span" sx={{ display: "inline-block", width: 22, borderTop: "3px dashed", borderColor: "warning.main", mr: .8, verticalAlign: "middle" }} />Current-rate estimate (dashed)</Typography>}
      {matchingSeries && <Typography variant="caption" color="textSecondary">Current-rate estimate matches the recorded estimate.</Typography>}
      <Typography variant="caption" color="textSecondary">Hover to inspect. Keyboard: focus chart, then use ← and →.</Typography>
      {peakDay && <Typography variant="caption" color="textSecondary">Peak day: {peakDay.date} · {formatChartCost(rawMax, chartCurrency)}.</Typography>}
      {rawMax === 0 && <Typography variant="caption" color="textSecondary">All reported values in this chart are {formatChartCost(0, chartCurrency)}.</Typography>}
      {currencyPolicy.note && <Typography variant="caption" color="textSecondary" sx={{ flexBasis: "100%" }}>{currencyPolicy.note}</Typography>}
    </Box>
  </Box>;
}

function recordedCost(day: AIUsageDaily): string {
  if (day.recorded_cost_status === "mixed_currency") return "Mixed currencies";
  if (day.recorded_cost_status === "unavailable") return "Unavailable";
  if (day.recorded_cost_status === "unknown") return "Unknown";
  return formatExactCost(day.totals.estimated_cost_nanos, day.recorded_currency);
}
function currentRateCost(day: AIUsageDaily): string {
  if (day.current_rate_status === "unavailable") return "Unavailable";
  return formatExactCost(day.current_rate_estimated_cost_nanos, day.current_rate_currency);
}

function cacheWriteValue(day: AIUsageDaily): string {
  const reported = day.totals.cache_write_reported_requests ?? 0;
  const tokens = day.totals.cache_write_input_tokens ?? 0;
  return reported > 0 || tokens > 0 ? formatExactTokens(tokens) : "Not reported";
}

function PartialDaySignal() {
  return (
    <Box component="span" sx={{ display: "inline-flex", alignItems: "center", gap: 0.5, color: "warning.main", fontSize: "12.5px", lineHeight: "18px", fontWeight: 700 }}>
      <Box component="span" aria-hidden="true" sx={{ width: 7, height: 7, borderRadius: "2px", bgcolor: "currentColor" }} />
      Partial UTC day
    </Box>
  );
}

function FeatureBreakdown({ day, currency }: { day: AIUsageDaily; currency?: string }) {
  if (day.features.length === 0) {
    return <Typography color="textSecondary" sx={overviewTypography.secondaryBody}>No feature activity was recorded for this UTC day.</Typography>;
  }
  return (
    <Box>
      {day.features.map((row) => (
        <Box
          key={row.feature}
          sx={{
            display: "grid",
            gridTemplateColumns: { xs: "1fr 1fr", md: "minmax(180px, 1fr) repeat(4, minmax(100px, auto))" },
            gap: 1,
            alignItems: "baseline",
            py: 1,
            borderBottom: "1px solid",
            borderColor: "divider",
            "&:last-child": { borderBottom: 0 },
          }}
        >
          <Typography sx={{ ...overviewTypography.secondaryBody, fontWeight: 700 }}>{featureLabels[row.feature]}</Typography>
          <Typography sx={overviewTypography.data}>{formatExactTokens(row.totals.operations)} operations</Typography>
          <Typography sx={overviewTypography.data}>{formatExactTokens(row.totals.model_requests)} requests</Typography>
          <Typography sx={overviewTypography.data}>{formatExactTokens(totalTokens(row.totals))} tokens</Typography>
          <Typography sx={overviewTypography.data}>{formatExactCost(row.totals.estimated_cost_nanos, currency)}</Typography>
        </Box>
      ))}
    </Box>
  );
}

const numericCell = { ...overviewTypography.data, whiteSpace: "nowrap" } as const;

function coverageDetails(day: AIUsageDaily): string {
  const states = day.coverage.states ?? [];
  return states.length
    ? states.map(coverageStateLabel).join(" · ")
    : day.coverage.status.charAt(0).toUpperCase() + day.coverage.status.slice(1);
}

export function DaySummaryButton({
  day,
  open,
  controls,
  onToggle,
}: {
  day: AIUsageDaily;
  open: boolean;
  controls: string;
  onToggle: () => void;
}) {
  const usageSummary = day.has_usage
    ? `${formatExactTokens(day.totals.operations)} operations. ${formatExactTokens(day.totals.model_requests)} requests. ${formatExactTokens(day.totals.cache_hits)} cache hits.`
    : "No usage recorded.";
  const coverageSummary = day.coverage.status === "unavailable"
    ? "Coverage unavailable."
    : `${day.coverage.status.charAt(0).toUpperCase() + day.coverage.status.slice(1)} coverage.`;
  const accessibleName = [
    `${open ? "Collapse" : "Expand"} feature breakdown for ${day.date}.`,
    usageSummary,
    `Recorded estimate ${recordedCost(day)}.`,
    `Current-rate reprice ${currentRateCost(day)}.`,
    coverageSummary,
    day.current_partial_utc ? "Partial UTC day." : "",
  ].filter(Boolean).join(" ");

  return (
    <ButtonBase
      type="button"
      onClick={onToggle}
      aria-expanded={open}
      aria-controls={controls}
      aria-label={accessibleName}
      sx={{
        width: "100%",
        minHeight: 44,
        display: "grid",
        gridTemplateColumns: "minmax(0, 1fr) 44px",
        gridTemplateAreas: '"date chevron" "summary chevron" "cost chevron" "coverage chevron"',
        color: "text.primary",
        textAlign: "left",
        borderTop: "1px solid",
        borderColor: "divider",
        opacity: day.has_usage ? 1 : 0.72,
        "&:hover": { bgcolor: "surface.containerHigh" },
        "&.Mui-focusVisible": {
          outline: "2px solid",
          outlineColor: "primary.main",
          outlineOffset: -2,
        },
      }}
    >
      <Box sx={{ gridArea: "date", px: 1.5, pt: 1.25, display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap" }}>
        <Typography component="time" dateTime={day.date} sx={{ ...overviewTypography.data, fontWeight: 700 }}>{day.date}</Typography>
        {day.current_partial_utc && <PartialDaySignal />}
      </Box>
      <Typography color="textSecondary" sx={{ gridArea: "summary", px: 1.5, pt: 0.25, ...overviewTypography.data }}>
        {day.has_usage
          ? `${formatExactTokens(day.totals.operations)} operations · ${formatExactTokens(day.totals.model_requests)} requests · ${formatExactTokens(day.totals.cache_hits)} cache hits`
          : "No usage recorded"}
      </Typography>
      <Typography sx={{ gridArea: "cost", px: 1.5, pt: 0.25, ...overviewTypography.data }}>
        Recorded {recordedCost(day)} · Current rate {currentRateCost(day)}
      </Typography>
      <Box sx={{ gridArea: "coverage", px: 1.5, pt: 0.25, pb: 1.25, minWidth: 0 }}>
        <UsageCoverageStatus status={day.coverage.status} />
        <Typography component="span" color="textSecondary" sx={{ ml: 1, ...overviewTypography.description }}>
          {coverageDetails(day)}
        </Typography>
      </Box>
      <Box sx={{ gridArea: "chevron", alignSelf: "stretch", display: "grid", placeItems: "center", color: "text.secondary" }}>
        {open ? <KeyboardArrowDown aria-hidden="true" /> : <KeyboardArrowRight aria-hidden="true" />}
      </Box>
    </ButtonBase>
  );
}

export function HistoricalTable({ days }: { days: AIUsageDaily[] }) {
  const [ascending, setAscending] = useState(false);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const sorted = useMemo(
    () => [...days].sort((a, b) => ascending ? a.date.localeCompare(b.date) : b.date.localeCompare(a.date)),
    [ascending, days],
  );
  const toggle = (date: string) => setExpanded((current) => {
    const next = new Set(current);
    if (next.has(date)) next.delete(date); else next.add(date);
    return next;
  });

  return (
    <>
      <TableContainer sx={{ display: { xs: "none", lg: "block" }, overflowX: "auto" }}>
        <Table size="small" aria-label="Historical daily AI usage and cost" sx={{ minWidth: 1280, "& .MuiTableCell-root": { px: 1, borderColor: "divider" } }}>
          <TableHead>
            <TableRow>
              <TableCell padding="checkbox" />
              <TableCell sortDirection={ascending ? "asc" : "desc"}>
                <TableSortLabel active direction={ascending ? "asc" : "desc"} onClick={() => setAscending((value) => !value)}>
                  UTC date
                </TableSortLabel>
              </TableCell>
              {["Operations", "Requests", "Cache hits", "Uncached input", "Cached read", "Cache write", "Output", "Recorded estimate", "Current-rate reprice", "Coverage"].map((label) => (
                <TableCell key={label} align={label === "Coverage" ? "left" : "right"} sx={{ minWidth: label === "Coverage" ? 220 : label.includes("estimate") || label.includes("reprice") ? 128 : undefined, ...overviewTypography.tableHeading }}>
                  {label}
                </TableCell>
              ))}
            </TableRow>
          </TableHead>
          <TableBody>
            {sorted.map((day) => {
              const open = expanded.has(day.date);
              const contentID = `usage-day-${day.date}`;
              return (
                <Fragment key={day.date}>
                  <TableRow hover sx={{ opacity: day.has_usage ? 1 : 0.7 }}>
                    <TableCell padding="checkbox">
                      <ButtonBase
                        type="button"
                        onClick={() => toggle(day.date)}
                        aria-label={`${open ? "Collapse" : "Expand"} feature breakdown for ${day.date}`}
                        aria-expanded={open}
                        aria-controls={contentID}
                        sx={{ width: 44, height: 44, borderRadius: "4px", "&.Mui-focusVisible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: -2 } }}
                      >
                        {open ? <KeyboardArrowDown /> : <KeyboardArrowRight />}
                      </ButtonBase>
                    </TableCell>
                    <TableCell component="th" scope="row" sx={{ minWidth: 170 }}>
                      <Typography component="time" dateTime={day.date} sx={{ ...overviewTypography.data, fontWeight: 700 }}>{day.date}</Typography>
                      {day.current_partial_utc && <Box sx={{ mt: 0.25 }}><PartialDaySignal /></Box>}
                    </TableCell>
                    <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.operations)}</TableCell>
                    <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.model_requests)}</TableCell>
                    <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.cache_hits)}</TableCell>
                    <TableCell align="right" sx={numericCell}>{formatExactTokens(uncachedInputTokens(day.totals))}</TableCell>
                    <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.cached_input_tokens)}</TableCell>
                    <TableCell align="right" sx={numericCell}>{cacheWriteValue(day)}</TableCell>
                    <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.output_tokens)}</TableCell>
                    <TableCell align="right" sx={numericCell}>{recordedCost(day)}</TableCell>
                    <TableCell align="right" sx={numericCell}>{currentRateCost(day)}</TableCell>
                    <TableCell sx={{ minWidth: 220 }}>
                      <UsageCoverageStatus status={day.coverage.status} />
                      <Typography color="textSecondary" sx={{ mt: 0.25, ...overviewTypography.description }}>{coverageDetails(day)}</Typography>
                    </TableCell>
                  </TableRow>
                  {open && (
                    <TableRow>
                      <TableCell colSpan={12} sx={{ py: 0, bgcolor: "surface.containerLow" }}>
                        <Collapse in timeout="auto">
                          <Box id={contentID} sx={{ px: 1.5, py: 1.5 }}>
                            <FeatureBreakdown day={day} currency={day.recorded_currency} />
                          </Box>
                        </Collapse>
                      </TableCell>
                    </TableRow>
                  )}
                </Fragment>
              );
            })}
          </TableBody>
        </Table>
      </TableContainer>

      <Box sx={{ display: { xs: "block", lg: "none" } }}>
        {sorted.map((day) => {
          const open = expanded.has(day.date);
          const contentID = `usage-mobile-day-${day.date}`;
          return (
            <Box component="article" key={day.date} sx={{ bgcolor: "surface.container" }}>
              <DaySummaryButton day={day} open={open} controls={contentID} onToggle={() => toggle(day.date)} />
              <Collapse in={open} timeout="auto" unmountOnExit>
                <Box id={contentID} sx={{ px: 1.5, py: 1.5, bgcolor: "surface.containerLow", borderTop: "1px solid", borderColor: "divider" }}>
                  <Box sx={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 1, pb: 1.25 }}>
                    {[
                      ["Uncached input", formatExactTokens(uncachedInputTokens(day.totals))],
                      ["Cached read", formatExactTokens(day.totals.cached_input_tokens)],
                      ["Cache write", cacheWriteValue(day)],
                      ["Output", formatExactTokens(day.totals.output_tokens)],
                    ].map(([label, value]) => (
                      <Box key={label}>
                        <Typography color="textSecondary" sx={overviewTypography.tableHeading}>{label}</Typography>
                        <Typography sx={overviewTypography.data}>{value}</Typography>
                      </Box>
                    ))}
                  </Box>
                  <FeatureBreakdown day={day} currency={day.recorded_currency} />
                </Box>
              </Collapse>
            </Box>
          );
        })}
      </Box>
    </>
  );
}

function FeatureMix({ data }: { data: AIUsageReport }) {
  const selectedTokens = totalTokens(data.totals);
  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Selected-range feature mix" metadata="Percentage of provider-reported selected-range tokens" />
      {data.features.length === 0 ? (
        <Box sx={{ minHeight: 140, display: "grid", placeItems: "center", px: 2, py: 3, textAlign: "center" }}>
          <Box>
            <Typography component="h3" sx={overviewTypography.categoryHeading}>No feature activity</Typography>
            <Typography color="textSecondary" sx={{ mt: 0.5, ...overviewTypography.secondaryBody }}>No provider-reported tokens were recorded for this selection.</Typography>
          </Box>
        </Box>
      ) : data.features.map((row) => {
        const tokens = totalTokens(row.totals);
        const percentage = featureTokenPercentage(tokens, selectedTokens);
        const percentageLabel = percentage > 0 && percentage < 0.1 ? "<0.1%" : `${percentage.toFixed(percentage % 1 === 0 ? 0 : 1)}%`;
        return (
          <Box key={row.feature} sx={{ display: "grid", gridTemplateColumns: { xs: "minmax(0, 1fr) auto", md: "minmax(190px, 1fr) 150px 80px" }, gap: 1, alignItems: "center", px: 1.5, py: 1.1, borderTop: "1px solid", borderColor: "divider" }}>
            <Typography sx={{ ...overviewTypography.secondaryBody, fontWeight: 700 }}>{featureLabels[row.feature]}</Typography>
            <Typography sx={{ ...overviewTypography.data, textAlign: { xs: "left", md: "right" } }}>{formatExactTokens(tokens)} tokens</Typography>
            <Typography sx={{ ...overviewTypography.data, textAlign: "right" }}>{percentageLabel}</Typography>
            <Box sx={{ gridColumn: "1 / -1", height: 5, borderRadius: "2px", bgcolor: "surface.containerHighest", overflow: "hidden" }}>
              <Box sx={{ width: `${percentage}%`, height: "100%", borderRadius: "2px", bgcolor: "primary.main" }} />
            </Box>
          </Box>
        );
      })}
    </Box>
  );
}

export function AIUsageDailySections({
  data,
  days,
  coverageSection,
}: {
  data: AIUsageReport;
  days: AIUsageDaily[];
  coverageSection?: ReactNode;
}) {
  const partialDay = days.find((day) => day.current_partial_utc)?.date;
  const rangeMetadata = `${data.range.start} to ${data.range.end} UTC${partialDay ? ` · ${partialDay} partial` : ""}`;
  return (
    <>
      <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
        <DetailSectionBand title="Daily cost chart" metadata={rangeMetadata} />
        <Box sx={{ px: { xs: 1.5, md: 2 }, py: 1.5 }}>
          <Typography id="daily-cost-chart-summary" color="textSecondary" sx={{ mb: 1, ...overviewTypography.description }}>
            UTC day boundaries for the selected range. The daily usage ledger below presents the same dates in newest-first order.
          </Typography>
          <DailyCostChart days={days} mixedCurrency={Boolean(data.mixed_currency)} />
        </Box>
      </Box>

      <FeatureMix data={data} />

      {coverageSection}

      <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
        <DetailSectionBand title="Daily usage ledger" metadata="Newest first · sortable by UTC date" />
        <Typography
          id="daily-usage-ledger-summary"
          component="span"
          sx={{ position: "absolute", width: "1px", height: "1px", p: 0, m: "-1px", overflow: "hidden", clip: "rect(0 0 0 0)", whiteSpace: "nowrap", border: 0 }}
        >
          The daily usage ledger lists the same UTC range as the chart, newest day first.
        </Typography>
        <HistoricalTable days={days} />
      </Box>
    </>
  );
}
