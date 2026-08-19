import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import type { AnalysisCauseLocation } from "../types/dashboard";
import { overviewTypography } from "../theme/overview";

// UpstreamCauseNotice states that a diagnosed cause lives in a dependency and
// names the repository the change belongs in. Aster verifies source only at the
// immutable revision a build pins for the project's own repository, so the
// reported paths are labelled as the unverified hints they are.
//
// The wording stays accurate wherever this renders: Aster never opens a pull
// request in a dependency, but a project-side mitigation can still be
// investigated when a failure has verified project source.
export function UpstreamCauseNotice({ location }: { location: AnalysisCauseLocation }) {
  return (
    <Box sx={{ mt: 1 }}>
      <Typography color="text.secondary" sx={overviewTypography.description}>
        The cause is in{" "}
        <Link href={`https://github.com/${location.repository}`} target="_blank" rel="noopener noreferrer">
          {location.repository}
        </Link>
        , a dependency of this project, so the change belongs in that repository. Aster does not open
        pull requests in a dependency, so anything it proposes here is a project-side mitigation
        rather than the upstream change itself.
      </Typography>
      {location.files && location.files.length > 0 && (
        <Typography color="text.secondary" sx={{ mt: 0.5, ...overviewTypography.description }}>
          Reported location, unverified because Aster reads source only at the revision this project
          pinned:{" "}
          {location.files.map((file, index) => (
            <Box component="span" key={file}>
              {index > 0 && ", "}
              <Box component="code" sx={{ fontFamily: "monospace" }}>
                {file}
              </Box>
            </Box>
          ))}
        </Typography>
      )}
    </Box>
  );
}
