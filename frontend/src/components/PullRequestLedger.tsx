import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import type { PullRequestSummary } from "../types/pullRequests";
import { shortSHA } from "../lib/pullRequests";
import { pullRequestPath } from "../lib/routes";
import { timeAgo } from "../lib/utils";
import { StatusChip } from "./StatusChip";
import { overviewLayout, overviewTypography } from "../theme/overview";

const wideBreakpoint = "@media (min-width: 1200px)";
const compactColumns = "72px minmax(220px, 2fr) 104px 96px 92px 96px 82px";
const wideColumns = "84px minmax(300px, 2.4fr) 128px 108px 104px 112px 88px";
const headers = ["Pull", "Title", "Base", "Checks", "Failing", "Updated", "State"];

// UNKNOWN has no dashboard status equivalent, so it renders as a neutral chip.
function stateChip(pull: PullRequestSummary) {
  if (pull.ci_state === "UNKNOWN") {
    return <StatusChip status="UNKNOWN" label="No runs" sx={{ height: 26, fontSize: "13px" }} />;
  }
  const label = pull.ci_state === "PENDING" ? "Pending" : undefined;
  const status = pull.ci_state === "PENDING" ? "RUNNING" : pull.ci_state;
  return <StatusChip status={status} label={label} sx={{ height: 26, fontSize: "13px" }} />;
}

function checksValue(pull: PullRequestSummary): string {
  return pull.checks_observed === 0 ? "None" : String(pull.checks_observed);
}

function failingValue(pull: PullRequestSummary): string {
  if (pull.checks_failing === 0) return "0";
  const jobs = `${pull.checks_failing} ${pull.checks_failing === 1 ? "job" : "jobs"}`;
  if (pull.failing_tests === 0) return jobs;
  return `${jobs} · ${pull.failing_tests} ${pull.failing_tests === 1 ? "test" : "tests"}`;
}

// PullLink stretches over its row so the whole row is clickable. The visible
// text stays the pull number, and the accessible name carries the title so the
// row target is not announced as a bare number.
function PullLink({ pull, compact }: { pull: PullRequestSummary; compact: boolean }) {
  return (
    <Link
      component={RouterLink}
      to={pullRequestPath(pull.number)}
      underline="none"
      aria-label={`Pull request ${pull.number}: ${pull.title || "Untitled"}`}
      sx={{
        position: "static",
        minHeight: compact ? 44 : 0,
        display: compact ? "inline-flex" : "block",
        alignItems: compact ? "center" : undefined,
        color: "text.primary",
        ...overviewTypography.jobIdentifier,
        "&:hover": { color: "primary.main", textDecoration: "underline" },
        // Cover the row so any point in it activates this link.
        "&::after": {
          content: '""',
          position: "absolute",
          inset: 0,
          zIndex: 1,
        },
        "&:focus-visible::after": {
          outline: "2px solid",
          outlineColor: "primary.main",
          outlineOffset: -2,
        },
      }}
    >
      #{pull.number}
    </Link>
  );
}

function TitleCell({ pull }: { pull: PullRequestSummary }) {
  return (
    <Box sx={{ minWidth: 0 }}>
      <Typography
        title={pull.title}
        sx={{
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
          color: "text.primary",
          ...overviewTypography.secondaryBody,
          fontWeight: 600,
        }}
      >
        {pull.title || "Untitled"}
      </Typography>
      <Typography
        variant="caption"
        color="text.secondary"
        sx={{
          display: "block",
          mt: 0.25,
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
          ...overviewTypography.description,
        }}
      >
        {pull.author ? `${pull.author} · ` : ""}
        {shortSHA(pull.head_sha) || "no head"}
      </Typography>
    </Box>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <Box sx={{ display: "inline-flex", alignItems: "baseline", gap: 0.5 }}>
      <Typography variant="caption" component="span" color="text.secondary" sx={overviewTypography.description}>
        {label}
      </Typography>
      <Typography variant="data" component="span" color="text.primary" sx={overviewTypography.data}>
        {value}
      </Typography>
    </Box>
  );
}

