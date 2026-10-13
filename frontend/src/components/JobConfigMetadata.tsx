import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { OpenInNew } from "@mui/icons-material";
import type { JobDetail } from "../types/dashboard";
import type { Discovery } from "../types/manifest";
import { overviewTypography } from "../theme/overview";

export interface JobConfigMetadataProps {
  job: Pick<JobDetail, "config_file" | "config_revision">;
  discovery?: Discovery;
  branch?: string;
}

export function JobConfigMetadata({
  job,
  discovery,
  branch,
}: JobConfigMetadataProps) {
  const file = job.config_file?.trim();
  if (!discovery || (discovery.source && discovery.source !== "testgrid") || !file) return null;

  const segments = file.split("/");
  if (
    !file.startsWith("config/jobs/") ||
    segments.some((segment) => !segment || segment === "." || segment === "..") ||
    file.includes("\\")
  ) return null;

  // TestGrid discovery reads only kubernetes/test-infra.
  const revision = job.config_revision || "master";
  const href = `https://github.com/kubernetes/test-infra/blob/${encodeURIComponent(revision)}/${segments.map(encodeURIComponent).join("/")}`;
  const testedBranch = branch?.trim();

  return (
    <Typography
      component="p"
      color="textSecondary"
      sx={{ m: 0, mt: 0.75, ...overviewTypography.secondaryBody, overflowWrap: "anywhere" }}
    >
      Defined in{" "}
      <Link
        href={href}
        target="_blank"
        rel="noopener noreferrer"
        aria-label={`Job definition in kubernetes/test-infra: ${file} (opens in a new tab)`}
        sx={{ color: "primary.main" }}
      >
        <Box component="code" sx={overviewTypography.data}>{file}</Box>
        <OpenInNew sx={{ ml: 0.5, fontSize: 14, verticalAlign: "middle" }} />
      </Link>
      {testedBranch && (
        <>
          {", tests branch "}
          <Box component="code" sx={overviewTypography.data}>{testedBranch}</Box>
        </>
      )}
    </Typography>
  );
}
