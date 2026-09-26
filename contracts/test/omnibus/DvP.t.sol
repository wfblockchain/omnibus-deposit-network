// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { Vm } from "forge-std/Vm.sol";
import { OmnibusBase } from "./OmnibusBase.t.sol";
import { HolderRegistry } from "src/omnibus/HolderRegistry.sol";
import { OmnibusLedger } from "src/omnibus/OmnibusLedger.sol";
import { IERC7943FungibleToken } from "src/omnibus/IERC7943.sol";
import { CrossChainMessenger } from "src/omnibus/crosschain/CrossChainMessenger.sol";
import { RemoteBankToken } from "src/omnibus/crosschain/RemoteBankToken.sol";
import { DvPSettlement } from "src/omnibus/dvp/DvPSettlement.sol";
import { MockSecurityToken, MockFeeToken, MockReentrantToken } from "test/utils/MockSecurityToken.sol";
import { Pausable } from "@openzeppelin/contracts/utils/Pausable.sol";

/// @dev Option 2: the cash goes to where the asset lives. Home is the operator's
///      omnibus chain (domain 0); domain 7 is a public chain where a
///      tokenized T-bill fund is issued. A buyer banking with Bank A
///      pays the fund's seller in dtA minted on domain 7, while the backing
///      stays in the joint account at the Fed.
abstract contract DvPBase is OmnibusBase {

    uint32 constant HOME = 0;
    uint32 constant REMOTE = 7;
    uint256 constant SHARE = 1e18;

    CrossChainMessenger home;
    CrossChainMessenger remote;
    RemoteBankToken cash; // dtA on domain 7
    HolderRegistry aRemoteRegistry;
    MockSecurityToken tbill;
    DvPSettlement dvp;

    uint256[3] attesterKeys = [uint256(0xA11CE), uint256(0xB0B), uint256(0xCA7)];
    uint256 aIssuerKey = 0x1550E; // Bank A's own attestation key

    uint256 buyerKey = 0xB0B0B0;
    address buyer; // an institutional client of Bank A
    address seller = makeAddr("fund-seller"); // sells fund shares for dollars
    address relayer = makeAddr("relayer");
    address pauser = makeAddr("venue-pauser");
    address aRemoteCompliance = makeAddr("bank-a-compliance-remote");

    function setUp() public virtual override {
        super.setUp();
        buyer = vm.addr(buyerKey);

        home = new CrossChainMessenger(HOME, HOME, ledger, address(this), 0);
        remote = new CrossChainMessenger(REMOTE, HOME, OmnibusLedger(address(0)), address(this), 0);
        aRemoteRegistry = new HolderRegistry(address(this), 0);
        aRemoteRegistry.grantRole(aRemoteRegistry.REGISTRAR_ROLE(), address(this));
        cash = new RemoteBankToken("Bank A USD", "A-dT", address(remote), aRemoteRegistry, address(this), 0);
        cash.grantRole(cash.COMPLIANCE_ROLE(), aRemoteCompliance);

        ledger.grantRole(ledger.MESSENGER_ROLE(), address(home));
        dtA.grantRole(dtA.MESSENGER_ROLE(), address(home));
        for (uint256 i = 0; i < 3; i++) {
            home.setAttester(vm.addr(attesterKeys[i]), true);
            remote.setAttester(vm.addr(attesterKeys[i]), true);
        }
        home.setThreshold(2);
        remote.setThreshold(2);
        home.setRemote(REMOTE, address(remote));
        remote.setRemote(HOME, address(home));
        home.setToken(BANK_A, address(dtA));
        remote.setToken(BANK_A, address(cash));
        home.setCorridorCap(BANK_A, REMOTE, 50_000_000 * M);
        remote.setSupplyCap(BANK_A, 50_000_000 * M);
        home.setIssuerAttester(BANK_A, vm.addr(aIssuerKey));
        remote.setIssuerAttester(BANK_A, vm.addr(aIssuerKey));

        dvp = new DvPSettlement(remote, address(this), 2 days);
        dvp.grantRole(dvp.PAUSER_ROLE(), pauser);

        // Bank A admits, on domain 7: its client, the fund's seller (it
        // is willing to owe the seller money) and the venue (it escrows cash).
        aRemoteRegistry.authorize(buyer, "KYC-BUYER");
        aRemoteRegistry.authorize(seller, "CPTY-SELLER");
        aRemoteRegistry.authorize(address(dvp), "VENUE");

        // The fund's transfer agent: buyer and seller are eligible holders;
        // the venue is NOT, because the asset leg is pulled, never escrowed.
        tbill = new MockSecurityToken();
        tbill.setEligible(buyer, true);
        tbill.setEligible(seller, true);
        tbill.issue(seller, 100_000 * SHARE);

        aRegistry.authorize(buyer, "KYC-BUYER");
        _fund(BANK_A, 60_000_000 * M, "FW-A-1");
        _mintA(buyer, 30_000_000 * M, "CORE-BUYER-1");
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Helpers
    //////////////////////////////////////////////////////////////////////////*/

    function _terms(uint256 shares, uint256 dollars, bytes32 salt) internal view returns (DvPSettlement.Terms memory) {
        return DvPSettlement.Terms({
            seller: seller,
            buyer: buyer,
            asset: address(tbill),
            assetAmount: shares * SHARE,
            cash: address(cash),
            cashAmount: dollars * M,
            settleBy: uint64(block.timestamp + 2 hours),
            ref: "SESE023-TX-0001",
            salt: salt
        });
    }

    /// Seller affirms directly; the buyer's signature is relayed by its bank.
    function _match(DvPSettlement.Terms memory t) internal returns (bytes32 id) {
        id = dvp.tradeId(t);
        vm.prank(seller);
        dvp.affirm(t);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(buyerKey, id);
        vm.prank(relayer);
        dvp.affirmFor(t, buyer, abi.encodePacked(r, s, v));
    }

    function _lastMessage() internal returns (bytes memory) {
        Vm.Log[] memory logs = vm.getRecordedLogs();
        bytes32 sig = keccak256("MessageSent(bytes)");
        for (uint256 i = logs.length; i > 0; i--) {
            if (logs[i - 1].topics[0] == sig) return abi.decode(logs[i - 1].data, (bytes));
        }
        revert("no MessageSent");
    }

    function _attest(bytes memory message) internal view returns (bytes[] memory sigs) {
        bytes32 digest = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", keccak256(message)));
        uint256 k0 = attesterKeys[0];
        uint256 k1 = attesterKeys[2];
        if (vm.addr(k1) < vm.addr(k0)) (k0, k1) = (k1, k0);
        sigs = new bytes[](2);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(k0, digest);
        sigs[0] = abi.encodePacked(r, s, v);
        (v, r, s) = vm.sign(k1, digest);
        sigs[1] = abi.encodePacked(r, s, v);
    }

    /// The issuing bank's co-signature over the same message.
    function _iss(bytes memory message) internal view returns (bytes memory) {
        bytes32 digest = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", keccak256(message)));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(aIssuerKey, digest);
        return abi.encodePacked(r, s, v);
    }

    /// At home: the buyer burns dtA and sends it to the venue, for the trade.
    function _sendCash(bytes32 id, uint256 dollars) internal returns (bytes memory m) {
        vm.recordLogs();
        vm.prank(buyer);
        home.depositForBurnWithHook(BANK_A, dollars * M, REMOTE, address(dvp), address(dvp), abi.encode(id, buyer), address(0));
        m = _lastMessage();
    }

    /// Cash the buyer already holds on domain 7, in its own wallet.
    function _cashToWallet(uint256 dollars) internal {
        vm.recordLogs();
        vm.prank(buyer);
        home.depositForBurn(BANK_A, dollars * M, REMOTE, buyer);
        bytes memory m = _lastMessage();
        remote.receiveMessage(m, _attest(m), _iss(m));
    }

}

contract DvPTest is DvPBase {

    /*//////////////////////////////////////////////////////////////////////////
                                The main path
    //////////////////////////////////////////////////////////////////////////*/

    /// The whole of option 2: cash leaves home by burn, arrives on the asset's
    /// chain together with its instruction, and the trade settles in that
    /// same transaction. The Fed and the backing do not move.
    function test_CashFromHomeSettlesTheTradeOnArrival() public {
        DvPSettlement.Terms memory t = _terms(10_000, 10_020_000, "A");
        bytes32 id = _match(t);
        assertEq(uint8(dvp.trade(id).status), uint8(DvPSettlement.Status.Matched));
        vm.prank(seller);
        tbill.approve(address(dvp), 10_000 * SHARE);

        uint256 fedBefore = ledger.omnibusTotal();
        bytes memory m = _sendCash(id, 10_020_000);
        assertEq(ledger.member(BANK_A).remoteSupply, 10_020_000 * M, "counted abroad while in flight");

        vm.prank(relayer);
        dvp.receiveCash(m, _attest(m), _iss(m));

        assertEq(uint8(dvp.trade(id).status), uint8(DvPSettlement.Status.Settled));
        assertEq(tbill.balanceOf(buyer), 10_000 * SHARE, "buyer has the fund shares");
        assertEq(cash.balanceOf(seller), 10_020_000 * M, "seller has Bank A dollars");
        assertEq(cash.balanceOf(address(dvp)), 0, "nothing left in the venue");
        assertEq(dvp.escrowed(address(cash)), 0);
        assertEq(ledger.omnibusTotal(), fedBefore, "the Fed saw nothing");
        assertEq(_backing(BANK_A), 30_000_000 * M, "backing never moved");
        assertTrue(ledger.invariantsHold());
    }

    /// Both legs already on this chain: nothing escrowed, both pulled in one
    /// transaction by whoever triggers it.
    function test_BothLegsPulledWhenBothAreHere() public {
        _cashToWallet(5_000_000);
        DvPSettlement.Terms memory t = _terms(5_000, 5_000_000, "B");
        bytes32 id = _match(t);
        vm.prank(seller);
        tbill.approve(address(dvp), 5_000 * SHARE);
        vm.prank(buyer);
        cash.approve(address(dvp), 5_000_000 * M);

        vm.prank(relayer);
        dvp.settle(id);
        assertEq(tbill.balanceOf(buyer), 5_000 * SHARE);
        assertEq(cash.balanceOf(seller), 5_000_000 * M);
    }

    /// The seller then takes its dollars home and redeems them at its bank's
    /// counterparty: the cash leg ends as an ordinary tokenized deposit.
    function test_TheSellerTakesItsDollarsHome() public {
        test_CashFromHomeSettlesTheTradeOnArrival();
        aRegistry.authorize(seller, "CPTY-SELLER");
        vm.recordLogs();
        vm.prank(seller);
        remote.depositForBurn(BANK_A, 10_020_000 * M, HOME, seller);
        bytes memory m = _lastMessage();
        home.receiveMessage(m, _attest(m), _iss(m));
        assertEq(dtA.balanceOf(seller), 10_020_000 * M);
        assertEq(ledger.member(BANK_A).remoteSupply, 0);
        assertEq(home.outstanding(BANK_A, REMOTE), 0);
        assertTrue(ledger.invariantsHold());
    }

    /*//////////////////////////////////////////////////////////////////////////
                            When a leg cannot move
    //////////////////////////////////////////////////////////////////////////*/

    function test_AnIneligibleBuyerFailsAtTheMatchNotAtTheDeadline() public {
        tbill.setEligible(buyer, false);
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "C");
        bytes32 id = dvp.tradeId(t);
        vm.prank(seller);
        dvp.affirm(t);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(buyerKey, id);
        vm.expectRevert(abi.encodeWithSelector(DvPSettlement.LegCannotMove.selector, address(tbill), buyer));
        dvp.affirmFor(t, buyer, abi.encodePacked(r, s, v));
    }

    /// Cash arrives before the seller has approved: the cash leg is escrowed
    /// and the trade waits; the seller approves and anyone settles it.
    function test_CashWaitsInEscrowUntilTheSellerIsReady() public {
        DvPSettlement.Terms memory t = _terms(2_000, 2_000_000, "D");
        bytes32 id = _match(t);
        bytes memory m = _sendCash(id, 2_000_000);
        vm.expectEmit(true, false, false, false, address(dvp));
        emit DvPSettlement.SettlementPending(id, "");
        dvp.receiveCash(m, _attest(m), _iss(m));
        assertEq(dvp.trade(id).cashFunder, buyer);
        assertEq(dvp.escrowed(address(cash)), 2_000_000 * M);

        vm.prank(seller);
        tbill.approve(address(dvp), 2_000 * SHARE);
        dvp.settle(id);
        assertEq(cash.balanceOf(seller), 2_000_000 * M);
        assertEq(dvp.escrowed(address(cash)), 0);
    }

    /// A freeze placed after the match fails the settlement, and both legs
    /// stay where they were: principal risk is zero.
    function test_AFreezeAfterTheMatchFailsBothLegs() public {
        DvPSettlement.Terms memory t = _terms(3_000, 3_000_000, "E");
        bytes32 id = _match(t);
        vm.prank(seller);
        tbill.approve(address(dvp), 3_000 * SHARE);
        tbill.setFrozenTokens(seller, 100_000 * SHARE);

        bytes memory m = _sendCash(id, 3_000_000);
        dvp.receiveCash(m, _attest(m), _iss(m));
        assertEq(uint8(dvp.trade(id).status), uint8(DvPSettlement.Status.Matched), "still open");
        assertEq(tbill.balanceOf(seller), 100_000 * SHARE, "asset did not move");
        assertEq(cash.balanceOf(seller), 0, "cash did not move");
        assertEq(dvp.escrowed(address(cash)), 3_000_000 * M, "cash waits in escrow");
    }

    /*//////////////////////////////////////////////////////////////////////////
                        Cash that cannot fund its trade
    //////////////////////////////////////////////////////////////////////////*/

    /// A message that arrives after the deadline is still delivered (a refused
    /// message could never be), credited to the buyer, and sent home.
    function test_LateCashIsCreditedAndGoesBackHome() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "F");
        bytes32 id = _match(t);
        bytes memory m = _sendCash(id, 1_000_000);
        vm.warp(block.timestamp + 3 hours);

        vm.expectEmit(true, true, true, true, address(dvp));
        emit DvPSettlement.CashCredited(id, buyer, address(cash), 1_000_000 * M, DvPSettlement.CreditReason.PastDeadline);
        dvp.receiveCash(m, _attest(m), _iss(m));
        assertEq(dvp.credit(address(cash), buyer), 1_000_000 * M);

        vm.recordLogs();
        vm.prank(buyer);
        dvp.withdrawCreditHome(BANK_A, buyer);
        bytes memory back = _lastMessage();
        home.receiveMessage(back, _attest(back), _iss(back));
        assertEq(dtA.balanceOf(buyer), 30_000_000 * M, "whole again at home");
        assertEq(ledger.member(BANK_A).remoteSupply, 0);
        assertEq(home.outstanding(BANK_A, REMOTE), 0);
        assertTrue(ledger.invariantsHold());
    }

    function test_CashForAnAlreadyFundedOrWrongTradeIsCredited() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "G");
        bytes32 id = _match(t);
        bytes memory m1 = _sendCash(id, 1_000_000);
        dvp.receiveCash(m1, _attest(m1), _iss(m1));
        bytes memory m2 = _sendCash(id, 1_000_000);
        dvp.receiveCash(m2, _attest(m2), _iss(m2));
        bytes memory m3 = _sendCash(id, 999_999);
        dvp.receiveCash(m3, _attest(m3), _iss(m3));
        assertEq(dvp.escrowed(address(cash)), 1_000_000 * M, "one funding");
        assertEq(dvp.credit(address(cash), buyer), 1_999_999 * M, "the rest credited");

        vm.prank(buyer);
        dvp.withdrawCredit(address(cash));
        assertEq(cash.balanceOf(buyer), 1_999_999 * M);
        assertEq(cash.balanceOf(address(dvp)), dvp.escrowed(address(cash)) + dvp.credited(address(cash)));
    }

    function test_AMalformedInstructionIsCreditedToTheSender() public {
        vm.recordLogs();
        vm.prank(buyer);
        home.depositForBurnWithHook(BANK_A, 7 * M, REMOTE, address(dvp), address(dvp), hex"deadbeef", address(0));
        bytes memory m = _lastMessage();
        dvp.receiveCash(m, _attest(m), _iss(m));
        assertEq(dvp.credit(address(cash), buyer), 7 * M);
    }

    /// Nobody but the venue can deliver a message meant for it, so tokens
    /// never land here without their instruction (the CCTP V2 stranding trap).
    function test_OnlyTheVenueDeliversItsCash() public {
        bytes32 id = _match(_terms(1_000, 1_000_000, "H"));
        bytes memory m = _sendCash(id, 1_000_000);
        bytes[] memory sigs = _attest(m);
        vm.expectRevert(abi.encodeWithSelector(CrossChainMessenger.WrongCaller.selector, relayer, address(dvp)));
        vm.prank(relayer);
        remote.receiveMessage(m, sigs, _iss(m));
    }

    /*//////////////////////////////////////////////////////////////////////////
                            Refunds and cancellation
    //////////////////////////////////////////////////////////////////////////*/

    function test_ALapsedTradeReturnsEscrowToWhoeverFundedIt() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "I");
        bytes32 id = _match(t);
        tbill.setEligible(address(dvp), true); // this seller escrows its shares
        vm.prank(seller);
        tbill.approve(address(dvp), 1_000 * SHARE);
        vm.prank(seller);
        dvp.fundAsset(id); // settlement attempted, pending: no cash yet

        vm.expectRevert(abi.encodeWithSelector(DvPSettlement.NotRefundable.selector, id));
        dvp.refund(id);
        vm.warp(block.timestamp + 3 hours);
        vm.prank(relayer);
        dvp.refund(id);
        assertEq(tbill.balanceOf(seller), 100_000 * SHARE);
        assertEq(uint8(dvp.trade(id).status), uint8(DvPSettlement.Status.Cancelled));
    }

    /// The buyer is removed from Bank A's holder list before the refund:
    /// its cash is credited, not stuck, and the seller's refund still goes out.
    function test_ABlockedFunderIsCreditedAndDoesNotBlockTheOther() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "J");
        bytes32 id = _match(t);
        tbill.setEligible(address(dvp), true);
        bytes memory m = _sendCash(id, 1_000_000);
        dvp.receiveCash(m, _attest(m), _iss(m));
        aRemoteRegistry.revoke(buyer, "KYC-LAPSED");
        vm.warp(block.timestamp + 3 hours);

        dvp.refund(id);
        assertEq(dvp.credit(address(cash), buyer), 1_000_000 * M, "credited, withdrawable once re-admitted");
        assertEq(dvp.escrowed(address(cash)), 0);
        assertEq(cash.balanceOf(address(dvp)), dvp.credited(address(cash)));
    }

    function test_AMatchedTradeCancelsOnlyBilaterally() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "K");
        bytes32 id = _match(t);
        bytes memory m = _sendCash(id, 1_000_000);
        dvp.receiveCash(m, _attest(m), _iss(m));

        vm.prank(seller);
        dvp.cancel(id);
        assertEq(uint8(dvp.trade(id).status), uint8(DvPSettlement.Status.Matched), "a request is not a cancellation");
        vm.prank(seller);
        vm.expectRevert(abi.encodeWithSelector(DvPSettlement.CancelAlreadyRequested.selector, id, seller));
        dvp.cancel(id);
        vm.prank(buyer);
        dvp.cancel(id);
        assertEq(uint8(dvp.trade(id).status), uint8(DvPSettlement.Status.Cancelled));
        assertEq(cash.balanceOf(buyer), 1_000_000 * M, "escrow back to the buyer");
    }

    function test_AnUnmatchedOfferIsWithdrawnByItsMaker() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "L");
        vm.prank(seller);
        bytes32 id = dvp.affirm(t);
        vm.prank(buyer);
        vm.expectRevert(abi.encodeWithSelector(DvPSettlement.NotAParty.selector, id, buyer));
        dvp.cancel(id);
        vm.prank(seller);
        dvp.cancel(id);
        assertEq(uint8(dvp.trade(id).status), uint8(DvPSettlement.Status.Cancelled));
    }

    /*//////////////////////////////////////////////////////////////////////////
                                    Signatures
    //////////////////////////////////////////////////////////////////////////*/

    function test_ASignatureIsBoundToItsTerms() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "M");
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(buyerKey, dvp.tradeId(t));
        DvPSettlement.Terms memory other = t;
        other.cashAmount = 2_000_000 * M;
        bytes32 otherId = dvp.tradeId(other);
        vm.expectRevert(abi.encodeWithSelector(DvPSettlement.BadSignature.selector, otherId, buyer));
        dvp.affirmFor(other, buyer, abi.encodePacked(r, s, v));
    }

    function test_ARevokedSignatureCanNeverAffirm() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "N");
        bytes32 id = dvp.tradeId(t);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(buyerKey, id);
        vm.prank(buyer);
        dvp.revoke(t);
        vm.expectRevert(
            abi.encodeWithSelector(DvPSettlement.WrongStatus.selector, id, DvPSettlement.Status.Cancelled)
        );
        dvp.affirmFor(t, buyer, abi.encodePacked(r, s, v));
    }

    function test_AStrangerCannotAffirm() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "O");
        bytes32 id = dvp.tradeId(t);
        vm.prank(outsider);
        vm.expectRevert(abi.encodeWithSelector(DvPSettlement.NotAParty.selector, id, outsider));
        dvp.affirm(t);
    }

    /*//////////////////////////////////////////////////////////////////////////
                                Pause and hostile tokens
    //////////////////////////////////////////////////////////////////////////*/

    function test_APauseStopsSettlementButNeverTrapsMoney() public {
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "P");
        bytes32 id = _match(t);
        bytes memory m1 = _sendCash(id, 1_000_000);
        dvp.receiveCash(m1, _attest(m1), _iss(m1));
        vm.prank(pauser);
        dvp.pause();

        vm.prank(seller);
        tbill.approve(address(dvp), 1_000 * SHARE);
        vm.expectRevert(Pausable.EnforcedPause.selector);
        dvp.settle(id);

        bytes memory m2 = _sendCash(id, 5_000);
        dvp.receiveCash(m2, _attest(m2), _iss(m2)); // delivered while paused, as a credit
        assertEq(dvp.credit(address(cash), buyer), 5_000 * M);

        vm.warp(block.timestamp + 3 hours);
        dvp.refund(id);
        vm.prank(buyer);
        dvp.withdrawCredit(address(cash));
        assertEq(cash.balanceOf(buyer), 1_005_000 * M, "everything back while paused");
    }

    function test_AFeeOnTransferLegIsRefused() public {
        MockFeeToken fee = new MockFeeToken();
        fee.transfer(seller, 2_000 * SHARE); // arrives as 1,980: the fee bites every hop
        _cashToWallet(1_000_000);
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "Q");
        t.asset = address(fee);
        bytes32 id = _match(t);
        vm.prank(seller);
        fee.approve(address(dvp), 1_000 * SHARE);
        vm.prank(buyer);
        cash.approve(address(dvp), 1_000_000 * M);
        vm.expectRevert(
            abi.encodeWithSelector(DvPSettlement.InexactTransfer.selector, address(fee), buyer, 1_000 * SHARE, 990 * SHARE)
        );
        dvp.settle(id);
    }

    function test_AReentrantTokenCannotSettleTwice() public {
        MockReentrantToken hook = new MockReentrantToken();
        hook.transfer(seller, 1_000 * SHARE);
        _cashToWallet(2_000_000);
        DvPSettlement.Terms memory t = _terms(1_000, 1_000_000, "R");
        t.asset = address(hook);
        bytes32 id = _match(t);
        vm.prank(seller);
        hook.approve(address(dvp), 2_000 * SHARE);
        vm.prank(buyer);
        cash.approve(address(dvp), 2_000_000 * M);
        hook.arm(address(dvp), abi.encodeCall(DvPSettlement.settle, (id)));

        dvp.settle(id);
        assertTrue(hook.reentered());
        assertFalse(hook.reentrySucceeded(), "the guard refused the second entry");
        assertEq(cash.balanceOf(seller), 1_000_000 * M, "paid once");
    }

    function test_TheAdminHasNoPathToEscrowAndHandsOverSlowly() public {
        assertEq(dvp.defaultAdminDelay(), 2 days);
        address next = makeAddr("next-admin");
        dvp.beginDefaultAdminTransfer(next);
        vm.prank(next);
        vm.expectRevert();
        dvp.acceptDefaultAdminTransfer();
    }

}