function DesktopRow({ pull }: { pull: PullRequestSummary }) {
  return (
    <Box
      role="row"
      sx={{
        position: "relative",
        minHeight: overviewLayout.ledgerRowMinHeight,
        display: "grid",
        gridTemplateColumns: compactColumns,
        gridTemplateAreas: '"pull title base checks failing updated state"',
        alignItems: "center",
        columnGap: 1,
        px: 1.5,
        py: 1,
        borderBottom: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.container",
        cursor: "pointer",
        transition: "background-color 140ms ease",
        "&:hover": { bgcolor: "surface.containerHigh" },
        "&:focus-within": { boxShadow: "inset 2px 0 0 var(--mui-palette-primary-main)" },
        [wideBreakpoint]: { gridTemplateColumns: wideColumns, columnGap: 1.5, px: 2 },
      }}
    >
      <Box role="cell" sx={{ gridArea: "pull", minWidth: 0 }}>
        <PullLink pull={pull} compact={false} />
      </Box>
      <Box role="cell" sx={{ gridArea: "title", minWidth: 0 }}>
        <TitleCell pull={pull} />
      </Box>
      <Typography
        role="cell"
        variant="data"
        color="text.secondary"
        title={pull.base_ref}
        sx={{ gridArea: "base", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", ...overviewTypography.data }}
      >
        {pull.base_ref || "Not set"}
      </Typography>
      <Typography role="cell" variant="data" sx={{ gridArea: "checks", ...overviewTypography.data }}>
        {checksValue(pull)}
      </Typography>
      <Typography
        role="cell"
        variant="data"
        color={pull.checks_failing > 0 ? "error.main" : "text.secondary"}
        sx={{ gridArea: "failing", ...overviewTypography.data }}
      >
        {failingValue(pull)}
      </Typography>
      <Typography role="cell" variant="data" color="text.secondary" sx={{ gridArea: "updated", ...overviewTypography.data }}>
        {timeAgo(pull.updated_at)}
      </Typography>
      <Box role="cell" sx={{ gridArea: "state", justifySelf: "end" }}>
        {stateChip(pull)}
      </Box>
    </Box>
  );
}

function MobileRow({ pull }: { pull: PullRequestSummary }) {
  return (
    <Box
      role="listitem"
      sx={{
        position: "relative",
        display: "grid",
        gridTemplateColumns: "minmax(0, 1fr) auto",
        gridTemplateAreas: '"pull state" "title title" "meta meta"',
        alignItems: "center",
        columnGap: 1,
        rowGap: 0.75,
        px: 1.5,
        py: 1,
        borderBottom: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.container",
        cursor: "pointer",
        transition: "background-color 140ms ease",
        "&:hover": { bgcolor: "surface.containerHigh" },
        "&:focus-within": { boxShadow: "inset 2px 0 0 var(--mui-palette-primary-main)" },
      }}
    >
      <Box sx={{ gridArea: "pull", minWidth: 0 }}>
        <PullLink pull={pull} compact />
      </Box>
      <Box sx={{ gridArea: "state", justifySelf: "end" }}>{stateChip(pull)}</Box>
      <Box sx={{ gridArea: "title", minWidth: 0 }}>
        <TitleCell pull={pull} />
      </Box>
      <Box sx={{ gridArea: "meta", display: "flex", minWidth: 0, alignItems: "center", gap: 1.5, flexWrap: "wrap" }}>
        <Metric label="Base" value={pull.base_ref || "Not set"} />
        <Metric label="Checks" value={checksValue(pull)} />
        <Metric label="Failing" value={failingValue(pull)} />
        <Metric label="Updated" value={timeAgo(pull.updated_at)} />
      </Box>
    </Box>
  );
}

export function PullRequestLedger({ pulls }: { pulls: PullRequestSummary[] }) {
  return (
    <>
      <Box role="table" aria-label="Open pull requests" sx={{ display: { xs: "none", lg: "block" } }}>
        <Box
          role="row"
          sx={{
            display: "grid",
            gridTemplateColumns: compactColumns,
            columnGap: 1,
            px: 1.5,
            py: 1,
            borderBottom: "1px solid",
            borderColor: "divider",
            bgcolor: "surface.containerHigh",
            [wideBreakpoint]: { gridTemplateColumns: wideColumns, columnGap: 1.5, px: 2 },
          }}
        >
          {headers.map((header, index) => (
            <Typography
              key={header}
              role="columnheader"
              color="text.secondary"
              sx={{
                ...overviewTypography.tableHeading,
                justifySelf: index === headers.length - 1 ? "end" : "start",
              }}
            >
              {header}
            </Typography>
          ))}
        </Box>
        {pulls.map((pull) => (
          <DesktopRow key={pull.number} pull={pull} />
        ))}
      </Box>

      <Box role="list" aria-label="Open pull requests" sx={{ display: { xs: "block", lg: "none" } }}>
        {pulls.map((pull) => (
          <MobileRow key={pull.number} pull={pull} />
        ))}
      </Box>
    </>
  );
}
