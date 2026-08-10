import { Fragment, useEffect, useMemo, useState, type KeyboardEvent, type PointerEvent } from "react";
import Download from "@mui/icons-material/Download";
import KeyboardArrowDown from "@mui/icons-material/KeyboardArrowDown";
import KeyboardArrowRight from "@mui/icons-material/KeyboardArrowRight";
import Paid from "@mui/icons-material/Paid";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import CircularProgress from "@mui/material/CircularProgress";
import Collapse from "@mui/material/Collapse";
import IconButton from "@mui/material/IconButton";
import Link from "@mui/material/Link";
import MenuItem from "@mui/material/MenuItem";
import Stack from "@mui/material/Stack";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TableSortLabel from "@mui/material/TableSortLabel";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { Panel } from "../components/Panel";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import {
  chartDateTickIndexes,
  chartTickValues,
  featureLabels,
  formatChartCost,
  formatCost,
  formatCoverage,
  formatExactCost,
  formatExactTokens,
  formatTokens,
  nearestChartDataIndex,
  pricedRequestCoverageNote,
  totalTokens,
  uncachedInputTokens,
  usageQuery,
} from "../lib/aiUsage";
import type {
  AIUsageDaily,
  AIUsageFeature,
  AIUsageReport,
  AIUsageTotals,
} from "../types/usage";

const API_BASE = import.meta.env.BASE_URL;
const today = () => new Date().toISOString().slice(0, 10);
const daysAgo = (days: number) => {
  const date = new Date();
  date.setUTCDate(date.getUTCDate() - days);
  return date.toISOString().slice(0, 10);
};
const zeroTotals: AIUsageTotals = {
  operations: 0, cache_hits: 0, failures: 0, external_unmetered_operations: 0,
  model_gateway_excluded_operations: 0, model_requests: 0, reported_requests: 0,
  priced_reported_requests: 0, cache_write_reported_requests: 0, cache_write_priced_requests: 0,
  cache_write_unreported_requests: 0, invalid_usage_requests: 0, unreported_requests: 0,
  input_tokens: 0, cached_input_tokens: 0, cache_write_input_tokens: 0, output_tokens: 0,
  reasoning_tokens: 0, estimated_cost_nanos: "0",
};

function Stat({ label, value, note }: { label: string; value: string; note?: string }) {
  return <Panel sx={{ p: 2.25, minWidth: 0 }}>
    <Typography component="h3" variant="overline" color="text.secondary">{label}</Typography>
    <Typography component="p" variant="h4" sx={{ mt: .4, fontVariantNumeric: "tabular-nums", overflowWrap: "anywhere" }}>{value}</Typography>
    {note && <Typography variant="caption" color="text.secondary">{note}</Typography>}
  </Panel>;
}

function Bar({ value, max }: { value: number; max: number }) {
  return <Box sx={{ height: 7, borderRadius: 9, bgcolor: "action.hover", overflow: "hidden" }}>
    <Box sx={{ width: `${max ? Math.max(2, value / max * 100) : 0}%`, height: "100%", bgcolor: "primary.main", borderRadius: 9 }} />
  </Box>;
}

const coverageLabels: Record<string, string> = {
  fully_priced_provider_reported: "Fully priced",
  partial_token_usage: "Partial tokens",
  cache_write_unreported: "Missing cache-write usage",
  cache_write_pricing_missing: "Missing cache-write rate",
  external_unmetered: "External unmetered",
  model_gateway_excluded: "Model gateway excluded",
  pricing_added_after_operation: "Pricing added later",
  pricing_unavailable: "Pricing unavailable",
  legacy_coverage_unknown: "Legacy coverage unknown",
  aggregate_overflow: "Aggregate overflow blocked",
  no_provider_usage: "No provider usage",
  no_usage_records: "No usage",
};

function CoverageBadges({ coverage }: { coverage: AIUsageReport["coverage"] }) {
  const states = coverage.states ?? [];
  return <Stack direction="row" sx={{ gap: .6, flexWrap: "wrap", alignItems: "center" }}>
    <Chip size="small" label={coverage.status} color={coverage.status === "complete" ? "success" : coverage.status === "partial" ? "warning" : "default"} />
    {states.slice(0, 2).map((state) => <Chip key={state} size="small" variant="outlined" label={coverageLabels[state] ?? state.replaceAll("_", " ")} />)}
  </Stack>;
}

