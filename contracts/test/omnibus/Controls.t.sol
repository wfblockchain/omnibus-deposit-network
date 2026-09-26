// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { OmnibusBase } from "./OmnibusBase.t.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { PaymentRouter } from "src/omnibus/PaymentRouter.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";
import { IAccessControl } from "@openzeppelin/contracts/access/IAccessControl.sol";
import { IAccessControlDefaultAdminRules } from
    "@openzeppelin/contracts/access/extensions/IAccessControlDefaultAdminRules.sol";

/// @dev The emergency and governance controls on the home chain: the issuing
///      bank's pause, the ledger's guardian, and slow admin handover.
contract ControlsTest is OmnibusBase {

    address aPauser = makeAddr("bank-a-pauser");
    address guardian = makeAddr("operator-guardian");

    function setUp() public override {
        super.setUp();
        dtA.grantRole(dtA.PAUSER_ROLE(), aPauser);
        ledger.grantRole(ledger.GUARDIAN_ROLE(), guardian);
        _fund(BANK_A, 1_000_000 * M, "FW-BANK_A");
        _fund(BANK_B, 1_000_000 * M, "FW-BANK_B");
        _mintA(alice, 10_000 * M, "CORE-A");
    }

    /*//////////////////////////////////////////////////////////////////////////
                                The issuer's pause
    //////////////////////////////////////////////////////////////////////////*/

    function test_APausedTokenStopsTransfersMintsRedemptionsAndNewHolds() public {
        vm.prank(aPauser);
        dtA.pause();

        vm.prank(alice);
        vm.expectRevert(Pausable.EnforcedPause.selector);
        dtA.transfer(carol, 1 * M);

        vm.prank(aIssuer);
        vm.expectRevert(Pausable.EnforcedPause.selector);
        dtA.mint(alice, 1 * M, "CORE-B");

        vm.prank(alice);
        vm.expectRevert(Pausable.EnforcedPause.selector);
        dtA.redeem(1 * M, "RED-1");

        vm.prank(alice);
        vm.expectRevert(Pausable.EnforcedPause.selector);
        router.pay(dtA, dtB, bob, 1 * M, "UETR-P1");

        assertFalse(dtA.canTransfer(alice, carol, 1 * M), "ERC-7943 view agrees");
        assertEq(dtA.balanceOf(alice), 10_000 * M, "nothing moved");
    }

    /// A payment already held when the bank pauses can still be released,
    /// so a pause never traps a payer's funds behind a hold.
    function test_APauseNeverTrapsAHeldPayment() public {
        vm.prank(alice);
        (bytes32 id,) = router.pay(dtA, dtB, bob, 3_000 * M, "UETR-P2");
        assertEq(dtA.heldBalanceOf(alice), 3_000 * M);

        vm.prank(aPauser);
        dtA.pause();
        vm.prank(bOperator);
        vm.expectRevert(Pausable.EnforcedPause.selector);
        router.accept(id); // settling burns the held tokens: stopped

        vm.prank(bOperator);
        router.reject(id, "AC04"); // releasing the hold moves nothing: allowed
        assertEq(dtA.heldBalanceOf(alice), 0);
    }

    /// Pause is also the kill switch for a compromised compliance key, so
    /// forced transfers and recovery stop too; freezing, which only
    /// restricts, still works.
    function test_APauseStopsForcedTransfersButNotFreezes() public {
        vm.prank(aPauser);
        dtA.pause();
        vm.prank(aCompliance);
        vm.expectRevert(Pausable.EnforcedPause.selector);
        dtA.forcedTransfer(alice, carol, 2_000 * M);
        vm.prank(aCompliance);
        vm.expectRevert(Pausable.EnforcedPause.selector);
        dtA.recover(alice, carol);
        vm.prank(aCompliance);
        dtA.setFrozenTokens(alice, 5_000 * M);
        assertEq(dtA.getFrozenTokens(alice), 5_000 * M);
    }

    function test_OnlyThePauserPausesAndResumes() public {
        bytes32 pauserRole = dtA.PAUSER_ROLE(); // read first: a call would eat the prank
        vm.prank(outsider);
        vm.expectRevert(
            abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, outsider, pauserRole)
        );
        dtA.pause();
        vm.prank(aPauser);
        dtA.pause();
        vm.prank(aPauser);
        dtA.unpause();
        vm.prank(alice);
        dtA.transfer(carol, 1 * M);
        assertEq(dtA.balanceOf(carol), 1 * M);
    }

    /*//////////////////////////////////////////////////////////////////////////
                    Restrictive fast, permissive slow
    //////////////////////////////////////////////////////////////////////////*/

    function test_TheGuardianSuspendsAtOnceButOnlyTheGovernorReinstates() public {
        bytes32 governorRole = ledger.GOVERNOR_ROLE();
        bytes32 guardianRole = ledger.GUARDIAN_ROLE();
        vm.prank(guardian);
        ledger.suspend(BANK_B);
        assertTrue(ledger.member(BANK_B).suspended);

        vm.prank(guardian);
        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector, guardian, governorRole
            )
        );
        ledger.reinstate(BANK_B);

        vm.prank(governor);
        ledger.reinstate(BANK_B);
        assertFalse(ledger.member(BANK_B).suspended);

        vm.prank(outsider);
        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector, outsider, guardianRole
            )
        );
        ledger.suspend(BANK_B);
    }

    /// Handing over the admin of every home contract is two-step and waits
    /// out the delay: a stolen admin key cannot seize a contract at once.
    function test_AdminHandoverIsTwoStepAndDelayed() public {
        OmnibusLedger l = new OmnibusLedger(address(this), 2 days);
        address next = makeAddr("next-admin");
        l.beginDefaultAdminTransfer(next);

        vm.prank(next);
        vm.expectRevert();
        l.acceptDefaultAdminTransfer();

        vm.warp(block.timestamp + 2 days + 1);
        vm.prank(next);
        l.acceptDefaultAdminTransfer();
        assertEq(l.defaultAdmin(), next);

        bytes32 adminRole = l.DEFAULT_ADMIN_ROLE();
        vm.prank(next);
        vm.expectRevert(
            abi.encodeWithSelector(IAccessControlDefaultAdminRules.AccessControlEnforcedDefaultAdminRules.selector)
        );
        l.grantRole(adminRole, address(this)); // the admin role is never granted directly
    }

}
