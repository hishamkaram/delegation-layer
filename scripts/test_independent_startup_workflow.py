"""The native component CI job executes required proofs and preserves their failures."""

import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest

from scripts.test_candidate_build_workflow import job_text, steps, WORKFLOW


PRODUCER_STARTER_CASES = (
    "TestProducerOutsideStarterOwnsCapturedEnvironment",
    "TestProducerResidentSnapshotUsesSharedAcceptanceWithoutSourceGuards",
    "TestProducerResidentSnapshotRequiresActualOpenGrant",
    "TestProducerResidentSnapshotRevalidatesPublishedEndpoint",
    "TestProducerSameRootOwnerRequestRefusedBeforeTargetPreparation",
    "TestOwnerEntryRejectsContainedTaskBeforeAuthority",
)


NATIVE_OBSERVATION_CASES = (
    "TestShutdownLeasePollInterruptedWait",
    "TestShutdownLeasePollInterruptRespectsOriginalContext",
    "TestShutdownLeasePollPreservesFaults",
    "TestShutdownLeaseKeventInterruptedWait",
    "TestShutdownLeaseKeventInterruptRespectsOriginalContext",
    "TestShutdownLeaseKeventPreservesFaultsAndLatchesEvent",
    "TestShutdownLeaseLsofRegularDescriptorRecords",
    "TestNestedNativeObservationPreservesCallerContext",
)


def go_test_commands(target: str) -> list[tuple[list[str], list[str]]]:
    commands = []
    for line in target.splitlines():
        tokens = shlex.split(line)
        index = 0
        while index < len(tokens) and re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*=.*", tokens[index]):
            index += 1
        if tokens[index:index + 2] == ["go", "test"]:
            command = tokens[index:]
            if "-c" not in command:
                commands.append((tokens[:index], command))
    return commands


def native_make_target() -> str:
    source = (WORKFLOW.parents[2] / "Makefile").read_text().replace("\\\n", " ")
    return source.split("acceptance-independent-startup: tool-versions\n", 1)[1].split("\n\n", 1)[0]