function normalizedDay(day: AIUsageDaily): AIUsageDaily {
  return {
    ...day,
    features: day.features ?? [],
    coverage: day.coverage ?? {
      status: "unavailable", states: ["legacy_coverage_unknown"], model_requests: day.totals.model_requests,
      reported_requests: day.totals.reported_requests, unreported_requests: day.totals.unreported_requests,
      external_unmetered_operations: day.totals.external_unmetered_operations,
    },
    has_usage: day.has_usage ?? day.totals.operations > 0,
    current_partial_utc: day.current_partial_utc ?? false,
    recorded_cost_status: day.recorded_cost_status ?? "unknown",
    current_rate_status: day.current_rate_status ?? "unavailable",
  };
}

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
  const width = 960; const height = 270;
  const plot = { left: 70, right: 18, top: 24, bottom: 42 };
  const plotWidth = width - plot.left - plot.right; const plotHeight = height - plot.top - plot.bottom;
  const recorded = days.map((day) => !mixedCurrency && (day.recorded_cost_status === "available" || day.recorded_cost_status === "partial") ? costNumber(day.totals.estimated_cost_nanos) : null);
  const current = days.map((day) => day.current_rate_status !== "unavailable" ? costNumber(day.current_rate_estimated_cost_nanos) : null);
  const rawMax = Math.max(0, ...recorded.map((value) => value ?? 0), ...current.map((value) => value ?? 0));
  const ticks = chartTickValues(rawMax); const axisMax = ticks.at(-1) ?? rawMax;
  const recordedPath = linePath(recorded, plotWidth, plotHeight, axisMax);
  const currentPath = linePath(current, plotWidth, plotHeight, axisMax);
  const availableIndexes = days.flatMap((_, index) => recorded[index] !== null || current[index] !== null ? [index] : []);
  const xForIndex = (index: number) => plot.left + (days.length === 1 ? plotWidth / 2 : index / (days.length - 1) * plotWidth);
  const yForValue = (value: number) => plot.top + plotHeight - value / axisMax * plotHeight;
  const chartCurrency = days.find((day) => day.current_rate_currency)?.current_rate_currency ?? days.find((day) => day.recorded_currency)?.recorded_currency;
  const selectedIndex = activeIndex !== null && availableIndexes.includes(activeIndex) ? activeIndex : null;
  const activeDay = selectedIndex === null ? null : days[selectedIndex];
  const activeX = selectedIndex === null ? null : xForIndex(selectedIndex);
  const tooltipTransform = selectedIndex !== null && selectedIndex <= Math.max(1, days.length * .2) ? "translateX(0)" : selectedIndex !== null && selectedIndex >= days.length * .8 ? "translateX(-100%)" : "translateX(-50%)";
  const selectPointerDay = (event: PointerEvent<SVGSVGElement>) => {
    const bounds = event.currentTarget.getBoundingClientRect();
    const svgX = (event.clientX - bounds.left) / bounds.width * width;
    const target = days.length <= 1 ? 0 : Math.round(Math.max(0, Math.min(plotWidth, svgX - plot.left)) / plotWidth * (days.length - 1));
    setActiveIndex(nearestChartDataIndex(target, availableIndexes));
  };
  const selectKeyboardDay = (event: KeyboardEvent<SVGSVGElement>) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const position = selectedIndex === null ? -1 : availableIndexes.indexOf(selectedIndex);
    if (event.key === "Home") setActiveIndex(availableIndexes[0] ?? null);
    else if (event.key === "End") setActiveIndex(availableIndexes.at(-1) ?? null);
    else if (event.key === "ArrowLeft") setActiveIndex(availableIndexes[Math.max(0, position < 0 ? availableIndexes.length - 1 : position - 1)] ?? null);
    else setActiveIndex(availableIndexes[Math.min(availableIndexes.length - 1, position < 0 ? 0 : position + 1)] ?? null);
  };
  if (rawMax === 0) return <Box sx={{ py: 5, textAlign: "center" }}><Typography color="text.secondary">No priced daily cost values are available in this range.</Typography></Box>;
  return <Box>
    <Box sx={{ overflowX: "auto", pb: .5 }}>
      <Box sx={{ position: "relative", minWidth: { xs: 720, md: 0 } }}>
        <Box
          component="svg"
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        tabIndex={0}
        aria-labelledby="daily-cost-chart-title daily-cost-chart-desc"
        onPointerMove={selectPointerDay}
        onPointerLeave={() => setActiveIndex(null)}
        onFocus={() => setActiveIndex((currentIndex) => currentIndex !== null && availableIndexes.includes(currentIndex) ? currentIndex : availableIndexes.at(-1) ?? null)}
        onBlur={() => setActiveIndex(null)}
        onKeyDown={selectKeyboardDay}
        sx={{ width: "100%", height: { xs: 230, md: 285 }, display: "block", borderRadius: 1, outline: "none", "&:focus-visible": { boxShadow: "0 0 0 2px var(--mui-palette-primary-main)" } }}
      >
        <title id="daily-cost-chart-title">Daily AI cost estimates</title>
        <desc id="daily-cost-chart-desc">Solid blue shows recorded estimates. Dashed amber shows current-rate estimates. Hover over the chart or focus it and use the left and right arrow keys to inspect dates. Exact daily values are listed in the table below.</desc>
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
          {recordedPath && <path d={recordedPath} fill="none" stroke="var(--mui-palette-primary-main)" strokeWidth="3" strokeLinecap="round" />}
          {currentPath && <path d={currentPath} fill="none" stroke="var(--mui-palette-warning-main)" strokeWidth="3" strokeDasharray="9 7" strokeLinecap="round" />}
          {recorded.map((value, index) => value === null ? null : <circle key={`recorded-${days[index].date}`} cx={xForIndex(index) - plot.left} cy={yForValue(value) - plot.top} r="3.5" fill="var(--mui-palette-primary-main)" stroke="var(--mui-palette-background-default)" strokeWidth="1.5" />)}
          {current.map((value, index) => value === null ? null : <rect key={`current-${days[index].date}`} x={xForIndex(index) - plot.left - 3.5} y={yForValue(value) - plot.top - 3.5} width="7" height="7" fill="var(--mui-palette-warning-main)" stroke="var(--mui-palette-background-default)" strokeWidth="1.5" />)}
        </g>
        {activeX !== null && <line x1={activeX} x2={activeX} y1={plot.top} y2={plot.top + plotHeight} stroke="var(--mui-palette-text-secondary)" strokeDasharray="3 5" opacity="0.7" />}
        {selectedIndex !== null && recorded[selectedIndex] !== null && <circle cx={activeX ?? 0} cy={yForValue(recorded[selectedIndex] ?? 0)} r="7" fill="var(--mui-palette-background-paper)" stroke="var(--mui-palette-primary-main)" strokeWidth="3" />}
        {selectedIndex !== null && current[selectedIndex] !== null && <rect x={(activeX ?? 0) - 6.5} y={yForValue(current[selectedIndex] ?? 0) - 6.5} width="13" height="13" fill="var(--mui-palette-background-paper)" stroke="var(--mui-palette-warning-main)" strokeWidth="3" />}
        </Box>
        {activeDay && activeX !== null && <Box role="status" aria-live="polite" sx={{ position: "absolute", top: 22, left: `${activeX / width * 100}%`, transform: tooltipTransform, minWidth: 210, maxWidth: 270, p: 1.25, borderRadius: 1.25, border: "1px solid", borderColor: "divider", bgcolor: "background.paper", boxShadow: 6, pointerEvents: "none", zIndex: 1 }}>
        <Typography variant="subtitle2" sx={{ fontFamily: "monospace" }}>{activeDay.date} UTC{activeDay.current_partial_utc ? " · Partial UTC day" : ""}</Typography>
        {recordedPath && <Typography variant="caption" component="div" sx={{ mt: .5 }}><Box component="span" sx={{ color: "primary.main" }}>●</Box> Recorded: {recordedCost(activeDay)}</Typography>}
        {currentPath && <Typography variant="caption" component="div"><Box component="span" sx={{ color: "warning.main" }}>■</Box> Current rate: {currentRateCost(activeDay)}</Typography>}
        <Typography variant="caption" component="div" color="text.secondary">Coverage: {activeDay.coverage.status}{activeDay.coverage.states?.length ? ` · ${activeDay.coverage.states.map((state) => coverageLabels[state] ?? state.replaceAll("_", " ")).join(", ")}` : ""}</Typography>
        </Box>}
      </Box>
    </Box>
    <Stack direction="row" sx={{ gap: 2, flexWrap: "wrap", alignItems: "center", mt: .5 }}>
      {recordedPath && <Typography variant="caption"><Box component="span" sx={{ display: "inline-block", width: 22, borderTop: "3px solid", borderColor: "primary.main", mr: .8, verticalAlign: "middle" }} />Recorded estimate (solid)</Typography>}
      {currentPath && <Typography variant="caption"><Box component="span" sx={{ display: "inline-block", width: 22, borderTop: "3px dashed", borderColor: "warning.main", mr: .8, verticalAlign: "middle" }} />Current-rate estimate (dashed)</Typography>}
      <Typography variant="caption" color="text.secondary">Hover to inspect. Keyboard: focus chart, then use ← and →.</Typography>
    </Stack>
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

