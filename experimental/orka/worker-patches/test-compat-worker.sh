#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../../.." && pwd)
script="$script_dir/compat-worker.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

bash -n "$script"
"$script" verify
metadata=$("$script" metadata 0123456789abcdef0123456789abcdef01234567)
grep -Fq 'orka_commit=1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254' <<< "$metadata"
grep -Fq 'patch_sha256=b118176958f647207291bbcb5ec9d470e06dc1c31776e2f509a8a74dfca6b44d' <<< "$metadata"
backtick='`'
grep -Fq "${backtick}1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254${backtick}" "$script_dir/COMPATIBILITY.md"
grep -Fq "${backtick}b118176958f647207291bbcb5ec9d470e06dc1c31776e2f509a8a74dfca6b44d${backtick}" "$script_dir/COMPATIBILITY.md"
for patch_file in \
  analysis_budget.go \
  analysis_context.go \
  tool_alias.go \
  tool_execution.go \
  validated_analysis.go; do
  grep -Fq "diff --git a/workers/ai/$patch_file b/workers/ai/$patch_file" "$script_dir/ai-worker-convergence.patch"
done
for test_name in \
  TestExecuteAgentLoopFinalizesValidatedAnalysis \
  TestAnalysisLoopGuardUsesToolCallBudget \
  TestToolAliasIsAdvertisedAndRouted \
  TestCachedToolResultDoesNotBypassRequestAllowlist \
  TestAnalysisEvidenceLedgerPreservesSuccessfulReads \
  TestAnalysisEvidenceLedgerNeverTruncatesLongTokens \
  TestAnalysisEvidenceLedgerRetainsNewestTokenPrefix \
  TestPrepareAnalysisRequestKeepsFinalizationPromptAfterCompaction \
  TestAnalysisTransientStateRejectsNull \
  TestValidatedAnalysisResultSupportsFlatSubmission \
  TestValidationPromptsRequireRelevantFilesArray \
  TestValidationRepairPromptRequiresRelevantFilesArray \
  TestExecuteAgentLoopSendsRelevantFilesRepairToModel \
  TestAnalysisLoopGuardToolCallResetsValidationFinalRetries \
  TestExecuteAgentLoopAllowsToolCallsAfterPrematureFinals \
  TestAnalysisLoopGuardRepairsMissingGroupsInOrder \
  TestAnalysisLoopGuardFocusLeavesTimelineFourRepairsAndSubmissionRounds \
  TestAnalysisLoopGuardSynthesizesRepairWhenModelReturnsNoTools \
  TestAnalysisLoopGuardReplacesPrematureSubmissionWithRepair \
  TestAnalysisLoopGuardReplacesRepeatedOrWrongRepairPath \
  TestAnalysisLoopGuardPreservesCorrectModelRepairCall \
  TestAnalysisLoopGuardSupportsScopedAndAliasedRepairReaders \
  TestAnalysisLoopGuardCachedRepairAdvancesQueue \
  TestAnalysisLoopGuardRejectsUnsuccessfulRepairResult \
  TestAnalysisLoopGuardDoesNotRetryFailedRepairGroup \
  TestAnalysisLoopGuardRejectsRepairPlanBeyondRemainingBudget \
  TestAnalysisLoopGuardNeverResumesBroadInvestigationAfterRepair \
  TestExecuteAgentLoopSynthesizesMissingRepairReader \
  TestExecuteAgentLoopPreservesCorrectRepairReader \
  TestAnalysisLoopGuardRequiresResolvedReadArtifactForRepair \
  TestValidationRepairPromptTargetsMissingEvidenceCandidates \
  TestValidationRepairPromptUsesLedgerAfterRepairBudget \
  TestValidationNeedsEvidenceRepairRequiresCandidatesForEveryGroup \
  TestRequestApprovalCanonicalizesAliasedTarget; do
  grep -Fq "func $test_name" "$script_dir/ai-worker-convergence.patch"
done
if grep -Fq 'ToolCallSkipped' "$script_dir/ai-worker-convergence.patch"; then
  echo 'worker-only patch must not require controller/UI event taxonomy changes' >&2
  exit 1
fi
workflow="$repo_root/.github/workflows/orka-compat-image.yml"
grep -Fq 'experimental/orka/worker-patches/compat-worker.sh prepare _orka' "$workflow"
grep -Fq 'inspect-published' "$workflow"
grep -Fq 'push: false' "$workflow"
grep -Fq 'push: true' "$workflow"
if [[ $(grep -Fc 'packages: write' "$workflow") -ne 1 ]]; then
  echo 'package write permission must be limited to the publish job' >&2
  exit 1
