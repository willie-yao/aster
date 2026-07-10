import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";

// Section heading with the dashboard's accent tick.
export function SectionHeading({ title }: { title: string }) {
  return (
    <Box sx={{ display: "flex", alignItems: "center", gap: 1.25, mb: 1.5 }}>
      <Box sx={{ width: 4, height: 18, borderRadius: 999, bgcolor: "primary.main", flexShrink: 0 }} />
      <Typography variant="headline" component="h2">
        {title}
      </Typography>
    </Box>
  );
}