function FeatureBreakdown({ day, currency }: { day: AIUsageDaily; currency?: string }) {
  if (day.features.length === 0) return <Typography color="text.secondary" variant="body2">No feature activity was recorded for this UTC day.</Typography>;
  return <Box sx={{ display: "grid", gap: 1 }}>
    {day.features.map((row) => <Box key={row.feature} sx={{ display: "grid", gridTemplateColumns: { xs: "1fr 1fr", md: "minmax(180px,1fr) repeat(4,minmax(90px,auto))" }, gap: 1, alignItems: "baseline", py: .75, borderBottom: "1px solid", borderColor: "divider" }}>
      <Typography variant="body2" sx={{ fontWeight: 700 }}>{featureLabels[row.feature]}</Typography>
      <Typography variant="caption">{formatExactTokens(row.totals.operations)} operations</Typography>
      <Typography variant="caption">{formatExactTokens(row.totals.model_requests)} requests</Typography>
      <Typography variant="caption">{formatExactTokens(totalTokens(row.totals))} tokens</Typography>
      <Typography variant="caption">{formatExactCost(row.totals.estimated_cost_nanos, currency)}</Typography>
    </Box>)}
  </Box>;
}

const numericCell = { fontVariantNumeric: "tabular-nums", whiteSpace: "nowrap" } as const;

