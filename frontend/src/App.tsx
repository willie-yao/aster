import { BrowserRouter, Routes, Route } from "react-router-dom";
import { DashboardPage } from "./pages/DashboardPage";
import { JobDetailPage } from "./pages/JobDetailPage";
import { TestDetailPage } from "./pages/TestDetailPage";
import { FlakinessPage } from "./pages/FlakinessPage";
import { ActionRequestPage } from "./pages/ActionRequestPage";
import { AnalysisTracesPage } from "./pages/AnalysisTracesPage";
import { Layout } from "./components/Layout";
import { ManifestProvider } from "./components/ManifestProvider";
import { CapabilitiesProvider } from "./components/CapabilitiesProvider";
import { AuthProvider } from "./components/AuthProvider";

// Vite injects BASE_URL with a trailing slash; BrowserRouter wants none.
const basename = import.meta.env.BASE_URL.replace(/\/$/, "");

export default function App() {
  return (
    <ManifestProvider>
      <CapabilitiesProvider>
        <AuthProvider>
          <BrowserRouter basename={basename}>
            <Routes>
              <Route element={<Layout />}>
                <Route index element={<DashboardPage />} />
                <Route path="flaky" element={<FlakinessPage />} />
                <Route path="analysis-traces" element={<AnalysisTracesPage />} />
                <Route path="job/:jobName" element={<JobDetailPage />} />
                <Route
                  path="job/:jobName/test/:testName"
                  element={<TestDetailPage />}
                />
                <Route
                  path="action-request/:requestID"
                  element={<ActionRequestPage />}
                />
              </Route>
            </Routes>
          </BrowserRouter>
        </AuthProvider>
      </CapabilitiesProvider>
    </ManifestProvider>
  );
}
