import Accordion from "@mui/material/Accordion";
import AccordionDetails from "@mui/material/AccordionDetails";
import AccordionSummary from "@mui/material/AccordionSummary";
import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { ExpandMore } from "@mui/icons-material";
import type {
  PatternCausalGroup,
  PatternRemediationInvestigationSummary,
} from "../types/dashboard";
import { patternRemediationPresentation } from "../lib/patternRemediation";
import { overviewTypography } from "../theme/overview";

export function PatternRemediation({
  groups,
  investigations,
}: {
  groups: PatternCausalGroup[];
  investigations?: PatternRemediationInvestigationSummary[];
}) {
  const recurringGroups = groups.filter((group) => group.builds.length >= 2);
  const investigationsByHash = new Map(
    investigations?.map((investigation) => [investigation.causal_group_hash, investigation]),
  );

  return (
    <Box aria-live="polite">
      <Typography
        component="h3"
        color="text.secondary"
        sx={{ ...overviewTypography.subsectionHeading, fontSize: "14px", lineHeight: "20px" }}
      >
        Remediation
      </Typography>
      {recurringGroups.length === 0 ? (
        <Typography sx={{ mt: 0.75 }}>
          No recurring causal group was identified for source investigation.
        </Typography>
      ) : (
        <Stack spacing={1.5} sx={{ mt: 0.75 }}>
          {recurringGroups.map((group, index) => {
            const presentation = patternRemediationPresentation(
              group.content_hash ? investigationsByHash.get(group.content_hash) : undefined,
            );
            return (
              <Box key={group.id ?? group.content_hash ?? `${group.builds.join("-")}-${index}`}>
                <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 0.5 }}>
                  {recurringGroups.length > 1 && (
                    <Typography sx={{ ...overviewTypography.data, fontWeight: 700 }}>
                      Cause {index + 1}
                    </Typography>
                  )}
                  <Chip
                    label={presentation.label}
                    size="small"
                    color={presentation.state === "actionable" ? "success" : "default"}
                    variant="outlined"
                  />
                </Stack>
                <Typography sx={{ mt: 0.75 }}>{presentation.message}</Typography>
                {presentation.detail && (
                  <Accordion
                    disableGutters
                    elevation={0}
                    sx={{
                      mt: 1,
                      border: "1px solid",
                      borderColor: "divider",
                      borderRadius: "4px",
                      "&::before": { display: "none" },
                    }}
                  >
                    <AccordionSummary expandIcon={<ExpandMore aria-hidden />}>
                      <Typography sx={overviewTypography.data}>Investigation details</Typography>
                    </AccordionSummary>
                    <AccordionDetails sx={{ pt: 0 }}>
                      <Typography color="text.secondary" sx={{ overflowWrap: "anywhere" }}>
                        {presentation.detail}
                      </Typography>
                    </AccordionDetails>
                  </Accordion>
                )}
              </Box>
            );
          })}
        </Stack>
      )}
    </Box>
  );
}