function HistoricalTable({ days }: { days: AIUsageDaily[] }) {
  const [ascending, setAscending] = useState(false); const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const sorted = useMemo(() => [...days].sort((a, b) => ascending ? a.date.localeCompare(b.date) : b.date.localeCompare(a.date)), [ascending, days]);
  const toggle = (date: string) => setExpanded((current) => { const next = new Set(current); if (next.has(date)) next.delete(date); else next.add(date); return next; });
  return <>
    <TableContainer sx={{ display: { xs: "none", md: "block" }, overflowX: "auto" }}>
      <Table size="small" aria-label="Historical daily AI usage and cost" sx={{ minWidth: 1320, "& .MuiTableCell-root": { px: 1 } }}>
        <TableHead><TableRow>
          <TableCell padding="checkbox" />
          <TableCell sortDirection={ascending ? "asc" : "desc"}><TableSortLabel active direction={ascending ? "asc" : "desc"} onClick={() => setAscending((v) => !v)}>UTC date</TableSortLabel></TableCell>
          {['Operations','Requests','Cache hits','Uncached input','Cached read','Cache write','Output','Recorded estimate','Current-rate estimate','Coverage'].map((label) => <TableCell key={label} align={label === 'Coverage' ? 'left' : 'right'} sx={{ minWidth: label === 'Coverage' ? 200 : label.includes('estimate') ? 120 : undefined }}>{label}</TableCell>)}
        </TableRow></TableHead>
        <TableBody>{sorted.map((day) => {
          const open = expanded.has(day.date);
          return <Fragment key={day.date}>
            <TableRow hover sx={{ opacity: day.has_usage ? 1 : .66 }}>
              <TableCell padding="checkbox"><IconButton size="small" onClick={() => toggle(day.date)} aria-label={`${open ? "Collapse" : "Expand"} feature breakdown for ${day.date}`} aria-expanded={open}>{open ? <KeyboardArrowDown /> : <KeyboardArrowRight />}</IconButton></TableCell>
              <TableCell component="th" scope="row" sx={{ ...numericCell, minWidth: 185, fontFamily: "monospace", fontWeight: 700 }}>{day.date}{day.current_partial_utc && <Chip size="small" label="Partial UTC day" sx={{ ml: 1 }} />}</TableCell>
              <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.operations)}</TableCell>
              <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.model_requests)}</TableCell>
              <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.cache_hits)}</TableCell>
              <TableCell align="right" sx={numericCell}>{formatExactTokens(uncachedInputTokens(day.totals))}</TableCell>
              <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.cached_input_tokens)}</TableCell>
              <TableCell align="right" sx={numericCell}>{(day.totals.cache_write_reported_requests ?? 0) > 0 || (day.totals.cache_write_input_tokens ?? 0) > 0 ? formatExactTokens(day.totals.cache_write_input_tokens ?? 0) : "Not reported"}</TableCell>
              <TableCell align="right" sx={numericCell}>{formatExactTokens(day.totals.output_tokens)}</TableCell>
              <TableCell align="right" sx={numericCell}>{recordedCost(day)}</TableCell>
              <TableCell align="right" sx={numericCell}>{currentRateCost(day)}</TableCell>
              <TableCell sx={{ minWidth: 200, maxWidth: 240 }}><CoverageBadges coverage={day.coverage} /></TableCell>
            </TableRow>
            <TableRow><TableCell colSpan={12} sx={{ py: 0, bgcolor: "action.hover" }}><Collapse in={open} timeout="auto" unmountOnExit><Box sx={{ py: 2, px: 1 }}><FeatureBreakdown day={day} currency={day.recorded_currency} /></Box></Collapse></TableCell></TableRow>
          </Fragment>;
        })}</TableBody>
      </Table>
    </TableContainer>
    <Stack sx={{ display: { xs: "flex", md: "none" }, gap: 1.25 }}>
      {sorted.map((day) => { const open = expanded.has(day.date); return <Panel key={day.date} sx={{ p: 0, overflow: "hidden", opacity: day.has_usage ? 1 : .7 }}>
        <Button fullWidth onClick={() => toggle(day.date)} aria-expanded={open} sx={{ p: 1.5, justifyContent: "space-between", color: "text.primary", textAlign: "left" }} endIcon={open ? <KeyboardArrowDown /> : <KeyboardArrowRight />}>
          <Box><Typography sx={{ fontFamily: "monospace", fontWeight: 800 }}>{day.date}</Typography><Typography variant="caption" color="text.secondary">{day.has_usage ? `${formatExactTokens(day.totals.operations)} ${day.totals.operations === 1 ? "operation" : "operations"} · ${formatExactTokens(day.totals.model_requests)} ${day.totals.model_requests === 1 ? "request" : "requests"}` : "No usage recorded"}</Typography></Box>
          {day.current_partial_utc && <Chip size="small" label="Partial" />}
        </Button>
        <Box sx={{ px: 1.5, pb: 1.5, display: "grid", gridTemplateColumns: "1fr 1fr", gap: 1 }}>
          <Box><Typography variant="caption" color="text.secondary">Recorded estimate</Typography><Typography variant="body2" sx={numericCell}>{recordedCost(day)}</Typography></Box>
          <Box><Typography variant="caption" color="text.secondary">Current-rate estimate</Typography><Typography variant="body2" sx={numericCell}>{currentRateCost(day)}</Typography></Box>
          <Box><Typography variant="caption" color="text.secondary">Cache hits</Typography><Typography variant="body2" sx={numericCell}>{formatExactTokens(day.totals.cache_hits)}</Typography></Box>
          <Box><Typography variant="caption" color="text.secondary">Uncached input</Typography><Typography variant="body2" sx={numericCell}>{formatExactTokens(uncachedInputTokens(day.totals))}</Typography></Box>
          <Box><Typography variant="caption" color="text.secondary">Cached read</Typography><Typography variant="body2" sx={numericCell}>{formatExactTokens(day.totals.cached_input_tokens)}</Typography></Box>
          <Box><Typography variant="caption" color="text.secondary">Cache write</Typography><Typography variant="body2" sx={numericCell}>{(day.totals.cache_write_reported_requests ?? 0) > 0 || (day.totals.cache_write_input_tokens ?? 0) > 0 ? formatExactTokens(day.totals.cache_write_input_tokens ?? 0) : "Not reported"}</Typography></Box>
          <Box><Typography variant="caption" color="text.secondary">Output</Typography><Typography variant="body2" sx={numericCell}>{formatExactTokens(day.totals.output_tokens)}</Typography></Box>
          <Box sx={{ gridColumn: "1 / -1" }}><CoverageBadges coverage={day.coverage} /></Box>
        </Box>
        <Collapse in={open} timeout="auto" unmountOnExit><Box sx={{ p: 1.5, pt: 0 }}><FeatureBreakdown day={day} currency={day.recorded_currency} /></Box></Collapse>
      </Panel>; })}
    </Stack>
  </>;
}

