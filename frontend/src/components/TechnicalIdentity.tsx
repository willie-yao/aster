import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import Typography from "@mui/material/Typography";
import { ChevronRight } from "@mui/icons-material";
import { useId, useState } from "react";
import { DetailSectionBand } from "./DetailSectionBand";
import { overviewTypography } from "../theme/overview";

export interface TechnicalIdentityItem {
  label: string;
  value: string;
  copyLabel?: string;
}

function CopyAction({ item }: { item: TechnicalIdentityItem }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    if (!navigator.clipboard) return;
    try {
      await navigator.clipboard.writeText(item.value);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      setCopied(false);
    }
  }

  if (!item.copyLabel) return null;

  return (
    <Button
      type="button"
      variant="text"
      size="small"
      onClick={() => void copy()}
      aria-label={item.copyLabel}
      sx={{
        minWidth: { xs: 44, sm: 36 },
        minHeight: { xs: 44, sm: 32 },
        px: 1,
        borderRadius: "4px",
        textTransform: "none",
        fontSize: "12px",
        fontWeight: 650,
      }}
    >
      {copied ? "Copied" : "Copy"}
    </Button>
  );
}

function IdentityRows({ items }: { items: TechnicalIdentityItem[] }) {
  return (
    <Box>
      {items.map((item) => (
        <Box
          key={`${item.label}\u0000${item.value}`}
          sx={{
            minWidth: 0,
            display: "grid",
            gridTemplateColumns: { xs: "minmax(0, 1fr) auto", sm: "160px minmax(0, 1fr) auto" },
            alignItems: "center",
            columnGap: 1,
            px: 1.5,
            py: 1,
            borderTop: "1px solid",
            borderColor: "divider",
          }}
        >
          <Typography
            component="span"
            color="text.secondary"
            sx={{ ...overviewTypography.tableHeading, gridColumn: { xs: "1 / -1", sm: "auto" } }}
          >
            {item.label}
          </Typography>
          <Typography
            component="code"
            color="text.primary"
            sx={{ ...overviewTypography.data, overflowWrap: "anywhere" }}
          >
            {item.value}
          </Typography>
          <CopyAction item={item} />
        </Box>
      ))}
    </Box>
  );
}

export function TechnicalIdentity({
  items,
  summary,
  desktopInline = false,
}: {
  items: TechnicalIdentityItem[];
  summary?: string;
  desktopInline?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const generatedID = useId();
  const contentID = `technical-identity-${generatedID.replaceAll(":", "")}`;

  return (
    <>
      {desktopInline && (
        <Box
          sx={{
            display: { xs: "none", md: "flex" },
            minWidth: 0,
            alignItems: "center",
            gap: 1,
            flexWrap: "wrap",
            color: "text.secondary",
          }}
        >
          {items.map((item) => (
            <Box
              key={`${item.label}\u0000${item.value}`}
              sx={{ display: "flex", minWidth: 0, alignItems: "center", gap: 1, flexWrap: "wrap" }}
            >
              <Typography component="span" sx={overviewTypography.description}>
                {item.label}
              </Typography>
              <Typography
                component="code"
                sx={{ ...overviewTypography.data, color: "text.secondary", overflowWrap: "anywhere" }}
              >
                {item.value}
              </Typography>
              <CopyAction item={item} />
            </Box>
          ))}
        </Box>
      )}

      <Box
        component="section"
        sx={{
          display: desktopInline ? { xs: "block", md: "none" } : "block",
          bgcolor: "surface.container",
          borderBottom: "1px solid",
          borderColor: "divider",
        }}
      >
        <DetailSectionBand title="Technical identity" metadata={summary} />
        <ButtonBase
          type="button"
          onClick={() => setOpen((value) => !value)}
          aria-expanded={open}
          aria-controls={contentID}
          sx={{
            width: "100%",
            minHeight: 44,
            px: 1.5,
            py: 0.75,
            justifyContent: "flex-start",
            gap: 1,
            color: "text.primary",
            textAlign: "left",
            "&:hover": { bgcolor: "surface.containerHigh" },
            "&.Mui-focusVisible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: -2,
            },
          }}
        >
          <Typography component="span" sx={{ ...overviewTypography.secondaryBody, fontWeight: 650 }}>
            {open ? "Hide technical identifiers" : "Show technical identifiers"}
          </Typography>
          <ChevronRight
            sx={{
              ml: "auto",
              fontSize: 20,
              color: "text.secondary",
              transform: open ? "rotate(90deg)" : "rotate(0deg)",
              transition: (theme) =>
                theme.transitions.create("transform", { duration: theme.transitions.duration.shortest }),
              "@media (prefers-reduced-motion: reduce)": { transition: "none" },
            }}
          />
        </ButtonBase>
        <Collapse in={open} timeout="auto">
          <Box id={contentID}>
            <IdentityRows items={items} />
          </Box>
        </Collapse>
      </Box>
    </>
  );
}
