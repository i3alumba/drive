import { createRoot } from "react-dom/client";
import { CssBaseline } from "@mui/material";
import { ThemeProvider } from "@mui/material/styles";
import { DrivePage } from "./pages/DrivePage";
import { theme } from "./theme";

createRoot(document.getElementById("root")!).render(
  <ThemeProvider
    theme={theme}
    defaultMode="system"
    modeStorageKey="drive-color-mode"
    disableTransitionOnChange
  >
    <CssBaseline enableColorScheme />
    <DrivePage />
  </ThemeProvider>,
);
