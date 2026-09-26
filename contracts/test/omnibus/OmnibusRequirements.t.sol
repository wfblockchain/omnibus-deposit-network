// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { OmnibusBase } from "./OmnibusBase.t.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { OmnibusNetting } from "src/omnibus/OmnibusNetting.sol";
import { BankToken } from "src/omnibus/BankToken.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { PaymentRouter } from "src/omnibus/PaymentRouter.sol";
import { IERC7943FungibleToken } from "src/omnibus/IERC7943.sol";

/// @dev Interbank holding, gross and netted settlement, funding controls.
///      Three members: Bank A, Bank B and Bank C.
contract OmnibusRequirementsTest is OmnibusBase {

    bytes32 constant BANK_C = "BNKCUS30";

    OmnibusNetting netting;
    HolderRegistry cRegistry;
    BankToken dtC;

    address cOperator = makeAddr("bank-c-gateway");
    address cApprover = makeAddr("bank-c-treasury-approver");
    address cIssuer = makeAddr("bank-c-issuer");
    address settlementOp = makeAddr("operator-settlement-operator");

    address aTreasury = makeAddr("bank-a-treasury");
    address bTreasury = makeAddr("bank-b-treasury");
    address cTreasury = makeAddr("bank-c-treasury");

    function setUp() public override {
        super.setUp();
        (cRegistry, dtC) = _bank("Bank C USD", "C-dT", cIssuer, makeAddr("bank-c-compliance"));
        netting = new OmnibusNetting(ledger, address(this), 0);
        ledger.grantRole(ledger.NETTING_ROLE(), address(netting));
        netting.grantRole(netting.OPERATOR_ROLE(), settlementOp);

        vm.startPrank(governor);
        ledger.admitMember(BANK_C, address(dtC), cOperator, cApprover, false);
        ledger.registerWallet(BANK_A, aTreasury);
        ledger.registerWallet(BANK_B, bTreasury);
        ledger.registerWallet(BANK_C, cTreasury);
        vm.stopPrank();

        vm.prank(bOperator);
        ledger.setRequiresAcceptance(BANK_B, false);

        _fund(BANK_A, 100 * M, "FW-BANK_A");
        _fund(BANK_B, 100 * M, "FW-BANK_B");
        _fund(BANK_C, 100 * M, "FW-BANK_C");
        _mintA(alice, 50 * M, "CORE-A-1");
    }

    /*//////////////////////////////////////////////////////////////////////////
            Member banks hold and move each other's tokenized deposits
    //////////////////////////////////////////////////////////////////////////*/

    function test_AMemberWalletHoldsAnyIssuersTicker() public {
        vm.prank(alice); // Alice pays Bank B itself, in dtA
        dtA.transfer(bTreasury, 20 * M);
        assertEq(dtA.balanceOf(bTreasury), 20 * M);
        assertTrue(dtA.canReceive(cTreasury), "no per-issuer admission needed for member wallets");

        vm.prank(bTreasury); // and Bank B passes it on to Bank C
        dtA.transfer(cTreasury, 5 * M);
        assertEq(dtA.balanceOf(cTreasury), 5 * M);
        assertTrue(ledger.invariantsHold());
    }

    function test_AnUnlistedWalletStillCannotHoldAnotherIssuersTicker() public {
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(IERC7943FungibleToken.ERC7943CannotReceive.selector, outsider));
        dtA.transfer(outsider, 1);
    }

    function test_OnlyGovernanceListsMemberWallets() public {
        vm.prank(bOperator);
        vm.expectRevert();
        ledger.registerWallet(BANK_B, outsider);
    }

    /*//////////////////////////////////////////////////////////////////////////
                Gross interbank settlement of held tokens
    //////////////////////////////////////////////////////////////////////////*/

    function test_AHolderBankSettlesAnotherBanksTokensIntoItsPosition() public {
        vm.prank(alice);
        dtA.transfer(bTreasury, 30 * M);
        uint256 bFree = ledger.freePosition(BANK_B);

        vm.prank(bTreasury);
        router.settleHeld(dtA, 30 * M);

        assertEq(dtA.balanceOf(bTreasury), 0);
        assertEq(_backing(BANK_A), 20 * M, "BANK_A's backing under the settled tokens left");
        assertEq(_position(BANK_A), 70 * M);
        assertEq(ledger.freePosition(BANK_B), bFree + 30 * M, "Bank B gained free position");
        assertEq(ledger.omnibusTotal(), 300 * M, "the Fed balance does not move");
        assertTrue(ledger.invariantsHold());

        _mintB(bob, 30 * M, "CORE-B-1"); // and can issue its own ticker against it
        assertEq(dtB.balanceOf(bob), 30 * M);
    }

    function test_OnlyMemberWalletsSettleAndNeverTheirOwnTicker() public {
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(PaymentRouter.NotAMemberWallet.selector, alice));
        router.settleHeld(dtA, 1);

        vm.prank(alice);
        dtA.transfer(aTreasury, 1 * M);
        vm.prank(aTreasury);
        vm.expectRevert(abi.encodeWithSelector(PaymentRouter.OwnTicker.selector, address(dtA)));
        router.settleHeld(dtA, 1 * M);
    }

    /*//////////////////////////////////////////////////////////////////////////
                    Gross versus multilateral netting
    //////////////////////////////////////////////////////////////////////////*/

    function _submit(address op, bytes32 payer, bytes32 payee, uint128 amount, bytes32 ref) internal returns (bytes32) {
        vm.prank(op);
        return netting.submit(payer, payee, amount, ref);
    }

    /// Circular obligations of $180, $170 and $160 between banks with $50 to
    /// $100 of free position: gross cannot fund the first; one cycle settles
    /// all $510 by moving $20.
    function test_NettingSettlesWhatGrossCannotFund() public {
        bytes32 a = _submit(aOperator, BANK_A, BANK_B, uint128(180 * M), "O-A-1");
        bytes32 b = _submit(bOperator, BANK_B, BANK_C, uint128(170 * M), "O-B-1");
        bytes32 c = _submit(cOperator, BANK_C, BANK_A, uint128(160 * M), "O-C-1");

        vm.prank(aOperator);
        vm.expectRevert(
            abi.encodeWithSelector(OmnibusLedger.InsufficientFreePosition.selector, BANK_A, 180 * M, 50 * M)
        );
        netting.settleGross(a);

        (bytes32[] memory members, int256[] memory nets) = _nets3(-20, 10, 10); // BANK_A -180+160, BANK_B +180-170, BANK_C +170-160
        bytes32[] memory ids = _sorted3(a, b, c);
        vm.prank(settlementOp);
        netting.settleCycle("CYCLE-1", members, nets, ids);

        assertEq(_position(BANK_A), 80 * M);
        assertEq(_position(BANK_B), 110 * M);
        assertEq(_position(BANK_C), 110 * M);
        assertEq(netting.grossDischarged(), 510 * M);
        assertEq(netting.netMoved(), 20 * M);
        assertEq(netting.liquidityEfficiencyBps(), 255_000, "25.5 : 1");
        assertTrue(ledger.invariantsHold());
    }

    function test_GrossSettlesOneObligationNowWhenFunded() public {
        bytes32 a = _submit(aOperator, BANK_A, BANK_C, uint128(10 * M), "O-A-2");
        vm.prank(aOperator);
        netting.settleGross(a);
        assertEq(_position(BANK_C), 110 * M);
        (,,,, OmnibusNetting.Status st) = netting.obligations(a);
        assertEq(uint8(st), uint8(OmnibusNetting.Status.Settled));
    }

    function test_TheChainRecomputesEveryNet() public {
        bytes32 a = _submit(aOperator, BANK_A, BANK_B, uint128(30 * M), "O-A-3");
        (bytes32[] memory members, int256[] memory nets) = _nets3(-20, 20, 0); // lies: should be -30/+30
        bytes32[] memory ids = new bytes32[](1);
        ids[0] = a;
        vm.prank(settlementOp);
        // members are checked in id order, and BNKAUS30 (Bank A) sorts first
        vm.expectRevert(abi.encodeWithSelector(OmnibusNetting.NetMismatch.selector, BANK_A, -20 * int256(M), -30 * int256(M)));
        netting.settleCycle("CYCLE-2", members, nets, ids);
    }

    function test_ACycleCannotCreateCredit() public {
        bytes32 a = _submit(aOperator, BANK_A, BANK_B, uint128(60 * M), "O-A-4"); // BANK_A has 50m free
        (bytes32[] memory members, int256[] memory nets) = _nets3(-60, 60, 0);
        bytes32[] memory ids = new bytes32[](1);
        ids[0] = a;
        vm.prank(settlementOp);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.InsufficientFreePosition.selector, BANK_A, 60 * M, 50 * M));
        netting.settleCycle("CYCLE-3", members, nets, ids);
    }

    function test_OnlyTheOperatorSubmitsPlansAndListsMustBeSorted() public {
        bytes32 a = _submit(aOperator, BANK_A, BANK_B, uint128(1 * M), "O-A-5");
        (bytes32[] memory members, int256[] memory nets) = _nets3(-1, 1, 0);
        bytes32[] memory ids = new bytes32[](1);
        ids[0] = a;
        vm.prank(outsider);
        vm.expectRevert();
        netting.settleCycle("CYCLE-4", members, nets, ids);

        (members[0], members[1]) = (members[1], members[0]);
        (nets[0], nets[1]) = (nets[1], nets[0]);
        vm.prank(settlementOp);
        vm.expectRevert(abi.encodeWithSelector(OmnibusNetting.NotStrictlyIncreasing.selector, 1));
        netting.settleCycle("CYCLE-4", members, nets, ids);
    }

    function test_ObligationsExpire() public {
        bytes32 a = _submit(aOperator, BANK_A, BANK_B, uint128(1 * M), "O-A-6");
        vm.warp(block.timestamp + 1 days + 1);
        vm.prank(aOperator);
        vm.expectRevert();
        netting.settleGross(a);
        netting.expire(a);
        (,,,, OmnibusNetting.Status st) = netting.obligations(a);
        assertEq(uint8(st), uint8(OmnibusNetting.Status.Expired));
    }

    function test_OnlyThePayersOperatorSubmits() public {
        vm.prank(bOperator);
        vm.expectRevert(abi.encodeWithSelector(OmnibusNetting.NotTheOperator.selector, BANK_A, bOperator));
        netting.submit(BANK_A, BANK_B, uint128(1 * M), "O-FAKE");
    }

    function test_BackingIsNeverAvailableToNetting() public {
        // BANK_A has a $100 position, $50 of it backing Alice's tokens: $50 free.
        bytes32 a = _submit(aOperator, BANK_A, BANK_B, uint128(51 * M), "O-A-7");
        vm.prank(aOperator);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.InsufficientFreePosition.selector, BANK_A, 51 * M, 50 * M));
        netting.settleGross(a);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                Funding controls
    //////////////////////////////////////////////////////////////////////////*/

    function test_IssuanceCapLimitsOutstandingTokens() public {
        vm.prank(governor);
        ledger.setLimits(BANK_A, 60 * M, 0);
        assertEq(ledger.mintCapacity(BANK_A), 10 * M);
        vm.prank(aIssuer);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.IssuanceCapExceeded.selector, BANK_A, 61 * M, 60 * M));
        dtA.mint(alice, 11 * M, "CORE-A-2");
    }

    function test_PrefundRequirementKeepsAFloorForSettlement() public {
        vm.prank(governor);
        ledger.setLimits(BANK_A, 0, 40 * M);
        assertEq(ledger.mintCapacity(BANK_A), 10 * M);

        vm.prank(aIssuer);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.BelowPrefundRequirement.selector, BANK_A, 39 * M, 40 * M));
        dtA.mint(alice, 11 * M, "CORE-A-3");

        vm.prank(aOperator);
        vm.expectRevert(abi.encodeWithSelector(OmnibusLedger.BelowPrefundRequirement.selector, BANK_A, 30 * M, 40 * M));
        ledger.requestDefund(BANK_A, 20 * M, "DEF-FLOOR");
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Helpers
    //////////////////////////////////////////////////////////////////////////*/

    function _nets3(int256 a, int256 b, int256 c)
        internal
        pure
        returns (bytes32[] memory members, int256[] memory nets)
    {
        bytes32[3] memory ids = [BANK_A, BANK_B, BANK_C];
        int256[3] memory v = [a * int256(M), b * int256(M), c * int256(M)];
        // sort ids ascending, carrying values
        for (uint256 i = 0; i < 3; i++) {
            for (uint256 j = i + 1; j < 3; j++) {
                if (ids[j] < ids[i]) {
                    (ids[i], ids[j]) = (ids[j], ids[i]);
                    (v[i], v[j]) = (v[j], v[i]);
                }
            }
        }
        members = new bytes32[](3);
        nets = new int256[](3);
        for (uint256 i = 0; i < 3; i++) {
            members[i] = ids[i];
            nets[i] = v[i];
        }
    }

    function _sorted3(bytes32 a, bytes32 b, bytes32 c) internal pure returns (bytes32[] memory ids) {
        ids = new bytes32[](3);
        ids[0] = a;
        ids[1] = b;
        ids[2] = c;
        for (uint256 i = 0; i < 3; i++) {
            for (uint256 j = i + 1; j < 3; j++) {
                if (ids[j] < ids[i]) (ids[i], ids[j]) = (ids[j], ids[i]);
            }
        }
    }

}