fi
tag=$(awk -F= '$1 == "image_tag" { print $2 }' <<< "$metadata")
[[ $tag == v6-orka-1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254-dashboard-0123456789abcdef0123456789abcdef01234567 ]]
(( ${#tag} <= 128 ))
if "$script" metadata short > /dev/null 2>&1; then
  echo 'short dashboard commit was accepted' >&2
  exit 1
fi


mkdir -p "$tmp/bin"
cat > "$tmp/bin/docker" <<'EOF_DOCKER'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == "buildx imagetools inspect "* ]] || { echo "unexpected docker invocation: $*" >&2; exit 3; }
if [[ "$*" == *' --raw' ]]; then
  provenance=https://slsa.dev/provenance/v1
  if [[ ${FAKE_REGISTRY_RESULT:-} == exact-v02 ]]; then
    provenance=https://slsa.dev/provenance/v0.2
  fi
  if [[ ${FAKE_REGISTRY_RESULT:-} == no-sbom ]]; then
    printf '{"layers":[{"annotations":{"in-toto.io/predicate-type":"%s"}}]}\n' "$provenance"
  else
    printf '{"layers":[{"annotations":{"in-toto.io/predicate-type":"%s"}},{"annotations":{"in-toto.io/predicate-type":"https://spdx.dev/Document"}}]}\n' "$provenance"
  fi
  exit 0
fi
case ${FAKE_REGISTRY_RESULT:-} in
  exact|exact-v02|mismatch|no-sbom)
    label_dashboard=${FAKE_LABEL_DASHBOARD:-$EXPECTED_DASHBOARD}
    cat <<JSON
{"manifest":{"digest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","manifests":[{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","annotations":{"vnd.docker.reference.type":"attestation-manifest"}}]},"image":{"os":"linux","architecture":"amd64","config":{"User":"65532:65532","Entrypoint":["/worker"],"Labels":{"org.opencontainers.image.revision":"$label_dashboard","io.orka.compatibility.revision":"$EXPECTED_ORKA","io.orka.compatibility.patch-sha256":"$EXPECTED_PATCH"}}}}
JSON
    ;;
  missing) echo 'ERROR: ghcr.io/example/worker:test: not found' >&2; exit 1 ;;
  missing404) echo 'ERROR: unexpected status from HEAD request: 404 Not Found' >&2; exit 1 ;;
  error) echo 'ERROR: unexpected status from HEAD request: 401 Unauthorized' >&2; exit 1 ;;
  *) echo 'missing FAKE_REGISTRY_RESULT' >&2; exit 3 ;;
esac
EOF_DOCKER
chmod +x "$tmp/bin/docker"

dashboard=0123456789abcdef0123456789abcdef01234567
orka=1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254
patch=b118176958f647207291bbcb5ec9d470e06dc1c31776e2f509a8a74dfca6b44d
published=$(FAKE_REGISTRY_RESULT=exact EXPECTED_DASHBOARD=$dashboard EXPECTED_ORKA=$orka EXPECTED_PATCH=$patch PATH="$tmp/bin:$PATH"   "$script" inspect-published ghcr.io/example/worker:test "$dashboard")
grep -Fq '"digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"' <<< "$published"
grep -Fq '"recovered": true' <<< "$published"
FAKE_REGISTRY_RESULT=exact-v02 EXPECTED_DASHBOARD=$dashboard EXPECTED_ORKA=$orka EXPECTED_PATCH=$patch PATH="$tmp/bin:$PATH" \
  "$script" inspect-published ghcr.io/example/worker:test "$dashboard" > /dev/null

for missing in missing missing404; do
  set +e
  FAKE_REGISTRY_RESULT=$missing PATH="$tmp/bin:$PATH"     "$script" inspect-published ghcr.io/example/worker:test "$dashboard" > /dev/null 2>&1
  rc=$?
  set -e
  [[ $rc -eq 10 ]] || { echo "$missing registry response returned $rc, want 10" >&2; exit 1; }
done

for failure in mismatch no-sbom error; do
  label_dashboard=$dashboard
  if [[ $failure == mismatch ]]; then
    label_dashboard=ffffffffffffffffffffffffffffffffffffffff
  fi
  set +e
  FAKE_REGISTRY_RESULT=$failure FAKE_LABEL_DASHBOARD=$label_dashboard \
    EXPECTED_DASHBOARD=$dashboard EXPECTED_ORKA=$orka EXPECTED_PATCH=$patch \
    PATH="$tmp/bin:$PATH" \
    "$script" inspect-published ghcr.io/example/worker:test "$dashboard" \
    > /dev/null 2>&1
  rc=$?
  set -e
  [[ $rc -eq 1 ]] || { echo "$failure registry response returned $rc, want 1" >&2; exit 1; }
done

echo 'Orka compatibility metadata checks passed.'