export function AIUsagePage() {
  const { features } = useCapabilities(); const auth = useAuth();
  const [start, setStart] = useState(daysAgo(29)); const [end, setEnd] = useState(today());
  const [feature, setFeature] = useState<AIUsageFeature | "">("");
  const [loaded, setLoaded] = useState<{ key: string | null; data: AIUsageReport | null; error: string }>({ key: null, data: null, error: "" });
  const query = usageQuery(start, end, feature || undefined);
  useEffect(() => {
    if (!features.ai_usage || auth.status !== "authenticated") return;
    const controller = new AbortController();
    fetch(`${API_BASE}api/ai-usage?${query}`, { credentials: "same-origin", signal: controller.signal })
      .then(async (response) => { if (response.status === 404) return null; if (!response.ok) throw new Error(`HTTP ${response.status}`); return response.json() as Promise<AIUsageReport>; })
      .then((data) => setLoaded({ key: query, data, error: "" }))
      .catch((error) => { if (!controller.signal.aborted) setLoaded({ key: query, data: null, error: error instanceof Error ? error.message : "Unable to load usage" }); });
    return () => controller.abort();
  }, [auth.status, features.ai_usage, query]);
  const data = loaded.key === query ? loaded.data : null; const error = loaded.key === query ? loaded.error : "";
  const loading = auth.status === "authenticated" && loaded.key !== query; const totals = data?.totals ?? zeroTotals;
  const pricedReportedRequests = totals.priced_reported_requests ?? data?.coverage.priced_reported_requests;
  const days = useMemo(() => (data?.daily ?? []).map(normalizedDay), [data]);
  if (!features.ai_usage) return <Alert severity="info">Private AI usage reporting is not available in this deployment.</Alert>;
  if (auth.status === "loading") return <Box sx={{ display: "grid", placeItems: "center", py: 10 }}><CircularProgress size={28} /></Box>;
  if (auth.status === "anonymous") return <Panel sx={{ maxWidth: 620, mx: "auto", p: 4, textAlign: "center" }}><Paid sx={{ fontSize: 38, color: "primary.main" }} /><Typography variant="h5">Operator sign-in required</Typography><Typography color="text.secondary" sx={{ my: 2 }}>Token usage and cost estimates are private operational data.</Typography><Button variant="contained" onClick={auth.signIn}>Sign in to view usage</Button></Panel>;
  return <Stack spacing={2.5}>
    <Stack direction={{ xs: "column", md: "row" }} sx={{ justifyContent: "space-between", gap: 2 }}><Box><Stack direction="row" sx={{ gap: 1, alignItems: "center" }}><Paid color="primary" /><Typography component="h1" variant="h4">AI Usage</Typography></Stack><Typography color="text.secondary">Provider-reported tokens, recorded estimates, and current-rate historical repricing.</Typography></Box><Button component="a" href={`${API_BASE}api/ai-usage/download?${query}`} startIcon={<Download />} variant="outlined">Download JSON</Button></Stack>
    <Panel sx={{ p: 2, display: "grid", gridTemplateColumns: { xs: "1fr", sm: "repeat(3,1fr)" }, gap: 1.5 }}><TextField type="date" label="Start" value={start} onChange={(event) => setStart(event.target.value)} slotProps={{ inputLabel: { shrink: true } }} /><TextField type="date" label="End" value={end} onChange={(event) => setEnd(event.target.value)} slotProps={{ inputLabel: { shrink: true } }} /><TextField select label="Feature" value={feature} onChange={(event) => setFeature(event.target.value as AIUsageFeature | "")}><MenuItem value="">All features</MenuItem>{Object.entries(featureLabels).map(([key, value]) => <MenuItem key={key} value={key}>{value}</MenuItem>)}</TextField></Panel>
    {error && <Alert severity="error">{error}</Alert>}
    {loading ? <Box sx={{ display: "grid", placeItems: "center", py: 8 }}><CircularProgress size={26} /></Box> : !data && !error ? <Alert severity="info">No AI usage has been recorded yet.</Alert> : data && <>
      <Stack direction="row" sx={{ gap: 1, flexWrap: "wrap" }}><CoverageBadges coverage={data.coverage} />{data.selected_model && <Chip label={`Model: ${data.selected_model}`} variant="outlined" />}{data.pricing_rule && <Chip label={`Pricing: ${data.pricing_rule}`} variant="outlined" />}{data.mixed_pricing && <Chip label="Mixed recorded pricing" variant="outlined" />}{data.mixed_currency && <Chip label="Mixed recorded currencies" color="warning" variant="outlined" />}</Stack>
      <Alert severity={data.coverage.status === "complete" ? "success" : "info"}><Typography variant="body2"><strong>{data.coverage.status}</strong> token coverage means {data.coverage.status === "complete" ? "every model request reported token usage and all reported cache-write tokens were priced" : data.coverage.status === "partial" ? "some model requests, cache writes, or external runtimes are outside complete billing coverage" : "no model request supplied complete usable accounting"}. Token totals include only provider-reported usage. Estimated cost covers priced records only. Recorded estimates use the price stored with each operation. Current-rate estimates apply the rates configured now to historical token totals. {data.pricing_coverage !== "complete" && <> <Link href="https://github.com/willie-yao/prow-ai-dashboard/blob/main/docs/project-configuration.md" target="_blank" rel="noopener">ai.usage.pricing</Link> {data.pricing_configured ? "is configured now" : "is not configured"}.</>}</Typography></Alert>
      <Typography component="h2" variant="h6">Usage totals</Typography>
      <Box sx={{ display: "grid", gridTemplateColumns: { xs: "1fr", sm: "repeat(2,1fr)", xl: "repeat(4,1fr)" }, gap: 1.5 }}>
        <Stat label="Estimated cost for priced records" value={data.mixed_currency ? "Mixed currencies" : data.recorded_cost_status === "unavailable" ? "Not priced" : formatCost(totals.estimated_cost_nanos, data.currency)} note={data.mixed_currency ? "Recorded totals cannot be combined across currencies" : pricedRequestCoverageNote(pricedReportedRequests, totals.reported_requests, data.pricing_coverage)} />
        <Stat label="Current-rate estimate" value={data.current_rate_status === "unavailable" ? "Unavailable" : formatCost(data.current_rate_estimated_cost_nanos ?? "0", data.current_rate_currency)} note={data.current_rate_status === "partial" ? "Known token totals only; coverage is partial" : "Historical tokens repriced with current rates"} />
        <Stat label="Provider-reported tokens" value={formatTokens(totalTokens(totals))} note={`${formatCoverage(totals.reported_requests, totals.model_requests)} model requests reported usage`} />
        <Stat label="Model requests" value={formatTokens(totals.model_requests)} note={`${totals.cache_hits.toLocaleString()} cache hits · ${totals.unreported_requests.toLocaleString()} without usage`} />
      </Box>
      <Panel sx={{ p: { xs: 1.5, md: 2.5 } }}><Stack direction={{ xs: "column", md: "row" }} sx={{ justifyContent: "space-between", gap: 1, mb: 2 }}><Box><Typography component="h2" variant="h6">Historical daily cost</Typography><Typography variant="body2" color="text.secondary">UTC day boundaries. The current UTC day is partial until 23:59:59 UTC.</Typography></Box><Typography variant="caption" color="text.secondary">Chart: chronological · Table: newest first</Typography></Stack><DailyCostChart days={days} mixedCurrency={Boolean(data.mixed_currency)} /></Panel>
      <Panel sx={{ p: { xs: 1, md: 2 }, minWidth: 0 }}><Typography component="h2" variant="h6" sx={{ px: { xs: .5, md: 0 }, mb: 1.5 }}>Daily ledger</Typography><HistoricalTable days={days} /></Panel>
      <Panel sx={{ p: 2.5 }}><Typography component="h2" variant="h6" sx={{ mb: 2 }}>Selected-range feature mix</Typography><Stack spacing={1.6}>{data.features.map((row) => <Box key={row.feature}><Stack direction="row" sx={{ justifyContent: "space-between" }}><Typography variant="body2" sx={{ fontWeight: 700 }}>{featureLabels[row.feature]}</Typography><Typography variant="caption">{formatTokens(totalTokens(row.totals))}</Typography></Stack><Bar value={totalTokens(row.totals)} max={Math.max(...data.features.map((item) => totalTokens(item.totals)), 1)} /></Box>)}{data.features.length === 0 && <Typography color="text.secondary">No feature activity in this range.</Typography>}</Stack></Panel>
    </>}
  </Stack>;
}
