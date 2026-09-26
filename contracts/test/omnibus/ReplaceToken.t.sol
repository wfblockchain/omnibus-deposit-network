// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { OmnibusBase } from "./OmnibusBase.t.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { PaymentRouter } from "src/omnibus/PaymentRouter.sol";
import { IAccessControl } from "@openzeppelin/contracts/access/IAccessControl.sol";

/// @dev Replacing a flawed ticker without moving a cent of backing: the old
///      supply is re-created on a fixed successor from the old contract's own
///      state, the ledger swaps the member's ticker only when every unit is
///      there, and the old ticker is retired for good.
contract ReplaceTokenTest is OmnibusBase {

    address aPauser = makeAddr("bank-a-pauser");
    address fresh = makeAddr("alice-new-key");
    BankToken next;

    function setUp() public override {
        super.setUp();
        dtA.grantRole(dtA.PAUSER_ROLE(), aPauser);
        _fund(BANK_A, 1_000_000 * M, "FW-BANK_A");
        _fund(BANK_B, 1_000_000 * M, "FW-BANK_B");
        _mintA(alice, 10_000 * M, "CORE-A");
        _mintA(carol, 4_000 * M, "CORE-C");
        vm.prank(aCompliance);
        dtA.setFrozenTokens(carol, 1_500 * M);

        // A lost key recovered before the flaw was found: its balance moved,
        // the lost key is blocked.
        aRegistry.authorize(fresh, "KYC-A2");
        address lost = makeAddr("dave-lost-key");
        aRegistry.authorize(lost, "KYC-D");
        _mintA(lost, 500 * M, "CORE-D");
        vm.prank(aCompliance);
        dtA.recover(lost, fresh);

        next = new BankToken("Bank A USD", "A-dT", ledger, address(router), aRegistry, address(this), 0);
        next.grantRole(next.PAUSER_ROLE(), aPauser);
        next.grantRole(next.ISSUER_ROLE(), aIssuer);
        vm.prank(aPauser);
        next.pause();
    }

    function _holders() internal returns (address[] memory h) {
        h = new address[](4);
        h[0] = alice;
        h[1] = carol;
        h[2] = fresh;
        h[3] = makeAddr("dave-lost-key");
    }

    function _begin() internal {
        vm.prank(aPauser);
        dtA.pause();
        vm.prank(governor);
        ledger.beginTokenReplacement(BANK_A, address(next));
    }

    function test_ATickerIsReplacedWithEveryBalanceFreezeAndBlockIntact() public {
        uint256 backing = _backing(BANK_A);
        _begin();
        next.migrateBalances(_holders());
        assertTrue(ledger.invariantsHold(), "the old ticker still carries the backing mid-migration");
        ledger.completeTokenReplacement(BANK_A);

        assertEq(ledger.member(BANK_A).token, address(next));
        assertEq(ledger.memberOfToken(address(next)), BANK_A);
        assertEq(ledger.memberOfToken(address(dtA)), bytes32(0));
        assertEq(next.balanceOf(alice), 10_000 * M);
        assertEq(next.balanceOf(carol), 4_000 * M);
        assertEq(next.balanceOf(fresh), 500 * M);
        assertEq(next.getFrozenTokens(carol), 1_500 * M, "freezes carried over");
        assertTrue(next.blocked(makeAddr("dave-lost-key")), "a blocked key stays blocked");
        assertEq(next.totalSupply(), 14_500 * M);
        assertEq(_backing(BANK_A), backing, "backing never moved");
        assertTrue(ledger.invariantsHold());
        assertTrue(dtA.retired());
    }

    /// After the swap the successor is the member's ticker in every respect.
    function test_TheSuccessorMintsPaysAndRedeems() public {
        _begin();
        next.migrateBalances(_holders());
        ledger.completeTokenReplacement(BANK_A);
        vm.prank(aPauser);
        next.unpause();

        vm.prank(aIssuer);
        next.mint(alice, 1_000 * M, "CORE-A2");
        vm.prank(bOperator);
        ledger.setRequiresAcceptance(BANK_B, false);
        vm.prank(alice);
        router.pay(next, dtB, bob, 2_000 * M, "UETR-R1");
        assertEq(dtB.balanceOf(bob), 2_000 * M, "converts at par to another ticker");
        vm.prank(carol);
        next.redeem(2_500 * M, "RED-C");
        assertTrue(ledger.invariantsHold());
    }

    /// Even if its bank unpauses it, a retired ticker can never move again,
    /// so no worthless copy of a deposit can circulate.
    function test_ARetiredTickerNeverMovesAgain() public {
        _begin();
        next.migrateBalances(_holders());
        ledger.completeTokenReplacement(BANK_A);
        vm.prank(aPauser);
        dtA.unpause();

        vm.prank(alice);
        vm.expectRevert(BankToken.TokenRetired.selector);
        dtA.transfer(carol, 1 * M);
        vm.prank(aCompliance);
        vm.expectRevert(BankToken.TokenRetired.selector);
        dtA.forcedTransfer(alice, carol, 1 * M);
        vm.prank(aIssuer);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.NotARegisteredToken.selector, address(dtA)));
        dtA.mint(alice, 1 * M, "CORE-X");
        assertFalse(dtA.canTransfer(alice, carol, 1 * M));
    }

    function test_CompletionWaitsForEveryUnitOfTheOldSupply() public {
        _begin();
        address[] memory some = new address[](1);
        some[0] = alice;
        next.migrateBalances(some);
        vm.expectRevert(
            abi.encodeWithSelector(OmnibusLedger.MigrationIncomplete.selector, BANK_A, 10_000 * M, 14_500 * M)
        );
        ledger.completeTokenReplacement(BANK_A);
    }

    function test_ABalanceIsMigratedOnce() public {
        _begin();
        next.migrateBalances(_holders());
        next.migrateBalances(_holders());
        assertEq(next.balanceOf(alice), 10_000 * M);
        (,, uint256 migratedTotal) = ledger.replacements(BANK_A);
        assertEq(migratedTotal, 14_500 * M);
    }

    function test_ReplacementNeedsBothTickersPausedAndNoOpenHolds() public {
        vm.prank(governor);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.TokenNotPaused.selector, address(dtA)));
        ledger.beginTokenReplacement(BANK_A, address(next));

        vm.prank(alice);
        router.pay(dtA, dtB, bob, 3_000 * M, "UETR-H1"); // BANK_B requires acceptance: a hold
        vm.prank(aPauser);
        dtA.pause();
        vm.prank(governor);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.HoldsOutstanding.selector, address(dtA), 3_000 * M));
        ledger.beginTokenReplacement(BANK_A, address(next));

        vm.prank(aPauser);
        next.unpause();
        vm.prank(governor);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.TokenNotPaused.selector, address(next)));
        ledger.beginTokenReplacement(BANK_A, address(next));
    }

    function test_OnlyTheGovernorBegins() public {
        vm.prank(aPauser);
        dtA.pause();
        bytes32 role = ledger.GOVERNOR_ROLE();
        vm.prank(outsider);
        vm.expectRevert(abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, outsider, role));
        ledger.beginTokenReplacement(BANK_A, address(next));
    }

    function test_AnyoneElsesContractCannotPoseAsTheSuccessor() public {
        _begin();
        vm.prank(outsider);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.NotASuccessor.selector, outsider));
        ledger.recordMigration(1);
    }

    function test_ACancelledReplacementRetiresTheSuccessor() public {
        _begin();
        next.migrateBalances(_holders());
        vm.prank(governor);
        ledger.cancelTokenReplacement(BANK_A);
        assertTrue(next.retired());
        assertEq(ledger.member(BANK_A).token, address(dtA), "the member keeps its ticker");
        vm.prank(aPauser);
        dtA.unpause();
        vm.prank(alice);
        dtA.transfer(carol, 1 * M);
        assertTrue(ledger.invariantsHold());
    }

    /// Any amounts, any batching, duplicates included: the successor ends
    /// with exactly the old balances, and completion is refused until then.
    function testFuzz_AnyBatchingReachesExactlyTheOldSupply(uint64[4] memory amounts, uint8 split, bool dup) public {
        address[] memory h = new address[](4);
        for (uint256 i = 0; i < 4; i++) {
            h[i] = makeAddr(string.concat("holder-", vm.toString(i)));
            aRegistry.authorize(h[i], "KYC");
            uint256 a = bound(amounts[i], 0, 50_000) * M;
            if (a > 0) _mintA(h[i], a, keccak256(abi.encode("F", i)));
        }
        _begin();
        uint256 cut = split % 5;
        address[] memory first = new address[](cut);
        address[] memory rest = new address[](4 - cut + (dup ? 1 : 0));
        for (uint256 i = 0; i < cut; i++) first[i] = h[i];
        for (uint256 i = cut; i < 4; i++) rest[i - cut] = h[i];
        if (dup) rest[rest.length - 1] = h[0]; // a holder listed twice
        next.migrateBalances(first);
        address[] memory others = _holders();
        next.migrateBalances(others);
        if (cut < 4) {
            uint256 missing;
            for (uint256 i = cut; i < 4; i++) missing += dtA.balanceOf(h[i]);
            if (missing > 0) {
                vm.expectRevert();
                ledger.completeTokenReplacement(BANK_A);
            }
        }
        next.migrateBalances(rest);
        ledger.completeTokenReplacement(BANK_A);
        for (uint256 i = 0; i < 4; i++) {
            assertEq(next.balanceOf(h[i]), dtA.balanceOf(h[i]));
        }
        assertEq(next.totalSupply(), dtA.totalSupply());
        assertTrue(ledger.invariantsHold());
    }

}
