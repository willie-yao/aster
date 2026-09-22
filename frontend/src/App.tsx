import { lazy } from "react";
import { BrowserRouter, Routes, Route } from "react-router-dom";
import { DashboardPage } from "./pages/DashboardPage";
import { NotFoundPage } from "./pages/NotFoundPage";
import { Layout } from "./components/Layout";
import { ManifestProvider } from "./components/ManifestProvider";
import { CapabilitiesProvider } from "./components/CapabilitiesProvider";
import { AuthProvider } from "./components/AuthProvider";

const JobDetailPage = lazy(() =>
  import("./pages/JobDetailPage").then((module) => ({ default: module.JobDetailPage })),
);
const TestDetailPage = lazy(() =>
  import("./pages/TestDetailPage").then((module) => ({ default: module.TestDetailPage })),
);
const BuildFailurePage = lazy(() =>
  import("./pages/BuildFailurePage").then((module) => ({ default: module.BuildFailurePage })),
);
const FlakinessPage = lazy(() =>
  import("./pages/FlakinessPage").then((module) => ({ default: module.FlakinessPage })),
);
const PullRequestsPage = lazy(() =>
  import("./pages/PullRequestsPage").then((module) => ({ default: module.PullRequestsPage })),
);
const PullRequestDetailPage = lazy(() =>
  import("./pages/PullRequestDetailPage").then((module) => ({ default: module.PullRequestDetailPage })),
);
const SharedFailurePage = lazy(() =>
  import("./pages/SharedFailurePage").then((module) => ({ default: module.SharedFailurePage })),
);
const ActionRequestPage = lazy(() =>
  import("./pages/ActionRequestPage").then((module) => ({ default: module.ActionRequestPage })),
);
const AnalysisHealthPage = lazy(() =>
  import("./pages/AnalysisHealthPage").then((module) => ({ default: module.AnalysisHealthPage })),
);
const InvestigationHistoryPage = lazy(() =>
  import("./pages/InvestigationHistoryPage").then((module) => ({ default: module.InvestigationHistoryPage })),
);
const InvestigationSessionPage = lazy(() =>
  import("./pages/InvestigationHistoryPage").then((module) => ({ default: module.InvestigationSessionPage })),
);
const AIUsagePage = lazy(() =>
  import("./pages/AIUsagePage").then((module) => ({ default: module.AIUsagePage })),
);

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
                <Route path="pull-requests" element={<PullRequestsPage />} />
                <Route
                  path="pull-requests/shared/:id"
                  element={<SharedFailurePage />}
                />
                <Route
                  path="pull-requests/:number"
                  element={<PullRequestDetailPage />}
                />
                <Route path="analysis-health" element={<AnalysisHealthPage />} />
                <Route path="investigations" element={<InvestigationHistoryPage />} />
                <Route path="investigations/:sessionID" element={<InvestigationSessionPage />} />
                <Route path="ai-usage" element={<AIUsagePage />} />
                <Route path="job/:jobName" element={<JobDetailPage />} />
                <Route
                  path="job/:jobName/test/:testName"
                  element={<TestDetailPage />}
                />
                <Route
                  path="job/:jobName/build/:buildId/failure"
                  element={<BuildFailurePage />}
                />
                <Route
                  path="action-request/:requestID"
                  element={<ActionRequestPage />}
                />
                <Route path="*" element={<NotFoundPage />} />
              </Route>
            </Routes>
          </BrowserRouter>
        </AuthProvider>
      </CapabilitiesProvider>
    </ManifestProvider>
  );
}