NATIVE_FOUNDATION_CASES = {
    "internal/supervisor": (
        "TestContinuationPairPrefersPublishedInstallation",
        "TestContinuationPairMissingPublishedMemberUsesSelectedInstallation",
        "TestContinuationPairCorruptPublicationRemainsFault",
        "TestControlContinuationWarmMappedGenerationNeedsNoRoute",
        "TestControlContinuationReadsActualThirdGenerationAfterRoute",
        "TestControlContinuationReadsCompatibleRuntimeWithoutInventory",
        "TestControlContinuationHealthyPairIgnoresUnavailableTransport",
        "TestControlRuntimeSavedReadsCompatibleRuntimeWithoutInventory",
        "TestControlRuntimeSavedUnavailablePairStillRoutesOwner",
        "TestControlRuntimeSavedCapturedPATHRefusalDoesNotRouteOwner",
        "TestControlRuntimeSavedEmptyCapturedPATHRefusesOwnerRoute",
        "TestControlRuntimeSavedReadsCapturedPATHTransport",
        "TestControlContinuationPreservesLegacyClientUpdateAndUnusedInventoryFault",
        "TestControlContinuationRefusesDamagedManagedPair",
        "TestRecoveryCandidateKeepsRecordedPairBeforeUnusedInventory",
        "TestRecoveryCandidateUsesCapturedPATHWithoutOverridingInitialChoice",
        "TestRecoveryCandidatePreservesJoinedBundleFaults",
        "TestPrepareTaskSupervisorDefersCompatibleRuntimeMaintenance",
        "TestPrepareAdmissionSupervisorAcceptsCompatibleDaemonBeforeScheduling",
        "TestPrivateAdmissionRuntimeRefusesDifferentLogicalGenerationWithoutAuthorization",
        "TestInfrastructureLaunchContextPreservesSelectedCallbackAndMaintenanceScopes",
        "TestInfrastructureLaunchContextPreservesNativeSizedEnvironments",
        "TestInfrastructureLaunchContextRebasesReservedOverlay",
        "TestInfrastructureTargetPinRefusesOtherActualStore",
        "TestAcceptedSupervisorPairKeepsDifferentlyNamedInstalledClient",
        "TestResolveSavedSupervisorClientPreservesRecordedInstallation",
        "TestResolveSavedSupervisorClientSelectsInstalledRoleForLegacyCopy",
        "TestResolveSavedSupervisorClientFallsBackOnlyForAbsence"),
    "internal/app": (
        "TestIssue135PublicContinuationPublishedLegacyPair",
        "TestIssue135PublicContinuationSeedContract",
        "TestIssue135PublicContinuationDataFaultStillClosesOwnedHandles",
        "TestIssue135PublicContinuationUnfinishedWaitRetainsBorrowers",
        "TestIssue135PublicContinuationReleaseFaultGatesRemoval",
        "TestInfrastructureRoutingCleanupRetainsActualBorrowedStore",
        "TestModelDiscoveryPrivatePreparationUsesCapturedRoutingFault",
        "TestModelDiscoveryCleanupRetainsRoutingStore",
        "TestSchedulerObservationCleanupRetainsRoutingStore",
        "TestSchedulerObserveRetainsActualRoutedStoreAndStopsAdmission",
        "TestSchedulerDispatchRetainsCleanupBeforeAnyObservation",
        "TestSchedulerCleanupClassificationPrecedesStorageIncompatibility",
        "TestSchedulerCleanupProjectionRetainsStoreCloseOwner",
        "TestInfrastructureRoutingSelectedOwnerDoesNotOpenUnavailableOrigin",
        "TestInfrastructureEstablishmentHostResolvesInstalledRunnerInPlace",
        "TestInfrastructureEstablishmentHostRejectsUnavailableInstalledRunner",
        "TestInfrastructureRoutingProbeDiagnosticPreservesConsumerPolicy",
        "TestInfrastructureRoutingProbeDiagnosticRetainsActualCleanupOwner",
        "TestInfrastructureRoutingManualMigrationSkipsOwnerProbe"),
    "internal/pueue/internal/control": (
        "TestObservePrivateRestoreRequiresFullPublishedGeneration",
        "TestObservePrivateRestoreReadsOnlySelectedFileWithoutWrites",
        "TestObservePrivateRestoreGzipRequiresOneCompleteMember",
        "TestObservePrivateRestoreMissingIsOnlySelectedLeafOrData",
        "TestObservePrivateRestorePartialAlwaysRefuses",
        "TestObservePrivateRestoreResolutionNeverUsesAmbient",
        "TestObservePrivateRestoreCancellationHasNoSelection",
        "TestObservePrivateRestoreCancellationDuringNativeReadIsOperational",
        "TestPrivateRestoreFaultClassificationPreservesOperationalCauses",
        "TestPrivateRestoreKeepsConcurrentReadCancellationAndParserFaults",
        "TestPrivateRestoreRetainsWholeAncestryAndConfig",
        "TestPrivateRestoreCloseCollectsEveryOwnedDescriptor",
        "TestPrivateRestoreStreamRetainsUnknownNativeStates",
        "TestProbeSavedPrivateCompatibleHealthyExactIgnoresSelector",
        "TestProbeSavedPrivateCompatibleUnavailablePairReadsThroughD",
        "TestProbeSavedPrivateCompatibleStoppedDoesNotStart",
        "TestProbeSavedPrivateCompatibleUnrelatedSavedDoesNotSelectD",
        "TestProbeSavedPrivateCompatibleSelectorFaultGraphIsHard",
        "TestProbeSavedPrivateTaskObservationReadsEquivalentAliasAfterPrimaryRetarget",
        "TestProbeSavedPrivateTaskObservationRefusesUnsafeDriftedAlias",
        "TestProbeSavedPrivateTaskObservationOlderBindingCannotUseEquivalentSuccessorAlias",
        "TestProbeSavedPrivateTaskObservationAliasSnapshotHoldsSharedLease",
        "TestProbeSavedPrivateTaskObservationUnrelatedCompleteExactStaysNotReady",
        "TestProbeSavedPrivateCompatibleHealthyAliasRemainsExact",
        "TestProbeSavedPrivateCompatibleNeverUsesDThroughIncompleteGeneration",
        "TestProbeSavedPrivateCompatiblePairFaultIsNotUnavailable",
        "TestProbeSavedPrivateCompatibleRechecksSelectedPublication",
        "TestProbeSavedPrivateCompatibleMissingMemberCannotHideOtherStatFault",
        "TestProbeSavedPrivateCompatibleCancellationDoesNotSelectD",
        "TestProbeSavedPrivateCompatibleUnavailableIsOnlyPureSelectorFact",
        "TestProbeSavedPrivateCompatibleRestoresBindingTagForCompletedFault",
        "TestProbeSavedPrivateCompatibleRetainsPendingExceptionAndJoinedFault",
        "TestProbeSavedPrivateCompatibleExactValidConfigDriftIsNotReady",
        "TestExactPrivatePairComparisonReadFaultStaysHard",
        "TestPrivateAdmissionRuntimeRefusalUsesCanonicalReadyPredicate",
        "TestPrivateAdmissionRuntimeRequiredFaultsSurviveUnsettledOrMissingFacts",
        "TestPrivateAdmissionRuntimeLeavesAliasAndMigrationErrorsToExistingOwners",
        "TestCheckPrivateAdmissionRuntimeCanonicalStoreDecisionsAreReadOnly",
        "TestCheckPrivateAdmissionRuntimeKeepsRequiredReadFaults",
        "TestCheckPrivateAdmissionRuntimeUnsettledCannotMaskIdentityPublicationReadFault",
        "TestCheckPrivateAdmissionRuntimeLockAndContextFaultsStayFaults",
        "TestCheckPrivateAdmissionRuntimeMissingLockIsNotPositiveMismatch",
        "TestPrivateBootstrapScopeRevokesRetainedTag",
        "TestManagedShutdownRejectsRevokedRetainedBootstrap",
        "TestPrivateBootstrapScopeSurvivesNestedLockAndCancellation",
        "TestPrivateBootstrapZeroScopeCannotConsumeAdmission",
        "TestPrivateBootstrapConsumePreservesPrivateAdmissionFault",
        "TestManagedShutdownFinalConsumeRejectsRevocationAfterDup",
        "TestLegacyUnscopedShutdownKeepsExistingBehavior",
        "TestStopRecoveredWithinBootstrapPreservesCleanupAuthority",
        "TestBootstrapCleanupSettledResultsReturnedSynchronously",
        "TestBootstrapCleanupSettledCloseFaultReturnedSynchronously",
        "TestBootstrapCleanupSettledNativeLockClosesBeforeReturn",
        "TestBootstrapCleanupOwnerRetainsNativeLockAndPendingGraph",
        "TestBootstrapCleanupPreservesPureInFlightProjection",
        "TestBootstrapCleanupLateNestedPendingRetainsNativeLock",
        "TestBootstrapCleanupSettledResultRetainsUnfinishedChild",
        "TestBootstrapCleanupReleaseFaultIsOwnedAndReported",
        "TestBootstrapCleanupRepeatedCloseKeepsHistoryAfterNativeRelease",
        "TestFinishPrivateBootstrapRetainsLockForEveryPendingCommand",
        "TestRetainBootstrapLockReportsDeferredReleaseFailure"),
    "internal/scheduler/runtime": (
        "TestSchedulerCleanupUnresolvedJoinsAllWorkersAndErrorGraphs",
        "TestSchedulerTaskCleanupMarkerPrecedesCancellationAndState",
        "TestSchedulerSelectedResultRetainsCleanupGraphWhenJoiningWakerFails",
        "TestSchedulerRunJoinsRemainingWorkerAfterSelectedResultAndWakerFault"),
    "internal/scheduler/host": (
        "TestHostPermanentChildFaultThroughBothFinalDrains",
        "TestHostCleanupFrameDoesNotOverrideAcceptedStop",
        "TestHostUnverifiedCleanupEventRetainsOrdinaryExitPolicy",
        "TestHostReportsOneCleanupFrameBeforeIncompatibility",
        "TestHostCleanupFrameWriteFailurePreservesGraph"),
    "internal/supervisor/infrastructure": (
        "TestLeaseInheritedReferenceRetainsExclusion",
        "TestAdoptionDoesNotAcquireFreeFlock",
        "TestCloseSerializesWithBorrower",
        "TestOriginValidateRootIsObservationalAndIdentityBound",
        "TestReadOriginRefusesMalformedRouting",
        "TestStartupIntentObservationNeverRenewsDeadline",
        "TestOwnerPublicationBindsOriginalAssumption",
        "TestOwnerJournalCreatesAssumptionAndPublicationOnce",
        "TestOwnerJournalAssumptionChecksOriginalDeadlineInTransaction",
        "TestInfrastructureContextStagingDoesNotRequireResidentLease",
        "TestInfrastructureContextCancellationBetweenChunksIsNotEvidenceFault",
        "TestContextChunksRoundTripEscapedAndNonUTF8Payload",
        "TestContextChunksStorageAboveFormerRoleCap",
        "TestContextManifestInvalidCountsRefuseBeforeChunkIO",
        "TestIntentRequiresCompleteBoundContext",
        "TestEndpointBindsInitialPrivateRootSocket",
        "TestEndpointClosePreservesReplacementAndCanRetry",
        "TestEndpointFailedBirthRetainsCleanupOwnership",
        "TestEndpointCleanupUsesIdentityAfterSocketModeChange",
        "TestIntentJournalCanonicalPublication",
        "TestStartupSettlementMalformedDarwinAssumptionIsNotAbsence",
        "TestProducerNativeLifecycle",
        "TestProducerObservedChildUsesNativeIdentitySemantics",
        "TestProducerExpiredDeadlineRefusesBeforeTargetWrites",
        "TestProducerDeadlineValidity",
        "TestProducerDeadlineAcceptsCanonicalMigrationControlBudget",
        "TestProducerBusyTerminalAttemptRefusesJoin",
        "TestOwnerServerConnectionCloseAndMapRemovalAreOneLockedPhase",
        "TestOwnerServerRejectedConnectionsRaceDrainWithoutCleanupFault",
        "TestOwnerServerOperationCapacityReservesStatusAndReturnsExactBusy",
        "TestOwnerClientProbesAuthenticatedResidentAfterStartupDeadline",
        "TestOwnerClientOwnPreparationPreservesSelectedContext",
        "TestOwnerClientDoesNotReplayBusyOrUnknownContact",
        "TestOwnerClientRequiresFiniteContextBeforeContact",
        "TestOwnerClientPreservesSavedSubject",
        "TestOwnerClientPreservesOtherRootSelectionWithoutBirth") + PRODUCER_STARTER_CASES,
    "internal/task": ("TestDecodeStrictByteSliceCanonicalRoundTrip",
                      "TestDecodeStrictByteSlicePreservesStructuralChecks"),
    "internal/lifetime": ("TestWithoutProcessScopeMarkerOwnsExactKeyRemoval",
                          "TestConfigureProcessScopeEnvironmentReplacesReservedMarker",
                          "TestConfigureProcessScopeEnvironmentPreservesInheritanceAndRefusal",
                          "TestSameProcessUsesCanonicalIncarnationSemantics",
                          "TestNewProcessRefRejectsMalformedRecognizedDarwinTokens",
                          "TestDarwinProcessPresentUsesStableUniqueID",
                          "TestShutdownLeaseNativeProof",
                          "TestShutdownLeaseDriverInterruption",
                          "TestShutdownLeaseDriverCleanupFaults",
                          "TestShutdownLeaseInstalledFixtureSelection",
                          "TestManagedShutdownLeaseNativeProof") + NATIVE_OBSERVATION_CASES,
    "internal/taskdir": ("TestInfrastructurePassiveMissingJournal",
                         "TestRootPinRejectsReplacementWithIdenticalMetadata"),
}


class IndependentStartupWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.source = WORKFLOW.read_text()
        self.job = job_text(self.source, "independent-startup-component")
        self.proof = steps(self.job)["Prove native infrastructure components only"]

    def test_compiling_a_fixture_does_not_execute_its_tests(self):
        compile_only = "go test -c -race -o fixture.test ./internal/lifetime"
        execute = "PROOF=1 go test -race -count=1 -timeout=4m ./internal/lifetime"
        self.assertEqual(go_test_commands(compile_only), [])
        commands = go_test_commands(compile_only + "\n" + execute)
        self.assertEqual(len(commands), 1)
        self.assertEqual(commands[0][0], ["PROOF=1"])
        self.assertNotIn("-c", commands[0][1])

    def test_native_component_job_keeps_full_ci_selection_without_an_exclusive_mode(self):
        self.assertNotIn("native_startup_component_only", self.source)
        for name in ("skill-package-macos", "gate-macos", "nested-survival", "development-candidate", "ccr"):
            with self.subTest(job=name):
                condition = job_text(self.source, name).split("    runs-on:", 1)[0]
                self.assertIn("needs.changes.outputs.full == 'true'", condition)
        linux_package = job_text(self.source, "skill-package-linux").split("    steps:", 1)[0]
        self.assertNotIn("    if:", linux_package)
        linux_gate = job_text(self.source, "gate-linux").split("    steps:", 1)[0]
        self.assertIn("if: ${{ always() }}", linux_gate)
        self.assertIn("needs: development-candidate", job_text(self.source, "development-candidate-install"))
        self.assertIn("needs.changes.outputs.full == 'true'", self.job)
        self.assertIn("os: [ubuntu-24.04, macos-15]", self.job)
        self.assertIn("DELEGATE_INDEPENDENT_EXPECTED_BACKEND: process-group", self.proof)
        self.assertNotIn("continue-on-error", self.job)

    def test_failed_make_is_not_hidden_by_successful_tee(self):
        shell = re.search(r"^        shell: (\S+)$", self.proof, re.M)
        if shell is None:
            self.fail("explicit bash is required for GitHub pipefail")
        self.assertEqual(shell.group(1), "bash")
        script = self.proof.split("        run: |\n", 1)[1]
        script = "\n".join(line.removeprefix("          ") for line in script.splitlines())
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            stub = root / "make"
            stub.write_text("#!/bin/sh\nexit 17\n")
            stub.chmod(0o700)
            result = subprocess.run(
                ["bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", script],
                cwd=root, env={"PATH": str(root) + os.pathsep + os.defpath},
                capture_output=True, text=True, timeout=10, check=False)
            self.assertEqual(result.returncode, 17, result.stderr)
            self.assertTrue((root / "bin/independent-startup-proof/native.log").is_file())

    def test_make_target_runs_required_native_cases_with_race_and_verified_pair(self):
        target = native_make_target()
        self.assertIn("./scripts/install-test-supervisor.sh", target)
        self.assertIn("DELEGATE_INDEPENDENT_EXPECTED_BACKEND:?", target)
        self.assertIn("go test -race -count=1 -timeout=4m", target)
        self.assertIn("NativeProof|PeerProof", target)
        for package in ("./internal/lifetime", "./internal/scheduler/host"):
            self.assertIn(package, target)
        for package, cases in (
            ("internal/lifetime", ("TestIndependentStartupNativeProof",)),
            ("internal/scheduler/host", ("TestIndependentStartupPeerProof",)),
        ):
            self.assert_native_cases_exist(package, cases)

    def test_executed_native_commands_keep_race_count_and_watchdog(self):
        commands = [command for _, command in go_test_commands(native_make_target())]
        self.assertTrue(commands, "native component tests must run")
        for command in commands:
            for option in ("-race", "-count=1", "-timeout=4m"):
                self.assertIn(option, command)

    def test_component_retains_bounded_native_log(self):
        upload = steps(self.job)["Retain bounded native component output"]
        self.assertIn("bin/independent-startup-proof/native.log", upload)
        self.assertNotIn("/**", upload)
        self.assertNotIn("continue-on-error", self.job)

    def test_native_target_exercises_production_foundations(self):
        commands = [command for _, command in go_test_commands(native_make_target())]
        for package, cases in NATIVE_FOUNDATION_CASES.items():
            self.assert_native_cases_exist(package, cases)
            for case in cases:
                with self.subTest(case=case):
                    selected = []
                    for command in commands:
                        if "./" + package not in command:
                            continue
                        pattern = (command[command.index("-run") + 1]
                                   if "-run" in command else "^Test")
                        selected.append(bool(re.search(pattern.replace("$$", "$"), case)))
                    self.assertEqual(sum(selected), 1,
                                     "each required test must run exactly once")

    def test_shutdown_lease_proof_cannot_pass_by_leaving_its_guard_unset(self):
        target = native_make_target()
        selected = [environment for environment, command in go_test_commands(target)
                    if "./internal/lifetime" in command and "-run" in command
                    and re.search(command[command.index("-run") + 1], "TestShutdownLeaseNativeProof")]
        self.assertEqual(len(selected), 1)
        self.assertIn("DELEGATE_SHUTDOWN_LEASE_PROOF=1", selected[0])
        self.assertIn('DELEGATE_SHUTDOWN_LEASE_PUEUE=$(BIN_DIR)/test-supervisor/pueue', selected[0])

    def test_managed_shutdown_uses_built_owner_and_caller_with_explicit_guard(self):
        target = native_make_target()
        selected = [environment for environment, command in go_test_commands(target)
                    if "./internal/lifetime" in command and "-run" in command
                    and re.search(command[command.index("-run") + 1], "TestManagedShutdownLeaseNativeProof")]
        self.assertEqual(len(selected), 1)
        for value in (
            "DELEGATE_SHUTDOWN_LEASE_PROOF=1",
            "DELEGATE_SHUTDOWN_LEASE_PUEUE=$(BIN_DIR)/test-supervisor/pueue",
            "DELEGATE_SHUTDOWN_LEASE_CONTROL_FIXTURE=$(BIN_DIR)/test-supervisor/private-control-fixture.test",
            "DELEGATE_SHUTDOWN_LEASE_CONTROL_OWNER=$(BIN_DIR)/test-supervisor/delegate-run",
        ):
            self.assertIn(value, selected[0])
        owner = 'go build -buildvcs=false -o "$(BIN_DIR)/test-supervisor/delegate-run" ./cmd/delegate-run'
        caller = 'go test -c -race -o "$(BIN_DIR)/test-supervisor/private-control-fixture.test" ./internal/pueue/internal/control'
        self.assertLess(target.index(owner), target.index("-run '^TestManagedShutdownLease'"))
        self.assertLess(target.index(caller), target.index("-run '^TestManagedShutdownLease'"))

    def assert_native_cases_exist(self, package, cases):
        directory = WORKFLOW.parents[2] / package
        source = "\n".join(path.read_text() for path in directory.glob("*_test.go"))
        declared = re.findall(r"^func (Test\w+)\(", source, re.M)
        for case in cases:
            with self.subTest(package=package, case=case):
                self.assertIn(case, declared, "native target must name an existing test")


if __name__ == "__main__":
    unittest.main()
