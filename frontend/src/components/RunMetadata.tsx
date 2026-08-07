import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import type { ReactNode } from "react";
import { OpenInNew } from "@mui/icons-material";
import { DetailSectionBand } from "./DetailSectionBand";
import { overviewTypography } from "../theme/overview";

export interface RunMetadataItem {
  label: string;
  value: ReactNode;
}

export interface RunMetadataLink {
  label: string;
  href: string;
}

export function RunMetadata({
  items,
  links,
  status,
  statusColor = "text.secondary",
}: {
  items: RunMetadataItem[];
  links: RunMetadataLink[];
  status: string;
  statusColor?: "warning.main" | "success.main" | "error.main" | "text.secondary";
}) {
  return (
    <Box component="section" sx={{ minWidth: 0, bgcolor: "surface.container" }}>
      <DetailSectionBand
        title="Run metadata"
        metadata={<Box component="span" sx={{ color: statusColor }}>{status}</Box>}
      />
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: { xs: "minmax(0, 1fr)", sm: "repeat(2, minmax(0, 1fr))" },
        }}
      >
        {items.map((item, index) => (
          <Box
            key={item.label}
            sx={{
              minWidth: 0,
              minHeight: 60,
              px: 1.5,
              py: 1,
              borderTop: "1px solid",
              borderLeft: { xs: "none", sm: index % 2 === 1 ? "1px solid" : "none" },
              borderColor: "divider",
            }}
          >
            <Typography component="div" color="text.secondary" sx={overviewTypography.tableHeading}>
              {item.label}
            </Typography>
            <Typography
              component="div"
              color="text.primary"
              sx={{ mt: 0.25, ...overviewTypography.data, overflowWrap: "anywhere" }}
            >
              {item.value}
            </Typography>
          </Box>
        ))}
      </Box>
      {links.length > 0 && (
        <Box
          sx={{
            minHeight: 44,
            display: "flex",
            alignItems: "center",
            gap: 2,
            flexWrap: "wrap",
            px: 1.5,
            py: 0.5,
            borderTop: "1px solid",
            borderBottom: "1px solid",
            borderColor: "divider",
          }}
        >
          {links.map((link) => (
            <Link
              key={link.label}
              href={link.href}
              target="_blank"
              rel="noopener noreferrer"
              sx={{
                minHeight: 36,
                display: "inline-flex",
                alignItems: "center",
                gap: 0.5,
                fontSize: "13px",
                fontWeight: 650,
              }}
            >
              {link.label} <OpenInNew sx={{ fontSize: 14 }} />
            </Link>
          ))}
        </Box>
      )}
    </Box>
  );
}
