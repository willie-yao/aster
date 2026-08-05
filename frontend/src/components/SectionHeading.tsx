import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";

// Section heading with the dashboard's primary rule.
export function SectionHeading({ title }: { title: string }) {
  return (
    <Box sx={{ display: "flex", alignItems: "center", gap: 1, mb: 1.5 }}>
      <Box sx={{ width: 2, height: 18, bgcolor: "primary.main", flexShrink: 0 }} />
      <Typography variant="headline" component="h2">
        {title}
      </Typography>
    </Box>
  );
}
