import type {
  BuildFailureSummary,
  FlakinessReport,
  PatternAnalysis,
  PatternRefreshReport,
  TestFlakiness,
} from "../types/dashboard";

export interface FlakinessReportWire {
  generated_at: string;
  most_flaky?: TestFlakiness[] | null;
  persistent_failures?: TestFlakiness[] | null;
  recently_broken?: TestFlakiness[] | null;
  build_failures?: BuildFailureSummary[] | null;
  recurring_patterns?: PatternAnalysis[] | null;
  pattern_refresh?: PatternRefreshReport;
}

export function normalizeFlakinessReport(report: FlakinessReportWire): FlakinessReport {
  return {
    generated_at: report.generated_at,
    most_flaky: report.most_flaky ?? [],
    persistent_failures: report.persistent_failures ?? [],
    recently_broken: report.recently_broken ?? [],
    build_failures: report.build_failures ?? [],
    recurring_patterns: report.recurring_patterns ?? [],
    pattern_refresh: report.pattern_refresh,
  };
}
