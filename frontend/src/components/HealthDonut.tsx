import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";

interface HealthDonutProps {
  passing: number;
  flaky: number;
  failing: number;
  /** Diameter in px. */
  size?: number;
}

// Proportional pass/flaky/fail ring. Pure SVG (no chart dependency); segment
// lengths are percentages via pathLength=100 and colored from MUI CSS palette
// variables. The center shows total jobs and the surrounding legend supplies
// the text labels, so color is not the only cue.
export function HealthDonut({ passing, flaky, failing, size = 128 }: HealthDonutProps) {
  const total = passing + flaky + failing;
  const segments: { value: number; stroke: string }[] = [
    { value: passing, stroke: "var(--mui-palette-success-main)" },
    { value: flaky, stroke: "var(--mui-palette-warning-main)" },
    { value: failing, stroke: "var(--mui-palette-error-main)" },
  ];

  const strokeWidth = 3.4;
  // A small gap between segments avoids the butt-cap spike where two arcs meet
  // (most visible when one category is 0, leaving exactly two segments).
  const active = segments.filter((seg) => seg.value > 0);
  const gap = active.length > 1 ? 1.5 : 0;
  let offset = 0;

  return (
    <Box sx={{ position: "relative", width: size, height: size, flexShrink: 0 }}>
      <Box
        component="svg"
        viewBox="0 0 36 36"
        role="img"
        aria-label={`${passing} passing, ${flaky} flaky, ${failing} failing of ${total} jobs`}
        sx={{ width: "100%", height: "100%", transform: "rotate(-90deg)" }}
      >
        <circle
          cx={18}
          cy={18}
          r={15.9155}
          fill="none"
          strokeWidth={strokeWidth}
          pathLength={100}
          stroke="var(--mui-palette-divider)"
        />
        {total > 0 &&
          active.map((seg) => {
            const pct = (seg.value / total) * 100;
            const dash = Math.max(pct - gap, 0.5);
            const circle = (
              <circle
                key={seg.stroke}
                cx={18}
                cy={18}
                r={15.9155}
                fill="none"
                strokeWidth={strokeWidth}
                strokeLinecap="butt"
                pathLength={100}
                strokeDasharray={`${dash} ${100 - dash}`}
                strokeDashoffset={-offset}
                stroke={seg.stroke}
              />
            );
            offset += pct;
            return circle;
          })}
      </Box>
      <Box
        sx={{
          position: "absolute",
          inset: 0,
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
          justifyContent: "center",
          gap: 0.25,
        }}
      >
        <Typography variant="stat" component="span" sx={{ fontSize: "1.5rem", lineHeight: 1 }}>
          {total}
        </Typography>
        <Typography
          variant="label"
          component="span"
          color="text.secondary"
          sx={{ textTransform: "uppercase", fontSize: "0.5625rem" }}
        >
          Jobs
        </Typography>
      </Box>
    </Box>
  );
}
