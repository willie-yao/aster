import SvgIcon from "@mui/material/SvgIcon";
import { useId } from "react";

// The Aster mark. Each instance mints its own gradient id so several marks can
// coexist in one document (the rail and the mobile bar are both mounted).
export function AsterMark({ size = 30 }: { size?: number }) {
  const gradientId = useId();
  return (
    <SvgIcon viewBox="0 0 64 64" sx={{ fontSize: size }}>
      <defs>
        <linearGradient id={gradientId} x1="0" y1="0" x2="0.35" y2="1">
          <stop offset="0" stopColor="var(--mui-palette-brand-from)" />
          <stop offset="1" stopColor="var(--mui-palette-brand-to)" />
        </linearGradient>
      </defs>
      <path
        d="M32 5.1 60 58.9 32 49.02 4 58.9Z M32 18.32 40.78 38.22 32 34.1 23.22 38.22Z"
        fill={`url(#${gradientId})`}
        fillRule="evenodd"
      />
    </SvgIcon>
  );
}
