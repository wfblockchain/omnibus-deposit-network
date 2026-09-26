// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Vm } from "forge-std/Vm.sol";
import { OmnibusBase } from "./OmnibusBase.t.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { PaymentRouter } from "src/omnibus/PaymentRouter.sol";
import { IERC7943FungibleToken } from "src/omnibus/IERC7943.sol";

contract OmnibusModelTest is OmnibusBase {

    function setUp() public override {
        super.setUp();
        _fund(BANK_A, 1_000_000 * M, "FW-A-1");
        _fund(BANK_B, 1_000_000 * M, "FW-B-1");
        _mintA(alice, 10_000 * M, "CORE-A-1");
    }

    /*//////////////////////////////////////////////////////////////////////////
                            Issuance against the omnibus
    //////////////////////////////////////////////////////////////////////////*/

    function test_MintEncumbersTheIssuersFreePosition() public view {
        assertEq(dtA.balanceOf(alice), 10_000 * M);
        assertEq(_backing(BANK_A), 10_000 * M);
        assertEq(ledger.freePosition(BANK_A), 990_000 * M);
        assertTrue(ledger.invariantsHold());
    }

    function test_NoMintBeyondFreePosition() public {
        vm.prank(aIssuer);
        vm.expectRevert(
            abi.encodeWithSelector(
                OmnibusLedger.InsufficientFreePosition.selector, BANK_A, 990_001 * M, 990_000 * M
            )
        );
        dtA.mint(alice, 990_001 * M, "CORE-A-2");
    }

    function test_MintReferenceIsIdempotent() public {
        vm.prank(aIssuer);
        vm.expectRevert(abi.encodeWithSelector(BankToken.RefAlreadyUsed.selector, bytes32("CORE-A-1")));
        dtA.mint(alice, 1, "CORE-A-1");
    }

    function test_OnlyTheIssuersTokenCanEncumberItsPosition() public {
        vm.prank(outsider);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.NotARegisteredToken.selector, outsider));
        ledger.encumber(1);
    }

    function test_RedeemFreesBackingAndNeedsAnAvailableBalance() public {
        vm.prank(alice);
        dtA.redeem(4_000 * M, "RED-1");
        assertEq(_backing(BANK_A), 6_000 * M);
        assertEq(ledger.freePosition(BANK_A), 994_000 * M);

        vm.prank(aCompliance);
        dtA.setFrozenTokens(alice, 6_000 * M);
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(BankToken.InsufficientAvailable.selector, alice, 1, 0));
        dtA.redeem(1, "RED-2");
    }

    function test_BankRedeemsOnlyWithinTheHoldersAllowance() public {
        vm.prank(aIssuer);
        vm.expectRevert();
        dtA.redeemFrom(alice, 100 * M, "RED-3");

        vm.prank(alice);
        dtA.approve(aIssuer, 100 * M);
        vm.prank(aIssuer);
        dtA.redeemFrom(alice, 100 * M, "RED-3");
        assertEq(dtA.balanceOf(alice), 9_900 * M);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    On-us transfer
    //////////////////////////////////////////////////////////////////////////*/

    function test_SameTickerTransferLeavesTheOmnibusUntouched() public {
        uint256 before = _position(BANK_A);
        vm.prank(alice);
        dtA.transfer(carol, 2_500 * M);
        assertEq(dtA.balanceOf(carol), 2_500 * M);
        assertEq(_position(BANK_A), before);
        assertEq(_backing(BANK_A), 10_000 * M);
    }

    function test_OnlyTheIssuersAdmittedHoldersCanHoldItsTicker() public {
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(IERC7943FungibleToken.ERC7943CannotReceive.selector, bob));
        dtA.transfer(bob, 1); // bob banks at BANK_B; BANK_A has not admitted him
    }

    /*//////////////////////////////////////////////////////////////////////////
                                Cross-bank payments
    //////////////////////////////////////////////////////////////////////////*/

    function test_InstantPaymentMovesReservesInsideTheOmnibusAtPar() public {
        vm.prank(bOperator);
        ledger.setRequiresAcceptance(BANK_B, false);
        uint256 total = ledger.omnibusTotal();

        vm.prank(alice);
        (bytes32 id, PaymentRouter.Status st) = router.pay(dtA, dtB, bob, 3_000 * M, "UETR-1");

        assertEq(uint8(st), uint8(PaymentRouter.Status.Settled));
        assertEq(dtA.balanceOf(alice), 7_000 * M);
        assertEq(dtB.balanceOf(bob), 3_000 * M, "par: 3,000 in, 3,000 out");
        assertEq(_position(BANK_A), 997_000 * M);
        assertEq(_position(BANK_B), 1_003_000 * M);
        assertEq(_backing(BANK_B), 3_000 * M);
        assertEq(ledger.omnibusTotal(), total, "the Fed balance does not move");
        assertTrue(ledger.invariantsHold());
        assertEq(router.payment(id).payee, bob);
    }

    function test_PaymentWaitsForTheReceivingBankThenSettles() public {
        vm.prank(alice);
        (bytes32 id, PaymentRouter.Status st) = router.pay(dtA, dtB, bob, 3_000 * M, "UETR-2");
        assertEq(uint8(st), uint8(PaymentRouter.Status.Pending));
        assertEq(dtA.heldBalanceOf(alice), 3_000 * M);
        assertEq(dtA.availableBalanceOf(alice), 7_000 * M);
        assertEq(dtB.balanceOf(bob), 0);

        vm.prank(alice); // held funds cannot be spent twice
        vm.expectRevert();
        dtA.transfer(carol, 7_001 * M);

        vm.prank(bOperator);
        router.accept(id);
        assertEq(dtB.balanceOf(bob), 3_000 * M);
        assertEq(dtA.balanceOf(alice), 7_000 * M);
        assertEq(dtA.heldBalanceOf(alice), 0);
        assertTrue(ledger.invariantsHold());
    }

    function test_RejectReleasesTheHoldWithAReasonCode() public {
        vm.prank(alice);
        (bytes32 id,) = router.pay(dtA, dtB, bob, 3_000 * M, "UETR-3");
        vm.expectEmit(true, false, false, true, address(router));
        emit PaymentRouter.PaymentRejected(id, "AC04");
        vm.prank(bOperator);
        router.reject(id, "AC04");
        assertEq(dtA.heldBalanceOf(alice), 0);
        assertEq(dtA.availableBalanceOf(alice), 10_000 * M);
    }

    function test_OnlyTheReceivingBankAnswers() public {
        vm.prank(alice);
        (bytes32 id,) = router.pay(dtA, dtB, bob, 3_000 * M, "UETR-4");
        vm.prank(aOperator);
        vm.expectRevert(abi.encodeWithSelector(PaymentRouter.NotReceivingOperator.selector, id, aOperator));
        router.accept(id);
    }

    function test_UnansweredPaymentExpiresAndAnyoneReleasesIt() public {
        vm.prank(alice);
        (bytes32 id,) = router.pay(dtA, dtB, bob, 3_000 * M, "UETR-5");
        vm.expectRevert();
        router.expire(id); // not yet
        vm.warp(block.timestamp + 31);
        vm.prank(bOperator);
        vm.expectRevert();
        router.accept(id); // too late
        vm.prank(outsider);
        router.expire(id);
        assertEq(dtA.heldBalanceOf(alice), 0);
        assertEq(uint8(router.payment(id).status), uint8(PaymentRouter.Status.Expired));
    }

    function test_AFreezeAfterInitiationStopsSettlement() public {
        vm.prank(alice);
        (bytes32 id,) = router.pay(dtA, dtB, bob, 3_000 * M, "UETR-6");
        vm.prank(aCompliance);
        dtA.setFrozenTokens(alice, 10_000 * M);
        vm.prank(bOperator);
        vm.expectRevert();
        router.accept(id);
        vm.prank(bOperator);
        router.reject(id, "RR04");
        assertEq(dtA.balanceOf(alice), 10_000 * M);
    }

    function test_SwapIsAPaymentToYourself() public {
        bRegistry.authorize(alice, "KYC-A-at-BANK_B");
        vm.prank(bOperator);
        ledger.setRequiresAcceptance(BANK_B, false);
        vm.prank(alice);
        router.pay(dtA, dtB, alice, 1_000 * M, "SWAP-1");
        assertEq(dtA.balanceOf(alice), 9_000 * M);
        assertEq(dtB.balanceOf(alice), 1_000 * M);
        assertTrue(ledger.invariantsHold());
    }

    function test_PayeeMustBeAdmittedByTheReceivingBank() public {
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(PaymentRouter.PayeeNotAdmitted.selector, carol));
        router.pay(dtA, dtB, carol, 1 * M, "UETR-7");
    }

    function test_SameTickerDoesNotGoThroughTheRouter() public {
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(PaymentRouter.SameTicker.selector, address(dtA)));
        router.pay(dtA, dtA, carol, 1 * M, "UETR-8");
    }

    /// Audit M-03: the id is derived from the payer, so no one can occupy it.
    function test_NobodyCanSquatAPayersPaymentId() public {
        bytes32 expected = router.paymentIdFor(alice, "UETR-9");
        aRegistry.authorize(outsider, "KYC-O");
        _mintA(outsider, 10 * M, "CORE-BANK_A-O");
        vm.prank(outsider);
        (bytes32 theirs,) = router.pay(dtA, dtB, bob, 1 * M, "UETR-9");
        vm.prank(alice);
        (bytes32 mine,) = router.pay(dtA, dtB, bob, 1 * M, "UETR-9");
        assertTrue(theirs != mine);
        assertEq(mine, expected);
    }

    function test_ReplayingAReferenceIsRefused() public {
        vm.prank(alice);
        (bytes32 id,) = router.pay(dtA, dtB, bob, 1 * M, "UETR-10");
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(PaymentRouter.DuplicatePayment.selector, id));
        router.pay(dtA, dtB, bob, 1 * M, "UETR-10");
    }

    function test_RouterLegsAreClosedToEveryoneElse() public {
        vm.startPrank(outsider);
        vm.expectRevert(abi.encodeWithSelector(BankToken.OnlyRouter.selector, outsider));
        dtA.routerMint(outsider, 1);
        vm.expectRevert(abi.encodeWithSelector(BankToken.OnlyRouter.selector, outsider));
        dtA.routerBurn(alice, 1);
        vm.expectRevert();
        ledger.moveBacking(BANK_A, BANK_B, 1);
        vm.stopPrank();
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Returns
    //////////////////////////////////////////////////////////////////////////*/

    function test_PayeeReturnsFundsUpToTheOriginal() public {
        vm.prank(alice);
        (bytes32 id,) = router.pay(dtA, dtB, bob, 3_000 * M, "UETR-11");
        vm.prank(bOperator);
        router.accept(id);

        vm.prank(alice);
        router.requestReturn(id, "FRAD");

        vm.prank(bob);
        router.returnPayment(id, 2_000 * M, "FRAD", "RTN-1");
        assertEq(dtA.balanceOf(alice), 9_000 * M);
        assertEq(dtB.balanceOf(bob), 1_000 * M);

        vm.prank(bob);
        vm.expectRevert(
            abi.encodeWithSelector(PaymentRouter.ReturnExceedsPayment.selector, id, 1_001 * M, 1_000 * M)
        );
        router.returnPayment(id, 1_001 * M, "FRAD", "RTN-2");
        assertTrue(ledger.invariantsHold());
    }

    function test_OnlyThePayeeReturns() public {
        vm.prank(alice);
        (bytes32 id,) = router.pay(dtA, dtB, bob, 3_000 * M, "UETR-12");
        vm.prank(bOperator);
        router.accept(id);
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(PaymentRouter.NotThePayee.selector, id, alice));
        router.returnPayment(id, 1, "DUPL", "RTN-3");
    }

    /*//////////////////////////////////////////////////////////////////////////
                            Fedwire funding and defunding
    //////////////////////////////////////////////////////////////////////////*/

    function test_AFedwireCreditIsPostedOnce() public {
        vm.prank(funding);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.FedRefAlreadyUsed.selector, bytes32("FW-A-1")));
        ledger.creditFunding(BANK_A, 1 * M, "FW-A-1");
    }

    function test_OnlyTheFundingServicePostsCredits() public {
        vm.prank(aOperator);
        vm.expectRevert();
        ledger.creditFunding(BANK_A, 1 * M, "FW-FAKE");
    }

    function test_DefundLifecycle() public {
        vm.prank(aOperator);
        bytes32 d = ledger.requestDefund(BANK_A, 500_000 * M, "DEF-1");
        assertEq(ledger.freePosition(BANK_A), 490_000 * M, "a requested defund is not free");

        vm.prank(funding); // nothing goes to Fedwire before the approver signs
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.DefundNotPending.selector, d));
        ledger.confirmDefund(d, "FW-OUT-1");

        vm.prank(aApprover);
        ledger.approveDefund(d);
        vm.prank(funding);
        ledger.confirmDefund(d, "FW-OUT-1");
        assertEq(_position(BANK_A), 500_000 * M);
        assertEq(ledger.omnibusTotal(), 1_500_000 * M);
        assertTrue(ledger.invariantsHold());
    }

    function test_FailedDefundReturnsToFreePosition() public {
        vm.prank(aOperator);
        bytes32 d = ledger.requestDefund(BANK_A, 500_000 * M, "DEF-2");
        vm.prank(aApprover);
        ledger.approveDefund(d);
        vm.prank(funding);
        ledger.failDefund(d, "FEDWIRE-CLOSED");
        assertEq(ledger.freePosition(BANK_A), 990_000 * M);
    }

    function test_TheOperatorCannotApproveItsOwnDefund() public {
        vm.prank(aOperator);
        bytes32 d = ledger.requestDefund(BANK_A, 1_000 * M, "DEF-MC");
        vm.prank(aOperator);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.NotTheApprover.selector, BANK_A, aOperator));
        ledger.approveDefund(d);
    }

    function test_AnUnapprovedDefundCanBeCancelled() public {
        vm.prank(aOperator);
        bytes32 d = ledger.requestDefund(BANK_A, 1_000 * M, "DEF-CX");
        vm.prank(aApprover);
        ledger.cancelDefund(d);
        assertEq(ledger.freePosition(BANK_A), 990_000 * M);
        vm.prank(aApprover);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.DefundNotRequested.selector, d));
        ledger.approveDefund(d);
    }

    function test_OperatorAndApproverMustBeDifferentKeys() public {
        vm.prank(governor);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.ApproverMustDiffer.selector, aOperator));
        ledger.setApprover(BANK_A, aOperator);
    }

    function test_DefundsAreWholeCentsBecauseFedwireIs() public {
        vm.prank(aOperator);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.NotWholeCents.selector, 1_000_001));
        ledger.requestDefund(BANK_A, 1_000_001, "DEF-C");
    }

    function test_BackingCanNeverBeDefunded() public {
        vm.prank(aOperator);
        vm.expectRevert(
            abi.encodeWithSelector(
                OmnibusLedger.InsufficientFreePosition.selector, BANK_A, 1_000_000 * M, 990_000 * M
            )
        );
        ledger.requestDefund(BANK_A, 1_000_000 * M, "DEF-3");
    }

    function test_OnlyTheMembersOperatorDefunds() public {
        vm.prank(bOperator);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.NotTheOperator.selector, BANK_A, bOperator));
        ledger.requestDefund(BANK_A, 1, "DEF-4");
    }

    /*//////////////////////////////////////////////////////////////////////////
                        Suspension: holders exit at par
    //////////////////////////////////////////////////////////////////////////*/

    function test_ASuspendedIssuersHoldersStillExitAtPar() public {
        vm.prank(bOperator);
        ledger.setRequiresAcceptance(BANK_B, false);
        vm.prank(governor);
        ledger.suspend(BANK_A);

        vm.prank(aIssuer);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.MemberSuspendedError.selector, BANK_A));
        dtA.mint(alice, 1, "CORE-A-9");

        vm.prank(aOperator);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.MemberSuspendedError.selector, BANK_A));
        ledger.requestDefund(BANK_A, 1, "DEF-5");

        vm.prank(alice); // the whole balance leaves, backed by reserves at the Fed
        router.pay(dtA, dtB, bob, 10_000 * M, "EXIT-1");
        assertEq(dtB.balanceOf(bob), 10_000 * M);
        assertEq(_backing(BANK_A), 0);
        assertTrue(ledger.invariantsHold());
    }

    function test_ASuspendedMemberCannotReceive() public {
        vm.prank(governor);
        ledger.suspend(BANK_B);
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(PaymentRouter.ReceiverSuspended.selector, BANK_B));
        router.pay(dtA, dtB, bob, 1 * M, "UETR-13");
    }

    /*//////////////////////////////////////////////////////////////////////////
                                Reconciliation
    //////////////////////////////////////////////////////////////////////////*/

    function test_AStatementMismatchHaltsMintingAndDefundingButNotPayments() public {
        vm.prank(reconciler);
        ledger.attestFedBalance(1_999_999 * M, "CAMT053-0925");
        assertTrue(ledger.reconciliationBreak());
        assertEq(ledger.breakAmount(), -int256(1 * M));

        vm.prank(aIssuer);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.ReconciliationHalt.selector, -int256(1 * M)));
        dtA.mint(alice, 1, "CORE-A-10");
        vm.prank(aOperator);
        vm.expectRevert();
        ledger.requestDefund(BANK_A, 1, "DEF-6");

        vm.prank(alice); // tokens already backed keep moving
        dtA.transfer(carol, 1 * M);

        vm.prank(reconciler);
        ledger.attestFedBalance(2_000_000 * M, "CAMT053-0925b");
        assertFalse(ledger.reconciliationBreak(), "matching statement clears the halt");
    }

    function test_TheFundingServiceCannotAttest() public {
        vm.prank(funding);
        vm.expectRevert();
        ledger.attestFedBalance(2_000_000 * M, "SELF");
    }

    /*//////////////////////////////////////////////////////////////////////////
                    Interest: time-weighted (audit H-03 fixed)
    //////////////////////////////////////////////////////////////////////////*/

    function test_InterestFollowsTimeWeightedPositionNotTheSnapshot() public {
        // Both members were funded with 1m at t0. Thirty days pass, then BANK_B
        // tops up by 9m one second before the Fed's interest arrives.
        vm.warp(t0 + 30 days - 1);
        _fund(BANK_B, 9_000_000 * M, "FW-BANK_B-TOPUP");
        vm.warp(t0 + 30 days);

        vm.prank(funding);
        ledger.distributeInterest(3_288 * M, "FW-IORB-SEP");

        uint256 aGot = _position(BANK_A) - 1_000_000 * M;
        uint256 bGot = _position(BANK_B) - 10_000_000 * M;
        assertApproxEqAbs(aGot, 1_644 * M, 10 * M, "equal time-weighted positions, equal shares");
        assertApproxEqAbs(bGot, 1_644 * M, 10 * M, "the 9m held for one second earned about nothing");
        assertTrue(ledger.invariantsHold());
    }

    /*//////////////////////////////////////////////////////////////////////////
                    Compliance (audit H-02, M-01, M-13, L-06 fixed)
    //////////////////////////////////////////////////////////////////////////*/

    function test_ComplianceCannotMintThroughTheZeroAddress() public {
        vm.startPrank(aCompliance);
        vm.expectRevert(BankToken.ZeroAddress.selector);
        dtA.setFrozenTokens(address(0), 1_000_000 * M);
        vm.expectRevert(BankToken.ZeroAddress.selector);
        dtA.forcedTransfer(address(0), carol, 1 * M);
        vm.stopPrank();
    }

    function test_ForcedTransferCannotTakeHeldFundsAndEmitsFrozen() public {
        vm.prank(alice);
        router.pay(dtA, dtB, bob, 9_000 * M, "UETR-14"); // 9,000 held
        vm.startPrank(aCompliance);
        dtA.setFrozenTokens(alice, 1_000 * M);
        vm.expectRevert();
        dtA.forcedTransfer(alice, carol, 1_001 * M); // only 1,000 is not held

        vm.recordLogs();
        dtA.forcedTransfer(alice, carol, 1_000 * M);
        vm.stopPrank();
        Vm.Log[] memory logs = vm.getRecordedLogs();
        bool sawFrozen;
        for (uint256 i = 0; i < logs.length; i++) {
            if (logs[i].topics[0] == keccak256("Frozen(address,uint256)")) sawFrozen = true;
        }
        assertTrue(sawFrozen, "frozen amount changed, Frozen emitted");
        assertEq(dtA.getFrozenTokens(alice), 0);
    }

    function test_RecoveryBlocksTheLostKeyForGood() public {
        address alice2 = makeAddr("alice-new-key");
        aRegistry.authorize(alice2, "KYC-A2");
        vm.prank(aCompliance);
        dtA.recover(alice, alice2);
        assertEq(dtA.balanceOf(alice2), 10_000 * M);
        assertTrue(dtA.blocked(alice));

        vm.prank(carol);
        vm.expectRevert(); // inflows to the lost key fail
        dtA.transfer(alice, 0);
        vm.prank(aIssuer);
        vm.expectRevert();
        dtA.mint(alice, 1, "CORE-A-11");
    }

    function test_RecoveryOntoItselfIsRefused() public {
        vm.prank(aCompliance);
        vm.expectRevert(BankToken.SelfTransfer.selector);
        dtA.recover(alice, alice);
    }

    function test_ERC7943IsReportedThroughERC165() public view {
        assertEq(type(IERC7943FungibleToken).interfaceId, bytes4(0x3edbb4c4));
        assertTrue(dtA.supportsInterface(0x3edbb4c4));
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Admin
    //////////////////////////////////////////////////////////////////////////*/

    function test_PauseStopsPaymentsButNeverTrapsAHold() public {
        vm.prank(alice);
        (bytes32 id,) = router.pay(dtA, dtB, bob, 1 * M, "UETR-15");
        router.pause();
        vm.prank(alice);
        vm.expectRevert();
        router.pay(dtA, dtB, bob, 1 * M, "UETR-16");
        vm.warp(block.timestamp + 31);
        router.expire(id);
        assertEq(dtA.heldBalanceOf(alice), 0);
    }

    function test_AdmissionRequiresSixDecimalsAndAFreshToken() public {
        vm.startPrank(governor);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.TokenAlreadyRegistered.selector, address(dtA)));
        ledger.admitMember("OTHER", address(dtA), aOperator, aApprover, false);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.UnknownMember.selector, bytes32(0)));
        ledger.admitMember(bytes32(0), makeAddr("tok"), aOperator, aApprover, false);
        vm.stopPrank();
    }

}
