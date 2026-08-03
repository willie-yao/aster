function routeSegment(value: string): string {
  return encodeURIComponent(value);
}

export function jobPath(jobID: string): string {
  return `/job/${routeSegment(jobID)}`;
}

export function testPath(jobID: string, testName: string): string {
  return `${jobPath(jobID)}/test/${routeSegment(testName)}`;
}

export function jobRunPath(jobID: string, runID: string): string {
  return `${jobPath(jobID)}?run=${routeSegment(runID)}`;
}

export function testRunPath(
  jobID: string,
  testName: string,
  runID: string,
): string {
  return `${testPath(jobID, testName)}?run=${routeSegment(runID)}`;
}

export function buildFailurePath(jobID: string, buildID: string): string {
  return `${jobPath(jobID)}/build/${routeSegment(buildID)}/failure`;
}

export function actionRequestPath(requestID: string): string {
  return `/action-request/${routeSegment(requestID)}`;
}
